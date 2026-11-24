package state

import (
	"errors"
	"testing"
)

func cancels(fx []Effect, train TrainID) bool {
	for _, e := range fx {
		if e.Kind == EffCancelRun && e.Train == train {
			return true
		}
	}
	return false
}

// A train is judged at its first decisive failure, and the rest of its run
// is cancelled (docs/queue-as-bag.md §6): when it waits at J1, and when its
// failure is kept behind a train ahead.
func TestADecidedRedTrainsRunIsCancelled(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 3}), cat(ready("a", "b", "c"), testing_("t1", "a", "b"), testing_("t2", "c"))...)
	_, fx, err := Next(s, CIFailed{Train: "t2", Blamed: ids("c"), Evidence: "c's test fails"})
	if err != nil || !cancels(fx, "t2") {
		t.Fatalf("a kept failure's run is not cancelled: %v %v", err, fx)
	}
	_, fx, err = Next(s, CIFailed{Train: "t1", Evidence: "a test fails"})
	if err != nil || !cancels(fx, "t1") {
		t.Fatalf("the run of a train at J1 is not cancelled: %v %v", err, fx)
	}
}

// Nothing is built on a train decided red (§6, the model's NoStackOnRed):
// one whose failure is kept, or one at J1. A rerun at J1 clears it.
func TestNothingIsBuiltOnATrainDecidedRed(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 3}), cat(ready("a", "b", "c", "d"), testing_("t1", "a"), testing_("t2", "b"))...)
	red := apply(t, s, CIFailed{Train: "t2", Blamed: ids("b"), Evidence: "b's test fails"})
	if red.DecidedRed() != "t2" {
		t.Fatalf("decided red: %q", red.DecidedRed())
	}
	if _, _, err := Next(red, assemble("t3", "c")); !errors.Is(err, ErrChain) {
		t.Fatalf("a train built behind a kept failure: %v", err)
	}
	j := apply(t, s, CIFailed{Train: "t1", Evidence: "a test fails"})
	if _, _, err := Next(j, assemble("t3", "c")); !errors.Is(err, ErrChain) {
		t.Fatalf("a train built behind a train at J1: %v", err)
	}
	j = apply(t, j, Judge{Train: "t1", Action: JudgeRerun, Why: "maybe a flake"})
	if j.DecidedRed() != "" {
		t.Fatalf("a rerun at J1 is still decided red: %q", j.DecidedRed())
	}
	apply(t, j, assemble("t3", "c"))
}
