package state

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"
)

// run applies events to a queue that requires line checks, failing on any
// refusal or broken invariant.
func checkedQueue(t *testing.T, evs ...Event) State {
	t.Helper()
	s := New("trunk0", Config{Depth: 2, RequireCheck: true})
	for _, ev := range evs {
		next, _, err := Next(s, ev)
		if err != nil {
			t.Fatalf("%s refused: %v", Name(ev), err)
		}
		if errs := Check(next); len(errs) > 0 {
			t.Fatalf("after %s: %v", Name(ev), errs)
		}
		s = next
	}
	return s
}

func pass(id string, head Head) Event {
	return LineChecked{Line: LineID(id), Heads: []Head{head}, Trunk: "trunk0", Pass: true}
}

// I10: no line is assembled without a current green line check.
func TestNoLineIsAssembledWithoutACurrentGreenCheck(t *testing.T) {
	s := checkedQueue(t, cat(ready("a", "b"))...)
	if _, _, err := Next(s, assemble("t1", "a")); !errors.Is(err, ErrJoin) {
		t.Fatalf("an unchecked line was assembled: %v", err)
	}
	s = checkedQueue(t, cat(ready("a", "b"), pass("a", "a1"))...)
	if _, _, err := Next(s, assemble("t1", "a", "b")); !errors.Is(err, ErrJoin) {
		t.Fatalf("a train took an unchecked line beside a checked one: %v", err)
	}
	if _, _, err := Next(s, assemble("t1", "a")); err != nil {
		t.Fatalf("a checked line was refused: %v", err)
	}
}

// A verdict belongs to the heads it was taken on: a push ends it, and a
// verdict on other heads than the line has now changes nothing.
func TestAPushEndsALineCheck(t *testing.T) {
	s := checkedQueue(t, cat(ready("a"), pass("a", "a1"), HeadMoved{Line: "a", Change: "a", Head: "a2"})...)
	if s.Lines["a"].CheckCurrent() {
		t.Fatal("the check outlived a push")
	}
	if _, _, err := Next(s, assemble("t1", "a")); !errors.Is(err, ErrJoin) {
		t.Fatalf("assembled on an old check: %v", err)
	}
	// the old head's verdict, arriving late, says nothing
	s = checkedQueue(t, cat(ready("a"), HeadMoved{Line: "a", Change: "a", Head: "a2"}, pass("a", "a1"))...)
	if s.Lines["a"].CheckCurrent() {
		t.Fatal("a verdict on an old head was kept")
	}
	// nor does a late failure on the old head charge the line
	s = checkedQueue(t, cat(ready("a"), HeadMoved{Line: "a", Change: "a", Head: "a2"},
		LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Evidence: "e"})...)
	if s.Lines["a"].State != LineWaiting {
		t.Fatalf("a failure on an old head moved the line to %s", s.Lines["a"].State)
	}
}

// A push is not a fix: a line charged by the line check and offered again
// rides only once the check has passed on its new head.
func TestAChargedLineRidesOnlyOnceItsNewHeadPasses(t *testing.T) {
	again := Offer{Line: "a", Changes: []Change{{ID: "a", Head: "a2"}}}
	s := checkedQueue(t, cat(ready("a"), LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Evidence: "e"},
		again, GateAccept{Line: "a"})...)
	if _, _, err := Next(s, assemble("t1", "a")); !errors.Is(err, ErrJoin) {
		t.Fatalf("a re-offered line rode unchecked: %v", err)
	}
	s = checkedQueue(t, cat(ready("a"), LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Evidence: "e"},
		again, GateAccept{Line: "a"}, pass("a", "a2"), assemble("t1", "a"))...)
	if s.Lines["a"].State != LineRiding {
		t.Fatalf("line a is %s", s.Lines["a"].State)
	}
}

// A failing check of a line that is held charges nobody: only its green is
// lost. One that rides keeps its green: its train judges it now.
func TestALineCheckChargesOnlyAWaitingLine(t *testing.T) {
	s := checkedQueue(t, cat(ready("a"), pass("a", "a1"), Hold{Line: "a", Reason: "r"},
		LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk1", Evidence: "e"})...)
	if l := s.Lines["a"]; l.State != LineHeld || l.CheckCurrent() {
		t.Fatalf("held line a is %s, checked %v", l.State, l.CheckCurrent())
	}
	s = checkedQueue(t, cat(ready("a"), pass("a", "a1"), assemble("t1", "a"),
		LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk1", Evidence: "e"})...)
	if l := s.Lines["a"]; l.State != LineRiding || !l.CheckCurrent() {
		t.Fatalf("riding line a is %s, checked %v", l.State, l.CheckCurrent())
	}
}

// A random walk over a queue that requires line checks: whatever the order of
// offers, pushes, checks, holds, assemblies and train endings, Check finds no
// breach, I10 among them, and no refusal is anything but a stated error.
func TestRandomWalkWithLineChecks(t *testing.T) {
	for seed := int64(1); seed <= 80; seed++ {
		r := rand.New(rand.NewSource(seed))
		s := New("trunk0", Config{Depth: 2, RequireCheck: true})
		names := []string{"a", "b", "c", "d"}
		pick := func() LineID { return LineID(names[r.Intn(len(names))]) }
		trains, rode := 0, 0
		for i := 0; i < 500; i++ {
			var ev Event
			tr := TrainID(fmt.Sprintf("t%d", max(1, trains-r.Intn(2))))
			switch r.Intn(16) {
			case 0, 1:
				id := pick()
				o := Offer{Line: id, Changes: []Change{{ID: ChangeID(id), Head: Head(fmt.Sprintf("%s%d", id, i))}}}
				if r.Intn(4) == 0 {
					o.Base = pick()
				}
				ev = o
			case 2, 3:
				ev = GateAccept{Line: pick()}
			case 4, 5, 6:
				// a verdict on the present head, or on a wrong one
				id := pick()
				l := s.Lines[id]
				if l == nil {
					continue
				}
				e := LineChecked{Line: id, Heads: []Head{l.Changes[0].Head}, Trunk: s.Trunk, Pass: r.Intn(5) != 0, Evidence: "e"}
				if r.Intn(6) == 0 {
					e.Heads = []Head{"stale"}
				}
				ev = e
			case 7:
				ev = HeadMoved{Line: pick(), Change: ChangeID(pick()), Head: Head(fmt.Sprintf("m%d", i))}
			case 8:
				ev = Hold{Line: pick(), Reason: "r"}
			case 9:
				ev = Release{Line: pick()}
			case 10, 11:
				w := s.Waiting()
				if len(w) == 0 {
					continue
				}
				trains++
				ev = Assemble{Train: TrainID(fmt.Sprintf("t%d", trains)), Lines: w[:1+r.Intn(len(w))]}
			case 12:
				ev = Built{Train: tr, Head: Head("h-" + string(tr))}
			case 13:
				ev = Opened{Train: tr, PR: "n", Head: Head("h-" + string(tr))}
			case 14:
				ev = CINoVerdict{Train: tr, Why: "w"}
			case 15:
				ev = CIFailed{Train: tr, Blamed: []LineID{pick()}, Evidence: "e"}
			}
			next, _, err := Next(s, ev)
			if err != nil {
				continue
			}
			if errs := Check(next); len(errs) > 0 {
				t.Fatalf("seed %d, step %d, %s %+v: %v", seed, i, Name(ev), ev, errs)
			}
			if _, ok := ev.(Assemble); ok {
				rode++
			}
			s = next
		}
		if seed == 1 && rode == 0 {
			t.Fatal("the walk never assembled a train: it proves nothing")
		}
	}
}
