package state

import (
	"fmt"
	"strings"
)

// Row is one legal transition. Table is normative: Next refuses to write a
// transition that is not a row (I8), and PROTOCOL.md §5.1 is printed from it.
type Row struct {
	Subject string // "line" or "train"
	From    string
	To      string
	Cause   Cause
	Trigger string // the events that cause it
	Guard   string
	Owner   string // the rule or judgement point that decides
	Default string // what happens if the owner is silent or the guard fails
}

const (
	sLine  = "line"
	sTrain = "train"
)

func lr(from, to LineState, c Cause, trigger, guard, owner, def string) Row {
	return Row{sLine, from.String(), to.String(), c, trigger, guard, owner, def}
}

func tr(from, to TrainState, c Cause, trigger, guard, owner, def string) Row {
	return Row{sTrain, from.String(), to.String(), c, trigger, guard, owner, def}
}

// Table lists every transition of a line and of a train.
var Table = buildTable()

func buildTable() []Row {
	const (
		gOffer   = "not live; its changes are in no live line; its base is known"
		oGate    = "the gate (§5.2)"
		dStay    = "refused; the line stays where it is"
		gHold    = "a reason is given"
		oJudge   = "the judge, or a human"
		gRelease = "it is held"
		oOwner   = "its owner"
		gNotLand = "its train is not landing"
		dRequeue = "the gate decides again"
		oRunner  = "the runner"
		gLive    = "the train is live and not landing"
	)
	rows := []Row{
		// A line.
		lr(LineNone, LineOffered, CauseOffer, "offer", gOffer, "a producer", "refused"),
		lr(LineReturned, LineOffered, CauseOffer, "offer", gOffer+"; budgets reset (§5.10)", "a producer", "refused"),
		lr(LineDropped, LineOffered, CauseOffer, "offer", gOffer+"; budgets reset (§5.10)", "a producer", "refused"),
		lr(LineOffered, LineWaiting, CauseGate, "gate-accept", "its base has landed, or it has none; no park covers it", oGate, "stays offered"),
		lr(LineOffered, LineWaitingBase, CauseGate, "gate-accept", "its base is live", oGate, "stays offered"),
		lr(LineOffered, LineParked, CauseGate, "gate-accept", "the queue is parked and this is not the park's fix", oGate, "stays offered"),
		lr(LineOffered, LineReturned, CauseRefused, "gate-refuse", "a reason is given", oGate, "stays offered"),

		lr(LineWaiting, LineHeld, CauseHold, "hold", gHold, oJudge, dStay),
		lr(LineWaitingBase, LineHeld, CauseHold, "hold", gHold, oJudge, dStay),
		lr(LineParked, LineHeld, CauseHold, "hold", gHold, oJudge, dStay),
		lr(LineRiding, LineHeld, CauseHold, "hold", gHold+"; its train is not landing; the train goes void", oJudge, "refused while landing"),
		lr(LineHeld, LineWaiting, CauseRelease, "release", gRelease, oJudge, "stays held, visible to its owner"),
		lr(LineHeld, LineWaitingBase, CauseRelease, "release", gRelease+"; its base has not landed", oJudge, "stays held"),
		lr(LineHeld, LineParked, CauseRelease, "release", gRelease+"; a park covers it", oJudge, "stays held"),

		lr(LineWaiting, LineParked, CausePark, "park", "the park has a restore condition", "J1, J2 or the judge", "not parked"),
		lr(LineParked, LineWaiting, CauseRestore, "restore", "the park exists; no other park covers the line", "the runner, when the condition holds", "restored at the park's deadline, with an alert"),

		lr(LineWaiting, LineRiding, CauseJoin, "assemble", "not held, not parked; alone if it must be; one split group per train", "the runner's joining rule (§5.3)", "stays waiting"),
		lr(LineRiding, LineReturned, CauseLeftOut, "leave-out", "the train is assembling; a reason is given", "the assembly rules (conflict, caps), or a pre-flight hook's leave-out (§5.4)", "refused without a reason"),
		lr(LineRiding, LineReturned, CauseCharged, "preflight-failed, ci-failed, judge (charge)", "evidence names the line (I6)", "a blame rule (§5.6), or J1", "never charged without evidence"),
		lr(LineRiding, LineReturned, CauseAbort, "abort (return)", gLive, "the judge (§5.9)", "requeue"),
		lr(LineRiding, LineWaiting, CauseRequeue, "any end of its train that is not a landing", "the gate passes it", oGate, dRequeue),
		lr(LineRiding, LineParked, CauseRequeue, "any end of its train that is not a landing", "a park covers it", oGate, dRequeue),
		lr(LineRiding, LineWaiting, CauseAbort, "abort (requeue)", gLive, "the judge (§5.9)", "requeue"),
		lr(LineRiding, LineParked, CauseAbort, "abort (park)", gLive+"; a restore condition is given", "the judge (§5.9)", "requeue"),
		lr(LineRiding, LineLanded, CauseLanded, "push-ok", "its train is landing", oRunner, "none: the push decided"),

		lr(LineWaitingBase, LineWaiting, CauseBaseLanded, "push-ok (of its base)", "no park covers it", oGate, dRequeue),
		lr(LineWaitingBase, LineParked, CauseBaseLanded, "push-ok (of its base)", "a park covers it", oGate, dRequeue),
		lr(LineWaitingBase, LineReturned, CauseBaseLost, "its base is returned or dropped", "always", "rule: a stack lands whole (I3)", "returned with the reason"),

		lr(LineOffered, LineDropped, CauseWithdraw, "withdraw", "", oOwner, dStay),
		lr(LineWaiting, LineDropped, CauseWithdraw, "withdraw", "", oOwner, dStay),
		lr(LineWaitingBase, LineDropped, CauseWithdraw, "withdraw", "", oOwner, dStay),
		lr(LineParked, LineDropped, CauseWithdraw, "withdraw", "", oOwner, dStay),
		lr(LineRiding, LineDropped, CauseWithdraw, "withdraw", gNotLand+"; the train goes void", oOwner, "refused while landing"),

		// A train.
		tr(TrainNone, TrainAssembling, CauseAssemble, "assemble", "not paused; the chain is shorter than D; the train ahead has a head", "the runner (§5.3)", "no train"),
		tr(TrainAssembling, TrainBuilt, CauseBuilt, "built", "it has a line; its head is new", "pre-flight (§5.4)", "void at the builder's deadline"),
		tr(TrainAssembling, TrainEmpty, CauseEmpty, "leave-out (the last line)", "every line was left out, each with a reason", "the assembly rules", ""),
		tr(TrainAssembling, TrainFailed, CauseCharged, "preflight-failed", "blame names some lines, not all (or the only one)", "a blame rule (§5.6)", ""),
		tr(TrainAssembling, TrainJudging, CauseUnblamed, "preflight-failed", "blame names none, or all of several", "J1", "J1's default (T10)"),
		tr(TrainBuilt, TrainTesting, CauseOpened, "opened", "the pull request is on the built head", oRunner, "void at the deadline"),
		tr(TrainTesting, TrainGreen, CausePassed, "ci-passed", "the pass is on the built head", "CI", "no verdict at the deadline"),
		tr(TrainTesting, TrainFailed, CauseCharged, "ci-failed", "blame names some lines, not all (or the only one)", "a blame rule (§5.6)", ""),
		tr(TrainTesting, TrainJudging, CauseUnblamed, "ci-failed", "blame names none, or all of several", "J1", "J1's default (T10)"),
		tr(TrainJudging, TrainFailed, CauseJudged, "judge (charge, split, park), judge-timeout", "a charge has evidence; a split has two lines", "J1", "park if the same test failed before or the train is one line; else split"),
		tr(TrainJudging, TrainTesting, CauseRerun, "judge (rerun)", "it has a head; one rerun", "J1", "no rerun"),
		tr(TrainGreen, TrainLanding, CauseVerified, "landing-verified", "first in the chain; the lander read the pass on this head, every change's head as assembled, and trunk at its base (I9)", oRunner, "void on any mismatch"),
		tr(TrainLanding, TrainLanded, CausePushed, "push-ok", "the forge fast-forwarded trunk", "the forge", "retried while the refusal is transient"),
		tr(TrainLanding, TrainVoid, CausePushNo, "push-refused (permanent)", "trunk is not where the train was built", "rule: no fallback merge (§5.7)", "rebuilt on trunk; J3 is told"),
	}
	for _, from := range []TrainState{TrainAssembling, TrainBuilt, TrainTesting, TrainJudging, TrainGreen} {
		rows = append(rows,
			tr(from, TrainVoid, CauseVoid, voidTriggers(from), "nothing was judged", "rule: no verdict charges nobody (§5.5)", "its lines pass the gate again"),
			tr(from, TrainAborted, CauseAbort, "abort", gLive+"; a reason is given", "the judge (§5.9)", "runs on"),
		)
	}
	return rows
}

func voidTriggers(from TrainState) string {
	t := []string{"hold or withdraw of a member", "head-moved", "the train ahead did not land"}
	switch from {
	case TrainAssembling, TrainBuilt, TrainTesting:
		t = append([]string{"ci-no-verdict"}, t...)
	case TrainGreen:
		t = append([]string{"landing-verified (mismatch)"}, t...)
	}
	return strings.Join(t, "; ")
}

var legal = func() map[string]bool {
	m := map[string]bool{}
	for _, r := range Table {
		m[key(r.Subject, r.From, r.To, r.Cause)] = true
	}
	return m
}()

func key(subject, from, to string, c Cause) string {
	return subject + " " + from + ">" + to + " " + string(c)
}

// Markdown prints the table as it stands in PROTOCOL.md §5.1.
func Markdown(subject string) string {
	var b strings.Builder
	b.WriteString("| # | From | To | Trigger | Guard | Decided by | Safe default |\n|---|---|---|---|---|---|---|\n")
	n := 0
	for _, r := range Table {
		if r.Subject != subject {
			continue
		}
		n++
		p := "L"
		if subject == sTrain {
			p = "T"
		}
		g, d := r.Guard, r.Default
		if g == "" {
			g = "none"
		}
		if d == "" {
			d = "none needed: a rule decides"
		}
		fmt.Fprintf(&b, "| %s%d | %s | %s | %s | %s | %s | %s |\n", p, n, r.From, r.To, r.Trigger, g, r.Owner, d)
	}
	return b.String()
}
