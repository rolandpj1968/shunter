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
// one that must ride alone, one of another half of a split, one past a cap,
// a stale one, and one whose base has not landed, rides in no earlier train
// and was not taken into this one (I3): so a line whose base was skipped is
// skipped too. Conflicts are the builder's to find, as leave-outs.
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
		lines := join(s, Caps{Lines: d.cfg.MaxLines, Changes: d.cfg.MaxChanges})
		if len(lines) == 0 {
			return
		}
		id := state.TrainID(fmt.Sprintf("t%d", d.st.Seq()+1))
		if d.apply("runner", state.Assemble{Train: id, Lines: lines}) != nil {
			return
		}
	}
}

// Caps bound one train (J4): at most Lines lines, and at most Changes
// changes in all (a stack is several changes in one line). Zero is no cap.
// A line that alone has more changes than the cap rides alone, first in
// order, so that no line waits for ever.
type Caps struct {
	Lines, Changes int
}

// Join is the joining rule: the lines the next train takes, in order. Shadow
// running uses it to say what the runner would assemble.
func Join(s state.State, c Caps) []state.LineID { return join(s, c) }

// join is the joining rule.
func join(s state.State, c Caps) []state.LineID {
	var out []state.LineID
	taken := map[state.LineID]bool{}
	group, changes := -1, 0
	for _, id := range s.Waiting() {
		l := s.Lines[id]
		n := len(l.Changes)
		if l.Stale != "" || !s.NeedMet(l, taken) {
			continue
		}
		switch {
		case len(out) == 0 && l.Alone:
			return []state.LineID{id}
		case l.Alone:
			continue
		case group >= 0 && l.Group != group:
			continue
		case len(out) == 0 && c.Changes > 0 && n >= c.Changes:
			return []state.LineID{id}
		case c.Changes > 0 && changes+n > c.Changes:
			// Past the cap: skipped, as a line that must ride alone is.
			continue
		}
		group = l.Group
		out = append(out, id)
		taken[id] = true
		changes += n
		if c.Lines > 0 && len(out) >= c.Lines {
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
	d.restack(now, d.st.State())
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
		var lines []adapter.BuildLine
		for _, l := range t.Lines {
			lines = append(lines, adapter.BuildLine{ID: l, Changes: append([]state.Change(nil), s.Lines[l].Changes...)})
		}
		d.poll(t.ID, t.Head, lines, t.Reruns >= 1)
	}
	d.prune(now, s)
}

// pruneEvery is how often a prune runs when the live trains have not
// changed, to catch a branch pushed after its train ended.
const pruneEvery = 10 * time.Minute

// prune deletes the branches of trains that have ended (open question 20),
// through the forge when it can: whenever the live trains change, and every
// pruneEvery. It is housekeeping, outside the machine: it is safe to repeat,
// and a failure is logged and tried again later.
func (d *Daemon) prune(now time.Time, s state.State) {
	p, ok := d.ad.Forge.(adapter.Pruner)
	if !ok || d.pruning {
		return
	}
	live := strings.Join(trainIDs(s.Chain), " ")
	due := live != d.pruned || now.Sub(d.prunedAt) >= pruneEvery
	if d.pruneFailed && now.Sub(d.prunedAt) < time.Minute {
		due = false
	}
	if !due {
		return
	}
	d.pruning, d.prunedAt = true, now
	ids := append([]state.TrainID(nil), s.Chain...)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Call)
		defer cancel()
		err := p.Prune(ctx, ids)
		select {
		case d.results <- result{prune: true, note: live, err: err}:
		case <-d.quit:
		}
	}()
}

func (d *Daemon) pruneDone(r result) {
	d.pruning, d.pruneFailed = false, r.err != nil
	if r.err != nil {
		d.log.Printf("pruning ended trains' branches: %v", r.err)
		return
	}
	d.pruned = r.note
}

func trainIDs(ids []state.TrainID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
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

// poll reads a train's CI. rerunSpent says the train's one rerun after a
// flake or a timing failure is spent, so that the same reading again is
// blamed as deterministic (§5.5).
func (d *Daemon) poll(id state.TrainID, head state.Head, lines []adapter.BuildLine, rerunSpent bool) {
	d.polls[id] = true
	ci := d.ad.CI
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Call)
		defer cancel()
		rep, err := ci.Status(ctx, id, head, lines)
		if err == nil && rerunSpent && rep.Outcome == adapter.Flaky {
			rep = reblame(ctx, ci, id, rep, lines)
		}
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

// reblame turns a known flake or a timing failure that failed again after
// its one rerun into a deterministic failure (§5.5 class 4), and asks blame
// again with that class forced. The rerun's failure is evidence in its own
// right: a line that makes a test slow, or makes a flaky test it touches
// fail every time, is named by its files like any other. A CI that can't
// blame again, or a blame that names nobody, leaves it unblamed: J1.
func reblame(ctx context.Context, ci adapter.CI, id state.TrainID, rep adapter.Report, lines []adapter.BuildLine) adapter.Report {
	out := rep
	out.Outcome, out.Blamed = adapter.Failed, nil
	out.Evidence = "failed again after its one rerun: " + orWhy(rep.Why, orWhy(rep.Evidence, "a known flake or a timeout"))
	out.Why = ""
	rb, ok := ci.(adapter.Reblamer)
	if !ok {
		return out
	}
	got, err := rb.Reblame(ctx, id, out, lines)
	if err != nil || got.Outcome != adapter.Failed {
		out.Evidence += " [blame, asked again, gave no answer]"
		return out
	}
	return got
}

func orWhy(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// pollDone applies what a poll read. A second flake on a train whose one
// rerun is spent is no longer a flake to the machine: the poll has already
// blamed it as deterministic (reblame). If the rerun was spent after the
// poll began, it is a failure nobody is named for, which goes to J1. A poll
// that keeps failing is escalated; the CI deadline voids the train in the
// end.
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
