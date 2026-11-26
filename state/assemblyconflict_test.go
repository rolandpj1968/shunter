package state

import (
	"errors"
	"testing"
)

// trunkConflict is the builder's leave-out of a line that does not merge
// onto the train's base, which the conflict hook could not resolve.
func trunkConflict(train, line string) Event {
	return LeaveOut{Train: TrainID(train), Line: LineID(line), Reason: "conflict in x.go, not resolved (with the train's base)", Base: true}
}

// Issue 55: a line that conflicts with trunk at assembly, in a way the
// conflict hook can't resolve, goes the way a line check's conflict goes
// (row L40): it is in conflict, joins no train, counts no cycle, and the
// runner is asked to rebase it; the rebase failing, it goes back to its
// owner once, uncharged, keeping its age and its starving (L36). It is never
// left out again at every assembly.
func TestATrunkConflictAtAssemblyGoesBackToItsOwnerOnce(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(offerBy("ann", "a", "a1"), GateAccept{Line: "a"}, ready("b"),
		Starving{Line: "a", Why: "12h"})...)
	seq := s.Lines["a"].Seq
	s = apply(t, s, assemble("t1", "a", "b"))
	next, fx, err := Next(s, trunkConflict("t1", "a"))
	if err != nil {
		t.Fatal(err)
	}
	a := next.Lines["a"]
	switch {
	case a.State != LineWaiting || a.Cause != CauseConflict:
		t.Fatalf("line a is %s (%s), want waiting (%s)", a.State, a.Cause, CauseConflict)
	case a.Conflict == "" || a.ConflictOn != "trunk0":
		t.Fatalf("line a is not in conflict with trunk: %q on %q", a.Conflict, a.ConflictOn)
	case a.Count != 0:
		t.Fatalf("a trunk conflict counted a cycle: %d", a.Count)
	case hasEffect(fx, EffRebase) == nil:
		t.Fatalf("the runner is not asked to rebase it: %v", fx)
	}
	// It joins no train while it is in conflict: no second leave-out.
	next = apply(t, next, Built{Train: "t1", Head: "h-t1"})
	if _, _, err := Next(next, assemble("t2", "a")); !errors.Is(err, ErrJoin) {
		t.Fatalf("a line in conflict with trunk was assembled again: %v", err)
	}
	// The rebase can't resolve it: back to its owner, uncharged, told.
	next, fx, err = Next(next, RebaseFailed{Line: "a", Reason: "a conflict that needs a person: x.go"})
	if err != nil {
		t.Fatal(err)
	}
	a = next.Lines["a"]
	if a.State != LineReturned || a.Cause != CauseConflict || len(a.ChargedAt) != 0 || a.Count != 0 {
		t.Fatalf("line a is %s (%s), charged at %v, count %d", a.State, a.Cause, a.ChargedAt, a.Count)
	}
	if n := hasEffect(fx, EffNotify); n == nil || n.Line != "a" {
		t.Fatalf("the owner is not told: %v", fx)
	}
	// Offered again once it merges, it keeps its age and its starving.
	next = apply(t, next, offerBy("bob", "c", "c1"), GateAccept{Line: "c"}, offerBy("ann", "a", "a2"), GateAccept{Line: "a"})
	a = next.Lines["a"]
	if a.Seq != seq || !a.Starving || a.Count != 0 {
		t.Fatalf("offered again: seq %d (was %d), starving %v, count %d", a.Seq, seq, a.Starving, a.Count)
	}
	order(t, next, "a", "c")
}

// The rebase may resolve it after all: the line is re-keyed at the new
// heads, checked again, and rides.
func TestATrunkConflictAtAssemblyTheRebaseResolvesRides(t *testing.T) {
	s := checkedQueue(t, cat(ready("a"), pass("a", "a1"), assemble("t1", "a"), trunkConflict("t1", "a"),
		HeadMoved{Line: "a", Change: "a", Head: "a2"}, pass("a", "a2"), assemble("t2", "a"))...)
	if l := s.Lines["a"]; l.State != LineRiding || l.Changes[0].Head != "a2" || l.Conflict != "" {
		t.Fatalf("line a is %s at %s, conflict %q", l.State, l.Changes[0].Head, l.Conflict)
	}
}

// A conflict only with another line of the train stays in the bag (L16): the
// line waits at its age, counts the cycle, and may ride in the next train.
func TestAConflictWithAnotherLineAtAssemblyStaysInTheBag(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b"), assemble("t1", "a", "b"))...)
	next, fx, err := Next(s, LeaveOut{Train: "t1", Line: "b", Reason: "conflict in x.go, not resolved (with a line before it in the train)"})
	if err != nil {
		t.Fatal(err)
	}
	b := next.Lines["b"]
	if b.State != LineWaiting || b.Cause != CauseLeftOut || b.Conflict != "" || b.Count != 1 {
		t.Fatalf("line b is %s (%s), conflict %q, count %d", b.State, b.Cause, b.Conflict, b.Count)
	}
	if hasEffect(fx, EffRebase) != nil {
		t.Fatalf("a conflict with another line asked for a rebase: %v", fx)
	}
	apply(t, next, Built{Train: "t1", Head: "h-t1"}, assemble("t2", "b"))
}

// A conflict with the base of a train built on the train ahead may be with
// the lines of that train, not trunk: it stays in the bag, as a conflict
// with another line does.
func TestABaseConflictBehindATrainStaysInTheBag(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b"), built("t1", "a"), assemble("t2", "b"), trunkConflict("t2", "b"))...)
	b := s.Lines["b"]
	if b.State != LineWaiting || b.Cause != CauseLeftOut || b.Conflict != "" || b.Count != 1 {
		t.Fatalf("line b is %s (%s), conflict %q, count %d", b.State, b.Cause, b.Conflict, b.Count)
	}
}
