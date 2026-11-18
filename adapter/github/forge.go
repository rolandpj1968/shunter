package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
	Body string `json:"body"`
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

func (c *Client) approved(p ghPull) bool {
	if len(c.cfg.Authors) == 0 || p.labelled(c.cfg.ApprovedLabel) {
		return true
	}
	return slices.Contains(c.cfg.Authors, p.User.Login)
}

// Describe implements adapter.Forge. A change stacked on a branch that is
// not trunk names as its base the pull request from that branch (an open one
// first); a base branch with no pull request is named "branch:<name>", which
// the gate cannot find, and so refuses.
func (c *Client) Describe(ctx context.Context, ids []state.ChangeID) ([]adapter.ChangeInfo, error) {
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
			Approved: c.approved(p),
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
func (c *Client) OpenPR(ctx context.Context, key string, t state.TrainID, head, base state.Head) (string, error) {
	branch := c.Branch(t)
	open, err := c.pullsFrom(ctx, branch, "open")
	if err != nil {
		return "", err
	}
	for _, p := range open {
		if state.Head(p.Head.SHA) == head || strings.Contains(p.Body, keyMark(key)) {
			return strconv.Itoa(p.Number), c.dispatch(ctx, branch, head)
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
	body := fmt.Sprintf("A merge train, assembled by Shunter. It lands when CI passes on exactly this head.\n\nTrain `%s`, head `%s`, built on `%s`.\n\n%s\n", t, head, base, keyMark(key))
	in := map[string]any{"title": fmt.Sprintf("Merge train %s", t), "head": branch, "base": baseBranch, "body": body}
	var p ghPull
	if err := c.do(ctx, http.MethodPost, c.repo("/pulls"), in, &p); err != nil {
		if isStatus(err, http.StatusUnprocessableEntity) {
			// Perhaps it exists already: a try whose answer was lost.
			if again, lerr := c.pullsFrom(ctx, branch, "open"); lerr == nil {
				for _, q := range again {
					if state.Head(q.Head.SHA) == head {
						return strconv.Itoa(q.Number), c.dispatch(ctx, branch, head)
					}
				}
			}
		}
		return "", err
	}
	if p.Number == 0 {
		return "", fmt.Errorf("train %s: the forge opened no pull request number", t)
	}
	return strconv.Itoa(p.Number), c.dispatch(ctx, branch, head)
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
