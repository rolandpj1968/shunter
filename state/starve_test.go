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

// A starving line rides first (§4, owner's ruling of 26 Nov): it sorts
// after a manual priority and before everything else; no train is
// assembled without it while it is forced, but one of prioritised lines;
// and it leads a train that other lines join, with the chain not empty.
func TestAStarvingLineLeadsAFullTrain(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b", "c"), stacked("d", "c"), GateAccept{Line: "d"}, testing_("t1", "a"))...)
	if _, _, err := Next(s, Starving{Line: "d"}); !errors.Is(err, ErrReason) {
		t.Fatalf("starving without a reason: %v", err)
	}
	s = apply(t, s, Starving{Line: "d", Why: "3 cycles"})
	if f := s.Forced(); f != "d" {
		t.Fatalf("forced %q", f)
	}
	order(t, s, "c", "d", "b")
	for _, lines := range [][]string{{"b"}, {"c"}, {"b", "c"}} {
		if _, _, err := Next(s, assemble("t2", lines...)); !errors.Is(err, ErrJoin) {
			t.Fatalf("%v passed the forced line: %v", lines, err)
		}
	}
	// It leads a full train, behind a train that rides.
	full := apply(t, s, assemble("t2", "b", "d", "c"))
	if got := full.Trains["t2"].Lines; !slices.Equal(got, ids("c", "d", "b")) {
		t.Fatalf("the train is %v", got)
	}
	if full.Forced() != "" || !full.Lines["d"].Starving {
		t.Fatalf("still forced: %q", full.Forced())
	}
	// A prioritised line goes before it.
	s = apply(t, s, prioritise("b"))
	order(t, s, "b", "c", "d")
	apply(t, s, assemble("t2", "b"))
}

// Several starving lines lead the same train, oldest first.
func TestStarvingLinesLeadTheSameTrainOldestFirst(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b", "c", "e"),
		Starving{Line: "e", Why: "3 cycles"}, Starving{Line: "c", Why: "12h"})...)
	if f := s.Forced(); f != "c" {
		t.Fatalf("forced %q, want the oldest starving line", f)
	}
	if got := s.Starved(); !slices.Equal(got, ids("c", "e")) {
		t.Fatalf("starved %v", got)
	}
	order(t, s, "c", "e", "a", "b")
	s = apply(t, s, assemble("t1", "a", "b", "e", "c"))
	if got := s.Trains["t1"].Lines; !slices.Equal(got, ids("c", "e", "a", "b")) {
		t.Fatalf("the train is %v", got)
	}
}

// A line marked alone rides alone, starving or not.
func TestAStarvingLineMarkedAloneStillRidesAlone(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a"), Offer{Line: "b", Changes: []Change{{ID: "b", Head: "b1"}}, Alone: true},
		GateAccept{Line: "b"}, Starving{Line: "b", Why: "3 cycles"})...)
	if _, _, err := Next(s, assemble("t1", "a", "b")); !errors.Is(err, ErrJoin) {
		t.Fatalf("a line marked alone rode beside another: %v", err)
	}
	apply(t, s, assemble("t1", "b"))
}

// A train a starving line leads is judged by the existing rules: blame that
// names another line charges it, and the starving line counts the cycle,
// stays starving and leads again; blame that names nobody goes to J1, and
// counts a failure blame could not clear it from (Line.Unclear).
func TestATrainLedByAStarvingLineIsJudgedAsAnyOther(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b"), Starving{Line: "a", Why: "3 cycles"}, testing_("t1", "a", "b"))...)
	if got := s.Trains["t1"].Lines; !slices.Equal(got, ids("a", "b")) {
		t.Fatalf("the train is %v", got)
	}
	charged := apply(t, s, CIFailed{Train: "t1", Blamed: ids("b"), Evidence: "b's test fails"})
	if b := charged.Lines["b"]; b.State != LineReturned || b.Cause != CauseCharged {
		t.Fatalf("line b is %s (%s)", b.State, b.Cause)
	}
	if a := charged.Lines["a"]; a.State != LineWaiting || !a.Starving || a.Count != 1 || a.Unclear != 0 || charged.Forced() != "a" {
		t.Fatalf("line a: %s, starving %v, count %d, unclear %d, forced %q", a.State, a.Starving, a.Count, a.Unclear, charged.Forced())
	}
	own := apply(t, s, CIFailed{Train: "t1", Blamed: ids("a"), Evidence: "a's test fails"})
	if a := own.Lines["a"]; a.State != LineReturned || a.Starving {
		t.Fatalf("charged, line a is %s, starving %v", a.State, a.Starving)
	}
	unclear := apply(t, s, CIFailed{Train: "t1", Evidence: "something fails"})
	if unclear.Trains["t1"].State != TrainJudging {
		t.Fatalf("train t1 is %s", unclear.Trains["t1"].State)
	}
	for _, id := range ids("a", "b") {
		if n := unclear.Lines[id].Unclear; n != 1 {
			t.Fatalf("line %s: unclear %d", id, n)
		}
	}
	// A new offer starts the count again.
	re := apply(t, unclear, Abort{Train: "t1", Reason: "the judge"}, Withdraw{Line: "a"}, offerBy("", "a", "a2"), GateAccept{Line: "a"})
	if n := re.Lines["a"].Unclear; n != 0 {
		t.Fatalf("offered again, unclear %d", n)
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
