package daemon_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/tier"
)

func gate(r *rig, onOff, why string) string {
	r.t.Helper()
	return r.exec(daemon.Request{Cmd: "config", Args: []string{"require-check", onOff}, Reason: why})
}

// The line-check gate is turned on at run time with a train riding and a
// line waiting: the waiting line stays until its check passes, and the
// train in flight lands as it was. The switch survives a restart.
func TestRequireCheckTurnedOnHoldsUncheckedLines(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.queue("a")
	ta := r.trainOf("a")
	if ta == "" {
		t.Fatal("a did not ride with the gate off")
	}
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather"})
	r.queue("b")
	if got := gate(r, "on", "the line checker is live"); !strings.Contains(got, "on") {
		t.Fatalf("config output: %q", got)
	}
	r.exec(daemon.Request{Cmd: "resume"})
	if r.train(ta).Gated || !r.train(ta).State.Live() {
		t.Fatalf("the train in flight was touched: gated %v, %s", r.train(ta).Gated, r.train(ta).State)
	}
	r.wantLine("b", state.LineWaiting)
	if st := r.exec(daemon.Request{Cmd: "status"}); !strings.Contains(st, "line checks required") {
		t.Fatalf("the status doesn't show the gate:\n%s", st)
	}
	r.restart()
	if !r.state().Config.RequireCheck {
		t.Fatal("the gate did not survive a restart")
	}
	r.wantLine("b", state.LineWaiting)
	r.exec(daemon.Request{Cmd: "checked", Args: []string{"b", "T0", "b-1"}, Action: "pass"})
	if tb := r.trainOf("b"); tb == "" || !r.train(tb).Gated {
		t.Fatalf("b, checked, is %s", r.line("b"))
	}
	if e := r.execErr(daemon.Request{Cmd: "config", Args: []string{"require-check", "on"}}); !strings.Contains(e, "usage") {
		t.Fatalf("no reason: %q", e)
	}
	if e := r.execErr(daemon.Request{Cmd: "config", Args: []string{"require-check", "on"}, Reason: "again"}); !strings.Contains(e, "already on") {
		t.Fatalf("on twice: %q", e)
	}
}

// Turned off, the gate releases the lines it held.
func TestRequireCheckTurnedOffReleasesLines(t *testing.T) {
	r := newRig(t)
	gate(r, "on", "the line checker is live")
	r.open("a")
	r.queue("a")
	r.wantLine("a", state.LineWaiting)
	gate(r, "off", "the line checker is down")
	if r.trainOf("a") == "" {
		t.Fatalf("a is %s with the gate off", r.line("a"))
	}
}

// With the gate on, a line checker that gives no sign of life within the
// bound while a line waits for it is raised to the judge, again each bound
// that passes; the gate stays shut. A sign of life starts the bound again.
func TestASilentLineCheckerIsAlarmed(t *testing.T) {
	r := newRigWith(t, 0, tier.Rules{}, func(r *rig) {
		r.config = func(c *daemon.Config) { c.CheckSilence = 45 * time.Minute }
	})
	gate(r, "on", "the line checker is live")
	r.open("a", "b")
	r.queue("a")
	r.queue("b")
	r.tick(40 * time.Minute)
	if len(r.w.NoticesTo("judge", "the line check has answered nothing")) > 0 {
		t.Fatal("alarmed inside the bound")
	}
	// A verdict is a sign of life: the bound runs from it.
	r.exec(daemon.Request{Cmd: "checked", Args: []string{"a", "T0", "a-1"}, Action: "pass"})
	r.tick(40 * time.Minute)
	if len(r.w.NoticesTo("judge", "the line check has answered nothing")) > 0 {
		t.Fatal("alarmed within the bound of a verdict")
	}
	r.tick(10 * time.Minute)
	if !r.alerted("the line check has answered nothing for 50m0s while 1 lines wait for it under require_check (b)") {
		t.Fatalf("no alarm; notices: %v", r.w.NoticesTo("judge", ""))
	}
	r.wantLine("b", state.LineWaiting)
	r.tick(45 * time.Minute)
	if n := len(r.w.NoticesTo("judge", "the line check has answered nothing")); n < 2 {
		t.Fatalf("the alarm was raised %d times in two bounds", n)
	}
	r.wantLine("b", state.LineWaiting)
	// With the gate off there is nothing to wait for, and no alarm.
	gate(r, "off", "the line checker is down")
	n := len(r.w.NoticesTo("judge", "the line check has answered nothing"))
	r.tick(2 * time.Hour)
	if m := len(r.w.NoticesTo("judge", "the line check has answered nothing")); m != n {
		t.Fatalf("alarmed with the gate off: %d, then %d", n, m)
	}
}
