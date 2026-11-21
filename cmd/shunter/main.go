// Command shunter is the merge-queue daemon and its CLI. Linked or copied as
// "shunt", it is the same program; the verbs are the same either way.
//
//	shunter serve --config FILE
//	shunter serve --store DIR [--socket PATH] [--trunk HEAD] [--depth N] --adapters fake
//	shunter shadow --config FILE [--lines DIR|FILE] [--stats FILE] [--since 6h] [--effects OUT] [--verbose] TRAIN-LOG...
//
//	shunt queue <change>... [--alone]
//	shunt hold <line|change> --reason WHY
//	shunt release <line|change>
//	shunt cancel <train|line> --action park|requeue|drop --reason WHY
//	shunt withdraw <line|change>
//	shunt pause --reason WHY
//	shunt resume [TRUNK]
//	shunt judge <train> charge|split|park|rerun --reason WHY [--lines a,b]
//	shunt status
//	shunt export [--out FILE] [--line-files DIR]
//	shunt import [--file FILE | --line-files DIR]
//	shunter watch --config FILE [--state FILE] [--no-probe] [--no-notify]
//
// The CLI talks to the daemon on its socket (--socket, or $SHUNTER_SOCKET,
// or DIR/shunter.sock with --store DIR). With --offline --store DIR it opens
// the store itself, which it can only do while no daemon holds it: the
// effects it asks for wait for the daemon's next start.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/fake"
	"github.com/rolandpj1968/shunter/config"
	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/replay/adapter/trainlog"
	"github.com/rolandpj1968/shunter/shadow"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/store"
	"github.com/rolandpj1968/shunter/watch"
)

// version is stamped at build time: -ldflags "-X main.version=...".
var version = "dev"

var boolFlags = map[string]bool{"alone": true, "offline": true, "verbose": true, "no-probe": true, "no-notify": true}
var valueFlags = map[string]bool{"action": true, "reason": true, "lines": true, "socket": true, "store": true,
	"file": true, "out": true, "trunk": true, "depth": true, "adapters": true, "config": true,
	"stats": true, "since": true, "effects": true, "line-files": true, "state": true,
	"until": true, "checks": true, "owners": true, "live": true}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	verb := os.Args[1]
	pos, fl, err := parse(os.Args[2:])
	if err != nil {
		fatal(err)
	}
	if verb == "serve" {
		fatal(serve(fl))
	}
	if verb == "shadow" {
		fatal(shadowRun(fl, pos))
	}
	if verb == "watch" {
		os.Exit(watchRun(fl))
	}
	if verb == "version" {
		fmt.Println(version)
		return
	}
	req := daemon.Request{Cmd: verb, Args: pos, Alone: fl["alone"] != "", Action: fl["action"], Reason: fl["reason"], Until: fl["until"], Actor: actor()}
	if fl["lines"] != "" {
		req.Lines = strings.Split(fl["lines"], ",")
	}
	if verb == "import" && fl["line-files"] != "" {
		lf, err := store.ReadLines(fl["line-files"])
		if err != nil {
			fatal(fmt.Errorf("import: %w", err))
		}
		req.Plain = &lf
	} else if verb == "import" {
		doc, err := readDoc(fl["file"])
		if err != nil {
			fatal(err)
		}
		req.Doc = doc
	}
	resp, err := send(fl, req)
	if err != nil {
		fatal(err)
	}
	if resp.Error != "" {
		fatal(errors.New(resp.Error))
	}
	if verb == "export" && resp.Doc != nil && fl["line-files"] != "" {
		lf, err := store.ToLines(*resp.Doc)
		if err != nil {
			fatal(err)
		}
		if err := lf.Write(fl["line-files"]); err != nil {
			fatal(err)
		}
		fmt.Printf("%d waiting, %d held, %d parks, written to %s\n", len(lf.Waiting), len(lf.Held), len(lf.Parks), fl["line-files"])
		if fl["out"] == "" {
			return
		}
	}
	if verb == "export" && resp.Doc != nil {
		b, _ := json.MarshalIndent(resp.Doc, "", "  ")
		b = append(b, '\n')
		if fl["out"] != "" {
			if err := store.WriteFileAtomic(fl["out"], b); err != nil {
				fatal(err)
			}
		} else {
			os.Stdout.Write(b)
			return
		}
	}
	if resp.Output != "" {
		fmt.Println(strings.TrimRight(resp.Output, "\n"))
	}
}

// parse splits arguments into positional ones and --flags, which may come
// in any order.
func parse(args []string) ([]string, map[string]string, error) {
	var pos []string
	fl := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		name, val, has := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		switch {
		case boolFlags[name]:
			fl[name] = "true"
		case valueFlags[name] && has:
			fl[name] = val
		case valueFlags[name] && i+1 < len(args):
			i++
			fl[name] = args[i]
		default:
			return nil, nil, fmt.Errorf("unknown or incomplete flag %s", a)
		}
	}
	return pos, fl, nil
}

func actor() string {
	if a := os.Getenv("SHUNTER_ACTOR"); a != "" {
		return a
	}
	return "cli"
}

func socketPath(fl map[string]string) string {
	if s := fl["socket"]; s != "" {
		return s
	}
	if s := os.Getenv("SHUNTER_SOCKET"); s != "" {
		return s
	}
	if c := fl["config"]; c != "" {
		if f, err := config.Load(c); err == nil {
			return f.Socket
		}
	}
	if d := fl["store"]; d != "" {
		return filepath.Join(d, "shunter.sock")
	}
	return ""
}

func send(fl map[string]string, req daemon.Request) (daemon.Response, error) {
	if fl["offline"] != "" {
		if fl["store"] == "" {
			return daemon.Response{}, errors.New("--offline needs --store DIR")
		}
		st, err := store.Open(fl["store"], store.Options{Version: version, Actor: req.Actor})
		if err != nil {
			return daemon.Response{}, err
		}
		defer st.Close()
		return daemon.Offline(st, req), nil
	}
	sock := socketPath(fl)
	if sock == "" {
		return daemon.Response{}, errors.New("no daemon socket: give --socket, $SHUNTER_SOCKET or --store")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancel()
	return daemon.Call(ctx, sock, req)
}

func readDoc(path string) (*store.Document, error) {
	var r io.Reader = os.Stdin
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	var d store.Document
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return nil, fmt.Errorf("import: %w", err)
	}
	return &d, nil
}

// serve runs the daemon: with --config, on the real adapters it names; with
// --adapters fake, on in-memory fakes that land nothing anywhere.
func serve(fl map[string]string) error {
	if fl["config"] != "" {
		return serveConfig(fl["config"], fl["socket"])
	}
	dir := fl["store"]
	if dir == "" {
		return errors.New("serve: --config FILE, or --store DIR with --adapters fake")
	}
	var ad adapter.Set
	switch fl["adapters"] {
	case "fake":
		trunk := state.Head(fl["trunk"])
		if trunk == "" {
			trunk = "trunk-0"
		}
		ad = fake.New(trunk).Set()
	default:
		return errors.New("serve: without --config, --adapters fake is the only set")
	}
	depth := 2
	if s := fl["depth"]; s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("serve: --depth: %w", err)
		}
		depth = n
	}
	trunk := state.Head(fl["trunk"])
	if trunk == "" {
		trunk = "trunk-0"
	}
	return run(dir, socketPath(fl), ad, state.Config{Depth: depth}, trunk, daemon.Config{Version: version})
}

func serveConfig(path, sock string) error {
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	ad, g, err := f.Adapters()
	if err != nil {
		return err
	}
	if sock == "" {
		sock = f.Socket
	}
	// A new store starts at trunk as the remote has it now.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	trunk, err := g.Trunk(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("serve: reading trunk: %w", err)
	}
	if err := os.MkdirAll(f.Store, 0o755); err != nil {
		return err
	}
	return run(f.Store, sock, ad, f.State(), trunk, f.Daemon(version))
}

func run(dir, sock string, ad adapter.Set, sc state.Config, trunk state.Head, dc daemon.Config) error {
	st, err := store.Open(dir, store.Options{Init: &store.Init{Trunk: trunk, Config: sc}, Version: version, Actor: "daemon"})
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := daemon.New(st, ad, dc, log.New(os.Stderr, "shunter: ", log.LstdFlags))
	errs := make(chan error, 2)
	go func() { errs <- d.Serve(ctx, sock) }()
	go func() { errs <- d.Run(ctx) }()
	err = <-errs
	stop()
	if err2 := <-errs; err == nil {
		err = err2
	}
	return err
}

// shadowRun is shadow running: the forge and CI read-only, the live queue's
// train logs and line file as the inputs, and a report on stdout. It has no
// git adapter and no notifier: it cannot build, push or tell anyone.
func shadowRun(fl map[string]string, logs []string) error {
	if fl["config"] == "" {
		return errors.New("shadow: --config FILE is required")
	}
	f, err := config.Load(fl["config"])
	if err != nil {
		return err
	}
	gh, err := f.GitHub(true)
	if err != nil {
		return err
	}
	in := shadow.Input{Read: gh, Depth: f.Queue.Depth, MaxLines: f.Queue.MaxLines, MaxChanges: f.Queue.MaxChanges, RequireCheck: f.Queue.RequireCheck}
	// The gate's facts: files of one JSON object a line, read and never
	// written (shadow.CheckFact, OwnerFact, LiveFact).
	if err := readFacts(fl["checks"], &in.Checks); err != nil {
		return fmt.Errorf("shadow: --checks: %w", err)
	}
	if err := readFacts(fl["owners"], &in.Owners); err != nil {
		return fmt.Errorf("shadow: --owners: %w", err)
	}
	if err := readFacts(fl["live"], &in.Live); err != nil {
		return fmt.Errorf("shadow: --live: %w", err)
	}
	if in.MaxLines == 0 {
		in.MaxLines = 8
	}
	var since time.Time
	if s := fl["since"]; s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("shadow: --since: %w", err)
		}
		since = time.Now().Add(-d)
	}
	sort.Slice(logs, func(i, j int) bool { return filepath.Base(logs[i]) < filepath.Base(logs[j]) })
	for _, p := range logs {
		if fi, err := os.Stat(p); err != nil || (!since.IsZero() && fi.ModTime().Before(since)) {
			continue
		}
		r, err := os.Open(p)
		if err != nil {
			return err
		}
		lg, err := trainlog.Parse(filepath.Base(p), r)
		r.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		in.Logs = append(in.Logs, lg)
	}
	if p := fl["stats"]; p != "" {
		r, err := os.Open(p)
		if err != nil {
			return err
		}
		landed, err := trainlog.LandedPRs(r)
		r.Close()
		if err != nil {
			return err
		}
		in.Landed = func(pr string) bool { return landed[pr] }
	}
	if p := fl["lines"]; p != "" {
		in.Lines, err = readLineFiles(p)
		if err != nil {
			return fmt.Errorf("shadow: --lines: %w", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep := shadow.Run(ctx, in)
	rep.Write(os.Stdout, fl["verbose"] != "")
	fmt.Printf("Forge requests: %v (read-only)\n", gh.Requests())
	if p := fl["effects"]; p != "" {
		var b strings.Builder
		enc := json.NewEncoder(&b)
		for _, e := range rep.Effect {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return store.WriteFileAtomic(p, []byte(b.String()))
	}
	return nil
}

// readLineFiles reads the live queue's lines: a directory of line files
// (waiting, held and parked, docs/cutover.md), or a waiting list alone.
func readLineFiles(p string) (store.LineFile, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return store.LineFile{}, err
	}
	if fi.IsDir() {
		return store.ReadLines(p)
	}
	r, err := os.Open(p)
	if err != nil {
		return store.LineFile{}, err
	}
	defer r.Close()
	w, err := store.ParseWaiting(r)
	return store.LineFile{Waiting: w}, err
}

// watchRun is the outside watcher: one check, its findings raised through
// the notify hook, the report on stdout. With --no-notify the hook is not
// called and the watch state is not changed: the notices that would have
// been sent go to stderr (for checking a configuration, and for tests). Exit 0 healthy, 1 not, 2 when the
// check itself failed.
func watchRun(fl map[string]string) int {
	if fl["config"] == "" {
		fmt.Fprintln(os.Stderr, "shunter: watch: --config FILE is required")
		return 2
	}
	f, err := config.Load(fl["config"])
	if err != nil {
		fmt.Fprintln(os.Stderr, "shunter: watch:", err)
		return 2
	}
	dl := f.Deadlines
	c := watch.Config{
		Store: f.Store, Socket: f.Socket, NoProbe: fl["no-probe"] != "",
		TrunkLock: f.Git.TrunkLock, MaxHold: f.Git.TrunkLockMaxHold.Duration,
		Deadlines: watch.Deadlines{Call: dl.Call.Duration, Build: dl.Build.Duration, Open: dl.Open.Duration,
			CI: dl.CI.Duration, Push: dl.Push.Duration, J1: dl.J1.Duration},
		Grace: f.Watch.Grace.Duration, Stall: f.Watch.Stall.Duration, Restarts: f.Watch.Restarts,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := watch.Check(ctx, c)
	if err != nil {
		rep.Findings = append(rep.Findings, watch.Finding{Subject: "watch", Message: err.Error()})
	}
	fmt.Print(rep)
	state := f.Watch.State
	if s := fl["state"]; s != "" {
		state = s
	}
	a := watch.Alerter{Hook: f.NotifyHook(), State: state, Realert: f.Watch.Realert.Duration, Out: os.Stderr,
		DryRun: fl["no-notify"] != ""}
	if _, aerr := a.Raise(ctx, rep.Findings); aerr != nil {
		fmt.Fprintln(os.Stderr, "shunter: watch: raising:", aerr)
		return 2
	}
	if err != nil {
		return 2
	}
	if len(rep.Findings) > 0 {
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: shunter serve --config FILE | shunter shadow --config FILE ... | shunter watch --config FILE | shunt <queue|checked|handover|hold|release|cancel|restore|withdraw|pause|resume|judge|status|export|import> ...")
	os.Exit(2)
}

func fatal(err error) {
	if err == nil {
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "shunter:", err)
	os.Exit(1)
}

// readFacts reads a file of one JSON object a line into out, a pointer to a
// slice. No path reads nothing. A line that is not an object of the slice's
// type is an error: a fact half read would be a gate half fed.
func readFacts[T any](path string, out *[]T) error {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for i, ln := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var v T
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			return fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		*out = append(*out, v)
	}
	return nil
}
