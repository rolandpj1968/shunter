// Package shadow runs Shunter beside a live queue, with no side effect: it
// reads the same inputs, runs the same state machine and the same rules, and
// reports what it would have done and where that differs from what the live
// queue did (docs/cutover.md, "Run in shadow").
//
// It has two halves:
//
//   - History. The live queue's train logs are replayed through the state
//     machine (the train-log adapter), and every effect the machine asks
//     for is logged as one Shunter would perform. For every train that
//     opened a pull request, Shunter reads the CI checks on the train's
//     exact head through its own CI adapter and classifies them by §5.5,
//     and compares that with the outcome the live queue recorded; for every
//     train the live queue landed, it asks whether its own lander would have
//     found the verdict on the head (I9).
//   - Now. The lines the live queue holds waiting, from its line file, go
//     through Shunter's own gate against the forge, and the joining rule
//     says which trains Shunter would assemble from them.
//
// Every adapter shadow is given must be read-only: it calls only Describe
// and Status. The command line gives it a forge client that refuses writes
// before they are sent.
package shadow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/replay"
	"github.com/rolandpj1968/shunter/replay/adapter/trainlog"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/store"
)

// Reader is what shadow may call: reads only.
type Reader interface {
	Describe(ctx context.Context, ids []state.ChangeID) ([]adapter.ChangeInfo, error)
	Status(ctx context.Context, train state.TrainID, head state.Head, lines []adapter.BuildLine) (adapter.Report, error)
}

// Input is everything a shadow run reads.
type Input struct {
	// Logs are the live queue's train logs, in order.
	Logs []trainlog.Log
	// Landed says whether the train with this pull request landed.
	Landed func(pr string) bool
	// Lines are the lines the live queue holds now, as line files: the
	// waiting ones in order with their alone flags, the held ones and the
	// parks. Optional.
	Lines store.LineFile
	// Read is the read-only forge and CI.
	Read Reader
	// Depth, MaxLines and MaxChanges shape the trains Shunter would
	// assemble.
	Depth, MaxLines, MaxChanges int

	// The gate's facts, all optional. They are what a live queue knows that
	// the forge does not say: read from files the shadow never writes.
	//
	// Checks are line-check verdicts (§5.4). With RequireCheck, Shunter's
	// gate takes a line only on a green verdict for the heads it has now.
	Checks       []CheckFact
	RequireCheck bool
	// Owners say which changes are with their owner, being fixed (P3): a
	// line holding one has not been offered again, and is not in the queue.
	Owners []OwnerFact
	// Live is the live queue's own answer for each waiting line: whether it
	// would take the line into a train now. Shunter's answer is compared
	// with it, line by line.
	Live []LiveFact
}

// CheckFact is one line-check verdict: the changes checked as one unit, the
// heads they were checked at, and the trunk they were merged with. Verdict is
// "pass", "fail" (Evidence says what failed) or "conflict" (Evidence says in
// what). Facts are in time order: the last for a line's present heads counts.
type CheckFact struct {
	Changes  []state.ChangeID `json:"changes"`
	Heads    []state.Head     `json:"heads"`
	Trunk    state.Head       `json:"trunk"`
	Verdict  string           `json:"verdict"`
	Evidence string           `json:"evidence,omitempty"`
}

// OwnerFact is one change's owner-state. Fixing says the change was charged
// and its owner has not yet said it is done.
type OwnerFact struct {
	Change state.ChangeID `json:"change"`
	Owner  string         `json:"owner,omitempty"`
	Fixing bool           `json:"fixing"`
	Why    string         `json:"why,omitempty"`
}

// LiveFact is the live queue's answer for one waiting line.
type LiveFact struct {
	Changes []state.ChangeID `json:"changes"`
	Takes   bool             `json:"takes"`
	Why     string           `json:"why,omitempty"`
}

// Train is one train of the history, as the live queue ran it and as
// Shunter reads it.
type Train struct {
	Log     string
	PR      string
	Head    state.Head
	Lines   int
	Old     string // what the live queue recorded: passed, failed, failed-unblamed, ...
	Shunter string // Shunter's reading of the checks on the head
	Landed  bool   // the live queue landed it
	Verdict bool   // Shunter's lander would find the verdict on the head
	Kind    string // "agree", or the kind of divergence
	Note    string
	// Rule names the Shunter rule a divergence follows from, when it is
	// one the protocol means (intended); empty when it is unexplained.
	Rule string
}

// intended names the rule behind each kind of divergence that Shunter means
// to make: it applies policy the live queue lacks (docs/cutover-readiness.md,
// class (a)). Any other kind is unexplained.
var intended = map[string]string{
	"unblamed-without-judge": "I6, J1: blame named nobody, so the judge decides, not a rule",
	"charged-on-no-verdict":  "§5.5: a run with no verdict charges nobody",
	"timing-charged":         "§5.5, §5.8: a timeout alone is rerun once before it is judged",
}

// Refusal is an event of the history the machine refused.
type Refusal struct {
	Segment, Event, Kind, Message string
	// Rule names the Shunter rule that refuses it, when the live queue's
	// act is one Shunter means not to take; empty when unexplained.
	Rule string
}

// refusalRule is the rule behind a refusal Shunter means: the live queue
// acted where Shunter's protocol would not. Empty for any other.
func refusalRule(err error) string {
	msg := err.Error()
	switch {
	case errors.Is(err, state.ErrEvidence):
		return "I6: a charge needs evidence; blame named nobody"
	case !errors.Is(err, state.ErrJoin):
		return ""
	case strings.Contains(msg, "another half of a split"):
		return "J1: the halves of a split ride apart until they land"
	case strings.Contains(msg, "rides alone"):
		return "§5.2: a line marked alone rides alone"
	case strings.Contains(msg, "is starving"):
		return "§5.2: a starving line rides alone and first"
	case strings.Contains(msg, "(I3)"):
		return "I3: a line rides only behind its base"
	case strings.Contains(msg, "conflicts with trunk"):
		return "§5.2: a line in conflict with trunk does not ride"
	case strings.Contains(msg, "no current green line check"):
		return "I10: checked before assembled"
	}
	return ""
}

// Line is one line of the live queue's, through Shunter's gate now.
type Line struct {
	Changes []state.ChangeID
	Alone   bool
	Gate    string // waiting, waiting-for-base, held, parked, refused, unread, with-owner
	Reason  string
	// Check is the line check as Shunter's gate holds it: "green", "none",
	// "conflict", "charged", or "" when no facts were given.
	Check string
	// Takes says Shunter's gate would let the line into a train now.
	Takes bool
	Why   string // why not, when it would not
	// Live is the live queue's answer, when it gave one, and Compare is
	// "agree", "diverge" or "" (no answer to compare).
	Live    *LiveFact
	Compare string
}

// Report is a shadow run's result.
type Report struct {
	Logs       int
	Unrecorded int // logs with no protocol line naming a change
	Trains     []Train
	// Agree, Intended and Diverge count the trains: Intended the
	// divergences a rule of Shunter's explains, Diverge the rest, by kind.
	Agree    int
	Intended map[string]int
	Diverge  map[string]int
	// Effects counts what the machine asked for, replaying the history: what
	// Shunter would have performed, by kind.
	Effects map[string]int
	// Effect is each of them, in order, for the effects log.
	Effect []Effect
	// Refused counts the history's events the machine refused and no rule
	// explains, by event and kind of refusal; Refusals is every refusal.
	Refused  map[string]int
	Refusals []Refusal
	Ignored  map[string]int
	// Now: the waiting lines through the gate, and the trains Shunter would
	// assemble from them.
	Waiting  []Line
	Assemble [][]state.LineID
	// Compared, GateAgree and GateDiverge count the waiting lines for which
	// the live queue gave its own answer, and how Shunter's gate compares.
	Compared, GateAgree, GateDiverge int
	// ReadErrors are reads that failed; nothing is concluded from them.
	ReadErrors []string
}

// Effect is one effect Shunter would perform.
type Effect struct {
	Segment string       `json:"segment"`
	Event   string       `json:"event"`
	Effect  state.Effect `json:"effect"`
}

// outcome is what a train log says became of the train.
type outcome struct {
	pr, head, green string
	lines           int
	opened          bool
	result          string
	reran           bool
}

func readLog(lg trainlog.Log) outcome {
	var o outcome
	unblamed, blamed := false, false
	for _, m := range lg.Markers {
		switch m.Word {
		case "BATCH-READY":
			if len(m.Args) > 0 {
				o.head = m.Args[0]
				o.lines = len(m.Args) - 1
			}
		case "BATCH-OPENED":
			if len(m.Args) >= 2 {
				o.opened, o.pr, o.head = true, m.Args[0], m.Args[1]
				o.result = "open"
			}
		case "BATCH-RERUN":
			o.reran = true
		case "BATCH-GREEN":
			o.result = "passed"
			if len(m.Args) >= 2 {
				o.green = m.Args[1]
			}
		case "BATCH-UNBLAMED":
			unblamed = true
		case "BATCH-BLAME":
			blamed = true
		case "BATCH-FAILED":
			switch {
			case !o.opened:
				o.result = "preflight-failed"
			case unblamed:
				o.result = "failed-unblamed"
			default:
				o.result = "failed-blamed"
			}
		case "BATCH-INFRA":
			o.result = "infra"
		case "BATCH-RUN-CANCELLED":
			o.result = "no-verdict"
		case "BATCH-CANCELLED":
			o.result = "aborted"
		case "BATCH-SPEC-FAILED":
			o.result = "void-ahead-failed"
		case "BATCH-PUSHED":
			o.result = "void-head-moved"
		}
	}
	if blamed && o.result == "open" {
		o.result = "failed-blamed"
	}
	return o
}

var outcomeName = map[adapter.Outcome]string{
	adapter.Pending: "pending", adapter.Passed: "passed", adapter.Failed: "failed",
	adapter.NoVerdict: "no-verdict", adapter.Infra: "infra", adapter.Flaky: "flaky",
}

// compare says whether Shunter's reading agrees with the live queue's
// outcome, and if not, what kind of divergence it is.
func compare(old, sh string, verdict, landed bool, blamedByHook bool) (kind, note string) {
	switch {
	case landed && !verdict:
		return "landed-without-verdict", "the live queue landed it; Shunter's lander finds no passing verdict on its head (I9) and would not land it"
	case old == "passed" && sh == "passed":
		if !landed {
			return "agree", "green, not landed: the train ahead decided it"
		}
		return "agree", ""
	case old == "passed":
		return "green-without-verdict", "the live queue recorded green; Shunter reads " + sh + " on the head"
	case strings.HasPrefix(old, "failed") && sh == "no-verdict":
		return "charged-on-no-verdict", "the live queue judged a failure; the checks say cancelled or skipped: Shunter judges nothing and puts the lines back"
	case strings.HasPrefix(old, "failed") && sh == "flaky":
		return "timing-charged", "only timeouts failed: Shunter reruns once before it judges"
	case strings.HasPrefix(old, "failed") && sh == "passed":
		return "failed-now-passes", "the head passes now: a later run passed"
	case old == "failed-unblamed" && sh == "failed":
		return "unblamed-without-judge", "blame named nobody, and the live queue split or returned the lines; Shunter asks J1 (I6)"
	case old == "failed-blamed" && sh == "failed" && !blamedByHook:
		return "agree", "blamed by the live queue's rules; Shunter needs its blame hook to name the same lines"
	case strings.HasPrefix(old, "failed") && sh == "failed":
		return "agree", ""
	case old == "infra" && sh == "failed":
		return "failure-read-as-infra", "the live queue called it infrastructure; the checks show a failure, which Shunter would send to blame"
	case old == "infra" || old == "no-verdict":
		return "agree", "nobody charged"
	case old == "open" && sh == "pending":
		return "agree", "no outcome in the log, and the required checks have not completed: in flight, or stopped outside the log (Shunter voids it at its CI deadline, no verdict)"
	case old == "open":
		return "unsettled", "the log has no outcome yet; the checks read " + sh
	case old == "aborted" || strings.HasPrefix(old, "void"):
		return "agree", "void or aborted: nothing to judge"
	}
	return "old-" + old + "-shunter-" + sh, ""
}

// Run is a shadow run.
func Run(ctx context.Context, in Input) Report {
	rep := Report{Diverge: map[string]int{}, Intended: map[string]int{}, Effects: map[string]int{}, Refused: map[string]int{}}
	rep.Logs = len(in.Logs)
	read := &cached{r: in.Read, describe: map[state.ChangeID]adapter.ChangeInfo{}}

	// The machine over the history: what Shunter would perform.
	src := &trainlog.Source{Logs: in.Logs, Landed: in.Landed}
	lines := map[state.TrainID][]adapter.BuildLine{}
	offered := map[state.LineID][]state.ChangeID{}
	_, rr := replay.RunObserved(state.New("trunk-at-start", state.Config{Depth: max(in.Depth, 1)}), src, func(seg string, ev state.Event, fx []state.Effect) {
		switch e := ev.(type) {
		case state.Offer:
			offered[e.Line] = nil
			for _, c := range e.Changes {
				offered[e.Line] = append(offered[e.Line], c.ID)
			}
		case state.Assemble:
			for _, id := range e.Lines {
				bl := adapter.BuildLine{ID: id}
				for _, c := range offered[id] {
					bl.Changes = append(bl.Changes, state.Change{ID: c})
				}
				lines[e.Train] = append(lines[e.Train], bl)
			}
		}
		for _, e := range fx {
			if e.Kind == state.EffRecord {
				continue
			}
			k := string(e.Kind)
			if e.Kind == state.EffAskJudge {
				k += " " + e.Point
			}
			rep.Effects[k]++
			rep.Effect = append(rep.Effect, Effect{Segment: seg, Event: state.Name(ev), Effect: e})
		}
	})
	for _, f := range rr.Refused {
		r := Refusal{Segment: f.Segment, Event: f.Event, Kind: replay.Kind(f.Err), Message: f.Err.Error(), Rule: refusalRule(f.Err)}
		rep.Refusals = append(rep.Refusals, r)
		if r.Rule == "" {
			rep.Refused[r.Event+": "+r.Kind]++
		}
	}
	rep.Unrecorded, rep.Ignored = len(src.Unrecorded), src.Ignored

	// Each train's CI, read by Shunter.
	for _, lg := range in.Logs {
		o := readLog(lg)
		if !o.opened {
			continue
		}
		tr := Train{Log: lg.Name, PR: o.pr, Head: state.Head(o.head), Lines: o.lines, Old: o.result}
		if in.Landed != nil {
			tr.Landed = in.Landed(o.pr)
		}
		r, err := read.status(ctx, state.TrainID(lg.Name), tr.Head, read.heads(ctx, lines[state.TrainID(lg.Name)]))
		if err != nil {
			rep.ReadErrors = append(rep.ReadErrors, fmt.Sprintf("%s: checks on %s: %v", lg.Name, short(tr.Head), err))
			tr.Shunter, tr.Kind = "unread", "unread"
		} else {
			tr.Shunter = outcomeName[r.Outcome]
			tr.Verdict = r.Outcome == adapter.Passed && r.Head == tr.Head
			tr.Kind, tr.Note = compare(tr.Old, tr.Shunter, tr.Verdict, tr.Landed, len(r.Blamed) > 0)
			if o.green != "" && o.green != o.head {
				tr.Kind, tr.Note = "green-on-another-head", "the live queue's green names "+short(state.Head(o.green))+", not the head it opened"
			}
			if tr.Old == "passed" && o.reran && tr.Kind == "agree" {
				tr.Note = strings.TrimSpace(tr.Note + " passed on its rerun")
			}
		}
		tr.Rule = intended[tr.Kind]
		switch {
		case tr.Kind == "agree":
			rep.Agree++
		case tr.Rule != "":
			rep.Intended[tr.Kind]++
		default:
			rep.Diverge[tr.Kind]++
		}
		rep.Trains = append(rep.Trains, tr)
	}

	// Now: the lines the live queue holds, through the gate, then its holds
	// and parks placed as an import places them (docs/cutover.md); then the
	// trains from what is left waiting.
	s := state.New("trunk", state.Config{Depth: max(in.Depth, 1), RequireCheck: in.RequireCheck})
	s = ridingTrains(ctx, read, s, in, lines)
	s, rep.Waiting = gateLines(ctx, read, s, in.Lines, in)
	for _, l := range rep.Waiting {
		switch l.Compare {
		case "agree":
			rep.Compared, rep.GateAgree = rep.Compared+1, rep.GateAgree+1
		case "diverge":
			rep.Compared, rep.GateDiverge = rep.Compared+1, rep.GateDiverge+1
		}
	}
	for i := 0; i < max(in.Depth, 1); i++ {
		take := daemon.Join(s, daemon.Caps{Lines: max(in.MaxLines, 1), Changes: in.MaxChanges})
		if len(take) == 0 {
			break
		}
		rep.Assemble = append(rep.Assemble, take)
		id := state.TrainID(fmt.Sprintf("shadow-%d", i+1))
		next, _, err := state.Next(s, state.Assemble{Train: id, Lines: take})
		if err != nil {
			break
		}
		// Built, so the next train may be built on it (I5).
		if next, _, err = state.Next(next, state.Built{Train: id, Head: state.Head("head-of-" + string(id))}); err != nil {
			break
		}
		s = next
	}
	return rep
}

// ridingTrains puts in s, as trains built and riding, the trains the history
// leaves in flight: a pull request open and no end, a green not landed, or
// merged with no pull request yet. A waiting line stacked on one of their
// lines then waits behind it, as it would in Shunter (I3), rather than being refused for a base in
// no live line. A line also in the line files, or whose changes can't be
// read, is left out.
func ridingTrains(ctx context.Context, read *cached, s state.State, in Input, lines map[state.TrainID][]adapter.BuildLine) state.State {
	listed := map[state.ChangeID]bool{}
	for _, pl := range in.Lines.Waiting {
		for _, c := range pl.Changes {
			listed[c] = true
		}
	}
	for _, p := range in.Lines.Parks {
		for _, pl := range p.Lines {
			for _, c := range pl.Changes {
				listed[c] = true
			}
		}
	}
	depth, check := s.Config.Depth, s.Config.RequireCheck
	s.Config.Depth, s.Config.RequireCheck = len(in.Logs)+depth, false
	n := 0
	for _, lg := range in.Logs {
		o := readLog(lg)
		flight := o.result == "open" || (o.result == "" && o.head != "") ||
			(o.result == "passed" && (in.Landed == nil || !in.Landed(o.pr)))
		if !flight {
			continue
		}
		before := s
		var take []state.LineID
		for _, bl := range lines[state.TrainID(lg.Name)] {
			var ids []state.ChangeID
			for _, c := range bl.Changes {
				ids = append(ids, c.ID)
			}
			if len(ids) == 0 || listed[ids[0]] {
				continue
			}
			info, err := read.describeAll(ctx, ids)
			if err != nil {
				continue
			}
			o := daemon.OfferFor(s, info, false)
			next, _, err := state.Next(s, o)
			if err != nil {
				continue
			}
			if next, _, err = state.Next(next, state.GateAccept{Line: o.Line}); err != nil {
				continue
			}
			s = next
			take = append(take, o.Line)
		}
		if len(take) == 0 {
			continue
		}
		n++
		id := state.TrainID(fmt.Sprintf("live-%d", n))
		next, _, err := state.Next(s, state.Assemble{Train: id, Lines: take})
		if err == nil {
			next, _, err = state.Next(next, state.Built{Train: id, Head: state.Head("head-of-" + string(id))})
		}
		if err != nil {
			s = before
			continue
		}
		s = next
	}
	s.Config.Depth, s.Config.RequireCheck = depth, check
	return s
}

// gateLines offers every line of lf, runs the gate on each from the forge,
// and then places the holds and parks on the lines the gate accepted. A line
// whose changes can't be read is reported, and the rest go on.
func gateLines(ctx context.Context, read *cached, s state.State, lf store.LineFile, in Input) (state.State, []Line) {
	var out []Line
	idx := map[state.LineID]int{}
	fixing := map[state.ChangeID]OwnerFact{}
	for _, o := range in.Owners {
		if o.Fixing {
			fixing[o.Change] = o
		}
	}
	// Each line read first: one that can't be is reported and left out.
	keep := func(pl store.PlainLine) bool {
		// A line holding a change that is with its owner, being fixed, has
		// not been offered again (P3): it is not in Shunter's queue at all.
		for _, c := range pl.Changes {
			if o, ok := fixing[c]; ok {
				why := fmt.Sprintf("change %s is with its owner, being fixed", c)
				if o.Owner != "" {
					why = fmt.Sprintf("change %s is with its owner %s, being fixed", c, o.Owner)
				}
				if o.Why != "" {
					why += " (" + o.Why + ")"
				}
				out = append(out, Line{Changes: pl.Changes, Alone: pl.Alone, Gate: "with-owner", Reason: why, Why: why})
				return false
			}
		}
		if _, err := read.describeAll(ctx, pl.Changes); err != nil {
			ln := Line{Changes: pl.Changes, Alone: pl.Alone, Gate: "unread", Reason: err.Error()}
			if adapter.IsPermanent(err) {
				ln.Gate = "refused"
			}
			out = append(out, ln)
			return false
		}
		return true
	}
	var ok store.LineFile
	for _, pl := range lf.Waiting {
		if keep(pl) {
			ok.Waiting = append(ok.Waiting, pl)
		}
	}
	kept := map[string]bool{}
	for _, pl := range ok.Waiting {
		kept[fmt.Sprint(pl.Changes)] = true
	}
	for _, h := range lf.Held {
		if kept[fmt.Sprint(h.Changes)] {
			ok.Held = append(ok.Held, h)
		}
	}
	for _, p := range lf.Parks {
		q := p
		q.Lines = nil
		for _, pl := range p.Lines {
			if keep(pl) {
				q.Lines = append(q.Lines, pl)
			}
		}
		ok.Parks = append(ok.Parks, q)
	}
	doc, err := store.FromLines(ok, func(ids []state.ChangeID) ([]store.Described, error) {
		info, err := read.describeAll(ctx, ids)
		if err != nil {
			return nil, err
		}
		d := make([]store.Described, len(info))
		for i, c := range info {
			d[i] = store.Described{ID: c.ID, Head: c.Head, Base: c.Base}
		}
		return d, nil
	})
	if err == nil {
		var plan store.Plan
		if plan, err = store.Import(doc); err == nil {
			accepted := map[state.LineID]bool{}
			for _, o := range plan.Offers {
				ln := Line{Alone: o.Alone}
				for _, c := range o.Changes {
					ln.Changes = append(ln.Changes, c.ID)
				}
				if o.Base == "" {
					// Its base may ride in a train in flight, which the
					// line files don't hold.
					if info, err := read.describeAll(ctx, []state.ChangeID{o.Changes[0].ID}); err == nil && info[0].Base != "" {
						if b, ok := daemon.LiveChanges(s, o.Line)[info[0].Base]; ok {
							o.Base = b
						}
					}
				}
				next, _, err := state.Next(s, o)
				if err != nil {
					ln.Gate, ln.Reason = "refused", "the offer: "+err.Error()
					out = append(out, ln)
					continue
				}
				s = next
				evs, err := daemon.Gate(ctx, read, *s.Lines[o.Line], daemon.LiveChanges(s, o.Line))
				if err != nil {
					ln.Gate, ln.Reason = "unread", err.Error()
					out = append(out, ln)
					continue
				}
				for _, ev := range evs {
					if next, _, err := state.Next(s, ev); err == nil {
						s = next
					}
				}
				if s.Lines[o.Line].State != state.LineReturned {
					accepted[o.Line] = true
				}
				idx[o.Line] = len(out)
				out = append(out, ln)
			}
			for _, ev := range plan.After(accepted) {
				if next, _, err := state.Next(s, ev); err == nil {
					s = next
				}
			}
		}
	}
	if err != nil {
		out = append(out, Line{Gate: "unread", Reason: "the line files: " + err.Error()})
	}
	// The line checks, as facts: each line's last verdict for the heads it
	// has now goes through the machine as a line-checked event.
	facts := len(in.Checks) > 0 || in.RequireCheck
	for id := range idx {
		if ev, ok := checkOf(s.Lines[id], in.Checks); ok {
			if next, _, err := state.Next(s, ev); err == nil {
				s = next
			}
		}
	}
	for id, i := range idx {
		l := s.Lines[id]
		out[i].Gate, out[i].Reason = l.State.String(), l.Reason
		if l.State == state.LineReturned {
			out[i].Gate = "refused"
			if l.Cause == state.CauseCharged {
				out[i].Gate = "charged"
			}
		}
		if facts {
			out[i].Check = checkName(l)
		}
		out[i].Takes, out[i].Why = takes(s, l)
	}
	live := map[string]*LiveFact{}
	for i := range in.Live {
		live[fmt.Sprint(in.Live[i].Changes)] = &in.Live[i]
	}
	for i := range out {
		lf := live[fmt.Sprint(out[i].Changes)]
		if lf == nil {
			continue
		}
		out[i].Live = lf
		out[i].Compare = "agree"
		if lf.Takes != out[i].Takes {
			out[i].Compare = "diverge"
		}
	}
	return s, out
}

// checkOf is the line-checked event the facts give a line: the last verdict
// on exactly the heads it has now, its changes checked as one unit. A line of
// several changes with no verdict of its own takes a pass when every change
// has a pass alone at its present head: a live queue that checks a composite
// line part by part says no more than that, and the event says so.
func checkOf(l *state.Line, facts []CheckFact) (state.LineChecked, bool) {
	if l == nil || l.State.Spent() || len(facts) == 0 {
		return state.LineChecked{}, false
	}
	heads := make([]state.Head, len(l.Changes))
	for i, c := range l.Changes {
		heads[i] = c.Head
	}
	same := func(f CheckFact, ids []state.ChangeID, hs []state.Head) bool {
		if len(f.Changes) != len(ids) || len(f.Heads) != len(hs) {
			return false
		}
		for i := range ids {
			if f.Changes[i] != ids[i] || f.Heads[i] != hs[i] {
				return false
			}
		}
		return true
	}
	ids := make([]state.ChangeID, len(l.Changes))
	for i, c := range l.Changes {
		ids[i] = c.ID
	}
	for i := len(facts) - 1; i >= 0; i-- {
		f := facts[i]
		if !same(f, ids, heads) {
			continue
		}
		ev := state.LineChecked{Line: l.ID, Heads: heads, Trunk: f.Trunk}
		switch f.Verdict {
		case "pass":
			ev.Pass = true
		case "fail":
			ev.Evidence = orWord(f.Evidence, "the line check failed")
		case "conflict":
			ev.Conflict = orWord(f.Evidence, "it does not merge onto trunk")
		default:
			return state.LineChecked{}, false
		}
		return ev, true
	}
	if len(ids) < 2 {
		return state.LineChecked{}, false
	}
	var trunk state.Head
	for i := range ids {
		found := false
		for j := len(facts) - 1; j >= 0 && !found; j-- {
			if same(facts[j], ids[i:i+1], heads[i:i+1]) {
				if facts[j].Verdict != "pass" {
					return state.LineChecked{}, false
				}
				found, trunk = true, facts[j].Trunk
			}
		}
		if !found {
			return state.LineChecked{}, false
		}
	}
	return state.LineChecked{Line: l.ID, Heads: heads, Trunk: trunk, Pass: true}, true
}

func orWord(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func checkName(l *state.Line) string {
	switch {
	case l.State == state.LineReturned && l.Cause == state.CauseCharged:
		return "charged"
	case l.Conflict != "":
		return "conflict"
	case l.CheckCurrent():
		return "green"
	}
	return "none"
}

// takes says whether Shunter's gate would let the line into a train now, as
// the joining rule tests each line, and why not when it would not.
func takes(s state.State, l *state.Line) (bool, string) {
	switch {
	case l.State == state.LineReturned && l.Cause == state.CauseCharged:
		return false, "charged by its line check: " + l.Reason
	case l.State != state.LineWaiting:
		return false, "it is " + l.State.String()
	case l.Stale != "":
		return false, "stale: " + l.Stale
	case l.Conflict != "":
		return false, "in conflict with trunk: " + l.Conflict
	case l.AtChargedHeads():
		return false, "at the heads it was charged on"
	case s.Config.RequireCheck && !l.CheckCurrent():
		return false, "no green line check on its present heads"
	}
	return true, ""
}

// cached is a read-only reader that reads each change once.
type cached struct {
	r        Reader
	mu       sync.Mutex
	describe map[state.ChangeID]adapter.ChangeInfo
}

func (c *cached) describeAll(ctx context.Context, ids []state.ChangeID) ([]adapter.ChangeInfo, error) {
	return c.Describe(ctx, ids)
}

func (c *cached) Describe(ctx context.Context, ids []state.ChangeID) ([]adapter.ChangeInfo, error) {
	var out []adapter.ChangeInfo
	for _, id := range ids {
		c.mu.Lock()
		info, ok := c.describe[id]
		c.mu.Unlock()
		if !ok {
			got, err := c.r.Describe(ctx, []state.ChangeID{id})
			if err != nil {
				return nil, err
			}
			if len(got) != 1 {
				return nil, fmt.Errorf("the forge described %d of 1 changes", len(got))
			}
			info = got[0]
			c.mu.Lock()
			c.describe[id] = info
			c.mu.Unlock()
		}
		out = append(out, info)
	}
	return out, nil
}

// The adapter.Forge methods a gate never calls.
func (c *cached) OpenPR(context.Context, string, state.TrainID, state.Head, state.Head) (string, error) {
	return "", adapter.Permanent(fmt.Errorf("shadow: no side effects"))
}
func (c *cached) ClosePR(context.Context, string, state.TrainID, string) error {
	return adapter.Permanent(fmt.Errorf("shadow: no side effects"))
}
func (c *cached) ReadLanding(context.Context, state.TrainID, state.Head, []state.ChangeID) (adapter.Reading, error) {
	return adapter.Reading{}, adapter.Permanent(fmt.Errorf("shadow: not read this way"))
}

// heads gives each change of the lines its head, for the blame hook. A
// train log names changes, not their heads, so this is each change's head
// now, read from the forge: the head that rode, unless the change was pushed
// again since. A change that can't be read keeps no head, and blame answers
// no verdict for its line rather than guess.
func (c *cached) heads(ctx context.Context, lines []adapter.BuildLine) []adapter.BuildLine {
	out := make([]adapter.BuildLine, len(lines))
	for i, l := range lines {
		out[i] = adapter.BuildLine{ID: l.ID, Changes: append([]state.Change(nil), l.Changes...)}
		for j, ch := range out[i].Changes {
			if info, err := c.Describe(ctx, []state.ChangeID{ch.ID}); err == nil {
				out[i].Changes[j].Head = info[0].Head
			}
		}
	}
	return out
}

func (c *cached) status(ctx context.Context, t state.TrainID, h state.Head, lines []adapter.BuildLine) (adapter.Report, error) {
	return c.r.Status(ctx, t, h, lines)
}

func short(h state.Head) string {
	if len(h) > 10 {
		return string(h[:10])
	}
	return string(h)
}

// Write prints the report for a human.
func (rep Report) Write(w io.Writer, verbose bool) {
	fmt.Fprintf(w, "History: %d train logs (%d name no change); %d trains opened a pull request.\n", rep.Logs, rep.Unrecorded, len(rep.Trains))
	nIntended := 0
	for _, n := range rep.Intended {
		nIntended += n
	}
	fmt.Fprintf(w, "  CI read by Shunter on each train's head: %d agree, %d diverge, %d intended (a rule of Shunter's).\n", rep.Agree, len(rep.Trains)-rep.Agree-nIntended, nIntended)
	for _, k := range sortedKeys(rep.Diverge) {
		fmt.Fprintf(w, "    %-26s %d\n", k, rep.Diverge[k])
	}
	for _, k := range sortedKeys(rep.Intended) {
		fmt.Fprintf(w, "    intended: %-16s %d  %s\n", k, rep.Intended[k], intended[k])
	}
	landed, verified := 0, 0
	for _, t := range rep.Trains {
		if t.Landed {
			landed++
			if t.Verdict {
				verified++
			}
		}
	}
	fmt.Fprintf(w, "  Landings: %d; Shunter's lander finds the verdict on the head for %d of them.\n", landed, verified)
	for _, t := range rep.Trains {
		if (t.Kind != "agree" && t.Rule == "") || verbose {
			note := t.Note
			if t.Rule != "" {
				note = "intended, " + t.Rule + ": " + note
			}
			fmt.Fprintf(w, "    %-28s pr %-6s head %s  live queue: %-17s Shunter: %-10s %s  %s\n", t.Log, t.PR, short(t.Head), t.Old, t.Shunter, t.Kind, note)
		}
	}
	fmt.Fprintf(w, "  Effects Shunter would have performed, replaying the history:\n")
	for _, k := range sortedKeys(rep.Effects) {
		fmt.Fprintf(w, "    %-26s %d\n", k, rep.Effects[k])
	}
	if len(rep.Refused) > 0 {
		fmt.Fprintf(w, "  Events of the history the machine refuses:\n")
		for _, k := range sortedKeys(rep.Refused) {
			fmt.Fprintf(w, "    %-60s %d\n", k, rep.Refused[k])
		}
	}
	byRule := map[string]int{}
	for _, r := range rep.Refusals {
		if r.Rule == "" {
			fmt.Fprintf(w, "    refused in %s: %s\n", r.Segment, r.Message)
		} else {
			byRule[r.Event+": "+r.Rule]++
		}
	}
	if len(byRule) > 0 {
		fmt.Fprintf(w, "  Events the machine refuses by a rule of Shunter's (intended):\n")
		for _, k := range sortedKeys(byRule) {
			fmt.Fprintf(w, "    %-60s %d\n", k, byRule[k])
		}
		if verbose {
			for _, r := range rep.Refusals {
				if r.Rule != "" {
					fmt.Fprintf(w, "    refused in %s: %s\n", r.Segment, r.Message)
				}
			}
		}
	}
	if len(rep.Ignored) > 0 {
		fmt.Fprintf(w, "  Marker words not understood: %v\n", rep.Ignored)
	}
	if len(rep.Waiting) > 0 || len(rep.Assemble) > 0 {
		count := map[string]int{}
		for _, l := range rep.Waiting {
			count[l.Gate]++
		}
		fmt.Fprintf(w, "Now: %d waiting lines through Shunter's gate: %v.\n", len(rep.Waiting), count)
		for _, l := range rep.Waiting {
			if l.Gate != "waiting" || verbose {
				fmt.Fprintf(w, "    %v  %s  %s\n", l.Changes, l.Gate, l.Reason)
			}
		}
		for i, t := range rep.Assemble {
			fmt.Fprintf(w, "  Train %d would take %d lines: %v\n", i+1, len(t), t)
		}
		if rep.Compared > 0 {
			fmt.Fprintf(w, "Gate comparison: %d waiting lines the live queue answered for: agree %d, DIVERGE %d.\n", rep.Compared, rep.GateAgree, rep.GateDiverge)
			word := map[bool]string{true: "takes", false: "does not take"}
			for _, l := range rep.Waiting {
				if l.Live == nil || (l.Compare == "agree" && !verbose) {
					continue
				}
				mark := "    "
				if l.Compare == "diverge" {
					mark = " ** "
				}
				fmt.Fprintf(w, "%s%v  live queue: %s (%s)  Shunter: %s (%s)  %s\n", mark, l.Changes,
					word[l.Live.Takes], orWord(l.Live.Why, "no reason given"), word[l.Takes], orWord(l.Why, "check "+orWord(l.Check, "not required")), strings.ToUpper(l.Compare))
			}
		}
	}
	if len(rep.ReadErrors) > 0 {
		fmt.Fprintf(w, "Reads that failed (nothing concluded from them): %d\n", len(rep.ReadErrors))
		for _, e := range rep.ReadErrors {
			fmt.Fprintf(w, "    %s\n", e)
		}
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
