package github

import (
	"testing"

	"github.com/rolandpj1968/shunter/adapter"
)

// A train is shown from assembly as a draft (adapter.Drafter): a draft
// starts no CI, opening it again finds it after the branch moved, and
// promotion marks it ready and dispatches CI on the built head.
func TestADraftIsOpenedThenPromoted(t *testing.T) {
	s, c := newServer(t)
	c.cfg.Dispatch, c.cfg.Workflow = true, "ci.yml"
	s.refs["main"] = "trunk1"
	s.refs["shunter/t1"] = "m1"
	pr, err := c.OpenDraft(ctx, "k1.draft", "t1", "m1", "trunk1")
	if err != nil {
		t.Fatal(err)
	}
	if s.pulls[501]["draft"] != true || s.count("POST", "/actions/workflows/ci.yml/dispatches") != 0 {
		t.Fatalf("draft %v, dispatches %d", s.pulls[501]["draft"], s.count("POST", "/actions/workflows/ci.yml/dispatches"))
	}
	// Pre-flight moved the branch; the draft is still the train's.
	s.refs["shunter/t1"] = "h1"
	s.pulls[501]["head"].(map[string]any)["sha"] = "h1"
	if again, err := c.OpenDraft(ctx, "k1.draft", "t1", "h1", "trunk1"); err != nil || again != pr {
		t.Fatalf("again: %s %v", again, err)
	}
	if err := c.Promote(ctx, "k1", "t1", pr, "h2"); err == nil || adapter.IsPermanent(err) {
		t.Fatalf("promoted at a head the pull request is not at: %v", err)
	}
	if err := c.Promote(ctx, "k1", "t1", pr, "h1"); err != nil {
		t.Fatal(err)
	}
	if s.pulls[501]["draft"] != false || s.count("POST", "/actions/workflows/ci.yml/dispatches") != 1 {
		t.Fatalf("after promotion: draft %v, dispatches %d", s.pulls[501]["draft"], s.count("POST", "/actions/workflows/ci.yml/dispatches"))
	}
}
