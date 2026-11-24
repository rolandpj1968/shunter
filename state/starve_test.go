package state

import (
	"errors"
	"slices"
	"testing"
)

func counts(t *testing.T, s State, want map[string]int) {
	t.Helper()
	for id, n := range want {
		if got := s.Lines[LineID(id)].Count; got != n {
			t.Fatalf("line %s's count is %d, want %d", id, got, n)
		}
	}
}

// The count rises for each cycle the line did not cause
// (docs/queue-as-bag.md §4): another line's charge ends its train, a train
// ahead does not land, trunk moves against it.
func TestTheCountRisesForCyclesTheLineDidNotCause(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b", "c"), testing_("t1", "a", "b"), testing_("t2", "c"),
		CIFailed{Train: "t1", Blamed: ids("a"), Evidence: "a's test fails"})...)
	counts(t, s, map[string]int{"a": 0, "b": 1, "c": 1})
	s = apply(t, s, LineChecked{Line: "b", Heads: []Head{"b1"}, Trunk: "trunk1", Conflict: "x.go"})
	counts(t, s, map[string]int{"b": 2})
	// The same conflict on the same trunk is not another cycle.
	s = apply(t, s, LineChecked{Line: "b", Heads: []Head{"b1"}, Trunk: "trunk1", Conflict: "x.go"})
	counts(t, s, map[string]int{"b": 2})
}

// Nothing of its own train's doing counts: no verdict and an abort are the
// infrastructure's and the judge's.
func TestTheCountDoesNotRiseForItsOwnTrainsNoVerdictOrAbort(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a"), testing_("t1", "a"), CINoVerdict{Train: "t1", Why: "cancelled"},
		testing_("t2", "a"), Abort{Train: "t2", Reason: "the judge"})...)
	counts(t, s, map[string]int{"a": 0})
}

// A failure of its own halves the count; a new offer keeps it.
func TestAChargeHalvesTheCountAndAnOfferKeepsIt(t *testing.T) {
	s := New("trunk0", Config{Depth: 2})
	s = apply(t, s, offerBy("ann", "a", "a1"), GateAccept{Line: "a"})
	for range 5 {
		s = apply(t, s, LineChecked{Line: "a", Heads: []Head{s.Lines["a"].Changes[0].Head}, Trunk: Head("trunk" + string(rune('0'+s.Lines["a"].Count))), Conflict: "x.go"},
			HeadMoved{Line: "a", Change: "a", Head: Head("a" + string(rune('2'+s.Lines["a"].Count)))})
	}
	counts(t, s, map[string]int{"a": 5})
	s = apply(t, s, testing_("t1", "a")...)
	s = apply(t, s, CIFailed{Train: "t1", Blamed: ids("a"), Evidence: "a's test fails"})
	counts(t, s, map[string]int{"a": 2})
	s = apply(t, s, offerBy("ann", "a", "a9"))
	counts(t, s, map[string]int{"a": 2})
}

// A starving line rides alone and first (§4): it sorts after a manual
// priority and before everything else; no other train is assembled while
// it is forced, and its own only with the chain empty.
func TestAStarvingLineRidesAloneAndFirst(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b", "c"), stacked("d", "c"), GateAccept{Line: "d"}, testing_("t1", "a"))...)
	if _, _, err := Next(s, Starving{Line: "d"}); !errors.Is(err, ErrReason) {
		t.Fatalf("starving without a reason: %v", err)
	}
	s = apply(t, s, Starving{Line: "d", Why: "3 cycles"})
	if f := s.Forced(); f != "d" {
		t.Fatalf("forced %q", f)
	}
	order(t, s, "c", "d", "b")
	s = apply(t, s, prioritise("b"))
	order(t, s, "b", "c", "d")
	for _, lines := range [][]string{{"c", "d"}, {"b", "c"}} {
		if _, _, err := Next(s, assemble("t2", lines...)); !errors.Is(err, ErrJoin) {
			t.Fatalf("%v assembled behind a train while d is forced: %v", lines, err)
		}
	}
	// A prioritised line goes before it.
	apply(t, s, assemble("t2", "b"))
	s = apply(t, s, green("t1", "a")[3:]...)
	s = apply(t, s, verify(s, "t1"), PushOK{Train: "t1"})
	if _, _, err := Next(s, assemble("t2", "d", "c", "b")); !errors.Is(err, ErrJoin) {
		t.Fatalf("a line rode beside the forced one: %v", err)
	}
	s = apply(t, s, assemble("t2", "d", "c"))
	if got := s.Trains["t2"].Lines; !slices.Equal(got, ids("c", "d")) {
		t.Fatalf("the forced train is %v", got)
	}
	if s.Forced() != "" || s.Lines["d"].Starving != true {
		t.Fatalf("still forced: %q", s.Forced())
	}
}

// A line the runner can't rebase is not forced: it needs a person, so it
// goes back to its owner, and the queue moves meanwhile. Offered again, it
// is forced again.
func TestAStuckLineIsNotForced(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b"),
		LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Conflict: "x.go"},
		Starving{Line: "a", Why: "12h"}, RebaseFailed{Line: "a", Reason: "a real conflict"})...)
	if f := s.Forced(); f != "" {
		t.Fatalf("a stuck line is forced: %q", f)
	}
	if s.Lines["a"].Count != 1 || !s.Lines["a"].Starving {
		t.Fatalf("a stuck line lost its count or its starving: %+v", s.Lines["a"])
	}
	apply(t, s, assemble("t1", "b"))
	s = apply(t, s, Offer{Line: "a", Changes: []Change{{ID: "a", Head: "a2"}}}, GateAccept{Line: "a"})
	if f := s.Forced(); f != "a" {
		t.Fatalf("offered again, forced %q", f)
	}
}
