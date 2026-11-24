package daemon_test

import (
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter/fake"
	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
)

// conflictBack sends line id back to its owner for a conflict with trunk the
// runner can't rebase (L36).
func (r *rig) conflictBack(id string) {
	r.t.Helper()
	r.exec(daemon.Request{Cmd: "checked", Args: []string{id, "T0", id + "-1"}, Action: "conflict", Reason: "x.go"})
	r.tick(time.Second)
	if l := r.state().Lines[state.LineID(id)]; l.State != state.LineReturned || l.Cause != state.CauseConflict {
		r.t.Fatalf("line %s is %s (%s), not sent back for its conflict", id, l.State, l.Cause)
	}
}

// TestALineSentBackForAConflictIsOfferedAgainOnceItMerges: a line sent back
// for a conflict with trunk needs a person, and comes back once it merges
// cleanly, at its first offer's age, with its owner (owner's ruling of 24
// Nov; docs/cutover-readiness.md B7). Until then the runner leaves it alone.
func TestALineSentBackForAConflictIsOfferedAgainOnceItMerges(t *testing.T) {
	r := newRig(t)
	r.open("a", "b", "c")
	r.exec(daemon.Request{Cmd: "pause", Reason: "check before any train"})
	r.queue("a")
	r.queue("b")
	r.conflictBack("a")
	before := *r.state().Lines["a"]
	r.queue("c")

	r.tick(time.Hour)
	if l := r.state().Lines["a"]; l.State != state.LineReturned {
		t.Fatalf("a line still in conflict was offered again: %s", l.State)
	}

	r.w.Change("a", func(c *fake.Change) { c.Head = "a-2" })
	r.w.Merging["a"] = true
	r.tick(time.Hour)
	a := r.state().Lines["a"]
	if a.State != state.LineWaiting || a.Changes[0].Head != "a-2" {
		t.Fatalf("a line that merges was not offered again: %s (%s) at %s", a.State, a.Cause, a.Changes[0].Head)
	}
	if a.Seq != before.Seq || a.Owner != before.Owner || a.Count != before.Count {
		t.Fatalf("offered again as seq %d, owner %q, count %d; was %d, %q, %d", a.Seq, a.Owner, a.Count, before.Seq, before.Owner, before.Count)
	}
	if got := r.state().Waiting(); len(got) == 0 || got[0] != "a" {
		t.Fatalf("a is not first by age: %v", got)
	}
}

// TestAChargedLineIsNeverOfferedAgainByTheRunner: a charged line comes back
// only by its owner's word (P3, I11), however cleanly it merges.
func TestAChargedLineIsNeverOfferedAgainByTheRunner(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.together("a", "b")
	r.w.Fail(r.trainOf("a"), "TestA failed in a/a_test.go", "a")
	r.tick(time.Second)
	r.wantLine("a", state.LineReturned)
	r.w.Change("a", func(c *fake.Change) { c.Head = "a-2" })
	r.w.Merging["a"] = true
	r.tick(time.Hour)
	r.tick(time.Hour)
	if l := r.state().Lines["a"]; l.State != state.LineReturned || l.Cause != state.CauseCharged {
		t.Fatalf("a charged line was offered again by the runner: %s (%s)", l.State, l.Cause)
	}
}

// TestAClosedChangeIsNotOfferedAgain: a change its owner closed is not the
// queue's to bring back.
func TestAClosedChangeIsNotOfferedAgain(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.exec(daemon.Request{Cmd: "pause", Reason: "check before any train"})
	r.queue("a")
	r.conflictBack("a")
	r.w.Change("a", func(c *fake.Change) { c.Open = false })
	r.w.Merging["a"] = true
	r.tick(time.Hour)
	if l := r.state().Lines["a"]; l.State != state.LineReturned || l.Cause != state.CauseConflict {
		t.Fatalf("a closed change was offered again: %s (%s)", l.State, l.Cause)
	}
}

// TestALineWhoseBaseWasLostComesBackWithItsBase: a line sent back because its
// base was charged is offered again only once its base is live again (or
// landed), and then stacked on it.
func TestALineWhoseBaseWasLostComesBackWithItsBase(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.stack("b", "a")
	r.together("a", "b")
	r.w.Fail(r.trainOf("a"), "TestA failed in a/a_test.go", "a")
	r.tick(time.Second)
	if l := r.state().Lines["b"]; l.State != state.LineReturned || l.Cause != state.CauseBaseLost {
		t.Fatalf("b is %s (%s)", l.State, l.Cause)
	}
	r.w.Merging["b"] = true
	r.tick(time.Hour)
	if l := r.state().Lines["b"]; l.State != state.LineReturned || l.Cause != state.CauseBaseLost {
		t.Fatalf("b was offered again while its base is with its owner: %s (%s)", l.State, l.Cause)
	}
	r.exec(daemon.Request{Cmd: "pause", Reason: "look at the lines"})
	r.w.Change("a", func(c *fake.Change) { c.Head = "a-2" })
	r.queue("a")
	r.tick(time.Hour)
	b := r.state().Lines["b"]
	if b.State == state.LineReturned || b.Base != "a" {
		t.Fatalf("b did not come back on its base: %s (%s), base %q", b.State, b.Cause, b.Base)
	}
}
