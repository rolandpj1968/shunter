package daemon_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/tier"
)

// The queue is a bag (docs/queue-as-bag.md §2): the one override is a
// manual priority, with a reason and an expiry. The prioritised line rides
// first; the status shows it; at its expiry it ends by itself.
func TestAManualPriorityRidesFirstAndExpires(t *testing.T) {
	r := newRigWith(t, 1, tier.Rules{})
	r.open("a", "b", "c")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather the queue"})
	for _, id := range []string{"a", "b", "c"} {
		r.queue(id)
	}
	for _, bad := range []daemon.Request{
		{Cmd: "priority", Args: []string{"c"}, Until: "2h"},
		{Cmd: "priority", Args: []string{"c"}, Reason: "fixes trunk"},
		{Cmd: "priority", Args: []string{"c"}, Reason: "fixes trunk", Until: "soon"},
	} {
		if e := r.execErr(bad); !strings.Contains(e, "usage") && !strings.Contains(e, "until") {
			t.Fatalf("%+v: %q", bad, e)
		}
	}
	r.exec(daemon.Request{Cmd: "priority", Args: []string{"c"}, Reason: "fixes trunk", Until: "2h"})
	if st := r.exec(daemon.Request{Cmd: "status"}); !strings.Contains(st, "priority until 11:00Z (fixes trunk)") {
		t.Fatalf("the status does not show the priority:\n%s", st)
	}
	if got := r.state().Waiting(); !slices.Equal(got, []state.LineID{"c", "a", "b"}) {
		t.Fatalf("waiting in the order %v", got)
	}
	r.tick(3 * time.Hour)
	if p := r.state().Lines["c"].Priority; p != nil {
		t.Fatalf("the priority outlived its expiry: %+v", p)
	}
	if got := r.state().Waiting(); !slices.Equal(got, []state.LineID{"a", "b", "c"}) {
		t.Fatalf("after the expiry, waiting in the order %v", got)
	}
	r.exec(daemon.Request{Cmd: "priority", Args: []string{"b"}, Reason: "fixes trunk", Until: "2h"})
	r.exec(daemon.Request{Cmd: "priority", Args: []string{"b"}, Action: "end", Reason: "not needed"})
	if p := r.state().Lines["b"].Priority; p != nil {
		t.Fatalf("a priority taken back stands: %+v", p)
	}
	r.exec(daemon.Request{Cmd: "priority", Args: []string{"c"}, Reason: "fixes trunk", Until: "2h"})
	r.exec(daemon.Request{Cmd: "resume"})
	if tr := r.trainOf("c"); r.train(tr).Lines[0] != "c" || len(r.train(tr).Lines) != 1 {
		t.Fatalf("the first train is %v", r.train(tr).Lines)
	}
}

// Assembly takes a stack by the age of its bottom (§3, step 3): the line
// stacked on the oldest line goes before an unrelated line offered between.
func TestJoinTakesAStackByItsBottom(t *testing.T) {
	s := waiting(t, 1, 1)
	for _, ev := range []state.Event{
		state.Offer{Line: "l3", Base: "l1", Changes: []state.Change{{ID: "c3", Head: "h"}}},
		state.GateAccept{Line: "l3"},
	} {
		next, _, err := state.Next(s, ev)
		if err != nil {
			t.Fatal(err)
		}
		s = next
	}
	if got, want := daemon.Join(s, daemon.Caps{Lines: 2}), []state.LineID{"l1", "l3"}; !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}
