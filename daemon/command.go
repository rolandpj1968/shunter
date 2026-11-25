package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/store"
)

// Request is one command of the shunt CLI.
type Request struct {
	Cmd    string          `json:"cmd"`
	Args   []string        `json:"args,omitempty"`
	Alone  bool            `json:"alone,omitempty"`
	Action string          `json:"action,omitempty"`
	Reason string          `json:"reason,omitempty"`
	Lines  []string        `json:"lines,omitempty"`
	Doc    *store.Document `json:"doc,omitempty"`
	// Plain is an import from line files (store.LineFile).
	Plain *store.LineFile `json:"plain,omitempty"`
	Actor string          `json:"actor,omitempty"`
	// Until is a park's restore condition (§6.4), for cancel --action park
	// and judge park: "landed:<line>" is one the runner tests, and such a
	// park ends only when that line lands or by a restore.
	Until string `json:"until,omitempty"`
	// Version is the version of the line check a verdict is by.
	Version string `json:"version,omitempty"`
	// Lease is how long a check claim stands, for claim ("20m"); empty
	// takes the configured Claim.
	Lease string `json:"lease,omitempty"`
}

// Response is a command's answer.
type Response struct {
	Output string          `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
	Doc    *store.Document `json:"doc,omitempty"`
}

// Exec runs a command in the loop.
func (d *Daemon) Exec(ctx context.Context, req Request) Response {
	var resp Response
	if err := d.do(ctx, func() { resp = d.exec(ctx, req) }); err != nil {
		return Response{Error: err.Error()}
	}
	return resp
}

// Offline runs a command on a store with no daemon running: the effects it
// asks for stay owed until the daemon starts. Commands that must read the
// forge (queue, import) are refused offline.
func Offline(st *store.Store, req Request) Response {
	d := New(st, adapterNone(), Config{}, nil)
	return d.exec(context.Background(), req)
}

func errResp(err error) Response { return Response{Error: err.Error()} }

func (d *Daemon) exec(ctx context.Context, req Request) Response {
	actor := req.Actor
	if actor == "" {
		actor = "cli"
	}
	switch req.Cmd {
	case "queue":
		return d.queue(ctx, actor, req)
	case "hold":
		if len(req.Args) != 1 || req.Reason == "" {
			return errResp(errors.New("usage: hold <line|change> --reason <why>"))
		}
		l, err := d.findLine(req.Args[0])
		if err != nil {
			return errResp(err)
		}
		return d.act(actor, state.Hold{Line: l, Reason: req.Reason}, "line "+string(l)+" held")
	case "checked":
		// checked <line|change> <trunk> <head>... --action pass|fail|conflict|infra [--reason <evidence, the files in conflict, or why no verdict>]
		if len(req.Args) < 3 || !slices.Contains([]string{"pass", "fail", "conflict", "infra"}, req.Action) || (req.Action != "pass" && req.Reason == "") {
			return errResp(errors.New("usage: checked <line|change> <trunk> <head>... --action pass|fail|conflict|infra [--reason <evidence for a fail, the files for a conflict, why there is no verdict for infra>]"))
		}
		l, err := d.findLine(req.Args[0])
		if err != nil {
			return errResp(err)
		}
		heads := make([]state.Head, 0, len(req.Args)-2)
		for _, h := range req.Args[2:] {
			heads = append(heads, state.Head(h))
		}
		ev := state.LineChecked{Line: l, Heads: heads, Trunk: state.Head(req.Args[1]), Pass: req.Action == "pass", Version: req.Version}
		switch req.Action {
		case "fail":
			ev.Evidence = req.Reason
		case "conflict":
			ev.Conflict = req.Reason
		}
		if v := d.st.State().Checkers[state.CheckerLineCheck]; req.Version != v {
			return errResp(fmt.Errorf("the line check is at version %q, not %q: a verdict of another version is not recorded (declare a new one with checker %s <version>)", v, req.Version, state.CheckerLineCheck))
		}
		if req.Action == "infra" {
			return d.release(actor, l, ev, req.Reason)
		}
		return d.act(actor, ev, "line "+string(l)+" check recorded: "+req.Action)
	case "checker":
		// checker <name> <version>: a checker's present version (docs/queue-as-bag.md §5)
		if len(req.Args) != 2 || !slices.Contains(state.Checkers, req.Args[0]) {
			return errResp(fmt.Errorf("usage: checker %s <version>", strings.Join(state.Checkers, "|")))
		}
		return d.act(actor, state.CheckerChanged{Checker: req.Args[0], Version: req.Args[1]}, "checker "+req.Args[0]+" at version "+req.Args[1])
	case "claim":
		return d.claim(actor, req)
	case "priority":
		return d.priority(actor, req)
	case "handover":
		// handover <line> <new owner> --reason <why>: a line, live or returned.
		if len(req.Args) != 2 || req.Reason == "" {
			return errResp(errors.New("usage: handover <line> <new owner> --reason <why>"))
		}
		if d.st.State().Lines[state.LineID(req.Args[0])] == nil {
			return errResp(fmt.Errorf("no line %q", req.Args[0]))
		}
		return d.act(actor, state.HandOver{Line: state.LineID(req.Args[0]), To: req.Args[1], Why: req.Reason}, "line "+req.Args[0]+" handed over to "+req.Args[1])
	case "restore":
		// restore <park> --reason <why>: the judge ends a park (§6.4).
		if len(req.Args) != 1 || req.Reason == "" {
			return errResp(errors.New("usage: restore <park> --reason <why>"))
		}
		if err := d.st.Note(actor, "restore "+req.Args[0]+": "+req.Reason); err != nil {
			return errResp(err)
		}
		return d.act(actor, state.Restore{Park: state.ParkID(req.Args[0])}, "park "+req.Args[0]+" restored")
	case "release":
		if len(req.Args) != 1 {
			return errResp(errors.New("usage: release <line|change>"))
		}
		l, err := d.findLine(req.Args[0])
		if err != nil {
			return errResp(err)
		}
		return d.act(actor, state.Release{Line: l}, "line "+string(l)+" released")
	case "cancel":
		return d.cancel(actor, req)
	case "requeue":
		return d.requeue(actor, req)
	case "withdraw":
		if len(req.Args) != 1 {
			return errResp(errors.New("usage: withdraw <line|change>"))
		}
		l, err := d.findLine(req.Args[0])
		if err != nil {
			return errResp(err)
		}
		return d.act(actor, state.Withdraw{Line: l}, "line "+string(l)+" withdrawn")
	case "pause":
		if req.Reason == "" {
			return errResp(errors.New("usage: pause --reason <why>"))
		}
		return d.act(actor, state.Pause{Reason: req.Reason}, "paused")
	case "resume":
		var trunk state.Head
		if len(req.Args) == 1 {
			trunk = state.Head(req.Args[0])
		}
		return d.act(actor, state.Resume{Trunk: trunk}, "resumed")
	case "judge":
		return d.judge(actor, req)
	case "status":
		return Response{Output: Status(d.st.State(), d.st.Pending())}
	case "export":
		doc := store.Export(d.st.State())
		return Response{Doc: &doc, Output: fmt.Sprintf("%d lines, %d parks", len(doc.Lines), len(doc.Parks))}
	case "import":
		if req.Plain != nil {
			doc, err := d.fromLines(ctx, *req.Plain)
			if err != nil {
				return errResp(err)
			}
			return d.importDoc(ctx, actor, &doc)
		}
		return d.importDoc(ctx, actor, req.Doc)
	}
	return errResp(fmt.Errorf("unknown command %q", req.Cmd))
}

func (d *Daemon) act(actor string, ev state.Event, ok string) Response {
	if err := d.apply(actor, ev); err != nil {
		return errResp(err)
	}
	d.drive()
	return Response{Output: ok}
}

// claim gives a checker one waiting line's check (§5.4): the line named, or
// the first in the queue that is due one, and records the claim in the
// store, so that no other checker is given it, a restarted one included,
// until its verdict or the lease's end. The output is the arguments that
// checked takes, "<line> <trunk> <head>...", or nothing when no line is due.
func (d *Daemon) claim(actor string, req Request) Response {
	if len(req.Args) > 1 {
		return errResp(errors.New("usage: claim [<line|change>] [--lease 20m] [--version <the checker's>]"))
	}
	if v := d.st.State().Checkers[state.CheckerLineCheck]; req.Version != "" && req.Version != v {
		return errResp(fmt.Errorf("the line check is at version %q, not %q: its verdict would not be recorded (declare it with checker %s <version>)", v, req.Version, state.CheckerLineCheck))
	}
	lease := d.cfg.Claim
	if req.Lease != "" {
		v, err := time.ParseDuration(req.Lease)
		if err != nil || v <= 0 {
			return errResp(fmt.Errorf("lease %q: a duration such as 20m", req.Lease))
		}
		lease = v
	}
	s := d.st.State()
	var l *state.Line
	if len(req.Args) == 1 {
		id, err := d.findLine(req.Args[0])
		if err != nil {
			return errResp(err)
		}
		l = s.Lines[id]
	} else {
		for _, id := range s.Waiting() {
			if c := s.Lines[id]; dueCheck(s, c) && !d.unanswered(c) {
				l = c
				break
			}
		}
		if l == nil {
			return Response{}
		}
	}
	key := state.CheckKey{Trunk: s.Trunk, Checker: s.Checkers[state.CheckerLineCheck]}
	for _, c := range l.Changes {
		key.Heads = append(key.Heads, c.Head)
	}
	ev := state.CheckClaimed{Line: l.ID, Key: key, By: actor, Until: d.now().Add(lease)}
	if err := d.apply(actor, ev); err != nil {
		return errResp(err)
	}
	out := []string{string(l.ID), string(key.Trunk)}
	for _, h := range key.Heads {
		out = append(out, string(h))
	}
	return Response{Output: strings.Join(out, " ")}
}

// release ends a claim whose checker could give no verdict (§5.4): the
// same step as a lease's end, taken at once, and the judge is told. The line
// is not given to a checker that asks for any line for Unanswered, so a
// failed infrastructure isn't asked again every lap.
func (d *Daemon) release(actor string, l state.LineID, v state.LineChecked, why string) Response {
	key := state.CheckKey{Heads: v.Heads, Trunk: v.Trunk, Checker: v.Version}
	if err := d.apply(actor, state.ClaimReleased{Line: l, Key: key, By: actor, Reason: why}); err != nil {
		return errResp(err)
	}
	d.paced[l] = unanswered{key: key, until: d.now().Add(d.cfg.Unanswered)}
	d.alert(fmt.Sprintf("infra:%s:%s:%v", l, key.Trunk, key.Heads), fmt.Sprintf("the line check of %s by %s gave no verdict: %s", l, actor, why))
	d.drive()
	return Response{Output: "line " + string(l) + " claim released, no verdict"}
}

// unanswered reports whether a line's check was given up with no verdict,
// on the key it would be claimed at now, within Unanswered.
func (d *Daemon) unanswered(l *state.Line) bool {
	p, ok := d.paced[l.ID]
	if !ok {
		return false
	}
	if !d.now().Before(p.until) || p.key.Trunk != d.st.State().Trunk || !l.OnHeads(p.key) {
		delete(d.paced, l.ID)
		return false
	}
	return true
}

// priority gives a line a manual priority, the one override of the bag's
// order (docs/queue-as-bag.md §2), or with --action end takes it back.
// --until is a duration from now or a time (RFC 3339).
func (d *Daemon) priority(actor string, req Request) Response {
	const usage = "usage: priority <line|change> --reason <why> --until <2h|RFC 3339 time> | priority <line|change> --action end --reason <why>"
	if len(req.Args) != 1 || req.Reason == "" || (req.Action != "" && req.Action != "end") {
		return errResp(errors.New(usage))
	}
	l, err := d.findLine(req.Args[0])
	if err != nil {
		return errResp(err)
	}
	if req.Action == "end" {
		return d.act(actor, state.PriorityEnded{Line: l, Why: req.Reason}, "line "+string(l)+": priority ended")
	}
	if req.Until == "" {
		return errResp(errors.New(usage))
	}
	until, err := time.Parse(time.RFC3339, req.Until)
	if err != nil {
		v, derr := time.ParseDuration(req.Until)
		if derr != nil || v <= 0 {
			return errResp(fmt.Errorf("until %q: a duration such as 2h, or an RFC 3339 time", req.Until))
		}
		until = d.now().Add(v)
	}
	if !until.After(d.now()) {
		return errResp(fmt.Errorf("until %q is past", req.Until))
	}
	return d.act(actor, state.Prioritise{Line: l, By: actor, Reason: req.Reason, Until: until}, "line "+string(l)+": priority until "+until.UTC().Format(time.RFC3339))
}

// dueCheck reports whether a waiting line is due a line check: it has no
// green check on its present heads, no claim on it stands, it waits for no
// rebase, and its base, if any, is checked or rides already.
func dueCheck(s state.State, l *state.Line) bool {
	if l.State != state.LineWaiting || l.Claim != nil || l.CheckCurrent() || l.Conflict != "" || l.Stale != "" {
		return false
	}
	b := s.Lines[l.Base]
	return b == nil || b.CheckCurrent() || b.State == state.LineRiding || b.State == state.LineLanded
}

// findLine resolves a line's name, or a change of a live line.
func (d *Daemon) findLine(name string) (state.LineID, error) {
	s := d.st.State()
	if l := s.Lines[state.LineID(name)]; l != nil && !l.State.Spent() {
		return l.ID, nil
	}
	for id, l := range s.Lines {
		if l.State.Spent() {
			continue
		}
		for _, c := range l.Changes {
			if string(c.ID) == name {
				return id, nil
			}
		}
	}
	if l := s.Lines[state.LineID(name)]; l != nil {
		return l.ID, nil
	}
	return "", fmt.Errorf("no live line or change %q", name)
}

// queue offers one line: the changes named, in stack order. Its heads and
// its base come from the forge; the gate then runs as an effect.
func (d *Daemon) queue(ctx context.Context, actor string, req Request) Response {
	if len(req.Args) == 0 {
		return errResp(errors.New("usage: queue <change>... [--alone]"))
	}
	if d.ad.Forge == nil {
		return errResp(errors.New("queue needs the forge: run it against the daemon"))
	}
	ids := make([]state.ChangeID, len(req.Args))
	for i, a := range req.Args {
		ids[i] = state.ChangeID(a)
	}
	cctx, cancel := context.WithTimeout(ctx, d.cfg.Call)
	defer cancel()
	info, err := d.ad.Forge.Describe(cctx, ids)
	if err != nil {
		if adapter.IsPermanent(err) {
			// Not found, say: an answer for the producer, not a fault.
			return errResp(fmt.Errorf("refused: %w", err))
		}
		return errResp(fmt.Errorf("reading the changes: %w", err))
	}
	if len(info) != len(ids) {
		return errResp(fmt.Errorf("the forge described %d of %d changes", len(info), len(ids)))
	}
	o := OfferFor(d.st.State(), info, req.Alone)
	o.By = actor
	msg := fmt.Sprintf("line %s offered (%d changes)", o.Line, len(o.Changes))
	if o.Base != "" {
		msg += ", stacked on line " + string(o.Base)
	}
	// The gate answers the offer before the producer is answered (§5.2):
	// an intake passes its refusal on, with its reason.
	_, owed, err := d.st.Apply(actor, o)
	if err != nil {
		if !errors.Is(err, store.ErrRefused) {
			d.fatal = err
		}
		return errResp(err)
	}
	defer d.drive()
	if err := d.gateNow(ctx, o.Line, owed); err != nil {
		return Response{Output: msg + "; the gate could not be read now, and runs again: " + err.Error()}
	}
	if l := d.st.State().Lines[o.Line]; l.State == state.LineReturned {
		return errResp(fmt.Errorf("line %s refused at the gate: %s", o.Line, l.Reason))
	} else if l.State != state.LineOffered {
		msg += ", " + l.State.String()
	}
	return Response{Output: msg}
}

// OfferFor is the offer of a line made of the changes described, in stack
// order: named by its first change, at the heads the forge gave, and stacked
// on the live line that holds the first change's base, if one does.
func OfferFor(s state.State, info []adapter.ChangeInfo, alone bool) state.Offer {
	o := state.Offer{Line: state.LineID(info[0].ID), Alone: alone}
	for _, c := range info {
		o.Changes = append(o.Changes, state.Change{ID: c.ID, Head: c.Head})
	}
	if b := info[0].Base; b != "" {
		if line, ok := liveChanges(s, o.Line)[b]; ok {
			o.Base = line
		}
	}
	return o
}

// requeue is the operator's override (§5.9): requeue <train|line>. A train
// is named by its id; anything else names a line, or a change of one. A
// live train is aborted; the lines go back to waiting, uncharged, with their
// budgets and cycles reset, and nothing parked.
func (d *Daemon) requeue(actor string, req Request) Response {
	if len(req.Args) != 1 || req.Reason == "" {
		return errResp(errors.New("usage: requeue <train|line> --reason <why>"))
	}
	if t := d.st.State().Trains[state.TrainID(req.Args[0])]; t != nil {
		return d.act(actor, state.RequeueByHand{Train: t.ID, Reason: req.Reason}, fmt.Sprintf("train %s: its lines requeued", t.ID))
	}
	l, err := d.findLine(req.Args[0])
	if err != nil {
		return errResp(err)
	}
	return d.act(actor, state.RequeueByHand{Line: l, Reason: req.Reason}, "line "+string(l)+" requeued")
}

// cancel aborts a train (§5.9), named by itself or by one of its lines. The
// action applies to the line named, or to every line when a train is named;
// the train's other lines go back to waiting, the default.
func (d *Daemon) cancel(actor string, req Request) Response {
	if len(req.Args) != 1 || req.Reason == "" {
		return errResp(errors.New("usage: cancel <train|line> --action park|requeue|drop --reason <why>"))
	}
	s := d.st.State()
	var kind state.DisposeKind
	switch req.Action {
	case "", "requeue":
		kind = state.Requeue
	case "park":
		kind = state.ParkIt
	case "drop":
		kind = state.Return
	default:
		return errResp(fmt.Errorf("action %q: park, requeue or drop", req.Action))
	}
	var t *state.Train
	var only state.LineID
	if x := s.Trains[state.TrainID(req.Args[0])]; x != nil {
		t = x
	} else {
		l, err := d.findLine(req.Args[0])
		if err != nil {
			return errResp(err)
		}
		if s.Lines[l].State != state.LineRiding {
			return errResp(fmt.Errorf("line %s is %s, not riding: hold or withdraw it instead", l, s.Lines[l].State))
		}
		t, only = s.Trains[s.Lines[l].Train], l
	}
	disp := state.Dispose{Kind: kind}
	if kind == state.ParkIt {
		disp.Park = state.ParkID("cancel-" + string(t.ID))
		disp.Cond = "deadline " + d.now().Add(d.cfg.Park).UTC().Format("2006-01-02T15:04Z") + ", or a restore by hand"
		if req.Until != "" {
			disp.Cond = req.Until
		}
	}
	lines := map[state.LineID]state.Dispose{}
	for _, id := range t.Lines {
		if only == "" || id == only {
			lines[id] = disp
		}
	}
	return d.act(actor, state.Abort{Train: t.ID, Reason: req.Reason, Lines: lines}, fmt.Sprintf("train %s aborted", t.ID))
}

// judge records a decision at J1: judge <train> charge|split|park|rerun.
func (d *Daemon) judge(actor string, req Request) Response {
	if len(req.Args) != 2 || req.Reason == "" {
		return errResp(errors.New("usage: judge <train> charge|split|park|rerun --reason <why> [--lines a,b]"))
	}
	t := state.TrainID(req.Args[0])
	j := state.Judge{Train: t, Action: state.JudgeAction(req.Args[1]), Why: req.Reason}
	switch j.Action {
	case state.JudgeCharge:
		for _, l := range req.Lines {
			j.Lines = append(j.Lines, state.LineID(l))
		}
		j.Evidence = req.Reason
	case state.JudgePark:
		j.Park = state.ParkID("j1-" + string(t))
		j.Cond = "deadline " + d.now().Add(d.cfg.Park).UTC().Format("2006-01-02T15:04Z") + ", or a restore by hand"
		if req.Until != "" {
			j.Cond = req.Until
		}
	}
	return d.act(actor, j, fmt.Sprintf("train %s: %s", t, j.Action))
}

// importDoc carries a document into the queue (docs/cutover.md): every line
// is offered again and passes the gate now, from the forge; then the parks
// and holds are placed on the lines the gate accepted.
func (d *Daemon) importDoc(ctx context.Context, actor string, doc *store.Document) Response {
	if doc == nil {
		return errResp(errors.New("import: no document"))
	}
	if d.ad.Forge == nil {
		return errResp(errors.New("import needs the forge, for the gate: run it against the daemon"))
	}
	plan, err := store.Import(*doc)
	if err != nil {
		return errResp(err)
	}
	var out []string
	accepted := map[state.LineID]bool{}
	for _, o := range plan.Offers {
		if _, owed, err := d.st.Apply(actor, o); err != nil {
			if !errors.Is(err, store.ErrRefused) {
				d.fatal = err
				return errResp(err)
			}
			out = append(out, fmt.Sprintf("%s: not offered: %v", o.Line, err))
			continue
		} else if err := d.gateNow(ctx, o.Line, owed); err != nil {
			out = append(out, fmt.Sprintf("%s: the gate could not be read; it stays offered: %v", o.Line, err))
			continue
		}
		l := d.st.State().Lines[o.Line]
		if l.State == state.LineReturned {
			out = append(out, fmt.Sprintf("%s: refused at the gate: %s", o.Line, l.Reason))
			continue
		}
		accepted[o.Line] = true
		out = append(out, fmt.Sprintf("%s: %s", o.Line, l.State))
	}
	for _, ev := range plan.After(accepted) {
		if err := d.apply(actor, ev); err != nil {
			out = append(out, fmt.Sprintf("%s: %v", state.Name(ev), err))
		}
	}
	d.drive()
	return Response{Output: strings.Join(out, "\n")}
}

// fromLines reads each line's heads and base from the forge, and so turns
// line files into a document.
func (d *Daemon) fromLines(ctx context.Context, lf store.LineFile) (store.Document, error) {
	if d.ad.Forge == nil {
		return store.Document{}, errors.New("import needs the forge, for the heads and the gate: run it against the daemon")
	}
	return store.FromLines(lf, func(ids []state.ChangeID) ([]store.Described, error) {
		cctx, cancel := context.WithTimeout(ctx, d.cfg.Call)
		defer cancel()
		info, err := d.ad.Forge.Describe(cctx, ids)
		if err != nil {
			return nil, err
		}
		out := make([]store.Described, len(info))
		for i, c := range info {
			out[i] = store.Described{ID: c.ID, Head: c.Head, Base: c.Base}
		}
		return out, nil
	})
}

// gateNow performs a line's gate effect at once, in the loop.
func (d *Daemon) gateNow(ctx context.Context, line state.LineID, owed []store.Keyed) error {
	for _, k := range owed {
		if k.Kind != state.EffGate || k.Line != line {
			continue
		}
		// The gate effect itself, as the loop would start it: staleness,
		// tiers and the gate hook included.
		work, timeout := d.call(d.st.State(), k)
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		r := work(cctx)
		if r.err != nil {
			return r.err
		}
		for i, ev := range r.events {
			var done []string
			if i == 0 {
				done = []string{k.Key}
			}
			d.apply(r.actor, ev, done...)
		}
	}
	return nil
}

// Status prints the queue for a human.
func Status(s state.State, pending []store.Keyed) string {
	var b strings.Builder
	fmt.Fprintf(&b, "trunk %s", s.Trunk)
	if s.Paused {
		fmt.Fprintf(&b, "  PAUSED: %s", s.PauseWhy)
	}
	b.WriteString("\n")
	for i, id := range s.Chain {
		t := s.Trains[id]
		fmt.Fprintf(&b, "train %d  %-6s %-10s lines %s", i+1, id, t.State, joinIDs(t.Lines))
		if t.PR != "" {
			fmt.Fprintf(&b, "  pr %s", t.PR)
		}
		b.WriteString("\n")
	}
	order := []state.LineState{state.LineOffered, state.LineWaiting, state.LineWaitingBase, state.LineHeld, state.LineParked}
	for _, st := range order {
		var ids []state.LineID
		for id, l := range s.Lines {
			if l.State == st {
				ids = append(ids, id)
			}
		}
		s.SortByPolicy(ids)
		for _, id := range ids {
			l := s.Lines[id]
			fmt.Fprintf(&b, "%-16s %s", st, id)
			if l.Base != "" {
				fmt.Fprintf(&b, " on %s", l.Base)
				if bl := s.Lines[l.Base]; st == state.LineWaiting && bl != nil && bl.State != state.LineLanded && bl.State != state.LineRiding {
					fmt.Fprintf(&b, " (waits: %s is %s)", l.Base, bl.State)
				}
			}
			if l.Stale != "" {
				fmt.Fprintf(&b, "  STALE: %s", l.Stale)
			}
			if l.Alone {
				b.WriteString(" alone")
			}
			if c := l.Claim; c != nil {
				fmt.Fprintf(&b, "  checking: %s until %s", c.By, c.Until.UTC().Format("15:04Z"))
			}
			if p := l.Priority; p != nil {
				fmt.Fprintf(&b, "  priority until %s (%s)", p.Until.UTC().Format("15:04Z"), p.Reason)
			}
			switch {
			case l.Starving && l.Stuck != "":
				fmt.Fprintf(&b, "  STARVING, STUCK: %s", l.Stuck)
			case l.Starving:
				b.WriteString("  STARVING: rides alone and first")
			case l.Count > 0:
				fmt.Fprintf(&b, "  cycles: %d", l.Count)
			}
			if l.Reason != "" {
				fmt.Fprintf(&b, "  (%s)", l.Reason)
			}
			b.WriteString("\n")
		}
	}
	for id, p := range s.Parks {
		fmt.Fprintf(&b, "park %s  restore when: %s  (%s)\n", id, p.Cond, p.Reason)
	}
	fmt.Fprintf(&b, "effects owed %d\n", len(pending))
	return b.String()
}

func joinIDs(ids []state.LineID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = string(id)
	}
	return strings.Join(s, " ")
}
