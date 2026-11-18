// Command shunter is the merge-queue daemon and its CLI. Linked or copied as
// "shunt", it is the same program; the verbs are the same either way.
//
//	shunter serve --store DIR [--socket PATH] [--trunk HEAD] [--depth N] --adapters fake
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
//	shunt export [--out FILE]
//	shunt import [--file FILE]
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/fake"
	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/store"
)

// version is stamped at build time: -ldflags "-X main.version=...".
var version = "dev"

var boolFlags = map[string]bool{"alone": true, "offline": true}
var valueFlags = map[string]bool{"action": true, "reason": true, "lines": true, "socket": true, "store": true,
	"file": true, "out": true, "trunk": true, "depth": true, "adapters": true}

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
	if verb == "version" {
		fmt.Println(version)
		return
	}
	req := daemon.Request{Cmd: verb, Args: pos, Alone: fl["alone"] != "", Action: fl["action"], Reason: fl["reason"], Actor: actor()}
	if fl["lines"] != "" {
		req.Lines = strings.Split(fl["lines"], ",")
	}
	if verb == "import" {
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

// serve runs the daemon. The only adapters so far are the in-memory fakes,
// for trying the CLI: they land nothing anywhere.
func serve(fl map[string]string) error {
	dir := fl["store"]
	if dir == "" {
		return errors.New("serve: --store DIR is required")
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
		return errors.New("serve: --adapters fake is the only set so far; the forge, CI and git adapters are the next load")
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
	st, err := store.Open(dir, store.Options{Init: &store.Init{Trunk: trunk, Config: state.Config{Depth: depth}}, Version: version, Actor: "daemon"})
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := daemon.New(st, ad, daemon.Config{Version: version}, log.New(os.Stderr, "shunter: ", log.LstdFlags))
	errs := make(chan error, 2)
	go func() { errs <- d.Serve(ctx, socketPath(fl)) }()
	go func() { errs <- d.Run(ctx) }()
	err = <-errs
	stop()
	if err2 := <-errs; err == nil {
		err = err2
	}
	return err
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: shunter serve --store DIR --adapters fake | shunt <queue|hold|release|cancel|withdraw|pause|resume|judge|status|export|import> ...")
	os.Exit(2)
}

func fatal(err error) {
	if err == nil {
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "shunter:", err)
	os.Exit(1)
}
