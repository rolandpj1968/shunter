package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
)

type ghRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type ghPull struct {
	Number int    `json:"number"`
	NodeID string `json:"node_id"`
	Draft  bool   `json:"draft"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	// MergedAt is set on a merged pull request in a list, where "merged"
	// is absent.
	MergedAt *string `json:"merged_at"`
	Head     ghRef   `json:"head"`
	Base     ghRef   `json:"base"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Title string `json:"title"`
	Body  string `json:"body"`
	// ChangedFiles is the number of files the pull request touches, on a
	// single pull request (absent in a list).
	ChangedFiles int `json:"changed_files"`
}

type ghFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
}

// maxFilePages bounds a file listing: the forge serves at most 3,000 files
// of a pull request, 100 a page.
const maxFilePages = 30

// Files implements adapter.Filer: every path the change touches, a rename's
// old name included. The list is whole or it is an error: it is counted
// against the number of files the pull request itself reports, and a pull
// request too large to list in full is refused.
func (c *Client) Files(ctx context.Context, id state.ChangeID) ([]string, error) {
	n, err := number(id)
	if err != nil {
		return nil, err
	}
	p, err := c.pull(ctx, n)
	if err != nil {
		return nil, err
	}
	var out []string
	listed := 0
	for page := 1; page <= maxFilePages; page++ {
		var fs []ghFile
		if err := c.do(ctx, http.MethodGet, c.repo("/pulls/%d/files?per_page=100&page=%d", n, page), nil, &fs); err != nil {
			return nil, err
		}
		for _, f := range fs {
			listed++
			out = append(out, f.Filename)
			if f.PreviousFilename != "" {
				out = append(out, f.PreviousFilename)
			}
		}
		if len(fs) < 100 {
			break
		}
	}
	if p.ChangedFiles == 0 || listed != p.ChangedFiles {
		return nil, fmt.Errorf("change %s: %d files listed of the %d it touches: the list is not whole", id, listed, p.ChangedFiles)
	}
	return out, nil
}

func (p ghPull) labelled(name string) bool {
	if name == "" {
		return false
	}
	for _, l := range p.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

type ghGitRef struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

// Branch is the branch of a train.
func (c *Client) Branch(t state.TrainID) string { return c.cfg.BranchPrefix + string(t) }

func number(id state.ChangeID) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(string(id), "#"))
	if err != nil || n <= 0 {
		return 0, adapter.Permanent(fmt.Errorf("change %q is not a pull request number", id))
	}
	return n, nil
}

func (c *Client) pull(ctx context.Context, n int) (ghPull, error) {
	var p ghPull
	err := c.do(ctx, http.MethodGet, c.repo("/pulls/%d", n), nil, &p)
	if err == nil && p.Number != n {
		err = fmt.Errorf("asked for pull request %d, the forge answered %d", n, p.Number)
	}
	return p, err
}

// pullsFrom lists the pull requests from a branch of this repository.
func (c *Client) pullsFrom(ctx context.Context, branch, st string) ([]ghPull, error) {
	var ps []ghPull
	q := url.Values{"head": {c.cfg.Owner + ":" + branch}, "state": {st}, "per_page": {"30"}}
	err := c.do(ctx, http.MethodGet, c.repo("/pulls?%s", q.Encode()), nil, &ps)
	return ps, err
}

func (c *Client) branchHead(ctx context.Context, branch string) (state.Head, error) {
	var r ghGitRef
	if err := c.do(ctx, http.MethodGet, c.repo("/git/ref/heads/%s", pathRef(branch)), nil, &r); err != nil {
		return "", err
	}
	if r.Object.SHA == "" {
		return "", fmt.Errorf("branch %s: no sha in the answer", branch)
	}
	return state.Head(r.Object.SHA), nil
}

// pathRef escapes each segment of a branch name for a URL path.
func pathRef(branch string) string {
	parts := strings.Split(branch, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// approved is the gate's producer rule: labelled ApprovedLabel, or listed
// in ApprovedFile, whoever wrote it; else an author with a branch rule only
// from a branch the rule matches; else an author in Authors, or any author
// when Authors is empty.
func (c *Client) approved(p ghPull, listed map[int]bool) bool {
	if p.labelled(c.cfg.ApprovedLabel) || listed[p.Number] {
		return true
	}
	if re, ok := c.branchRule[p.User.Login]; ok {
		return re.MatchString(p.Head.Ref)
	}
	return len(c.cfg.Authors) == 0 || slices.Contains(c.cfg.Authors, p.User.Login)
}

// approvedList reads ApprovedFile: the changes it lists. A missing file, or
// none configured, lists nothing; one that can't be read is a transient
// failure, so the gate asks again rather than refuse.
func (c *Client) approvedList() (map[int]bool, error) {
	out := map[int]bool{}
	if c.cfg.ApprovedFile == "" {
		return out, nil
	}
	b, err := os.ReadFile(c.cfg.ApprovedFile)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the approved file: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		for _, w := range strings.Fields(line) {
			n, err := strconv.Atoi(strings.TrimPrefix(w, "#"))
			if err != nil || n <= 0 {
				break
			}
			out[n] = true
		}
	}
	return out, nil
}

// Describe implements adapter.Forge. A change stacked on a branch that is
// not trunk names as its base the pull request from that branch (an open one
// first); a base branch with no pull request is named "branch:<name>", which
// the gate cannot find, and so refuses.
func (c *Client) Describe(ctx context.Context, ids []state.ChangeID) ([]adapter.ChangeInfo, error) {
	listed, err := c.approvedList()
	if err != nil {
		return nil, err
	}
	out := make([]adapter.ChangeInfo, 0, len(ids))
	for _, id := range ids {
		n, err := number(id)
		if err != nil {
			return nil, err
		}
		p, err := c.pull(ctx, n)
		if err != nil {
			if IsNotFound(err) {
				return nil, adapter.Permanent(fmt.Errorf("change %s not found", id))
			}
			return nil, err
		}
		if p.Head.SHA == "" {
			return nil, fmt.Errorf("change %s: the forge gave no head", id)
		}
		info := adapter.ChangeInfo{
			ID:       id,
			Head:     state.Head(p.Head.SHA),
			Open:     p.State == "open",
			Approved: c.approved(p, listed),
			Landed:   p.Merged || p.MergedAt != nil || (p.State == "closed" && p.labelled(c.cfg.LandedLabel)),
		}
		if p.Base.Ref != c.cfg.Trunk {
			base, err := c.changeOn(ctx, p.Base.Ref)
			if err != nil {
				return nil, err
			}
			info.Base = base
		}
		out = append(out, info)
	}
	return out, nil
}

// changeOn names the change whose branch is branch.
func (c *Client) changeOn(ctx context.Context, branch string) (state.ChangeID, error) {
	ps, err := c.pullsFrom(ctx, branch, "all")
	if err != nil {
		return "", err
	}
	best := -1
	for i, p := range ps {
		if best < 0 || (p.State == "open" && ps[best].State != "open") {
			best = i
		}
	}
	if best < 0 {
		return state.ChangeID("branch:" + branch), nil
	}
	return state.ChangeID(strconv.Itoa(ps[best].Number)), nil
}

// keyMark is how a train's pull request carries the key that opened it.
func keyMark(key string) string { return "<!-- shunter-key: " + key + " -->" }

// OpenPR implements adapter.Forge. The train's branch must already be at
// head (the git adapter pushes it). The pull request goes to trunk when base
// is trunk's tip, and otherwise to the train branch whose tip is base: the
// train this one is built on. An open pull request from the train's branch
// on the same head is the same pull request; so is one carrying the key.
func (c *Client) OpenPR(ctx context.Context, key string, t state.TrainID, changes []state.ChangeID, head, base state.Head) (string, error) {
	return c.open(ctx, key, t, changes, head, base, false)
}

// OpenDraft implements adapter.Drafter: the pull request opens as a draft,
// and no CI is dispatched. A workflow run by pull request events should
// skip drafts and run when one is marked ready.
func (c *Client) OpenDraft(ctx context.Context, key string, t state.TrainID, changes []state.ChangeID, head, base state.Head) (string, error) {
	return c.open(ctx, key, t, changes, head, base, true)
}

// Promote implements adapter.Drafter: the draft is marked ready for review,
// and CI is dispatched on head.
func (c *Client) Promote(ctx context.Context, key string, t state.TrainID, pr string, head state.Head) error {
	n, err := strconv.Atoi(pr)
	if err != nil {
		return adapter.Permanent(fmt.Errorf("train %s: pull request %q", t, pr))
	}
	p, err := c.pull(ctx, n)
	if err != nil {
		return err
	}
	if p.State != "open" {
		return adapter.Permanent(fmt.Errorf("train %s: pull request %d is %s", t, n, p.State))
	}
	if state.Head(p.Head.SHA) != head {
		return fmt.Errorf("train %s: pull request %d is at %s, not yet at the built head %s", t, n, p.Head.SHA, head)
	}
	if p.Draft {
		in := map[string]any{
			"query":     "mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } } }",
			"variables": map[string]string{"id": p.NodeID},
		}
		var out struct {
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := c.do(ctx, http.MethodPost, "/graphql", in, &out); err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			return fmt.Errorf("train %s: marking pull request %d ready: %s", t, n, out.Errors[0].Message)
		}
	}
	return c.dispatch(ctx, c.Branch(t), head)
}

// Retitle implements adapter.Drafter: the draft's title and body name
// changes, as OpenDraft names them. The body keeps the key's mark it
// carries, byte for byte; one with no mark is given none. A draft that
// already says so is left alone.
func (c *Client) Retitle(ctx context.Context, t state.TrainID, pr string, changes []state.ChangeID, head, base state.Head) error {
	n, err := strconv.Atoi(pr)
	if err != nil {
		return adapter.Permanent(fmt.Errorf("train %s: pull request %q", t, pr))
	}
	p, err := c.pull(ctx, n)
	if err != nil {
		if IsNotFound(err) {
			return adapter.Permanent(fmt.Errorf("train %s: pull request %d not found", t, n))
		}
		return err
	}
	if p.State != "open" {
		return adapter.Permanent(fmt.Errorf("train %s: pull request %d is %s", t, n, p.State))
	}
	title := trainTitle(t, changes)
	body := trainBody("", t, changes, head, base)
	if key, ok := markedKey(p.Body); ok {
		body = trainBody(key, t, changes, head, base)
	} else {
		body = strings.TrimSuffix(body, keyMark("")+"\n")
	}
	if p.Title == title && p.Body == body {
		return nil
	}
	return c.do(ctx, http.MethodPatch, c.repo("/pulls/%d", n), map[string]string{"title": title, "body": body}, nil)
}

// markedKey reads the key a pull request's body carries the mark of.
func markedKey(body string) (string, bool) {
	const open, end = "<!-- shunter-key: ", " -->"
	i := strings.Index(body, open)
	if i < 0 {
		return "", false
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, end)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// open opens the train's pull request, a draft or not. A draft is the
// same pull request whatever head its branch has moved to.
func (c *Client) open(ctx context.Context, key string, t state.TrainID, changes []state.ChangeID, head, base state.Head, draft bool) (string, error) {
	branch := c.Branch(t)
	start := func() error {
		if draft {
			return nil
		}
		return c.dispatch(ctx, branch, head)
	}
	open, err := c.pullsFrom(ctx, branch, "open")
	if err != nil {
		return "", err
	}
	for _, p := range open {
		if state.Head(p.Head.SHA) == head || strings.Contains(p.Body, keyMark(key)) || (draft && p.Draft) {
			return strconv.Itoa(p.Number), start()
		}
		return "", adapter.Permanent(fmt.Errorf("train %s: pull request %d is open on %s, not on %s", t, p.Number, p.Head.SHA, head))
	}
	got, err := c.branchHead(ctx, branch)
	if err != nil {
		if IsNotFound(err) {
			return "", fmt.Errorf("train %s: its branch %s is not on the forge yet", t, branch)
		}
		return "", err
	}
	if got != head {
		return "", adapter.Permanent(fmt.Errorf("train %s: its branch %s is at %s, not at the built head %s", t, branch, got, head))
	}
	baseBranch, err := c.baseBranch(ctx, base)
	if err != nil {
		return "", err
	}
	in := map[string]any{"title": trainTitle(t, changes), "head": branch, "base": baseBranch, "body": trainBody(key, t, changes, head, base), "draft": draft}
	var p ghPull
	if err := c.do(ctx, http.MethodPost, c.repo("/pulls"), in, &p); err != nil {
		if isStatus(err, http.StatusUnprocessableEntity) {
			// Perhaps it exists already: a try whose answer was lost.
			if again, lerr := c.pullsFrom(ctx, branch, "open"); lerr == nil {
				for _, q := range again {
					if state.Head(q.Head.SHA) == head {
						return strconv.Itoa(q.Number), start()
					}
				}
			}
		}
		return "", err
	}
	if p.Number == 0 {
		return "", fmt.Errorf("train %s: the forge opened no pull request number", t)
	}
	return strconv.Itoa(p.Number), start()
}

// titleMax is the most characters a train's pull request title holds.
const titleMax = 72

// trainTitle names the train and the changes it carries, as pull request
// numbers in ride order: "Merge train t8: #12 #15". When they do not all
// fit in titleMax, it names as many as fit, then "+N more".
func trainTitle(t state.TrainID, changes []state.ChangeID) string {
	title := fmt.Sprintf("Merge train %s", t)
	if len(changes) == 0 {
		return title
	}
	title += ":"
	for k := len(changes); k >= 0; k-- {
		s := title
		for _, id := range changes[:k] {
			s += " #" + string(id)
		}
		if k < len(changes) {
			s += fmt.Sprintf(" +%d more", len(changes)-k)
		}
		if len(s) <= titleMax || k == 0 {
			return s
		}
	}
	return title
}

// trainBody lists the train's changes, one to a line so the forge links
// each, then says what the train is and carries the key's mark.
func trainBody(key string, t state.TrainID, changes []state.ChangeID, head, base state.Head) string {
	var b strings.Builder
	if len(changes) > 0 {
		b.WriteString("Changes, in ride order:\n\n")
		for _, id := range changes {
			fmt.Fprintf(&b, "- #%s\n", id)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "A merge train, assembled by Shunter. It lands when CI passes on exactly this head.\n\nTrain `%s`, head `%s`, built on `%s`.\n\n%s\n", t, head, base, keyMark(key))
	return b.String()
}

// baseBranch names the branch whose tip is base: trunk, or a train's.
func (c *Client) baseBranch(ctx context.Context, base state.Head) (string, error) {
	trunk, err := c.branchHead(ctx, c.cfg.Trunk)
	if err != nil {
		return "", err
	}
	if trunk == base {
		return c.cfg.Trunk, nil
	}
	var refs []ghGitRef
	prefix := strings.TrimSuffix(c.cfg.BranchPrefix, "/")
	if err := c.do(ctx, http.MethodGet, c.repo("/git/matching-refs/heads/%s", pathRef(prefix)), nil, &refs); err != nil {
		return "", err
	}
	for _, r := range refs {
		if state.Head(r.Object.SHA) == base {
			return strings.TrimPrefix(r.Ref, "refs/heads/"), nil
		}
	}
	return "", adapter.Permanent(fmt.Errorf("the base %s is neither trunk's tip nor a train branch's", base))
}

// ClosePR implements adapter.Forge: every open pull request from the
// train's branch is closed, each with the reason as a comment.
func (c *Client) ClosePR(ctx context.Context, key string, t state.TrainID, reason string) error {
	open, err := c.pullsFrom(ctx, c.Branch(t), "open")
	if err != nil {
		return err
	}
	for _, p := range open {
		if reason != "" {
			if err := c.do(ctx, http.MethodPost, c.repo("/issues/%d/comments", p.Number), map[string]string{"body": reason}, nil); err != nil {
				return err
			}
		}
		if err := c.do(ctx, http.MethodPatch, c.repo("/pulls/%d", p.Number), map[string]string{"state": "closed"}, nil); err != nil {
			return err
		}
	}
	return nil
}

// ReadLanding implements adapter.Forge: the verdict is read from the check
// runs on head, not from anything CI or a hook reported (I9).
func (c *Client) ReadLanding(ctx context.Context, t state.TrainID, head state.Head, changes []state.ChangeID) (adapter.Reading, error) {
	var r adapter.Reading
	checks, err := c.checks(ctx, head)
	if err != nil {
		return r, err
	}
	if passed(checks, c.cfg.Checks) {
		r.Verdict = head
	}
	info, err := c.Describe(ctx, changes)
	if err != nil {
		return r, err
	}
	r.Heads = map[state.ChangeID]state.Head{}
	for _, i := range info {
		r.Heads[i.ID] = i.Head
	}
	if r.Trunk, err = c.branchHead(ctx, c.cfg.Trunk); err != nil {
		return r, err
	}
	return r, nil
}
