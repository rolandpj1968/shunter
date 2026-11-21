package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

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
		// checked <line|change> <trunk> <head>... --action pass|fail [--reason <evidence>]
		if len(req.Args) < 3 || (req.Action != "pass" && req.Action != "fail") || (req.Action == "fail" && req.Reason == "") {
			return errResp(errors.New("usage: checked <line|change> <trunk> <head>... --action pass|fail [--reason <evidence, for a fail>]"))
		}
		l, err := d.findLine(req.Args[0])
		if err != nil {
			return errResp(err)
		}
		heads := make([]state.Head, 0, len(req.Args)-2)
		for _, h := range req.Args[2:] {
			heads = append(heads, state.Head(h))
		}
		return d.act(actor, state.LineChecked{Line: l, Heads: heads, Trunk: state.Head(req.Args[1]), Pass: req.Action == "pass", Evidence: req.Reason}, "line "+string(l)+" check recorded: "+req.Action)
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
		return errResp(fmt.Errorf("reading the changes: %w", err))
	}
	if len(info) != len(ids) {
		return errResp(fmt.Errorf("the forge described %d of %d changes", len(info), len(ids)))
	}
	o := OfferFor(d.st.State(), info, req.Alone)
	msg := fmt.Sprintf("line %s offered (%d changes)", o.Line, len(o.Changes))
	if o.Base != "" {
		msg += ", stacked on line " + string(o.Base)
	}
	return d.act(actor, o, msg)
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
		cctx, cancel := context.WithTimeout(ctx, d.cfg.Call)
		defer cancel()
		s := d.st.State()
		events, err := gate(cctx, d.ad.Forge, *s.Lines[line], liveChanges(s, line))
		if err != nil {
			return err
		}
		for i, ev := range events {
			var done []string
			if i == 0 {
				done = []string{k.Key}
			}
			d.apply("gate", ev, done...)
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
		sort.Slice(ids, func(i, j int) bool { return s.Lines[ids[i]].Seq < s.Lines[ids[j]].Seq })
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
