package daemon_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/fake"
	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/tier"
)

type admitFunc func(adapter.Offered) (string, error)

func (f admitFunc) Admit(_ context.Context, o adapter.Offered) (string, error) { return f(o) }

// TestTheGateHookRefusesWithItsReason: a repository's own rule at the gate
// (§5.2) refuses an offer, and the producer is answered with its reason by
// the queue command itself. It is asked only after the gate's own checks
// pass, with the line's changes and trunk; a hook with no answer refuses
// nothing.
func TestTheGateHookRefusesWithItsReason(t *testing.T) {
	var asked []adapter.Offered
	r := newRigWith(t, 0, tier.Rules{}, func(r *rig) {
		r.admit = admitFunc(func(o adapter.Offered) (string, error) {
			asked = append(asked, o)
			switch o.Line {
			case "m":
				return "#m: its migration sorts before trunk's latest: rename it", nil
			case "n":
				return "", errors.New("the hook crashed")
			}
			return "", nil
		})
	})
	r.open("a", "m", "n", "x")
	r.w.Change("x", func(c *fake.Change) { c.Approved = false })
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather"})
	if e := r.execErr(daemon.Request{Cmd: "queue", Args: []string{"m"}}); e != "line m refused at the gate: #m: its migration sorts before trunk's latest: rename it" {
		t.Fatalf("queue m: %q", e)
	}
	r.wantLine("m", state.LineReturned)
	if out := r.exec(daemon.Request{Cmd: "queue", Args: []string{"a"}, Alone: true}); !strings.Contains(out, "waiting") {
		t.Fatalf("queue a: %q", out)
	}
	if out := r.exec(daemon.Request{Cmd: "queue", Args: []string{"n"}}); !strings.Contains(out, "waiting") {
		t.Fatalf("a hook with no answer refused n: %q", out)
	}
	if e := r.execErr(daemon.Request{Cmd: "queue", Args: []string{"x"}}); !strings.Contains(e, "not from an approved producer") {
		t.Fatalf("queue x: %q", e)
	}
	if e := r.execErr(daemon.Request{Cmd: "queue", Args: []string{"a"}}); e == "" {
		t.Fatal("a change queued twice was taken")
	}
	var lines []string
	for _, o := range asked {
		lines = append(lines, string(o.Line))
	}
	if strings.Join(lines, " ") != "m a n" {
		t.Fatalf("the hook was asked of %v: never of a line the gate refused itself", lines)
	}
	if o := asked[1]; !o.Alone || o.Trunk != "T0" || len(o.Changes) != 1 || o.Changes[0].ID != "a" || o.Changes[0].Head != "a-1" {
		t.Fatalf("the hook's input: %+v", o)
	}
}
