package daemon_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/fake"
	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
)

// movedHeads is a builder that does what the git builder does: a change
// whose head on the forge is not the head it was asked to build is left
// out, with its new head. It counts those leave-outs.
func movedHeads(w *fake.World, heads func(state.ChangeID) state.Head, n *int) func(context.Context, adapter.BuildRequest) (adapter.BuildResult, error) {
	return func(ctx context.Context, req adapter.BuildRequest) (adapter.BuildResult, error) {
		res := adapter.BuildResult{LeftOut: map[state.LineID]string{}, Moved: map[state.ChangeID]state.Head{}}
		for _, l := range req.Lines {
			for _, c := range l.Changes {
				if h := heads(c.ID); h != c.Head {
					res.LeftOut[l.ID] = fmt.Sprintf("change %s: its head moved from %s to %s", c.ID, c.Head, h)
					res.Moved[c.ID] = h
					*n++
				}
			}
		}
		return res, nil
	}
}

func (r *rig) head(id state.ChangeID) state.Head {
	var h state.Head
	r.w.Change(id, func(c *fake.Change) { h = c.Head })
	return h
}

func (r *rig) push(id string, head state.Head) {
	r.w.Change(state.ChangeID(id), func(c *fake.Change) { c.Head = head })
}

// Issue 51: a waiting line pushed to is left out of one assembly, re-keyed
// at its new head, and rides there; it is never left out again, and the
// leave-out counts no cycle.
func TestAWaitingLinePushedToIsReKeyedAndRides(t *testing.T) {
	r := newRig(t)
	leftOut := 0
	r.w.BuildHook = movedHeads(r.w, r.head, &leftOut)
	r.open("a", "b")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather the queue"})
	r.queue("a")
	r.queue("b")
	r.push("a", "a-2")
	r.exec(daemon.Request{Cmd: "resume"})
	for i := 0; i < 5; i++ {
		for _, id := range []string{"a", "b"} {
			if l := r.state().Lines[state.LineID(id)]; l.State == state.LineRiding {
				r.w.Pass(l.Train)
			}
		}
		r.tick(time.Second)
	}
	r.wantLine("a", state.LineLanded)
	r.wantLine("b", state.LineLanded)
	l := r.state().Lines["a"]
	if leftOut != 1 {
		t.Fatalf("line a was left out %d times for its moved head, want once", leftOut)
	}
	if l.Changes[0].Head != "a-2" || l.Count != 0 {
		t.Fatalf("line a landed at %s with count %d, want a-2 and 0", l.Changes[0].Head, l.Count)
	}
}

// A held line pushed to is re-keyed while it is held, and stays held;
// released, it rides at the new head with no leave-out at all.
func TestAHeldLinePushedToRidesAtItsNewHeadOnRelease(t *testing.T) {
	r := newRig(t)
	leftOut := 0
	r.w.BuildHook = movedHeads(r.w, r.head, &leftOut)
	r.open("a")
	r.exec(daemon.Request{Cmd: "pause", Reason: "hold it before it rides"})
	r.queue("a")
	r.exec(daemon.Request{Cmd: "hold", Args: []string{"a"}, Reason: "the review found a problem"})
	r.exec(daemon.Request{Cmd: "resume"})
	r.push("a", "a-2")
	r.tick(2 * time.Minute)
	if l := r.state().Lines["a"]; l.State != state.LineHeld || l.Changes[0].Head != "a-2" {
		t.Fatalf("line a is %s at %s, want held at a-2\n%s", l.State, l.Changes[0].Head, daemon.Status(r.state(), nil))
	}
	r.exec(daemon.Request{Cmd: "release", Args: []string{"a"}})
	r.w.Pass(r.trainOf("a"))
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
	if leftOut != 0 {
		t.Fatalf("line a was left out %d times for a head the queue had read", leftOut)
	}
	if !strings.HasPrefix(string(r.state().Lines["a"].Changes[0].Head), "a-2") {
		t.Fatalf("line a landed at %s", r.state().Lines["a"].Changes[0].Head)
	}
}
