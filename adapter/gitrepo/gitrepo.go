// Package gitrepo is the git adapter: it builds trains in a local clone and
// moves trunk on the remote, by running the git command.
//
// A build checks out the train's base (trunk's tip, or the head of the train
// it is built on), applies each line's changes in queue order, runs the
// pre-flight hook, and pushes the result to the train's branch. A line whose
// changes conflict with the lines before it is left out, with the files
// named, unless the conflict hook resolves it. Two strategies apply a change:
// "rebase" replays its own commits (a linear trunk; the default), "merge"
// merges its head with a merge commit.
//
// A landing is a push of the train's exact head to trunk, with a lease on
// the trunk the train was built on, and only when that head descends from
// it: a fast-forward, never a merge commit and never a fallback (§5.7). A
// push of unknown outcome is settled by reading trunk: at the head, it took.
package gitrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/bound"
	"github.com/rolandpj1968/shunter/adapter/hook"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/store"
	"github.com/rolandpj1968/shunter/trunklock"
)

// Config is the adapter's configuration.
type Config struct {
	// Dir is a clone the adapter owns: nothing else may work in it.
	Dir string
	// Remote is the remote's name in the clone. Default "origin".
	Remote string
	// Trunk is the branch trains land on.
	Trunk string
	// BranchPrefix names train branches: BranchPrefix + train id.
	BranchPrefix string
	// ChangeRef is the remote ref of a change, with {id} for its id.
	// Default "refs/pull/{id}/head".
	ChangeRef string
	// Strategy is "rebase" (default) or "merge".
	Strategy string
	// Preflight runs on the assembled train. Optional: none passes.
	Preflight hook.Hook
	// Conflict may resolve a conflict. Optional: none leaves the line out.
	Conflict hook.Hook
	// Policy bounds each network call (fetch, ls-remote, push).
	Policy bound.Policy
	// Env is added to git's environment: the committer, say.
	Env []string
	// StateDir keeps the results of builds by key. Default: in the clone's
	// git directory.
	StateDir string
	// Git is the git program. Default "git".
	Git string
	// TrunkLock is the trunk push lock's file (package trunklock), which
	// every pusher of trunk holds to push. Empty: no lock.
	TrunkLock string
	// TrunkLockWait bounds the wait for the lock in one push. Default 2m.
	TrunkLockWait time.Duration
	// TrunkLockMaxHold is how long a holder may hold it before the lock is
	// stale. Default 15m.
	TrunkLockMaxHold time.Duration
}

// Repo is the git adapter. It implements adapter.Git.
type Repo struct {
	cfg  Config
	work sync.Mutex // the working tree: one build at a time
	net  sync.Mutex // fetches and pushes: one at a time
}

// New returns the adapter over an existing clone.
func New(cfg Config) (*Repo, error) {
	if cfg.Dir == "" || cfg.Trunk == "" {
		return nil, errors.New("gitrepo: dir and trunk are required")
	}
	if cfg.Remote == "" {
		cfg.Remote = "origin"
	}
	if cfg.BranchPrefix == "" {
		cfg.BranchPrefix = "shunter/"
	}
	if cfg.ChangeRef == "" {
		cfg.ChangeRef = "refs/pull/{id}/head"
	}
	switch cfg.Strategy {
	case "":
		cfg.Strategy = "rebase"
	case "rebase", "merge":
	default:
		return nil, fmt.Errorf("gitrepo: strategy %q: rebase or merge", cfg.Strategy)
	}
	if cfg.Git == "" {
		cfg.Git = "git"
	}
	if cfg.Policy.Timeout <= 0 {
		cfg.Policy.Timeout = 5 * time.Minute
	}
	r := &Repo{cfg: cfg}
	if _, err := r.git(context.Background(), "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("gitrepo: %s is not a clone: %w", cfg.Dir, err)
	}
	if r.cfg.StateDir == "" {
		gd, err := r.git(context.Background(), "rev-parse", "--absolute-git-dir")
		if err != nil {
			return nil, err
		}
		r.cfg.StateDir = filepath.Join(gd, "shunter")
	}
	if err := os.MkdirAll(filepath.Join(r.cfg.StateDir, "builds"), 0o755); err != nil {
		return nil, err
	}
	return r, nil
}

// Branch is the branch of a train.
func (r *Repo) Branch(t state.TrainID) string { return r.cfg.BranchPrefix + string(t) }

// git runs git in the clone and returns its trimmed output.
func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.cfg.Git, args...)
	cmd.Dir = r.cfg.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_EDITOR=true", "GIT_MERGE_AUTOEDIT=no")
	cmd.Env = append(cmd.Env, r.cfg.Env...)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		return string(out), &bound.GitError{Args: args, Code: code, Output: string(out)}
	}
	return strings.TrimSpace(string(out)), nil
}

// netgit runs a network command, bounded and retried.
func (r *Repo) netgit(ctx context.Context, args ...string) (string, error) {
	var out string
	err := bound.Do(ctx, r.cfg.Policy, "git "+args[0], func(ctx context.Context) error {
		var err error
		out, err = r.git(ctx, args...)
		return err
	})
	return out, err
}

func (r *Repo) changeRef(id state.ChangeID) string {
	return strings.ReplaceAll(r.cfg.ChangeRef, "{id}", string(id))
}

func localChangeRef(id state.ChangeID) string { return "refs/shunter/changes/" + string(id) }

// fetch brings trunk, the train branches and the given changes. A change
// whose ref the remote does not have is skipped: its line is left out when
// its head is not found.
func (r *Repo) fetch(ctx context.Context, changes []state.ChangeID) error {
	r.net.Lock()
	defer r.net.Unlock()
	return r.fetchLocked(ctx, changes)
}

func (r *Repo) fetchLocked(ctx context.Context, changes []state.ChangeID) error {
	rm := r.cfg.Remote
	base := []string{"fetch", "--quiet", "--no-tags", "--prune", rm,
		fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", r.cfg.Trunk, rm, r.cfg.Trunk),
		fmt.Sprintf("+refs/heads/%s*:refs/remotes/%s/%s*", r.cfg.BranchPrefix, rm, r.cfg.BranchPrefix)}
	args := slices.Clone(base)
	for _, id := range changes {
		args = append(args, "+"+r.changeRef(id)+":"+localChangeRef(id))
	}
	_, err := r.netgit(ctx, args...)
	if err == nil || !adapter.IsPermanent(err) || len(changes) == 0 {
		return err
	}
	if _, err := r.netgit(ctx, base...); err != nil {
		return err
	}
	for _, id := range changes {
		r.git(ctx, "fetch", "--quiet", "--no-tags", rm, "+"+r.changeRef(id)+":"+localChangeRef(id))
	}
	return nil
}

func (r *Repo) has(ctx context.Context, h state.Head) bool {
	if h == "" {
		return false
	}
	_, err := r.git(ctx, "cat-file", "-e", string(h)+"^{commit}")
	return err == nil
}

// PreflightInput is what the pre-flight hook reads.
type PreflightInput struct {
	Dir   string              `json:"dir"`
	Train state.TrainID       `json:"train"`
	Base  state.Head          `json:"base"`
	Head  state.Head          `json:"head"`
	Lines []adapter.BuildLine `json:"lines"`
	// Tips is each line's last commit in the train, in the order of Lines:
	// the train as it stood after that line. A hook bisects with it, and
	// reads each line's files in the train as the difference from the tip
	// before (Base for the first).
	Tips map[state.LineID]state.Head `json:"tips"`
}

// PreflightAnswer is the pre-flight hook's answer. A failure may name the
// lines it charges; it is never a verdict on CI's behalf.
//
// LeftOut leaves lines out of the train, each with a reason: they didn't fit
// this train (they don't build on it, or their generated parts clash with
// another line's), which is not a charge. It is taken as a conflict is
// (§5.3): the train is built again without them, pre-flight runs again on
// what is left, and the rest of this answer is not read. Lines not in the
// train are ignored.
type PreflightAnswer struct {
	Pass     bool                    `json:"pass"`
	Blamed   []state.LineID          `json:"blamed,omitempty"`
	Evidence string                  `json:"evidence,omitempty"`
	LeftOut  map[state.LineID]string `json:"left_out,omitempty"`
}

// ConflictInput is what the conflict hook reads. The working tree is in the
// middle of the merge or the replay, with the conflicts in Files.
type ConflictInput struct {
	Dir    string         `json:"dir"`
	Train  state.TrainID  `json:"train"`
	Line   state.LineID   `json:"line"`
	Change state.ChangeID `json:"change"`
	Files  []string       `json:"files"`
}

// ConflictAnswer is the conflict hook's answer: resolved, with every
// resolution staged, or not, with a reason.
type ConflictAnswer struct {
	Resolved bool   `json:"resolved"`
	Reason   string `json:"reason,omitempty"`
}

// cached results, by effect key.
func (r *Repo) cachePath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(r.cfg.StateDir, "builds", hex.EncodeToString(sum[:12])+".json")
}

type cached struct {
	Key    string              `json:"key"`
	Result adapter.BuildResult `json:"result"`
}

func (r *Repo) cached(key string) (adapter.BuildResult, bool) {
	b, err := os.ReadFile(r.cachePath(key))
	if err != nil {
		return adapter.BuildResult{}, false
	}
	var c cached
	if json.Unmarshal(b, &c) != nil || c.Key != key {
		return adapter.BuildResult{}, false
	}
	return c.Result, true
}

func (r *Repo) remember(key string, res adapter.BuildResult) error {
	b, err := json.Marshal(cached{Key: key, Result: res})
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(r.cachePath(key), b)
}

// Build implements adapter.Git. The same key returns the same result: it is
// kept on disk once the train's branch is pushed.
func (r *Repo) Build(ctx context.Context, key string, req adapter.BuildRequest) (adapter.BuildResult, error) {
	if res, ok := r.cached(key); ok {
		return res, nil
	}
	r.work.Lock()
	defer r.work.Unlock()
	var ids []state.ChangeID
	for _, l := range req.Lines {
		for _, c := range l.Changes {
			ids = append(ids, c.ID)
		}
	}
	if err := r.fetch(ctx, ids); err != nil {
		return adapter.BuildResult{}, err
	}
	if !r.has(ctx, req.Base) {
		return adapter.BuildResult{}, fmt.Errorf("the base %s is not in the clone after a fetch", req.Base)
	}
	// Each round builds the train from its base, without the lines left
	// out so far, and runs pre-flight on it. A pre-flight that leaves lines
	// out starts another round without them, so that the head never holds a
	// line the train doesn't: at most one round a line, and one more.
	out := map[state.LineID]string{}
	var res adapter.BuildResult
	var head string
	for {
		var err error
		var again bool
		res, head, again, err = r.round(ctx, req, out)
		if err != nil {
			return adapter.BuildResult{}, err
		}
		if !again {
			break
		}
	}
	if head == "" {
		return res, r.remember(key, res)
	}
	if res.PreflightFailed {
		return res, r.remember(key, res)
	}
	var err error
	r.net.Lock()
	_, err = r.netgit(ctx, "push", "--quiet", "--no-verify", "--force", r.cfg.Remote, head+":refs/heads/"+r.Branch(req.Train))
	r.net.Unlock()
	if err != nil {
		return adapter.BuildResult{}, err
	}
	res.Head = state.Head(head)
	return res, r.remember(key, res)
}

// round builds the train on its base without the lines in out, and runs
// pre-flight on it. It adds to out the lines it leaves out. again says that
// pre-flight left lines out of this build, which must be built again
// without them. An empty head with no error is a train with every line
// left out.
func (r *Repo) round(ctx context.Context, req adapter.BuildRequest, out map[state.LineID]string) (res adapter.BuildResult, head string, again bool, err error) {
	r.reset(ctx)
	if _, err := r.git(ctx, "checkout", "--quiet", "--detach", "--force", string(req.Base)); err != nil {
		return res, "", false, err
	}
	if _, err := r.git(ctx, "clean", "-fdq"); err != nil {
		return res, "", false, err
	}
	res = adapter.BuildResult{LeftOut: map[state.LineID]string{}}
	var kept []adapter.BuildLine
	tips := map[state.LineID]state.Head{}
	for _, l := range req.Lines {
		if why, ok := out[l.ID]; ok {
			res.LeftOut[l.ID] = why
			continue
		}
		why, err := r.apply(ctx, req.Train, l)
		if err != nil {
			r.reset(ctx)
			return res, "", false, err
		}
		if why != "" {
			res.LeftOut[l.ID], out[l.ID] = why, why
			continue
		}
		kept = append(kept, l)
		tip, err := r.git(ctx, "rev-parse", "HEAD")
		if err != nil {
			return res, "", false, err
		}
		tips[l.ID] = state.Head(tip)
	}
	if len(kept) == 0 {
		return res, "", false, nil
	}
	if head, err = r.git(ctx, "rev-parse", "HEAD"); err != nil {
		return res, "", false, err
	}
	if !r.cfg.Preflight.Configured() {
		return res, head, false, nil
	}
	in := PreflightInput{Dir: r.cfg.Dir, Train: req.Train, Base: req.Base, Head: state.Head(head), Lines: kept, Tips: tips}
	var a PreflightAnswer
	hr, err := r.cfg.Preflight.Run(ctx, in, &a)
	if err != nil {
		// A pre-flight that crashed, hung or said nothing has caught
		// nothing: no verdict, never a charge (§5.5 class 0).
		return res, "", false, fmt.Errorf("pre-flight gave no answer: %w %s", err, hr.Stderr)
	}
	// A leave-out comes first: "didn't fit this train" (§5.3). The rest of
	// the answer was about a train that will not exist, so it is not read.
	for _, l := range kept {
		if why, ok := a.LeftOut[l.ID]; ok {
			if why == "" {
				why = "didn't fit this train"
			}
			out[l.ID], again = "left out by pre-flight: "+why, true
		}
	}
	if again {
		return res, head, true, nil
	}
	switch {
	case !a.Pass:
		res.PreflightFailed = true
		for _, id := range a.Blamed {
			if slices.ContainsFunc(kept, func(l adapter.BuildLine) bool { return l.ID == id }) {
				res.Blamed = append(res.Blamed, id)
			}
		}
		res.Evidence = a.Evidence
		return res, head, false, nil
	case hr.Code != 0:
		return res, "", false, fmt.Errorf("pre-flight answered pass with exit %d", hr.Code)
	}
	if st, err := r.git(ctx, "status", "--porcelain"); err != nil || st != "" {
		return res, "", false, adapter.Permanent(fmt.Errorf("pre-flight left the tree changed and uncommitted: %s", st))
	}
	head, err = r.git(ctx, "rev-parse", "HEAD")
	return res, head, false, err
}

// reset leaves the clone with nothing in progress.
func (r *Repo) reset(ctx context.Context) {
	r.git(ctx, "rebase", "--abort")
	r.git(ctx, "merge", "--abort")
	r.git(ctx, "cherry-pick", "--abort")
	r.git(ctx, "reset", "--quiet", "--hard")
}

// apply puts one line on HEAD. It returns why the line is left out, or "".
func (r *Repo) apply(ctx context.Context, t state.TrainID, l adapter.BuildLine) (string, error) {
	start, err := r.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	back := func(why string) (string, error) {
		r.reset(ctx)
		if _, err := r.git(ctx, "checkout", "--quiet", "--detach", "--force", start); err != nil {
			return "", err
		}
		return why, nil
	}
	var prev state.Head
	for i, c := range l.Changes {
		if !r.has(ctx, c.Head) {
			return back(fmt.Sprintf("change %s: its head %s is not on the forge (pushed to since it was offered?)", c.ID, short(c.Head)))
		}
		if got, err := r.git(ctx, "rev-parse", "--verify", "--quiet", localChangeRef(c.ID)); err == nil && got != string(c.Head) {
			return back(fmt.Sprintf("change %s: its head moved from %s to %s", c.ID, short(c.Head), short(state.Head(got))))
		}
		cur, err := r.git(ctx, "rev-parse", "HEAD")
		if err != nil {
			return "", err
		}
		var args []string
		var cont []string
		if r.cfg.Strategy == "merge" {
			args = []string{"merge", "--no-ff", "--no-edit", "-m", fmt.Sprintf("Merge change %s (line %s)", c.ID, l.ID), string(c.Head)}
			cont = []string{"commit", "--no-edit"}
		} else {
			upstream := string(prev)
			if i == 0 {
				if upstream, err = r.git(ctx, "merge-base", cur, string(c.Head)); err != nil {
					return back(fmt.Sprintf("change %s has no history in common with the train", c.ID))
				}
			}
			args = []string{"-c", "core.hooksPath=/dev/null", "rebase", "--quiet", "--no-autosquash", "--onto", cur, upstream, string(c.Head)}
			cont = []string{"-c", "core.hooksPath=/dev/null", "rebase", "--continue"}
		}
		_, err = r.git(ctx, args...)
		for tries := 0; err != nil && tries < 50; tries++ {
			var ge *bound.GitError
			if !errors.As(err, &ge) {
				return "", err
			}
			files, _ := r.git(ctx, "diff", "--name-only", "--diff-filter=U")
			if files == "" {
				return back(fmt.Sprintf("change %s could not be applied: %s", c.ID, firstLine(ge.Output)))
			}
			fs := strings.Fields(files)
			if !r.cfg.Conflict.Configured() {
				return back(fmt.Sprintf("conflict with the train's base or the lines before it, in %s", strings.Join(fs, " ")))
			}
			var a ConflictAnswer
			if _, herr := r.cfg.Conflict.Run(ctx, ConflictInput{Dir: r.cfg.Dir, Train: t, Line: l.ID, Change: c.ID, Files: fs}, &a); herr != nil || !a.Resolved {
				why := a.Reason
				if herr != nil {
					why = "the conflict hook gave no answer: " + herr.Error()
				}
				return back(fmt.Sprintf("conflict in %s, not resolved: %s", strings.Join(fs, " "), why))
			}
			if left, _ := r.git(ctx, "diff", "--name-only", "--diff-filter=U"); left != "" {
				return back(fmt.Sprintf("conflict in %s: the conflict hook said resolved, and left %s unresolved", strings.Join(fs, " "), left))
			}
			_, err = r.git(ctx, cont...)
		}
		if err != nil {
			return back(fmt.Sprintf("change %s could not be applied: %v", c.ID, err))
		}
		prev = c.Head
	}
	end, err := r.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if end == start {
		return back("nothing to land: its changes are already on the train's base")
	}
	return "", nil
}

// Push implements adapter.Git: trunk moves from expect to head, by
// fast-forward and under a lease, or not at all. Trunk already at head is
// success. Trunk anywhere but expect, or a head that does not descend from
// expect, is a permanent refusal.
//
// With a trunk lock configured, the whole push, from reading trunk to the
// last reading after it, is made holding the lock: no other pusher moves
// trunk in between. A lock not got in time is a transient failure.
func (r *Repo) Push(ctx context.Context, key string, head, expect state.Head) error {
	r.net.Lock()
	defer r.net.Unlock()
	if r.cfg.TrunkLock != "" {
		wait := r.cfg.TrunkLockWait
		if wait <= 0 {
			wait = 2 * time.Minute
		}
		lctx, cancel := context.WithTimeout(ctx, wait)
		l, err := trunklock.Acquire(lctx, r.cfg.TrunkLock, trunklock.Options{Who: "shunter", MaxHold: r.cfg.TrunkLockMaxHold})
		cancel()
		if err != nil {
			return fmt.Errorf("push: the trunk lock: %w", err)
		}
		defer l.Release()
	}
	ref := "refs/heads/" + r.cfg.Trunk
	err := bound.Do(ctx, r.cfg.Policy, "push", func(ctx context.Context) error {
		cur, err := r.trunk(ctx)
		if err != nil {
			return err
		}
		switch {
		case cur == head:
			return nil // it took, on an earlier try or an earlier call
		case cur != expect:
			return adapter.Permanent(fmt.Errorf("not a fast-forward: trunk is at %s, not %s", short(cur), short(expect)))
		}
		if !r.has(ctx, head) || !r.has(ctx, expect) {
			if err := r.fetchLocked(ctx, nil); err != nil {
				return err
			}
			if !r.has(ctx, head) {
				return adapter.Permanent(fmt.Errorf("the head %s is not in the clone", short(head)))
			}
		}
		if _, err := r.git(ctx, "merge-base", "--is-ancestor", string(expect), string(head)); err != nil {
			return adapter.Permanent(fmt.Errorf("not a fast-forward: %s does not descend from %s", short(head), short(expect)))
		}
		_, err = r.git(ctx, "push", "--porcelain", "--no-verify", "--force-with-lease="+ref+":"+string(expect), r.cfg.Remote, string(head)+":"+ref)
		return err
	})
	if err == nil {
		return nil
	}
	// The outcome may be unknown: read trunk. At the head, it took.
	rctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if cur, rerr := r.trunk(rctx); rerr == nil && cur == head {
		return nil
	}
	return err
}

// trunk reads trunk on the remote.
func (r *Repo) trunk(ctx context.Context) (state.Head, error) {
	out, err := r.git(ctx, "ls-remote", "--exit-code", r.cfg.Remote, "refs/heads/"+r.cfg.Trunk)
	if err != nil {
		return "", err
	}
	f := strings.Fields(out)
	if len(f) < 1 {
		return "", errors.New("ls-remote: no answer")
	}
	return state.Head(f[0]), nil
}

// Trunk reads trunk on the remote, for a start or a resume.
func (r *Repo) Trunk(ctx context.Context) (state.Head, error) {
	var h state.Head
	err := bound.Do(ctx, r.cfg.Policy, "ls-remote", func(ctx context.Context) error {
		var err error
		h, err = r.trunk(ctx)
		return err
	})
	return h, err
}

func short(h state.Head) string {
	if len(h) > 12 {
		return string(h[:12])
	}
	return string(h)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
