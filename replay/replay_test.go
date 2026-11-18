package replay

import (
	"errors"
	"strings"
	"testing"

	"github.com/rolandpj1968/shunter/state"
)

func TestRun(t *testing.T) {
	one := []state.Change{{ID: "a", Head: "a1"}}
	src := &Slice{Name: "s", Events: []state.Event{
		state.Offer{Line: "a", Changes: one},
		state.GateAccept{Line: "a"},
		state.Assemble{Train: "t1", Lines: []state.LineID{"a"}},
		state.Built{Train: "t1", Head: "h1"},
		state.PushOK{Train: "t1"}, // refused: nothing lands without a verdict
		state.Opened{Train: "t1", PR: "1", Head: "h1"},
		state.CIPassed{Train: "t1", Head: "h2"}, // refused: not the head built
	}}
	s, rep := Run(state.New("trunk0", state.Config{}), src)
	if rep.Events != 7 || rep.Applied != 5 || len(rep.Refused) != 2 || len(rep.Breaches) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if !errors.Is(rep.Refused[0].Err, state.ErrState) || !errors.Is(rep.Refused[1].Err, state.ErrHead) {
		t.Errorf("refusals: %v", rep.Refused)
	}
	// The log ends with the train in CI: it is unsettled, and aborted so
	// that its line is not lost.
	if len(rep.Unsettled) != 1 || rep.Unsettled[0].State != state.TrainTesting {
		t.Errorf("unsettled: %+v", rep.Unsettled)
	}
	if s.Trains["t1"].State != state.TrainAborted || s.Lines["a"].State != state.LineWaiting || s.Trunk != "trunk0" {
		t.Errorf("final state: train %s, line %s, trunk %s", s.Trains["t1"].State, s.Lines["a"].State, s.Trunk)
	}
	var b strings.Builder
	rep.Write(&b, false)
	if !strings.Contains(b.String(), "refused 2") || !strings.Contains(b.String(), "I1, I9") {
		t.Errorf("report:\n%s", b.String())
	}
}

func TestRunResumesAfterJ3(t *testing.T) {
	one := []state.Change{{ID: "a", Head: "a1"}}
	evs := []state.Event{state.Offer{Line: "a", Changes: one}, state.GateAccept{Line: "a"}}
	for _, tr := range []state.TrainID{"t1", "t2"} {
		evs = append(evs, state.Assemble{Train: tr, Lines: []state.LineID{"a"}}, state.CINoVerdict{Train: tr, Why: "builder died"})
	}
	s, rep := Run(state.New("trunk0", state.Config{}), &Slice{Name: "s", Events: evs})
	if rep.Pauses != 1 || s.Paused || len(rep.Refused) != 0 {
		t.Errorf("pauses %d, paused %v, refused %v", rep.Pauses, s.Paused, rep.Refused)
	}
}
