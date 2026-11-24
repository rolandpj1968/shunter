package state

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// Helpers that build event sequences. Line "a" has one change "a" at "a1".

func offer(id string) Event {
	return Offer{Line: LineID(id), Changes: []Change{{ID: ChangeID(id), Head: Head(id + "1")}}}
}

func stacked(id, base string) Event {
	o := offer(id).(Offer)
	o.Base = LineID(base)
	return o
}

func ready(ids ...string) []Event {
	var evs []Event
	for _, id := range ids {
		evs = append(evs, offer(id), GateAccept{Line: LineID(id)})
	}
	return evs
}

func ids(s ...string) []LineID {
	out := make([]LineID, len(s))
	for i, x := range s {
		out[i] = LineID(x)
	}
	return out
}

func assemble(train string, lines ...string) Event {
	return Assemble{Train: TrainID(train), Lines: ids(lines...)}
}

// built, testing and green take a train to that state on head "h-<train>".
func built(train string, lines ...string) []Event {
	return []Event{assemble(train, lines...), Built{Train: TrainID(train), Head: Head("h-" + train)}}
}

func testing_(train string, lines ...string) []Event {
	return append(built(train, lines...), Opened{Train: TrainID(train), PR: "pr-" + train, Head: Head("h-" + train)})
}

func green(train string, lines ...string) []Event {
	return append(testing_(train, lines...), CIPassed{Train: TrainID(train), Head: Head("h-" + train)})
}

// verify is the lander's reading when everything agrees with the state.
func verify(s State, train string) Event {
	t := s.Trains[TrainID(train)]
	heads := map[ChangeID]Head{}
	for _, id := range t.Lines {
		for _, c := range s.Lines[id].Changes {
			heads[c.ID] = c.Head
		}
	}
	return LandingVerified{Train: t.ID, Verdict: t.Head, Heads: heads, Trunk: s.Trunk}
}

// lazy is an event that needs the state it is applied to.
type lazy func(State) Event

func (lazy) event() string { return "lazy" }

// seed changes the state by hand, for a state no event reaches any more: a
// line queued before needs first, in the retired waiting-for-base state.
type seed func(*State)

func (seed) event() string { return "seed" }

func legacy(id, base string) Event {
	return seed(func(s *State) {
		s.Lines[LineID(id)] = &Line{ID: LineID(id), State: LineWaitingBase, Seq: s.NextSeq, Base: LineID(base), Changes: []Change{{ID: ChangeID(id), Head: Head(id + "1")}}}
		s.NextSeq++
	})
}

func verifyOf(train string) Event { return lazy(func(s State) Event { return verify(s, train) }) }

func landing(train string, lines ...string) []Event {
	return append(green(train, lines...), verifyOf(train))
}

func cat(parts ...any) []Event {
	var out []Event
	for _, p := range parts {
		switch v := p.(type) {
		case []Event:
			out = append(out, v...)
		case Event:
			out = append(out, v)
		}
	}
	return out
}

type harness struct {
	t    *testing.T
	s    State
	seen map[string]bool // the table rows exercised
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, s: New("trunk0", Config{Depth: 2, NoVerdictBudget: 2, PushRetries: 2}), seen: map[string]bool{}}
}

// step applies one event, checks that Next left its argument alone, and
// checks every invariant on the result.
func (h *harness) step(ev Event) ([]Effect, error) {
	h.t.Helper()
	if l, ok := ev.(lazy); ok {
		ev = l(h.s)
	}
	if sd, ok := ev.(seed); ok {
		next := h.s.clone()
		sd(&next)
		h.s = next
		return nil, nil
	}
	keep := h.s.clone()
	next, fx, err := Next(h.s, ev)
	if !reflect.DeepEqual(keep, h.s.clone()) {
		h.t.Fatalf("%s: Next changed its argument", Name(ev))
	}
	if err != nil {
		if !reflect.DeepEqual(next.clone(), keep) || fx != nil {
			h.t.Fatalf("%s: a refused event changed the state or had effects", Name(ev))
		}
		return nil, err
	}
	for _, e := range Check(next) {
		h.t.Errorf("after %s: %v", Name(ev), e)
	}
	for _, e := range CheckStep(h.s, next, fx) {
		h.t.Errorf("step %s: %v", Name(ev), e)
	}
	for _, e := range fx {
		if e.Kind == EffRecord {
			subject := sLine
			if e.Line == "" {
				subject = sTrain
			}
			h.seen[key(subject, e.From, e.To, e.Cause)] = true
		}
	}
	h.s = next
	return fx, nil
}

func (h *harness) must(evs ...Event) {
	h.t.Helper()
	for i, ev := range evs {
		if _, err := h.step(ev); err != nil {
			h.t.Fatalf("setup step %d: %v", i, err)
		}
	}
}

type lineWant map[string]LineState
type trainWant map[string]TrainState

type tcase struct {
	name   string
	setup  []Event
	ev     Event
	err    error // the refusal expected, if any
	lines  lineWant
	trains trainWant
	fx     []EffectKind // effects that must be among those returned
	check  func(*testing.T, State)
}

var allPark = ParkLines{Park: "p", Cond: "fix f has landed", Reason: "trunk is broken"}

func fix(id string) Event {
	o := offer(id).(Offer)
	o.Fix = "p"
	return o
}

// transitions has at least one case for every row of Table.
// TestEveryTransition fails if a row is not exercised.
var transitions = []tcase{
	{name: "line check fails: the waiting line is charged", setup: ready("a"), ev: LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Evidence: "lint fails on the line; it passes on trunk alone"}, lines: lineWant{"a": LineReturned},
		fx: []EffectKind{EffNotify}, check: func(t *testing.T, s State) {
			if s.Lines["a"].Cause != CauseCharged {
				t.Errorf("cause %s", s.Lines["a"].Cause)
			}
		}},

	// A line: offer and the gate.
	{name: "offer", ev: offer("a"), lines: lineWant{"a": LineOffered}},
	{name: "offer again after a refusal", setup: cat(offer("a"), GateRefuse{Line: "a", Reason: "not approved"}), ev: offer("a"), lines: lineWant{"a": LineOffered}},
	{name: "offer again after a withdrawal", setup: cat(offer("a"), Withdraw{Line: "a"}), ev: offer("a"), lines: lineWant{"a": LineOffered}},
	{name: "gate accepts", setup: cat(offer("a")), ev: GateAccept{Line: "a"}, lines: lineWant{"a": LineWaiting}},
	{name: "gate accepts, base is live: it waits in the queue", setup: cat(ready("a"), stacked("b", "a")), ev: GateAccept{Line: "b"}, lines: lineWant{"b": LineWaiting}},
	{name: "gate accepts, queue is parked", setup: cat(allPark, offer("a")), ev: GateAccept{Line: "a"}, lines: lineWant{"a": LineParked}, fx: []EffectKind{EffNotify}},
	{name: "gate accepts the park's fix", setup: cat(allPark, fix("f")), ev: GateAccept{Line: "f"}, lines: lineWant{"f": LineWaiting}},
	{name: "gate refuses", setup: cat(offer("a")), ev: GateRefuse{Line: "a", Reason: "not approved"}, lines: lineWant{"a": LineReturned}, fx: []EffectKind{EffNotify}},

	// Hold and release.
	{name: "hold a waiting line", setup: ready("a"), ev: Hold{Line: "a", Reason: "review says no"}, lines: lineWant{"a": LineHeld}, fx: []EffectKind{EffNotify}},
	{name: "hold a line waiting for its base (retired)", setup: cat(ready("a"), legacy("b", "a")), ev: Hold{Line: "b", Reason: "r"}, lines: lineWant{"b": LineHeld}},
	{name: "hold a parked line", setup: cat(ready("a"), allPark), ev: Hold{Line: "a", Reason: "r"}, lines: lineWant{"a": LineHeld}},
	{name: "hold a riding line voids its train", setup: cat(ready("a", "b"), testing_("t1", "a", "b")), ev: Hold{Line: "a", Reason: "r"},
		lines: lineWant{"a": LineHeld, "b": LineWaiting}, trains: trainWant{"t1": TrainVoid}, fx: []EffectKind{EffCancelRun, EffClosePR}},
	{name: "release", setup: cat(ready("a"), Hold{Line: "a", Reason: "r"}), ev: Release{Line: "a"}, lines: lineWant{"a": LineWaiting}},
	{name: "release behind a live base", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}, Hold{Line: "b", Reason: "r"}), ev: Release{Line: "b"}, lines: lineWant{"b": LineWaiting}},
	{name: "release into a park", setup: cat(ready("a"), allPark, Hold{Line: "a", Reason: "r"}), ev: Release{Line: "a"}, lines: lineWant{"a": LineParked}},
	{name: "release after the park was restored", setup: cat(ready("a"), allPark, Hold{Line: "a", Reason: "r"}, Restore{Park: "p"}), ev: Release{Line: "a"}, lines: lineWant{"a": LineWaiting}},
	{name: "release after the base was lost", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}, Hold{Line: "b", Reason: "r"}, Withdraw{Line: "a"}), ev: Release{Line: "b"}, lines: lineWant{"b": LineReturned}},

	// Park and restore.
	{name: "park the queue", setup: ready("a", "b"), ev: allPark, lines: lineWant{"a": LineParked, "b": LineParked}},
	{name: "park named lines", setup: ready("a", "b"), ev: ParkLines{Park: "q", Lines: ids("a"), Cond: "c", Reason: "r"}, lines: lineWant{"a": LineParked, "b": LineWaiting}},
	{name: "restore", setup: cat(ready("a", "b"), allPark), ev: Restore{Park: "p"}, lines: lineWant{"a": LineWaiting, "b": LineWaiting},
		check: func(t *testing.T, s State) {
			if w := s.Waiting(); len(w) != 2 || w[0] != "a" {
				t.Errorf("restored out of order: %v", w)
			}
		}},

	// Riding.
	{name: "join", setup: ready("a", "b"), ev: assemble("t1", "a", "b"), lines: lineWant{"a": LineRiding, "b": LineRiding}, trains: trainWant{"t1": TrainAssembling}},
	{name: "left out", setup: cat(ready("a", "b"), assemble("t1", "a", "b")), ev: LeaveOut{Train: "t1", Line: "a", Reason: "conflict in x"},
		lines: lineWant{"a": LineWaiting, "b": LineRiding}, trains: trainWant{"t1": TrainAssembling}, check: func(t *testing.T, s State) {
			if l := s.Lines["a"]; l.Count != 1 || l.Cause != CauseLeftOut {
				t.Errorf("left out: count %d, cause %s", l.Count, l.Cause)
			}
		}},
	{name: "every line left out", setup: cat(ready("a"), assemble("t1", "a")), ev: LeaveOut{Train: "t1", Line: "a", Reason: "conflict in x"},
		lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainEmpty}},
	{name: "pre-flight blames one", setup: cat(ready("a", "b"), assemble("t1", "a", "b")), ev: PreflightFailed{Train: "t1", Blamed: ids("a"), Evidence: "lint at a line a added"},
		lines: lineWant{"a": LineReturned, "b": LineWaiting}, trains: trainWant{"t1": TrainFailed}},
	{name: "pre-flight blames none", setup: cat(ready("a", "b"), assemble("t1", "a", "b")), ev: PreflightFailed{Train: "t1"},
		lines: lineWant{"a": LineRiding, "b": LineRiding}, trains: trainWant{"t1": TrainJudging}, fx: []EffectKind{EffAskJudge}},
	{name: "built", setup: cat(ready("a"), assemble("t1", "a")), ev: Built{Train: "t1", Head: "h1"}, trains: trainWant{"t1": TrainBuilt}, fx: []EffectKind{EffOpenPR}},
	{name: "opened", setup: cat(ready("a"), built("t1", "a")), ev: Opened{Train: "t1", PR: "9", Head: "h-t1"}, trains: trainWant{"t1": TrainTesting}},
	{name: "CI passes", setup: cat(ready("a"), testing_("t1", "a")), ev: CIPassed{Train: "t1", Head: "h-t1"}, trains: trainWant{"t1": TrainGreen}, fx: []EffectKind{EffVerify}},
	{name: "CI fails, blame names one", setup: cat(ready("a", "b"), testing_("t1", "a", "b")), ev: CIFailed{Train: "t1", Blamed: ids("b"), Evidence: "failing test in a file b touches"},
		lines: lineWant{"a": LineWaiting, "b": LineReturned}, trains: trainWant{"t1": TrainFailed}, fx: []EffectKind{EffNotify, EffClosePR}},
	{name: "CI fails, blame names the only line", setup: cat(ready("a"), testing_("t1", "a")), ev: CIFailed{Train: "t1", Blamed: ids("a"), Evidence: "e"},
		lines: lineWant{"a": LineReturned}, trains: trainWant{"t1": TrainFailed}},
	{name: "CI fails, blame names none", setup: cat(ready("a", "b"), testing_("t1", "a", "b")), ev: CIFailed{Train: "t1"},
		lines: lineWant{"a": LineRiding, "b": LineRiding}, trains: trainWant{"t1": TrainJudging}, fx: []EffectKind{EffAskJudge}},
	{name: "CI fails, blame names all of several", setup: cat(ready("a", "b"), testing_("t1", "a", "b")), ev: CIFailed{Train: "t1", Blamed: ids("a", "b"), Evidence: "e"},
		trains: trainWant{"t1": TrainJudging}},
	{name: "rerun after a flake", setup: cat(ready("a"), testing_("t1", "a")), ev: CIRerun{Train: "t1", Why: "known flake"}, trains: trainWant{"t1": TrainTesting}, fx: []EffectKind{EffRerun}},

	// J1.
	{name: "judge charges", setup: cat(ready("a", "b"), testing_("t1", "a", "b"), CIFailed{Train: "t1"}), ev: Judge{Train: "t1", Action: JudgeCharge, Lines: ids("a"), Evidence: "a's description breaks rule r"},
		lines: lineWant{"a": LineReturned, "b": LineWaiting}, trains: trainWant{"t1": TrainFailed}},
	{name: "judge splits", setup: cat(ready("a", "b", "c"), testing_("t1", "a", "b", "c"), CIFailed{Train: "t1"}), ev: Judge{Train: "t1", Action: JudgeSplit},
		lines: lineWant{"a": LineWaiting, "b": LineWaiting, "c": LineWaiting}, trains: trainWant{"t1": TrainFailed},
		check: func(t *testing.T, s State) {
			if s.Lines["a"].Group == 0 || s.Lines["a"].Group != s.Lines["b"].Group || s.Lines["c"].Group == s.Lines["a"].Group {
				t.Errorf("split groups: a=%d b=%d c=%d", s.Lines["a"].Group, s.Lines["b"].Group, s.Lines["c"].Group)
			}
		}},
	{name: "judge parks", setup: cat(ready("a", "b", "c"), testing_("t1", "a", "b"), CIFailed{Train: "t1"}), ev: Judge{Train: "t1", Action: JudgePark, Park: "p", Cond: "fix landed", Why: "trunk is red"},
		lines: lineWant{"a": LineParked, "b": LineParked, "c": LineParked}, trains: trainWant{"t1": TrainFailed}},
	{name: "judge reruns", setup: cat(ready("a", "b"), testing_("t1", "a", "b"), CIFailed{Train: "t1"}), ev: Judge{Train: "t1", Action: JudgeRerun, Why: "runner was sick"},
		lines: lineWant{"a": LineRiding}, trains: trainWant{"t1": TrainTesting}, fx: []EffectKind{EffRerun}},
	{name: "judge silent: split", setup: cat(ready("a", "b"), testing_("t1", "a", "b"), CIFailed{Train: "t1"}), ev: JudgeTimeout{Train: "t1"},
		lines: lineWant{"a": LineWaiting, "b": LineWaiting}, trains: trainWant{"t1": TrainFailed}},
	{name: "judge silent, same test failed before: park", setup: cat(ready("a", "b"), testing_("t1", "a", "b"), CIFailed{Train: "t1"}), ev: JudgeTimeout{Train: "t1", SameTestBefore: true},
		lines: lineWant{"a": LineParked, "b": LineParked}, trains: trainWant{"t1": TrainFailed}},
	{name: "judge silent on one line: charged by elimination, and the queue is not parked", setup: cat(ready("a", "b"), testing_("t1", "a"), CIFailed{Train: "t1"}), ev: JudgeTimeout{Train: "t1"},
		lines: lineWant{"a": LineReturned, "b": LineWaiting}, trains: trainWant{"t1": TrainFailed},
		check: func(t *testing.T, s State) {
			if l := s.Lines["a"]; l.Cause != CauseCharged || !strings.Contains(l.Reason, "by elimination") {
				t.Errorf("line a: cause %s, reason %q", l.Cause, l.Reason)
			}
			if len(s.Parks) != 0 {
				t.Errorf("the queue was parked: %v", s.Parks)
			}
		}},
	{name: "judge silent on one line, same test failed before: park", setup: cat(ready("a"), testing_("t1", "a"), CIFailed{Train: "t1"}), ev: JudgeTimeout{Train: "t1", SameTestBefore: true},
		lines: lineWant{"a": LineParked}, trains: trainWant{"t1": TrainFailed}},

	// §5.3: only the first train of the chain is judged.
	{name: "train 2 fails behind an undecided train 1: nothing is judged", setup: cat(ready("a", "b"), testing_("t1", "a"), testing_("t2", "b")), ev: CIFailed{Train: "t2", Blamed: ids("b"), Evidence: "b's test fails"},
		lines: lineWant{"a": LineRiding, "b": LineRiding}, trains: trainWant{"t1": TrainTesting, "t2": TrainTesting},
		check: func(t *testing.T, s State) {
			if s.Trains["t2"].Behind == nil {
				t.Error("the failure was not kept")
			}
		}},
	{name: "train 1 then fails: train 2's failure was inherited, and charges nobody", setup: cat(ready("a", "b"), testing_("t1", "a"), testing_("t2", "b"), CIFailed{Train: "t2", Blamed: ids("b"), Evidence: "b's test fails"}),
		ev:    CIFailed{Train: "t1", Blamed: ids("a"), Evidence: "a's test fails"},
		lines: lineWant{"a": LineReturned, "b": LineWaiting}, trains: trainWant{"t1": TrainFailed, "t2": TrainVoid},
		check: func(t *testing.T, s State) {
			if s.Lines["b"].Cause == CauseCharged || s.Trains["t2"].Behind != nil {
				t.Errorf("line b: cause %s; train 2 still holds a failure: %v", s.Lines["b"].Cause, s.Trains["t2"].Behind != nil)
			}
		}},
	{name: "train 1 lands: train 2's failure is its own, and is judged then", setup: cat(ready("a", "b"), testing_("t1", "a"), testing_("t2", "b"), CIFailed{Train: "t2", Blamed: ids("b"), Evidence: "b's test fails"}, CIPassed{Train: "t1", Head: "h-t1"}, verifyOf("t1")),
		ev:    PushOK{Train: "t1"},
		lines: lineWant{"a": LineLanded, "b": LineReturned}, trains: trainWant{"t1": TrainLanded, "t2": TrainFailed},
		check: func(t *testing.T, s State) {
			if s.Lines["b"].Cause != CauseCharged {
				t.Errorf("line b: cause %s", s.Lines["b"].Cause)
			}
		}},
	{name: "train 1 lands: train 2's unblamed failure goes to J1 then", setup: cat(ready("a", "b", "c"), testing_("t1", "a"), testing_("t2", "b", "c"), CIFailed{Train: "t2"}, CIPassed{Train: "t1", Head: "h-t1"}, verifyOf("t1")),
		ev:    PushOK{Train: "t1"},
		lines: lineWant{"b": LineRiding, "c": LineRiding}, trains: trainWant{"t2": TrainJudging}, fx: []EffectKind{EffAskJudge}},
	{name: "a pre-flight failure behind an undecided train is kept too", setup: cat(ready("a", "b"), testing_("t1", "a"), assemble("t2", "b")), ev: PreflightFailed{Train: "t2", Blamed: ids("b"), Evidence: "lint"},
		lines: lineWant{"b": LineRiding}, trains: trainWant{"t2": TrainAssembling}},

	// Void: nothing was judged.
	{name: "no verdict while assembling", setup: cat(ready("a"), assemble("t1", "a")), ev: CINoVerdict{Train: "t1", Why: "the builder died"}, lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainVoid}},
	{name: "no verdict before the PR opened", setup: cat(ready("a"), built("t1", "a")), ev: CINoVerdict{Train: "t1", Infra: true, Why: "push failed"}, lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainVoid}},
	{name: "run cancelled: no verdict, nobody charged", setup: cat(ready("a"), testing_("t1", "a")), ev: CINoVerdict{Train: "t1", Why: "run cancelled"},
		lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainVoid}, fx: []EffectKind{EffCancelRun, EffClosePR}},
	{name: "head moved while riding", setup: cat(ready("a", "b"), testing_("t1", "a", "b")), ev: HeadMoved{Line: "a", Change: "a", Head: "a2"},
		lines: lineWant{"a": LineWaiting, "b": LineWaiting}, trains: trainWant{"t1": TrainVoid},
		check: func(t *testing.T, s State) {
			if s.Lines["a"].Changes[0].Head != "a2" {
				t.Errorf("the new head was not recorded")
			}
		}},
	{name: "member held while judging", setup: cat(ready("a", "b"), testing_("t1", "a", "b"), CIFailed{Train: "t1"}), ev: Hold{Line: "b", Reason: "r"},
		lines: lineWant{"a": LineWaiting, "b": LineHeld}, trains: trainWant{"t1": TrainVoid}},
	{name: "member held while green", setup: cat(ready("a"), green("t1", "a")), ev: Hold{Line: "a", Reason: "r"}, lines: lineWant{"a": LineHeld}, trains: trainWant{"t1": TrainVoid}},
	{name: "requeue into a park", setup: cat(allPark, fix("f"), GateAccept{Line: "f"}, testing_("t1", "f"), Restore{Park: "p"}, ParkLines{Park: "p2", Cond: "c"}),
		ev: CINoVerdict{Train: "t1", Why: "w"}, lines: lineWant{"f": LineParked}, trains: trainWant{"t1": TrainVoid}},

	// Abort.
	{name: "abort while assembling", setup: cat(ready("a"), assemble("t1", "a")), ev: Abort{Train: "t1", Reason: "wrong train"}, lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainAborted}},
	{name: "abort before the PR opened", setup: cat(ready("a"), built("t1", "a")), ev: Abort{Train: "t1", Reason: "r"}, lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainAborted}},
	{name: "abort in CI, each line disposed", setup: cat(ready("a", "b", "c"), testing_("t1", "a", "b", "c")),
		ev:    Abort{Train: "t1", Reason: "r", Lines: map[LineID]Dispose{"a": {Kind: Return}, "b": {Kind: ParkIt, Park: "q", Cond: "the fill has landed"}}},
		lines: lineWant{"a": LineReturned, "b": LineParked, "c": LineWaiting}, trains: trainWant{"t1": TrainAborted}, fx: []EffectKind{EffCancelRun, EffClosePR, EffNotify}},
	{name: "abort while judging", setup: cat(ready("a", "b"), testing_("t1", "a", "b"), CIFailed{Train: "t1"}), ev: Abort{Train: "t1", Reason: "r"}, trains: trainWant{"t1": TrainAborted}},
	{name: "abort a green train", setup: cat(ready("a"), green("t1", "a")), ev: Abort{Train: "t1", Reason: "r"}, lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainAborted}},
	{name: "abort requeues into a park", setup: cat(allPark, fix("f"), GateAccept{Line: "f"}, testing_("t1", "f"), Restore{Park: "p"}, ParkLines{Park: "p2", Cond: "c"}),
		ev: Abort{Train: "t1", Reason: "r"}, lines: lineWant{"f": LineParked}, trains: trainWant{"t1": TrainAborted}},
	{name: "abort twice is abort once", setup: cat(ready("a"), testing_("t1", "a"), Abort{Train: "t1", Reason: "r"}), ev: Abort{Train: "t1", Reason: "again"}, lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainAborted}},

	// Landing.
	{name: "the lander verifies", setup: cat(ready("a"), green("t1", "a")), ev: verifyOf("t1"), trains: trainWant{"t1": TrainLanding}, fx: []EffectKind{EffPush}},
	{name: "verify finds a pass on another head", setup: cat(ready("a"), green("t1", "a")), ev: LandingVerified{Train: "t1", Verdict: "other", Heads: map[ChangeID]Head{"a": "a1"}, Trunk: "trunk0"},
		lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainVoid}},
	{name: "verify finds a moved head", setup: cat(ready("a"), green("t1", "a")), ev: LandingVerified{Train: "t1", Verdict: "h-t1", Heads: map[ChangeID]Head{"a": "a2"}, Trunk: "trunk0"},
		lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainVoid}},
	{name: "verify finds another writer on trunk", setup: cat(ready("a"), green("t1", "a")), ev: LandingVerified{Train: "t1", Verdict: "h-t1", Heads: map[ChangeID]Head{"a": "a1"}, Trunk: "elsewhere"},
		lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainVoid}, fx: []EffectKind{EffAskJudge},
		check: func(t *testing.T, s State) {
			if !s.Paused || s.Trunk != "trunk0" {
				t.Errorf("paused=%v trunk=%s", s.Paused, s.Trunk)
			}
		}},
	{name: "push lands", setup: cat(ready("a", "b"), landing("t1", "a", "b")), ev: PushOK{Train: "t1"}, lines: lineWant{"a": LineLanded, "b": LineLanded}, trains: trainWant{"t1": TrainLanded}, fx: []EffectKind{EffClosePR, EffNotify},
		check: func(t *testing.T, s State) {
			if s.Trunk != "h-t1" || len(s.Landings) != 1 || len(s.Chain) != 0 {
				t.Errorf("trunk=%s landings=%d chain=%d", s.Trunk, len(s.Landings), len(s.Chain))
			}
		}},
	{name: "push refused, transient: retried", setup: cat(ready("a"), landing("t1", "a")), ev: PushRefused{Train: "t1", Why: "503"}, trains: trainWant{"t1": TrainLanding}, fx: []EffectKind{EffPush}},
	{name: "push refused for good: void, no fallback merge", setup: cat(ready("a"), landing("t1", "a")), ev: PushRefused{Train: "t1", Permanent: true, Why: "not a fast-forward"},
		lines: lineWant{"a": LineWaiting}, trains: trainWant{"t1": TrainVoid}, fx: []EffectKind{EffAskJudge},
		check: func(t *testing.T, s State) {
			if s.Trunk != "trunk0" || len(s.Landings) != 0 {
				t.Errorf("trunk moved")
			}
		}},
	{name: "a late push is not hidden", setup: cat(ready("a"), landing("t1", "a"), HeadMoved{Line: "a", Change: "a", Head: "a2"}), ev: PushOK{Train: "t1"}, lines: lineWant{"a": LineLanded},
		check: func(t *testing.T, s State) {
			if !strings.Contains(s.Lines["a"].Reason, "a2") {
				t.Errorf("owner not told of the late push: %q", s.Lines["a"].Reason)
			}
		}},

	// Needs first (I3).
	{name: "a line rides in its base's train, behind it", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}), ev: assemble("t1", "a", "b"), lines: lineWant{"a": LineRiding, "b": LineRiding}},
	{name: "a line rides in the train behind its base's", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}, built("t1", "a")), ev: assemble("t2", "b"), lines: lineWant{"b": LineRiding}, trains: trainWant{"t2": TrainAssembling}},
	{name: "base lands: the line behind it rides on", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}, landing("t1", "a")), ev: PushOK{Train: "t1"}, lines: lineWant{"a": LineLanded, "b": LineWaiting}},
	{name: "base withdrawn: the line behind it returns, uncharged", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}), ev: Withdraw{Line: "a"}, lines: lineWant{"a": LineDropped, "b": LineReturned}, fx: []EffectKind{EffNotify},
		check: func(t *testing.T, s State) {
			if s.Lines["b"].Cause != CauseBaseLost {
				t.Errorf("b went back as %s", s.Lines["b"].Cause)
			}
		}},
	{name: "base charged: the line waiting behind it returns", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}, testing_("t1", "a")), ev: CIFailed{Train: "t1", Blamed: ids("a"), Evidence: "e"}, lines: lineWant{"a": LineReturned, "b": LineReturned}},
	{name: "base charged in the same train: the line behind it goes with it, uncharged", setup: cat(ready("a", "c"), stacked("b", "a"), GateAccept{Line: "b"}, testing_("t1", "a", "b", "c")),
		ev: CIFailed{Train: "t1", Blamed: ids("a"), Evidence: "e"}, lines: lineWant{"a": LineReturned, "b": LineReturned, "c": LineWaiting}, trains: trainWant{"t1": TrainFailed},
		check: func(t *testing.T, s State) {
			if s.Lines["b"].Cause != CauseBaseLost || s.Lines["a"].Cause != CauseCharged {
				t.Errorf("a went back as %s, b as %s", s.Lines["a"].Cause, s.Lines["b"].Cause)
			}
		}},
	{name: "the line behind a blamed one is blamed alone", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}, testing_("t1", "a", "b")),
		ev: CIFailed{Train: "t1", Blamed: ids("b"), Evidence: "e"}, lines: lineWant{"a": LineWaiting, "b": LineReturned}, trains: trainWant{"t1": TrainFailed}},
	{name: "base left out: the line behind it is left out too", setup: cat(ready("a", "c"), stacked("b", "a"), GateAccept{Line: "b"}, stacked("d", "b"), GateAccept{Line: "d"}, assemble("t1", "a", "b", "c", "d")),
		ev: LeaveOut{Train: "t1", Line: "a", Reason: "conflict in x"}, lines: lineWant{"a": LineWaiting, "b": LineWaiting, "c": LineRiding, "d": LineWaiting}, trains: trainWant{"t1": TrainAssembling}},
	{name: "base held while riding: the line behind it waits", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}, testing_("t1", "a", "b")),
		ev: Hold{Line: "a", Reason: "r"}, lines: lineWant{"a": LineHeld, "b": LineWaiting}, trains: trainWant{"t1": TrainVoid}},
	{name: "a co-requires group is charged whole", setup: cat(Offer{Line: "g", Changes: []Change{{ID: "g1", Head: "h1"}, {ID: "g2", Head: "h2"}}}, GateAccept{Line: "g"}, ready("c"), testing_("t1", "g", "c")),
		ev: CIFailed{Train: "t1", Blamed: ids("g"), Evidence: "e"}, lines: lineWant{"g": LineReturned, "c": LineWaiting}},
	{name: "stale, then fresh", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}, Stale{Line: "b", Reason: "its head doesn't hold a's"}), ev: Fresh{Line: "b"}, lines: lineWant{"b": LineWaiting},
		check: func(t *testing.T, s State) {
			if s.Lines["b"].Stale != "" {
				t.Errorf("still stale: %q", s.Lines["b"].Stale)
			}
		}},
	{name: "a trunk conflict the rebase can't resolve goes back", setup: cat(ready("a"), LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk1", Conflict: "x.go"}),
		ev: RebaseFailed{Line: "a", Reason: "both sides changed x.go"}, lines: lineWant{"a": LineReturned}, fx: []EffectKind{EffNotify}},
	{name: "stale: the owner is told", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}), ev: Stale{Line: "b", Reason: "rebase onto a"}, lines: lineWant{"b": LineWaiting}, fx: []EffectKind{EffNotify}},
	{name: "base lands: a retired line behind it queues", setup: cat(ready("a"), legacy("b", "a"), landing("t1", "a")), ev: PushOK{Train: "t1"}, lines: lineWant{"a": LineLanded, "b": LineWaiting}},
	{name: "base lands under a park: a retired line behind it parks", setup: cat(allPark, fix("f"), GateAccept{Line: "f"}, legacy("b", "f"), landing("t1", "f")), ev: PushOK{Train: "t1"}, lines: lineWant{"f": LineLanded, "b": LineParked}},
	{name: "charge void: the line check changed", setup: cat(ready("a"), LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Evidence: "e"}), ev: CheckerChanged{Checker: CheckerLineCheck, Version: "v2"}, lines: lineWant{"a": LineWaiting}},
	{name: "charge void under a park", setup: cat(ready("a"), LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Evidence: "e"}, allPark), ev: CheckerChanged{Checker: CheckerLineCheck, Version: "v2"}, lines: lineWant{"a": LineParked}},
	{name: "base lost: a retired line behind it returns", setup: cat(ready("a"), legacy("b", "a")), ev: Withdraw{Line: "a"}, lines: lineWant{"a": LineDropped, "b": LineReturned}},

	// Withdraw.
	{name: "withdraw an offered line", setup: cat(offer("a")), ev: Withdraw{Line: "a"}, lines: lineWant{"a": LineDropped}},
	{name: "withdraw a waiting line", setup: ready("a"), ev: Withdraw{Line: "a"}, lines: lineWant{"a": LineDropped}},
	{name: "withdraw a parked line", setup: cat(ready("a"), allPark), ev: Withdraw{Line: "a"}, lines: lineWant{"a": LineDropped}},
	{name: "withdraw a riding line", setup: cat(ready("a", "b"), built("t1", "a", "b")), ev: Withdraw{Line: "a"}, lines: lineWant{"a": LineDropped, "b": LineWaiting}, trains: trainWant{"t1": TrainVoid}},
	{name: "withdraw behind a base", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}), ev: Withdraw{Line: "b"}, lines: lineWant{"b": LineDropped}},
	{name: "withdraw a retired line behind a base", setup: cat(ready("a"), legacy("b", "a")), ev: Withdraw{Line: "b"}, lines: lineWant{"b": LineDropped}},

	// Speculation.
	{name: "train 2 on train 1's head", setup: cat(ready("a", "b"), built("t1", "a")), ev: assemble("t2", "b"), trains: trainWant{"t2": TrainAssembling},
		check: func(t *testing.T, s State) {
			if s.Trains["t2"].BaseHead != "h-t1" {
				t.Errorf("train 2 built on %s", s.Trains["t2"].BaseHead)
			}
		}},
	{name: "train 1 fails: train 2 is void", setup: cat(ready("a", "b"), testing_("t1", "a"), green("t2", "b")), ev: CIFailed{Train: "t1", Blamed: ids("a"), Evidence: "e"},
		lines: lineWant{"a": LineReturned, "b": LineWaiting}, trains: trainWant{"t1": TrainFailed, "t2": TrainVoid}},
	{name: "train 1 aborted: train 2 is void", setup: cat(ready("a", "b"), testing_("t1", "a"), testing_("t2", "b")), ev: Abort{Train: "t1", Reason: "r"}, trains: trainWant{"t1": TrainAborted, "t2": TrainVoid}},
	{name: "train 1 lands: green train 2 is checked next", setup: cat(ready("a", "b"), testing_("t1", "a"), green("t2", "b"), CIPassed{Train: "t1", Head: "h-t1"}, verifyOf("t1")), ev: PushOK{Train: "t1"},
		trains: trainWant{"t1": TrainLanded, "t2": TrainGreen}, fx: []EffectKind{EffVerify}},

	// Pause and budgets.
	{name: "second no-verdict on a line: J3", setup: cat(ready("a"), testing_("t1", "a"), CINoVerdict{Train: "t1", Why: "w"}, testing_("t2", "a")), ev: CINoVerdict{Train: "t2", Why: "w"},
		lines: lineWant{"a": LineWaiting}, fx: []EffectKind{EffAskJudge}, check: func(t *testing.T, s State) {
			if !s.Paused {
				t.Errorf("not paused")
			}
		}},
	{name: "pushes keep failing: J3", setup: cat(ready("a"), landing("t1", "a"), PushRefused{Train: "t1", Why: "503"}, PushRefused{Train: "t1", Why: "503"}), ev: PushRefused{Train: "t1", Why: "503"},
		trains: trainWant{"t1": TrainLanding}, fx: []EffectKind{EffAskJudge}},
	{name: "resume", setup: cat(Pause{Reason: "forge is down"}), ev: Resume{}, check: func(t *testing.T, s State) {
		if s.Paused {
			t.Errorf("still paused")
		}
	}},
	{name: "resume adopts a foreign trunk", setup: cat(Pause{Reason: "second writer"}), ev: Resume{Trunk: "x9"}, check: func(t *testing.T, s State) {
		if s.Trunk != "x9" || !s.Landings[0].Foreign {
			t.Errorf("trunk=%s", s.Trunk)
		}
	}},
	{name: "budget expires", setup: cat(ready("a"), testing_("t1", "a"), CINoVerdict{Train: "t1", Why: "w"}), ev: ExpireBudget{Line: "a"}, check: func(t *testing.T, s State) {
		if s.Lines["a"].NoVerdicts != 0 {
			t.Errorf("budget not reset")
		}
	}},
	{name: "a new head resets the budget", setup: cat(ready("a"), testing_("t1", "a"), CINoVerdict{Train: "t1", Why: "w"}), ev: HeadMoved{Line: "a", Change: "a", Head: "a2"}, lines: lineWant{"a": LineWaiting}, check: func(t *testing.T, s State) {
		if s.Lines["a"].NoVerdicts != 0 {
			t.Errorf("budget not reset")
		}
	}},
}

// refusals are the illegal transitions: each is an error, and each leaves the
// state untouched (step checks that).
var refusals = []tcase{
	// A train whose failure waits for the train ahead takes no other report.
	{name: "a pass after a kept failure", setup: cat(ready("a", "b"), testing_("t1", "a"), testing_("t2", "b"), CIFailed{Train: "t2", Blamed: ids("b"), Evidence: "e"}), ev: CIPassed{Train: "t2", Head: "h-t2"}, err: ErrState},
	{name: "a second failure after a kept failure", setup: cat(ready("a", "b"), testing_("t1", "a"), testing_("t2", "b"), CIFailed{Train: "t2", Blamed: ids("b"), Evidence: "e"}), ev: CIFailed{Train: "t2"}, err: ErrState},
	{name: "no verdict after a kept failure", setup: cat(ready("a", "b"), testing_("t1", "a"), testing_("t2", "b"), CIFailed{Train: "t2", Blamed: ids("b"), Evidence: "e"}), ev: CINoVerdict{Train: "t2", Why: "w"}, err: ErrState},
	{name: "a kept blame still needs evidence", setup: cat(ready("a", "b"), testing_("t1", "a"), testing_("t2", "b")), ev: CIFailed{Train: "t2", Blamed: ids("b")}, err: ErrEvidence},

	// I6 at the line check: a failure with no evidence charges nobody.
	{name: "line check fails without evidence", setup: ready("a"), ev: LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0"}, err: ErrEvidence},
	{name: "line check of a spent line", setup: cat(ready("a"), Withdraw{Line: "a"}), ev: LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Pass: true}, err: ErrState},

	// Offers and the gate.
	{name: "offer with no change", ev: Offer{Line: "a"}, err: ErrInput},
	{name: "offer a live line", setup: ready("a"), ev: offer("a"), err: ErrDuplicate},
	{name: "offer a landed line", setup: cat(ready("a"), landing("t1", "a"), PushOK{Train: "t1"}), ev: offer("a"), err: ErrState},
	{name: "offer a change that is in a live line", setup: ready("a"), ev: Offer{Line: "z", Changes: []Change{{ID: "a", Head: "a1"}}}, err: ErrDuplicate},
	{name: "offer on an unknown base", ev: stacked("b", "nope"), err: ErrUnknown},
	{name: "gate an unknown line", ev: GateAccept{Line: "a"}, err: ErrUnknown},
	{name: "gate a line twice", setup: ready("a"), ev: GateAccept{Line: "a"}, err: ErrState},
	{name: "refuse without a reason", setup: cat(offer("a")), ev: GateRefuse{Line: "a"}, err: ErrReason},
	{name: "refuse a waiting line", setup: ready("a"), ev: GateRefuse{Line: "a", Reason: "r"}, err: ErrState},

	// A held line is never spent and never rides.
	{name: "hold without a reason", setup: ready("a"), ev: Hold{Line: "a"}, err: ErrReason},
	{name: "hold before the gate", setup: cat(offer("a")), ev: Hold{Line: "a", Reason: "r"}, err: ErrState},
	{name: "hold twice", setup: cat(ready("a"), Hold{Line: "a", Reason: "r"}), ev: Hold{Line: "a", Reason: "r"}, err: ErrHeld},
	{name: "hold a landing line", setup: cat(ready("a"), landing("t1", "a")), ev: Hold{Line: "a", Reason: "r"}, err: ErrLanding},
	{name: "hold a landed line", setup: cat(ready("a"), landing("t1", "a"), PushOK{Train: "t1"}), ev: Hold{Line: "a", Reason: "r"}, err: ErrState},
	{name: "a held line cannot join", setup: cat(ready("a"), Hold{Line: "a", Reason: "r"}), ev: assemble("t1", "a"), err: ErrHeld},
	{name: "a held line cannot be withdrawn", setup: cat(ready("a"), Hold{Line: "a", Reason: "r"}), ev: Withdraw{Line: "a"}, err: ErrHeld},
	{name: "a held line cannot be parked", setup: cat(ready("a"), Hold{Line: "a", Reason: "r"}), ev: ParkLines{Park: "q", Lines: ids("a"), Cond: "c"}, err: ErrHeld},
	{name: "a held line cannot be refused", setup: cat(ready("a"), Hold{Line: "a", Reason: "r"}), ev: GateRefuse{Line: "a", Reason: "r"}, err: ErrState},
	{name: "release a line that is not held", setup: ready("a"), ev: Release{Line: "a"}, err: ErrState},

	// Parks.
	{name: "park without a restore condition", setup: ready("a"), ev: ParkLines{Park: "p"}, err: ErrReason},
	{name: "park the queue twice", setup: cat(allPark), ev: ParkLines{Park: "p2", Cond: "c"}, err: ErrDuplicate},
	{name: "park a riding line", setup: cat(ready("a"), assemble("t1", "a")), ev: ParkLines{Park: "q", Lines: ids("a"), Cond: "c"}, err: ErrState},
	{name: "restore an unknown park", ev: Restore{Park: "p"}, err: ErrUnknown},
	{name: "a parked line cannot join", setup: cat(ready("a"), allPark), ev: assemble("t1", "a"), err: ErrState},

	// Assembly.
	{name: "assemble nothing", ev: Assemble{Train: "t1"}, err: ErrInput},
	{name: "assemble an offered line", setup: cat(offer("a")), ev: assemble("t1", "a"), err: ErrState},
	{name: "assemble a line without its base", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}), ev: assemble("t1", "b"), err: ErrJoin},
	{name: "assemble a line behind a held base", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}, Hold{Line: "a", Reason: "r"}), ev: assemble("t1", "b"), err: ErrJoin},
	{name: "assemble a stale line", setup: cat(ready("a"), Stale{Line: "a", Reason: "r"}), ev: assemble("t1", "a"), err: ErrJoin},
	{name: "stale with no reason", setup: ready("a"), ev: Stale{Line: "a"}, err: ErrReason},
	{name: "stale while riding", setup: cat(ready("a"), assemble("t1", "a")), ev: Stale{Line: "a", Reason: "r"}, err: ErrState},
	{name: "fresh when not stale", setup: ready("a"), ev: Fresh{Line: "a"}, err: ErrState},
	{name: "an offer that would need itself", setup: cat(ready("a"), stacked("b", "a"), GateAccept{Line: "b"}, Hold{Line: "b", Reason: "r"}, Withdraw{Line: "a"}), ev: stacked("a", "b"), err: ErrInput},
	{name: "a line cannot ride two trains", setup: cat(ready("a"), built("t1", "a")), ev: assemble("t2", "a"), err: ErrState},
	{name: "a train name is used once", setup: cat(ready("a", "b"), built("t1", "a")), ev: assemble("t1", "b"), err: ErrDuplicate},
	{name: "deeper than D", setup: cat(ready("a", "b", "c"), built("t1", "a"), built("t2", "b")), ev: assemble("t3", "c"), err: ErrChain},
	{name: "train 2 before train 1 has a head", setup: cat(ready("a", "b"), assemble("t1", "a")), ev: assemble("t2", "b"), err: ErrChain},
	{name: "train 2 behind a train at J1", setup: cat(ready("a", "b", "c"), testing_("t1", "a", "b"), CIFailed{Train: "t1"}), ev: assemble("t2", "c"), err: ErrChain},
	{name: "assemble while paused", setup: cat(ready("a"), Pause{Reason: "r"}), ev: assemble("t1", "a"), err: ErrPaused},
	{name: "a line that rides alone", setup: cat(Offer{Line: "a", Alone: true, Changes: []Change{{ID: "a", Head: "a1"}}}, GateAccept{Line: "a"}, ready("b")), ev: assemble("t1", "a", "b"), err: ErrJoin},
	{name: "halves of a split ride apart", setup: cat(ready("a", "b"), testing_("t1", "a", "b"), CIFailed{Train: "t1"}, Judge{Train: "t1", Action: JudgeSplit}), ev: assemble("t2", "a", "b"), err: ErrJoin},
	{name: "leave out without a reason", setup: cat(ready("a"), assemble("t1", "a")), ev: LeaveOut{Train: "t1", Line: "a"}, err: ErrReason},
	{name: "leave out after the build", setup: cat(ready("a"), built("t1", "a")), ev: LeaveOut{Train: "t1", Line: "a", Reason: "r"}, err: ErrState},
	{name: "leave out a stranger", setup: cat(ready("a", "b"), assemble("t1", "a")), ev: LeaveOut{Train: "t1", Line: "b", Reason: "r"}, err: ErrUnknown},
	{name: "built on its own base", setup: cat(ready("a"), assemble("t1", "a")), ev: Built{Train: "t1", Head: "trunk0"}, err: ErrHead},
	{name: "built twice", setup: cat(ready("a"), built("t1", "a")), ev: Built{Train: "t1", Head: "h2"}, err: ErrState},

	// I1, I9: a verdict counts only on exactly the built head.
	{name: "opened on another head", setup: cat(ready("a"), built("t1", "a")), ev: Opened{Train: "t1", PR: "9", Head: "other"}, err: ErrHead},
	{name: "pass reported on another head", setup: cat(ready("a"), testing_("t1", "a")), ev: CIPassed{Train: "t1", Head: "other"}, err: ErrHead},
	{name: "pass before the PR opened", setup: cat(ready("a"), built("t1", "a")), ev: CIPassed{Train: "t1", Head: "h-t1"}, err: ErrState},
	{name: "pass for an unknown train", ev: CIPassed{Train: "t9", Head: "h"}, err: ErrUnknown},
	{name: "push with no landing check", setup: cat(ready("a"), green("t1", "a")), ev: PushOK{Train: "t1"}, err: ErrState},
	{name: "landing check on a train still in CI", setup: cat(ready("a"), testing_("t1", "a")), ev: LandingVerified{Train: "t1", Verdict: "h-t1", Trunk: "trunk0"}, err: ErrState},
	{name: "a second pass after green", setup: cat(ready("a"), green("t1", "a")), ev: CIPassed{Train: "t1", Head: "h-t1"}, err: ErrState},
	{name: "a failure after green", setup: cat(ready("a"), green("t1", "a")), ev: CIFailed{Train: "t1"}, err: ErrState},
	{name: "no verdict after green", setup: cat(ready("a"), green("t1", "a")), ev: CINoVerdict{Train: "t1", Why: "w"}, err: ErrState},
	{name: "no verdict without a reason", setup: cat(ready("a"), testing_("t1", "a")), ev: CINoVerdict{Train: "t1"}, err: ErrReason},
	{name: "two reruns", setup: cat(ready("a"), testing_("t1", "a"), CIRerun{Train: "t1"}), ev: CIRerun{Train: "t1"}, err: ErrBudget},

	// I5: trains land in chain order.
	{name: "train 2 cannot land first", setup: cat(ready("a", "b"), testing_("t1", "a"), green("t2", "b")), ev: verifyOf("t2"), err: ErrChain},

	// A cancelled train never lands; an ended train stays ended.
	{name: "landing check on an aborted train", setup: cat(ready("a"), green("t1", "a"), Abort{Train: "t1", Reason: "r"}), ev: LandingVerified{Train: "t1", Verdict: "h-t1", Heads: map[ChangeID]Head{"a": "a1"}, Trunk: "trunk0"}, err: ErrState},
	{name: "push of an aborted train", setup: cat(ready("a"), green("t1", "a"), Abort{Train: "t1", Reason: "r"}), ev: PushOK{Train: "t1"}, err: ErrState},
	{name: "late pass for an aborted train", setup: cat(ready("a"), testing_("t1", "a"), Abort{Train: "t1", Reason: "r"}), ev: CIPassed{Train: "t1", Head: "h-t1"}, err: ErrState},
	{name: "late pass for a void train", setup: cat(ready("a"), testing_("t1", "a"), CINoVerdict{Train: "t1", Why: "w"}), ev: CIPassed{Train: "t1", Head: "h-t1"}, err: ErrState},
	{name: "abort without a reason", setup: cat(ready("a"), testing_("t1", "a")), ev: Abort{Train: "t1"}, err: ErrReason},
	{name: "abort a landing train", setup: cat(ready("a"), landing("t1", "a")), ev: Abort{Train: "t1", Reason: "r"}, err: ErrLanding},
	{name: "abort a landed train", setup: cat(ready("a"), landing("t1", "a"), PushOK{Train: "t1"}), ev: Abort{Train: "t1", Reason: "r"}, err: ErrState},
	{name: "abort a failed train", setup: cat(ready("a"), testing_("t1", "a"), CIFailed{Train: "t1", Blamed: ids("a"), Evidence: "e"}), ev: Abort{Train: "t1", Reason: "r"}, err: ErrState},
	{name: "abort parks with no restore condition", setup: cat(ready("a"), testing_("t1", "a")), ev: Abort{Train: "t1", Reason: "r", Lines: map[LineID]Dispose{"a": {Kind: ParkIt, Park: "q"}}}, err: ErrReason},
	{name: "withdraw a landing line", setup: cat(ready("a"), landing("t1", "a")), ev: Withdraw{Line: "a"}, err: ErrLanding},
	{name: "withdraw a returned line", setup: cat(offer("a"), GateRefuse{Line: "a", Reason: "r"}), ev: Withdraw{Line: "a"}, err: ErrState},

	// I6: blame needs evidence.
	{name: "blame without evidence", setup: cat(ready("a", "b"), testing_("t1", "a", "b")), ev: CIFailed{Train: "t1", Blamed: ids("a")}, err: ErrEvidence},
	{name: "pre-flight blame without evidence", setup: cat(ready("a", "b"), assemble("t1", "a", "b")), ev: PreflightFailed{Train: "t1", Blamed: ids("a")}, err: ErrEvidence},
	{name: "blame a stranger", setup: cat(ready("a", "b"), testing_("t1", "a")), ev: CIFailed{Train: "t1", Blamed: ids("b"), Evidence: "e"}, err: ErrUnknown},
	{name: "judge charges without evidence", setup: cat(ready("a", "b"), testing_("t1", "a", "b"), CIFailed{Train: "t1"}), ev: Judge{Train: "t1", Action: JudgeCharge, Lines: ids("a")}, err: ErrEvidence},
	{name: "judge splits one line", setup: cat(ready("a"), testing_("t1", "a"), CIFailed{Train: "t1"}), ev: Judge{Train: "t1", Action: JudgeSplit}, err: ErrState},
	{name: "judge parks with no restore condition", setup: cat(ready("a"), testing_("t1", "a"), CIFailed{Train: "t1"}), ev: Judge{Train: "t1", Action: JudgePark, Park: "p"}, err: ErrReason},
	{name: "judge reruns twice", setup: cat(ready("a"), testing_("t1", "a"), CIFailed{Train: "t1"}, Judge{Train: "t1", Action: JudgeRerun}, CIFailed{Train: "t1"}), ev: Judge{Train: "t1", Action: JudgeRerun}, err: ErrBudget},
	{name: "judge reruns a pre-flight", setup: cat(ready("a"), assemble("t1", "a"), PreflightFailed{Train: "t1"}), ev: Judge{Train: "t1", Action: JudgeRerun}, err: ErrBudget},
	{name: "judge off the menu", setup: cat(ready("a"), testing_("t1", "a"), CIFailed{Train: "t1"}), ev: Judge{Train: "t1", Action: "land it anyway"}, err: ErrInput},
	{name: "judge a train that is not at J1", setup: cat(ready("a"), testing_("t1", "a")), ev: Judge{Train: "t1", Action: JudgeSplit}, err: ErrState},
	{name: "timeout on a train that is not at J1", setup: cat(ready("a"), testing_("t1", "a")), ev: JudgeTimeout{Train: "t1"}, err: ErrState},

	// The rest.
	{name: "head moved on a spent line", setup: cat(offer("a"), Withdraw{Line: "a"}), ev: HeadMoved{Line: "a", Change: "a", Head: "a2"}, err: ErrState},
	{name: "head moved on a stranger's change", setup: ready("a"), ev: HeadMoved{Line: "a", Change: "zz", Head: "a2"}, err: ErrUnknown},
	{name: "push refused without a reason", setup: cat(ready("a"), landing("t1", "a")), ev: PushRefused{Train: "t1"}, err: ErrReason},
	{name: "push refused for a train not landing", setup: cat(ready("a"), green("t1", "a")), ev: PushRefused{Train: "t1", Why: "w"}, err: ErrState},
	{name: "pause twice", setup: cat(Pause{Reason: "r"}), ev: Pause{Reason: "r"}, err: ErrState},
	{name: "resume when not paused", ev: Resume{}, err: ErrState},
	{name: "adopt a foreign trunk under a live train", setup: cat(ready("a"), testing_("t1", "a"), Pause{Reason: "r"}), ev: Resume{Trunk: "x9"}, err: ErrChain},
}

func runCase(t *testing.T, c tcase, seen map[string]bool) {
	h := newHarness(t)
	h.seen = seen
	h.must(c.setup...)
	fx, err := h.step(c.ev)
	if c.err != nil {
		if !errors.Is(err, c.err) {
			t.Fatalf("got %v, want %v", err, c.err)
		}
		return
	}
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	for id, want := range c.lines {
		if got := h.s.Lines[LineID(id)].State; got != want {
			t.Errorf("line %s is %s, want %s", id, got, want)
		}
	}
	for id, want := range c.trains {
		if got := h.s.Trains[TrainID(id)].State; got != want {
			t.Errorf("train %s is %s, want %s", id, got, want)
		}
	}
	for _, k := range c.fx {
		found := false
		for _, e := range fx {
			found = found || e.Kind == k
		}
		if !found {
			t.Errorf("no %s effect in %v", k, fx)
		}
	}
	if c.check != nil {
		c.check(t, h.s)
	}
}

func TestEveryTransition(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range transitions {
		t.Run(c.name, func(t *testing.T) { runCase(t, c, seen) })
	}
	for _, r := range Table {
		if !seen[key(r.Subject, r.From, r.To, r.Cause)] {
			t.Errorf("no test exercises the row: %s %s to %s (%s)", r.Subject, r.From, r.To, r.Cause)
		}
	}
}

func TestIllegalTransitionsAreErrors(t *testing.T) {
	for _, c := range refusals {
		if c.err == nil {
			t.Fatalf("%s: a refusal case needs its error", c.name)
		}
		t.Run(c.name, func(t *testing.T) { runCase(t, c, map[string]bool{}) })
	}
}

// TestTransitionOffTheTableIsRefused shows I8's last clause: a transition that
// is not a row of the table cannot be written.
func TestTransitionOffTheTableIsRefused(t *testing.T) {
	h := newHarness(t)
	h.must(ready("a")...)
	k := key(sLine, "waiting", "held", CauseHold)
	delete(legal, k)
	defer func() { legal[k] = true }()
	if _, err := h.step(Hold{Line: "a", Reason: "r"}); !errors.Is(err, ErrTable) {
		t.Fatalf("got %v, want %v", err, ErrTable)
	}
}

// TestCheckCatchesBreaches corrupts a good state by hand, one invariant at a
// time, to show that Check and CheckStep would see a breach if Next made one.
func TestCheckCatchesBreaches(t *testing.T) {
	base := func() State {
		h := newHarness(t)
		h.must(cat(ready("a", "b", "c"), landing("t1", "a"), PushOK{Train: "t1"}, green("t2", "b"), Hold{Line: "c", Reason: "r"})...)
		return h.s
	}
	static := []struct {
		inv     string
		corrupt func(*State)
	}{
		{"I1", func(s *State) { s.Trains["t1"].Verdict = "other" }},
		{"I1", func(s *State) { s.Trains["t1"].State = TrainAborted }},
		{"I9", func(s *State) { s.Trains["t2"].State = TrainLanding }},
		{"I2", func(s *State) { s.Trunk = "elsewhere" }},
		{"I5", func(s *State) { s.Trains["t2"].BaseHead = "trunk0" }},
		{"I8", func(s *State) { s.Lines["b"].State = LineWaiting }},
		{"I8", func(s *State) { s.Chain = nil }},
		{"held", func(s *State) { s.Trains["t2"].Lines = append(s.Trains["t2"].Lines, "c") }},
		{"I6", func(s *State) { s.Lines["c"].State, s.Lines["c"].Reason = LineReturned, "" }},
		{"L1", func(s *State) { s.Lines["c"].State = LineParked }},
		{"I3", func(s *State) { s.Lines["b"].Base = "c" }},
	}
	for i, c := range static {
		s := base()
		c.corrupt(&s)
		if !mentions(Check(s), c.inv) {
			t.Errorf("case %d: Check missed a breach of %s: %v", i, c.inv, Check(s))
		}
	}
	steps := []struct {
		inv     string
		corrupt func(*State)
		fx      []Effect
	}{
		{"held", func(s *State) {}, []Effect{{Kind: EffRecord, Line: "c", From: "held", To: "returned", Cause: CauseCharged}}},
		{"I2", func(s *State) { s.Landings = nil }, nil},
		{"I2", func(s *State) { s.Landings[0].Head = "rewritten" }, nil},
		{"I2", func(s *State) { s.Trunk = "elsewhere" }, nil},
		{"held", func(s *State) { s.Lines["c"].State = LineDropped }, nil},
		{"I2", func(s *State) { s.Lines["a"].State = LineWaiting }, nil},
		{"I4", func(s *State) { s.Lines["b"].Changes[0].Head = "b2" }, nil},
		{"ended", func(s *State) { s.Trains["t1"].State = TrainGreen }, nil},
		{"I1", func(s *State) { s.Trains["t2"].Head = "swapped" }, nil},
	}
	for i, c := range steps {
		before, after := base(), base()
		c.corrupt(&after)
		if !mentions(CheckStep(before, after, c.fx), c.inv) {
			t.Errorf("case %d: CheckStep missed a breach of %s", i, c.inv)
		}
	}
}

func mentions(errs []error, inv string) bool {
	for _, e := range errs {
		if strings.HasPrefix(e.Error(), inv+":") {
			return true
		}
	}
	return false
}

// TestRandomWalk drives the machine with events chosen at random, legal or
// not, the judge included. Whatever is chosen, every state reached satisfies
// the invariants. It also checks liveness in the small: when the adversary
// stops and the runner is left to work, every line ends landed, returned or
// dropped.
func TestRandomWalk(t *testing.T) {
	for seed := int64(1); seed <= 60; seed++ {
		r := rand.New(rand.NewSource(seed))
		h := newHarness(t)
		names := []string{"a", "b", "c", "d", "e"}
		trains := 0
		pick := func() LineID { return LineID(names[r.Intn(len(names))]) }
		tid := func() TrainID {
			if trains == 0 || r.Intn(3) == 0 {
				trains++
			}
			return TrainID(fmt.Sprintf("t%d", r.Intn(trains)+1))
		}
		for i := 0; i < 400; i++ {
			var ev Event
			tr := tid()
			head := Head("h-" + string(tr))
			if r.Intn(12) == 0 {
				head = "wrong"
			}
			switch r.Intn(26) {
			case 0, 1:
				id := pick()
				o := Offer{Line: id, Changes: []Change{{ID: ChangeID(id), Head: Head(fmt.Sprintf("%s%d", id, i))}}}
				if r.Intn(4) == 0 {
					o.Base = pick()
				}
				ev = o
			case 2, 3:
				ev = GateAccept{Line: pick()}
			case 4:
				ev = GateRefuse{Line: pick(), Reason: "r"}
			case 5:
				ev = Hold{Line: pick(), Reason: "r"}
			case 6:
				ev = Release{Line: pick()}
			case 7:
				ev = ParkLines{Park: ParkID(fmt.Sprintf("p%d", r.Intn(3))), Cond: "c", Reason: "r"}
			case 8:
				ev = Restore{Park: ParkID(fmt.Sprintf("p%d", r.Intn(3)))}
			case 9:
				ev = Withdraw{Line: pick()}
			case 10, 11, 12:
				w := h.s.Waiting()
				if len(w) == 0 {
					continue
				}
				trains++
				ev = Assemble{Train: TrainID(fmt.Sprintf("t%d", trains)), Lines: w[:1+r.Intn(len(w))]}
			case 13:
				ev = LeaveOut{Train: tr, Line: pick(), Reason: "conflict"}
			case 14:
				ev = Built{Train: tr, Head: head}
			case 15:
				ev = Opened{Train: tr, PR: "n", Head: head}
			case 16, 17:
				ev = CIPassed{Train: tr, Head: head}
			case 18:
				ev = CINoVerdict{Train: tr, Why: "w"}
			case 19:
				f := CIFailed{Train: tr}
				if r.Intn(2) == 0 {
					f.Blamed, f.Evidence = []LineID{pick()}, "e"
				}
				ev = f
			case 20:
				acts := []JudgeAction{JudgeCharge, JudgeSplit, JudgePark, JudgeRerun}
				ev = Judge{Train: tr, Action: acts[r.Intn(4)], Lines: []LineID{pick()}, Evidence: "e", Park: ParkID(fmt.Sprintf("p%d", r.Intn(3))), Cond: "c"}
			case 21:
				ev = HeadMoved{Line: pick(), Change: ChangeID(pick()), Head: Head(fmt.Sprintf("m%d", i))}
			case 22:
				ev = Abort{Train: tr, Reason: "r", Lines: map[LineID]Dispose{pick(): {Kind: DisposeKind(r.Intn(3)), Park: "q", Cond: "c"}}}
			case 23:
				if h.s.Trains[tr] == nil {
					continue
				}
				v := verify(h.s, string(tr)).(LandingVerified)
				if r.Intn(8) == 0 {
					v.Verdict = "other"
				}
				ev = v
			case 24:
				if r.Intn(4) == 0 {
					ev = PushRefused{Train: tr, Permanent: r.Intn(2) == 0, Why: "w"}
				} else {
					ev = PushOK{Train: tr}
				}
			case 25:
				if r.Intn(3) == 0 {
					if r.Intn(2) == 0 {
						ev = Stale{Line: pick(), Reason: "r"}
					} else {
						ev = Fresh{Line: pick()}
					}
				} else if h.s.Paused {
					ev = Resume{}
				} else if r.Intn(4) == 0 {
					ev = Pause{Reason: "r"}
				} else {
					ev = JudgeTimeout{Train: tr, SameTestBefore: r.Intn(2) == 0}
				}
			}
			_, _ = h.step(ev) // a refusal is fine; a broken invariant is not
		}
		settle(t, h, &trains)
		if t.Failed() {
			t.Fatalf("seed %d", seed)
		}
	}
}

// settle is a fair runner and a fair judge with no adversary: holds are
// released, parks restored, the pause lifted, offers gated, and every train
// built, passed and landed. Then nothing may be left in the queue (L1).
func settle(t *testing.T, h *harness, trains *int) {
	t.Helper()
	for round := 0; round < 200; round++ {
		if h.s.Paused {
			h.must(Resume{})
		}
		for id := range h.s.Parks {
			h.must(Restore{Park: id})
		}
		for _, id := range h.s.lineIDs() {
			switch h.s.Lines[id].State {
			case LineHeld:
				h.must(Release{Line: id})
			case LineOffered:
				h.must(GateAccept{Line: id})
			}
		}
		for len(h.s.Chain) > 0 {
			tr := h.s.Trains[h.s.Chain[0]]
			id, head := string(tr.ID), Head("s-"+string(tr.ID))
			switch tr.State {
			case TrainAssembling:
				h.must(Built{Train: tr.ID, Head: head})
			case TrainBuilt:
				h.must(Opened{Train: tr.ID, PR: "n", Head: tr.Head})
			case TrainTesting:
				h.must(CIPassed{Train: tr.ID, Head: tr.Head})
			case TrainJudging:
				h.must(JudgeTimeout{Train: tr.ID})
			case TrainGreen:
				h.must(verify(h.s, id))
			case TrainLanding:
				h.must(PushOK{Train: tr.ID})
			}
		}
		var w []LineID
		for _, id := range h.s.lineIDs() {
			if l := h.s.Lines[id]; !l.State.Spent() && l.Stale != "" {
				h.must(Fresh{Line: id})
			}
		}
		for _, id := range h.s.Waiting() {
			if h.s.NeedMet(h.s.Lines[id], nil) {
				w = append(w, id)
			}
		}
		if len(w) == 0 {
			if len(h.s.Parks) > 0 || len(h.s.inState(LineOffered)) > 0 || len(h.s.Waiting()) > 0 {
				continue
			}
			break
		}
		*trains++
		h.must(Assemble{Train: TrainID(fmt.Sprintf("t%d", *trains)), Lines: w[:1]})
	}
	for id, l := range h.s.Lines {
		if !l.State.Spent() {
			t.Errorf("L1: line %s is still %s (%s)", id, l.State, l.Reason)
		}
	}
}

func TestTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Table {
		k := key(r.Subject, r.From, r.To, r.Cause)
		if seen[k] {
			t.Errorf("duplicate row %s", k)
		}
		seen[k] = true
		if r.Owner == "" || r.Trigger == "" {
			t.Errorf("row %s has no owner or trigger", k)
		}
	}
	if !strings.Contains(Markdown(sLine), "| L1 |") || !strings.Contains(Markdown(sTrain), "| T1 |") {
		t.Errorf("Markdown lost its rows")
	}
}
