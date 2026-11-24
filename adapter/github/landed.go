package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
)

// Landed is a notifier that also records a landing on the forge. A train
// built by replaying commits lands new commits, so the forge never sees its
// changes as merged: each change of a landed line is labelled LandedLabel
// (when set), told where it landed, and closed. Then the notice goes on to
// Next. It is safe to repeat: a closed change is left as it is.
type Landed struct {
	*Client
	Next adapter.Notifier
}

// Tell implements adapter.Teller: the landing is recorded, and the whole
// notice goes on to Next if it takes one.
func (n Landed) Tell(ctx context.Context, key string, o adapter.OwnerNotice) error {
	if err := n.landed(ctx, o.Changes, o.Message); err != nil {
		return err
	}
	if t, ok := n.Next.(adapter.Teller); ok {
		return t.Tell(ctx, key, o)
	}
	return n.Next.Owner(ctx, key, o.Line, o.Changes, o.Message)
}

// Owner implements adapter.Notifier.
func (n Landed) Owner(ctx context.Context, key string, line state.LineID, changes []state.ChangeID, msg string) error {
	if err := n.landed(ctx, changes, msg); err != nil {
		return err
	}
	return n.Next.Owner(ctx, key, line, changes, msg)
}

func (n Landed) landed(ctx context.Context, changes []state.ChangeID, msg string) error {
	if strings.HasPrefix(msg, state.LineLanded.String()+":") {
		mine := map[int]bool{}
		for _, id := range changes {
			if num, err := number(id); err == nil {
				mine[num] = true
			}
		}
		for _, id := range changes {
			if err := n.markLanded(ctx, id, msg, mine); err != nil {
				return err
			}
		}
	}
	return nil
}

// Judge implements adapter.Notifier.
func (n Landed) Judge(ctx context.Context, key, point string, t state.TrainID, msg string) error {
	return n.Next.Judge(ctx, key, point, t, msg)
}

func (c *Client) markLanded(ctx context.Context, id state.ChangeID, msg string, line map[int]bool) error {
	num, err := number(id)
	if err != nil {
		return err
	}
	p, err := c.pull(ctx, num)
	if err != nil {
		return err
	}
	if p.State != "open" {
		return nil
	}
	why := fmt.Sprintf("Its base #%d landed on `%s`; retargeted to `%s` by Shunter.", num, c.cfg.Trunk, c.cfg.Trunk)
	if err := c.retargetFrom(ctx, p.Head.Ref, line, why); err != nil {
		return err
	}
	if c.cfg.LandedLabel != "" && !p.labelled(c.cfg.LandedLabel) {
		if err := c.do(ctx, http.MethodPost, c.repo("/issues/%d/labels", num), map[string][]string{"labels": {c.cfg.LandedLabel}}, nil); err != nil {
			return err
		}
	}
	body := fmt.Sprintf("Landed on %s by Shunter (%s).", c.cfg.Trunk, msg)
	if err := c.do(ctx, http.MethodPost, c.repo("/issues/%d/comments", num), map[string]string{"body": body}, nil); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, c.repo("/pulls/%d", num), map[string]string{"state": "closed"}, nil)
}
