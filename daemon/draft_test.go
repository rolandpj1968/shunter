package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/tier"
)

func withDrafts(r *rig) { r.drafts = true }

// waitFor polls the daemon's state until ok holds, or fails the test.
func (r *rig) waitFor(what string, ok func(state.State) bool) state.State {
	r.t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if s := r.state(); ok(s) {
			return s
		}
	}
	r.t.Fatalf("timed out waiting for %s", what)
	return state.State{}
}

// A train is visible from assembly (§5.4): its draft opens once its lines
// are merged, while pre-flight runs, and starts no CI. When pre-flight
// passes the draft is promoted, which starts CI; no second pull request is
// opened.
func TestATrainIsADraftFromAssemblyAndPromotedAfterPreflight(t *testing.T) {
	r := newRigWith(t, 0, tier.Rules{}, withDrafts)
	release := r.w.Block("preflight")
	r.open("a")
	r.queue("a")
	s := r.waitFor("the draft", func(s state.State) bool {
		if len(s.Chain) == 0 {
			return false
		}
		tr := s.Trains[s.Chain[0]]
		return tr != nil && tr.PR != ""
	})
	tr := s.Trains[s.Chain[0]]
	if tr.State != state.TrainAssembling || !tr.Draft {
		t.Fatalf("while pre-flight runs, the train is %s, draft %v", tr.State, tr.Draft)
	}
	if pr, draft := r.w.Draft(tr.ID); pr != tr.PR || !draft {
		t.Fatalf("the forge shows %q, draft %v", pr, draft)
	}
	release()
	r.settle()
	r.wantTrain(tr.ID, state.TrainTesting)
	if pr, draft := r.w.Draft(tr.ID); pr != tr.PR || draft {
		t.Fatalf("after pre-flight, the forge shows %q, draft %v", pr, draft)
	}
	if r.w.Acts["open-pr"] != 0 || r.w.Acts["promote"] != 1 {
		t.Fatalf("acts %v", r.w.Acts)
	}
	r.w.Pass(tr.ID)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
}

// A failed pre-flight closes the draft, with its evidence.
func TestAFailedPreflightClosesTheDraft(t *testing.T) {
	r := newRigWith(t, 0, tier.Rules{}, withDrafts)
	r.w.BuildHook = func(ctx context.Context, req adapter.BuildRequest) (adapter.BuildResult, error) {
		return adapter.BuildResult{PreflightFailed: true, Blamed: []state.LineID{"a"}, Evidence: "step vet: a.go:1: unused"}, nil
	}
	r.open("a")
	r.queue("a")
	r.settle()
	r.wantLine("a", state.LineReturned)
	if r.w.Acts["open-draft"] != 1 || r.w.Acts["close-pr"] != 1 || r.w.Acts["promote"] != 0 {
		t.Fatalf("acts %v", r.w.Acts)
	}
}
