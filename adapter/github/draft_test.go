package github

import (
	"strings"
	"testing"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
)

// A train is shown from assembly as a draft (adapter.Drafter): a draft
// starts no CI, opening it again finds it after the branch moved, and
// promotion marks it ready and dispatches CI on the built head.
func TestADraftIsOpenedThenPromoted(t *testing.T) {
	s, c := newServer(t)
	c.cfg.Dispatch, c.cfg.Workflow = true, "ci.yml"
	s.refs["main"] = "trunk1"
	s.refs["shunter/t1"] = "m1"
	pr, err := c.OpenDraft(ctx, "k1.draft", "t1", nil, "m1", "trunk1")
	if err != nil {
		t.Fatal(err)
	}
	if s.pulls[501]["draft"] != true || s.count("POST", "/actions/workflows/ci.yml/dispatches") != 0 {
		t.Fatalf("draft %v, dispatches %d", s.pulls[501]["draft"], s.count("POST", "/actions/workflows/ci.yml/dispatches"))
	}
	// Pre-flight moved the branch; the draft is still the train's.
	s.refs["shunter/t1"] = "h1"
	s.pulls[501]["head"].(map[string]any)["sha"] = "h1"
	if again, err := c.OpenDraft(ctx, "k1.draft", "t1", nil, "h1", "trunk1"); err != nil || again != pr {
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

// A draft whose build left a line out is retitled to name what the train
// carries: the title and the list change, the key's mark is kept byte for
// byte, and a draft that already says so is not written again.
func TestADraftIsRetitled(t *testing.T) {
	s, c := newServer(t)
	s.refs["main"] = "trunk1"
	s.refs["shunter/t1"] = "m1"
	pr, err := c.OpenDraft(ctx, "k1.draft", "t1", []state.ChangeID{"11", "12"}, "m1", "trunk1")
	if err != nil {
		t.Fatal(err)
	}
	mark := keyMark("k1.draft")
	if s.pulls[501]["title"] != "Merge train t1: #11 #12" || !strings.Contains(s.pulls[501]["body"].(string), "- #12\n") {
		t.Fatalf("opened as %q: %q", s.pulls[501]["title"], s.pulls[501]["body"])
	}
	if err := c.Retitle(ctx, "t1", pr, []state.ChangeID{"11"}, "h1", "trunk1"); err != nil {
		t.Fatal(err)
	}
	body := s.pulls[501]["body"].(string)
	if s.pulls[501]["title"] != "Merge train t1: #11" || strings.Contains(body, "#12") || !strings.Contains(body, "- #11\n") || !strings.Contains(body, mark) || strings.Count(body, "shunter-key") != 1 {
		t.Fatalf("retitled as %q: %q", s.pulls[501]["title"], body)
	}
	if body != trainBody("k1.draft", "t1", []state.ChangeID{"11"}, "h1", "trunk1") {
		t.Fatalf("the body is not as a pull request opened naming #11: %q", body)
	}
	before := s.count("PATCH", "/pulls/501")
	if err := c.Retitle(ctx, "t1", pr, []state.ChangeID{"11"}, "h1", "trunk1"); err != nil || s.count("PATCH", "/pulls/501") != before {
		t.Fatalf("again: %v, %d writes", err, s.count("PATCH", "/pulls/501")-before)
	}
	if err := c.Retitle(ctx, "t1", "999", []state.ChangeID{"11"}, "h1", "trunk1"); !adapter.IsPermanent(err) {
		t.Fatalf("a pull request not there: %v", err)
	}
}
