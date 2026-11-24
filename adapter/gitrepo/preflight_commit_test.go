package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/hook"
	"github.com/rolandpj1968/shunter/state"
)

// commitWorld is a trunk with a generated file (gen.txt, from a.src and
// b.src) and a counters file (counters.txt), and two lines that each change
// one source, the second without regenerating: combined, with nothing in
// conflict, gen.txt is stale.
func commitWorld(t *testing.T, preflight string) (w *world, base state.Head, lines []adapter.BuildLine) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "preflight.sh")
	if err := os.WriteFile(p, []byte(preflight), 0o755); err != nil {
		t.Fatal(err)
	}
	w = newWorld(t, Config{})
	w.repo.cfg.Preflight = hook.Hook{Command: []string{p}, Dir: w.repo.cfg.Dir, Timeout: time.Minute, Env: ident}
	w.write("a.src", "a0\n")
	w.write("b.src", "b0\n")
	w.write("gen.txt", "a0\nb0\n")
	w.write("counters.txt", "9\n")
	run(t, w.dev, "add", ".")
	run(t, w.dev, "commit", "--quiet", "-m", "generated and counted")
	run(t, w.dev, "push", "--quiet", "origin", "main")
	base = w.trunk()
	h1 := w.change("1", "main", map[string]string{"a.src": "a1\n", "gen.txt": "a1\nb0\n"})
	h2 := w.change("2", "main", map[string]string{"b.src": "b1\n"})
	return w, base, []adapter.BuildLine{line("1", ch("1", h1)), line("2", ch("2", h2))}
}

// The two steps the old queue's pre-flight commits to its train: regenerate
// from the merged sources, and record what the combination improves.
const committingPreflight = `#!/bin/sh
cat > /dev/null
cat a.src b.src > gen.txt
git diff --quiet || git commit --quiet -am "regenerate the combined train" >&2 || exit 3
echo 7 > counters.txt
git diff --quiet || git commit --quiet -am "record what the train improves" >&2 || exit 3
echo '{"pass":true}'
`

// TestPreflightCommitReachesTheHead proves that a pre-flight hook may commit
// to the train: the built head is the hook's last commit, on top of every
// line; the draft shows the merged head while pre-flight runs; the train's
// branch, where CI runs, is moved to the built head; and that head is what
// lands.
func TestPreflightCommitReachesTheHead(t *testing.T) {
	w, base, lines := commitWorld(t, committingPreflight)
	var shown state.Head
	req := adapter.BuildRequest{Train: "t1", Base: base, Lines: lines, Merged: func(h state.Head) { shown = h }}
	res, err := w.repo.Build(ctx, "k1", req)
	if err != nil || res.PreflightFailed || res.Head == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if shown == "" || shown == res.Head {
		t.Fatalf("draft shown at %q, built head %s: want the merged head, before the hook's commits", shown, res.Head)
	}
	if got := run(t, w.repo.cfg.Dir, "rev-list", "--count", string(shown)+".."+string(res.Head)); got != "2" {
		t.Fatalf("%s commits from the merged head to the built head, want the hook's 2", got)
	}
	if !w.repo.ancestor(ctx, shown, res.Head) {
		t.Fatal("the built head does not hold the merged lines")
	}
	if got := w.cat(res.Head, "gen.txt"); got != "a1\nb1" {
		t.Fatalf("gen.txt at the built head is %q", got)
	}
	if got := w.cat(res.Head, "counters.txt"); got != "7" {
		t.Fatalf("counters.txt at the built head is %q", got)
	}
	branch := run(t, w.dev, "ls-remote", "origin", "refs/heads/"+w.repo.Branch("t1"))
	if !strings.HasPrefix(branch, string(res.Head)) {
		t.Fatalf("the train's branch is %q, want the built head %s", branch, res.Head)
	}
	if err := w.repo.Push(ctx, "p1", res.Head, base); err != nil || w.trunk() != res.Head {
		t.Fatalf("landing the built head: %v", err)
	}
}

// TestPreflightMayNotDropALine: a hook's commits go on top of the train. A
// head that no longer holds the merged lines (reset, rebased, amended) would
// land without a line the train says rode, so it is refused: no verdict.
func TestPreflightMayNotDropALine(t *testing.T) {
	for name, body := range map[string]string{
		"reset":   "git reset --quiet --hard HEAD~1 >&2",
		"amended": "echo x > x.txt && git add x.txt && git commit --quiet --amend --no-edit >&2",
	} {
		t.Run(name, func(t *testing.T) {
			w, base, lines := commitWorld(t, "#!/bin/sh\ncat > /dev/null\n"+body+" || exit 3\necho '{\"pass\":true}'\n")
			res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: lines})
			if err == nil || !adapter.IsPermanent(err) {
				t.Fatalf("a pre-flight that rewrote the train: %+v %v", res, err)
			}
		})
	}
}

// TestPreflightMayNotMergeIn: a hook's commits are its own. A merge would
// bring in history no line of the train holds, so it is refused.
func TestPreflightMayNotMergeIn(t *testing.T) {
	w, base, lines := commitWorld(t, "#!/bin/sh\ncat > /dev/null\ngit merge --quiet --no-edit refs/shunter/changes/3 >&2 || exit 3\necho '{\"pass\":true}'\n")
	w.change("3", "main", map[string]string{"three.txt": "3\n"})
	run(t, w.repo.cfg.Dir, "fetch", "--quiet", "origin", "refs/pull/3/head:refs/shunter/changes/3")
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: lines})
	if err == nil || !adapter.IsPermanent(err) {
		t.Fatalf("a pre-flight that merged another change in: %+v %v", res, err)
	}
}

// The example pre-flight hook (hooks/preflight.py) with the two steps that
// commit, driven by the adapter: the regeneration and the counters reach
// the built head, which is the one CI judges and trunk takes.
func TestExamplePreflightCommits(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	cfg := filepath.Join(t.TempDir(), "preflight.json")
	steps := `{"steps": [
	 {"name": "regenerate", "command": ["sh", "-c", "cat a.src b.src > gen.txt"], "commit": {"message": "regenerate the combined train", "paths": ["gen.txt"]}},
	 {"name": "counters", "command": ["sh", "-c", "echo 7 > counters.txt"], "commit": {"message": "record what the train improves", "paths": ["counters.txt"]}}]}`
	if err := os.WriteFile(cfg, []byte(steps), 0o644); err != nil {
		t.Fatal(err)
	}
	w, base, lines := commitWorld(t, "#!/bin/sh\nexit 9\n")
	script, _ := filepath.Abs("../../hooks/preflight.py")
	w.repo.cfg.Preflight = hook.Hook{Command: []string{py, "-B", script, "--config", cfg}, Dir: w.repo.cfg.Dir, Timeout: time.Minute, Env: ident}
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: lines})
	if err != nil || res.PreflightFailed || res.Head == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := w.cat(res.Head, "gen.txt"); got != "a1\nb1" {
		t.Fatalf("gen.txt at the built head is %q", got)
	}
	if got := w.cat(res.Head, "counters.txt"); got != "7" {
		t.Fatalf("counters.txt at the built head is %q", got)
	}
	if err := w.repo.Push(ctx, "p1", res.Head, base); err != nil || w.trunk() != res.Head {
		t.Fatalf("landing the built head: %v", err)
	}
}
