package state

import (
	"fmt"
	"sort"
)

// Next applies one event. It returns the new state and the effects the runner
// must perform, or an error and the state unchanged. It is pure: the same
// state and event always give the same answer, and nothing happens outside.
func Next(s State, ev Event) (State, []Effect, error) {
	if s.Lines == nil {
		return s, nil, fmt.Errorf("%w: a state must come from New", ErrInput)
	}
	m := &machine{s: s.clone()}
	if err := m.apply(ev); err != nil {
		return s, nil, fmt.Errorf("%s: %w", ev.event(), err)
	}
	for _, e := range m.fx {
		if e.Kind != EffRecord {
			continue
		}
		subject, id := sLine, string(e.Line)
		if e.Line == "" {
			subject, id = sTrain, string(e.Train)
		}
		if !legal[key(subject, e.From, e.To, e.Cause)] {
			return s, nil, fmt.Errorf("%s: %w: %s %s %s to %s (%s)", ev.event(), ErrTable, subject, id, e.From, e.To, e.Cause)
		}
	}
	return m.s, m.fx, nil
}

type machine struct {
	s  State
	fx []Effect
}

func bad(err error, format string, a ...any) error {
	return fmt.Errorf("%w: %s", err, fmt.Sprintf(format, a...))
}

func (m *machine) emit(e Effect) { m.fx = append(m.fx, e) }

func (m *machine) line(id LineID) (*Line, error) {
	l, ok := m.s.Lines[id]
	if !ok || l.State == LineNone {
		return nil, bad(ErrUnknown, "line %q", id)
	}
	return l, nil
}

func (m *machine) train(id TrainID) (*Train, error) {
	t, ok := m.s.Trains[id]
	if !ok {
		return nil, bad(ErrUnknown, "train %q", id)
	}
	return t, nil
}

// setLine is the only place a line changes state. It records the transition,
// tells the owner where the protocol says the owner is told, and moves the
// lines stacked on this one when it is spent: a waiting line whose base is
// returned or dropped goes back too, uncharged (I3). One that rides goes
// back when its train ends (place), and one that is held or parked when it
// is released or restored.
func (m *machine) setLine(l *Line, to LineState, c Cause, reason string) {
	from := l.State
	l.State, l.Cause, l.Reason = to, c, reason
	if to != LineRiding {
		l.Train = ""
	}
	if to != LineParked && to != LineHeld {
		l.Park = ""
	}
	m.emit(Effect{Kind: EffRecord, Line: l.ID, From: from.String(), To: to.String(), Cause: c, Reason: reason})
	switch to {
	case LineHeld, LineParked, LineReturned, LineLanded, LineDropped:
		m.emit(Effect{Kind: EffNotify, Line: l.ID, Reason: to.String() + ": " + reason})
	}
	if !to.Spent() {
		return
	}
	if to == LineLanded {
		l.NoVerdicts, l.Group = 0, 0
	}
	if to == LineReturned && c == CauseCharged {
		l.ChargedAt = l.ChargedAt[:0]
		for _, ch := range l.Changes {
			l.ChargedAt = append(l.ChargedAt, ch.Head)
		}
	}
	for _, id := range m.s.lineIDs() {
		d := m.s.Lines[id]
		if d.Base != l.ID {
			continue
		}
		switch {
		case d.State == LineWaitingBase && to == LineLanded:
			m.place(d, CauseBaseLanded) // a line queued before needs first
		case (d.State == LineWaiting || d.State == LineWaitingBase) && to != LineLanded:
			m.setLine(d, LineReturned, CauseBaseLost, baseLost(l))
		}
	}
}

func baseLost(base *Line) string {
	return fmt.Sprintf("its base %s did not land (%s): rebase and offer it again", base.ID, base.State)
}

func (m *machine) setTrain(t *Train, to TrainState, c Cause, reason string) {
	from := t.State
	t.State, t.Reason = to, reason
	m.emit(Effect{Kind: EffRecord, Train: t.ID, From: from.String(), To: to.String(), Cause: c, Reason: reason})
}

func (m *machine) allPark() *Park {
	for _, p := range m.s.Parks {
		if p.All {
			return p
		}
	}
	return nil
}

// place is the gate. Every path that puts a line back in the queue comes
// through here, so a gate condition holds at every transition, not only at
// the offer (§5.2). A line whose base is live waits in the queue; the
// joining rule takes it only behind its base (I3). A line whose base was
// returned or dropped passes into the queue and goes back at once.
func (m *machine) place(l *Line, c Cause) {
	if l.Base != "" {
		if b := m.s.Lines[l.Base]; b.State == LineReturned || b.State == LineDropped {
			if l.State != LineWaiting {
				m.setLine(l, LineWaiting, c, "")
			}
			m.setLine(l, LineReturned, CauseBaseLost, baseLost(b))
			return
		}
	}
	if l.Park != "" && m.s.Parks[l.Park] == nil {
		l.Park = ""
	}
	if p := m.allPark(); l.Park == "" && p != nil && l.Fix != p.ID {
		l.Park = p.ID
	}
	if l.Park != "" {
		p := m.s.Parks[l.Park]
		if l.State != LineParked {
			park := l.Park
			m.setLine(l, LineParked, c, p.Reason+"; restored when: "+p.Cond)
			l.Park = park
		}
		return
	}
	m.setLine(l, LineWaiting, c, "")
}

// end finishes a train that will not land. Each of its lines is disposed of
// by dispose, or passes the gate again. Every train behind it in the chain is
// void: the head it was built on will never exist (I5).
func (m *machine) end(t *Train, to TrainState, c Cause, reason string, dispose func(*Line) bool) {
	idx := -1
	for i, id := range m.s.Chain {
		if id == t.ID {
			idx = i
		}
	}
	var behind []TrainID
	if idx >= 0 {
		behind = append(behind, m.s.Chain[idx+1:]...)
		m.s.Chain = m.s.Chain[:idx]
	}
	m.finish(t, to, c, reason, dispose)
	for _, id := range behind {
		m.finish(m.s.Trains[id], TrainVoid, CauseVoid, fmt.Sprintf("train %s ahead of it did not land", t.ID), nil)
	}
}

func (m *machine) finish(t *Train, to TrainState, c Cause, reason string, dispose func(*Line) bool) {
	if t.State >= TrainBuilt {
		m.emit(Effect{Kind: EffCancelRun, Train: t.ID, Reason: reason})
		m.emit(Effect{Kind: EffClosePR, Train: t.ID, Reason: reason})
	}
	lines := t.Lines
	t.Behind = nil // judged now, or void with the train ahead: either way it is no longer waiting
	m.setTrain(t, to, c, reason)
	for _, id := range lines {
		l := m.s.Lines[id]
		if l.State != LineRiding || l.Train != t.ID {
			continue
		}
		if dispose != nil && dispose(l) {
			continue
		}
		m.place(l, CauseRequeue)
	}
}

func (m *machine) pause(point, reason string, t TrainID) {
	if !m.s.Paused {
		m.s.Paused, m.s.PauseWhy = true, reason
	}
	m.emit(Effect{Kind: EffAskJudge, Point: point, Train: t, Reason: reason})
}

// askVerify asks the lander to check the first train once it is green.
func (m *machine) askVerify() {
	if len(m.s.Chain) == 0 {
		return
	}
	t := m.s.Trains[m.s.Chain[0]]
	if t.State == TrainGreen && !t.Asked {
		t.Asked = true
		m.emit(Effect{Kind: EffVerify, Train: t.ID, Head: t.Head})
	}
}

func distinct(ids []LineID) bool {
	seen := map[LineID]bool{}
	for _, id := range ids {
		if seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func (m *machine) apply(ev Event) error {
	switch e := ev.(type) {
	case Offer:
		return m.offer(e)
	case GateAccept:
		l, err := m.line(e.Line)
		if err != nil {
			return err
		}
		if l.State != LineOffered {
			return bad(ErrState, "line %s is %s", l.ID, l.State)
		}
		m.place(l, CauseGate)
	case GateRefuse:
		l, err := m.line(e.Line)
		if err != nil {
			return err
		}
		if l.State != LineOffered {
			return bad(ErrState, "line %s is %s", l.ID, l.State)
		}
		if e.Reason == "" {
			return bad(ErrReason, "a refusal at the gate")
		}
		m.setLine(l, LineReturned, CauseRefused, e.Reason)
	case Hold:
		return m.hold(e)
	case Release:
		l, err := m.line(e.Line)
		if err != nil {
			return err
		}
		if l.State != LineHeld {
			return bad(ErrState, "line %s is %s", l.ID, l.State)
		}
		m.place(l, CauseRelease)
	case ParkLines:
		return m.park(e)
	case Restore:
		p, ok := m.s.Parks[e.Park]
		if !ok {
			return bad(ErrUnknown, "park %q", e.Park)
		}
		delete(m.s.Parks, p.ID)
		for _, id := range m.s.lineIDs() {
			if l := m.s.Lines[id]; l.Park == p.ID && l.State == LineParked {
				l.Park = ""
				m.place(l, CauseRestore)
			}
		}
	case Withdraw:
		return m.withdraw(e)
	case LineChecked:
		return m.lineChecked(e)
	case HandOver:
		l, err := m.line(e.Line)
		if err != nil {
			return err
		}
		if e.To == "" || e.Why == "" {
			return bad(ErrReason, "a hand-over names the new owner and says why")
		}
		l.Owner = e.To
		m.emit(Effect{Kind: EffNotify, Line: l.ID, Reason: "handed over to " + e.To + ": " + e.Why})
	case RebaseFailed:
		l, err := m.line(e.Line)
		if err != nil {
			return err
		}
		if l.Conflict == "" || l.State.Spent() {
			return bad(ErrState, "line %s is not in conflict", l.ID)
		}
		if e.Reason == "" {
			return bad(ErrReason, "a rebase that failed")
		}
		m.emit(Effect{Kind: EffNotify, Line: l.ID, Reason: "conflicts with trunk (" + l.Conflict + ") and was not rebased by the runner: " + e.Reason})
	case Assemble:
		return m.assemble(e)
	case LeaveOut:
		return m.leaveOut(e)
	case PreflightFailed:
		t, err := m.train(e.Train)
		if err != nil {
			return err
		}
		if t.State != TrainAssembling || t.Behind != nil {
			return bad(ErrState, "train %s is %s%s", t.ID, t.State, waits(t))
		}
		return m.failed(t, e.Blamed, e.Evidence)
	case CIFailed:
		t, err := m.train(e.Train)
		if err != nil {
			return err
		}
		if t.State != TrainTesting || t.Behind != nil {
			return bad(ErrState, "train %s is %s%s", t.ID, t.State, waits(t))
		}
		return m.failed(t, e.Blamed, e.Evidence)
	case Built:
		t, err := m.train(e.Train)
		if err != nil {
			return err
		}
		if t.State != TrainAssembling || t.Behind != nil {
			return bad(ErrState, "train %s is %s%s", t.ID, t.State, waits(t))
		}
		if e.Head == "" || e.Head == t.BaseHead {
			return bad(ErrHead, "train %s built at %q on %q", t.ID, e.Head, t.BaseHead)
		}
		t.Head = e.Head
		m.setTrain(t, TrainBuilt, CauseBuilt, "")
		m.emit(Effect{Kind: EffOpenPR, Train: t.ID, Head: t.Head, Expect: t.BaseHead})
	case Opened:
		t, err := m.train(e.Train)
		if err != nil {
			return err
		}
		if t.State != TrainBuilt {
			return bad(ErrState, "train %s is %s", t.ID, t.State)
		}
		if e.Head != t.Head {
			return bad(ErrHead, "train %s opened at %s, built at %s", t.ID, e.Head, t.Head)
		}
		t.PR = e.PR
		m.setTrain(t, TrainTesting, CauseOpened, "")
	case CIPassed:
		t, err := m.train(e.Train)
		if err != nil {
			return err
		}
		if t.State != TrainTesting || t.Behind != nil {
			return bad(ErrState, "train %s is %s%s", t.ID, t.State, waits(t))
		}
		if e.Head != t.Head {
			return bad(ErrHead, "train %s passed at %s, built at %s", t.ID, e.Head, t.Head)
		}
		t.Verdict = e.Head
		m.setTrain(t, TrainGreen, CausePassed, "")
		m.askVerify()
	case CIRerun:
		t, err := m.train(e.Train)
		if err != nil {
			return err
		}
		if t.State != TrainTesting || t.Behind != nil {
			return bad(ErrState, "train %s is %s%s", t.ID, t.State, waits(t))
		}
		if t.Reruns >= 1 {
			if e.Known {
				// A recorded flake failed again after its one rerun. It
				// charges nobody, whatever the train's size: the test is
				// known to fail on code where it should pass, so its failure
				// names no line. Nothing is judged; the lines go back, and
				// their no-verdict budget brings a test that keeps failing
				// to the judge (J3, J5).
				return m.noVerdict(CINoVerdict{Train: t.ID, Why: "a recorded flake failed again after its rerun, which charges nobody: " + e.Why})
			}
			return bad(ErrBudget, "train %s was rerun already", t.ID)
		}
		t.Reruns++
		m.emit(Effect{Kind: EffRerun, Train: t.ID, Reason: e.Why})
	case CINoVerdict:
		return m.noVerdict(e)
	case Judge:
		return m.judge(e)
	case JudgeTimeout:
		t, err := m.train(e.Train)
		if err != nil {
			return err
		}
		if t.State != TrainJudging {
			return bad(ErrState, "train %s is %s", t.ID, t.State)
		}
		switch {
		case e.SameTestBefore:
			// The same failure in the train before, with unrelated lines:
			// trunk or a shared fixture is suspect (J2). The queue parks.
			return m.judge(Judge{Train: t.ID, Action: JudgePark, Park: ParkID("j1-" + string(t.ID)), Cond: "deadline: the judge reviews", Why: "J1 default: park"})
		case len(t.Lines) < 2:
			// One line, judged at the head of the chain: its base is trunk
			// itself, which is tested (I1), and the failure was not seen in
			// the train before. The line is the only change there is, and
			// that is its evidence (I6). Parking the whole queue for it, as
			// 0.2 did, stopped every line for one line's failure, and when
			// the park's deadline restored it the line rode and failed again.
			return m.judge(Judge{Train: t.ID, Action: JudgeCharge, Lines: append([]LineID(nil), t.Lines...),
				Evidence: "by elimination: the only line of a train built on tested trunk; the failure was not seen in the train before",
				Why:      "J1 default: charge by elimination"})
		}
		return m.judge(Judge{Train: t.ID, Action: JudgeSplit, Why: "J1 default: split"})
	case HeadMoved:
		return m.headMoved(e)
	case Abort:
		return m.abort(e)
	case LandingVerified:
		return m.verified(e)
	case PushOK:
		t, err := m.train(e.Train)
		if err != nil {
			return err
		}
		if t.State != TrainLanding {
			return bad(ErrState, "train %s is %s", t.ID, t.State)
		}
		m.s.Chain = m.s.Chain[1:]
		m.s.Trunk = t.Head
		m.s.Landings = append(m.s.Landings, Landing{Head: t.Head, Train: t.ID})
		m.setTrain(t, TrainLanded, CausePushed, "")
		m.emit(Effect{Kind: EffClosePR, Train: t.ID, Reason: "landed"})
		for _, id := range t.Lines {
			l := m.s.Lines[id]
			why := "at " + string(t.Head)
			if l.Late != "" {
				why += fmt.Sprintf("; the later push %s came after the landing check and is not on trunk", l.Late)
			}
			m.setLine(l, LineLanded, CauseLanded, why)
		}
		if err := m.judgeDeferred(); err != nil {
			return err
		}
		m.askVerify()
	case PushRefused:
		t, err := m.train(e.Train)
		if err != nil {
			return err
		}
		if t.State != TrainLanding {
			return bad(ErrState, "train %s is %s", t.ID, t.State)
		}
		if e.Why == "" {
			return bad(ErrReason, "a refused push")
		}
		if e.Permanent {
			m.end(t, TrainVoid, CausePushNo, e.Why, nil)
			m.emit(Effect{Kind: EffAskJudge, Point: "J3", Train: t.ID, Reason: "the fast-forward was refused: " + e.Why})
			return nil
		}
		t.Pushes++
		if t.Pushes > m.s.Config.PushRetries {
			m.pause("J3", "the push keeps failing: "+e.Why, t.ID)
			return nil
		}
		m.emit(Effect{Kind: EffPush, Train: t.ID, Head: t.Head, Expect: t.BaseHead})
	case Pause:
		if e.Reason == "" {
			return bad(ErrReason, "a pause")
		}
		if m.s.Paused {
			return bad(ErrState, "already paused")
		}
		m.s.Paused, m.s.PauseWhy = true, e.Reason
	case Resume:
		if !m.s.Paused {
			return bad(ErrState, "not paused")
		}
		if e.Trunk != "" && e.Trunk != m.s.Trunk {
			if len(m.s.Chain) > 0 {
				return bad(ErrChain, "trunk moved under %d live trains; abort them first", len(m.s.Chain))
			}
			m.s.Trunk = e.Trunk
			m.s.Landings = append(m.s.Landings, Landing{Head: e.Trunk, Foreign: true})
		}
		m.s.Paused, m.s.PauseWhy = false, ""
	case Stale:
		l, err := m.line(e.Line)
		if err != nil {
			return err
		}
		if e.Reason == "" {
			return bad(ErrReason, "a stale line")
		}
		switch l.State {
		case LineOffered, LineWaiting, LineWaitingBase, LineHeld, LineParked:
		default:
			return bad(ErrState, "line %s is %s", l.ID, l.State)
		}
		l.Stale = e.Reason
		m.emit(Effect{Kind: EffNotify, Line: l.ID, Reason: "stale, waits for a rebase: " + e.Reason})
	case Fresh:
		l, err := m.line(e.Line)
		if err != nil {
			return err
		}
		if l.Stale == "" || l.State.Spent() {
			return bad(ErrState, "line %s is not stale", l.ID)
		}
		l.Stale = ""
	case ExpireBudget:
		l, err := m.line(e.Line)
		if err != nil {
			return err
		}
		if l.State.Spent() {
			return bad(ErrState, "line %s is %s", l.ID, l.State)
		}
		l.NoVerdicts = 0
	default:
		return bad(ErrInput, "unknown event %T", ev)
	}
	return nil
}

func (m *machine) offer(e Offer) error {
	if e.Line == "" || len(e.Changes) == 0 {
		return bad(ErrInput, "a line needs a name and a change")
	}
	seen := map[ChangeID]bool{}
	for _, c := range e.Changes {
		if c.ID == "" || c.Head == "" || seen[c.ID] {
			return bad(ErrInput, "change %q at %q", c.ID, c.Head)
		}
		seen[c.ID] = true
	}
	old := m.s.Lines[e.Line]
	from := LineNone
	if old != nil {
		from = old.State
		if from == LineLanded {
			return bad(ErrState, "line %s has landed", e.Line)
		}
		if !from.Spent() {
			return bad(ErrDuplicate, "line %s is %s", e.Line, from)
		}
	}
	for _, o := range m.s.Lines {
		if o.State.Spent() || o.ID == e.Line {
			continue
		}
		for _, c := range o.Changes {
			if seen[c.ID] {
				return bad(ErrDuplicate, "change %s is in line %s, which is %s", c.ID, o.ID, o.State)
			}
		}
	}
	if e.Base != "" {
		if e.Base == e.Line {
			return bad(ErrInput, "a line cannot be its own base")
		}
		if _, err := m.line(e.Base); err != nil {
			return err
		}
		// Needs are acyclic: ancestry is. A base whose own bases lead back
		// to this line would leave both waiting for ever.
		for b, n := m.s.Lines[e.Base], 0; b != nil && b.Base != "" && n <= len(m.s.Lines); b, n = m.s.Lines[b.Base], n+1 {
			if b.Base == e.Line {
				return bad(ErrInput, "line %s would need itself, through line %s", e.Line, b.ID)
			}
		}
	}
	l := &Line{ID: e.Line, State: from, Seq: m.s.NextSeq, Changes: append([]Change(nil), e.Changes...), Base: e.Base, Alone: e.Alone, Fix: e.Fix, Owner: e.By}
	if old != nil {
		// The owner-state (§7): a returned line is with its owner until its
		// owner says it is done, by offering it again. A push is not that:
		// anyone's push, a rebase, a half-made fix all move the head.
		if old.Owner != "" {
			l.Owner = old.Owner
		}
		if old.State == LineReturned && old.Cause == CauseCharged {
			if old.Owner != "" && e.By != old.Owner {
				return bad(ErrOwner, "line %s was charged and is with %s, who alone offers it again (or hands it over)", e.Line, old.Owner)
			}
			l.ChargedAt = append([]Head(nil), old.ChargedAt...)
			if l.AtChargedHeads() {
				return bad(ErrUnfixed, "line %s is at the heads it was charged on: a fix is a push", e.Line)
			}
		}
	}
	m.s.NextSeq++
	m.s.Lines[e.Line] = l
	m.setLine(l, LineOffered, CauseOffer, "")
	m.emit(Effect{Kind: EffGate, Line: l.ID})
	return nil
}

func (m *machine) hold(e Hold) error {
	l, err := m.line(e.Line)
	if err != nil {
		return err
	}
	if e.Reason == "" {
		return bad(ErrReason, "a hold")
	}
	switch l.State {
	case LineWaiting, LineWaitingBase:
		m.setLine(l, LineHeld, CauseHold, e.Reason)
	case LineParked:
		park := l.Park
		m.setLine(l, LineHeld, CauseHold, e.Reason)
		l.Park = park
	case LineRiding:
		t := m.s.Trains[l.Train]
		if t.State == TrainLanding {
			return bad(ErrLanding, "train %s", t.ID)
		}
		m.end(t, TrainVoid, CauseVoid, fmt.Sprintf("line %s was held: %s", l.ID, e.Reason), func(x *Line) bool {
			if x.ID != l.ID {
				return false
			}
			m.setLine(x, LineHeld, CauseHold, e.Reason)
			return true
		})
	case LineHeld:
		return bad(ErrHeld, "line %s is held already", l.ID)
	default:
		return bad(ErrState, "line %s is %s", l.ID, l.State)
	}
	return nil
}

func (m *machine) park(e ParkLines) error {
	if e.Park == "" || e.Cond == "" {
		return bad(ErrReason, "a park needs a name and a restore condition")
	}
	if m.s.Parks[e.Park] != nil {
		return bad(ErrDuplicate, "park %s", e.Park)
	}
	if !distinct(e.Lines) {
		return bad(ErrInput, "a line is named twice")
	}
	p := &Park{ID: e.Park, All: len(e.Lines) == 0, Cond: e.Cond, Reason: e.Reason}
	ids := e.Lines
	if p.All {
		if m.allPark() != nil {
			return bad(ErrDuplicate, "the queue is parked already")
		}
		for _, id := range m.s.Waiting() {
			if m.s.Lines[id].Fix != p.ID {
				ids = append(ids, id)
			}
		}
	}
	for _, id := range ids {
		l, err := m.line(id)
		if err != nil {
			return err
		}
		if l.State == LineHeld {
			return bad(ErrHeld, "line %s", id)
		}
		if l.State != LineWaiting {
			return bad(ErrState, "line %s is %s", id, l.State)
		}
	}
	m.s.Parks[p.ID] = p
	for _, id := range ids {
		l := m.s.Lines[id]
		m.setLine(l, LineParked, CausePark, p.Reason+"; restored when: "+p.Cond)
		l.Park = p.ID
	}
	return nil
}

func (m *machine) withdraw(e Withdraw) error {
	l, err := m.line(e.Line)
	if err != nil {
		return err
	}
	const why = "withdrawn by its owner"
	switch l.State {
	case LineOffered, LineWaiting, LineWaitingBase, LineParked:
		m.setLine(l, LineDropped, CauseWithdraw, why)
	case LineRiding:
		t := m.s.Trains[l.Train]
		if t.State == TrainLanding {
			return bad(ErrLanding, "train %s", t.ID)
		}
		m.end(t, TrainVoid, CauseVoid, fmt.Sprintf("line %s was withdrawn", l.ID), func(x *Line) bool {
			if x.ID != l.ID {
				return false
			}
			m.setLine(x, LineDropped, CauseWithdraw, why)
			return true
		})
	case LineHeld:
		return bad(ErrHeld, "line %s: release it first", l.ID)
	default:
		return bad(ErrState, "line %s is %s", l.ID, l.State)
	}
	return nil
}

// lineChecked records a line check's verdict. Only a verdict on the heads the
// line has now counts. A failure charges a waiting line. A line that is held
// or parked only loses its green; one that rides keeps it (see below).
func (m *machine) lineChecked(e LineChecked) error {
	l, err := m.line(e.Line)
	if err != nil {
		return err
	}
	if l.State.Spent() {
		return bad(ErrState, "line %s is %s", l.ID, l.State)
	}
	if len(e.Heads) != len(l.Changes) {
		return nil
	}
	for i, c := range l.Changes {
		if e.Heads[i] != c.Head {
			return nil
		}
	}
	if e.Pass {
		l.Checked, l.CheckedOn = append([]Head(nil), e.Heads...), e.Trunk
		l.Conflict = ""
		return nil
	}
	if e.Conflict != "" {
		// Not a failure: nobody is charged. The line joins no train, and
		// the runner is asked to rebase it. One that rides was assembled,
		// so it merged: its train judges it.
		if l.State != LineRiding && l.Conflict != e.Conflict {
			l.Checked, l.CheckedOn = nil, ""
			l.Conflict = e.Conflict
			m.emit(Effect{Kind: EffRebase, Line: l.ID, Reason: e.Conflict})
		}
		return nil
	}
	if e.Evidence == "" {
		return bad(ErrEvidence, "line %s", l.ID)
	}
	if l.State == LineRiding {
		// It was assembled on a green check of these heads (I10), and a
		// later verdict on another trunk does not undo that: its train's
		// own pre-flight and CI judge it now.
		return nil
	}
	l.Checked, l.CheckedOn = nil, ""
	if l.State == LineWaiting {
		m.setLine(l, LineReturned, CauseCharged, "line check: "+e.Evidence)
	}
	return nil
}

func (m *machine) assemble(e Assemble) error {
	if e.Train == "" || len(e.Lines) == 0 || !distinct(e.Lines) {
		return bad(ErrInput, "a train needs a name and distinct lines")
	}
	if m.s.Trains[e.Train] != nil {
		return bad(ErrDuplicate, "train %s", e.Train)
	}
	if m.s.Paused {
		return bad(ErrPaused, "%s", m.s.PauseWhy)
	}
	if len(m.s.Chain) >= m.s.Config.Depth {
		return bad(ErrChain, "%d trains ride already", len(m.s.Chain))
	}
	base := m.s.Trunk
	if n := len(m.s.Chain); n > 0 {
		a := m.s.Trains[m.s.Chain[n-1]]
		switch a.State {
		case TrainBuilt, TrainTesting, TrainGreen, TrainLanding:
			base = a.Head
		default:
			return bad(ErrChain, "train %s ahead is %s and has no head to build on", a.ID, a.State)
		}
	}
	group := -1
	taken := map[LineID]bool{}
	for _, id := range e.Lines {
		taken[id] = true
	}
	for _, id := range e.Lines {
		l, err := m.line(id)
		if err != nil {
			return err
		}
		switch {
		case l.State == LineHeld:
			return bad(ErrHeld, "line %s", id)
		case l.State != LineWaiting:
			return bad(ErrState, "line %s is %s", id, l.State)
		case l.Alone && len(e.Lines) > 1:
			return bad(ErrJoin, "line %s rides alone", id)
		case group >= 0 && l.Group != group:
			return bad(ErrJoin, "line %s is in another half of a split", id)
		case l.Stale != "":
			return bad(ErrJoin, "line %s is stale: %s", id, l.Stale)
		case l.Conflict != "":
			return bad(ErrJoin, "line %s conflicts with trunk: %s", id, l.Conflict)
		case l.AtChargedHeads():
			return bad(ErrUnfixed, "line %s is at the heads it was charged on", id)
		case m.s.Config.RequireCheck && !l.CheckCurrent():
			return bad(ErrJoin, "line %s has no current green line check (§5.4)", id)
		case l.Base != "" && !m.s.NeedMet(l, taken):
			return bad(ErrJoin, "line %s needs line %s, which has not landed, rides in no earlier train and is not earlier in this train (I3)", id, l.Base)
		}
		group = l.Group
	}
	lines := append([]LineID(nil), e.Lines...)
	sort.Slice(lines, func(i, j int) bool { return m.s.Lines[lines[i]].Seq < m.s.Lines[lines[j]].Seq })
	t := &Train{ID: e.Train, Lines: lines, BaseHead: base}
	m.s.Trains[t.ID] = t
	m.s.Chain = append(m.s.Chain, t.ID)
	m.setTrain(t, TrainAssembling, CauseAssemble, "")
	for _, id := range lines {
		l := m.s.Lines[id]
		m.setLine(l, LineRiding, CauseJoin, "")
		l.Train = t.ID
	}
	m.emit(Effect{Kind: EffBuild, Train: t.ID, Expect: base})
	return nil
}

func (t *Train) has(id LineID) bool {
	for _, x := range t.Lines {
		if x == id {
			return true
		}
	}
	return false
}

func (t *Train) drop(id LineID) {
	out := t.Lines[:0]
	for _, x := range t.Lines {
		if x != id {
			out = append(out, x)
		}
	}
	t.Lines = out
}

func (m *machine) leaveOut(e LeaveOut) error {
	t, err := m.train(e.Train)
	if err != nil {
		return err
	}
	if t.State != TrainAssembling {
		return bad(ErrState, "train %s is %s", t.ID, t.State)
	}
	if !t.has(e.Line) {
		return bad(ErrUnknown, "line %s is not in train %s", e.Line, t.ID)
	}
	if e.Reason == "" {
		return bad(ErrReason, "line %s left out of train %s", e.Line, t.ID)
	}
	t.drop(e.Line)
	m.setLine(m.s.Lines[e.Line], LineReturned, CauseLeftOut, e.Reason)
	// A line of the train that needs a line left out is left out too (I3):
	// the train would hold it without its base.
	for again := true; again; {
		again = false
		for _, id := range t.Lines {
			d := m.s.Lines[id]
			if b := m.s.Lines[d.Base]; d.Base != "" && b != nil && (b.State == LineReturned || b.State == LineDropped) {
				t.drop(id)
				m.setLine(d, LineReturned, CauseLeftOut, fmt.Sprintf("its base %s was left out of train %s", b.ID, t.ID))
				again = true
				break
			}
		}
	}
	if len(t.Lines) == 0 {
		m.end(t, TrainEmpty, CauseEmpty, "every line was left out", nil)
	}
	return nil
}

// fail handles a deterministic failure (§5.6). Blame that names some lines
// and not all charges them. Blame that names none, or all of several, is not
// a decision blame can make: the train goes to J1.
// waits says, for an error, that a train's failure is recorded and unjudged.
func waits(t *Train) string {
	if t.Behind != nil {
		return ", and its failure waits for the train ahead"
	}
	return ""
}

// failed takes a failure report. Only the first train of the chain is
// judged: its base is trunk, so its failure is its own. A train behind it was
// built on a head that may never land, and its failure may be that head's:
// charging, splitting or parking on it would judge the wrong train. Its
// report is kept (Train.Behind) and judged when every train ahead has landed
// (judgeDeferred). If a train ahead does not land, this one is void with it
// (end), and the report is dropped with nobody charged.
func (m *machine) failed(t *Train, blamed []LineID, evidence string) error {
	if len(m.s.Chain) > 0 && m.s.Chain[0] == t.ID {
		return m.fail(t, blamed, evidence)
	}
	if err := m.checkBlame(t, blamed, evidence); err != nil {
		return err
	}
	t.Behind = &Deferred{Blamed: append([]LineID(nil), blamed...), Evidence: evidence}
	return nil
}

// judgeDeferred judges the failure the new first train reported while it was
// behind, now that everything ahead of it has landed. A line blamed then and
// no longer in the train is no longer named.
func (m *machine) judgeDeferred() error {
	if len(m.s.Chain) == 0 {
		return nil
	}
	t := m.s.Trains[m.s.Chain[0]]
	d := t.Behind
	if d == nil {
		return nil
	}
	t.Behind = nil
	var blamed []LineID
	for _, id := range d.Blamed {
		if t.has(id) {
			blamed = append(blamed, id)
		}
	}
	return m.fail(t, blamed, d.Evidence)
}

func (m *machine) checkBlame(t *Train, blamed []LineID, evidence string) error {
	if !distinct(blamed) {
		return bad(ErrInput, "a line is blamed twice")
	}
	for _, id := range blamed {
		if !t.has(id) {
			return bad(ErrUnknown, "line %s is not in train %s", id, t.ID)
		}
	}
	if len(blamed) > 0 && evidence == "" {
		return bad(ErrEvidence, "train %s", t.ID)
	}
	return nil
}

func (m *machine) fail(t *Train, blamed []LineID, evidence string) error {
	if err := m.checkBlame(t, blamed, evidence); err != nil {
		return err
	}
	return m.judgeFailure(t, blamed, evidence)
}

func (m *machine) judgeFailure(t *Train, blamed []LineID, evidence string) error {
	if len(blamed) == 0 || (len(blamed) == len(t.Lines) && len(t.Lines) > 1) {
		m.setTrain(t, TrainJudging, CauseUnblamed, evidence)
		m.emit(Effect{Kind: EffAskJudge, Point: "J1", Train: t.ID, Reason: "a failure blame cannot attribute"})
		return nil
	}
	m.charge(t, CauseCharged, blamed, evidence)
	return nil
}

func (m *machine) charge(t *Train, c Cause, blamed []LineID, evidence string) {
	named := map[LineID]bool{}
	for _, id := range blamed {
		named[id] = true
	}
	m.end(t, TrainFailed, c, evidence, func(l *Line) bool {
		if !named[l.ID] {
			return false
		}
		m.setLine(l, LineReturned, CauseCharged, evidence)
		return true
	})
}

func (m *machine) noVerdict(e CINoVerdict) error {
	t, err := m.train(e.Train)
	if err != nil {
		return err
	}
	switch t.State {
	case TrainAssembling, TrainBuilt, TrainTesting:
	default:
		return bad(ErrState, "train %s is %s", t.ID, t.State)
	}
	if t.Behind != nil {
		return bad(ErrState, "train %s is %s%s", t.ID, t.State, waits(t))
	}
	if e.Why == "" {
		return bad(ErrReason, "a train with no verdict")
	}
	over := false
	for _, id := range t.Lines {
		l := m.s.Lines[id]
		l.NoVerdicts++
		over = over || l.NoVerdicts >= m.s.Config.NoVerdictBudget
	}
	m.end(t, TrainVoid, CauseVoid, "no verdict: "+e.Why, nil)
	if over {
		m.pause("J3", "repeated trains with no verdict: "+e.Why, t.ID)
	}
	return nil
}

func (m *machine) judge(e Judge) error {
	t, err := m.train(e.Train)
	if err != nil {
		return err
	}
	if t.State != TrainJudging {
		return bad(ErrState, "train %s is %s", t.ID, t.State)
	}
	switch e.Action {
	case JudgeCharge:
		if len(e.Lines) == 0 || !distinct(e.Lines) {
			return bad(ErrInput, "a charge names lines")
		}
		for _, id := range e.Lines {
			if !t.has(id) {
				return bad(ErrUnknown, "line %s is not in train %s", id, t.ID)
			}
		}
		if e.Evidence == "" {
			return bad(ErrEvidence, "train %s", t.ID)
		}
		m.charge(t, CauseJudged, e.Lines, e.Evidence)
	case JudgeSplit:
		if len(t.Lines) < 2 {
			return bad(ErrState, "train %s has one line: it cannot be split, and it is not charged without evidence", t.ID)
		}
		half := (len(t.Lines) + 1) / 2
		for i, id := range t.Lines {
			g := m.s.NextGroup
			if i >= half {
				g++
			}
			m.s.Lines[id].Group = g
		}
		m.s.NextGroup += 2
		m.end(t, TrainFailed, CauseJudged, "split: "+e.Why, nil)
	case JudgePark:
		// The failure is not the train's: trunk or a shared fixture is
		// broken. The whole queue parks (§6.4), and the train's lines with it.
		if m.allPark() == nil {
			if err := m.park(ParkLines{Park: e.Park, Cond: e.Cond, Reason: "parked at J1: " + e.Why}); err != nil {
				return err
			}
		}
		m.end(t, TrainFailed, CauseJudged, "park: "+e.Why, nil)
	case JudgeRerun:
		if t.Head == "" || t.Judged >= 1 {
			return bad(ErrBudget, "train %s cannot be rerun", t.ID)
		}
		t.Judged++
		m.setTrain(t, TrainTesting, CauseRerun, e.Why)
		m.emit(Effect{Kind: EffRerun, Train: t.ID, Reason: e.Why})
	default:
		return bad(ErrInput, "J1 has no action %q", e.Action)
	}
	return nil
}

func (m *machine) headMoved(e HeadMoved) error {
	l, err := m.line(e.Line)
	if err != nil {
		return err
	}
	if l.State.Spent() {
		return bad(ErrState, "line %s is %s", l.ID, l.State)
	}
	i := -1
	for k, c := range l.Changes {
		if c.ID == e.Change {
			i = k
		}
	}
	if i < 0 || e.Head == "" {
		return bad(ErrUnknown, "change %q of line %s", e.Change, l.ID)
	}
	if l.State == LineRiding {
		t := m.s.Trains[l.Train]
		if t.State == TrainLanding {
			l.Late = e.Head
			return nil
		}
		l.Changes[i].Head, l.NoVerdicts = e.Head, 0
		l.Checked, l.CheckedOn, l.Conflict = nil, "", ""
		m.end(t, TrainVoid, CauseVoid, fmt.Sprintf("change %s moved while it rode (I4)", e.Change), nil)
		return nil
	}
	l.Changes[i].Head, l.NoVerdicts = e.Head, 0
	l.Checked, l.CheckedOn, l.Conflict = nil, "", ""
	return nil
}

func (m *machine) abort(e Abort) error {
	t, err := m.train(e.Train)
	if err != nil {
		return err
	}
	if t.State == TrainAborted {
		return nil // aborting twice is the same as once
	}
	if t.State == TrainLanding {
		return bad(ErrLanding, "train %s", t.ID)
	}
	if !t.State.Live() {
		return bad(ErrState, "train %s is %s", t.ID, t.State)
	}
	if e.Reason == "" {
		return bad(ErrReason, "an abort")
	}
	for id, d := range e.Lines {
		if !t.has(id) {
			return bad(ErrUnknown, "line %s is not in train %s", id, t.ID)
		}
		if d.Kind == ParkIt {
			if d.Park == "" {
				return bad(ErrReason, "a park needs a name")
			}
			if p := m.s.Parks[d.Park]; p == nil && d.Cond == "" {
				return bad(ErrReason, "a park needs a restore condition")
			}
		}
	}
	m.end(t, TrainAborted, CauseAbort, e.Reason, func(l *Line) bool {
		d := e.Lines[l.ID]
		switch d.Kind {
		case Return:
			m.setLine(l, LineReturned, CauseAbort, e.Reason)
			return true
		case ParkIt:
			p := m.s.Parks[d.Park]
			if p == nil {
				p = &Park{ID: d.Park, Cond: d.Cond, Reason: e.Reason}
				m.s.Parks[p.ID] = p
			}
			m.setLine(l, LineParked, CauseAbort, p.Reason+"; restored when: "+p.Cond)
			l.Park = p.ID
			return true
		}
		m.place(l, CauseAbort) // requeue: through the gate, as the abort's act
		return true
	})
	return nil
}

// verified is I9: the lander's own reading decides, not the builder's report.
func (m *machine) verified(e LandingVerified) error {
	t, err := m.train(e.Train)
	if err != nil {
		return err
	}
	if t.State != TrainGreen {
		return bad(ErrState, "train %s is %s", t.ID, t.State)
	}
	if len(m.s.Chain) == 0 || m.s.Chain[0] != t.ID {
		return bad(ErrChain, "train %s is not first; trains land in chain order", t.ID)
	}
	if e.Trunk != m.s.Trunk {
		m.end(t, TrainVoid, CauseVoid, fmt.Sprintf("trunk is at %s, not %s: another writer (I7)", e.Trunk, m.s.Trunk), nil)
		m.pause("J3", "trunk was moved by another writer", t.ID)
		return nil
	}
	if e.Verdict != t.Head {
		m.end(t, TrainVoid, CauseVoid, fmt.Sprintf("no verdict: the forge shows a pass on %q, not on %s", e.Verdict, t.Head), nil)
		return nil
	}
	for _, id := range t.Lines {
		l := m.s.Lines[id]
		for i, c := range l.Changes {
			if got, ok := e.Heads[c.ID]; !ok || got != c.Head {
				if ok {
					l.Changes[i].Head, l.NoVerdicts = got, 0
				}
				m.end(t, TrainVoid, CauseVoid, fmt.Sprintf("change %s moved while it rode (I4)", c.ID), nil)
				return nil
			}
		}
	}
	t.Verified = true
	m.setTrain(t, TrainLanding, CauseVerified, "")
	m.emit(Effect{Kind: EffPush, Train: t.ID, Head: t.Head, Expect: t.BaseHead})
	return nil
}
