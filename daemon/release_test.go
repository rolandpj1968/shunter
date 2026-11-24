package daemon_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/daemon"
)

// TestALineCheckWithNoVerdictReleasesItsClaim: a checker whose
// infrastructure failed answers infra (§5.4). Its claim ends at once, the
// judge is told, nothing is recorded of the line, and the line is not given
// to the next checker that asks for any line until the pacing has passed.
// A claim names the version it checks by, and a verdict by that version is
// the claim's.
func TestALineCheckWithNoVerdictReleasesItsClaim(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather"})
	r.queue("a")
	r.queue("b")
	r.exec(daemon.Request{Cmd: "checker", Args: []string{"line-check", "v1"}})
	claim := func(worker string) string {
		t.Helper()
		return r.exec(daemon.Request{Cmd: "claim", Actor: worker, Version: "v1"})
	}
	if e := r.execErr(daemon.Request{Cmd: "claim", Actor: "w0", Version: "v2"}); !strings.Contains(e, `at version "v1"`) {
		t.Fatalf("a claim by another version: %q", e)
	}
	if got := claim("w1"); got != "a T0 a-1" {
		t.Fatalf("first claim: %q", got)
	}
	if e := r.execErr(daemon.Request{Cmd: "checked", Args: []string{"a", "T0", "a-1"}, Action: "infra", Version: "v1", Actor: "w1"}); !strings.Contains(e, "usage") {
		t.Fatalf("an infra answer with no reason: %q", e)
	}
	r.exec(daemon.Request{Cmd: "checked", Args: []string{"a", "T0", "a-1"}, Action: "infra", Reason: "the CI host was unreachable", Version: "v1", Actor: "w1"})
	if l := r.state().Lines["a"]; l.Claim != nil || l.CheckCurrent() || l.State.String() != "waiting" {
		t.Fatalf("after infra: %+v", l)
	}
	if !r.alerted("the line check of a by w1 gave no verdict: the CI host was unreachable") {
		t.Fatal("the judge was not told of a check with no verdict")
	}
	if got := claim("w2"); got != "b T0 b-1" {
		t.Fatalf("a line given up with no verdict was given again at once: %q", got)
	}
	r.exec(daemon.Request{Cmd: "checked", Args: []string{"b", "T0", "b-1"}, Action: "pass", Version: "v1", Actor: "w2"})
	if !r.state().Lines["b"].CheckCurrent() {
		t.Fatal("the verdict of a claim under v1 was not kept")
	}
	if got := claim("w3"); got != "" {
		t.Fatalf("within the pacing: %q", got)
	}
	r.tick(11 * time.Minute)
	if got := claim("w3"); got != "a T0 a-1" {
		t.Fatalf("after the pacing: %q", got)
	}
}
