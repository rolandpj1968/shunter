package notify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter/hook"
)

var ctx = context.Background()

func lines(t *testing.T, p string) []string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestEventsFileOncePerKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "events.jsonl")
	n, err := Open(p, hook.Hook{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Owner(ctx, "k1", "l1", nil, "returned: conflict in a.txt"); err != nil {
		t.Fatal(err)
	}
	n.Owner(ctx, "k1", "l1", nil, "returned: conflict in a.txt")
	n.Judge(ctx, "k2", "J1", "t1", "blame names nobody")
	n.Judge(ctx, "", "alert", "", "an unkeyed alert")
	n.Judge(ctx, "", "alert", "", "an unkeyed alert")
	if got := lines(t, p); len(got) != 4 {
		t.Fatalf("%d lines: %v", len(got), got)
	}
	// A restart reads the keys back.
	n2, err := Open(p, hook.Hook{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	n2.Owner(ctx, "k1", "l1", nil, "again")
	if got := lines(t, p); len(got) != 4 {
		t.Fatalf("after a restart: %d lines", len(got))
	}
}

func TestHookDeliversAndFailureIsTheCallers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	out := filepath.Join(dir, "delivered")
	flag := filepath.Join(dir, "fail")
	script := filepath.Join(dir, "deliver.sh")
	body := "#!/bin/sh\nif [ -e " + flag + " ]; then echo down >&2; exit 1; fi\ncat >> " + out + "\necho >> " + out + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(flag, nil, 0o644)
	n, err := Open(p, hook.Hook{Command: []string{script}, Timeout: 10 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Owner(ctx, "k1", "l1", nil, "landed"); err == nil {
		t.Fatal("a hook that fails is the call's failure")
	}
	os.Remove(flag)
	if err := n.Owner(ctx, "k1", "l1", nil, "landed"); err != nil {
		t.Fatal(err)
	}
	if err := n.Owner(ctx, "k1", "l1", nil, "landed"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if c := strings.Count(string(got), `"key":"k1"`); c != 1 {
		t.Fatalf("delivered %d times: %s", c, got)
	}
	// The file has the notice and its delivery.
	if ls := lines(t, p); len(ls) != 2 || !strings.Contains(ls[1], `"delivered":true`) {
		t.Fatalf("events %v", ls)
	}
}
