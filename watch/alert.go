package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rolandpj1968/shunter/adapter/hook"
)

// Alerter raises findings through the notify hook, once each, then as a
// reminder every Realert while the finding stands, and says when one has
// cleared. What it has raised is kept in the State file between runs.
type Alerter struct {
	// Hook is the notify hook; with none, notices go to Out.
	Hook hook.Hook
	// State is the file that remembers what was raised.
	State string
	// Realert is how often a standing finding is raised again. Default 1h.
	Realert time.Duration
	// Out takes the notices when there is no hook. Default stdout.
	Out *os.File
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// DryRun sends nothing through the hook and changes no State: each
	// notice that would be sent goes to Out instead. For checks and tests
	// whose hook would reach people.
	DryRun bool
}

// Notice is what the hook reads: the shape of the daemon's notices, to the
// judge, at the point "watch".
type Notice struct {
	Time    time.Time `json:"time"`
	Key     string    `json:"key"`
	To      string    `json:"to"`
	Point   string    `json:"point"`
	Message string    `json:"message"`
}

type raised struct {
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
	Message string    `json:"message"`
}

// Raise sends what is new, due again or cleared, and returns the notices
// sent. A notice the hook did not take is tried again at the next run.
func (a Alerter) Raise(ctx context.Context, fs []Finding) ([]Notice, error) {
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	if a.Realert <= 0 {
		a.Realert = time.Hour
	}
	t := now()
	state := map[string]raised{}
	if a.State != "" {
		if b, err := os.ReadFile(a.State); err == nil {
			if err := json.Unmarshal(b, &state); err != nil {
				state = map[string]raised{}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	var out []Notice
	var errs []error
	send := func(subject, msg string) bool {
		n := Notice{Time: t, Key: fmt.Sprintf("watch:%s:%d", subject, t.Unix()), To: "judge", Point: "watch", Message: msg}
		if err := a.send(ctx, n); err != nil {
			errs = append(errs, err)
			return false
		}
		out = append(out, n)
		return true
	}
	seen := map[string]bool{}
	for _, f := range fs {
		seen[f.Subject] = true
		r, ok := state[f.Subject]
		switch {
		case !ok:
			if send(f.Subject, "Shunter watch: "+f.Message) {
				state[f.Subject] = raised{First: t, Last: t, Message: f.Message}
			}
		case t.Sub(r.Last) >= a.Realert:
			if send(f.Subject, fmt.Sprintf("Shunter watch, still, since %s: %s", r.First.UTC().Format(time.RFC3339), f.Message)) {
				r.Last, r.Message = t, f.Message
				state[f.Subject] = r
			}
		}
	}
	gone := make([]string, 0)
	for s := range state {
		if !seen[s] {
			gone = append(gone, s)
		}
	}
	sort.Strings(gone)
	for _, s := range gone {
		if send(s, "Shunter watch: cleared: "+state[s].Message) {
			delete(state, s)
		}
	}
	if a.State != "" && !a.DryRun {
		b, _ := json.MarshalIndent(state, "", "  ")
		if err := writeAtomic(a.State, b); err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

func (a Alerter) send(ctx context.Context, n Notice) error {
	if !a.Hook.Configured() || a.DryRun {
		o := a.Out
		if o == nil {
			o = os.Stdout
		}
		b, _ := json.Marshal(n)
		_, err := fmt.Fprintln(o, string(b))
		return err
	}
	res, err := a.Hook.Run(ctx, n, nil)
	if err != nil {
		return fmt.Errorf("the notify hook: %w %s", err, res.Stderr)
	}
	if res.Code != 0 {
		return fmt.Errorf("the notify hook exited %d: %s", res.Code, res.Stderr)
	}
	return nil
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}
