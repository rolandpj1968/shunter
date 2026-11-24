package hooks_test

import (
	"slices"
	"testing"
)

// TestPreflightBlamesByWhatALineAdds: the pre-flight's blame reads each
// line's added lines from the tips, and the base for "new".
func TestPreflightBlamesByWhatALineAdds(t *testing.T) {
	tr := newTrain(t, map[string]string{"src/one.go": "package src\n\nvar y = 1\n"}, map[string]string{"src/late.go": "package src\n\nconst NewKind = 2\n"})
	cfg := map[string]any{
		"steps": []any{map[string]any{"name": "lint", "command": []string{"sh", "-c", "echo 'missing cases in switch of type src.Kind: src.NewKind'; exit 1"}}},
		"blame": map[string]any{"patterns": []any{map[string]any{"regex": "missing cases in switch of type \\S+: (.*)", "kind": "added",
			"tokens": "[A-Za-z_][A-Za-z0-9_]{5,}", "fallback": true, "new": []string{"*.go"}}}},
	}
	a, stderr := hook(t, "preflight.py", cfg, tr.input())
	if a["pass"] != false || !slices.Equal(strs(a["blamed"]), []string{"2"}) {
		t.Fatalf("%v\n%s", a, stderr)
	}
}

// TestBlameByANameNewInTheTrain: a failure names two names. One is on
// trunk already, and a line merely uses it; the other is new, and a line
// adds it. With "new", only the new name counts, so only the line that
// adds it is named. Each line's added lines are read with git.
func TestBlameByANameNewInTheTrain(t *testing.T) {
	d := newRepo(t)
	commit(t, d, "root", map[string]string{"k/kinds.go": "package k\n\nconst OldKind = 1\n"})
	git(t, d, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(t, d, "checkout", "--quiet", "-b", "one")
	h1 := commit(t, d, "one", map[string]string{"k/use.go": "package k\n\nvar x = OldKind\n"})
	git(t, d, "checkout", "--quiet", "-b", "two", "main")
	h2 := commit(t, d, "two", map[string]string{"k/late.go": "package k\n\nconst NewKind = 2\n"})
	in := map[string]any{"train": "t1", "head": "h",
		"evidence": "lint: missing cases in switch of type k.Kind: k.OldKind, k.NewKind",
		"lines": []any{
			map[string]any{"id": "1", "changes": []any{map[string]any{"ID": "1", "Head": h1}}},
			map[string]any{"id": "2", "changes": []any{map[string]any{"ID": "2", "Head": h2}}},
		}}
	pattern := map[string]any{"regex": "missing cases in switch of type \\S+: (.*)", "kind": "added",
		"tokens": "[A-Za-z_][A-Za-z0-9_]{5,}", "fallback": true}
	cfg := map[string]any{"git_dir": d, "trunk_ref": "refs/remotes/origin/main", "patterns": []any{pattern}}
	a, stderr := hook(t, "blame.py", cfg, in)
	if len(strs(a["blamed"])) != 0 {
		t.Fatalf("without new, both lines add a name the failure names: %v\n%s", a, stderr)
	}
	pattern["new"] = []string{"*.go"}
	a, stderr = hook(t, "blame.py", cfg, in)
	if a["class"] != "deterministic" || !slices.Equal(strs(a["blamed"]), []string{"2"}) {
		t.Fatalf("with new: %v\n%s", a, stderr)
	}
}
