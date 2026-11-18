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

// Owner implements adapter.Notifier.
func (n Landed) Owner(ctx context.Context, key string, line state.LineID, changes []state.ChangeID, msg string) error {
	if strings.HasPrefix(msg, state.LineLanded.String()+":") {
		for _, id := range changes {
			if err := n.markLanded(ctx, id, msg); err != nil {
				return err
			}
		}
	}
	return n.Next.Owner(ctx, key, line, changes, msg)
}

// Judge implements adapter.Notifier.
func (n Landed) Judge(ctx context.Context, key, point string, t state.TrainID, msg string) error {
	return n.Next.Judge(ctx, key, point, t, msg)
}

func (c *Client) markLanded(ctx context.Context, id state.ChangeID, msg string) error {
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
