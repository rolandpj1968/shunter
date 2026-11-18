package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
)

type ghCheckRun struct {
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  *string    `json:"conclusion"`
	StartedAt   *time.Time `json:"started_at"`
	DetailsURL  string     `json:"details_url"`
	HTMLURL     string     `json:"html_url"`
	HeadSHA     string     `json:"head_sha"`
	CompletedAt *time.Time `json:"completed_at"`
	CheckSuite  struct {
		ID int64 `json:"id"`
	} `json:"check_suite"`
	Output struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

// checks reads the check runs on head: the latest of each name, by its
// started time. A rerun is a new run of the same name, started later. With
// a Workflow configured, only the check suites of that workflow's runs on
// the head are read: another workflow on the same commit (a nightly run on
// a head that has landed, say) is not this train's CI.
func (c *Client) checks(ctx context.Context, head state.Head) ([]adapter.Check, error) {
	var page struct {
		Total int          `json:"total_count"`
		Runs  []ghCheckRun `json:"check_runs"`
	}
	if head == "" {
		return nil, adapter.Permanent(fmt.Errorf("no head to read checks on"))
	}
	q := url.Values{"per_page": {"100"}, "filter": {"latest"}}
	if err := c.do(ctx, http.MethodGet, c.repo("/commits/%s/check-runs?%s", url.PathEscape(string(head)), q.Encode()), nil, &page); err != nil {
		return nil, err
	}
	suites, err := c.workflowSuites(ctx, head)
	if err != nil {
		return nil, err
	}
	latest := map[string]adapter.Check{}
	for _, r := range page.Runs {
		if r.HeadSHA != "" && r.HeadSHA != string(head) {
			continue
		}
		if suites != nil && !suites[r.CheckSuite.ID] {
			continue
		}
		ch := adapter.Check{Name: r.Name, Status: r.Status, URL: r.HTMLURL, Summary: strings.TrimSpace(r.Output.Title), Suite: r.CheckSuite.ID}
		if ch.URL == "" {
			ch.URL = r.DetailsURL
		}
		if r.Conclusion != nil {
			ch.Conclusion = *r.Conclusion
		}
		if r.StartedAt != nil {
			ch.Started = *r.StartedAt
		}
		if old, ok := latest[r.Name]; !ok || ch.Started.After(old.Started) {
			latest[r.Name] = ch
		}
	}
	out := make([]adapter.Check, 0, len(latest))
	for _, ch := range latest {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// workflowSuites is the check suites of the configured workflow's runs on
// head, or nil (read every suite) when no workflow is configured or its runs
// carry no suite. No run of the workflow on the head is an empty set: its
// checks have not started.
func (c *Client) workflowSuites(ctx context.Context, head state.Head) (map[int64]bool, error) {
	if c.cfg.Workflow == "" {
		return nil, nil
	}
	rs, err := c.runs(ctx, url.Values{"head_sha": {string(head)}})
	if err != nil {
		return nil, err
	}
	out := map[int64]bool{}
	unknown := false
	for _, r := range rs {
		if r.HeadSHA != "" && r.HeadSHA != string(head) {
			continue
		}
		if r.CheckSuiteID == 0 {
			unknown = true
			continue
		}
		out[r.CheckSuiteID] = true
	}
	if unknown && len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// scope is the checks that make a verdict's evidence: with required names,
// those in the check suites of the required checks, so that another
// workflow's checks on the same commit count for nothing; with none, all.
// A required check with no suite known leaves the scope at all.
func scope(checks, req []adapter.Check, names []string) []adapter.Check {
	if len(names) == 0 {
		return checks
	}
	suites := map[int64]bool{}
	for _, ch := range req {
		if ch.Suite == 0 {
			return checks
		}
		suites[ch.Suite] = true
	}
	var out []adapter.Check
	for _, ch := range checks {
		if suites[ch.Suite] {
			out = append(out, ch)
		}
	}
	return out
}

// required picks the checks that make the verdict, and names any missing.
func required(checks []adapter.Check, names []string) (req []adapter.Check, missing []string) {
	if len(names) == 0 {
		return checks, nil
	}
	for _, n := range names {
		i := slices.IndexFunc(checks, func(ch adapter.Check) bool { return ch.Name == n })
		if i < 0 {
			missing = append(missing, n)
			continue
		}
		req = append(req, checks[i])
	}
	return req, missing
}

// passed is the verdict: every required check ran on the head, completed
// and succeeded. No checks at all is no verdict.
func passed(checks []adapter.Check, names []string) bool {
	req, missing := required(checks, names)
	if len(missing) > 0 || len(req) == 0 {
		return false
	}
	for _, ch := range req {
		if ch.Status != "completed" || ch.Conclusion != "success" {
			return false
		}
	}
	return true
}

// noVerdict are conclusions that judge nothing (§5.5 class 0).
var noVerdict = map[string]bool{"cancelled": true, "skipped": true, "stale": true, "neutral": true, "action_required": true, "startup_failure": true}

// Classify turns check runs into an outcome, before blame: §5.5's order. A
// failed check is a failure (class 4, until blame says otherwise); only
// timeouts are timing (class 3); a cancelled or skipped required check with
// nothing failed is no verdict (class 0). Until every required check has
// completed, the run is pending: no fail-fast, so that blame sees every
// failure. Only the checks in the required checks' suites count (scope):
// a failure in another workflow on the same head is not this run's.
func Classify(head state.Head, checks []adapter.Check, names []string) adapter.Report {
	rep := adapter.Report{Head: head, Checks: checks}
	req, missing := required(checks, names)
	if len(missing) > 0 || len(req) == 0 {
		rep.Outcome = adapter.Pending
		if len(missing) > 0 {
			rep.Why = "waiting for " + strings.Join(missing, ", ")
		}
		return rep
	}
	for _, ch := range req {
		if ch.Status != "completed" {
			rep.Outcome = adapter.Pending
			rep.Why = ch.Name + " is " + ch.Status
			return rep
		}
	}
	rep.Checks = scope(checks, req, names)
	var failed, timed, none []string
	for _, ch := range rep.Checks {
		if ch.Status != "completed" {
			continue
		}
		switch {
		case ch.Conclusion == "failure":
			failed = append(failed, describe(ch))
		case ch.Conclusion == "timed_out":
			timed = append(timed, ch.Name)
		}
	}
	for _, ch := range req {
		if noVerdict[ch.Conclusion] {
			none = append(none, ch.Name+" "+ch.Conclusion)
		}
	}
	switch {
	case len(failed) > 0:
		rep.Outcome = adapter.Failed
		rep.Evidence = "failed: " + strings.Join(failed, "; ")
	case len(timed) > 0:
		rep.Outcome = adapter.Flaky
		rep.Why = "only timeouts: " + strings.Join(timed, ", ")
	case len(none) > 0:
		rep.Outcome = adapter.NoVerdict
		rep.Why = "no verdict: " + strings.Join(none, ", ")
	default:
		rep.Outcome = adapter.Passed
	}
	return rep
}

func describe(ch adapter.Check) string {
	if ch.Summary != "" {
		return ch.Name + " (" + ch.Summary + ")"
	}
	return ch.Name
}

// BlameInput is what the blame hook reads.
type BlameInput struct {
	Train    state.TrainID   `json:"train"`
	Head     state.Head      `json:"head"`
	Lines    []BlameLine     `json:"lines"`
	Checks   []adapter.Check `json:"checks"`
	Evidence string          `json:"evidence"`
}

// BlameLine is one line of the train, for the blame hook.
type BlameLine struct {
	ID      state.LineID   `json:"id"`
	Changes []state.Change `json:"changes"`
}

// BlameAnswer is what the blame hook answers. Class is one of
// "deterministic", "flaky", "infra" or "no-verdict". It can only say what a
// failure is: no answer makes a failed run pass.
type BlameAnswer struct {
	Class    string         `json:"class"`
	Blamed   []state.LineID `json:"blamed,omitempty"`
	Evidence string         `json:"evidence,omitempty"`
	Why      string         `json:"why,omitempty"`
}

// blame asks the hook what a failure is. A hook that fails, or answers
// what it may not, leaves the failure unblamed: J1 decides (I6).
func (c *Client) blame(ctx context.Context, t state.TrainID, rep adapter.Report, lines []adapter.BuildLine) adapter.Report {
	if !c.cfg.Blame.Configured() {
		return rep
	}
	in := BlameInput{Train: t, Head: rep.Head, Checks: rep.Checks, Evidence: rep.Evidence}
	member := map[state.LineID]bool{}
	for _, l := range lines {
		in.Lines = append(in.Lines, BlameLine{ID: l.ID, Changes: l.Changes})
		member[l.ID] = true
	}
	var a BlameAnswer
	res, err := c.cfg.Blame.Run(ctx, in, &a)
	if err != nil || res.Code != 0 {
		rep.Evidence += fmt.Sprintf(" [the blame hook gave no answer: exit %d %v]", res.Code, err)
		return rep
	}
	if a.Evidence != "" {
		rep.Evidence = a.Evidence
	}
	switch a.Class {
	case "flaky":
		rep.Outcome, rep.Why = adapter.Flaky, orDefault(a.Why, "a known flake, by the blame hook")
	case "infra":
		rep.Outcome, rep.Why = adapter.Infra, orDefault(a.Why, "the infrastructure failed, by the blame hook")
	case "no-verdict":
		rep.Outcome, rep.Why = adapter.NoVerdict, orDefault(a.Why, "no verdict, by the blame hook")
	case "deterministic":
		for _, id := range a.Blamed {
			if member[id] {
				rep.Blamed = append(rep.Blamed, id)
			}
		}
	default:
		rep.Evidence += fmt.Sprintf(" [the blame hook answered class %q, which is not one it may give]", a.Class)
	}
	return rep
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Status implements adapter.CI.
func (c *Client) Status(ctx context.Context, t state.TrainID, head state.Head, lines []adapter.BuildLine) (adapter.Report, error) {
	checks, err := c.checks(ctx, head)
	if err != nil {
		return adapter.Report{}, err
	}
	rep := Classify(head, checks, c.cfg.Checks)
	if rep.Outcome == adapter.Failed {
		rep = c.blame(ctx, t, rep, lines)
	}
	return rep, nil
}

type ghRun struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	WorkflowID int64   `json:"workflow_id"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
	HeadSHA    string  `json:"head_sha"`
	HeadBranch string  `json:"head_branch"`
	// CheckSuiteID is the check suite of the run's checks.
	CheckSuiteID int64 `json:"check_suite_id"`
}

// runs lists the workflow runs that match q, limited to Workflow when it is
// set.
func (c *Client) runs(ctx context.Context, q url.Values) ([]ghRun, error) {
	var page struct {
		Runs []ghRun `json:"workflow_runs"`
	}
	q.Set("per_page", "50")
	path := c.repo("/actions/runs?%s", q.Encode())
	if c.cfg.Workflow != "" {
		path = c.repo("/actions/workflows/%s/runs?%s", url.PathEscape(c.cfg.Workflow), q.Encode())
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil, err
	}
	return page.Runs, nil
}

// Rerun implements adapter.CI: the failed jobs of every completed run on the
// head that did not succeed. A run on the head that is still going is taken
// as this rerun (a rerun asked for twice, or started by hand). Nothing to
// rerun is a permanent refusal.
func (c *Client) Rerun(ctx context.Context, key string, t state.TrainID, head state.Head) error {
	rs, err := c.runs(ctx, url.Values{"head_sha": {string(head)}})
	if err != nil {
		return err
	}
	var again []int64
	for _, r := range rs {
		if r.HeadSHA != "" && r.HeadSHA != string(head) {
			continue
		}
		if r.Status != "completed" {
			return nil
		}
		if r.Conclusion != nil && *r.Conclusion != "success" && *r.Conclusion != "skipped" {
			again = append(again, r.ID)
		}
	}
	if len(again) == 0 {
		return adapter.Permanent(fmt.Errorf("train %s: no completed, unsuccessful run on %s to rerun", t, head))
	}
	for _, id := range again {
		if err := c.do(ctx, http.MethodPost, c.repo("/actions/runs/%d/rerun-failed-jobs", id), nil, nil); err != nil {
			if isStatus(err, http.StatusForbidden) || isStatus(err, http.StatusConflict) {
				// Refused because it runs again already: look once more.
				if more, lerr := c.runs(ctx, url.Values{"head_sha": {string(head)}}); lerr == nil {
					for _, r := range more {
						if r.ID == id && r.Status != "completed" {
							goto next
						}
					}
				}
			}
			return err
		}
	next:
	}
	return nil
}

// Cancel implements adapter.CI: every run on the train's branch that has
// not completed. A run that completed in the meantime is no error. A
// cancelled run is no verdict, never a failure (§5.5 class 0).
func (c *Client) Cancel(ctx context.Context, key string, t state.TrainID) error {
	rs, err := c.runs(ctx, url.Values{"branch": {c.Branch(t)}})
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Status == "completed" || (r.HeadBranch != "" && r.HeadBranch != c.Branch(t)) {
			continue
		}
		if err := c.do(ctx, http.MethodPost, c.repo("/actions/runs/%d/cancel", r.ID), nil, nil); err != nil && !isStatus(err, http.StatusConflict) {
			return err
		}
	}
	return nil
}

// dispatch starts the workflow on the train's branch, in dispatch mode,
// unless a run of it is already on the head.
func (c *Client) dispatch(ctx context.Context, branch string, head state.Head) error {
	if !c.cfg.Dispatch {
		return nil
	}
	if c.cfg.Workflow == "" {
		return adapter.Permanent(fmt.Errorf("dispatch: no workflow configured"))
	}
	rs, err := c.runs(ctx, url.Values{"head_sha": {string(head)}})
	if err != nil {
		return err
	}
	if len(rs) > 0 {
		return nil
	}
	return c.do(ctx, http.MethodPost, c.repo("/actions/workflows/%s/dispatches", url.PathEscape(c.cfg.Workflow)), map[string]string{"ref": branch}, nil)
}
