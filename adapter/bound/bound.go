// Package bound gives every adapter call a deadline and a bounded retry
// with backoff (PROTOCOL.md §2.2: every external call is fallible; a failure
// is transient by default, and permanent only when the refusal follows from
// what the request means).
//
// The daemon retries an effect again, over a longer horizon, and escalates
// to J3 when its own budget is spent. The retry here is the short one: a
// handful of tries inside one call's budget, so that a single 502 does not
// cost an effect a whole backoff round in the daemon.
package bound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
)

// Policy bounds one call.
type Policy struct {
	// Attempts is the number of tries, at least 1. Default 3.
	Attempts int
	// Timeout bounds each try. Zero means only the caller's context bounds it.
	Timeout time.Duration
	// Backoff is the wait before the second try; it doubles for each try
	// after. Default 1s.
	Backoff time.Duration
	// MaxBackoff caps one wait. Default 30s.
	MaxBackoff time.Duration
	// Sleep waits; tests replace it. Default: a timer that the context
	// interrupts.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (p Policy) withDefaults() Policy {
	if p.Attempts <= 0 {
		p.Attempts = 3
	}
	if p.Backoff <= 0 {
		p.Backoff = time.Second
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = 30 * time.Second
	}
	if p.Sleep == nil {
		p.Sleep = sleep
	}
	return p
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait returns the wait before try n (n ≥ 2), or the error's own Retry-After
// when it names a longer one, capped by MaxBackoff.
func (p Policy) Wait(n int, err error) time.Duration {
	p = p.withDefaults()
	d := p.Backoff
	for i := 2; i < n && d < p.MaxBackoff; i++ {
		d *= 2
	}
	var h *HTTPError
	if errors.As(err, &h) && h.RetryAfter > d {
		d = h.RetryAfter
	}
	if d > p.MaxBackoff {
		d = p.MaxBackoff
	}
	return d
}

// Do runs f until it succeeds, fails permanently, or its tries are spent. Each
// try has its own deadline. The error returned is the last one; it is
// permanent (adapter.IsPermanent) exactly when Classify says so.
func Do(ctx context.Context, p Policy, what string, f func(ctx context.Context) error) error {
	p = p.withDefaults()
	var err error
	for n := 1; n <= p.Attempts; n++ {
		if n > 1 {
			if serr := p.Sleep(ctx, p.Wait(n, err)); serr != nil {
				return fmt.Errorf("%s: %w (and then %v)", what, err, serr)
			}
		}
		err = try(ctx, p.Timeout, f)
		if err == nil {
			return nil
		}
		switch Classify(ctx, err) {
		case Permanent:
			if !adapter.IsPermanent(err) {
				err = adapter.Permanent(err)
			}
			return fmt.Errorf("%s: %w", what, err)
		case Stopped:
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	return fmt.Errorf("%s: %d tries: %w", what, p.Attempts, err)
}

func try(ctx context.Context, timeout time.Duration, f func(ctx context.Context) error) error {
	if timeout <= 0 {
		return f(ctx)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := f(cctx)
	if err != nil && cctx.Err() != nil && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
		// The try's own deadline passed, and the callee wrapped it in
		// something else (a killed process): say so.
		err = fmt.Errorf("%w: %v", context.DeadlineExceeded, err)
	}
	return err
}

// Kind is an error's class.
type Kind uint8

const (
	// Transient errors are retried: a server error, a lost connection, a
	// try's deadline, an empty answer, a rate limit.
	Transient Kind = iota
	// Permanent errors follow from what the request means and are not
	// retried: not found, permission denied, a rule, not a fast-forward.
	Permanent
	// Stopped means the caller's own context ended: nothing more is tried.
	Stopped
)

func (k Kind) String() string { return [...]string{"transient", "permanent", "stopped"}[k] }

// Classify says what an error is. Anything not known to be permanent is
// transient (§2.2).
func Classify(ctx context.Context, err error) Kind {
	switch {
	case err == nil:
		return Transient
	case ctx != nil && ctx.Err() != nil:
		return Stopped
	case adapter.IsPermanent(err):
		return Permanent
	}
	var h *HTTPError
	if errors.As(err, &h) {
		return h.Kind()
	}
	var g *GitError
	if errors.As(err, &g) {
		return g.Kind()
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return Transient
	}
	return Transient
}

// HTTPError is a forge's answer that was not a success.
type HTTPError struct {
	Method     string
	Path       string
	Status     int
	Message    string
	RetryAfter time.Duration
	// RateLimited is set when the forge says the limit is spent: a 403 or
	// a 429 that waiting clears.
	RateLimited bool
}

func (e *HTTPError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, msg)
}

// Kind classifies by status: server errors, timeouts and rate limits are
// transient; the other client errors follow from the request and are
// permanent.
func (e *HTTPError) Kind() Kind {
	switch {
	case e.RateLimited, e.Status == http.StatusTooManyRequests, e.Status == http.StatusRequestTimeout:
		return Transient
	case e.Status >= 500, e.Status == 0:
		return Transient
	case e.Status >= 400:
		return Permanent
	}
	return Transient
}

// ParseRetryAfter reads a Retry-After header in seconds, or an
// X-RateLimit-Reset epoch, relative to now.
func ParseRetryAfter(h http.Header, now time.Time) time.Duration {
	if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if n, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if d := time.Unix(n, 0).Sub(now); d > 0 {
				return d
			}
		}
	}
	return 0
}

// GitError is a git command that failed, with what it printed.
type GitError struct {
	Args   []string
	Code   int
	Output string
}

func (e *GitError) Error() string {
	out := strings.TrimSpace(e.Output)
	if len(out) > 600 {
		out = out[:600] + "…"
	}
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.Args, " "), e.Code, out)
}

// permanentGit is what a refusal that follows from the request looks like:
// not a fast-forward, a stale lease, permission, a protected branch or a
// rule, a reference that does not exist. Anything else (a dropped
// connection, a server error, a lock race) is transient.
var permanentGit = regexp.MustCompile(`(?i)non-fast-forward|\[rejected\]|fetch first|stale info|permission to .* denied|permission denied|protected branch|declined due to repository rule|rule violation|does not match any|src refspec|couldn't find remote ref|not a valid object name|unknown revision|bad revision|invalid reference`)

// transientGit overrides a permanent-looking line that is really the
// network: a 5xx from the remote, a reset connection, a lock race.
var transientGit = regexp.MustCompile(`(?i)(returned error|http)[: ]+5\d\d\b|internal server error|connection (reset|refused|timed out)|could not resolve host|early eof|the remote end hung up|cannot lock ref|operation timed out|rpc failed`)

// Kind classifies a git failure by its output.
func (e *GitError) Kind() Kind {
	if transientGit.MatchString(e.Output) {
		return Transient
	}
	if permanentGit.MatchString(e.Output) {
		return Permanent
	}
	return Transient
}
