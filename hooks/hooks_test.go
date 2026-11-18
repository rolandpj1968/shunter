// Package hooks_test runs the example hooks as Shunter runs them: one JSON
// document on stdin, one on stdout, against synthetic fixtures and
// throwaway git repositories. The hooks are Python; the tests are skipped
// where python3 is not installed.
package hooks_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var ident = []string{
	"GIT_AUTHOR_NAME=Test Producer", "GIT_AUTHOR_EMAIL=producer@example.com",
	"GIT_COMMITTER_NAME=Test Producer", "GIT_COMMITTER_EMAIL=producer@example.com",
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
}

func python(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	return p
}

// hook runs one hook with a configuration and an input, and decodes its
// answer. extra is added to its environment.
func hook(t *testing.T, script string, cfg any, in any, extra ...string) (map[string]any, string) {
	t.Helper()
	py := python(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(in)
	abs, _ := filepath.Abs(script)
	cmd := exec.Command(py, "-B", abs, "--config", cfgPath)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(append(os.Environ(), ident...), extra...)
	cmd.Env = append(cmd.Env, "PYTHONDONTWRITEBYTECODE=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v\nstdout: %s\nstderr: %s", script, err, out.String(), errb.String())
	}
	var a map[string]any
	dec := json.NewDecoder(&out)
	dec.UseNumber()
	if err := dec.Decode(&a); err != nil {
		t.Fatalf("%s: no JSON answer: %v\nstderr: %s", script, err, errb.String())
	}
	return a, errb.String()
}

func strs(v any) []string {
	var out []string
	for _, x := range asSlice(v) {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), ident...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitTry(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), ident...)
	return cmd.Run()
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, msg string, files map[string]string) string {
	t.Helper()
	for f, b := range files {
		write(t, dir, f, b)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

func newRepo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	git(t, d, "init", "--quiet", "--initial-branch=main")
	return d
}

// TestHooksWriteNoBytecode: run as a configuration may run them, with no -B
// and no PYTHONDONTWRITEBYTECODE, the hooks still write no __pycache__
// beside themselves (Shunter runs them in its clone).
func TestHooksWriteNoBytecode(t *testing.T) {
	py := python(t)
	dir := t.TempDir()
	for _, f := range []string{"shunterhooks.py", "blame.py", "preflight.py", "conflict.py"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		write(t, dir, f, string(b))
	}
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "PYTHONDONTWRITEBYTECODE=") && !strings.HasPrefix(e, "PYTHONPYCACHEPREFIX=") {
			env = append(env, e)
		}
	}
	cmd := exec.Command(py, filepath.Join(dir, "blame.py"))
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(`{"train":"t1","head":"h","evidence":"x","lines":[{"id":"1","files":["a/b.go"]}]}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "__pycache__")); !os.IsNotExist(err) {
		t.Fatalf("the hook wrote __pycache__: %v", err)
	}
}

// TestBlameFixtures runs the blame hook on each case in testdata/blame:
// {"config", "input", "want": {"class", "blamed", "evidence_contains"}}.
func TestBlameFixtures(t *testing.T) {
	cases, err := filepath.Glob("testdata/blame/*.json")
	if err != nil || len(cases) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, p := range cases {
		t.Run(strings.TrimSuffix(filepath.Base(p), ".json"), func(t *testing.T) {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var c struct {
				Config map[string]any `json:"config"`
				Input  map[string]any `json:"input"`
				Want   struct {
					Class    string   `json:"class"`
					Blamed   []string `json:"blamed"`
					Evidence string   `json:"evidence_contains"`
				} `json:"want"`
			}
			if err := json.Unmarshal(b, &c); err != nil {
				t.Fatal(err)
			}
			a, stderr := hook(t, "blame.py", c.Config, c.Input)
			if a["class"] != c.Want.Class {
				t.Fatalf("class %v, want %s (%v)\n%s", a["class"], c.Want.Class, a, stderr)
			}
			if got := strs(a["blamed"]); !slices.Equal(got, c.Want.Blamed) && !(len(got) == 0 && len(c.Want.Blamed) == 0) {
				t.Fatalf("blamed %v, want %v\n%s", got, c.Want.Blamed, stderr)
			}
			if c.Want.Evidence != "" && !strings.Contains(fmt.Sprint(a["evidence"]), c.Want.Evidence) {
				t.Fatalf("evidence %q lacks %q", a["evidence"], c.Want.Evidence)
			}
			if len(strs(a["blamed"])) > 0 && a["evidence"] == "" {
				t.Fatal("a charge with no evidence")
			}
		})
	}
}

// TestBlameReadsEachLinesFilesFromGit: lines with no files given are read
// from a clone: what each line's last head changes since it left trunk.
func TestBlameReadsEachLinesFilesFromGit(t *testing.T) {
	d := newRepo(t)
	commit(t, d, "root", map[string]string{"README": "x\n", "a/x.go": "package a\n", "b/y.go": "package b\n"})
	git(t, d, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(t, d, "checkout", "--quiet", "-b", "one")
	h1 := commit(t, d, "one", map[string]string{"a/x.go": "package a // 1\n"})
	git(t, d, "checkout", "--quiet", "-b", "two", "main")
	h2a := commit(t, d, "two a", map[string]string{"b/y.go": "package b // 2\n"})
	h2b := commit(t, d, "two b", map[string]string{"b/z.go": "package b\n"})
	in := map[string]any{"train": "t1", "head": "h", "evidence": "b/z.go:1: syntax error",
		"lines": []any{
			map[string]any{"id": "1", "changes": []any{map[string]any{"ID": "1", "Head": h1}}},
			map[string]any{"id": "2", "changes": []any{map[string]any{"ID": "2", "Head": h2a}, map[string]any{"ID": "3", "Head": h2b}}},
		}}
	a, stderr := hook(t, "blame.py", map[string]any{"git_dir": d, "trunk_ref": "refs/remotes/origin/main"}, in)
	if a["class"] != "deterministic" || !slices.Equal(strs(a["blamed"]), []string{"2"}) {
		t.Fatalf("%v\n%s", a, stderr)
	}
	// A head the clone does not have, and no remote to fetch it from:
	// nothing is judged on what could not be read.
	in["lines"].([]any)[0].(map[string]any)["changes"] = []any{map[string]any{"ID": "1", "Head": strings.Repeat("0", 40)}}
	a, _ = hook(t, "blame.py", map[string]any{"git_dir": d, "trunk_ref": "refs/remotes/origin/main"}, in)
	if a["class"] != "no-verdict" {
		t.Fatalf("an unreadable head: %v", a)
	}
}

// train is a clone with a train built in it: base, then line 1, then line
// 2, each one commit, with the work tree at the head.
type train struct {
	dir              string
	base, tip1, tip2 string
	lines            []any
	tips             map[string]string
}

func newTrain(t *testing.T, one, two map[string]string) train {
	d := newRepo(t)
	tr := train{dir: d}
	tr.base = commit(t, d, "root", map[string]string{"README": "x\n", "src/a.go": "package src\n"})
	tr.tip1 = commit(t, d, "line 1", one)
	tr.tip2 = commit(t, d, "line 2", two)
	git(t, d, "checkout", "--quiet", "--detach", tr.tip2)
	tr.lines = []any{
		map[string]any{"ID": "1", "Changes": []any{map[string]any{"ID": "1", "Head": "c1"}}},
		map[string]any{"ID": "2", "Changes": []any{map[string]any{"ID": "2", "Head": "c2"}}},
	}
	tr.tips = map[string]string{"1": tr.tip1, "2": tr.tip2}
	return tr
}

func (tr train) input() map[string]any {
	return map[string]any{"dir": tr.dir, "train": "t1", "base": tr.base, "head": tr.tip2, "lines": tr.lines, "tips": tr.tips}
}

func readRecord(t *testing.T, p string) [][]string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			rows = append(rows, strings.Split(l, "\t"))
		}
	}
	return rows
}

func TestPreflightRunsTimesAndRecordsEachStep(t *testing.T) {
	tr := newTrain(t, map[string]string{"one.txt": "1\n"}, map[string]string{"src/b.go": "package src\n"})
	rec := filepath.Join(t.TempDir(), "steps.tsv")
	cfg := map[string]any{"record": rec, "steps": []any{
		map[string]any{"name": "first", "command": []string{"sh", "-c", "test \"$SHUNTER_TRAIN\" = t1"}},
		map[string]any{"name": "second", "command": []string{"true"}},
	}}
	a, stderr := hook(t, "preflight.py", cfg, tr.input())
	if a["pass"] != true {
		t.Fatalf("%v\n%s", a, stderr)
	}
	rows := readRecord(t, rec)
	if len(rows) != 2 || rows[0][1] != "t1" || rows[0][2] != "first" || rows[1][2] != "second" || rows[0][4] != "0" {
		t.Fatalf("record %v", rows)
	}
	// A failure stops the run, and its output names a file only line 2
	// touches: line 2 is blamed, from the tips.
	cfg["steps"] = []any{
		map[string]any{"name": "vet", "command": []string{"sh", "-c", "echo 'src/b.go:1:1: bad'; exit 1"}},
		map[string]any{"name": "never-reached", "command": []string{"true"}},
	}
	a, stderr = hook(t, "preflight.py", cfg, tr.input())
	if a["pass"] != false || !slices.Equal(strs(a["blamed"]), []string{"2"}) || !strings.Contains(fmt.Sprint(a["evidence"]), "vet") {
		t.Fatalf("%v\n%s", a, stderr)
	}
	if rows := readRecord(t, rec); len(rows) != 3 || rows[2][2] != "vet" || rows[2][4] != "1" {
		t.Fatalf("record %v", rows)
	}
}

func TestPreflightBisectsByLine(t *testing.T) {
	// Line 2 adds bad.txt; the check fails wherever it exists.
	tr := newTrain(t, map[string]string{"one.txt": "1\n"}, map[string]string{"bad.txt": "x\n"})
	cfg := map[string]any{"steps": []any{
		map[string]any{"name": "nobad", "command": []string{"sh", "-c", "test ! -e bad.txt"}, "bisect": true},
	}}
	a, stderr := hook(t, "preflight.py", cfg, tr.input())
	if a["pass"] != false || !slices.Equal(strs(a["blamed"]), []string{"2"}) || !strings.Contains(fmt.Sprint(a["evidence"]), "bisection") {
		t.Fatalf("%v\n%s", a, stderr)
	}
	// The clone is left at the train's head, clean.
	if h := git(t, tr.dir, "rev-parse", "HEAD"); h != tr.tip2 {
		t.Fatalf("left at %s, not the head", h)
	}
	if st := git(t, tr.dir, "status", "--porcelain"); st != "" {
		t.Fatalf("left the tree changed: %s", st)
	}
	// A check that fails at the train's base too blames nobody.
	cfg["steps"] = []any{map[string]any{"name": "noreadme", "command": []string{"sh", "-c", "test ! -e README"}, "bisect": true}}
	a, _ = hook(t, "preflight.py", cfg, tr.input())
	if a["pass"] != false || len(strs(a["blamed"])) != 0 || !strings.Contains(fmt.Sprint(a["evidence"]), "trunk is broken") {
		t.Fatalf("%v", a)
	}
}

// TestPreflightBlameIsSetPerStep: "blame": "none" names nobody even when
// the output names a file one line touched; "files" (the default) names
// that line; an unknown mode is a configuration error, not an answer.
func TestPreflightBlameIsSetPerStep(t *testing.T) {
	tr := newTrain(t, map[string]string{"one.txt": "1\n"}, map[string]string{"src/b.go": "package src\n"})
	step := func(mode string) map[string]any {
		st := map[string]any{"name": "gate", "command": []string{"sh", "-c", "echo 'src/b.go:1:1: bad'; exit 1"}}
		if mode != "" {
			st["blame"] = mode
		}
		return map[string]any{"steps": []any{st}}
	}
	a, stderr := hook(t, "preflight.py", step("none"), tr.input())
	if a["pass"] != false || len(strs(a["blamed"])) != 0 || !strings.Contains(fmt.Sprint(a["evidence"]), "blame: none") {
		t.Fatalf("none: %v\n%s", a, stderr)
	}
	for _, mode := range []string{"", "files"} {
		if a, stderr := hook(t, "preflight.py", step(mode), tr.input()); !slices.Equal(strs(a["blamed"]), []string{"2"}) {
			t.Fatalf("%q: %v\n%s", mode, a, stderr)
		}
	}
	// An unknown mode: the hook exits non-zero with no answer, which
	// Shunter reads as no verdict.
	py := python(t)
	cfg := filepath.Join(t.TempDir(), "c.json")
	b, _ := json.Marshal(step("blame-everyone"))
	os.WriteFile(cfg, b, 0o644)
	in, _ := json.Marshal(tr.input())
	cmd := exec.Command(py, "-B", "preflight.py", "--config", cfg)
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.Output()
	if err == nil || len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("an unknown blame mode answered %q (%v)", out, err)
	}
}

func TestPreflightUsefulnessRule(t *testing.T) {
	tr := newTrain(t, map[string]string{"one.txt": "1\n"}, map[string]string{"two.txt": "2\n"})
	dir := t.TempDir()
	rec := filepath.Join(dir, "steps.tsv")
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	var b strings.Builder
	// 120 runs in the last week, none caught anything, median 3 s: skipped.
	// 120 runs of "cheap", median 1 s: kept. 50 runs of "rare": kept.
	for i := 0; i < 120; i++ {
		at := now.Add(-time.Duration(i) * time.Hour).Format("2006-01-02T15:04:05Z")
		fmt.Fprintf(&b, "%s\tt%d\tslow\t3.0\t0\n%s\tt%d\tcheap\t1.0\t0\n", at, i, at, i)
		if i < 50 {
			fmt.Fprintf(&b, "%s\tt%d\trare\t9.0\t0\n", at, i)
		}
	}
	if err := os.WriteFile(rec, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fail := []string{"sh", "-c", "exit 1"}
	steps := func(names ...string) []any {
		var out []any
		for _, n := range names {
			out = append(out, map[string]any{"name": n, "command": fail})
		}
		return out
	}
	env := fmt.Sprintf("SHUNTER_HOOK_NOW=%d", now.Unix())
	cfg := map[string]any{"record": rec, "steps": steps("slow"), "usefulness": map[string]any{"days": 7, "min_runs": 100, "min_seconds": 2}}
	a, stderr := hook(t, "preflight.py", cfg, tr.input(), env)
	if a["pass"] != true || !strings.Contains(stderr, "slow skipped") {
		t.Fatalf("a step that never caught anything ran: %v\n%s", a, stderr)
	}
	for _, name := range []string{"cheap", "rare"} {
		cfg["steps"] = steps(name)
		if a, _ := hook(t, "preflight.py", cfg, tr.input(), env); a["pass"] != false {
			t.Fatalf("%s was skipped", name)
		}
	}
	// Always run: kept whatever its record.
	cfg["steps"], cfg["always"] = steps("slow"), []string{"slow"}
	if a, _ := hook(t, "preflight.py", cfg, tr.input(), env); a["pass"] != false {
		t.Fatal("an always step was skipped")
	}
	// Recalled in the window: it runs again.
	delete(cfg, "always")
	recalled := filepath.Join(dir, "recalled.tsv")
	write(t, dir, "recalled.tsv", now.Add(-time.Hour).Format("2006-01-02T15:04:05Z")+"\tslow\tCI failed in what it covers\n")
	cfg["recalled"] = recalled
	if a, _ := hook(t, "preflight.py", cfg, tr.input(), env); a["pass"] != false {
		t.Fatal("a recalled step was skipped")
	}
	// Never: skipped whatever its record.
	delete(cfg, "recalled")
	cfg["steps"], cfg["never"] = steps("rare"), []string{"rare"}
	if a, _ := hook(t, "preflight.py", cfg, tr.input(), env); a["pass"] != true {
		t.Fatal("a never step ran")
	}
}

// conflictWorld is a clone in the middle of replaying a change onto the
// train: both changed the sources (cleanly) and the generated file
// (a conflict).
func conflictWorld(t *testing.T, both map[string]string, mine map[string]string) string {
	t.Helper()
	d := newRepo(t)
	commit(t, d, "root", map[string]string{
		".gitattributes": "gen.txt merge=regenerate\n",
		"a.src":          "a0\n", "b.src": "b0\n", "gen.txt": "a0\nb0\n", "other.txt": "o\n",
	})
	git(t, d, "checkout", "--quiet", "-b", "change")
	commit(t, d, "change", mine)
	git(t, d, "checkout", "--quiet", "main")
	commit(t, d, "train", both)
	if err := gitTry(d, "rebase", "--quiet", "main", "change"); err == nil {
		t.Fatal("the replay did not conflict")
	}
	return d
}

func TestConflictRegeneratesMarkedFilesOnly(t *testing.T) {
	regen := []string{"sh", "-c", "cat a.src b.src > gen.txt; echo touched >> other.txt"}
	d := conflictWorld(t,
		map[string]string{"a.src": "a1\n", "gen.txt": "a1\nb0\n"},
		map[string]string{"b.src": "b1\n", "gen.txt": "a0\nb1\n"})
	in := map[string]any{"dir": d, "train": "t1", "line": "7", "change": "7", "files": []string{"gen.txt"}}
	a, stderr := hook(t, "conflict.py", map[string]any{"command": regen}, in)
	if a["resolved"] != true {
		t.Fatalf("%v\n%s", a, stderr)
	}
	if !strings.Contains(stderr, "undone: other.txt") {
		t.Fatalf("a change to a file that is not generated was kept: %s", stderr)
	}
	if err := gitTry(d, "-c", "core.editor=true", "rebase", "--continue"); err != nil {
		t.Fatalf("the replay could not continue: %v", err)
	}
	if got := git(t, d, "show", "HEAD:gen.txt"); got != "a1\nb1" {
		t.Fatalf("gen.txt is %q: not regenerated from the merged sources", got)
	}
	if got := git(t, d, "show", "HEAD:b.src"); got != "b1" {
		t.Fatalf("the change's own source was lost: %q", got)
	}
	if got := git(t, d, "show", "HEAD:other.txt"); got != "o" {
		t.Fatalf("other.txt is %q", got)
	}
}

func TestConflictInAnUnmarkedFileIsAConflict(t *testing.T) {
	d := conflictWorld(t,
		map[string]string{"a.src": "a1\n", "gen.txt": "a1\nb0\n"},
		map[string]string{"a.src": "a2\n", "gen.txt": "a2\nb0\n"})
	in := map[string]any{"dir": d, "train": "t1", "line": "7", "change": "7", "files": []string{"a.src", "gen.txt"}}
	a, _ := hook(t, "conflict.py", map[string]any{"command": []string{"true"}}, in)
	if a["resolved"] != false || !strings.Contains(fmt.Sprint(a["reason"]), "a.src") {
		t.Fatalf("%v", a)
	}
	// A failing regenerate is not a resolution.
	in["files"] = []string{"gen.txt"}
	a, _ = hook(t, "conflict.py", map[string]any{"command": []string{"sh", "-c", "echo no generator; exit 2"}}, in)
	if a["resolved"] != false || !strings.Contains(fmt.Sprint(a["reason"]), "no generator") {
		t.Fatalf("%v", a)
	}
}
