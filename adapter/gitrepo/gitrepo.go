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
	// Rebase may rebase a waiting line that conflicts with trunk (§5.2).
	// Optional: none leaves the conflict to the line's owner.
	Rebase hook.Hook
	// Policy bounds each network call (fetch, ls-remote, push).
	Policy bound.Policy
	// Env is added to git's environment: the committer, say.
	Env []string
	// Resolvable marks the files the conflict hook resolves, as
	// "attribute=value" (e.g. "merge=regenerate"): a conflict only in such
	// files is no conflict to Merges. The build is still the test.
	Resolvable []string
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

// Versions implements adapter.Versioner: the pre-flight hook's version.
func (r *Repo) Versions() map[string]string {
	return map[string]string{state.CheckerPreflight: r.cfg.Preflight.Version()}
}

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
// RebaseInput is what the rebase hook is given: a waiting line that does not
// merge onto trunk. The hook rebases what it can without a person (the
// repository's own rule says what that is), pushes the new heads itself, and
// answers them; or it answers the conflict that needs a person. It must not
// push a head it did not test as the producers' own pushes are tested.
type RebaseInput struct {
	Dir    string            `json:"dir"`
	Remote string            `json:"remote"`
	Trunk  state.Head        `json:"trunk"`
	Line   adapter.BuildLine `json:"line"`
}

// RebaseAnswer is the rebase hook's answer.
type RebaseAnswer struct {
	Heads    map[state.ChangeID]state.Head `json:"heads,omitempty"`
	Conflict string                        `json:"conflict,omitempty"`
}

// Rebase implements adapter.Rebaser with the rebase hook. No hook, a hook
// that exits non-zero, or one that answers neither heads nor a conflict, is
// a conflict left to the owner: the runner never guesses a resolution.
func (r *Repo) Rebase(ctx context.Context, key string, line adapter.BuildLine, trunk state.Head) (adapter.RebaseResult, error) {
	if !r.cfg.Rebase.Configured() {
		return adapter.RebaseResult{Conflict: "no rebase hook is configured"}, nil
	}
	var a RebaseAnswer
	hr, err := r.cfg.Rebase.Run(ctx, RebaseInput{Dir: r.cfg.Dir, Remote: r.cfg.Remote, Trunk: trunk, Line: line}, &a)
	switch {
	case err != nil:
		return adapter.RebaseResult{}, fmt.Errorf("the rebase hook gave no answer: %w %s", err, hr.Stderr)
	case hr.Code != 0:
		return adapter.RebaseResult{Conflict: fmt.Sprintf("the rebase hook exited %d", hr.Code)}, nil
	case a.Conflict == "" && len(a.Heads) == 0:
		return adapter.RebaseResult{Conflict: "the rebase hook answered nothing"}, nil
	}
	return adapter.RebaseResult{Heads: a.Heads, Conflict: a.Conflict}, nil
}

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
	// NoVerdict says the pre-flight could not judge (its infrastructure
	// failed), with why: the train ends with no verdict, charging nobody.
	NoVerdict string `json:"no_verdict,omitempty"`
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
	if res.PreflightFailed || res.NoVerdict != "" {
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
	if req.Merged != nil {
		// The train is shown from here, while pre-flight runs: its branch
		// must be at the merged head first.
		r.net.Lock()
		_, err = r.netgit(ctx, "push", "--quiet", "--no-verify", "--force", r.cfg.Remote, head+":refs/heads/"+r.Branch(req.Train))
		r.net.Unlock()
		if err != nil {
			return res, "", false, err
		}
		req.Merged(state.Head(head))
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
	case !a.Pass && a.NoVerdict != "":
		res.NoVerdict = a.NoVerdict
		return res, head, false, nil
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
	merged := head
	if head, err = r.git(ctx, "rev-parse", "HEAD"); err != nil {
		return res, "", false, err
	}
	if err := r.onTop(ctx, merged, head); err != nil {
		return res, "", false, adapter.Permanent(fmt.Errorf("pre-flight: %w", err))
	}
	return res, head, false, nil
}

// onTop checks that a pre-flight's head is the merged head with only the
// hook's own commits on it (a regeneration, a counters update): it still
// holds every line of the train, and nothing else's history. CI judges, and
// trunk takes, exactly that head, so whatever the commits change is
// checked; only a head that dropped a line or merged another in could land
// something the train doesn't say.
func (r *Repo) onTop(ctx context.Context, merged, head string) error {
	if head == merged {
		return nil
	}
	ok, err := r.isAncestor(ctx, state.Head(merged), state.Head(head))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("its head %s does not hold the merged train %s: a hook may only add commits", short(state.Head(head)), short(state.Head(merged)))
	}
	merges, err := r.git(ctx, "rev-list", "--merges", merged+".."+head)
	if err != nil {
		return err
	}
	if merges != "" {
		return fmt.Errorf("it added a merge (%s): a hook may only add commits of its own", short(state.Head(firstLine(merges))))
	}
	return nil
}

// ancestor reports whether a is h or in h's history, in the clone as it is.
func (r *Repo) ancestor(ctx context.Context, a, h state.Head) bool {
	ok, err := r.isAncestor(ctx, a, h)
	return err == nil && ok
}

func (r *Repo) isAncestor(ctx context.Context, a, h state.Head) (bool, error) {
	_, err := r.git(ctx, "merge-base", "--is-ancestor", string(a), string(h))
	if err == nil {
		return true, nil
	}
	var ge *bound.GitError
	if errors.As(err, &ge) && ge.Code == 1 {
		return false, nil
	}
	return false, err
}

// Contains implements adapter.Ancestry: whether head contains ancestor, read
// in the clone, after fetching the changes named when either is missing.
// It only reads: it takes no lock on the work tree, so it doesn't wait for
// a build.
func (r *Repo) Contains(ctx context.Context, head, ancestor state.Head, ids []state.ChangeID) (bool, error) {
	if head == "" || ancestor == "" {
		return false, adapter.Permanent(errors.New("an empty head"))
	}
	if !r.has(ctx, head) || !r.has(ctx, ancestor) {
		if err := r.fetch(ctx, ids); err != nil {
			return false, err
		}
		if !r.has(ctx, head) || !r.has(ctx, ancestor) {
			return false, fmt.Errorf("%s or %s is not in the clone after a fetch", short(head), short(ancestor))
		}
	}
	return r.isAncestor(ctx, ancestor, head)
}

// Merges implements adapter.Merger: the line's last head merged onto trunk
// in memory (git merge-tree, git 2.38 or later), after fetching what the
// clone lacks. Like Contains it takes no lock on the work tree. The merge
// drivers of [git] env apply, as they do at a build.
func (r *Repo) Merges(ctx context.Context, line adapter.BuildLine, trunk state.Head) (string, error) {
	if len(line.Changes) == 0 || trunk == "" {
		return "", adapter.Permanent(errors.New("nothing to merge"))
	}
	top := line.Changes[len(line.Changes)-1].Head
	if !r.has(ctx, top) || !r.has(ctx, trunk) {
		ids := make([]state.ChangeID, len(line.Changes))
		for i, c := range line.Changes {
			ids[i] = c.ID
		}
		if err := r.fetch(ctx, ids); err != nil {
			return "", err
		}
		if !r.has(ctx, top) || !r.has(ctx, trunk) {
			return "", fmt.Errorf("%s or %s is not in the clone after a fetch", short(top), short(trunk))
		}
	}
	out, err := r.git(ctx, "merge-tree", "--write-tree", "--name-only", "--no-messages", string(trunk), string(top))
	var ge *bound.GitError
	switch {
	case err == nil:
		return "", nil
	case errors.As(err, &ge) && ge.Code == 1:
		// The first line is the tree written; the rest name the files in
		// conflict.
		files := strings.Split(strings.TrimSpace(out), "\n")
		if len(files) < 2 {
			return "a conflict", nil
		}
		left, err := r.unresolvable(ctx, trunk, files[1:])
		if err != nil {
			return "", err
		}
		return strings.Join(left, " "), nil
	default:
		return "", err
	}
}

// unresolvable is the files the conflict hook won't resolve: those that no
// Resolvable attribute marks, as trunk's .gitattributes says.
func (r *Repo) unresolvable(ctx context.Context, trunk state.Head, files []string) ([]string, error) {
	if len(r.cfg.Resolvable) == 0 {
		return files, nil
	}
	ok := map[string]bool{}
	for _, spec := range r.cfg.Resolvable {
		attr, value, _ := strings.Cut(spec, "=")
		out, err := r.git(ctx, append([]string{"check-attr", "-z", "--source", string(trunk), attr, "--"}, files...)...)
		if err != nil {
			return nil, err
		}
		f := strings.Split(out, "\x00")
		for i := 0; i+2 < len(f); i += 3 {
			if f[i+2] == value {
				ok[f[i]] = true
			}
		}
	}
	var left []string
	for _, f := range files {
		if !ok[f] {
			left = append(left, f)
		}
	}
	return left, nil
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
			if i == 0 && l.Upstream != "" && r.ancestor(ctx, l.Upstream, c.Head) {
				// A stacked line: replay only its own commits, after its
				// base's head, whatever its base became in the train.
				upstream = string(l.Upstream)
			} else if i == 0 {
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
