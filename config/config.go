// Package config reads Shunter's one configuration file, in TOML, and builds
// the daemon's settings and its adapters from it. See
// examples/shunter.toml for every key, with its default.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/bound"
	"github.com/rolandpj1968/shunter/adapter/github"
	"github.com/rolandpj1968/shunter/adapter/gitrepo"
	"github.com/rolandpj1968/shunter/adapter/hook"
	"github.com/rolandpj1968/shunter/adapter/notify"
	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/tier"
)

// Duration is a time.Duration written as "90m" or "5s".
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("a negative duration: %s", b)
	}
	d.Duration = v
	return nil
}

// File is the configuration file.
type File struct {
	// Store is the store's directory (the queue's one record). Required.
	Store string `toml:"store"`
	// Socket is the CLI's unix socket. Default <store>/shunter.sock.
	Socket string `toml:"socket"`

	Repo      Repo      `toml:"repo"`
	Git       Git       `toml:"git"`
	CI        CI        `toml:"ci"`
	Queue     Queue     `toml:"queue"`
	Deadlines Deadlines `toml:"deadlines"`
	Retry     Retry     `toml:"retry"`
	Hooks     Hooks     `toml:"hooks"`
	Tiers     Tiers     `toml:"tiers"`
	Notify    Notify    `toml:"notify"`
	Watch     Watch     `toml:"watch"`
	Owners    Owners    `toml:"owners"`

	dir string // the file's directory: relative paths are relative to it
}

// Repo is the forge and the repository.
type Repo struct {
	Forge          string            `toml:"forge"` // "github"
	API            string            `toml:"api"`
	Owner          string            `toml:"owner"`
	Name           string            `toml:"name"`
	Trunk          string            `toml:"trunk"`
	TokenEnv       string            `toml:"token_env"`
	TokenCommand   []string          `toml:"token_command"`
	Authors        []string          `toml:"authors"`
	AuthorBranches map[string]string `toml:"author_branches"`
	ApprovedFile   string            `toml:"approved_file"`
	ApprovedLabel  string            `toml:"approved_label"`
	LandedLabel    string            `toml:"landed_label"`
	MinGap         Duration          `toml:"min_gap"`
}

// Git is the clone trains are built in.
type Git struct {
	Dir          string   `toml:"dir"`
	Remote       string   `toml:"remote"`
	BranchPrefix string   `toml:"branch_prefix"`
	ChangeRef    string   `toml:"change_ref"`
	Strategy     string   `toml:"strategy"`
	Env          []string `toml:"env"`
	// Resolvable: files the conflict hook resolves, as "attribute=value".
	Resolvable []string `toml:"resolvable"`
	// TrunkLock is the trunk push lock every pusher holds (docs/cutover.md).
	TrunkLock        string   `toml:"trunk_lock"`
	TrunkLockWait    Duration `toml:"trunk_lock_wait"`
	TrunkLockMaxHold Duration `toml:"trunk_lock_max_hold"`
}

// CI is how a train is tested and how its verdict is read.
type CI struct {
	Mode     string   `toml:"mode"` // "checks" or "dispatch"
	Workflow string   `toml:"workflow"`
	Checks   []string `toml:"checks"`
}

// Queue is the queue's shape.
type Queue struct {
	Depth      int `toml:"depth"`
	MaxLines   int `toml:"max_lines"`
	MaxChanges int `toml:"max_changes"`
	// RequireCheck: no line joins a train without a current green line
	// check (PROTOCOL.md §5.4, I10).
	RequireCheck bool `toml:"require_check"`
	// Drafts: each train is shown from assembly as a draft pull request,
	// promoted when its pre-flight passes (PROTOCOL.md §5.4).
	Drafts bool `toml:"drafts"`
	// StarveAfterCycles, StarveAfterHours and StarveAlert are the
	// starvation thresholds (docs/queue-as-bag.md §4, §9.4). Unset, they
	// take the ruled values; 0 turns each off.
	StarveAfterCycles *int `toml:"starve_after_cycles"`
	StarveAfterHours  *int `toml:"starve_after_hours"`
	StarveAlert       *int `toml:"starve_alert"`
}

// The ruled starvation thresholds (docs/queue-as-bag.md §9.4).
const (
	defaultStarveCycles = 3
	defaultStarveHours  = 12
	defaultStarveAlert  = 3
)

func orDefault(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// Deadlines are every wait's bound; zero takes the daemon's default.
type Deadlines struct {
	Tick  Duration `toml:"tick"`
	Poll  Duration `toml:"poll"`
	Call  Duration `toml:"call"`
	Build Duration `toml:"build"`
	Open  Duration `toml:"open"`
	CI    Duration `toml:"ci"`
	Push  Duration `toml:"push"`
	J1    Duration `toml:"j1"`
	Park  Duration `toml:"park"`
	// Review and Owner are alerts, not releases: a hold that has stood
	// this long, a charged line this long with its owner.
	Review Duration `toml:"review"`
	Owner  Duration `toml:"owner"`
	// Claim is a checker's claim on a line check, unless it names its own.
	Claim Duration `toml:"claim"`
}

// Retry is the two retry budgets: the adapters' short one, inside a call,
// and the daemon's, across calls, before J3.
type Retry struct {
	Attempts   int      `toml:"attempts"`
	Backoff    Duration `toml:"backoff"`
	MaxBackoff Duration `toml:"max_backoff"`
	Effects    int      `toml:"effects"`
	EffectWait Duration `toml:"effect_backoff"`
}

// Tiers are the rules of the gate's tier classifier (PROTOCOL.md §5.4):
// which content is inert, so that a line of nothing else takes the light
// tier. Patterns are slash-separated, "**" for any number of directories.
type Tiers struct {
	// Inert is content no build, test or generator reads.
	Inert []string `toml:"inert"`
	// Read is content something does read, though an inert pattern matches
	// it too. It is never inert.
	Read []string `toml:"read"`
}

// Hooks are the repository's own rules.
type Hooks struct {
	Preflight Hook `toml:"preflight"`
	Conflict  Hook `toml:"conflict"`
	Rebase    Hook `toml:"rebase"`
	Blame     Hook `toml:"blame"`
}

// Hook is one external command.
type Hook struct {
	Command []string `toml:"command"`
	Timeout Duration `toml:"timeout"`
	Dir     string   `toml:"dir"`
	Env     []string `toml:"env"`
}

// Notify is where notices go.
type Notify struct {
	Events  string   `toml:"events"`
	Command []string `toml:"command"`
	Timeout Duration `toml:"timeout"`
}

// Watch is the outside watcher's (shunter watch) own bounds.
type Watch struct {
	// State remembers what was raised. Default <store>/watch.json.
	State    string   `toml:"state"`
	Grace    Duration `toml:"grace"`
	Stall    Duration `toml:"stall"`
	Realert  Duration `toml:"realert"`
	Restarts int      `toml:"restarts"`
}

// Owners is how the daemon tells a live owner from one that has gone
// (PROTOCOL.md §7). With no Heartbeats, every owner counts as live.
type Owners struct {
	// Heartbeats is a directory with one file a producer, named as the
	// producer offers lines; its modification time is the producer's
	// last sign of life.
	Heartbeats string   `toml:"heartbeats"`
	Alive      Duration `toml:"alive"`
	Ownerless  Duration `toml:"ownerless"`
}

// Load reads and checks a configuration file. A key it does not know is an
// error, so that a misspelt deadline is not silently the default.
func Load(path string) (*File, error) {
	var f File
	md, err := toml.DecodeFile(path, &f)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f.dir = filepath.Dir(abs)
	f.defaults()
	if err := f.check(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &f, nil
}

func (f *File) defaults() {
	if f.Repo.Forge == "" {
		f.Repo.Forge = "github"
	}
	if f.Repo.TokenEnv == "" && len(f.Repo.TokenCommand) == 0 {
		f.Repo.TokenEnv = "GH_TOKEN"
	}
	if f.Git.Remote == "" {
		f.Git.Remote = "origin"
	}
	if f.Git.BranchPrefix == "" {
		f.Git.BranchPrefix = "shunter/"
	}
	if f.Git.Strategy == "" {
		f.Git.Strategy = "rebase"
	}
	if f.CI.Mode == "" {
		f.CI.Mode = "checks"
	}
	if f.Queue.Depth == 0 {
		f.Queue.Depth = 2
	}
	if f.Notify.Events == "" && f.Store != "" {
		f.Notify.Events = filepath.Join(f.Store, "events.jsonl")
	}
	f.Store = f.path(f.Store)
	f.Socket = f.path(f.Socket)
	f.Git.Dir = f.path(f.Git.Dir)
	f.Git.TrunkLock = f.path(f.Git.TrunkLock)
	f.Notify.Events = f.path(f.Notify.Events)
	if f.Watch.State == "" && f.Store != "" {
		f.Watch.State = filepath.Join(f.Store, "watch.json")
	}
	f.Watch.State = f.path(f.Watch.State)
	if f.Socket == "" && f.Store != "" {
		f.Socket = filepath.Join(f.Store, "shunter.sock")
	}
}

// path resolves p against the file's directory.
func (f *File) path(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return filepath.Join(f.dir, p)
}

func (f *File) check() error {
	var errs []error
	need := func(v, name string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	need(f.Store, "store")
	need(f.Repo.Owner, "repo.owner")
	need(f.Repo.Name, "repo.name")
	need(f.Repo.Trunk, "repo.trunk")
	need(f.Git.Dir, "git.dir")
	if f.Repo.Forge != "github" {
		errs = append(errs, fmt.Errorf("repo.forge %q: only github so far", f.Repo.Forge))
	}
	if f.Git.Strategy != "rebase" && f.Git.Strategy != "merge" {
		errs = append(errs, fmt.Errorf("git.strategy %q: rebase or merge", f.Git.Strategy))
	}
	switch f.CI.Mode {
	case "checks":
	case "dispatch":
		if f.CI.Workflow == "" {
			errs = append(errs, errors.New("ci.mode dispatch needs ci.workflow"))
		}
	default:
		errs = append(errs, fmt.Errorf("ci.mode %q: checks or dispatch", f.CI.Mode))
	}
	if len(f.CI.Checks) == 0 {
		errs = append(errs, errors.New("ci.checks: name the checks whose success is the verdict"))
	}
	for k, p := range map[string]*int{"starve_after_cycles": f.Queue.StarveAfterCycles, "starve_after_hours": f.Queue.StarveAfterHours, "starve_alert": f.Queue.StarveAlert} {
		if p != nil && *p < 0 {
			errs = append(errs, fmt.Errorf("queue.%s %d: 0 (off) or more", k, *p))
		}
	}
	if f.Queue.MaxChanges < 0 {
		errs = append(errs, fmt.Errorf("queue.max_changes %d: 0 (no cap) or more", f.Queue.MaxChanges))
	}
	if f.Queue.Depth < 1 {
		errs = append(errs, fmt.Errorf("queue.depth %d: at least 1", f.Queue.Depth))
	}
	if !strings.Contains(f.Git.ChangeRef, "{id}") && f.Git.ChangeRef != "" {
		errs = append(errs, errors.New("git.change_ref must contain {id}"))
	}
	return errors.Join(errs...)
}

// Daemon is the daemon's configuration.
func (f *File) Daemon(version string) daemon.Config {
	d := f.Deadlines
	return daemon.Config{
		Version: version,
		Tiers:   tier.Rules{Inert: f.Tiers.Inert, Read: f.Tiers.Read},
		Tick:    d.Tick.Duration, Poll: d.Poll.Duration, Call: d.Call.Duration,
		Build: d.Build.Duration, Open: d.Open.Duration, CI: d.CI.Duration, Push: d.Push.Duration,
		J1: d.J1.Duration, Park: d.Park.Duration, Review: d.Review.Duration, Owner: d.Owner.Duration, Claim: d.Claim.Duration,
		MaxLines: f.Queue.MaxLines, MaxChanges: f.Queue.MaxChanges, Drafts: f.Queue.Drafts, Retries: f.Retry.Effects, Backoff: f.Retry.EffectWait.Duration,
		Heartbeats: f.path(f.Owners.Heartbeats), Alive: f.Owners.Alive.Duration, Ownerless: f.Owners.Ownerless.Duration,
		StarveCycles: orDefault(f.Queue.StarveAfterCycles, defaultStarveCycles),
		StarveAfter:  time.Duration(orDefault(f.Queue.StarveAfterHours, defaultStarveHours)) * time.Hour,
		StarveAlert:  orDefault(f.Queue.StarveAlert, defaultStarveAlert),
	}
}

// State is the queue's configuration, for a new store.
func (f *File) State() state.Config {
	return state.Config{Depth: f.Queue.Depth, RequireCheck: f.Queue.RequireCheck}
}

func (f *File) policy() bound.Policy {
	return bound.Policy{Attempts: f.Retry.Attempts, Backoff: f.Retry.Backoff.Duration, MaxBackoff: f.Retry.MaxBackoff.Duration}
}

func (f *File) hook(h Hook, dir string) hook.Hook {
	out := hook.Hook{Command: h.Command, Timeout: h.Timeout.Duration, Dir: f.path(h.Dir), Env: h.Env}
	if out.Dir == "" {
		out.Dir = dir
	}
	if len(out.Command) > 0 && strings.Contains(out.Command[0], "/") {
		out.Command = append([]string{f.path(out.Command[0])}, out.Command[1:]...)
	}
	return out
}

// NotifyHook is the notify hook, for the watcher.
func (f *File) NotifyHook() hook.Hook {
	return f.hook(Hook{Command: f.Notify.Command, Timeout: f.Notify.Timeout}, "")
}

// token returns the forge token, from the environment or a command. It is
// never logged.
func (f *File) token() (string, error) {
	if f.Repo.TokenEnv != "" {
		if v := os.Getenv(f.Repo.TokenEnv); v != "" {
			return v, nil
		}
		if len(f.Repo.TokenCommand) == 0 {
			return "", fmt.Errorf("$%s is empty and no token_command is set", f.Repo.TokenEnv)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, f.Repo.TokenCommand[0], f.Repo.TokenCommand[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("token_command failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// GitHub is the forge and CI adapter. Read-only refuses every write.
func (f *File) GitHub(readOnly bool) (*github.Client, error) {
	return github.New(github.Config{
		API: f.Repo.API, Owner: f.Repo.Owner, Repo: f.Repo.Name, Trunk: f.Repo.Trunk, Token: f.token,
		BranchPrefix: f.Git.BranchPrefix, Authors: f.Repo.Authors, ApprovedLabel: f.Repo.ApprovedLabel,
		AuthorBranches: f.Repo.AuthorBranches, ApprovedFile: f.path(f.Repo.ApprovedFile), LandedLabel: f.Repo.LandedLabel,
		Checks: f.CI.Checks, Workflow: f.CI.Workflow, Dispatch: f.CI.Mode == "dispatch",
		Blame:    f.hook(f.Hooks.Blame, ""),
		ReadOnly: readOnly, MinGap: f.Repo.MinGap.Duration, Policy: f.policy(),
	})
}

// Adapters builds every adapter for a live daemon.
func (f *File) Adapters() (adapter.Set, *gitrepo.Repo, error) {
	gh, err := f.GitHub(false)
	if err != nil {
		return adapter.Set{}, nil, err
	}
	g, err := gitrepo.New(gitrepo.Config{
		Dir: f.Git.Dir, Remote: f.Git.Remote, Trunk: f.Repo.Trunk, BranchPrefix: f.Git.BranchPrefix,
		ChangeRef: f.Git.ChangeRef, Strategy: f.Git.Strategy, Env: f.Git.Env, Resolvable: f.Git.Resolvable, Policy: f.policy(),
		Preflight: f.hook(f.Hooks.Preflight, f.Git.Dir), Conflict: f.hook(f.Hooks.Conflict, f.Git.Dir), Rebase: f.hook(f.Hooks.Rebase, f.Git.Dir),
		TrunkLock: f.Git.TrunkLock, TrunkLockWait: f.Git.TrunkLockWait.Duration, TrunkLockMaxHold: f.Git.TrunkLockMaxHold.Duration,
	})
	if err != nil {
		return adapter.Set{}, nil, err
	}
	n, err := notify.Open(f.Notify.Events, f.hook(Hook{Command: f.Notify.Command, Timeout: f.Notify.Timeout}, ""), nil)
	if err != nil {
		return adapter.Set{}, nil, err
	}
	return adapter.Set{Forge: gh, CI: gh, Git: g, Notify: github.Landed{Client: gh, Next: n}}, g, nil
}
