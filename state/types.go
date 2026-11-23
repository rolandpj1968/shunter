// Package state is the state machine of PROTOCOL.md §5.1 as one pure
// function, Next. It performs no side effects: what must happen in the world
// is returned as data (effects), and an illegal transition is an error.
package state

import (
	"slices"
	"sort"
	"time"
)

type (
	// LineID names a line for its whole life. A line is never identified by
	// its text (the changes it lists), which changes when it is re-offered.
	LineID string
	// TrainID names a train (a batch).
	TrainID string
	// ChangeID names one change (one pull request).
	ChangeID string
	// Head is a commit.
	Head string
	// ParkID names a park, so that its restore can find its lines.
	ParkID string
)

// LineState is the one state a line is in (I8).
type LineState uint8

const (
	LineNone        LineState = iota // not known to the queue
	LineOffered                      // offered; the gate has not answered
	LineWaiting                      // queued; may join a train
	LineWaitingBase                  // retired: a line queued before needs first, behind its base
	LineHeld                         // kept out of trains by a recorded judgement
	LineParked                       // moved aside under a park, until its restore
	LineRiding                       // in a live train; its stage is the train's state
	LineLanded                       // spent: on trunk
	LineReturned                     // spent: back with its owner, with a reason
	LineDropped                      // spent: withdrawn by its owner
)

var lineNames = [...]string{"none", "offered", "waiting", "waiting-for-base", "held", "parked", "riding", "landed", "returned", "dropped"}

func (s LineState) String() string { return lineNames[s] }

// Spent reports whether the line's offer has ended.
func (s LineState) Spent() bool { return s == LineLanded || s == LineReturned || s == LineDropped }

// TrainState is the one state a train is in.
type TrainState uint8

const (
	TrainNone       TrainState = iota
	TrainAssembling            // lines taken; merge and pre-flight in progress
	TrainBuilt                 // pre-flight passed; its head is fixed
	TrainTesting               // its pull request is open and CI runs on its head
	TrainJudging               // failed with no usable blame; waits at J1
	TrainGreen                 // CI reported a pass on its head; waits its turn
	TrainLanding               // verified by the lander; the push is in flight
	TrainLanded                // ended: trunk is at its head
	TrainFailed                // ended: a failure was charged or judged
	TrainVoid                  // ended: nothing was judged; its lines went back
	TrainAborted               // ended: cancelled by the judge
	TrainEmpty                 // ended: every line was left out
)

var trainNames = [...]string{"none", "assembling", "built", "testing", "judging", "green", "landing", "landed", "failed", "void", "aborted", "empty"}

func (s TrainState) String() string { return trainNames[s] }

// Live reports whether the train is still in the chain.
func (s TrainState) Live() bool { return s >= TrainAssembling && s <= TrainLanding }

// Cause is why a transition happened. Every recorded transition carries one,
// and the pair (from, to) with its cause must be a row of Table.
type Cause string

const (
	CauseOffer      Cause = "offer"
	CauseGate       Cause = "gate"
	CauseRefused    Cause = "refused"
	CauseHold       Cause = "hold"
	CauseRelease    Cause = "release"
	CausePark       Cause = "park"
	CauseRestore    Cause = "restore"
	CauseWithdraw   Cause = "withdraw"
	CauseJoin       Cause = "join"
	CauseLeftOut    Cause = "left-out"
	CauseCharged    Cause = "charged"
	CauseRequeue    Cause = "requeue"
	CauseAbort      Cause = "abort"
	CauseLanded     Cause = "landed"
	CauseBaseLanded Cause = "base-landed"
	CauseBaseLost   Cause = "base-lost"

	CauseAssemble Cause = "assemble"
	CauseBuilt    Cause = "built"
	CauseOpened   Cause = "opened"
	CausePassed   Cause = "passed"
	CauseUnblamed Cause = "unblamed"
	CauseJudged   Cause = "judged"
	CauseRerun    Cause = "rerun"
	CauseEmpty    Cause = "empty"
	CauseVoid     Cause = "void"
	CauseVerified Cause = "verified"
	CausePushed   Cause = "pushed"
	CausePushNo   Cause = "push-refused"
)

// Change is one change of a line and the head it was offered at.
type Change struct {
	ID   ChangeID
	Head Head
}

// Line is the single record of a line (I8).
type Line struct {
	ID      LineID
	State   LineState
	Seq     int      // its place in the queue: lower goes first
	Changes []Change // in stack order: a change's base is the one before it
	Base    LineID   // the line this one is stacked on, if any: the line it needs (I3)
	Alone   bool     // must ride in a train of one
	Fix     ParkID   // the park this line is the fix for; it is exempt from it
	Train   TrainID  // the live train it rides, when riding
	Park    ParkID   // the park it is under, when parked (kept while held)
	Group   int      // the half of a split it belongs to; 0 for none
	// Stale says why the line waits for a rebase (§5.2): its head and its
	// declared base disagree. A stale line joins no train. Empty when not.
	Stale  string
	Cause  Cause  // why it is in its state
	Reason string // the stated reason, shown to its owner
	// NoVerdicts is a retry budget (§5.10): the trains this line rode that
	// ended with no verdict. It resets on landing, a new offer or a new head.
	NoVerdicts int
	// Late is a head pushed while the line's train was landing. It did not
	// land, and the owner is told so.
	Late Head
	// Checked is the line check's green verdict (§5.4): the heads of
	// the line's changes it passed on, in order, and CheckedOn the trunk it
	// was merged with. A verdict belongs to exactly those heads: a push to
	// any change ends it.
	Checked   []Head
	CheckedOn Head
	// Inert says the gate classified the line as inert content only (§5.4):
	// it takes the light tier, and joins any train whatever its size. A push
	// ends it: the line is full until it passes the gate again.
	Inert bool
	// Conflict says the line's check found that it does not merge onto
	// trunk, and in what. A line in conflict joins no train. Only a push to
	// one of its changes ends it: the runner's rebase (§5.2), or its owner's.
	Conflict string
	// ConflictOn is the trunk the conflict was found against. The runner is
	// asked to rebase once for a conflict of these heads with this trunk,
	// and again only when a check finds a conflict with another trunk.
	ConflictOn Head
	// Owner is who answers for the line: who offered it first, or whom it
	// was handed over to. Only its owner offers a charged line again.
	Owner string
	// ChargedAt are the heads the line had when it was last charged. It does
	// not ride on those heads again: a charge is cleared by a fix, and a fix
	// is a push.
	ChargedAt []Head
	// Claim is the checker's claim on the line's check in flight, if any:
	// no other checker is given the line while it stands (§5.4).
	Claim *Claim
}

// CheckKey is what a line check judges: the line's heads, in order, merged
// with Trunk. A claim and the verdict it brings must have the same key.
type CheckKey struct {
	Heads []Head
	Trunk Head
}

// Equal reports whether two keys are the same.
func (k CheckKey) Equal(o CheckKey) bool { return k.Trunk == o.Trunk && slices.Equal(k.Heads, o.Heads) }

// Claim is one checker's claim on a line check, until a time.
type Claim struct {
	Key   CheckKey
	By    string
	Until time.Time
}

// OnHeads reports whether the key is on exactly the heads the line has now.
func (l *Line) OnHeads(k CheckKey) bool {
	if len(k.Heads) != len(l.Changes) {
		return false
	}
	for i, c := range l.Changes {
		if k.Heads[i] != c.Head {
			return false
		}
	}
	return true
}

// AtChargedHeads reports whether the line's changes are at exactly the heads
// it was charged on.
func (l *Line) AtChargedHeads() bool {
	if len(l.ChargedAt) == 0 || len(l.ChargedAt) != len(l.Changes) {
		return false
	}
	for i, c := range l.Changes {
		if l.ChargedAt[i] != c.Head {
			return false
		}
	}
	return true
}

// CheckCurrent reports whether the line holds a green line check on exactly
// the heads its changes have now.
func (l *Line) CheckCurrent() bool {
	if len(l.Checked) == 0 || len(l.Checked) != len(l.Changes) {
		return false
	}
	for i, c := range l.Changes {
		if l.Checked[i] != c.Head {
			return false
		}
	}
	return true
}

// Train is the single record of a train.
type Train struct {
	ID       TrainID
	State    TrainState
	Lines    []LineID // the lines still in it, in queue order
	BaseHead Head     // the exact head it is built on (I5)
	Head     Head     // its own head, once built
	Verdict  Head     // the head CI passed, as reported by CI
	Verified bool     // the lander has read the verdict and the heads itself (I9)
	PR       string   // the train's own pull request
	Reruns   int      // reruns after a flake or a timing failure
	Judged   int      // reruns ordered at J1
	Pushes   int      // transient push refusals
	Asked    bool     // the landing check has been asked for
	// TreeWide says the failure the train reported was of a test that scans
	// the whole tree (CIFailed.TreeWide).
	TreeWide bool
	// Light says the train was assembled of inert lines only, so the light
	// tier is enough for it (§5.4). A train with any other line is full.
	Light  bool
	Reason string
	// Behind is a failure reported while a train ahead of this one was still
	// undecided (§5.3). Nothing is judged on it yet: the failure may be the
	// train ahead's, inherited. If every train ahead lands it is this train's
	// own and is judged then; if one does not, this train is void with it.
	Behind *Deferred
}

// Deferred is a failure report kept unjudged.
type Deferred struct {
	Blamed   []LineID
	Evidence string
}

// Park is a recorded judgement that moves lines aside. All parks the whole
// queue: lines waiting, and lines arriving.
type Park struct {
	ID     ParkID
	All    bool
	Cond   string // the restore condition the runner can test, or a deadline
	Reason string
}

// Landing is one advance of trunk.
type Landing struct {
	Head    Head
	Train   TrainID
	Foreign bool // trunk was moved by another writer; adopted at J3
}

// Config bounds the machine.
type Config struct {
	Depth           int // trains riding at once: 1 is no speculation
	NoVerdictBudget int // no-verdict trains a line may ride before J3
	PushRetries     int // transient push refusals before J3
	// RequireCheck turns the line check into a gate (§5.4): no line
	// is assembled into a train without a current green line check.
	RequireCheck bool
}

// State is the whole queue. Treat it as a value: Next never changes its
// argument.
type State struct {
	Config    Config
	Trunk     Head
	Landings  []Landing
	Lines     map[LineID]*Line
	Trains    map[TrainID]*Train
	Chain     []TrainID // the live trains, in landing order
	Parks     map[ParkID]*Park
	Paused    bool
	PauseWhy  string
	NextSeq   int
	NextGroup int
}

// New returns an empty queue on the given trunk.
func New(trunk Head, c Config) State {
	if c.Depth < 1 {
		c.Depth = 2
	}
	if c.NoVerdictBudget < 1 {
		c.NoVerdictBudget = 2
	}
	if c.PushRetries < 1 {
		c.PushRetries = 3
	}
	return State{Config: c, Trunk: trunk, Lines: map[LineID]*Line{}, Trains: map[TrainID]*Train{}, Parks: map[ParkID]*Park{}, NextSeq: 1, NextGroup: 1}
}

func (s State) clone() State {
	c := s
	c.Landings = append([]Landing(nil), s.Landings...)
	c.Chain = append([]TrainID(nil), s.Chain...)
	c.Lines = make(map[LineID]*Line, len(s.Lines))
	for k, v := range s.Lines {
		l := *v
		l.Changes = append([]Change(nil), v.Changes...)
		l.Checked = append([]Head(nil), v.Checked...)
		l.ChargedAt = append([]Head(nil), v.ChargedAt...)
		if v.Claim != nil {
			cl := *v.Claim
			cl.Key.Heads = append([]Head(nil), v.Claim.Key.Heads...)
			l.Claim = &cl
		}
		c.Lines[k] = &l
	}
	c.Trains = make(map[TrainID]*Train, len(s.Trains))
	for k, v := range s.Trains {
		t := *v
		t.Lines = append([]LineID(nil), v.Lines...)
		if v.Behind != nil {
			t.Behind = &Deferred{Blamed: append([]LineID(nil), v.Behind.Blamed...), Evidence: v.Behind.Evidence}
		}
		c.Trains[k] = &t
	}
	c.Parks = make(map[ParkID]*Park, len(s.Parks))
	for k, v := range s.Parks {
		p := *v
		c.Parks[k] = &p
	}
	return c
}

// NeedMet reports whether the line's need is met for a train that takes the
// lines in taken (I3): it has no base, or its base has landed, rides in a
// live train (every one of which is earlier in the chain than a train being
// assembled), or is taken into this train ahead of it.
func (s State) NeedMet(l *Line, taken map[LineID]bool) bool {
	if l.Base == "" {
		return true
	}
	b := s.Lines[l.Base]
	switch {
	case b == nil:
		return false
	case b.State == LineLanded, b.State == LineRiding:
		return true
	case taken[b.ID] && b.State == LineWaiting:
		return b.Seq < l.Seq && s.NeedMet(b, taken)
	}
	return false
}

// Waiting returns the lines that may join a train, in queue order.
func (s State) Waiting() []LineID { return s.inState(LineWaiting) }

func (s State) inState(st LineState) []LineID {
	var out []LineID
	for id, l := range s.Lines {
		if l.State == st {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return s.Lines[out[i]].Seq < s.Lines[out[j]].Seq })
	return out
}

func (s State) lineIDs() []LineID {
	out := make([]LineID, 0, len(s.Lines))
	for id := range s.Lines {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return s.Lines[out[i]].Seq < s.Lines[out[j]].Seq })
	return out
}
