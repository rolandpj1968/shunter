package daemon_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/tier"
)

// leavesOut is a builder that leaves out the line named, once, after the
// draft is open: the draft names it, and the built head does not carry it.
func leavesOut(id state.LineID) func(context.Context, adapter.BuildRequest) (adapter.BuildResult, error) {
	var once sync.Once
	return func(ctx context.Context, req adapter.BuildRequest) (adapter.BuildResult, error) {
		res := adapter.BuildResult{LeftOut: map[state.LineID]string{}}
		for i, l := range req.Lines {
			if l.ID == id && i > 0 {
				once.Do(func() {
					res.LeftOut[l.ID] = "conflict in x.go, not resolved (with a line before it in the train)"
				})
			}
		}
		return res, nil
	}
}

// draftedTrain gathers lines a and b into one drafted train whose build
// leaves b out, and returns it once it is testing.
func draftedTrain(r *rig) state.TrainID {
	r.t.Helper()
	r.w.BuildHook = leavesOut("b")
	r.open("a", "b")
	r.together("a", "b")
	r.settle()
	s := r.waitFor("the train testing", func(s state.State) bool {
		return s.Lines["a"].State == state.LineRiding && s.Trains[s.Lines["a"].Train].State == state.TrainTesting
	})
	return s.Lines["a"].Train
}

// waitCount polls the fake world until the act has happened n times.
func (r *rig) waitCount(kind string, n int) {
	r.t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if r.w.Count(kind, true) >= n {
			return
		}
	}
	r.t.Fatalf("timed out waiting for %d %s (acts: %d)", n, kind, r.w.Count(kind, true))
}

// A draft opened naming lines a and b, whose build left b out, is
// retitled to name a alone when it is promoted: a pull request never lands
// naming a change it did not carry.
func TestADraftIsRetitledWhenItsBuildLeftALineOut(t *testing.T) {
	r := newRigWith(t, 0, tier.Rules{}, withDrafts)
	tr := draftedTrain(r)
	r.waitCount("retitle", 1)
	if got, want := r.w.Named(tr), []state.ChangeID{"a"}; !slices.Equal(got, want) {
		t.Fatalf("the promoted pull request names %v, want %v", got, want)
	}
	r.w.Pass(tr)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
	if n := r.w.Count("retitle", true); n != 1 {
		t.Fatalf("%d retitles, want 1", n)
	}
}

// A draft whose build carried every line it names is not retitled.
func TestADraftThatCarriesWhatItNamesIsNotRetitled(t *testing.T) {
	r := newRigWith(t, 0, tier.Rules{}, withDrafts)
	r.open("a", "b")
	r.together("a", "b")
	r.settle()
	s := r.waitFor("the train testing", func(s state.State) bool {
		return s.Lines["a"].State == state.LineRiding && s.Trains[s.Lines["a"].Train].State == state.TrainTesting
	})
	tr := s.Lines["a"].Train
	r.w.Pass(tr)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
	r.wantLine("b", state.LineLanded)
	if n := r.w.Count("retitle", true) + r.w.Count("retitle-failed", true); n != 0 {
		t.Fatalf("%d retitles of a draft that named what it carried", n)
	}
	if got, want := r.w.Named(tr), []state.ChangeID{"a", "b"}; !slices.Equal(got, want) {
		t.Fatalf("the pull request names %v, want %v", got, want)
	}
}

// A retitle that fails, permanently or not, never holds the train: it is
// promoted and lands. A transient failure is tried again, and the names
// come right; a permanent one is logged and left.
func TestARetitleFailureDoesNotBlockTheLanding(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault error
		want  []state.ChangeID
	}{
		{"permanent", adapter.Permanent(errors.New("422 validation failed")), []state.ChangeID{"a", "b"}},
		{"transient", errors.New("502 bad gateway"), []state.ChangeID{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRigWith(t, 0, tier.Rules{}, withDrafts, func(r *rig) {
				r.config = func(c *daemon.Config) { c.Backoff = 10 * time.Millisecond }
			})
			r.w.RetitleFaults = []error{tc.fault}
			// The train is promoted, and its CI runs, whatever the retitle
			// does.
			tr := draftedTrain(r)
			r.waitCount("retitle-failed", 1)
			if !adapter.IsPermanent(tc.fault) {
				r.waitCount("retitle", 1)
			}
			r.w.Pass(tr)
			r.tick(time.Second)
			r.wantLine("a", state.LineLanded)
			if got := r.w.Named(tr); !slices.Equal(got, tc.want) {
				t.Fatalf("the pull request names %v, want %v", got, tc.want)
			}
		})
	}
}
