package state

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func hasEffect(fx []Effect, k EffectKind) *Effect {
	i := slices.IndexFunc(fx, func(e Effect) bool { return e.Kind == k })
	if i < 0 {
		return nil
	}
	return &fx[i]
}

// A train is visible from assembly (§5.4): its draft pull request is
// recorded while its cheap tier runs, and promotion (Opened) makes it the
// train's pull request in CI.
func TestADraftIsRecordedAtAssemblyAndPromoted(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a"), assemble("t1", "a"))...)
	s = apply(t, s, DraftOpened{Train: "t1", PR: "7"})
	if tr := s.Trains["t1"]; tr.PR != "7" || !tr.Draft || tr.State != TrainAssembling {
		t.Fatalf("train t1: %+v", tr)
	}
	// The same draft again is no change; another is refused.
	apply(t, s, DraftOpened{Train: "t1", PR: "7"})
	if _, _, err := Next(s, DraftOpened{Train: "t1", PR: "8"}); !errors.Is(err, ErrState) {
		t.Fatalf("a second draft: %v", err)
	}
	s = apply(t, s, Built{Train: "t1", Head: "h-t1"}, Opened{Train: "t1", PR: "7", Head: "h-t1"})
	if tr := s.Trains["t1"]; tr.Draft || tr.State != TrainTesting {
		t.Fatalf("after promotion, train t1: %+v", tr)
	}
	if _, _, err := Next(s, DraftOpened{Train: "t1", PR: "7"}); !errors.Is(err, ErrState) {
		t.Fatalf("a draft for a train past assembly: %v", err)
	}
}

// A failed pre-flight comments its evidence on the draft and closes it.
func TestAFailedPreflightClosesItsDraft(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a"), assemble("t1", "a"), DraftOpened{Train: "t1", PR: "7"})...)
	_, fx, err := Next(s, PreflightFailed{Train: "t1", Blamed: ids("a"), Evidence: "step vet: x.go:3: unused"})
	if err != nil {
		t.Fatal(err)
	}
	if c := hasEffect(fx, EffClosePR); c == nil || !strings.Contains(c.Reason, "x.go:3: unused") {
		t.Fatalf("the draft is not closed with the evidence: %+v", fx)
	}
	// With no draft, a pre-flight failure has no pull request to close.
	s = apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a"), assemble("t1", "a"))...)
	if _, fx, _ := Next(s, PreflightFailed{Train: "t1", Blamed: ids("a"), Evidence: "e"}); hasEffect(fx, EffClosePR) != nil {
		t.Fatalf("closed a pull request never opened: %+v", fx)
	}
}
