package gitrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/bound"
	"github.com/rolandpj1968/shunter/adapter/hook"
	"github.com/rolandpj1968/shunter/state"
)

var ident = []string{
	"GIT_AUTHOR_NAME=Test Producer", "GIT_AUTHOR_EMAIL=producer@example.com",
	"GIT_COMMITTER_NAME=Test Producer", "GIT_COMMITTER_EMAIL=producer@example.com",
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
}

// world is a remote (bare), a producer's clone that writes changes, and the
// adapter's own clone.
type world struct {
	t      *testing.T
	remote string
	dev    string
	repo   *Repo
}

func run(t *testing.T, dir string, args ...string) string {
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

func newWorld(t *testing.T, cfg Config) *world {
	t.Helper()
	root := t.TempDir()
	w := &world{t: t, remote: filepath.Join(root, "remote.git"), dev: filepath.Join(root, "dev")}
	run(t, root, "init", "--quiet", "--bare", "--initial-branch=main", w.remote)
	run(t, root, "init", "--quiet", "--initial-branch=main", w.dev)
	w.write("README", "hello\n")
	w.write("a.txt", "a1\na2\na3\n")
	run(t, w.dev, "add", ".")
	run(t, w.dev, "commit", "--quiet", "-m", "root")
	run(t, w.dev, "remote", "add", "origin", w.remote)
	run(t, w.dev, "push", "--quiet", "origin", "main")
	clone := filepath.Join(root, "shunter")
	run(t, root, "clone", "--quiet", w.remote, clone)
	cfg.Dir, cfg.Trunk = clone, "main"
	cfg.Env = ident
	cfg.Policy = bound.Policy{Attempts: 2, Backoff: time.Millisecond}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w.repo = r
	return w
}

func (w *world) write(name, body string) {
	if err := os.WriteFile(filepath.Join(w.dev, name), []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// change makes a change on a branch from base (a branch or sha), publishes
// it as refs/pull/<id>/head, and returns its head.
func (w *world) change(id, from string, files map[string]string) state.Head {
	run(w.t, w.dev, "checkout", "--quiet", "-B", "c"+id, from)
	for f, b := range files {
		w.write(f, b)
	}
	run(w.t, w.dev, "add", ".")
	run(w.t, w.dev, "commit", "--quiet", "-m", "change "+id)
	head := run(w.t, w.dev, "rev-parse", "HEAD")
	run(w.t, w.dev, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/c"+id, "HEAD:refs/pull/"+id+"/head")
	return state.Head(head)
}

func (w *world) trunk() state.Head {
	return state.Head(run(w.t, w.dev, "ls-remote", "origin", "refs/heads/main")[:40])
}

func line(id string, changes ...state.Change) adapter.BuildLine {
	return adapter.BuildLine{ID: state.LineID(id), Changes: changes}
}

func ch(id string, h state.Head) state.Change { return state.Change{ID: state.ChangeID(id), Head: h} }

func (w *world) cat(head state.Head, file string) string {
	run(w.t, w.dev, "fetch", "--quiet", "origin", "+refs/heads/*:refs/remotes/origin/*")
	return run(w.t, w.dev, "show", string(head)+":"+file)
}

var ctx = context.Background()

func TestBuildThenLandByFastForward(t *testing.T) {
	w := newWorld(t, Config{})
	base := w.trunk()
	h1 := w.change("1", "main", map[string]string{"one.txt": "1\n"})
	h2 := w.change("2", "main", map[string]string{"two.txt": "2\n"})
	h3 := w.change("3", "c2", map[string]string{"three.txt": "3\n"}) // stacked on 2
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{
		line("1", ch("1", h1)), line("2+3", ch("2", h2), ch("3", h3)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Head == "" || len(res.LeftOut) != 0 || res.PreflightFailed {
		t.Fatalf("result %+v", res)
	}
	for _, f := range []string{"one.txt", "two.txt", "three.txt", "README"} {
		w.cat(res.Head, f)
	}
	// Linear: no merge commits between base and head.
	if merges := run(t, w.dev, "rev-list", "--merges", string(base)+".."+string(res.Head)); merges != "" {
		t.Fatalf("merge commits on a rebase build: %s", merges)
	}
	if n := run(t, w.dev, "rev-list", "--count", string(base)+".."+string(res.Head)); n != "3" {
		t.Fatalf("%s commits, want 3", n)
	}
	// The train's branch is on the remote at the head.
	if got := run(t, w.dev, "ls-remote", "origin", "refs/heads/shunter/t1"); !strings.HasPrefix(got, string(res.Head)) {
		t.Fatalf("train branch %q, want %s", got, res.Head)
	}
	// The same key gives the same result, without building again.
	again, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base})
	if err != nil || again.Head != res.Head {
		t.Fatalf("again: %+v %v", again, err)
	}
	// Land: trunk moves to exactly the head.
	if err := w.repo.Push(ctx, "p1", res.Head, base); err != nil {
		t.Fatal(err)
	}
	if w.trunk() != res.Head {
		t.Fatalf("trunk %s, want %s", w.trunk(), res.Head)
	}
	// Pushing again is success: it took.
	if err := w.repo.Push(ctx, "p1", res.Head, base); err != nil {
		t.Fatalf("a push that took already: %v", err)
	}
}

func TestTrainTwoOnTrainOne(t *testing.T) {
	w := newWorld(t, Config{})
	base := w.trunk()
	h1 := w.change("1", "main", map[string]string{"one.txt": "1\n"})
	h2 := w.change("2", "main", map[string]string{"two.txt": "2\n"})
	r1, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{line("1", ch("1", h1))}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := w.repo.Build(ctx, "k2", adapter.BuildRequest{Train: "t2", Base: r1.Head, Lines: []adapter.BuildLine{line("2", ch("2", h2))}})
	if err != nil {
		t.Fatal(err)
	}
	if run(t, w.dev, "fetch", "--quiet", "origin"); run(t, w.dev, "rev-parse", string(r2.Head)+"^") != string(r1.Head) {
		t.Fatal("train 2 is not built on train 1's exact head (I5)")
	}
	// Train 2 cannot land before train 1: trunk is not at its base.
	if err := w.repo.Push(ctx, "p2", r2.Head, r1.Head); !adapter.IsPermanent(err) {
		t.Fatalf("train 2 first: %v, want a permanent refusal", err)
	}
	if err := w.repo.Push(ctx, "p1", r1.Head, base); err != nil {
		t.Fatal(err)
	}
	if err := w.repo.Push(ctx, "p2", r2.Head, r1.Head); err != nil {
		t.Fatal(err)
	}
	if w.trunk() != r2.Head {
		t.Fatal("trunk is not at train 2's head")
	}
}

func TestConflictLeavesTheLineOut(t *testing.T) {
	w := newWorld(t, Config{})
	base := w.trunk()
	h1 := w.change("1", "main", map[string]string{"a.txt": "a1\nONE\na3\n"})
	h2 := w.change("2", "main", map[string]string{"a.txt": "a1\nTWO\na3\n"})
	h3 := w.change("3", "main", map[string]string{"three.txt": "3\n"})
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{
		line("1", ch("1", h1)), line("2", ch("2", h2)), line("3", ch("3", h3)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	why := res.LeftOut["2"]
	if !strings.Contains(why, "conflict") || !strings.Contains(why, "a.txt") || len(res.LeftOut) != 1 {
		t.Fatalf("left out %v", res.LeftOut)
	}
	if got := w.cat(res.Head, "a.txt"); got != "a1\nONE\na3" {
		t.Fatalf("a.txt %q", got)
	}
	w.cat(res.Head, "three.txt")
}

func TestConflictHookResolves(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "resolve.sh")
	// Takes the train's side of every conflicted file, stages it, and says so.
	body := "#!/bin/sh\nset -e\ncat > /dev/null\ngit checkout --ours -- a.txt\ngit add a.txt\necho '{\"resolved\":true}'\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	w := newWorld(t, Config{})
	w.repo.cfg.Conflict = hook.Hook{Command: []string{p}, Dir: w.repo.cfg.Dir, Env: ident, Timeout: time.Minute}
	base := w.trunk()
	h1 := w.change("1", "main", map[string]string{"a.txt": "a1\nONE\na3\n"})
	h2 := w.change("2", "main", map[string]string{"a.txt": "a1\nTWO\na3\n", "two.txt": "2\n"})
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{
		line("1", ch("1", h1)), line("2", ch("2", h2)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.LeftOut) != 0 || res.Head == "" {
		t.Fatalf("result %+v", res)
	}
	w.cat(res.Head, "two.txt")
}

func TestMovedHeadIsLeftOut(t *testing.T) {
	w := newWorld(t, Config{})
	base := w.trunk()
	h1 := w.change("1", "main", map[string]string{"one.txt": "1\n"})
	w.change("1", "c1", map[string]string{"one.txt": "1 again\n"}) // pushed to after the offer
	h2 := w.change("2", "main", map[string]string{"two.txt": "2\n"})
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{
		line("1", ch("1", h1)), line("2", ch("2", h2)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.LeftOut["1"], "moved") || res.Head == "" {
		t.Fatalf("result %+v", res)
	}
}

func TestEveryLineLeftOutIsNoHead(t *testing.T) {
	w := newWorld(t, Config{})
	base := w.trunk()
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{
		line("9", ch("9", "0123456789012345678901234567890123456789")),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Head != "" || res.LeftOut["9"] == "" {
		t.Fatalf("result %+v", res)
	}
}

func TestPreflightHook(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "preflight.sh")
	// Fails when the tree has bad.txt, and blames line 2 (and a line that
	// is not in the train, which must be ignored).
	body := "#!/bin/sh\ncat > /dev/null\nif [ -e bad.txt ]; then echo '{\"pass\":false,\"blamed\":[\"2\",\"77\"],\"evidence\":\"bad.txt is bad\"}'; exit 1; fi\necho '{\"pass\":true}'\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	w := newWorld(t, Config{})
	w.repo.cfg.Preflight = hook.Hook{Command: []string{p}, Dir: w.repo.cfg.Dir, Timeout: time.Minute}
	base := w.trunk()
	h1 := w.change("1", "main", map[string]string{"one.txt": "1\n"})
	h2 := w.change("2", "main", map[string]string{"bad.txt": "x\n"})
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{line("1", ch("1", h1)), line("2", ch("2", h2))}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.PreflightFailed || len(res.Blamed) != 1 || res.Blamed[0] != "2" || res.Evidence != "bad.txt is bad" {
		t.Fatalf("result %+v", res)
	}
	res, err = w.repo.Build(ctx, "k2", adapter.BuildRequest{Train: "t2", Base: base, Lines: []adapter.BuildLine{line("1", ch("1", h1))}})
	if err != nil || res.PreflightFailed || res.Head == "" {
		t.Fatalf("result %+v %v", res, err)
	}
	// A pre-flight that crashes has caught nothing: an error, never a
	// failure charged to a line.
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err = w.repo.Build(ctx, "k3", adapter.BuildRequest{Train: "t3", Base: base, Lines: []adapter.BuildLine{line("2", ch("2", h2))}})
	if err == nil || res.PreflightFailed {
		t.Fatalf("a crashed pre-flight: %+v %v", res, err)
	}
}

func TestLeaseRefusedWhenTrunkMoved(t *testing.T) {
	w := newWorld(t, Config{})
	base := w.trunk()
	h1 := w.change("1", "main", map[string]string{"one.txt": "1\n"})
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{line("1", ch("1", h1))}})
	if err != nil {
		t.Fatal(err)
	}
	// Another writer moves trunk (a breach of I7).
	run(t, w.dev, "checkout", "--quiet", "main")
	w.write("other.txt", "o\n")
	run(t, w.dev, "add", ".")
	run(t, w.dev, "commit", "--quiet", "-m", "another writer")
	run(t, w.dev, "push", "--quiet", "origin", "main")
	moved := w.trunk()
	err = w.repo.Push(ctx, "p1", res.Head, base)
	if !adapter.IsPermanent(err) || !strings.Contains(err.Error(), "not a fast-forward") {
		t.Fatalf("push over a moved trunk: %v", err)
	}
	if w.trunk() != moved {
		t.Fatal("trunk changed")
	}
}

func TestRemoteRefusalIsPermanent(t *testing.T) {
	// The remote refuses the push by a rule: permanent, and trunk stays.
	w := newWorld(t, Config{})
	base := w.trunk()
	h1 := w.change("1", "main", map[string]string{"one.txt": "1\n"})
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{line("1", ch("1", h1))}})
	if err != nil {
		t.Fatal(err)
	}
	hookp := filepath.Join(w.remote, "hooks", "pre-receive")
	if err := os.WriteFile(hookp, []byte("#!/bin/sh\necho 'declined due to repository rule violations' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err = w.repo.Push(ctx, "p1", res.Head, base)
	if !adapter.IsPermanent(err) || w.trunk() != base {
		t.Fatalf("a refused push: %v; trunk %s", err, w.trunk())
	}
}

func TestPushRefusesANonDescendant(t *testing.T) {
	w := newWorld(t, Config{})
	base := w.trunk()
	// A head built on something other than trunk: not a fast-forward of it.
	run(t, w.dev, "checkout", "--quiet", "--orphan", "elsewhere")
	w.write("x.txt", "x\n")
	run(t, w.dev, "add", ".")
	run(t, w.dev, "commit", "--quiet", "-m", "unrelated")
	run(t, w.dev, "push", "--quiet", "origin", "HEAD:refs/heads/shunter/tx")
	head := state.Head(run(t, w.dev, "rev-parse", "HEAD"))
	err := w.repo.Push(ctx, "p1", head, base)
	if !adapter.IsPermanent(err) || w.trunk() != base {
		t.Fatalf("a non-descendant: %v", err)
	}
}

func TestMergeStrategy(t *testing.T) {
	w := newWorld(t, Config{Strategy: "merge"})
	base := w.trunk()
	h1 := w.change("1", "main", map[string]string{"one.txt": "1\n"})
	h2 := w.change("2", "main", map[string]string{"two.txt": "2\n"})
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{line("1", ch("1", h1)), line("2", ch("2", h2))}})
	if err != nil {
		t.Fatal(err)
	}
	run(t, w.dev, "fetch", "--quiet", "origin", "+refs/heads/*:refs/remotes/origin/*")
	if n := run(t, w.dev, "rev-list", "--merges", "--count", string(base)+".."+string(res.Head)); n != "2" {
		t.Fatalf("%s merge commits, want 2", n)
	}
	// Both change heads are in the train, unchanged (merge keeps them).
	run(t, w.dev, "merge-base", "--is-ancestor", string(h1), string(res.Head))
	run(t, w.dev, "merge-base", "--is-ancestor", string(h2), string(res.Head))
}

func TestPushOfUnknownOutcomeIsSettledByReadingTrunk(t *testing.T) {
	// A git that pushes, then loses the answer: the push took, and git
	// says it failed.
	p := filepath.Join(t.TempDir(), "lossy-git")
	body := "#!/bin/sh\nif [ \"$1\" = push ]; then git \"$@\" >/dev/null 2>&1; echo 'fatal: the remote end hung up unexpectedly' >&2; exit 128; fi\nexec git \"$@\"\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	w := newWorld(t, Config{})
	base := w.trunk()
	h1 := w.change("1", "main", map[string]string{"one.txt": "1\n"})
	w.repo.cfg.Git = p
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{line("1", ch("1", h1))}})
	if err == nil {
		t.Fatalf("the train branch's push lost its answer and the build said ok: %+v", res)
	}
	w.repo.cfg.Git = "git"
	res, err = w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: []adapter.BuildLine{line("1", ch("1", h1))}})
	if err != nil {
		t.Fatal(err)
	}
	w.repo.cfg.Git = p
	if err := w.repo.Push(ctx, "p1", res.Head, base); err != nil {
		t.Fatalf("a push that took but lost its answer: %v", err)
	}
	if w.trunk() != res.Head {
		t.Fatal("trunk did not move")
	}
}
