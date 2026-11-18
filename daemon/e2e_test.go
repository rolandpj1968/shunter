package daemon_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/fake"
	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/store"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// rig is a daemon on the fake world, with its store in a temporary
// directory. It can crash and restart the daemon at any point.
type rig struct {
	t      *testing.T
	ctx    context.Context
	dir    string
	w      *fake.World
	clk    *clock
	st     *store.Store
	d      *daemon.Daemon
	cancel context.CancelFunc
	done   chan error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	r := &rig{t: t, ctx: ctx, dir: t.TempDir(), w: fake.New("T0"), clk: &clock{t: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}}
	r.start()
	r.settle()
	t.Cleanup(func() {
		if r.d != nil {
			r.stop(false)
		}
		r.audit()
	})
	return r
}

func (r *rig) start() {
	r.t.Helper()
	st, err := store.Open(r.dir, store.Options{Init: &store.Init{Trunk: "T0", Config: state.Config{Depth: 2, NoVerdictBudget: 2, PushRetries: 2}},
		Version: "test", Actor: "test", Now: r.clk.Now, SnapshotEvery: 7})
	if err != nil {
		r.t.Fatal(err)
	}
	r.st = st
	// The real ticker never fires in a test; the test ticks by hand.
	r.d = daemon.New(st, r.w.Set(), daemon.Config{Version: "test", Now: r.clk.Now, Tick: time.Hour, Poll: time.Nanosecond, Backoff: time.Second}, nil)
	ctx, cancel := context.WithCancel(r.ctx)
	r.cancel, r.done = cancel, make(chan error, 1)
	go func() { r.done <- r.d.Run(ctx) }()
}

// stop stops the daemon. With crash, the store is dropped as a dying
// process would drop it: no snapshot, no final record.
func (r *rig) stop(crash bool) {
	r.t.Helper()
	r.cancel()
	if err := <-r.done; err != nil {
		r.t.Fatalf("daemon: %v", err)
	}
	if crash {
		r.st.Abandon()
	} else if err := r.st.Close(); err != nil {
		r.t.Fatal(err)
	}
	r.d = nil
}

func (r *rig) restart() { r.t.Helper(); r.stop(true); r.start(); r.settle() }

// settle waits until the daemon is quiet: nothing in flight but the calls
// the test holds blocked, and no owed effect due.
func (r *rig) settle() {
	r.t.Helper()
	for {
		n, due, err := r.d.Busy(r.ctx)
		if err != nil {
			r.t.Fatalf("settle: %v", err)
		}
		if !due && n <= r.w.Blocked() {
			// Twice, in case a result was on its way into the loop.
			if n2, due2, _ := r.d.Busy(r.ctx); !due2 && n2 == n && n2 <= r.w.Blocked() {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
}

// tick moves the clock by d and runs the timers and polls until the daemon
// is quiet again. Two passes: a poll's answer may start new effects.
func (r *rig) tick(d time.Duration) {
	r.t.Helper()
	r.clk.Add(d)
	for i := 0; i < 3; i++ {
		if err := r.d.Tick(r.ctx); err != nil {
			r.t.Fatalf("tick: %v", err)
		}
		r.settle()
	}
}

func (r *rig) exec(req daemon.Request) string {
	r.t.Helper()
	resp := r.d.Exec(r.ctx, req)
	if resp.Error != "" {
		r.t.Fatalf("%s %v: %s", req.Cmd, req.Args, resp.Error)
	}
	r.settle()
	return resp.Output
}

func (r *rig) execErr(req daemon.Request) string {
	r.t.Helper()
	resp := r.d.Exec(r.ctx, req)
	r.settle()
	return resp.Error
}

// open adds changes to the forge, each on trunk.
func (r *rig) open(ids ...string) {
	for _, id := range ids {
		r.w.Open(state.ChangeID(id), state.Head(id+"-1"), "")
	}
}

func (r *rig) queue(ids ...string) { r.t.Helper(); r.exec(daemon.Request{Cmd: "queue", Args: ids}) }

// together queues lines while the queue is paused, so they ride one train.
func (r *rig) together(ids ...string) {
	r.t.Helper()
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather a train"})
	for _, id := range ids {
		r.queue(id)
	}
	r.exec(daemon.Request{Cmd: "resume"})
}

func (r *rig) state() state.State {
	r.t.Helper()
	s, err := r.d.State(r.ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *rig) line(id string) state.LineState { return r.state().Lines[state.LineID(id)].State }

func (r *rig) trainOf(id string) state.TrainID {
	r.t.Helper()
	l := r.state().Lines[state.LineID(id)]
	if l.State != state.LineRiding {
		r.t.Fatalf("line %s is %s, not riding", id, l.State)
	}
	return l.Train
}

func (r *rig) train(id state.TrainID) *state.Train { return r.state().Trains[id] }

func (r *rig) wantLine(id string, want state.LineState) {
	r.t.Helper()
	if got := r.line(id); got != want {
		r.t.Fatalf("line %s is %s, want %s\n%s", id, got, want, daemon.Status(r.state(), nil))
	}
}

func (r *rig) wantTrain(id state.TrainID, want state.TrainState) {
	r.t.Helper()
	if got := r.train(id).State; got != want {
		r.t.Fatalf("train %s is %s, want %s\n%s", id, got, want, daemon.Status(r.state(), nil))
	}
}

func (r *rig) wantTrunk(heads int) {
	r.t.Helper()
	trunk, hist := r.w.Trunk()
	if len(hist) != heads {
		r.t.Fatalf("trunk moved %d times, want %d: %v", len(hist), heads, hist)
	}
	if s := r.state(); s.Trunk != trunk {
		r.t.Fatalf("the store says trunk is %s; the forge says %s", s.Trunk, trunk)
	}
}

// audit replays the whole log through the machine from its init record,
// checking every invariant at every step: the record is the state (I8).
func (r *rig) audit() {
	r.t.Helper()
	recs, err := store.ReadLog(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	var s state.State
	for _, rec := range recs {
		switch rec.Kind {
		case store.KindInit:
			s = state.New(rec.Trunk, *rec.Config)
		case store.KindApplied:
			ev, err := store.Decode(*rec.Event)
			if err != nil {
				r.t.Fatal(err)
			}
			next, fx, err := state.Next(s, ev)
			if err != nil {
				r.t.Fatalf("record %d: the log holds an event the machine refuses: %v", rec.Seq, err)
			}
			for _, e := range append(state.Check(next), state.CheckStep(s, next, fx)...) {
				r.t.Errorf("record %d %s: %v", rec.Seq, state.Name(ev), e)
			}
			s = next
		}
	}
}

func TestLandsTwoTrainsInChainOrder(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.queue("a")
	r.queue("b")
	t1, t2 := r.trainOf("a"), r.trainOf("b")
	if t1 == t2 {
		t.Fatal("a and b rode one train")
	}
	r.wantTrain(t1, state.TrainTesting)
	r.wantTrain(t2, state.TrainTesting)
	if r.train(t2).BaseHead != r.train(t1).Head {
		t.Fatal("train 2 is not built on train 1's head (I5)")
	}
	// Train 2 passes first; it waits its turn.
	r.w.Pass(t2)
	r.tick(time.Second)
	r.wantTrain(t2, state.TrainGreen)
	r.wantTrunk(0)
	r.w.Pass(t1)
	r.tick(time.Second)
	r.wantTrain(t1, state.TrainLanded)
	r.wantTrain(t2, state.TrainLanded)
	r.wantLine("a", state.LineLanded)
	r.wantLine("b", state.LineLanded)
	r.wantTrunk(2)
	if _, hist := r.w.Trunk(); hist[0] != r.train(t1).Head || hist[1] != r.train(t2).Head {
		t.Fatalf("landed out of chain order: %v", hist)
	}
	if r.w.PR(t1) != "" || r.w.PR(t2) != "" {
		t.Fatal("a landed train's pull request is still open")
	}
	if len(r.w.NoticesTo("owner", "landed")) != 2 {
		t.Fatal("the owners were not told")
	}
}

func TestBlameReturnsOneLineAndTheRestLand(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.together("a", "b")
	t1 := r.trainOf("a")
	r.w.Fail(t1, "TestB failed in b/b_test.go, a file b touches", "b")
	r.tick(time.Second)
	r.wantTrain(t1, state.TrainFailed)
	r.wantLine("b", state.LineReturned)
	if len(r.w.NoticesTo("owner", "TestB failed")) != 1 {
		t.Fatal("b's owner was not told, with the evidence")
	}
	t2 := r.trainOf("a")
	r.w.Pass(t2)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
	r.wantTrunk(1)
}

func TestUnblamedFailureWaitsAtJ1ThenSplits(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.together("a", "b")
	t1 := r.trainOf("a")
	r.w.Fail(t1, "TestShared failed")
	r.tick(time.Second)
	r.wantTrain(t1, state.TrainJudging)
	if len(r.w.NoticesTo("judge", "J1")) != 1 {
		t.Fatal("the judge was not asked at J1")
	}
	// The judge is silent; at the deadline the default splits the train.
	r.tick(9 * time.Minute)
	r.wantTrain(t1, state.TrainJudging)
	r.tick(2 * time.Minute)
	r.wantTrain(t1, state.TrainFailed)
	ta, tb := r.trainOf("a"), r.trainOf("b")
	if ta == tb {
		t.Fatal("the halves of a split rode together")
	}
	r.w.Pass(ta)
	r.w.Pass(tb)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
	r.wantLine("b", state.LineLanded)
}

func TestJ1DefaultParksWhenTheSameTestFailedBefore(t *testing.T) {
	r := newRig(t)
	r.open("a", "b", "c", "d")
	r.together("a", "b")
	r.w.Fail(r.trainOf("a"), "TestFixture failed")
	r.tick(time.Second)
	r.exec(daemon.Request{Cmd: "judge", Args: []string{string(r.state().Lines["a"].Train), "split"}, Reason: "look at the halves"})
	// The halves fail on the same test, with unrelated lines: a broken fixture.
	r.w.Fail(r.trainOf("a"), "TestFixture failed")
	r.tick(time.Second)
	r.w.Fail(r.trainOf("b"), "TestFixture failed")
	r.tick(time.Second)
	// J1's deadline passes. The first half is one line: its default is park,
	// which parks the queue; the second half had the same failure as the
	// train before it, with unrelated lines: park too.
	r.tick(11 * time.Minute)
	r.tick(11 * time.Minute)
	for _, id := range []string{"a", "b"} {
		if st := r.line(id); st != state.LineParked {
			t.Fatalf("line %s is %s; want parked by J1's default\n%s", id, st, daemon.Status(r.state(), nil))
		}
	}
	// New arrivals park too, until the park's deadline restores the queue.
	r.queue("c")
	r.wantLine("c", state.LineParked)
	r.tick(25 * time.Hour)
	if r.line("c") == state.LineParked {
		t.Fatal("the park was not restored at its deadline")
	}
	if len(r.w.NoticesTo("judge", "restored at its deadline")) == 0 {
		t.Fatal("no alert when the park was restored at its deadline")
	}
}

// TestCancelAtEveryStage aborts a train at each stage before its push, with
// each action, and checks that nothing of it lands.
func TestCancelAtEveryStage(t *testing.T) {
	stages := []struct {
		name  string
		block string // the call held so the train stays in the stage
		to    func(r *rig, tr state.TrainID)
		want  state.TrainState
	}{
		{"assembling", "build", nil, state.TrainAssembling},
		{"built", "open-pr", nil, state.TrainBuilt},
		{"testing", "", nil, state.TrainTesting},
		{"judging", "", func(r *rig, tr state.TrainID) { r.w.Fail(tr, "unattributed"); r.tick(time.Second) }, state.TrainJudging},
		{"green", "verify", func(r *rig, tr state.TrainID) { r.w.Pass(tr); r.tick(time.Second) }, state.TrainGreen},
	}
	actions := map[string]state.LineState{"park": state.LineParked, "requeue": state.LineRiding, "drop": state.LineReturned}
	for _, st := range stages {
		for action, want := range actions {
			t.Run(st.name+"/"+action, func(t *testing.T) {
				r := newRig(t)
				r.open("a", "b")
				var release func()
				if st.block != "" {
					release = r.w.Block(st.block)
					defer release()
				}
				r.together("a", "b")
				tr := r.trainOf("a")
				if st.to != nil {
					st.to(r, tr)
				}
				r.wantTrain(tr, st.want)
				r.exec(daemon.Request{Cmd: "cancel", Args: []string{"a"}, Action: action, Reason: "the coordinator says stop"})
				r.wantTrain(tr, state.TrainAborted)
				if release != nil {
					release()
					r.settle()
				}
				r.tick(time.Second)
				r.wantTrain(tr, state.TrainAborted)
				// The named line takes the action; the other goes again.
				if got := r.line("a"); got != want {
					t.Fatalf("a is %s, want %s", got, want)
				}
				if got := r.line("b"); got != state.LineRiding {
					t.Fatalf("b is %s, want riding again", got)
				}
				if r.w.PR(tr) != "" {
					t.Fatal("the aborted train's pull request is open")
				}
				r.wantTrunk(0)
				// Cancelling again is the same as once.
				r.exec(daemon.Request{Cmd: "cancel", Args: []string{string(tr)}, Action: "requeue", Reason: "again"})
			})
		}
	}
}

func TestCancelIsRefusedWhileLanding(t *testing.T) {
	r := newRig(t)
	r.open("a")
	release := r.w.Block("push")
	r.queue("a")
	tr := r.trainOf("a")
	r.w.Pass(tr)
	r.tick(time.Second)
	r.wantTrain(tr, state.TrainLanding)
	if e := r.execErr(daemon.Request{Cmd: "cancel", Args: []string{"a"}, Action: "drop", Reason: "too late"}); !strings.Contains(e, "point of no return") {
		t.Fatalf("cancel while landing: %q", e)
	}
	if e := r.execErr(daemon.Request{Cmd: "hold", Args: []string{"a"}, Reason: "too late"}); !strings.Contains(e, "point of no return") {
		t.Fatalf("hold while landing: %q", e)
	}
	release()
	r.settle()
	r.wantLine("a", state.LineLanded)
	r.wantTrunk(1)
}

func TestHoldAndRelease(t *testing.T) {
	r := newRig(t)
	r.open("a", "b", "c")
	r.together("a", "b")
	tr := r.trainOf("a")
	r.queue("c")
	// Hold a riding line: its train is void, and the rest go again.
	r.exec(daemon.Request{Cmd: "hold", Args: []string{"a"}, Reason: "the review found a problem"})
	r.wantTrain(tr, state.TrainVoid)
	r.wantLine("a", state.LineHeld)
	r.trainOf("b")
	if len(r.w.NoticesTo("owner", "the review found a problem")) != 1 {
		t.Fatal("a's owner was not shown the hold")
	}
	// A held line cannot be withdrawn, and never rides.
	if e := r.execErr(daemon.Request{Cmd: "withdraw", Args: []string{"a"}}); !strings.Contains(e, "held") {
		t.Fatalf("withdraw of a held line: %q", e)
	}
	for _, id := range []string{"b", "c"} {
		r.w.Pass(r.trainOf(id))
	}
	r.tick(time.Second)
	r.wantLine("b", state.LineLanded)
	r.wantLine("c", state.LineLanded)
	r.wantLine("a", state.LineHeld)
	// Released, it goes back through the gate, and lands.
	r.exec(daemon.Request{Cmd: "release", Args: []string{"a"}})
	r.w.Pass(r.trainOf("a"))
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
}

// TestRestartMidEffect crashes the daemon with an effect asked for and not
// yet performed, and again with an effect performed and not yet recorded.
func TestRestartMidEffect(t *testing.T) {
	r := newRig(t)
	r.open("a")
	release := r.w.Block("open-pr")
	r.queue("a")
	tr := r.trainOf("a")
	r.wantTrain(tr, state.TrainBuilt)
	r.stop(true) // the open-pr call is cut off
	r.start()
	release()
	r.settle()
	r.wantTrain(tr, state.TrainTesting)
	if n := r.w.Count("open-pr", true); n != 1 {
		t.Fatalf("%d pull requests opened for one train", n)
	}

	// The push moves trunk, and the daemon dies before it records that.
	var once sync.Once
	stuck := make(chan struct{})
	r.w.AfterPush = func(ctx context.Context) error {
		once.Do(func() { close(stuck) })
		<-ctx.Done()
		return ctx.Err()
	}
	r.w.Pass(tr)
	r.clk.Add(time.Second)
	r.d.Tick(r.ctx)
	<-stuck
	r.w.AfterPush = nil
	r.restart()
	r.wantTrain(tr, state.TrainLanded)
	r.wantLine("a", state.LineLanded)
	r.wantTrunk(1)
}

func TestRestartWhileTesting(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.queue("a")
	r.queue("b")
	t1, t2 := r.trainOf("a"), r.trainOf("b")
	r.restart()
	r.restart()
	r.wantTrain(t1, state.TrainTesting)
	r.wantTrain(t2, state.TrainTesting)
	r.w.Pass(t1)
	r.w.Pass(t2)
	r.tick(time.Second)
	r.wantTrunk(2)
}

// TestForgedVerdict is a builder's output that carries a passing report
// with no pass behind it (§7, I9): the lander reads the forge and finds none.
func TestForgedVerdict(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.queue("a")
	tr := r.trainOf("a")
	// A pass on a head that is not the train's: refused, and logged.
	r.w.Report(tr, adapter.Report{Outcome: adapter.Passed, Head: "some-other-head"})
	r.tick(time.Second)
	r.wantTrain(tr, state.TrainTesting)
	// A pass reported on the right head, which the forge does not show.
	r.w.Report(tr, adapter.Report{Outcome: adapter.Passed})
	r.tick(time.Second)
	r.wantTrain(tr, state.TrainVoid)
	r.wantTrunk(0)
	t2 := r.trainOf("a")
	if t2 == tr {
		t.Fatal("the line did not go again")
	}
	r.w.Pass(t2)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
	r.wantTrunk(1)
	recs, _ := store.ReadLog(r.dir)
	refused := 0
	for _, rec := range recs {
		if rec.Kind == store.KindRefused && rec.Event.Type == "ci-passed" {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("the pass on another head was not refused and logged")
	}
}

func TestHeadMovedWhileGreenVoidsAtLanding(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.queue("a")
	tr := r.trainOf("a")
	r.w.Change("a", func(c *fake.Change) { c.Head = "a-2" })
	r.w.Pass(tr)
	r.tick(time.Second)
	r.wantTrain(tr, state.TrainVoid)
	r.wantTrunk(0)
	if h := r.state().Lines["a"].Changes[0].Head; h != "a-2" {
		t.Fatalf("the line did not take its new head: %s", h)
	}
}

func TestTransientPushIsRetried(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.w.PushFaults = []error{fake.ErrTransient}
	r.queue("a")
	tr := r.trainOf("a")
	r.w.Pass(tr)
	r.tick(time.Second)
	r.wantTrain(tr, state.TrainLanding)
	r.tick(2 * time.Second) // past the backoff
	r.wantLine("a", state.LineLanded)
	r.wantTrunk(1)
}

func TestAnotherWriterOnTrunkPausesAtJ3(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.queue("a")
	tr := r.trainOf("a")
	r.w.MoveTrunk("foreign")
	r.w.Pass(tr)
	r.tick(time.Second)
	r.wantTrain(tr, state.TrainVoid)
	if !r.state().Paused {
		t.Fatal("not paused at J3")
	}
	r.wantLine("a", state.LineWaiting)
	r.exec(daemon.Request{Cmd: "resume", Args: []string{"foreign"}})
	r.w.Pass(r.trainOf("a"))
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
}

func TestNoVerdictAtTheCIDeadline(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.queue("a")
	t1 := r.trainOf("a")
	r.tick(91 * time.Minute)
	r.wantTrain(t1, state.TrainVoid)
	t2 := r.trainOf("a")
	r.tick(91 * time.Minute)
	r.wantTrain(t2, state.TrainVoid)
	// The line's no-verdict budget is spent: J3, and the queue pauses.
	if !r.state().Paused || len(r.w.NoticesTo("judge", "J3")) == 0 {
		t.Fatal("repeated no-verdicts did not go to J3")
	}
	r.wantLine("a", state.LineWaiting)
}

func TestGateRefusesAndStacksWait(t *testing.T) {
	r := newRig(t)
	r.open("a", "x")
	r.w.Open("b", "b-1", "a") // b is stacked on a
	r.w.Change("x", func(c *fake.Change) { c.Approved = false })
	r.queue("x")
	r.wantLine("x", state.LineReturned)
	release := r.w.Block("open-pr")
	r.queue("a")
	r.queue("b")
	r.wantLine("b", state.LineWaitingBase)
	release()
	r.settle()
	r.w.Pass(r.trainOf("a"))
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
	r.w.Pass(r.trainOf("b"))
	r.tick(time.Second)
	r.wantLine("b", state.LineLanded)
}

func TestAloneRidesAlone(t *testing.T) {
	r := newRig(t)
	r.open("a", "b", "c")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather"})
	r.queue("a")
	r.exec(daemon.Request{Cmd: "queue", Args: []string{"b"}, Alone: true})
	r.queue("c")
	r.exec(daemon.Request{Cmd: "resume"})
	ta, tb, tc := r.trainOf("a"), r.trainOf("b"), r.trainOf("c")
	if ta != tc || tb == ta {
		t.Fatalf("trains: a %s, b %s, c %s", ta, tb, tc)
	}
	if n := len(r.train(tb).Lines); n != 1 {
		t.Fatalf("the alone line rode with %d lines", n)
	}
}

func TestExportImport(t *testing.T) {
	r := newRig(t)
	r.open("a", "b", "c")
	r.exec(daemon.Request{Cmd: "pause", Reason: "cut-over"})
	r.queue("a")
	r.queue("b")
	r.queue("c")
	r.exec(daemon.Request{Cmd: "hold", Args: []string{"b"}, Reason: "waits on a design call"})
	resp := r.d.Exec(r.ctx, daemon.Request{Cmd: "export"})
	if resp.Doc == nil || len(resp.Doc.Lines) != 3 {
		t.Fatalf("export: %+v", resp)
	}
	r2 := newRig(t)
	r2.w = r.w // the same forge
	r2.restart()
	r2.exec(daemon.Request{Cmd: "pause", Reason: "import"})
	out := r2.exec(daemon.Request{Cmd: "import", Doc: resp.Doc})
	if !strings.Contains(out, "a: waiting") {
		t.Fatalf("import: %s", out)
	}
	r2.wantLine("a", state.LineWaiting)
	r2.wantLine("b", state.LineHeld)
	r2.wantLine("c", state.LineWaiting)
	if r2.state().Lines["b"].Reason != "waits on a design call" {
		t.Fatal("the hold's reason was not carried")
	}
}

func TestOfflineCommandsAndOneWriter(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.exec(daemon.Request{Cmd: "pause", Reason: "x"})
	r.queue("a")
	// The daemon holds the store: an offline command cannot open it.
	if _, err := store.Open(r.dir, store.Options{}); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("a second writer: %v", err)
	}
	r.stop(false)
	st, err := store.Open(r.dir, store.Options{Now: r.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if resp := daemon.Offline(st, daemon.Request{Cmd: "hold", Args: []string{"a"}, Reason: "offline"}); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if resp := daemon.Offline(st, daemon.Request{Cmd: "queue", Args: []string{"a"}}); resp.Error == "" {
		t.Fatal("queued offline with no forge")
	}
	exp := daemon.Offline(st, daemon.Request{Cmd: "export"})
	if exp.Doc == nil || exp.Doc.Lines[0].State != "held" {
		t.Fatalf("offline export: %+v", exp)
	}
	st.Close()
	r.start()
	r.settle()
	// The notice the offline hold owed is sent once the daemon runs.
	if len(r.w.NoticesTo("owner", "offline")) != 1 {
		t.Fatal("the owed notice was not sent after the start")
	}
}

func TestSocket(t *testing.T) {
	r := newRig(t)
	sock := filepath.Join(t.TempDir(), "s")
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- r.d.Serve(ctx, sock) }()
	var resp daemon.Response
	var err error
	for i := 0; i < 200; i++ {
		if resp, err = daemon.Call(r.ctx, sock, daemon.Request{Cmd: "status"}); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || !strings.Contains(resp.Output, "trunk T0") {
		t.Fatalf("status over the socket: %v %+v", err, resp)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}
