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
	"github.com/rolandpj1968/shunter/tier"
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
	// maxLines and tiers are the daemon's train cap and tier rules.
	maxLines int
	tiers    tier.Rules
}

func newRig(t *testing.T) *rig { return newRigWith(t, 0, tier.Rules{}) }

// newRigWith is a rig whose daemon caps a train's lines and classifies lines
// by the tier rules given.
func newRigWith(t *testing.T, maxLines int, tiers tier.Rules) *rig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	r := &rig{t: t, ctx: ctx, dir: t.TempDir(), w: fake.New("T0"), clk: &clock{t: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}, maxLines: maxLines, tiers: tiers}
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
	r.d = daemon.New(st, r.w.Set(), daemon.Config{Version: "test", Now: r.clk.Now, Tick: time.Hour, Poll: time.Nanosecond, Backoff: time.Second,
		MaxLines: r.maxLines, Tiers: r.tiers}, nil)
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
	// J1's deadline passes. The first half is one line at the head of the
	// chain, and its failure was not seen in an unrelated train before: it
	// is charged by elimination, and the queue is not parked for it. The
	// second half's failure, reported behind the first, was not judged; it
	// rides again, fails on the same test, and now the same failure was seen
	// in the train before it, with unrelated lines: the queue parks.
	r.tick(11 * time.Minute)
	r.wantLine("a", state.LineReturned)
	r.wantLine("b", state.LineRiding)
	r.w.Fail(r.trainOf("b"), "TestFixture failed")
	r.tick(time.Second)
	r.tick(11 * time.Minute)
	if st := r.line("b"); st != state.LineParked {
		t.Fatalf("line b is %s; want parked by J1's default\n%s", st, daemon.Status(r.state(), nil))
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
	r.queue("a")
	r.queue("b")
	// b waits in the queue, and rides in the train behind a's (I3).
	ta, tb := r.trainOf("a"), r.trainOf("b")
	if ta == tb || r.train(tb).BaseHead != r.train(ta).Head {
		t.Fatalf("b rides %s on %s; a rides %s at %s", tb, r.train(tb).BaseHead, ta, r.train(ta).Head)
	}
	r.w.Pass(tb)
	r.w.Pass(ta)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
	r.wantLine("b", state.LineLanded)
}

// stack opens b stacked on a.
func (r *rig) stack(id, base string) {
	r.w.Open(state.ChangeID(id), state.Head(id+"-1"), state.ChangeID(base))
}

// TestStackedLineRidesBehindItsBaseInOneTrain: needs first (I3). A line
// stacked on a waiting line joins the same train, behind it, and both land.
func TestStackedLineRidesBehindItsBaseInOneTrain(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.stack("b", "a")
	r.together("a", "b")
	t1 := r.trainOf("a")
	if r.trainOf("b") != t1 {
		t.Fatalf("a rides %s, b rides %s", t1, r.trainOf("b"))
	}
	if l := r.train(t1).Lines; len(l) != 2 || l[0] != "a" || l[1] != "b" {
		t.Fatalf("train order %v: the base must go first", l)
	}
	r.w.Pass(t1)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
	r.wantLine("b", state.LineLanded)
}

// TestSkippedBaseSkipsItsStack: a line whose base is skipped (held, or must
// ride alone) is skipped too, and isn't taken into a train its build would
// have to leave it out of.
func TestSkippedBaseSkipsItsStack(t *testing.T) {
	r := newRig(t)
	r.open("a", "c")
	r.stack("b", "a")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather"})
	r.queue("a")
	r.exec(daemon.Request{Cmd: "hold", Args: []string{"a"}, Reason: "waits on a design call"})
	r.queue("b")
	r.queue("c")
	r.exec(daemon.Request{Cmd: "resume"})
	tc := r.trainOf("c")
	if n := len(r.train(tc).Lines); n != 1 {
		t.Fatalf("the train took %v; want c alone", r.train(tc).Lines)
	}
	r.wantLine("b", state.LineWaiting)
	if st := r.exec(daemon.Request{Cmd: "status"}); !strings.Contains(st, "waits: a is held") {
		t.Fatalf("the status doesn't say what b waits on:\n%s", st)
	}
	// Released, a rides, and b behind it.
	r.exec(daemon.Request{Cmd: "release", Args: []string{"a"}})
	if r.trainOf("b") == tc {
		t.Fatal("b rode before its base")
	}
	if r.line("a") != state.LineRiding {
		t.Fatalf("a is %s", r.line("a"))
	}
}

// TestAloneBaseRidesFirstAndItsStackFollows: a base that must ride alone
// rides first; the line stacked on it rides in the next train, with others.
func TestAloneBaseRidesFirstAndItsStackFollows(t *testing.T) {
	r := newRig(t)
	r.open("a", "c")
	r.stack("b", "a")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather"})
	r.exec(daemon.Request{Cmd: "queue", Args: []string{"a"}, Alone: true})
	r.queue("b")
	r.queue("c")
	r.exec(daemon.Request{Cmd: "resume"})
	ta, tb := r.trainOf("a"), r.trainOf("b")
	if ta == tb || len(r.train(ta).Lines) != 1 || r.trainOf("c") != tb {
		t.Fatalf("a in %s %v, b in %s %v", ta, r.train(ta).Lines, tb, r.train(tb).Lines)
	}
}

// TestReturnedBaseTakesItsStackUncharged: blame names the base; the line
// stacked on it goes back with it, uncharged, and the rest ride again.
func TestReturnedBaseTakesItsStackUncharged(t *testing.T) {
	r := newRig(t)
	r.open("a", "c")
	r.stack("b", "a")
	r.together("a", "b", "c")
	t1 := r.trainOf("a")
	r.w.Fail(t1, "TestA failed in a/a_test.go", "a")
	r.tick(time.Second)
	r.wantLine("a", state.LineReturned)
	r.wantLine("b", state.LineReturned)
	s := r.state()
	if s.Lines["a"].Cause != state.CauseCharged || s.Lines["b"].Cause != state.CauseBaseLost {
		t.Fatalf("a went back %s, b %s", s.Lines["a"].Cause, s.Lines["b"].Cause)
	}
	if len(r.w.NoticesTo("owner", "its base a did not land")) != 1 {
		t.Fatal("b's owner was not told why")
	}
	r.w.Pass(r.trainOf("c"))
	r.tick(time.Second)
	r.wantLine("c", state.LineLanded)
}

// TestBlamedStackLeavesItsBase: blame names only the stacked line; its base
// lands with the rest.
func TestBlamedStackLeavesItsBase(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.stack("b", "a")
	r.together("a", "b")
	r.w.Fail(r.trainOf("a"), "TestB failed in b/b_test.go", "b")
	r.tick(time.Second)
	r.wantLine("b", state.LineReturned)
	r.w.Pass(r.trainOf("a"))
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
}

// TestWithdrawnBaseReturnsItsWaitingStack: the base is withdrawn while the
// line stacked on it waits; that line goes back, uncharged.
func TestWithdrawnBaseReturnsItsWaitingStack(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.stack("b", "a")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather"})
	r.queue("a")
	r.queue("b")
	r.exec(daemon.Request{Cmd: "withdraw", Args: []string{"a"}})
	r.wantLine("a", state.LineDropped)
	r.wantLine("b", state.LineReturned)
}

// TestCoRequiresGroupLandsAndReturnsWhole: a line of two changes is a
// declared group: it rides, is charged and lands as one.
func TestCoRequiresGroupLandsAndReturnsWhole(t *testing.T) {
	r := newRig(t)
	r.open("g1", "c")
	r.stack("g2", "g1")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather"})
	r.queue("g1", "g2")
	r.queue("c")
	r.exec(daemon.Request{Cmd: "resume"})
	t1 := r.trainOf("g1")
	r.w.Fail(t1, "TestG failed in g/g_test.go", "g1")
	r.tick(time.Second)
	if l := r.state().Lines["g1"]; l.State != state.LineReturned || len(l.Changes) != 2 {
		t.Fatalf("the group is %s with %d changes", l.State, len(l.Changes))
	}
	// Charged, the group is with its owner. Offered again as it stands it is
	// refused: a fix is a push.
	if msg := r.execErr(daemon.Request{Cmd: "queue", Args: []string{"g1", "g2"}}); !strings.Contains(msg, "charged") {
		t.Fatalf("a charged line was offered again unchanged: %q", msg)
	}
	r.w.Change("g2", func(c *fake.Change) { c.Head = "g2-2" })
	r.exec(daemon.Request{Cmd: "queue", Args: []string{"g1", "g2"}})
	tg := r.trainOf("g1")
	r.w.Pass(r.trainOf("c"))
	r.w.Pass(tg)
	r.tick(time.Second)
	r.wantLine("g1", state.LineLanded)
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

func TestEndedTrainsArePrunedAndALandedBaseIsRetargeted(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.queue("a")
	t1 := r.trainOf("a")
	r.w.Pass(t1)
	r.tick(time.Second)
	r.wantTrain(t1, state.TrainLanded)
	prunes := r.w.Prunes()
	if len(prunes) == 0 {
		t.Fatal("no prune after a train ended")
	}
	if last := prunes[len(prunes)-1]; len(last) != 0 {
		t.Fatalf("the last prune kept %v live", last)
	}
	// Nothing changed: no second prune until pruneEvery has passed.
	n := len(prunes)
	r.tick(time.Second)
	if len(r.w.Prunes()) != n {
		t.Fatal("a prune with nothing changed")
	}
	r.tick(11 * time.Minute)
	if len(r.w.Prunes()) != n+1 {
		t.Fatal("no periodic prune")
	}
	// b is stacked on a, which has landed and is in no live line: the gate
	// takes it and moves it onto trunk (open question 21).
	r.w.Open("b", "b-1", "a")
	r.queue("b")
	if got := r.w.Retargeted; len(got) != 1 || got[0] != "b" {
		t.Fatalf("retargeted: %v", got)
	}
	r.w.Pass(r.trainOf("b"))
	r.tick(time.Second)
	r.wantLine("b", state.LineLanded)
}

// The rollback path: export as line files, and import them into another
// queue on the same forge. Heads and bases come from the forge again.
func TestExportImportLineFiles(t *testing.T) {
	r := newRig(t)
	r.open("a", "c")
	r.w.Open("b", "b-1", "a") // b is stacked on a
	r.exec(daemon.Request{Cmd: "pause", Reason: "cut-over"})
	r.queue("a")
	r.queue("b")
	r.exec(daemon.Request{Cmd: "queue", Args: []string{"c"}, Alone: true})
	r.exec(daemon.Request{Cmd: "hold", Args: []string{"a"}, Reason: "waits on a design call"})
	resp := r.d.Exec(r.ctx, daemon.Request{Cmd: "export"})
	lf, err := store.ToLines(*resp.Doc)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := lf.Write(dir); err != nil {
		t.Fatal(err)
	}
	back, err := store.ReadLines(dir)
	if err != nil {
		t.Fatal(err)
	}
	r2 := newRig(t)
	r2.w = r.w
	r2.restart()
	r2.exec(daemon.Request{Cmd: "pause", Reason: "import"})
	r2.exec(daemon.Request{Cmd: "import", Plain: &back})
	r2.wantLine("a", state.LineHeld)
	r2.wantLine("b", state.LineWaiting)
	r2.wantLine("c", state.LineWaiting)
	s := r2.state()
	if s.Lines["b"].Base != "a" || !s.Lines["c"].Alone || s.Lines["a"].Reason != "waits on a design call" {
		t.Fatalf("not carried: b on %q, c alone %v, a's reason %q", s.Lines["b"].Base, s.Lines["c"].Alone, s.Lines["a"].Reason)
	}
}

// TestSecondFlakeIsBlamedByFiles: a known flake gets one rerun (§5.5). When
// the rerun fails the same way, that failure is evidence in its own right:
// blame is asked again with the class forced to deterministic, and a line
// it names goes back to its owner without a judge (open question 3).
func TestSecondFlakeIsBlamedByFiles(t *testing.T) {
	r := newRig(t)
	r.w.ReblameHook = func(id state.TrainID, rep adapter.Report, lines []adapter.BuildLine) adapter.Report {
		if rep.Outcome != adapter.Failed || !strings.Contains(rep.Evidence, "failed again after its one rerun") {
			t.Errorf("a forced blame was asked about %+v", rep)
		}
		rep.Blamed = []state.LineID{"b"}
		rep.Evidence += "; blame: b touches b/slow_test.go"
		return rep
	}
	r.open("a", "b")
	r.together("a", "b")
	t1 := r.trainOf("a")
	flake := adapter.Report{Outcome: adapter.Flaky, Head: r.train(t1).Head, Why: "only timeouts: TestSlow"}
	r.w.Report(t1, flake)
	r.tick(time.Second)
	r.wantTrain(t1, state.TrainTesting)
	if r.w.Count("rerun", true) != 1 || r.w.Count("reblame", false) != 0 {
		t.Fatalf("the first flake: %d reruns, %d forced blames; want 1 and 0", r.w.Count("rerun", true), r.w.Count("reblame", false))
	}
	r.w.Report(t1, flake)
	r.tick(time.Second)
	r.wantTrain(t1, state.TrainFailed)
	r.wantLine("b", state.LineReturned)
	if r.w.Count("reblame", false) != 1 || r.w.Count("rerun", true) != 1 {
		t.Fatalf("the second flake: %d forced blames, %d reruns; want 1 and 1", r.w.Count("reblame", false), r.w.Count("rerun", true))
	}
	if len(r.w.NoticesTo("owner", "failed again after its one rerun")) != 1 {
		t.Fatal("b's owner was not told, with the evidence")
	}
	if len(r.w.NoticesTo("judge", "J1")) != 0 {
		t.Fatal("the judge was asked about a failure blame named")
	}
	t2 := r.trainOf("a")
	r.w.Pass(t2)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
}

// TestSecondFlakeThatNamesNobodyGoesToJ1: a forced blame that names no line
// leaves the failure unblamed, and J1 decides, as before.
func TestSecondFlakeThatNamesNobodyGoesToJ1(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.together("a", "b")
	t1 := r.trainOf("a")
	flake := adapter.Report{Outcome: adapter.Flaky, Head: r.train(t1).Head, Why: "TestShared is a known flake"}
	r.w.Report(t1, flake)
	r.tick(time.Second)
	r.w.Report(t1, flake)
	r.tick(time.Second)
	r.wantTrain(t1, state.TrainJudging)
	if r.w.Count("reblame", false) != 1 {
		t.Fatalf("blame was asked again %d times, want 1", r.w.Count("reblame", false))
	}
	if len(r.w.NoticesTo("judge", "J1")) != 1 {
		t.Fatal("the judge was not asked at J1")
	}
	if tr := r.train(t1); !strings.Contains(tr.Reason, "failed again after its one rerun") {
		t.Fatalf("the failure's evidence is %q", tr.Reason)
	}
}

// TestStaleWhenItsBaseIsPushed: a push to a base leaves the line stacked on
// it holding the base's old head. The runner reads the needs again, finds
// the line stale, tells its owner, and keeps it out of trains; a rebase
// onto the new head clears it (§5.2).
func TestStaleWhenItsBaseIsPushed(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.stack("b", "a")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather"})
	r.queue("a")
	r.queue("b")
	r.w.Change("a", func(c *fake.Change) { c.Head = "a-2" })
	r.tick(time.Minute)
	s := r.state()
	if s.Lines["a"].Changes[0].Head != "a-2" || !strings.Contains(s.Lines["b"].Stale, "doesn't contain its base a") {
		t.Fatalf("a at %s; b stale %q", s.Lines["a"].Changes[0].Head, s.Lines["b"].Stale)
	}
	if len(r.w.NoticesTo("owner", "stale, waits for a rebase")) != 1 {
		t.Fatal("b's owner was not told")
	}
	r.exec(daemon.Request{Cmd: "resume"})
	ta := r.trainOf("a")
	r.wantLine("b", state.LineWaiting)
	// b is rebased onto a's new head: fresh, and it rides behind a.
	r.w.Change("b", func(c *fake.Change) { c.Head, c.Parent = "b-2", "a-2" })
	r.tick(time.Minute)
	if st := r.state().Lines["b"]; st.Stale != "" || st.Changes[0].Head != "b-2" {
		t.Fatalf("b is still stale (%q) at %s", st.Stale, st.Changes[0].Head)
	}
	tb := r.trainOf("b")
	if tb == ta || r.train(tb).BaseHead != r.train(ta).Head {
		t.Fatalf("b rides %s on %s", tb, r.train(tb).BaseHead)
	}
}

// TestStaleAtTheGateWhenItCarriesALiveChange: a change that targets trunk
// but carries a live change's commits is taken stale at the gate.
func TestStaleAtTheGateWhenItCarriesALiveChange(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.w.Change("b", func(c *fake.Change) { c.Parent = "a-1" })
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather"})
	r.queue("a")
	r.queue("b")
	if s := r.state().Lines["b"].Stale; !strings.Contains(s, "carries change a's commits") {
		t.Fatalf("b's staleness: %q", s)
	}
	r.exec(daemon.Request{Cmd: "resume"})
	if r.trainOf("a") == "" || r.line("b") != state.LineWaiting {
		t.Fatalf("b is %s", r.line("b"))
	}
	if st := r.exec(daemon.Request{Cmd: "status"}); !strings.Contains(st, "STALE") {
		t.Fatalf("the status doesn't show b stale:\n%s", st)
	}
}

// alerted waits for an alert to the judge that holds s. An alert is sent from
// a goroutine of its own, so it may arrive a moment after the tick that
// raised it.
func (r *rig) alerted(s string) bool {
	r.t.Helper()
	for i := 0; i < 400; i++ {
		if len(r.w.NoticesTo("judge", s)) > 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestAParkOnALandingNeverEndsOnAClock: a park whose condition the runner can
// test ("landed:<line>") ends when that line lands, or by the judge's
// restore. Its deadline restores nothing: a wait that acted on its timeout
// released lines whose fix had not landed. If the line it waits for is spent
// without landing, the judge is told that the wait cannot end.
func TestAParkOnALandingNeverEndsOnAClock(t *testing.T) {
	r := newRig(t)
	r.open("a", "fix")
	r.queue("a")
	r.exec(daemon.Request{Cmd: "cancel", Args: []string{"a"}, Action: "park", Reason: "wait for the fix", Until: "landed:fix"})
	r.wantLine("a", state.LineParked)
	// The deadline passes: nothing is restored, and the judge is told.
	r.tick(25 * time.Hour)
	r.wantLine("a", state.LineParked)
	if !r.alerted("stays parked until that line lands") {
		t.Fatal("no alert when the park passed its deadline")
	}
	// The fix is offered and then withdrawn: it will not land.
	r.queue("fix")
	r.exec(daemon.Request{Cmd: "withdraw", Args: []string{"fix"}})
	r.tick(time.Minute)
	r.wantLine("a", state.LineParked)
	if !r.alerted("the wait cannot end") {
		t.Fatalf("no alert when the awaited line was spent without landing\n%s\nfix is %s", daemon.Status(r.state(), nil), r.line("fix"))
	}
	// Only the judge's restore ends it.
	var park string
	for id := range r.state().Parks {
		park = string(id)
	}
	r.exec(daemon.Request{Cmd: "restore", Args: []string{park}, Reason: "the fix was abandoned; a rides as it is"})
	if r.line("a") == state.LineParked {
		t.Fatal("the judge's restore did not end the park")
	}
}

// TestAParkOnALandingEndsWhenThatLineLands is the other half: the condition,
// not a clock, ends the park.
func TestAParkOnALandingEndsWhenThatLineLands(t *testing.T) {
	r := newRig(t)
	r.open("a", "fix")
	r.queue("a")
	r.exec(daemon.Request{Cmd: "cancel", Args: []string{"a"}, Action: "park", Reason: "wait for the fix", Until: "landed:fix"})
	r.wantLine("a", state.LineParked)
	r.queue("fix")
	r.w.Pass(r.trainOf("fix"))
	r.tick(time.Second)
	r.tick(time.Second)
	r.wantLine("fix", state.LineLanded)
	r.tick(time.Second)
	if r.line("a") == state.LineParked {
		t.Fatalf("the park did not end when its line landed\n%s", daemon.Status(r.state(), nil))
	}
}

// TestARecordedFlakeThatFailsTwiceChargesNobody: a recorded flake gets its
// one rerun (§5.5). When it fails again, nothing is judged, even for a train
// of one line: blame is not asked, nobody is charged, J1 is not opened, and
// the line rides again.
func TestARecordedFlakeThatFailsTwiceChargesNobody(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.queue("a")
	t1 := r.trainOf("a")
	flake := adapter.Report{Outcome: adapter.Flaky, KnownFlake: true, Head: r.train(t1).Head, Why: "TestShared is a recorded flake"}
	r.w.Report(t1, flake)
	r.tick(time.Second)
	r.w.Report(t1, flake)
	r.tick(time.Second)
	r.wantTrain(t1, state.TrainVoid)
	if n := r.w.Count("reblame", false); n != 0 {
		t.Fatalf("blame was asked %d times for a recorded flake", n)
	}
	if len(r.w.NoticesTo("judge", "J1")) != 0 {
		t.Fatal("J1 was opened for a recorded flake")
	}
	l := r.state().Lines["a"]
	if l.State == state.LineReturned || l.Cause == state.CauseCharged {
		t.Fatalf("line a is %s (%s): a recorded flake charged it", l.State, l.Cause)
	}
	if t2 := r.trainOf("a"); t2 == t1 || t2 == "" {
		t.Fatalf("line a does not ride again: train %q", t2)
	}
}

// TestALineInConflictIsRebasedByTheRunner: a line whose check finds that it
// does not merge onto trunk is rebased by the runner where that needs no
// person (§5.2): the new head is recorded, and the line rides once it is
// checked again. A conflict that needs a person goes to the line's owner,
// uncharged, and the line stays out of trains until a push.
func TestALineInConflictIsRebasedByTheRunner(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.exec(daemon.Request{Cmd: "pause", Reason: "check before any train"})
	r.queue("a")
	r.queue("b")
	r.w.Rebases["a"] = adapter.RebaseResult{Heads: map[state.ChangeID]state.Head{"a": "a-2"}}
	r.exec(daemon.Request{Cmd: "checked", Args: []string{"a", "T0", "a-1"}, Action: "conflict", Reason: "gen/api.go"})
	r.exec(daemon.Request{Cmd: "checked", Args: []string{"b", "T0", "b-1"}, Action: "conflict", Reason: "x.go"})
	r.tick(time.Second)
	a, b := r.state().Lines["a"], r.state().Lines["b"]
	if a.Changes[0].Head != "a-2" || a.Conflict != "" {
		t.Fatalf("line a was not rebased: head %s, conflict %q", a.Changes[0].Head, a.Conflict)
	}
	if b.Conflict == "" || b.State != state.LineWaiting || b.Cause == state.CauseCharged {
		t.Fatalf("line b: conflict %q, %s (%s)", b.Conflict, b.State, b.Cause)
	}
	if n := r.w.Count("rebase", true); n != 2 {
		t.Fatalf("%d rebases, want one a line", n)
	}
	r.exec(daemon.Request{Cmd: "resume"})
	if r.line("a") != state.LineRiding {
		t.Fatalf("the rebased line does not ride: %s", r.line("a"))
	}
	if r.line("b") == state.LineRiding {
		t.Fatal("a line in conflict rides")
	}
}

// TestAPauseDrainsTheQueueForAReload: to replace the runner between trains
// (§7), pause it. A pause starts no new train, and the trains in flight
// finish and land; lines offered meanwhile wait, in order. The runner can
// then be stopped and started at any point, and a resume takes the waiting
// lines up. Nothing is held by hand and nothing is killed mid-train.
func TestAPauseDrainsTheQueueForAReload(t *testing.T) {
	r := newRig(t)
	r.open("a", "b", "c")
	r.queue("a")
	t1 := r.trainOf("a")
	r.exec(daemon.Request{Cmd: "pause", Reason: "drain for a reload"})
	r.queue("b")
	r.queue("c")
	for _, id := range []string{"b", "c"} {
		if st := r.line(id); st != state.LineWaiting {
			t.Fatalf("line %s is %s while the queue drains; want waiting", id, st)
		}
	}
	r.w.Pass(t1)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
	if n := len(r.state().Chain); n != 0 {
		t.Fatalf("%d trains ride after the drain", n)
	}
	r.restart()
	for _, id := range []string{"b", "c"} {
		if st := r.line(id); st != state.LineWaiting {
			t.Fatalf("line %s is %s after the reload, before the resume", id, st)
		}
	}
	r.exec(daemon.Request{Cmd: "resume"})
	if r.line("b") != state.LineRiding {
		t.Fatalf("line b does not ride after the resume: %s", r.line("b"))
	}
	if sb, sc := r.state().Lines["b"].Seq, r.state().Lines["c"].Seq; sb >= sc {
		t.Fatalf("the waiting lines lost their order: b %d, c %d", sb, sc)
	}
}

// TestAnInertLineTakesTheLightTierAndJoinsAnyTrain: the gate classifies a
// line by the whole list of files its changes touch (§5.4). A line of inert
// content only is inert: it counts against no cap of a train, and a train of
// nothing else is light. Anything short of that is full: one file that is
// not inert, a file list the forge cannot vouch for, a push.
func TestAnInertLineTakesTheLightTierAndJoinsAnyTrain(t *testing.T) {
	r := newRigWith(t, 2, tier.Rules{Inert: []string{"docs/**", "**/*.md"}, Read: []string{"docs/generated/**"}})
	files := map[string][]string{
		"code1": {"pkg/a.go"},
		"code2": {"pkg/b.go"},
		"doc1":  {"docs/a.md"},
		"doc2":  {"docs/b.md", "NOTES.md"},
		"mixed": {"docs/c.md", "pkg/c.go"},
		"read":  {"docs/generated/api.md"},
		"blind": {"docs/d.md"},
	}
	order := []string{"code1", "code2", "doc1", "doc2", "mixed", "read", "blind"}
	r.open(order...)
	for id, fs := range files {
		r.w.Change(state.ChangeID(id), func(c *fake.Change) { c.Files = fs })
	}
	r.w.Change("blind", func(c *fake.Change) { c.FilesErr = errors.New("the list is not whole") })
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather"})
	for _, id := range order {
		r.queue(id)
	}
	want := map[string]bool{"doc1": true, "doc2": true}
	for _, id := range order {
		if got := r.state().Lines[state.LineID(id)].Inert; got != want[id] {
			t.Errorf("line %s: inert %v, want %v", id, got, want[id])
		}
	}
	r.exec(daemon.Request{Cmd: "resume"})
	// The cap is two lines: the two code lines fill it, and both documents
	// ride with them all the same. The train is full: it holds code.
	t1 := r.trainOf("code1")
	rides := func(id string) state.TrainID { return r.state().Lines[state.LineID(id)].Train }
	for _, id := range []string{"code2", "doc1", "doc2"} {
		if rides(id) != t1 {
			t.Errorf("line %s does not ride train %s with the lines it could join\n%s", id, t1, daemon.Status(r.state(), nil))
		}
	}
	for _, id := range []string{"mixed", "read", "blind"} {
		if rides(id) == t1 {
			t.Errorf("line %s is past the cap and not inert, and rides train %s", id, t1)
		}
	}
	if r.train(t1).Light {
		t.Errorf("train %s holds code and is light", t1)
	}
}

// TestATrainOfInertLinesOnlyIsLight. (That a push ends a line's inertness is
// the machine's rule, tested there: TestAPushMakesAnInertLineFull.)
func TestATrainOfInertLinesOnlyIsLight(t *testing.T) {
	r := newRigWith(t, 8, tier.Rules{Inert: []string{"docs/**"}})
	r.open("doc1", "doc2")
	r.w.Change("doc1", func(c *fake.Change) { c.Files = []string{"docs/a.md"} })
	r.w.Change("doc2", func(c *fake.Change) { c.Files = []string{"docs/b.md"} })
	r.together("doc1", "doc2")
	t1 := r.trainOf("doc1")
	if r.trainOf("doc2") != t1 || !r.train(t1).Light {
		t.Fatalf("the documents' train: light %v\n%s", r.train(t1).Light, daemon.Status(r.state(), nil))
	}
}
