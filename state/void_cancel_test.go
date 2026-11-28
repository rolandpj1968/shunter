package state

import "testing"

// A void train's run is stopped, whatever voided it. Closing its pull
// request does not stop its CI, so a run left going competes with the
// trains after it for the runners. Cancelling a run that has finished is
// harmless: the CI adapter skips a completed run.
func TestAVoidTrainsRunIsCancelled(t *testing.T) {
	two := cat(ready("a", "b", "c"), testing_("t1", "a"), testing_("t2", "b", "c"))
	cases := []struct {
		name string
		ev   Event
		want []TrainID
	}{
		{"the train ahead fails", CIFailed{Train: "t1", Blamed: ids("a"), Evidence: "a's test fails"}, []TrainID{"t1", "t2"}},
		{"a line of the train ahead is withdrawn", Withdraw{Line: "a"}, []TrainID{"t1", "t2"}},
		{"a line of the train ahead is held", Hold{Line: "a", Reason: "r"}, []TrainID{"t1", "t2"}},
		{"a line of the train ahead moves", HeadMoved{Line: "a", Change: "a", Head: "a2"}, []TrainID{"t1", "t2"}},
		{"the train ahead has no verdict", CINoVerdict{Train: "t1", Why: "w"}, []TrainID{"t1", "t2"}},
		{"a line of the train behind is withdrawn", Withdraw{Line: "c"}, []TrainID{"t2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.must(two...)
			fx, err := h.step(c.ev)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range c.want {
				if h.s.Trains[id].State.Live() {
					t.Fatalf("train %s is still %s", id, h.s.Trains[id].State)
				}
				if !cancels(fx, id) {
					t.Errorf("train %s ended %s with no cancel-run: %v", id, h.s.Trains[id].State, fx)
				}
			}
		})
	}
}
