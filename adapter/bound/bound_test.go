package bound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
)

func noSleep(waits *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error { *waits = append(*waits, d); return nil }
}

func TestClassify(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		want Kind
	}{
		{"marked permanent", adapter.Permanent(errors.New("no")), Permanent},
		{"plain error", errors.New("something broke"), Transient},
		{"deadline", context.DeadlineExceeded, Transient},
		{"502", &HTTPError{Status: 502}, Transient},
		{"500", &HTTPError{Status: 500}, Transient},
		{"no status", &HTTPError{Status: 0}, Transient},
		{"404", &HTTPError{Status: 404}, Permanent},
		{"422", &HTTPError{Status: 422}, Permanent},
		{"401", &HTTPError{Status: 401}, Permanent},
		{"403", &HTTPError{Status: 403}, Permanent},
		{"403 rate limited", &HTTPError{Status: 403, RateLimited: true}, Transient},
		{"429", &HTTPError{Status: 429}, Transient},
		{"408", &HTTPError{Status: 408}, Transient},
		{"wrapped 404", fmt.Errorf("reading: %w", &HTTPError{Status: 404}), Permanent},
		{"net error", &net.OpError{Op: "dial", Err: errors.New("refused")}, Transient},
		{"git non-ff", &GitError{Output: " ! [rejected] x -> main (non-fast-forward)"}, Permanent},
		{"git stale lease", &GitError{Output: " ! [rejected] main (stale info)"}, Permanent},
		{"git protected", &GitError{Output: "remote: error: GH006: Protected branch update failed"}, Permanent},
		{"git rule", &GitError{Output: "push declined due to repository rule violations"}, Permanent},
		{"git no ref", &GitError{Output: "error: src refspec nope does not match any"}, Permanent},
		{"git 502", &GitError{Output: "fatal: unable to access 'https://forge/': The requested URL returned error: 502"}, Transient},
		{"git https denied", &GitError{Output: "remote: Permission to o/r.git denied to u.\nfatal: unable to access 'https://forge/o/r.git/': The requested URL returned error: 403"}, Permanent},
		{"git hung up", &GitError{Output: "fatal: the remote end hung up unexpectedly"}, Transient},
		{"git lock race", &GitError{Output: "error: cannot lock ref 'refs/remotes/origin/x'"}, Transient},
		{"git unknown", &GitError{Output: "something odd"}, Transient},
	}
	for _, c := range cases {
		if got := Classify(ctx, c.err); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if got := Classify(cctx, errors.New("x")); got != Stopped {
		t.Errorf("a cancelled caller: %v, want stopped", got)
	}
}

func TestDoRetriesTransientThenSucceeds(t *testing.T) {
	var waits []time.Duration
	n := 0
	err := Do(context.Background(), Policy{Attempts: 4, Backoff: time.Second, Sleep: noSleep(&waits)}, "read", func(context.Context) error {
		n++
		if n < 3 {
			return &HTTPError{Status: 502}
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("err %v after %d tries", err, n)
	}
	if fmt.Sprint(waits) != "[1s 2s]" {
		t.Fatalf("waits %v", waits)
	}
}

func TestDoStopsOnPermanent(t *testing.T) {
	var waits []time.Duration
	n := 0
	err := Do(context.Background(), Policy{Attempts: 5, Sleep: noSleep(&waits)}, "read", func(context.Context) error {
		n++
		return &HTTPError{Status: 404, Method: "GET", Path: "/x"}
	})
	if n != 1 || !adapter.IsPermanent(err) {
		t.Fatalf("%d tries, err %v (permanent %v)", n, err, adapter.IsPermanent(err))
	}
}

func TestDoSpendsItsBudgetAndStaysTransient(t *testing.T) {
	var waits []time.Duration
	n := 0
	err := Do(context.Background(), Policy{Attempts: 3, Backoff: 10 * time.Second, MaxBackoff: 15 * time.Second, Sleep: noSleep(&waits)}, "read", func(context.Context) error {
		n++
		return &HTTPError{Status: 503}
	})
	if n != 3 || err == nil || adapter.IsPermanent(err) {
		t.Fatalf("%d tries, err %v", n, err)
	}
	if fmt.Sprint(waits) != "[10s 15s]" {
		t.Fatalf("waits %v, want capped at 15s", waits)
	}
}

func TestDoGivesEachTryADeadline(t *testing.T) {
	var waits []time.Duration
	n := 0
	err := Do(context.Background(), Policy{Attempts: 2, Timeout: 20 * time.Millisecond, Sleep: noSleep(&waits)}, "hang", func(ctx context.Context) error {
		n++
		<-ctx.Done()
		return errors.New("killed")
	})
	if n != 2 || !errors.Is(err, context.DeadlineExceeded) || adapter.IsPermanent(err) {
		t.Fatalf("%d tries, err %v", n, err)
	}
}

func TestDoHonoursTheCallersContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	err := Do(ctx, Policy{Attempts: 5, Sleep: func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }}, "x", func(context.Context) error {
		n++
		return &HTTPError{Status: 500}
	})
	if n != 1 || err == nil {
		t.Fatalf("%d tries, err %v", n, err)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Unix(1000, 0)
	h := http.Header{}
	h.Set("Retry-After", "7")
	if d := ParseRetryAfter(h, now); d != 7*time.Second {
		t.Fatalf("retry-after %v", d)
	}
	h = http.Header{}
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset", "1012")
	if d := ParseRetryAfter(h, now); d != 12*time.Second {
		t.Fatalf("reset %v", d)
	}
	p := Policy{Backoff: time.Second, MaxBackoff: time.Minute}
	if d := p.Wait(2, &HTTPError{Status: 429, RetryAfter: 20 * time.Second}); d != 20*time.Second {
		t.Fatalf("wait %v, want the forge's 20s", d)
	}
}
