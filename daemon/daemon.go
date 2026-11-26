// Package daemon is the runner: one loop that takes events from adapters,
// timers and commands, applies them through the store (and so through
// state.Next), and performs the effects the machine asks for through the
// adapters.
//
// The order of an act is fixed: the event and the effects it asks for are
// written to the store first, and only then are the effects performed. Each
// effect has a key, and its completion is recorded as an event (or as done)
// in the store. A restart finds every effect not recorded as done and
// performs it again under the same key, which the adapters make safe.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/store"
)

// Daemon is the runner. Its state lives in the store; what it holds in
// memory (the calls in flight, the retry clocks) is rebuilt from the store
// at start.
type Daemon struct {
	cfg Config
	st  *store.Store
	ad  adapter.Set
	log *log.Logger

	in      chan func()
	results chan result
	quit    chan struct{}
	wg      sync.WaitGroup

	jobs     map[string]*job // effect key -> the call in flight
	retry    map[string]retry
	polls    map[state.TrainID]bool
	polled   map[state.TrainID]time.Time
	pollErrs map[state.TrainID]int
	alerted  map[string]bool
	// retitled is the list each train's draft was last retitled to.
	retitled map[state.TrainID]string
	// paced holds the line checks given up with no verdict (release).
	paced map[state.LineID]unanswered
	// mergeTried is the heads and trunk each line sent back uncharged was
	// last found in conflict at: the merge isn't tested again until one moves.
	mergeTried  map[state.LineID]string
	pruning     bool      // a prune of ended trains' branches is in flight
	restacking  bool      // a re-read of stacked lines' needs is in flight
	restackedAt time.Time // when one was started last
	reoffering  bool      // a look at the lines sent back uncharged is in flight
	reofferedAt time.Time // when one was started last
	pruneFailed bool      // the last prune failed: the next waits a minute
	pruned      string    // the live trains at the last prune that succeeded
	prunedAt    time.Time // when it was tried last
	fatal       error
	online      bool
}

type job struct {
	k        store.Keyed
	cancel   context.CancelFunc
	stale    bool // cancelled because the machine no longer wants it
	shutdown bool // cancelled because the daemon is stopping
}

type retry struct {
	n  int
	at time.Time
}

// unanswered is a line check given up with no verdict, on its key, and when
// the line may be given to any checker again.
type unanswered struct {
	key   state.CheckKey
	until time.Time
}

// result is what a goroutine brings back to the loop.
type result struct {
	key   string // the effect, or "" for a CI poll
	poll  state.TrainID
	prune bool // a prune's result
	// reoffer is a look at the lines sent back uncharged.
	reoffer *reoffered
	// restack is a re-read of stacked and stale lines' needs (§5.2).
	restack bool
	// side is events an effect brings while it still runs (a train's
	// draft, opened mid-build); they complete nothing.
	side   bool
	events []state.Event
	actor  string
	note   string
	err    error
}

// New returns a daemon over an open store. It owns the store from here.
func New(st *store.Store, ad adapter.Set, cfg Config, logger *log.Logger) *Daemon {
	if logger == nil {
		logger = log.New(log.Writer(), "shunter: ", log.LstdFlags)
	}
	return &Daemon{
		cfg: cfg.withDefaults(), st: st, ad: ad, log: logger,
		in: make(chan func()), results: make(chan result), quit: make(chan struct{}),
		jobs: map[string]*job{}, retry: map[string]retry{}, polls: map[state.TrainID]bool{},
		polled: map[state.TrainID]time.Time{}, pollErrs: map[state.TrainID]int{}, alerted: map[string]bool{}, retitled: map[state.TrainID]string{}, paced: map[state.LineID]unanswered{},
		mergeTried: map[state.LineID]string{},
	}
}

func (d *Daemon) now() time.Time { return d.cfg.Now() }

// Run is the loop. It returns when ctx ends (nil) or the store fails (the
// error: a runner that cannot record must stop, not carry on unrecorded).
// Calls in flight are cancelled and left owed; the next start performs them
// again.
func (d *Daemon) Run(ctx context.Context) error {
	d.online = true
	tick := time.NewTicker(d.cfg.Tick)
	defer tick.Stop()
	defer d.stop()
	d.drive()
	for d.fatal == nil {
		select {
		case <-ctx.Done():
			return nil
		case f := <-d.in:
			f()
		case r := <-d.results:
			d.complete(r)
		case <-tick.C:
			d.tick()
		}
		if d.fatal == nil {
			d.drive()
		}
	}
	return d.fatal
}

func (d *Daemon) stop() {
	for _, j := range d.jobs {
		j.shutdown = true
		j.cancel()
	}
	close(d.quit)
	d.wg.Wait()
}

// do runs f in the loop and waits for it.
func (d *Daemon) do(ctx context.Context, f func()) error {
	done := make(chan struct{})
	select {
	case d.in <- func() { f(); close(done) }:
	case <-ctx.Done():
		return ctx.Err()
	case <-d.quit:
		return errors.New("daemon: stopped")
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Post applies an event from outside the loop (a webhook, a test, the
// judge) and returns the machine's answer.
func (d *Daemon) Post(ctx context.Context, actor string, ev state.Event) error {
	var err error
	if derr := d.do(ctx, func() { err = d.apply(actor, ev) }); derr != nil {
		return derr
	}
	return err
}

// State returns a copy of the state, read in the loop.
func (d *Daemon) State(ctx context.Context) (state.State, error) {
	var s state.State
	err := d.do(ctx, func() { s = d.st.State() })
	return s, err
}

// Tick runs the timers and starts the CI polls now. Tests use it after
// moving the clock.
func (d *Daemon) Tick(ctx context.Context) error { return d.do(ctx, d.tick) }

// Busy reports the calls in flight (effects and CI polls), and whether an
// owed effect is due to start. A daemon with none of either is quiet.
func (d *Daemon) Busy(ctx context.Context) (inflight int, due bool, err error) {
	err = d.do(ctx, func() {
		d.drive()
		inflight, due = len(d.jobs)+len(d.polls), d.due()
		if d.pruning || d.reoffering {
			inflight++
		}
	})
	return
}

func (d *Daemon) due() bool {
	now := d.now()
	for _, k := range d.st.Pending() {
		if _, busy := d.jobs[k.Key]; busy {
			continue
		}
		if r, ok := d.retry[k.Key]; !ok || !now.Before(r.at) {
			return true
		}
	}
	return false
}

// apply records one event. A refusal is the machine's answer, logged by the
// store; a store failure stops the daemon.
func (d *Daemon) apply(actor string, ev state.Event, done ...string) error {
	_, _, err := d.st.Apply(actor, ev, done...)
	if err != nil && !errors.Is(err, store.ErrRefused) {
		d.fatal = fmt.Errorf("daemon: the store failed; stopping: %w", err)
		return d.fatal
	}
	if err != nil {
		d.log.Printf("%s refused: %v", state.Name(ev), err)
		return err
	}
	d.reap()
	return nil
}

// note records an alert in the store and tells the judge. Once per subject.
func (d *Daemon) alert(subject, msg string) {
	if d.alerted[subject] {
		return
	}
	d.alerted[subject] = true
	if err := d.st.Note("runner", "alert: "+msg); err != nil {
		d.fatal = fmt.Errorf("daemon: the store failed; stopping: %w", err)
		return
	}
	d.log.Printf("alert: %s", msg)
	if d.ad.Notify == nil {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Call)
		defer cancel()
		if err := d.ad.Notify.Judge(ctx, "", "alert", "", msg); err != nil {
			d.log.Printf("alert not delivered: %v", err)
		}
	}()
}

// reap cancels the calls in flight that the machine no longer wants: the
// build of a train that went void, the pull request of an aborted one.
func (d *Daemon) reap() {
	s := d.st.State()
	for _, j := range d.jobs {
		if !j.stale && stale(s, j.k) {
			j.stale = true
			j.cancel()
		}
	}
}

// stale reports whether the machine has moved past an effect, so that
// performing it would act on a world that is gone. Cancelling a run, closing
// a pull request and telling someone are never stale.
func stale(s state.State, k store.Keyed) bool {
	tr := func(want state.TrainState) bool {
		t := s.Trains[k.Train]
		return t == nil || t.State != want
	}
	switch k.Kind {
	case state.EffGate:
		l := s.Lines[k.Line]
		return l == nil || l.State != state.LineOffered
	case state.EffBuild:
		return tr(state.TrainAssembling)
	case state.EffOpenPR:
		return tr(state.TrainBuilt)
	case state.EffRerun:
		return tr(state.TrainTesting)
	case state.EffVerify:
		return tr(state.TrainGreen)
	case state.EffPush:
		return tr(state.TrainLanding)
	case state.EffRebase:
		l := s.Lines[k.Line]
		return l == nil || l.State.Spent() || l.State == state.LineRiding || l.Conflict == ""
	}
	return false
}
