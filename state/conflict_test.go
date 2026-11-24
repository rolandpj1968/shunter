package state

import (
	"errors"
	"testing"
)

// A conflict with trunk that the runner's rebase can't resolve needs a
// person, and forcing the line alone can't make it merge: the line goes back
// to its owner, uncharged, and when it is offered again, once it merges, it
// keeps its first offer's age, its count and its starving (owner's ruling of
// 24 Nov, docs/queue-as-bag.md §4).
func TestATrunkConflictTheRebaseCannotResolveGoesBackKeepingItsAge(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(offerBy("ann", "a", "a1"), GateAccept{Line: "a"}, ready("b"),
		LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk1", Conflict: "x.go"},
		Starving{Line: "a", Why: "12h"})...)
	seq := s.Lines["a"].Seq
	next, fx, err := Next(s, RebaseFailed{Line: "a", Reason: "both sides changed x.go"})
	if err != nil {
		t.Fatal(err)
	}
	a := next.Lines["a"]
	if a.State != LineReturned || a.Cause != CauseConflict || len(a.ChargedAt) != 0 {
		t.Fatalf("line a is %s (%s), charged at %v", a.State, a.Cause, a.ChargedAt)
	}
	if len(fx) != 2 || fx[1].Kind != EffNotify || fx[1].Line != "a" {
		t.Fatalf("the owner is not told: %v", fx)
	}
	if next.Forced() != "" {
		t.Fatalf("a returned line is forced: %q", next.Forced())
	}
	// A line offered later stays younger than a, which comes back.
	next = apply(t, next, offerBy("bob", "c", "c1"), GateAccept{Line: "c"})
	next = apply(t, next, offerBy("ann", "a", "a2"), GateAccept{Line: "a"})
	a = next.Lines["a"]
	if a.Seq != seq || a.Count != 1 || !a.Starving {
		t.Fatalf("offered again: seq %d (was %d), count %d, starving %v", a.Seq, seq, a.Count, a.Starving)
	}
	order(t, next, "a", "b", "c")
	if next.Forced() != "a" {
		t.Fatalf("offered again, starving, and not forced: %q", next.Forced())
	}
}

// A line left out at assembly because it didn't fit with the lines taken (a
// conflict with another line of the train) stays in the bag: it waits, at
// its age, and counts the cycle; at three it is starving and rides alone and
// first (owner's ruling of 24 Nov, docs/queue-as-bag.md §4).
func TestALineInConflictWithAnotherLineStaysInTheBag(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), ready("a", "b", "c")...)
	seq := s.Lines["c"].Seq
	for i, train := range []string{"t1", "t2", "t3"} {
		s = apply(t, s, assemble(train, "a", "c"), LeaveOut{Train: TrainID(train), Line: "c", Reason: "conflicts with a in x.go"},
			Abort{Train: TrainID(train), Reason: "the next cycle"})
		c := s.Lines["c"]
		if c.State != LineWaiting || c.Seq != seq || c.Count != i+1 {
			t.Fatalf("after %s: line c is %s (%s), seq %d, count %d", train, c.State, c.Cause, c.Seq, c.Count)
		}
	}
	s = apply(t, s, Starving{Line: "c", Why: "3 cycles"})
	if _, _, err := Next(s, assemble("t4", "a", "c")); !errors.Is(err, ErrJoin) {
		t.Fatalf("a starving line rode beside another: %v", err)
	}
	apply(t, s, assemble("t4", "c"))
}

// A line that needs a line left out leaves with it (I3), and waits too.
func TestALineWhoseBaseIsLeftOutWaitsWithIt(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b"), stacked("d", "b"), GateAccept{Line: "d"},
		assemble("t1", "a", "b", "d"), LeaveOut{Train: "t1", Line: "b", Reason: "conflicts with a"})...)
	if got := s.Trains["t1"].Lines; len(got) != 1 || got[0] != "a" {
		t.Fatalf("train t1 holds %v", got)
	}
	for _, id := range []LineID{"b", "d"} {
		if l := s.Lines[id]; l.State != LineWaiting {
			t.Fatalf("line %s is %s (%s)", id, l.State, l.Cause)
		}
	}
}
