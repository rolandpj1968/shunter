package daemon

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
)

// assemble extends the chain with one train when it can (§5.3): not paused,
// shorter than D, no train in it decided red, and the train at its tail has a
// head to build on. The
// joining rule takes waiting lines in the policy's order (state.SortByPolicy)
// and skips a line it cannot take:
// one that must ride alone, one of another half of a split, one past a cap,
// a stale one, and one whose base has not landed, rides in no earlier train
// and was not taken into this one (I3): so a line whose base was skipped is
// skipped too. Conflicts are the builder's to find, as leave-outs.
func (d *Daemon) assemble() {
	for d.fatal == nil {
		s := d.st.State()
		if s.Paused || len(s.Chain) >= s.Config.Depth || s.DecidedRed() != "" {
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

// join is the joining rule. While a line is forced (docs/queue-as-bag.md
// §4), only prioritised lines may ride before it, and then it rides alone,
// with what beneath it still waits, once the chain is empty and it is ready.
func join(s state.State, c Caps) []state.LineID {
	f := s.Forced()
	if f == "" {
		return joinFrom(s, c, s.Waiting())
	}
	prio := s.Prioritised()
	var first []state.LineID
	for _, id := range s.Waiting() {
		if prio[id] {
			first = append(first, id)
		}
	}
	if out := joinFrom(s, c, first); len(out) > 0 {
		return out
	}
	if len(s.Chain) > 0 {
		return nil
	}
	want := s.ForcedTrain(f)
	if got := joinFrom(s, Caps{}, want); len(got) == len(want) {
		return got
	}
	return nil
}

// joinFrom takes lines from ids, which are in the policy's order.
func joinFrom(s state.State, c Caps, ids []state.LineID) []state.LineID {
	var out []state.LineID
	taken := map[state.LineID]bool{}
	group, changes := -1, 0
	counted, full := 0, false // the lines that count against the cap; whether it is reached
	for _, id := range ids {
		l := s.Lines[id]
		n := len(l.Changes)
		if l.Stale != "" || !s.NeedMet(l, taken) {
			continue
		}
		if l.Conflict != "" || l.AtChargedHeads() {
			// In conflict with trunk, or still at the heads it was charged
			// on: it waits for a push.
			continue
		}
		if s.Config.RequireCheck && !l.CheckCurrent() {
			// No current green line check (I10): it waits for one. A line
			// stacked on it waits too, since its need is then unmet.
			continue
		}
		switch {
		case len(out) == 0 && l.Alone:
			return []state.LineID{id}
		case l.Alone:
			continue
		case group >= 0 && l.Group != group:
			continue
		case l.Inert && len(out) > 0:
			// Inert content joins any train whatever its size (§5.4): it
			// can break nothing the train's checks look at, so it counts
			// against neither cap, and joins a train that is already full.
			// (First in a train, it is counted like any line, so that a
			// queue of nothing else still makes trains of the configured
			// size.)
			out = append(out, id)
			taken[id] = true
			continue
		case full:
			// The cap is reached: only inert lines join from here on.
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
		counted++
		if c.Lines > 0 && counted >= c.Lines {
			full = true
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
		case t.Behind != nil:
			// Its failure is in and waits for the train ahead (§5.3): no
			// deadline of its own runs meanwhile.
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
	d.claims(now, s)
	d.priorities(now, s)
	d.starving(now)
	d.reviews(now, s)
	d.restack(now, d.st.State())
	d.versions()
	rerunning := map[state.TrainID]bool{}
	for _, k := range d.st.Pending() {
		if k.Kind == state.EffRerun || k.Kind == state.EffOpenPR {
			rerunning[k.Train] = true
		}
	}
	for _, id := range s.Chain {
		t := s.Trains[id]
		if t.State != state.TrainTesting || t.Behind != nil || d.polls[id] || rerunning[id] || d.ad.CI == nil {
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
	if fs[at].TreeWide {
		// A test that scans the whole tree fails for any change that
		// breaks its set, whatever the change touches: the same failure
		// in an unrelated train is no evidence that trunk is broken, and
		// the queue is not parked on it (§5.6).
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
// runner can test is "landed:<line>": such a park ends when that line lands,
// or by the judge's restore, and never on a clock. Any other condition is
// words the runner cannot test, and has the park's deadline, after which the
// park is restored with an alert.
func (d *Daemon) restores(now time.Time) {
	s := d.st.State()
	for id, p := range s.Parks {
		at, ok := d.st.Since().Parks[id]
		if line, isLanded := strings.CutPrefix(p.Cond, "landed:"); isLanded {
			l := s.Lines[state.LineID(line)]
			switch {
			case l != nil && l.State == state.LineLanded:
				d.apply("runner", state.Restore{Park: id})
				return
			case l != nil && l.State.Spent():
				// The line it waits for was returned or dropped: this offer
				// of it will not land, and the wait cannot end by itself.
				// The judge is told; nothing is restored. (A line not yet
				// offered, or offered again, may still land.)
				d.alert("park-void:"+string(id), fmt.Sprintf("park %s waits for line %s to land, and that line is %s: the wait cannot end; restore the park or park again on another condition", id, line, l.State))
			case ok && now.Sub(at) > d.cfg.Park:
				// A condition the runner can test is never met by a clock
				// (a wait that acted on its timeout released lines whose
				// fix had not landed). Past the deadline the judge is told.
				d.alert("park-late:"+string(id), fmt.Sprintf("park %s has waited past its deadline for line %s to land; it stays parked until that line lands or the judge restores it", id, line))
			}
			continue
		}
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
		if err == nil && rerunSpent && rep.Outcome == adapter.Flaky && !rep.KnownFlake {
			// A timing failure that failed again is evidence, and is blamed.
			// A recorded flake that failed again charges nobody (§5.5): it
			// goes to the machine as it is, which judges nothing.
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
		return evs(state.CIFailed{Train: id, Blamed: r.Blamed, Evidence: r.Evidence, TreeWide: r.TreeWide})
	case adapter.NoVerdict:
		return evs(state.CINoVerdict{Train: id, Why: orWhy(r.Why, "the run gave no verdict")})
	case adapter.Infra:
		return evs(state.CINoVerdict{Train: id, Infra: true, Why: orWhy(r.Why, "the infrastructure failed")})
	case adapter.Flaky:
		return evs(state.CIRerun{Train: id, Why: orWhy(r.Why, "a known flake or a timeout"), Known: r.KnownFlake})
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
		if rr, ok := ev.(state.CIRerun); ok && t.Reruns >= 1 && !rr.Known {
			ev = state.CIFailed{Train: rr.Train, Evidence: "failed again after its one rerun: " + rr.Why}
		}
		d.apply(r.actor, ev)
	}
}

// waitsOnOwner reports whether only the line's owner can move it on: it was
// charged, or it waits stale or in conflict for a rebase.
func waitsOnOwner(l *state.Line) bool {
	switch l.State {
	case state.LineReturned:
		return l.Cause == state.CauseCharged
	case state.LineWaiting:
		return l.Stale != "" || l.Conflict != ""
	}
	return false
}

// ownerLive reports whether the owner has shown a sign of life within
// Alive: its heartbeat file, in Heartbeats, is that fresh. An owner that
// has finished leaves no fresh heartbeat, so it counts as no owner (§7).
// Without Heartbeats, every named owner counts as live.
func (d *Daemon) ownerLive(owner string, now time.Time) bool {
	if d.cfg.Heartbeats == "" {
		return owner != ""
	}
	if owner == "" || owner != filepath.Base(owner) || owner == "." || owner == ".." {
		return false
	}
	fi, err := os.Stat(filepath.Join(d.cfg.Heartbeats, owner))
	if err != nil {
		return false
	}
	return now.Sub(fi.ModTime()) <= d.cfg.Alive
}

// reviews raises what stands too long with nobody looking (L1, L3): a line
// that waits on an owner who is not live, a hold past its review time, and
// a charged line that has stayed with its owner.
// claims ends each check claim whose lease has run out: its checker died or
// hung, and the line may be claimed again. The judge is told (L3: a checker
// that stops is watched).
func (d *Daemon) claims(now time.Time, s state.State) {
	for _, id := range s.Waiting() {
		c := s.Lines[id].Claim
		if c == nil || !now.After(c.Until) {
			continue
		}
		if d.apply("timer", state.ClaimExpired{Line: id}) == nil {
			d.alert(fmt.Sprintf("claim:%s:%s", id, c.Until.UTC().Format(time.RFC3339)), fmt.Sprintf("the line check of %s claimed by %s ran past its lease and was given up: is the checker alive?", id, c.By))
		}
	}
}

// priorities ends each manual priority whose expiry has passed
// (docs/queue-as-bag.md §2): an expired priority is no longer read.
func (d *Daemon) priorities(now time.Time, s state.State) {
	for _, id := range sortedLines(s, slices.Collect(maps.Keys(s.Lines))) {
		if p := s.Lines[id].Priority; p != nil && now.After(p.Until) {
			if err := d.apply("timer", state.PriorityEnded{Line: id, Why: "expired"}); err != nil {
				return
			}
		}
	}
}

// versions records each adapter's checkers' present versions
// (docs/queue-as-bag.md §5): a change makes every verdict of the old one
// stop counting. The line check, run outside, declares its own (command
// checker).
func (d *Daemon) versions() {
	for _, a := range []any{d.ad.Forge, d.ad.CI, d.ad.Git} {
		v, ok := a.(adapter.Versioner)
		if !ok {
			continue
		}
		vs := v.Versions()
		for _, name := range state.Checkers {
			if ver, ok := vs[name]; ok && ver != d.st.State().Checkers[name] {
				if d.apply("runner", state.CheckerChanged{Checker: name, Version: ver}) != nil {
					return
				}
			}
		}
	}
}

// starving marks each waiting line past a starvation threshold
// (docs/queue-as-bag.md §4), and raises to the judge, at once, a starving
// line the runner could not rebase (it needs a person) and StarveAlert or
// more starving lines at once (J4: the thresholds may be too low).
func (d *Daemon) starving(now time.Time) {
	since := d.st.Since()
	for _, id := range d.st.State().Waiting() {
		l := d.st.State().Lines[id]
		if l.Starving {
			continue
		}
		var why string
		switch q := since.QueueTime(id, now); {
		case d.cfg.StarveCycles > 0 && l.Count >= d.cfg.StarveCycles:
			why = fmt.Sprintf("%d cycles it did not cause", l.Count)
		case d.cfg.StarveAfter > 0 && q >= d.cfg.StarveAfter:
			why = fmt.Sprintf("%s of queue time", q.Round(time.Minute))
		default:
			continue
		}
		if d.apply("timer", state.Starving{Line: id, Why: why}) != nil {
			return
		}
	}
	s := d.st.State()
	var starving []string
	for _, id := range s.Waiting() {
		l := s.Lines[id]
		if !l.Starving {
			continue
		}
		starving = append(starving, string(id))
		if l.Stuck != "" {
			d.alert(fmt.Sprintf("stuck:%s:%s", id, l.Conflict), fmt.Sprintf("line %s is starving and stuck: it conflicts with trunk (%s) and the runner could not rebase it (%s); it needs a person, or a hand-over", id, l.Conflict, l.Stuck))
		}
	}
	if d.cfg.StarveAlert > 0 && len(starving) >= d.cfg.StarveAlert {
		d.alert("starving:"+strings.Join(starving, ","), fmt.Sprintf("J4: %d lines are starving at once (%s): one rides at a time; raise the thresholds if the queue is only behind", len(starving), strings.Join(starving, ", ")))
	}
}

// reviews raises what stands too long with nobody looking (L1, L3): a hold
// past its review time, and a charged line that has stayed with its owner.
// Both are alerts, each repeated once every period that passes, and neither
// acts: a hold ends only by its release, and a line leaves its owner only by
// its owner's offer or a recorded hand-over (a wait never acts on a clock).
func (d *Daemon) reviews(now time.Time, s state.State) {
	since := d.st.Since().Lines
	for id, l := range s.Lines {
		at, ok := since[id]
		if !ok {
			continue
		}
		age := now.Sub(at)
		switch {
		case waitsOnOwner(l) && age > d.cfg.Ownerless && !d.ownerLive(l.Owner, now):
			n := int((age - d.cfg.Ownerless) / d.cfg.Owner)
			who := l.Owner
			if who == "" {
				who = "nobody on record"
			}
			d.alert(fmt.Sprintf("ownerless:%s:%d", id, n), fmt.Sprintf("line %s has waited %s on its owner (%s), who is not live: it has no owner; hand it over", id, age.Round(time.Minute), who))
		case l.State == state.LineHeld && age > d.cfg.Review:
			n := int(age / d.cfg.Review)
			d.alert(fmt.Sprintf("hold:%s:%d", id, n), fmt.Sprintf("line %s has been held for %s (%s): look at it again; release it, or it stays held", id, age.Round(time.Minute), l.Reason))
		case l.State == state.LineReturned && l.Cause == state.CauseCharged && age > d.cfg.Owner:
			n := int(age / d.cfg.Owner)
			who := l.Owner
			if who == "" {
				who = "nobody on record"
			}
			d.alert(fmt.Sprintf("owner:%s:%d", id, n), fmt.Sprintf("line %s was charged %s ago and is still with its owner (%s), not offered again: ask, or hand it over", id, age.Round(time.Minute), who))
		}
	}
}
