package hook

import (
	"context"
	"strings"

	"github.com/rolandpj1968/shunter/adapter"
)

// Gate is a repository's own rules at the gate (§5.2), as a hook. Its input
// is the line offered (adapter.Offered); it answers {"refuse": "<reason>"}
// to refuse it, or {} to let it in. A hook that gives no answer refuses
// nothing: the gate's own checks stand, and the line check still runs.
type Gate struct{ Hook }

type gateAnswer struct {
	Refuse string `json:"refuse"`
}

// Admit implements adapter.Admitter.
func (g Gate) Admit(ctx context.Context, o adapter.Offered) (string, error) {
	var a gateAnswer
	if _, err := g.Run(ctx, o, &a); err != nil {
		return "", err
	}
	return strings.TrimSpace(a.Refuse), nil
}
