package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/store"
)

// drive does what the runner does whenever anything happened: it starts
// every owed effect that is due, and then extends the chain.
func (d *Daemon) drive() {
	if !d.online || d.fatal != nil {
		return
	}
	now := d.now()
	for _, k := range d.st.Pending() {
		if _, busy := d.jobs[k.Key]; busy {
			continue
		}
		if r, ok := d.retry[k.Key]; ok && now.Before(r.at) {
			continue
		}
		d.start(k)
		if d.fatal != nil {
			return
		}
	}
	d.assemble()
}

// start performs one effect in its own goroutine, unless the machine has
// moved past it, in which case it is recorded as done and stale.
func (d *Daemon) start(k store.Keyed) {
	s := d.st.State()
	if stale(s, k) {
		d.done(k.Key, "stale: the machine has moved past it")
		return
	}
	call, timeout := d.call(s, k)
	if call == nil {
		d.done(k.Key, "nothing to do")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	d.jobs[k.Key] = &job{k: k, cancel: cancel}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer cancel()
		r := call(ctx)
		r.key = k.Key
		select {
		case d.results <- r:
		case <-d.quit:
		}
	}()
}

func (d *Daemon) done(key, result string) {
	if err := d.st.Done("runner", result, key); err != nil {
		d.fatal = fmt.Errorf("daemon: the store failed; stopping: %w", err)
	}
	delete(d.retry, key)
}

// call builds the function that performs an effect. It reads what it needs
// from the state here, in the loop; the function touches only the adapters.
func (d *Daemon) call(s state.State, k store.Keyed) (func(context.Context) result, time.Duration) {
	a := d.ad
	switch k.Kind {
	case state.EffGate:
		l := copyLine(s.Lines[k.Line])
		live := liveChanges(s, l.ID)
		n := needsOf(s, &l)
		anc, _ := a.Git.(adapter.Ancestry)
		return func(ctx context.Context) result {
			events, info, err := gateInfo(ctx, a.Forge, l, live)
			if err == nil && anc != nil && accepted(events) {
				// The gate reads the need twice (§5.2): a line whose
				// declared base and ancestry disagree is taken, stale.
				moved := map[state.ChangeID]state.Head{}
				for _, ev := range events {
					if hm, ok := ev.(state.HeadMoved); ok {
						moved[hm.Change] = hm.Head
					}
				}
				if why, serr := staleness(ctx, anc, n, withHeads(l.Changes, moved), info[0].Base); serr == nil && why != "" {
					events = append(events, state.Stale{Line: l.ID, Reason: why})
				}
			}
			return result{events: events, actor: "gate", err: err}
		}, d.cfg.Call
	case state.EffBuild:
		t := s.Trains[k.Train]
		req := adapter.BuildRequest{Train: t.ID, Base: k.Expect}
		for _, id := range t.Lines {
			req.Lines = append(req.Lines, adapter.BuildLine{ID: id, Changes: append([]state.Change(nil), s.Lines[id].Changes...)})
		}
		return func(ctx context.Context) result {
			r, err := a.Git.Build(ctx, k.Key, req)
			if err != nil {
				return result{actor: "builder", err: err}
			}
			return result{events: built(req, r), actor: "builder"}
		}, d.cfg.Build
	case state.EffOpenPR:
		return func(ctx context.Context) result {
			pr, err := a.Forge.OpenPR(ctx, k.Key, k.Train, k.Head, k.Expect)
			return result{events: evs(state.Opened{Train: k.Train, PR: pr, Head: k.Head}), actor: "forge", err: err}
		}, d.cfg.Call
	case state.EffRerun:
		head := s.Trains[k.Train].Head
		return func(ctx context.Context) result {
			return result{actor: "ci", note: "rerun started", err: a.CI.Rerun(ctx, k.Key, k.Train, head)}
		}, d.cfg.Call
	case state.EffCancelRun:
		return func(ctx context.Context) result {
			return result{actor: "ci", note: "run cancelled", err: a.CI.Cancel(ctx, k.Key, k.Train)}
		}, d.cfg.Call
	case state.EffClosePR:
		return func(ctx context.Context) result {
			return result{actor: "forge", note: "pull request closed", err: a.Forge.ClosePR(ctx, k.Key, k.Train, k.Reason)}
		}, d.cfg.Call
	case state.EffVerify:
		t := s.Trains[k.Train]
		var ids []state.ChangeID
		for _, id := range t.Lines {
			for _, c := range s.Lines[id].Changes {
				ids = append(ids, c.ID)
			}
		}
		return func(ctx context.Context) result {
			r, err := a.Forge.ReadLanding(ctx, k.Train, k.Head, ids)
			ev := state.LandingVerified{Train: k.Train, Verdict: r.Verdict, Heads: r.Heads, Trunk: r.Trunk}
			return result{events: evs(ev), actor: "lander", err: err}
		}, d.cfg.Call
	case state.EffPush:
		return func(ctx context.Context) result {
			err := a.Git.Push(ctx, k.Key, k.Head, k.Expect)
			return result{events: evs(state.PushOK{Train: k.Train}), actor: "lander", err: err}
		}, d.cfg.Push
	case state.EffNotify:
		l := s.Lines[k.Line]
		var ids []state.ChangeID
		for _, c := range l.Changes {
			ids = append(ids, c.ID)
		}
		return func(ctx context.Context) result {
			return result{actor: "notify", note: "owner told", err: a.Notify.Owner(ctx, k.Key, k.Line, ids, k.Reason)}
		}, d.cfg.Call
	case state.EffAskJudge:
		return func(ctx context.Context) result {
			return result{actor: "notify", note: "judge asked", err: a.Notify.Judge(ctx, k.Key, k.Point, k.Train, k.Reason)}
		}, d.cfg.Call
	}
	return nil, 0
}

func evs(e state.Event) []state.Event { return []state.Event{e} }

// liveChanges maps each change of a live line other than skip to its line.
func liveChanges(s state.State, skip state.LineID) map[state.ChangeID]state.LineID {
	out := map[state.ChangeID]state.LineID{}
	for id, l := range s.Lines {
		if id == skip || l.State.Spent() {
			continue
		}
		for _, c := range l.Changes {
			out[c.ID] = id
		}
	}
	return out
}

// Gate is the gate's reading of one line (§5.2), for shadow running: the
// events the runner would apply. live maps each change of a live line other
// than this one to its line.
func Gate(ctx context.Context, f adapter.Forge, l state.Line, live map[state.ChangeID]state.LineID) ([]state.Event, error) {
	return gate(ctx, f, l, live)
}

// LiveChanges maps each change of a live line other than skip to its line.
func LiveChanges(s state.State, skip state.LineID) map[state.ChangeID]state.LineID {
	return liveChanges(s, skip)
}

// gate is §5.2, read from the forge: every change open and from an approved
// producer, and stacked only on trunk, on the change before it in the line,
// or (the first change) on a change of the line's base.
//
// A change whose head moved since the offer is taken at its new head, as a
// head-moved event before the gate's answer.
func gate(ctx context.Context, f adapter.Forge, l state.Line, live map[state.ChangeID]state.LineID) ([]state.Event, error) {
	events, _, err := gateInfo(ctx, f, l, live)
	return events, err
}

// accepted reports whether the gate's events end in its yes.
func accepted(events []state.Event) bool {
	if len(events) == 0 {
		return false
	}
	_, ok := events[len(events)-1].(state.GateAccept)
	return ok
}

// gateInfo is gate, with the changes as the forge described them.
func gateInfo(ctx context.Context, f adapter.Forge, l state.Line, live map[state.ChangeID]state.LineID) ([]state.Event, []adapter.ChangeInfo, error) {
	ids := make([]state.ChangeID, len(l.Changes))
	for i, c := range l.Changes {
		ids[i] = c.ID
	}
	info, err := f.Describe(ctx, ids)
	if err != nil {
		if adapter.IsPermanent(err) {
			return evs(state.GateRefuse{Line: l.ID, Reason: err.Error()}), nil, nil
		}
		return nil, nil, err
	}
	refuse := func(format string, a ...any) ([]state.Event, []adapter.ChangeInfo, error) {
		return evs(state.GateRefuse{Line: l.ID, Reason: fmt.Sprintf(format, a...)}), nil, nil
	}
	if len(info) != len(l.Changes) {
		return nil, nil, fmt.Errorf("the forge described %d of %d changes", len(info), len(l.Changes))
	}
	var out []state.Event
	for i, c := range info {
		if c.Head != l.Changes[i].Head && c.Head != "" {
			out = append(out, state.HeadMoved{Line: l.ID, Change: c.ID, Head: c.Head})
		}
		switch {
		case !c.Open:
			return refuse("change %s is not open", c.ID)
		case !c.Approved:
			return refuse("change %s is not from an approved producer", c.ID)
		case c.Base == "":
		case i > 0 && c.Base == info[i-1].ID:
		case i == 0 && l.Base != "" && live[c.Base] == l.Base:
		case i == 0 && l.Base == "" && live[c.Base] == "":
			// Stacked on a change no live line holds: it must have landed.
			base, err := f.Describe(ctx, []state.ChangeID{c.Base})
			if err != nil {
				if adapter.IsPermanent(err) {
					return refuse("change %s is stacked on %s: %v", c.ID, c.Base, err)
				}
				return nil, nil, err
			}
			if !base[0].Landed {
				return refuse("change %s is stacked on %s, which has not landed and is in no live line", c.ID, c.Base)
			}
			// Its base has landed: move it onto trunk (open question 21).
			// Advice only: the change is taken whether or not it takes.
			if r, ok := f.(adapter.Retargeter); ok {
				r.Retarget(ctx, c.ID)
			}
		default:
			return refuse("change %s is stacked on %s, which is not its base in this line", c.ID, c.Base)
		}
	}
	return append(out, state.GateAccept{Line: l.ID}), info, nil
}

// built turns a builder's answer into events: each left-out line, then the
// pre-flight failure or the built head.
func built(req adapter.BuildRequest, r adapter.BuildResult) []state.Event {
	var out []state.Event
	kept := 0
	for _, l := range req.Lines {
		if why, ok := r.LeftOut[l.ID]; ok {
			if why == "" {
				why = "left out at assembly"
			}
			out = append(out, state.LeaveOut{Train: req.Train, Line: l.ID, Reason: why})
			continue
		}
		kept++
	}
	switch {
	case kept == 0:
	case r.PreflightFailed:
		out = append(out, state.PreflightFailed{Train: req.Train, Blamed: r.Blamed, Evidence: r.Evidence})
	default:
		out = append(out, state.Built{Train: req.Train, Head: r.Head})
	}
	return out
}

// complete takes a goroutine's result back into the loop.
func (d *Daemon) complete(r result) {
	if r.prune {
		d.pruneDone(r)
		return
	}
	if r.restack {
		d.restackDone(r)
		return
	}
	if r.key == "" {
		d.polled[r.poll] = d.now()
		delete(d.polls, r.poll)
		d.pollDone(r)
		return
	}
	j := d.jobs[r.key]
	delete(d.jobs, r.key)
	if j == nil || j.shutdown {
		return
	}
	if j.stale {
		d.done(r.key, "stale: cancelled when the machine moved past it")
		return
	}
	if r.err == nil {
		delete(d.retry, r.key)
		if len(r.events) == 0 {
			d.done(r.key, r.note)
			return
		}
		for i, ev := range r.events {
			var done []string
			if i == 0 {
				done = []string{r.key}
			}
			if d.apply(r.actor, ev, done...) != nil && d.fatal != nil {
				return
			}
		}
		return
	}
	d.failed(j.k, r)
}

// failed handles an effect whose call failed. A push's refusal is the
// machine's to count (§5.10); a build, a pull request or a rerun that cannot
// be made is no verdict; anything else is retried, and escalated to J3 when
// its budget is spent.
func (d *Daemon) failed(k store.Keyed, r result) {
	perm := adapter.IsPermanent(r.err)
	why := r.err.Error()
	if errors.Is(r.err, context.DeadlineExceeded) {
		why = "no answer by the deadline: " + why
	}
	n := d.retry[k.Key].n + 1
	switch k.Kind {
	case state.EffPush:
		// A push that timed out may have taken: the next push under a new
		// key finds trunk at the head and succeeds. Waits between tries.
		if d.apply("lander", state.PushRefused{Train: k.Train, Permanent: perm, Why: why}, k.Key) != nil {
			return
		}
		if t := d.st.State().Trains[k.Train]; t != nil && t.State == state.TrainLanding {
			for _, p := range d.st.Pending() {
				if p.Kind == state.EffPush && p.Train == k.Train {
					d.retry[p.Key] = retry{n: t.Pushes, at: d.now().Add(d.backoff(t.Pushes))}
				}
			}
		}
		return
	case state.EffBuild, state.EffOpenPR, state.EffRerun:
		if perm || n > d.cfg.Retries {
			d.apply(r.actor, state.CINoVerdict{Train: k.Train, Infra: true, Why: string(k.Kind) + ": " + why}, k.Key)
			return
		}
	}
	if n > d.cfg.Retries || perm {
		d.alert("J3:"+k.Key, fmt.Sprintf("J3: %s %s%s keeps failing (%d tries): %s", k.Kind, k.Line, k.Train, n, why))
	}
	d.retry[k.Key] = retry{n: n, at: d.now().Add(d.backoff(n))}
}

func (d *Daemon) backoff(n int) time.Duration {
	b := d.cfg.Backoff
	for i := 1; i < n && b < 5*time.Minute; i++ {
		b *= 2
	}
	if b > 5*time.Minute {
		b = 5 * time.Minute
	}
	return b
}

// sortedLines returns ids in queue order.
func sortedLines(s state.State, ids []state.LineID) []state.LineID {
	out := append([]state.LineID(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return s.Lines[out[i]].Seq < s.Lines[out[j]].Seq })
	return out
}
