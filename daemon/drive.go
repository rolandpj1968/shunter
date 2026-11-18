package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
)

// assemble extends the chain with one train when it can (§5.3): not paused,
// shorter than D, and the train at its tail has a head to build on. The
// joining rule takes waiting lines in order and skips a line it cannot take:
// one that must ride alone, one of another half of a split, or one past the
// cap. Conflicts are the builder's to find, as leave-outs.
func (d *Daemon) assemble() {
	for d.fatal == nil {
		s := d.st.State()
		if s.Paused || len(s.Chain) >= s.Config.Depth {
			return
		}
		if n := len(s.Chain); n > 0 {
			switch s.Trains[s.Chain[n-1]].State {
			case state.TrainBuilt, state.TrainTesting, state.TrainGreen, state.TrainLanding:
			default:
				return
			}
		}
		lines := join(s, d.cfg.MaxLines)
		if len(lines) == 0 {
			return
		}
		id := state.TrainID(fmt.Sprintf("t%d", d.st.Seq()+1))
		if d.apply("runner", state.Assemble{Train: id, Lines: lines}) != nil {
			return
		}
	}
}

// join is the joining rule.
func join(s state.State, max int) []state.LineID {
	var out []state.LineID
	group := -1
	for _, id := range s.Waiting() {
		l := s.Lines[id]
		switch {
		case len(out) == 0 && l.Alone:
			return []state.LineID{id}
		case l.Alone:
			continue
		case group >= 0 && l.Group != group:
			continue
		}
		group = l.Group
		out = append(out, id)
		if len(out) >= max {
			break
		}
	}
	return out
}

// tick runs the deadlines of PROTOCOL.md's judgement points and starts the
// CI polls.
func (d *Daemon) tick() {
	if !d.online || d.fatal != nil {
		return
	}
	now := d.now()
	s := d.st.State()
	since := d.st.Since()
	for _, id := range s.Chain {
		t := s.Trains[id]
		at, ok := since.Trains[id]
		if !ok {
			continue
		}
		age := now.Sub(at)
		var ev state.Event
		switch {
		case t.State == state.TrainAssembling && age > d.cfg.Build:
			ev = state.CINoVerdict{Train: id, Infra: true, Why: "the builder's deadline passed"}
		case t.State == state.TrainBuilt && age > d.cfg.Open:
			ev = state.CINoVerdict{Train: id, Infra: true, Why: "no pull request by the deadline"}
		case t.State == state.TrainTesting && age > d.cfg.CI:
			ev = state.CINoVerdict{Train: id, Why: "no verdict by the deadline"}
		case t.State == state.TrainJudging && age > d.cfg.J1:
			ev = state.JudgeTimeout{Train: id, SameTestBefore: d.sameTest(t)}
		}
		if ev != nil {
			d.apply("timer", ev)
			// One act a tick: the chain may have changed under us.
			return
		}
	}
	d.restores(now)
	rerunning := map[state.TrainID]bool{}
	for _, k := range d.st.Pending() {
		if k.Kind == state.EffRerun || k.Kind == state.EffOpenPR {
			rerunning[k.Train] = true
		}
	}
	for _, id := range s.Chain {
		t := s.Trains[id]
		if t.State != state.TrainTesting || d.polls[id] || rerunning[id] || d.ad.CI == nil {
			continue
		}
		if last, ok := d.polled[id]; ok && now.Sub(last) < d.cfg.Poll {
			continue
		}
		d.poll(t.ID, t.Head)
	}
}

// sameTest is J1's question: did the same test fail in the train before,
// with unrelated changes? The machine records a failure's evidence as a
// string, so "the same test" is the same evidence.
func (d *Daemon) sameTest(t *state.Train) bool {
	fs := d.st.Since().Failures
	var mine string
	at := -1
	for i, f := range fs {
		if f.Train == t.ID {
			mine, at = f.Evidence, i
		}
	}
	if at <= 0 || mine == "" {
		return false
	}
	prev := fs[at-1]
	if prev.Evidence != mine {
		return false
	}
	for _, a := range prev.Lines {
		for _, b := range t.Lines {
			if a == b {
				return false // not unrelated: the same line rode both
			}
		}
	}
	return true
}

// restores tests each park's restore condition (§6.4). A condition the
// runner can test is "landed:<line>". Any other condition has the park's
// deadline, after which the park is restored with an alert.
func (d *Daemon) restores(now time.Time) {
	s := d.st.State()
	for id, p := range s.Parks {
		if line, ok := strings.CutPrefix(p.Cond, "landed:"); ok {
			if l := s.Lines[state.LineID(line)]; l != nil && l.State == state.LineLanded {
				d.apply("runner", state.Restore{Park: id})
				return
			}
		}
		at, ok := d.st.Since().Parks[id]
		if ok && now.Sub(at) > d.cfg.Park {
			d.alert("park:"+string(id), fmt.Sprintf("park %s restored at its deadline; its condition was: %s", id, p.Cond))
			d.apply("timer", state.Restore{Park: id})
			return
		}
	}
}

func (d *Daemon) poll(id state.TrainID, head state.Head) {
	d.polls[id] = true
	ci := d.ad.CI
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Call)
		defer cancel()
		rep, err := ci.Status(ctx, id, head)
		r := result{poll: id, actor: "ci", err: err}
		if err == nil {
			r.events = report(id, rep)
		}
		select {
		case d.results <- r:
		case <-d.quit:
		}
	}()
}

// report turns CI's report into an event. A report is not a verdict: a
// pass only makes a train green, and the lander reads for itself (I9).
func report(id state.TrainID, r adapter.Report) []state.Event {
	switch r.Outcome {
	case adapter.Passed:
		return evs(state.CIPassed{Train: id, Head: r.Head})
	case adapter.Failed:
		return evs(state.CIFailed{Train: id, Blamed: r.Blamed, Evidence: r.Evidence})
	case adapter.NoVerdict:
		return evs(state.CINoVerdict{Train: id, Why: orWhy(r.Why, "the run gave no verdict")})
	case adapter.Infra:
		return evs(state.CINoVerdict{Train: id, Infra: true, Why: orWhy(r.Why, "the infrastructure failed")})
	case adapter.Flaky:
		return evs(state.CIRerun{Train: id, Why: orWhy(r.Why, "a known flake or a timeout")})
	}
	return nil
}

func orWhy(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// pollDone applies what a poll read. A second flake on a train whose one
// rerun is spent is no longer a flake to the machine: it goes to J1 as a
// failure nobody is named for. A poll that keeps failing is escalated; the
// CI deadline voids the train in the end.
func (d *Daemon) pollDone(r result) {
	if r.err != nil {
		d.pollErrs[r.poll]++
		if d.pollErrs[r.poll] > d.cfg.Retries {
			d.alert("poll:"+string(r.poll), fmt.Sprintf("J3: CI for train %s cannot be read: %v", r.poll, r.err))
		}
		return
	}
	delete(d.pollErrs, r.poll)
	t := d.st.State().Trains[r.poll]
	if t == nil || t.State != state.TrainTesting {
		return
	}
	for _, ev := range r.events {
		if rr, ok := ev.(state.CIRerun); ok && t.Reruns >= 1 {
			ev = state.CIFailed{Train: rr.Train, Evidence: "failed again after its one rerun: " + rr.Why}
		}
		d.apply(r.actor, ev)
	}
}
