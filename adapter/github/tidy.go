package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
)

// Prune implements adapter.Pruner: the branch of every train that has ended
// is deleted (open question 20). A train's branch is the base of the next
// train's pull request while both ride, and deleting a pull request's base
// closes it, so every open pull request based on the branch is first
// retargeted to trunk: the train it was built on has ended, and if it landed,
// trunk now holds exactly that branch's commits. A branch that is still the
// head of an open pull request is left for a later prune: its train's
// closing, with the reason, comes first.
//
// It is safe to repeat and to interrupt: a branch already gone is no error.
func (c *Client) Prune(ctx context.Context, live []state.TrainID) error {
	keep := map[string]bool{}
	for _, t := range live {
		keep[c.Branch(t)] = true
	}
	var refs []ghGitRef
	prefix := strings.TrimSuffix(c.cfg.BranchPrefix, "/")
	if err := c.do(ctx, http.MethodGet, c.repo("/git/matching-refs/heads/%s", pathRef(prefix)), nil, &refs); err != nil {
		return err
	}
	for _, r := range refs {
		branch := strings.TrimPrefix(r.Ref, "refs/heads/")
		if !strings.HasPrefix(branch, c.cfg.BranchPrefix) || keep[branch] || branch == c.cfg.Trunk {
			continue
		}
		heads, err := c.pullsFrom(ctx, branch, "open")
		if err != nil {
			return err
		}
		if len(heads) > 0 {
			continue
		}
		if err := c.retargetFrom(ctx, branch, nil, fmt.Sprintf("Its base `%s` was a merge train's branch, and that train has ended; retargeted to `%s` by Shunter.", branch, c.cfg.Trunk)); err != nil {
			return err
		}
		if err := c.do(ctx, http.MethodDelete, c.repo("/git/refs/heads/%s", pathRef(branch)), nil, nil); err != nil &&
			!IsNotFound(err) && !isStatus(err, http.StatusUnprocessableEntity) {
			return err
		}
	}
	return nil
}

// pullsOnto lists the open pull requests whose base is branch.
func (c *Client) pullsOnto(ctx context.Context, branch string) ([]ghPull, error) {
	var ps []ghPull
	q := url.Values{"base": {branch}, "state": {"open"}, "per_page": {"100"}}
	err := c.do(ctx, http.MethodGet, c.repo("/pulls?%s", q.Encode()), nil, &ps)
	return ps, err
}

// retargetFrom moves every open pull request based on branch onto trunk,
// with a comment saying why, except those in skip.
func (c *Client) retargetFrom(ctx context.Context, branch string, skip map[int]bool, why string) error {
	if branch == "" || branch == c.cfg.Trunk {
		return nil
	}
	ps, err := c.pullsOnto(ctx, branch)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if skip[p.Number] || p.Base.Ref != branch {
			continue
		}
		if err := c.retarget(ctx, p.Number, why); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) retarget(ctx context.Context, n int, why string) error {
	if err := c.do(ctx, http.MethodPatch, c.repo("/pulls/%d", n), map[string]string{"base": c.cfg.Trunk}, nil); err != nil {
		return err
	}
	if why == "" {
		return nil
	}
	return c.do(ctx, http.MethodPost, c.repo("/issues/%d/comments", n), map[string]string{"body": why}, nil)
}

// Retarget implements adapter.Retargeter: a change stacked on a change that
// has landed is moved onto trunk (open question 21). A change already on
// trunk, or not open, is left as it is.
func (c *Client) Retarget(ctx context.Context, id state.ChangeID) error {
	n, err := number(id)
	if err != nil {
		return err
	}
	p, err := c.pull(ctx, n)
	if err != nil {
		return err
	}
	if p.State != "open" || p.Base.Ref == c.cfg.Trunk {
		return nil
	}
	return c.retarget(ctx, n, fmt.Sprintf("Its base `%s` has landed on `%s`; retargeted to `%s` by Shunter.", p.Base.Ref, c.cfg.Trunk, c.cfg.Trunk))
}

var (
	_ adapter.Pruner     = (*Client)(nil)
	_ adapter.Retargeter = (*Client)(nil)
)
