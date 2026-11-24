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

// TestPreflightStepCommits: the old queue's two commits to its train, as
// steps. "regenerate" rebuilds a generated file stale only in combination
// and may commit only files marked generated; "counters" records an
// improvement and may commit only the counters file. What a step changes
// outside its allowance is undone; a later step sees the new head; a step
// that changes nothing commits nothing.
func TestPreflightStepCommits(t *testing.T) {
	tr := newTrain(t,
		map[string]string{".gitattributes": "gen/** merge=regenerate\n", "a.src": "a1\n", "gen/out.txt": "a1\n", "counters.txt": "9\n"},
		map[string]string{"b.src": "b1\n"})
	cfg := map[string]any{"steps": []any{
		map[string]any{"name": "regenerate", "command": []string{"sh", "-c", "cat a.src b.src > gen/out.txt; echo stray > stray.txt; echo x >> README"},
			"commit": map[string]any{"message": "regenerate train {train}", "attribute": "merge", "value": "regenerate"}},
		map[string]any{"name": "counters", "command": []string{"sh", "-c", "echo 7 > counters.txt"},
			"commit": map[string]any{"message": "record what the train improves", "paths": []string{"counters.txt"}}},
		map[string]any{"name": "unchanged", "command": []string{"true"},
			"commit": map[string]any{"message": "never made", "paths": []string{"counters.txt"}}},
		map[string]any{"name": "sees-the-head", "command": []string{"sh", "-c", "test \"$SHUNTER_HEAD\" = \"$(git rev-parse HEAD)\""}},
	}}
	a, stderr := hook(t, "preflight.py", cfg, tr.input())
	if a["pass"] != true {
		t.Fatalf("%v\n%s", a, stderr)
	}
	if got := git(t, tr.dir, "log", "--format=%s", tr.tip2+"..HEAD"); got != "record what the train improves\nregenerate train t1" {
		t.Fatalf("commits on the train: %q", got)
	}
	if got := git(t, tr.dir, "show", "HEAD~1", "--name-only", "--format="); got != "gen/out.txt" {
		t.Fatalf("the regenerate commit holds %q", got)
	}
	if got := git(t, tr.dir, "show", "HEAD:gen/out.txt"); got != "a1\nb1" {
		t.Fatalf("gen/out.txt is %q", got)
	}
	if got := git(t, tr.dir, "show", "HEAD", "--name-only", "--format="); got != "counters.txt" {
		t.Fatalf("the counters commit holds %q", got)
	}
	if st := git(t, tr.dir, "status", "--porcelain"); st != "" {
		t.Fatalf("the tree is left changed: %q", st)
	}
	if !strings.Contains(stderr, "undone: README stray.txt") {
		t.Fatalf("the undone files are not said:\n%s", stderr)
	}
}

// TestPreflightCommitNeedsAnAllowance: a commit with no paths and no
// attribute could commit anything; the hook answers nothing, which Shunter
// reads as no verdict.
func TestPreflightCommitNeedsAnAllowance(t *testing.T) {
	py := python(t)
	tr := newTrain(t, map[string]string{"one.txt": "1\n"}, map[string]string{"two.txt": "2\n"})
	cfg := filepath.Join(t.TempDir(), "c.json")
	b, _ := json.Marshal(map[string]any{"steps": []any{
		map[string]any{"name": "gen", "command": []string{"true"}, "commit": map[string]any{"message": "m"}},
	}})
	if err := os.WriteFile(cfg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(tr.input())
	cmd := exec.Command(py, "-B", "preflight.py", "--config", cfg)
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.Output()
	if err == nil || len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("a commit with no allowance answered %q (%v)", out, err)
	}
}
