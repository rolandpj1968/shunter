//go:build unix

package watch

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter/hook"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/store"
	"github.com/rolandpj1968/shunter/trunklock"
)

// The tests write stores with the store package, as the daemon does; the
// watcher reads them without it.

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

var t0 = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func open(t *testing.T, dir string, clk *clock) *store.Store {
	t.Helper()
	st, err := store.Open(dir, store.Options{Init: &store.Init{Trunk: "T0", Config: state.Config{Depth: 2}}, Version: "test", Actor: "daemon", Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func apply(t *testing.T, st *store.Store, evs ...state.Event) {
	t.Helper()
	for _, ev := range evs {
		if _, _, err := st.Apply("test", ev); err != nil {
			t.Fatalf("%s: %v", state.Name(ev), err)
		}
	}
}

// queued writes a store with line a waiting, and leaves it open (held).
func queued(t *testing.T, clk *clock) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st := open(t, dir, clk)
	t.Cleanup(func() { st.Close() })
	apply(t, st, state.Offer{Line: "a", Changes: []state.Change{{ID: "a", Head: "a-1"}}}, state.GateAccept{Line: "a"})
	// The gate's effect was performed.
	for _, k := range st.Pending() {
		st.Done("runner", "done", k.Key)
	}
	return dir, st
}

func subjects(r Report) []string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, f.Subject)
	}
	return out
}

func has(r Report, prefix string) bool {
	for _, s := range subjects(r) {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func check(t *testing.T, c Config) Report {
	t.Helper()
	c.NoProbe = c.Socket == ""
	r, err := Check(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHealthyAndDown(t *testing.T) {
	clk := &clock{t0}
	dir, st := queued(t, clk)
	r := check(t, Config{Store: dir, Now: clk.Now})
	if len(r.Findings) != 0 || !r.Running || r.Waiting != 1 {
		t.Fatalf("a held, quiet store: %v\n%s", subjects(r), r)
	}
	st.Close()
	r = check(t, Config{Store: dir, Now: clk.Now})
	if !has(r, "daemon-down") || r.Running {
		t.Fatalf("no daemon: %v", subjects(r))
	}
}

func TestStuckEffectAndTrain(t *testing.T) {
	clk := &clock{t0}
	dir, st := queued(t, clk)
	apply(t, st, state.Assemble{Train: "t1", Lines: []state.LineID{"a"}})
	clk.t = t0.Add(40 * time.Minute)
	if r := check(t, Config{Store: dir, Now: clk.Now}); len(r.Findings) != 0 || r.Live["t1"] != "assembling" || r.Pending != 1 {
		t.Fatalf("within the build's deadline and its grace: %v\n%s", subjects(r), r)
	}
	clk.t = t0.Add(46 * time.Minute)
	r := check(t, Config{Store: dir, Now: clk.Now})
	if !has(r, "effect:") || !has(r, "train:t1:assembling") {
		t.Fatalf("a build owed past 30m and 15m of grace: %v\n%s", subjects(r), r)
	}
	// Built and testing: the CI deadline bounds it now.
	for _, k := range st.Pending() {
		st.Done("runner", "done", k.Key)
	}
	apply(t, st, state.Built{Train: "t1", Head: "h1"})
	for _, k := range st.Pending() {
		st.Done("runner", "done", k.Key)
	}
	apply(t, st, state.Opened{Train: "t1", PR: "9", Head: "h1"})
	if r := check(t, Config{Store: dir, Now: clk.Now}); len(r.Findings) != 0 {
		t.Fatalf("testing, just started: %v", subjects(r))
	}
	clk.t = clk.t.Add(2 * time.Hour)
	if r := check(t, Config{Store: dir, Now: clk.Now}); !has(r, "train:t1:testing") {
		t.Fatalf("testing past 90m and grace: %v", subjects(r))
	}
}

func TestStall(t *testing.T) {
	clk := &clock{t0}
	dir, st := queued(t, clk)
	clk.t = t0.Add(29 * time.Minute)
	if r := check(t, Config{Store: dir, Now: clk.Now}); len(r.Findings) != 0 {
		t.Fatalf("%v", subjects(r))
	}
	clk.t = t0.Add(31 * time.Minute)
	if r := check(t, Config{Store: dir, Now: clk.Now}); !has(r, "stall") {
		t.Fatalf("a line waiting half an hour with nothing in flight: %v", subjects(r))
	}
	// Paused, it is meant to wait.
	apply(t, st, state.Pause{Reason: "the judge paused it"})
	for _, k := range st.Pending() {
		st.Done("runner", "done", k.Key)
	}
	clk.t = t0.Add(3 * time.Hour)
	if r := check(t, Config{Store: dir, Now: clk.Now}); has(r, "stall") || !r.Paused {
		t.Fatalf("paused: %v", subjects(r))
	}
}

func TestRestartsAndTheLog(t *testing.T) {
	clk := &clock{t0}
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		clk.t = t0.Add(time.Duration(i) * 10 * time.Minute)
		st := open(t, dir, clk)
		st.Close()
	}
	if r := check(t, Config{Store: dir, Now: clk.Now}); !has(r, "restarts") {
		t.Fatalf("%v", subjects(r))
	}
	// A torn last line is skipped; damage before the end is a finding.
	log := filepath.Join(dir, "log.jsonl")
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`0000 {"seq":99,"ki`)
	f.Close()
	if r := check(t, Config{Store: dir, Now: clk.Now}); has(r, "log-unreadable") || r.Records == 0 {
		t.Fatalf("a torn last line: %v", subjects(r))
	}
	f, _ = os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n0000 {\"seq\":100}\n")
	f.Close()
	if r := check(t, Config{Store: dir, Now: clk.Now}); !has(r, "log-unreadable") {
		t.Fatalf("damage in the middle: %v", subjects(r))
	}
}

func TestProbe(t *testing.T) {
	clk := &clock{t0}
	dir, _ := queued(t, clk)
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	var answer atomic.Bool
	answer.Store(true)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["cmd"] != "status" || !answer.Load() {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
			return
		}
		w.Write([]byte(`{"output":"trunk T0"}`))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if r := check(t, Config{Store: dir, Socket: sock, Now: clk.Now}); len(r.Findings) != 0 {
		t.Fatalf("an answering daemon: %v", subjects(r))
	}
	answer.Store(false)
	if r := check(t, Config{Store: dir, Socket: sock, ProbeTimeout: 200 * time.Millisecond, Now: clk.Now}); !has(r, "daemon-unresponsive") {
		t.Fatalf("a daemon whose loop hangs: %v", subjects(r))
	}
}

func TestStaleTrunkLock(t *testing.T) {
	clk := &clock{t0}
	dir, _ := queued(t, clk)
	lock := filepath.Join(t.TempDir(), "trunk.lock")
	f, _ := os.OpenFile(lock, os.O_RDWR|os.O_CREATE, 0o644)
	defer f.Close()
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	host, _ := os.Hostname()
	f.WriteString(trunklock.Holder{PID: os.Getpid(), Host: host, Who: "legacy", Since: t0}.Format())
	if r := check(t, Config{Store: dir, TrunkLock: lock, MaxHold: 15 * time.Minute, Now: clk.Now}); has(r, "trunk-lock") {
		t.Fatalf("held in time: %v", subjects(r))
	}
	clk.t = t0.Add(time.Hour)
	if r := check(t, Config{Store: dir, TrunkLock: lock, MaxHold: 15 * time.Minute, Stall: 2 * time.Hour, Now: clk.Now}); !has(r, "trunk-lock") {
		t.Fatalf("held an hour: %v", subjects(r))
	}
}

func TestAlerterRaisesOnceRemindsAndClears(t *testing.T) {
	dir := t.TempDir()
	got := filepath.Join(dir, "got.jsonl")
	script := filepath.Join(dir, "notify.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ncat >> "+got+"\necho >> "+got+"\n"), 0o755)
	clk := &clock{t0}
	a := Alerter{Hook: hook.Hook{Command: []string{script}, Timeout: time.Minute}, State: filepath.Join(dir, "watch.json"), Realert: time.Hour, Now: clk.Now}
	down := []Finding{{Subject: "daemon-down", Message: "no daemon"}}
	ctx := context.Background()
	if n, err := a.Raise(ctx, down); err != nil || len(n) != 1 || !strings.Contains(n[0].Message, "no daemon") {
		t.Fatalf("first: %v %v", n, err)
	}
	clk.t = t0.Add(10 * time.Minute)
	if n, _ := a.Raise(ctx, down); len(n) != 0 {
		t.Fatalf("raised again within the hour: %v", n)
	}
	clk.t = t0.Add(61 * time.Minute)
	if n, _ := a.Raise(ctx, down); len(n) != 1 || !strings.Contains(n[0].Message, "still") {
		t.Fatalf("no reminder: %v", n)
	}
	clk.t = t0.Add(70 * time.Minute)
	if n, _ := a.Raise(ctx, nil); len(n) != 1 || !strings.Contains(n[0].Message, "cleared") {
		t.Fatalf("not cleared: %v", n)
	}
	if n, _ := a.Raise(ctx, nil); len(n) != 0 {
		t.Fatalf("cleared twice: %v", n)
	}
	b, _ := os.ReadFile(got)
	if c := strings.Count(string(b), `"point":"watch"`); c != 3 {
		t.Fatalf("the hook took %d notices:\n%s", c, b)
	}
	// A hook that fails: the finding is tried again at the next run.
	os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if _, err := a.Raise(ctx, down); err == nil {
		t.Fatal("a failing hook was no error")
	}
	os.WriteFile(script, []byte("#!/bin/sh\ncat > /dev/null\n"), 0o755)
	if n, err := a.Raise(ctx, down); err != nil || len(n) != 1 {
		t.Fatalf("not tried again: %v %v", n, err)
	}
}
