package notify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/hook"
	"github.com/rolandpj1968/shunter/state"
)

// TestTellCarriesStateAndCause: an owner's notice reaches the events file
// and the hook with the state the line went to and why, so that delivery
// can act by kind.
func TestTellCarriesStateAndCause(t *testing.T) {
	dir := t.TempDir()
	got := filepath.Join(dir, "got.json")
	p := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\ncat > "+got+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	n, err := Open(filepath.Join(dir, "events.jsonl"), hook.Hook{Command: []string{p}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var _ adapter.Teller = n
	o := adapter.OwnerNotice{Line: "7", Changes: []state.ChangeID{"7"}, State: "returned", Cause: state.CauseCharged, Message: "returned: CI failed in x_test.go"}
	if err := n.Tell(context.Background(), "k1", o); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	var no Notice
	if err := json.Unmarshal(b, &no); err != nil {
		t.Fatal(err)
	}
	if no.To != "owner" || no.State != "returned" || no.Cause != state.CauseCharged || no.Line != "7" {
		t.Fatalf("the hook read %+v", no)
	}
	ev, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if !strings.Contains(string(ev), `"cause":"charged"`) {
		t.Fatalf("events file: %s", ev)
	}
}
