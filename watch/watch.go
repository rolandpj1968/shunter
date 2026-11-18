// Package watch is the outside watcher (L3, open question 14): a check,
// run from a timer, that the daemon is alive and making progress, which
// raises the notify hook when it is not.
//
// It deliberately shares no code path with the daemon's main loop. It
// doesn't open the store (which would need the daemon's lock), replay the
// log through the state machine, or call the daemon's packages. It reads the
// store's files as plain JSON, probes the store's lock and the daemon's
// socket from outside, and so still works when the thing it watches is the
// thing that is broken: a daemon that died, hangs in its loop, or stopped
// on a store failure.
//
// What it checks:
//
//   - the store's lock is held: a daemon is running;
//   - the daemon answers a status command on its socket in time: its loop
//     is turning (a command runs in the loop);
//   - it has not restarted again and again in the last hour;
//   - no effect it was asked for is still owed past its deadline, with
//     grace: a build past the builder's deadline, a push past the push's,
//     any other call past the call's (the daemon retries and escalates on
//     its own; the watcher says when that has not cleared it);
//   - no live train has stayed in one state past that state's deadline, with
//     grace: the daemon's own timers should have moved it;
//   - lines are not waiting with no train in flight, the queue not paused,
//     and nothing recorded for longer than the stall bound;
//   - the trunk lock is not stale.
package watch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rolandpj1968/shunter/trunklock"
)

// Config is what the watcher checks and its bounds.
type Config struct {
	// Store is the store's directory.
	Store string
	// Socket is the daemon's socket. Probed unless NoProbe.
	Socket  string
	NoProbe bool
	// TrunkLock is the trunk push lock's file, when one is used.
	TrunkLock string
	MaxHold   time.Duration
	// Deadlines are the daemon's.
	Deadlines Deadlines
	// Grace is added to every deadline. Default 15m.
	Grace time.Duration
	// Stall is how long lines may wait with nothing in flight and nothing
	// recorded. Default 30m.
	Stall time.Duration
	// Restarts is how many daemon starts in an hour are too many. Default 3.
	Restarts int
	// ProbeTimeout bounds the socket probe. Default 10s.
	ProbeTimeout time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Deadlines are the daemon's deadlines, as its configuration gives them.
// Zero takes the daemon's default.
type Deadlines struct {
	Call, Build, Open, CI, Push, J1 time.Duration
}

func (c Config) withDefaults() Config {
	d := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	d(&c.Deadlines.Call, 2*time.Minute)
	d(&c.Deadlines.Build, 30*time.Minute)
	d(&c.Deadlines.Open, 10*time.Minute)
	d(&c.Deadlines.CI, 90*time.Minute)
	d(&c.Deadlines.Push, 5*time.Minute)
	d(&c.Deadlines.J1, 10*time.Minute)
	d(&c.Grace, 15*time.Minute)
	d(&c.Stall, 30*time.Minute)
	d(&c.ProbeTimeout, 10*time.Second)
	if c.Restarts <= 0 {
		c.Restarts = 3
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Finding is one thing wrong. Subject is stable while the same thing stays
// wrong, so that it is raised once and then only as a reminder.
type Finding struct {
	Subject string `json:"subject"`
	Message string `json:"message"`
}

// Report is one check.
type Report struct {
	Findings []Finding
	// What the log says, for the report.
	Running  bool
	Records  int
	Last     time.Time
	LastKind string
	Pending  int
	Live     map[string]string // live train -> its state
	Waiting  int
	Paused   bool
}

// The log, read as plain JSON: only what the watcher needs.
type record struct {
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	Actor string    `json:"actor"`
	Kind  string    `json:"kind"`
	Event *struct {
		Type string `json:"type"`
	} `json:"event"`
	Effects []effect `json:"effects"`
	Done    []string `json:"done"`
}

type effect struct {
	Key   string `json:"key"`
	Kind  string `json:"Kind"`
	Line  string `json:"Line"`
	Train string `json:"Train"`
	To    string `json:"To"`
}

type owed struct {
	effect
	at time.Time
}

type since struct {
	state string
	at    time.Time
}

// live train states, and the deadline that bounds each.
var trainLive = map[string]func(Deadlines) time.Duration{
	"assembling": func(d Deadlines) time.Duration { return d.Build },
	"built":      func(d Deadlines) time.Duration { return d.Open },
	"testing":    func(d Deadlines) time.Duration { return d.CI },
	"judging":    func(d Deadlines) time.Duration { return d.J1 },
	"green":      func(d Deadlines) time.Duration { return d.Call },
	"landing":    func(d Deadlines) time.Duration { return d.Push },
}

func effectDeadline(kind string, d Deadlines) time.Duration {
	switch kind {
	case "build":
		return d.Build
	case "push":
		return d.Push
	}
	return d.Call
}

// Check runs every check once.
func Check(ctx context.Context, c Config) (Report, error) {
	c = c.withDefaults()
	now := c.Now()
	var rep Report
	add := func(subject, format string, a ...any) {
		rep.Findings = append(rep.Findings, Finding{Subject: subject, Message: fmt.Sprintf(format, a...)})
	}
	if c.Store == "" {
		return rep, errors.New("watch: no store")
	}
	if _, err := os.Stat(c.Store); err != nil {
		return rep, fmt.Errorf("watch: the store: %w", err)
	}
	running, err := lockHeld(filepath.Join(c.Store, "lock"))
	if err != nil {
		return rep, err
	}
	rep.Running = running
	if !running {
		add("daemon-down", "no daemon holds the store at %s: Shunter is not running", c.Store)
	} else if !c.NoProbe && c.Socket != "" {
		if err := probe(ctx, c.Socket, c.ProbeTimeout); err != nil {
			add("daemon-unresponsive", "the daemon holds the store but does not answer on its socket: %v", err)
		}
	}

	recs, err := readLog(filepath.Join(c.Store, "log.jsonl"))
	if err != nil {
		add("log-unreadable", "the store's log cannot be read: %v", err)
		return rep, nil
	}
	rep.Records = len(recs)
	pending := map[string]owed{}
	trains := map[string]since{}
	lines := map[string]string{}
	var starts []time.Time
	for _, r := range recs {
		rep.Last, rep.LastKind = r.Time, r.Kind
		if r.Kind == "start" && r.Actor == "daemon" {
			starts = append(starts, r.Time)
		}
		if r.Event != nil {
			switch r.Event.Type {
			case "pause":
				rep.Paused = true
			case "resume":
				rep.Paused = false
			}
		}
		for _, e := range r.Effects {
			switch {
			case e.Kind == "record" && e.Line != "":
				lines[e.Line] = e.To
			case e.Kind == "record" && e.Train != "":
				trains[e.Train] = since{e.To, r.Time}
			case e.Kind != "record":
				pending[e.Key] = owed{e, r.Time}
			}
		}
		for _, k := range r.Done {
			delete(pending, k)
		}
	}
	rep.Pending = len(pending)
	rep.Live = map[string]string{}
	for id, s := range trains {
		dl, live := trainLive[s.state]
		if !live {
			continue
		}
		rep.Live[id] = s.state
		if age := now.Sub(s.at); age > dl(c.Deadlines)+c.Grace {
			add("train:"+id+":"+s.state, "train %s has been %s for %s, past its deadline of %s and %s of grace: the daemon's timers are not moving it",
				id, s.state, age.Round(time.Minute), dl(c.Deadlines), c.Grace)
		}
	}
	keys := make([]string, 0, len(pending))
	for k := range pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	stuck := 0
	for _, k := range keys {
		p := pending[k]
		dl := effectDeadline(p.Kind, c.Deadlines)
		if age := now.Sub(p.at); age > dl+c.Grace {
			stuck++
			if stuck <= 5 {
				what := p.Train
				if p.Line != "" {
					what = "line " + p.Line
				} else if what != "" {
					what = "train " + what
				}
				add("effect:"+k, "the %s effect %s (%s) has been owed for %s, past its deadline of %s and %s of grace",
					p.Kind, k, strings.TrimSpace(what), age.Round(time.Minute), dl, c.Grace)
			}
		}
	}
	if stuck > 5 {
		add("effects", "%d effects in all are owed past their deadlines", stuck)
	}
	for _, s := range lines {
		if s == "waiting" {
			rep.Waiting++
		}
	}
	if rep.Waiting > 0 && len(rep.Live) == 0 && !rep.Paused && !rep.Last.IsZero() && now.Sub(rep.Last) > c.Stall {
		add("stall", "%d lines are waiting, no train is in flight, and nothing has been recorded for %s", rep.Waiting, now.Sub(rep.Last).Round(time.Minute))
	}
	n := 0
	for _, t := range starts {
		if now.Sub(t) <= time.Hour {
			n++
		}
	}
	if n >= c.Restarts {
		add("restarts", "the daemon has started %d times in the last hour", n)
	}
	if c.TrunkLock != "" {
		st, err := trunklock.Inspect(c.TrunkLock, trunklock.Options{MaxHold: c.MaxHold, Now: c.Now})
		switch {
		case err != nil:
			add("trunk-lock", "the trunk lock cannot be read: %v", err)
		case st.Stale != "":
			add("trunk-lock", "the trunk lock is %s", st)
		}
	}
	return rep, nil
}

// readLog reads every whole record of the log. A torn last line, which the
// store drops at its next start, is skipped; damage anywhere else is an
// error.
func readLog(path string) ([]record, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []record
	var bad error
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	n := 0
	for sc.Scan() {
		n++
		if bad != nil {
			return nil, bad
		}
		line := sc.Bytes()
		_, body, ok := bytes.Cut(line, []byte(" "))
		var r record
		if !ok || json.Unmarshal(body, &r) != nil {
			bad = fmt.Errorf("record on line %d is damaged", n)
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// probe sends the status command on the daemon's socket.
func probe(ctx context.Context, sock string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://shunter/v1/command", strings.NewReader(`{"cmd":"status","actor":"watch"}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("it answered %s", res.Status)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&body); err != nil {
		return fmt.Errorf("its answer: %w", err)
	}
	if body.Error != "" {
		return errors.New(body.Error)
	}
	return nil
}

// String prints the report for a person.
func (r Report) String() string {
	var b strings.Builder
	state := "running"
	if !r.Running {
		state = "NOT RUNNING"
	}
	last := "no records"
	if r.Records > 0 {
		last = fmt.Sprintf("%d records, the last (%s) at %s", r.Records, r.LastKind, r.Last.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "daemon %s; %s; %d effects owed; %d live trains; %d lines waiting",
		state, last, r.Pending, len(r.Live), r.Waiting)
	if r.Paused {
		b.WriteString("; paused")
	}
	b.WriteString("\n")
	if len(r.Findings) == 0 {
		b.WriteString("healthy\n")
	}
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "%s: %s\n", f.Subject, f.Message)
	}
	return b.String()
}
