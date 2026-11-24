package daemon_test

import (
	"strings"
	"testing"

	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
)

// A line check's verdict names its version, and only the present version's
// is recorded (docs/queue-as-bag.md §5). A new version, declared with the
// checker command, ends every waiting line's green and voids its charges.
func TestAVerdictIsByThePresentLineCheck(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.exec(daemon.Request{Cmd: "pause", Reason: "check before any train"})
	r.queue("a")
	r.queue("b")
	r.exec(daemon.Request{Cmd: "checker", Args: []string{state.CheckerLineCheck, "v1"}})
	if e := r.execErr(daemon.Request{Cmd: "checked", Args: []string{"a", "T0", "a-1"}, Action: "pass", Version: "v0"}); !strings.Contains(e, "version") {
		t.Fatalf("a verdict of another version: %q", e)
	}
	r.exec(daemon.Request{Cmd: "checked", Args: []string{"a", "T0", "a-1"}, Action: "pass", Version: "v1"})
	r.exec(daemon.Request{Cmd: "checked", Args: []string{"b", "T0", "b-1"}, Action: "fail", Reason: "b's test fails", Version: "v1"})
	r.wantLine("b", state.LineReturned)
	r.exec(daemon.Request{Cmd: "checker", Args: []string{state.CheckerLineCheck, "v2"}})
	if r.state().Lines["a"].CheckCurrent() {
		t.Fatal("a green of the replaced line check stands")
	}
	r.wantLine("b", state.LineWaiting)
	if e := r.execErr(daemon.Request{Cmd: "checker", Args: []string{"nonesuch", "v1"}}); !strings.Contains(e, "usage") {
		t.Fatalf("an unknown checker: %q", e)
	}
}
