package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
)

// conflicts is a builder that does what the git builder does with a line
// it can't merge: it leaves the line out, and says whether the conflict is
// with the train's base (onBase) or only with a line before it. It counts
// the leave-outs of each line.
func conflicts(onBase map[state.LineID]bool, withLine map[state.LineID]int, n map[state.LineID]int) func(context.Context, adapter.BuildRequest) (adapter.BuildResult, error) {
	return func(ctx context.Context, req adapter.BuildRequest) (adapter.BuildResult, error) {
		res := adapter.BuildResult{LeftOut: map[state.LineID]string{}, Conflicted: map[state.LineID]bool{}}
		for i, l := range req.Lines {
			switch {
			case onBase[l.ID]:
				res.LeftOut[l.ID] = "conflict in x.go, not resolved (with the train's base)"
				res.Conflicted[l.ID] = true
				n[l.ID]++
			case withLine[l.ID] > 0 && i > 0:
				withLine[l.ID]--
				res.LeftOut[l.ID] = "conflict in x.go, not resolved (with a line before it in the train)"
				n[l.ID]++
			}
		}
		return res, nil
	}
}

// Issue 55: a line in conflict with trunk at assembly, which neither the
// conflict hook nor the rebase can resolve, goes back to its owner once,
// uncharged and counting no cycle; it is not left out again at every
// assembly, and the line beside it lands.
func TestATrunkConflictAtAssemblyGoesBackOnce(t *testing.T) {
	r := newRig(t)
	n := map[state.LineID]int{}
	r.w.BuildHook = conflicts(map[state.LineID]bool{"a": true}, nil, n)
	r.open("a", "b")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather the queue"})
	r.queue("a")
	r.queue("b")
	r.exec(daemon.Request{Cmd: "resume"})
	for i := 0; i < 8; i++ {
		if l := r.state().Lines["b"]; l.State == state.LineRiding {
			r.w.Pass(l.Train)
		}
		r.tick(time.Second)
	}
	r.wantLine("b", state.LineLanded)
	a := r.state().Lines["a"]
	if a.State != state.LineReturned || a.Cause != state.CauseConflict || len(a.ChargedAt) != 0 {
		t.Fatalf("line a is %s (%s), charged at %v, want returned (%s), uncharged", a.State, a.Cause, a.ChargedAt, state.CauseConflict)
	}
	if n["a"] != 1 {
		t.Fatalf("line a was left out %d times for its trunk conflict, want once", n["a"])
	}
	if a.Count != 0 || a.Seq != 1 {
		t.Fatalf("line a: count %d, seq %d; want 0 and its first offer's", a.Count, a.Seq)
	}
}

// A conflict only with another line of the train stays in the bag: the line
// waits, counts the cycle, and rides in a later train.
func TestAConflictWithAnotherLineAtAssemblyStaysInTheBag(t *testing.T) {
	r := newRig(t)
	n := map[state.LineID]int{}
	r.w.BuildHook = conflicts(nil, map[state.LineID]int{"b": 1}, n)
	r.open("a", "b")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather the queue"})
	r.queue("a")
	r.queue("b")
	r.exec(daemon.Request{Cmd: "resume"})
	for i := 0; i < 8; i++ {
		for _, id := range []string{"a", "b"} {
			if l := r.state().Lines[state.LineID(id)]; l.State == state.LineRiding {
				r.w.Pass(l.Train)
			}
		}
		r.tick(time.Second)
	}
	r.wantLine("a", state.LineLanded)
	r.wantLine("b", state.LineLanded)
	if b := r.state().Lines["b"]; n["b"] != 1 || b.Count != 1 {
		t.Fatalf("line b was left out %d times, count %d; want once, counted", n["b"], b.Count)
	}
}
