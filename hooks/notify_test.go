package hooks_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// notify runs the notify hook on one notice; it answers by its exit.
func notify(t *testing.T, cfg map[string]any, in map[string]any) error {
	t.Helper()
	py := python(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "notify.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(in)
	cmd := exec.Command(py, "-B", "notify.py", "--config", p)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	return cmd.Run()
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// TestNotifyRehearsal: one notice of each kind through a dry run. A
// charge marks each change as being fixed and comments; a conflict with
// trunk asks for a rebase; every owner's notice reaches the owners' file;
// the judge's go to the alerts file; and nothing else runs.
func TestNotifyRehearsal(t *testing.T) {
	dir := t.TempDir()
	dry, owners, alerts := filepath.Join(dir, "dry.jsonl"), filepath.Join(dir, "owners.tsv"), filepath.Join(dir, "alerts.log")
	cfg := map[string]any{
		"dry_run": dry,
		"rules": []any{
			map[string]any{"when": map[string]any{"to": "owner", "state": "returned", "cause": []string{"charged"}},
				"run": []any{[]string{"forge", "label", "{change}", "being-fixed"}, []string{"forge", "comment", "{change}", "{message}"}}},
			map[string]any{"when": map[string]any{"to": "owner", "cause": "conflict"},
				"run": []any{[]string{"forge", "comment", "{change}", "please rebase: {message}"}}},
			map[string]any{"when": map[string]any{"to": "owner"},
				"append": []any{map[string]any{"file": owners, "format": "{changes}\t{kind}\t{cause}\t{message}"}}},
			map[string]any{"when": map[string]any{"to": "judge"},
				"append": []any{map[string]any{"file": alerts, "format": "{point} {train}: {message}"}}},
		},
	}
	notices := []map[string]any{
		{"to": "owner", "line": "1", "changes": []string{"1", "2"}, "state": "returned", "cause": "charged", "message": "returned: CI failed in a_test.go {change}"},
		{"to": "owner", "line": "3", "changes": []string{"3"}, "state": "returned", "cause": "conflict", "message": "returned: conflicts with trunk in x.go"},
		{"to": "owner", "line": "4", "changes": []string{"4"}, "state": "returned", "cause": "refused", "message": "returned: not from an approved producer"},
		{"to": "owner", "line": "5", "changes": []string{"5"}, "state": "held", "cause": "hold", "message": "held: review says no"},
		{"to": "owner", "line": "6", "changes": []string{"6"}, "state": "landed", "cause": "landed", "message": "landed: in train t9"},
		{"to": "owner", "line": "7", "changes": []string{"7"}, "cause": "conflict", "message": "conflicts with trunk (y.go) and was not rebased by the runner"},
		{"to": "judge", "point": "J1", "train": "t9", "message": "J1: nobody named"},
	}
	for _, n := range notices {
		if err := notify(t, cfg, n); err != nil {
			t.Fatalf("%v: %v", n, err)
		}
	}
	want := []string{
		`["forge", "label", "1", "being-fixed"]`, `["forge", "label", "2", "being-fixed"]`,
		`["forge", "comment", "1", "returned: CI failed in a_test.go {change}"]`, `["forge", "comment", "2", "returned: CI failed in a_test.go {change}"]`,
		`["forge", "comment", "3", "please rebase: returned: conflicts with trunk in x.go"]`,
		`["forge", "comment", "7", "please rebase: conflicts with trunk (y.go) and was not rebased by the runner"]`,
	}
	if got := readLines(t, dry); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	o := readLines(t, owners)
	if len(o) != 6 || o[0] != "1 2\treturned\tcharged\treturned: CI failed in a_test.go {change}" || !strings.HasPrefix(o[5], "7\tadvice\tconflict\t") {
		t.Fatalf("owners' file:\n%s", strings.Join(o, "\n"))
	}
	if a := readLines(t, alerts); len(a) != 1 || a[0] != "J1 t9: J1: nobody named" {
		t.Fatalf("alerts: %v", a)
	}
}

// TestNotifyFailureIsNotDelivered: a command that fails fails the
// delivery, so Shunter runs the hook again.
func TestNotifyFailureIsNotDelivered(t *testing.T) {
	cfg := map[string]any{"rules": []any{map[string]any{"run": []any{[]string{"false"}}}}}
	if err := notify(t, cfg, map[string]any{"to": "owner", "line": "1", "message": "m"}); err == nil {
		t.Fatal("a failed command was delivered")
	}
	cfg = map[string]any{"rules": []any{map[string]any{"when": map[string]any{"to": "judge"}, "run": []any{[]string{"false"}}}}}
	if err := notify(t, cfg, map[string]any{"to": "owner", "line": "1", "message": "m"}); err != nil {
		t.Fatalf("a rule that doesn't match ran: %v", err)
	}
}
