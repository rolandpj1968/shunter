package daemon_test

import (
	"slices"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/tier"
)

// A line starves by its queue time as the backstop (docs/queue-as-bag.md
// §4). Queue time doesn't run while the queue is paused. Starving lines at
// StarveAlert at once are raised to the judge.
func TestALineStarvesByQueueTimeAndTheJudgeIsTold(t *testing.T) {
	r := newRigWith(t, 1, tier.Rules{}, func(r *rig) {
		r.config = func(c *daemon.Config) { c.StarveAfter, c.StarveAlert = 12*time.Hour, 1 }
	})
	r.open("a", "b", "c")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather the queue"})
	for _, id := range []string{"a", "b", "c"} {
		r.queue(id)
	}
	r.tick(13 * time.Hour)
	if r.state().Lines["c"].Starving {
		t.Fatal("queue time ran while the queue was paused")
	}
	r.exec(daemon.Request{Cmd: "resume"})
	r.wantLine("c", state.LineWaiting)
	r.tick(12 * time.Hour)
	s := r.state()
	if !s.Lines["c"].Starving || s.Forced() != "c" {
		t.Fatalf("c is not forced: %+v", s.Lines["c"])
	}
	if !r.alerted("1 lines are starving at once") {
		t.Fatal("the judge was not told")
	}
	if got := daemon.Join(s, daemon.Caps{Lines: 1}); got != nil {
		t.Fatalf("assembled %v behind live trains while c is forced", got)
	}
}

// While a line is forced, join takes it, with what beneath it still waits,
// alone, once the chain is empty, past the caps; a prioritised line still
// goes first.
func TestJoinTakesTheForcedLineAlone(t *testing.T) {
	s := waiting(t, 1, 1, 1)
	for _, ev := range []state.Event{
		state.Offer{Line: "l4", Base: "l3", Changes: []state.Change{{ID: "c4", Head: "h"}}},
		state.GateAccept{Line: "l4"},
		state.Starving{Line: "l4", Why: "3 cycles"},
	} {
		next, _, err := state.Next(s, ev)
		if err != nil {
			t.Fatal(err)
		}
		s = next
	}
	if got, want := daemon.Join(s, daemon.Caps{Lines: 1}), []state.LineID{"l3", "l4"}; !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
	next, _, err := state.Next(s, state.Prioritise{Line: "l2", By: "judge", Reason: "fixes trunk", Until: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := daemon.Join(next, daemon.Caps{Lines: 8}), []state.LineID{"l2"}; !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

// A starving line the runner can't rebase is raised at once: it needs a
// person.
func TestAStarvingLineTheRunnerCannotRebaseIsRaised(t *testing.T) {
	r := newRigWith(t, 1, tier.Rules{}, func(r *rig) {
		r.config = func(c *daemon.Config) { c.StarveCycles = 1 }
	})
	r.open("a")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather the queue"})
	r.queue("a")
	r.exec(daemon.Request{Cmd: "checked", Args: []string{"a", "T0", "a-1"}, Action: "conflict", Reason: "x.go"})
	r.tick(time.Minute)
	r.tick(time.Minute)
	if !r.alerted("starving and stuck") {
		t.Fatal("the judge was not told")
	}
}
