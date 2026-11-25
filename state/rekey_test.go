package state

import (
	"errors"
	"testing"
)

// moved is the builder's leave-out of a line whose change it found at a new
// head (row L39).
func moved(train, line string, head Head) Event {
	return LeaveOut{Train: TrainID(train), Line: LineID(line), Reason: "change " + line + ": its head moved", Moved: []Change{{ID: ChangeID(line), Head: head}}}
}

// A waiting line pushed to is re-keyed once at assembly, at its new head:
// it keeps its place, its old green no longer counts, it rides only after a
// line check at the new head, and it is not left out again.
func TestAMovedHeadReKeysTheLineOnce(t *testing.T) {
	setup := cat(ready("a", "b"), pass("a", "a1"), pass("b", "b1"), assemble("t1", "a", "b"))
	seq := checkedQueue(t, setup...).Lines["a"].Seq
	s := checkedQueue(t, append(setup, moved("t1", "a", "a2"))...)
	l := s.Lines["a"]
	switch {
	case l.State != LineWaiting || l.Cause != CauseMoved:
		t.Fatalf("line a is %s (%s), want waiting (%s)", l.State, l.Cause, CauseMoved)
	case l.Changes[0].Head != "a2":
		t.Fatalf("line a is at %s, want a2", l.Changes[0].Head)
	case l.Seq != seq:
		t.Fatalf("line a lost its place: seq %d, was %d", l.Seq, seq)
	case l.CheckCurrent():
		t.Fatal("the green at the old head outlived the re-key")
	case l.Count != 0:
		t.Fatalf("a self-caused leave-out counted a cycle: %d", l.Count)
	}
	// Not assembled again until checked at the new head; a late green at the
	// old head says nothing.
	s = apply(t, s, pass("a", "a1"), Built{Train: "t1", Head: "h-t1"})
	if _, _, err := Next(s, assemble("t2", "a")); !errors.Is(err, ErrJoin) {
		t.Fatalf("assembled with no check at the new head: %v", err)
	}
	s = apply(t, s, pass("a", "a2"), assemble("t2", "a"))
	if l := s.Lines["a"]; l.State != LineRiding || l.Changes[0].Head != "a2" {
		t.Fatalf("line a is %s at %s, want riding at a2", l.State, l.Changes[0].Head)
	}
	if s.Lines["b"].Count != 0 {
		t.Fatalf("line b counted a's push: %d", s.Lines["b"].Count)
	}
}

// Every moved change of a line is re-keyed in the one leave-out.
func TestAMovedHeadReKeysEveryMovedChange(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}),
		Offer{Line: "a", Changes: []Change{{ID: "x", Head: "x1"}, {ID: "y", Head: "y1"}}}, GateAccept{Line: "a"}, assemble("t1", "a"),
		LeaveOut{Train: "t1", Line: "a", Reason: "moved", Moved: []Change{{ID: "x", Head: "x2"}, {ID: "y", Head: "y2"}}})
	if c := s.Lines["a"].Changes; c[0].Head != "x2" || c[1].Head != "y2" {
		t.Fatalf("heads %v", c)
	}
}

// A leave-out for the line's own moved head is never a cycle it did not
// cause: no starving credit, however often it happens; any other leave-out
// still counts.
func TestASelfCausedLeaveOutCountsNoCycle(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), ready("a", "b")...)
	for i, h := range []Head{"a2", "a3", "a4", "a5"} {
		train := string(rune('p' + i))
		s = apply(t, s, assemble(train, "a", "b"), moved(train, "a", h), LeaveOut{Train: TrainID(train), Line: "b", Reason: "conflict in x"})
	}
	counts(t, s, map[string]int{"a": 0, "b": 4})
	if s.Lines["a"].Changes[0].Head != "a5" {
		t.Fatalf("line a at %s", s.Lines["a"].Changes[0].Head)
	}
}

// A held line pushed to is re-keyed while held and stays held; released, it
// rides at the new head once checked there.
func TestAHeldLineIsReKeyedAndStaysHeld(t *testing.T) {
	s := checkedQueue(t, cat(ready("a"), pass("a", "a1"), Hold{Line: "a", Reason: "review"}, HeadMoved{Line: "a", Change: "a", Head: "a2"})...)
	if l := s.Lines["a"]; l.State != LineHeld || l.Changes[0].Head != "a2" || l.CheckCurrent() {
		t.Fatalf("line a is %s at %s (checked %v), want held at a2 unchecked", l.State, l.Changes[0].Head, l.CheckCurrent())
	}
	s = apply(t, s, Release{Line: "a"})
	if _, _, err := Next(s, assemble("t1", "a")); !errors.Is(err, ErrJoin) {
		t.Fatalf("released and assembled on the old green: %v", err)
	}
	s = apply(t, s, pass("a", "a2"), assemble("t1", "a"))
	if l := s.Lines["a"]; l.State != LineRiding || l.Changes[0].Head != "a2" {
		t.Fatalf("line a is %s at %s, want riding at a2", l.State, l.Changes[0].Head)
	}
}

// A held line whose push nobody read while it was held is re-keyed by its
// first assembly after release, once, and then rides.
func TestAReleasedLineAtAnOldHeadIsReKeyedOnce(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a"), Hold{Line: "a", Reason: "review"}, Release{Line: "a"}, assemble("t1", "a"), moved("t1", "a", "a2"))...)
	if s.Trains["t1"].State != TrainEmpty {
		t.Fatalf("train t1 is %s", s.Trains["t1"].State)
	}
	s = apply(t, s, assemble("t2", "a"), Built{Train: "t2", Head: "h-t2"})
	if l := s.Lines["a"]; l.State != LineRiding || l.Changes[0].Head != "a2" || l.Count != 0 {
		t.Fatalf("line a is %s at %s, count %d", l.State, l.Changes[0].Head, l.Count)
	}
}
