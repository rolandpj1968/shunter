package store

import (
	"testing"

	"github.com/rolandpj1968/shunter/state"
)

// The line-check gate changed at run time is an event in the log: a replay
// with no snapshot reproduces it, and the trains it marked, and the line
// checker's clock it started.
func TestRequireCheckReplays(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir, 1000)
	offer := state.Offer{Line: "a", Changes: []state.Change{{ID: "a", Head: "a1"}}}
	applyAll(t, st, []state.Event{
		offer, state.GateAccept{Line: "a"},
		state.RequireCheck{On: true, Reason: "the checker is live"},
		state.LineChecked{Line: "a", Heads: []state.Head{"a1"}, Trunk: "t0", Pass: true},
		state.Assemble{Train: "t1", Lines: []state.LineID{"a"}},
	})
	want, since := st.State(), st.Since()
	if !want.Config.RequireCheck || !want.Trains["t1"].Gated || since.Checker.IsZero() {
		t.Fatalf("gate %v, t1 gated %v, checker clock %v", want.Config.RequireCheck, want.Trains["t1"].Gated, since.Checker)
	}
	st.Abandon() // a crash: no snapshot, the log alone

	st = open(t, dir, 1000)
	defer st.Close()
	if got := st.State(); !got.Config.RequireCheck || !got.Trains["t1"].Gated {
		t.Fatalf("after replay: gate %v, t1 gated %v", got.Config.RequireCheck, got.Trains["t1"].Gated)
	}
	if !st.Since().Checker.Equal(since.Checker) {
		t.Fatalf("checker clock after replay %v, want %v", st.Since().Checker, since.Checker)
	}
	if _, _, err := st.Apply("judge", state.RequireCheck{On: false, Reason: "the checker is down"}); err != nil {
		t.Fatal(err)
	}
	if st.State().Config.RequireCheck {
		t.Fatal("the gate did not turn off")
	}
}
