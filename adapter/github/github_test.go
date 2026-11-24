package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/hook"
	"github.com/rolandpj1968/shunter/state"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
)

func TestDescribe(t *testing.T) {
	s, c := newServer(t)
	s.pull(11, "open", "aaa", "feat-a", "main", "alice")
	s.pull(12, "open", "bbb", "feat-b", "feat-a", "alice")
	s.pull(13, "closed", "ccc", "feat-c", "main", "bob", "landed")
	s.pull(14, "open", "ddd", "feat-d", "main", "mallory")
	s.pull(15, "open", "eee", "feat-e", "gone-branch", "alice")
	got, err := c.Describe(ctx, []state.ChangeID{"11", "12", "13", "14", "15"})
	if err != nil {
		t.Fatal(err)
	}
	want := []adapter.ChangeInfo{
		{ID: "11", Head: "aaa", Open: true, Approved: true},
		{ID: "12", Head: "bbb", Base: "11", Open: true, Approved: true},
		{ID: "13", Head: "ccc", Open: false, Approved: true, Landed: true},
		{ID: "14", Head: "ddd", Open: true, Approved: false},
		{ID: "15", Head: "eee", Base: "branch:gone-branch", Open: true, Approved: true},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change %s: %+v, want %+v", want[i].ID, got[i], want[i])
		}
	}
	if _, err := c.Describe(ctx, []state.ChangeID{"99"}); !adapter.IsPermanent(err) {
		t.Fatalf("a missing change: %v, want permanent", err)
	}
	if _, err := c.Describe(ctx, []state.ChangeID{"not-a-number"}); !adapter.IsPermanent(err) {
		t.Fatalf("a bad id: %v, want permanent", err)
	}
}

// TestGateProducerRules: an approval file approves a change whoever wrote
// it, and an author with a branch rule is approved only from a branch the
// rule matches.
func TestGateProducerRules(t *testing.T) {
	s, c := newServer(t)
	dir := t.TempDir()
	list := filepath.Join(dir, "approved.list")
	c.cfg.ApprovedFile = list
	c2, err := New(Config{Owner: "acme", Repo: "widgets", Trunk: "main", AuthorBranches: map[string]string{"refresh-bot": "^refresh/", "alice": "^feat-"}})
	if err != nil {
		t.Fatal(err)
	}
	c.branchRule = c2.branchRule
	s.pull(21, "open", "a21", "refresh/deps", "main", "refresh-bot")
	s.pull(22, "open", "a22", "fix-everything", "main", "refresh-bot")
	s.pull(23, "open", "a23", "contrib", "main", "mallory")
	s.pull(24, "open", "a24", "feat-x", "main", "alice")
	s.pull(25, "open", "a25", "other", "main", "alice")
	s.pull(26, "open", "a26", "b", "main", "bob")
	approved := func() string {
		got, err := c.Describe(ctx, []state.ChangeID{"21", "22", "23", "24", "25", "26"})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, ch := range got {
			if ch.Approved {
				out = append(out, string(ch.ID))
			}
		}
		return strings.Join(out, " ")
	}
	if got := approved(); got != "21 24 26" {
		t.Fatalf("no approval file: approved %s", got)
	}
	os.WriteFile(list, []byte("# changes approved by hand\n#23 reviewed\n22 25\n"), 0o644)
	if got := approved(); got != "21 22 23 24 25 26" {
		t.Fatalf("with the approval file: approved %s", got)
	}
	if _, err := New(Config{Owner: "o", Repo: "r", Trunk: "main", AuthorBranches: map[string]string{"x": "("}}); err == nil {
		t.Fatal("a bad branch rule was taken")
	}
}

func TestTransientFaultsAreRetriedAndEmptyIsNoAnswer(t *testing.T) {
	s, c := newServer(t)
	s.pull(11, "open", "aaa", "feat-a", "main", "alice")
	s.faults["/pulls/11"] = []int{502, 503}
	if _, err := c.Describe(ctx, []state.ChangeID{"11"}); err != nil {
		t.Fatalf("two 5xx then an answer: %v", err)
	}
	s.faults["/pulls/11"] = []int{502, 502, 502}
	_, err := c.Describe(ctx, []state.ChangeID{"11"})
	if err == nil || adapter.IsPermanent(err) {
		t.Fatalf("three 5xx: %v, want transient", err)
	}
	s.empty["/pulls/11"] = true
	if _, err := c.Describe(ctx, []state.ChangeID{"11"}); err != nil {
		t.Fatalf("one empty answer then a real one: %v", err)
	}
	if n := s.count("GET", "/pulls/11"); n != 8 {
		t.Fatalf("%d reads, want 8", n)
	}
}

func TestReadOnlyRefusesWritesBeforeSending(t *testing.T) {
	s, c := newServer(t)
	c.cfg.ReadOnly = true
	s.refs["main"] = "trunk1"
	s.refs["shunter/t1"] = "h1"
	_, err := c.OpenPR(ctx, "k1", "t1", "h1", "trunk1")
	if !errors.Is(err, errReadOnly) || !adapter.IsPermanent(err) {
		t.Fatalf("open in read-only: %v", err)
	}
	if err := c.Cancel(ctx, "k2", "t1"); err != nil {
		t.Fatalf("cancel with nothing to cancel is a read: %v", err)
	}
	s.run(7, "h1", "shunter/t1", "in_progress", "")
	if err := c.Cancel(ctx, "k3", "t1"); !errors.Is(err, errReadOnly) {
		t.Fatalf("cancel in read-only: %v", err)
	}
	for _, l := range s.log {
		if !strings.HasPrefix(l, "GET ") {
			t.Fatalf("a write reached the forge: %s", l)
		}
	}
}

func TestOpenPRIsIdempotentAndFindsItsBase(t *testing.T) {
	s, c := newServer(t)
	s.refs["main"] = "trunk1"
	s.refs["shunter/t1"] = "h1"
	pr, err := c.OpenPR(ctx, "k1", "t1", "h1", "trunk1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.OpenPR(ctx, "k1", "t1", "h1", "trunk1")
	if err != nil || again != pr || s.count("POST", "/pulls") != 1 {
		t.Fatalf("again: %s %v (posts %d)", again, err, s.count("POST", "/pulls"))
	}
	if base := s.pulls[501]["base"].(map[string]any)["ref"]; base != "main" {
		t.Fatalf("train 1's base %v", base)
	}
	// Train 2 is built on train 1's head: its pull request goes to train
	// 1's branch.
	s.refs["shunter/t2"] = "h2"
	if _, err := c.OpenPR(ctx, "k2", "t2", "h2", "h1"); err != nil {
		t.Fatal(err)
	}
	if base := s.pulls[502]["base"].(map[string]any)["ref"]; base != "shunter/t1" {
		t.Fatalf("train 2's base %v", base)
	}
	// A branch not at the built head is refused, permanently.
	s.refs["shunter/t3"] = "other"
	if _, err := c.OpenPR(ctx, "k3", "t3", "h3", "trunk1"); !adapter.IsPermanent(err) {
		t.Fatalf("branch elsewhere: %v", err)
	}
	// A base that is nothing known is refused.
	s.refs["shunter/t4"] = "h4"
	if _, err := c.OpenPR(ctx, "k4", "t4", "h4", "nowhere"); !adapter.IsPermanent(err) {
		t.Fatalf("unknown base: %v", err)
	}
}

func TestDispatchMode(t *testing.T) {
	s, c := newServer(t)
	c.cfg.Dispatch, c.cfg.Workflow = true, "ci.yml"
	s.refs["main"] = "trunk1"
	s.refs["shunter/t1"] = "h1"
	if _, err := c.OpenPR(ctx, "k1", "t1", "h1", "trunk1"); err != nil {
		t.Fatal(err)
	}
	if n := s.count("POST", "/actions/workflows/ci.yml/dispatches"); n != 1 {
		t.Fatalf("%d dispatches", n)
	}
	s.run(1, "h1", "shunter/t1", "queued", "")
	if _, err := c.OpenPR(ctx, "k1", "t1", "h1", "trunk1"); err != nil {
		t.Fatal(err)
	}
	if n := s.count("POST", "/actions/workflows/ci.yml/dispatches"); n != 1 {
		t.Fatalf("a run on the head already: %d dispatches, want still 1", n)
	}
}

func TestClosePR(t *testing.T) {
	s, c := newServer(t)
	s.refs["main"] = "trunk1"
	s.refs["shunter/t1"] = "h1"
	pr, _ := c.OpenPR(ctx, "k1", "t1", "h1", "trunk1")
	if err := c.ClosePR(ctx, "k2", "t1", "void: a head moved"); err != nil {
		t.Fatal(err)
	}
	if s.pulls[501]["state"] != "closed" || pr != "501" {
		t.Fatalf("not closed")
	}
	if err := c.ClosePR(ctx, "k2", "t1", "void: a head moved"); err != nil {
		t.Fatalf("closing a closed one: %v", err)
	}
	if n := s.count("POST", "/issues/501/comments"); n != 1 {
		t.Fatalf("%d comments", n)
	}
	if err := c.ClosePR(ctx, "k3", "never", "x"); err != nil {
		t.Fatalf("closing one never opened: %v", err)
	}
}

func TestStatusClassifies(t *testing.T) {
	cases := []struct {
		name   string
		checks func(s *server)
		want   adapter.Outcome
		why    string
	}{
		{"nothing yet", func(s *server) {}, adapter.Pending, ""},
		{"running", func(s *server) { s.check("h", "check", "in_progress", "", t0) }, adapter.Pending, "check is in_progress"},
		{"passed", func(s *server) {
			s.check("h", "test (1)", "completed", "success", t0)
			s.check("h", "check", "completed", "success", t0)
		}, adapter.Passed, ""},
		{"failed", func(s *server) {
			s.check("h", "test (1)", "completed", "failure", t0)
			s.check("h", "check", "completed", "failure", t0)
		}, adapter.Failed, ""},
		{"cancelled by hand", func(s *server) {
			s.check("h", "test (1)", "completed", "cancelled", t0)
			s.check("h", "check", "completed", "cancelled", t0)
		}, adapter.NoVerdict, "no verdict: check cancelled"},
		{"skipped", func(s *server) { s.check("h", "check", "completed", "skipped", t0) }, adapter.NoVerdict, "no verdict: check skipped"},
		{"timed out only", func(s *server) {
			s.check("h", "test (1)", "completed", "timed_out", t0)
			s.check("h", "check", "completed", "failure", t0)
		}, adapter.Failed, ""},
		{"timeout alone", func(s *server) {
			s.check("h", "test (1)", "completed", "timed_out", t0)
			s.check("h", "check", "completed", "cancelled", t0)
		}, adapter.Flaky, "only timeouts: test (1)"},
		{"a rerun passed", func(s *server) {
			s.check("h", "test (1)", "completed", "failure", t0)
			s.check("h", "check", "completed", "failure", t0)
			s.check("h", "test (1)", "completed", "success", t0.Add(time.Hour))
			s.check("h", "check", "completed", "success", t0.Add(time.Hour))
		}, adapter.Passed, ""},
		// docs/queue-as-bag.md §6: a failed job is reported while the rest
		// still runs, without blame to say otherwise decisive.
		{"a job failed, the rest runs", func(s *server) {
			s.check("h", "test (1)", "completed", "failure", t0)
			s.check("h", "check", "in_progress", "", t0)
		}, adapter.Failed, ""},
		{"a job timed out, the rest runs", func(s *server) {
			s.check("h", "test (1)", "completed", "timed_out", t0)
			s.check("h", "check", "in_progress", "", t0)
		}, adapter.Pending, "check is in_progress"},
		{"a rerun runs", func(s *server) {
			s.check("h", "check", "completed", "failure", t0)
			s.check("h", "check", "queued", "", t0.Add(time.Hour))
		}, adapter.Pending, "check is queued"},
	}
	for _, tc := range cases {
		s, c := newServer(t)
		tc.checks(s)
		rep, err := c.Status(ctx, "t1", "h", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Outcome != tc.want || (tc.why != "" && rep.Why != tc.why) {
			t.Errorf("%s: outcome %d why %q, want %d %q", tc.name, rep.Outcome, rep.Why, tc.want, tc.why)
		}
	}
}

// TestStatusReadsEveryPage: a head that became trunk collects other
// workflows' checks and runs by the hundred, and the verdict is on a later
// page than the first. Reading one page found no required check and read
// a landed train as pending.
func TestStatusReadsEveryPage(t *testing.T) {
	for _, workflow := range []string{"", "ci.yml"} {
		s, c := newServer(t)
		c.cfg.Workflow = workflow
		s.refs["main"] = "h"
		for i := range int64(70) {
			s.runOf("lines.yml", 100+i, 100+i, "h", "main", "completed", "success")
			s.checkIn(100+i, "h", "line", "completed", "success", t0.Add(time.Hour))
			s.checkIn(100+i, "h", "plan", "completed", "success", t0.Add(time.Hour))
		}
		s.runOf("ci.yml", 10, 1, "h", "shunter/t1", "completed", "success")
		s.checkIn(10, "h", "test (1)", "completed", "success", t0)
		s.checkIn(10, "h", "check", "completed", "success", t0)
		rep, err := c.Status(ctx, "t1", "h", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Outcome != adapter.Passed {
			t.Errorf("workflow %q: outcome %d (%s)", workflow, rep.Outcome, rep.Why)
		}
		if r, err := c.ReadLanding(ctx, "t1", "h", nil); err != nil || r.Verdict != "h" {
			t.Errorf("workflow %q: the lander reads %+v %v", workflow, r, err)
		}
	}
}

// TestStatusCountsOnlyItsOwnWorkflow: another workflow's failed checks on
// the same commit (a nightly run on a head that has landed) are not the
// train's CI. With a workflow configured, only its runs' check suites are
// read; with required checks named, only the suites holding them count.
func TestStatusCountsOnlyItsOwnWorkflow(t *testing.T) {
	world := func(s *server) {
		s.refs["main"] = "trunk1"
		s.runOf("ci.yml", 10, 1, "h", "shunter/t1", "completed", "success")
		s.runOf("nightly.yml", 20, 2, "h", "main", "completed", "failure")
		s.checkIn(10, "h", "test (1)", "completed", "success", t0)
		s.checkIn(10, "h", "check", "completed", "success", t0)
		s.checkIn(20, "h", "e2e", "completed", "failure", t0.Add(time.Hour))
		s.checkIn(20, "h", "mutation", "completed", "timed_out", t0.Add(time.Hour))
	}
	for _, tc := range []struct {
		name     string
		workflow string
		checks   []string
	}{
		{"by workflow", "ci.yml", []string{"check"}},
		{"by the required check's suite", "", []string{"check"}},
		{"by workflow, no check named", "ci.yml", nil},
	} {
		s, c := newServer(t)
		c.cfg.Workflow, c.cfg.Checks = tc.workflow, tc.checks
		world(s)
		rep, err := c.Status(ctx, "t1", "h", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Outcome != adapter.Passed {
			t.Errorf("%s: outcome %d (%s %s), want passed", tc.name, rep.Outcome, rep.Evidence, rep.Why)
		}
		for _, ch := range rep.Checks {
			if ch.Suite == 20 {
				t.Errorf("%s: the report carries another workflow's check %s", tc.name, ch.Name)
			}
		}
		if r, err := c.ReadLanding(ctx, "t1", "h", nil); err != nil || r.Verdict != "h" {
			t.Errorf("%s: the lander reads %+v %v", tc.name, r, err)
		}
	}
	// A failure in the train's own workflow still fails it.
	s, c := newServer(t)
	c.cfg.Workflow = "ci.yml"
	world(s)
	s.checkIn(10, "h", "test (2)", "completed", "failure", t0)
	if rep, _ := c.Status(ctx, "t1", "h", nil); rep.Outcome != adapter.Failed || !strings.Contains(rep.Evidence, "test (2)") || strings.Contains(rep.Evidence, "e2e") {
		t.Errorf("its own failure: outcome %d evidence %q", rep.Outcome, rep.Evidence)
	}
	// No run of the workflow on the head yet: nothing read, pending.
	s, c = newServer(t)
	c.cfg.Workflow = "ci.yml"
	s.runOf("nightly.yml", 20, 2, "h", "main", "completed", "failure")
	s.checkIn(20, "h", "check", "completed", "failure", t0)
	if rep, _ := c.Status(ctx, "t1", "h", nil); rep.Outcome != adapter.Pending {
		t.Errorf("no run of the workflow: outcome %d, want pending", rep.Outcome)
	}
}

func blameHook(t *testing.T, answer string, code int) hook.Hook {
	p := filepath.Join(t.TempDir(), "blame.sh")
	body := "#!/bin/sh\ncat > /dev/null\necho '" + answer + "'\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return hook.Hook{Command: []string{p}, Timeout: 10 * time.Second}
}

func TestBlameHookAdvisesButCannotPass(t *testing.T) {
	lines := []adapter.BuildLine{{ID: "11", Changes: []state.Change{{ID: "11", Head: "aaa"}}}, {ID: "12"}}
	failed := func(s *server) {
		s.check("h", "test (1)", "completed", "failure", t0)
		s.check("h", "check", "completed", "failure", t0)
	}
	cases := []struct {
		answer string
		code   int
		want   adapter.Outcome
		blamed string
	}{
		{`{"class":"deterministic","blamed":["11","99"],"evidence":"TestX in a.go"}`, 0, adapter.Failed, "[11]"},
		{`{"class":"flaky","why":"TestY is a known flake"}`, 0, adapter.Flaky, "[]"},
		{`{"class":"infra"}`, 0, adapter.Infra, "[]"},
		{`{"class":"passed"}`, 0, adapter.Failed, "[]"},
		{`{"class":"deterministic","blamed":["11"]}`, 1, adapter.Failed, "[]"},
		{`not json`, 0, adapter.Failed, "[]"},
	}
	for _, tc := range cases {
		s, c := newServer(t)
		c.cfg.Blame = blameHook(t, tc.answer, tc.code)
		failed(s)
		rep, err := c.Status(ctx, "t1", "h", lines)
		if err != nil {
			t.Fatal(err)
		}
		blamed := "[]"
		if len(rep.Blamed) > 0 {
			blamed = "[" + string(rep.Blamed[0]) + "]"
		}
		if rep.Outcome != tc.want || blamed != tc.blamed {
			t.Errorf("%s exit %d: outcome %d blamed %v (%s), want %d %s", tc.answer, tc.code, rep.Outcome, rep.Blamed, rep.Evidence, tc.want, tc.blamed)
		}
	}
}

// TestAnEarlyFailureIsDecisiveOnlyIfDeterministic: a job that failed while
// the run goes on is reported at once only when blame finds it
// deterministic (docs/queue-as-bag.md §6). One a rerun could clear waits
// for the run to end.
func TestAnEarlyFailureIsDecisiveOnlyIfDeterministic(t *testing.T) {
	lines := []adapter.BuildLine{{ID: "11", Changes: []state.Change{{ID: "11", Head: "aaa"}}}}
	for _, tc := range []struct {
		answer string
		want   adapter.Outcome
	}{
		{`{"class":"deterministic","blamed":["11"],"evidence":"TestX in a.go"}`, adapter.Failed},
		{`{"class":"flaky","why":"TestY is a known flake"}`, adapter.Pending},
		{`{"class":"infra"}`, adapter.Pending},
	} {
		s, c := newServer(t)
		c.cfg.Blame = blameHook(t, tc.answer, 0)
		s.check("h", "test (1)", "completed", "failure", t0)
		s.check("h", "check", "in_progress", "", t0)
		rep, err := c.Status(ctx, "t1", "h", lines)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Outcome != tc.want || (tc.want == adapter.Failed && (!rep.Early || len(rep.Blamed) != 1)) {
			t.Errorf("%s: outcome %d early %v blamed %v (%s)", tc.answer, rep.Outcome, rep.Early, rep.Blamed, rep.Why)
		}
	}
}

// TestBlameHookReadsTheChangesDescriptions: each line comes to the blame
// hook with its changes' titles and descriptions, for a rule a description
// breaks (§5.6). A change that can't be read adds nothing.
func TestBlameHookReadsTheChangesDescriptions(t *testing.T) {
	s, c := newServer(t)
	s.pull(11, "open", "aaa", "feat-a", "main", "alice")
	s.pulls[11]["title"], s.pulls[11]["body"] = "Tidy the list", "This closes #42."
	s.check("h", "check", "completed", "failure", t0)
	got := filepath.Join(t.TempDir(), "in.json")
	p := filepath.Join(t.TempDir(), "blame.sh")
	os.WriteFile(p, []byte("#!/bin/sh\ncat > "+got+"\necho '{\"class\":\"deterministic\"}'\n"), 0o755)
	c.cfg.Blame = hook.Hook{Command: []string{p}, Timeout: 10 * time.Second}
	lines := []adapter.BuildLine{{ID: "11", Changes: []state.Change{{ID: "11", Head: "aaa"}}}, {ID: "12", Changes: []state.Change{{ID: "12", Head: "bbb"}}}}
	if _, err := c.Status(ctx, "t1", "h", lines); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	var in BlameInput
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Lines) != 2 || in.Lines[0].Text != "Tidy the list\n\nThis closes #42." || in.Lines[1].Text != "" {
		t.Fatalf("the hook read %s", b)
	}
}

func TestReadLandingReadsTheVerdictItself(t *testing.T) {
	s, c := newServer(t)
	s.pull(11, "open", "aaa", "feat-a", "main", "alice")
	s.refs["main"] = "trunk1"
	r, err := c.ReadLanding(ctx, "t1", "h", []state.ChangeID{"11"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != "" || r.Trunk != "trunk1" || r.Heads["11"] != "aaa" {
		t.Fatalf("no checks yet: %+v", r)
	}
	s.check("h", "check", "completed", "success", t0)
	s.check("other", "check", "completed", "success", t0)
	if r, _ = c.ReadLanding(ctx, "t1", "h", []state.ChangeID{"11"}); r.Verdict != "h" {
		t.Fatalf("passed: %+v", r)
	}
	// A check that names another head is not a verdict on this one.
	s.checks["h2"] = append(s.checks["h2"], s.checks["other"]...)
	if r, _ = c.ReadLanding(ctx, "t2", "h2", nil); r.Verdict != "" {
		t.Fatalf("a check on another head counted: %+v", r)
	}
}

func TestRerunAndCancel(t *testing.T) {
	s, c := newServer(t)
	s.run(1, "h", "shunter/t1", "completed", "failure")
	s.run(2, "h", "shunter/t1", "completed", "success")
	if err := c.Rerun(ctx, "k1", "t1", "h"); err != nil {
		t.Fatal(err)
	}
	if s.count("POST", "/actions/runs/1/rerun-failed-jobs") != 1 || s.count("POST", "/actions/runs/2/") != 0 {
		t.Fatalf("reran %v", s.log)
	}
	// Now run 1 is queued again: a second rerun is the same rerun.
	if err := c.Rerun(ctx, "k1", "t1", "h"); err != nil || s.count("POST", "/actions/runs/1/rerun-failed-jobs") != 1 {
		t.Fatalf("a rerun asked twice: %v", err)
	}
	if err := c.Rerun(ctx, "k2", "t1", "nothing-here"); !adapter.IsPermanent(err) {
		t.Fatalf("nothing to rerun: %v", err)
	}
	if err := c.Cancel(ctx, "k3", "t1"); err != nil {
		t.Fatal(err)
	}
	if s.count("POST", "/actions/runs/1/cancel") != 1 || s.count("POST", "/actions/runs/2/cancel") != 0 {
		t.Fatalf("cancelled %v", s.log)
	}
	if err := c.Cancel(ctx, "k3", "t1"); err != nil {
		t.Fatalf("cancelling nothing: %v", err)
	}
}

type told struct{ n int }

func (t *told) Owner(context.Context, string, state.LineID, []state.ChangeID, string) error {
	t.n++
	return nil
}
func (t *told) Judge(context.Context, string, string, state.TrainID, string) error { return nil }

func TestLandedClosesAndLabelsEachChange(t *testing.T) {
	s, c := newServer(t)
	s.pull(11, "open", "aaa", "feat-a", "main", "alice")
	s.pull(12, "open", "bbb", "feat-b", "feat-a", "alice")
	next := &told{}
	n := Landed{Client: c, Next: next}
	if err := n.Owner(ctx, "k1", "11", []state.ChangeID{"11", "12"}, "returned: conflict"); err != nil || s.pulls[11]["state"] != "open" {
		t.Fatalf("a return closed the change: %v", err)
	}
	if err := n.Owner(ctx, "k2", "11", []state.ChangeID{"11", "12"}, "landed: in train t1"); err != nil {
		t.Fatal(err)
	}
	info, _ := c.Describe(ctx, []state.ChangeID{"11", "12"})
	if !info[0].Landed || !info[1].Landed || info[0].Open {
		t.Fatalf("after landing: %+v", info)
	}
	if err := n.Owner(ctx, "k2", "11", []state.ChangeID{"11", "12"}, "landed: in train t1"); err != nil || s.count("PATCH", "/pulls/") != 2 {
		t.Fatalf("again: %v, %d closes", err, s.count("PATCH", "/pulls/"))
	}
	if next.n != 3 {
		t.Fatalf("told the owner %d times", next.n)
	}
}

func TestPruneRetargetsThenDeletesEndedTrainsBranches(t *testing.T) {
	s, c := newServer(t)
	s.refs["main"] = "m1"
	s.refs["shunter/t1"] = "h1" // ended (landed): train 2's pull request is based on it
	s.refs["shunter/t2"] = "h2" // live
	s.refs["shunter/t3"] = "h3" // ended, but its pull request is still open: not yet
	s.refs["feature"] = "f1"    // not Shunter's
	s.pull(601, "closed", "h1", "shunter/t1", "main", "shunter-bot")
	s.pull(602, "open", "h2", "shunter/t2", "shunter/t1", "shunter-bot")
	s.pull(603, "open", "h3", "shunter/t3", "main", "shunter-bot")
	if err := c.Prune(ctx, []state.TrainID{"t2"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.refs["shunter/t1"]; ok {
		t.Fatal("the ended train's branch was kept")
	}
	if got := s.pulls[602]["base"].(map[string]any)["ref"]; got != "main" {
		t.Fatalf("train 2's pull request is based on %v, not retargeted to trunk before its base went", got)
	}
	for _, b := range []string{"shunter/t2", "shunter/t3", "feature", "main"} {
		if _, ok := s.refs[b]; !ok {
			t.Fatalf("%s was deleted", b)
		}
	}
	// Again, and with the branch already gone: no error, nothing more.
	if err := c.Prune(ctx, []state.TrainID{"t2"}); err != nil {
		t.Fatal(err)
	}
	if n := s.count("DELETE", "/git/refs/"); n != 1 {
		t.Fatalf("%d deletes", n)
	}
	// Read-only (shadow running) refuses the writes before they are sent.
	s.refs["shunter/t9"] = "h9"
	c.cfg.ReadOnly = true
	if err := c.Prune(ctx, nil); err == nil {
		t.Fatal("a read-only prune deleted")
	}
	if _, ok := s.refs["shunter/t9"]; !ok {
		t.Fatal("a read-only prune deleted")
	}
}

func TestRetargetAStackedChangeWhoseBaseLanded(t *testing.T) {
	s, c := newServer(t)
	s.pull(21, "open", "aaa", "feat-a", "main", "alice")
	s.pull(22, "open", "bbb", "feat-b", "feat-a", "alice")
	s.pull(23, "open", "ccc", "feat-c", "feat-a", "alice")
	n := Landed{Client: c, Next: &told{}}
	// 21 lands alone: 22 and 23 are stacked on it and move to trunk first.
	if err := n.Owner(ctx, "k", "21", []state.ChangeID{"21"}, "landed: in train t1"); err != nil {
		t.Fatal(err)
	}
	for _, x := range []int{22, 23} {
		if got := s.pulls[x]["base"].(map[string]any)["ref"]; got != "main" {
			t.Fatalf("#%d is based on %v", x, got)
		}
	}
	// Retarget by itself: idempotent, and a change on trunk is left alone.
	s.pull(24, "open", "ddd", "feat-d", "feat-x", "alice")
	if err := c.Retarget(ctx, "24"); err != nil || s.pulls[24]["base"].(map[string]any)["ref"] != "main" {
		t.Fatalf("retarget: %v", err)
	}
	before := s.count("PATCH", "/pulls/24")
	if err := c.Retarget(ctx, "24"); err != nil || s.count("PATCH", "/pulls/24") != before {
		t.Fatalf("a second retarget patched again: %v", err)
	}
}

// TestReblameForcesTheClass: asked again after a flake's one rerun failed,
// the hook is told the class, and only a deterministic answer names lines.
func TestReblameForcesTheClass(t *testing.T) {
	lines := []adapter.BuildLine{{ID: "11", Changes: []state.Change{{ID: "11", Head: "aaa"}}}, {ID: "12"}}
	rep := adapter.Report{Outcome: adapter.Flaky, Head: "h", Evidence: "failed again after its one rerun: only timeouts"}
	cases := []struct {
		answer string
		blamed string
	}{
		{`{"class":"deterministic","blamed":["11"],"evidence":"TestSlow in a.go"}`, "[11]"},
		{`{"class":"flaky","why":"still a flake"}`, "[]"},
		{`{"class":"no-verdict"}`, "[]"},
	}
	for _, tc := range cases {
		_, c := newServer(t)
		got := filepath.Join(t.TempDir(), "in.json")
		p := filepath.Join(t.TempDir(), "blame.sh")
		os.WriteFile(p, []byte("#!/bin/sh\ncat > "+got+"\necho '"+tc.answer+"'\n"), 0o755)
		c.cfg.Blame = hook.Hook{Command: []string{p}, Timeout: 10 * time.Second}
		out, err := c.Reblame(ctx, "t1", rep, lines)
		if err != nil {
			t.Fatal(err)
		}
		blamed := "[]"
		if len(out.Blamed) > 0 {
			blamed = "[" + string(out.Blamed[0]) + "]"
		}
		if out.Outcome != adapter.Failed || blamed != tc.blamed {
			t.Errorf("%s: outcome %d blamed %v (%s), want failed %s", tc.answer, out.Outcome, out.Blamed, out.Evidence, tc.blamed)
		}
		b, _ := os.ReadFile(got)
		var in BlameInput
		if json.Unmarshal(b, &in) != nil || in.ForceClass != "deterministic" {
			t.Errorf("the hook read %s", b)
		}
	}
}

// TestFilesIsWholeOrAnError: the file list the tier classifier reads is every
// path a pull request touches, a rename's old name included, over as many
// pages as it takes. A list that does not add up to the number of files the
// pull request reports is an error, never a shorter list: the classifier
// must not call a change inert on files it did not see.
func TestFilesIsWholeOrAnError(t *testing.T) {
	s, c := newServer(t)
	s.pull(31, "open", "aaa", "feat-a", "main", "alice")
	var fs []map[string]any
	for i := 0; i < 230; i++ {
		fs = append(fs, map[string]any{"filename": fmt.Sprintf("docs/%03d.md", i)})
	}
	fs = append(fs, map[string]any{"filename": "docs/new.md", "previous_filename": "pkg/old.go"})
	s.pulls[31]["files"], s.pulls[31]["changed_files"] = fs, len(fs)
	got, err := c.Files(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 232 || !slices.Contains(got, "pkg/old.go") || !slices.Contains(got, "docs/229.md") {
		t.Fatalf("%d paths; the rename's old name present: %v", len(got), slices.Contains(got, "pkg/old.go"))
	}
	// The pull request reports more files than the listing gives.
	s.pulls[31]["changed_files"] = len(fs) + 1
	if _, err := c.Files(ctx, "31"); err == nil {
		t.Fatal("a short list was given as whole")
	}
	// A pull request that reports no count is not listed either.
	s.pull(32, "open", "bbb", "feat-b", "main", "alice")
	s.pulls[32]["files"] = []map[string]any{{"filename": "docs/a.md"}}
	if _, err := c.Files(ctx, "32"); err == nil {
		t.Fatal("a list with no count to check it against was given as whole")
	}
}
