package state

import (
	"errors"
	"time"
)

// Event is something that happened: an act of a producer, the runner or the
// judge, or a report from the forge or CI. Events are data.
type Event interface{ event() string }

// Offer puts a line forward for landing. A spent line that did not land may
// be offered again; that is a new offer with a new place in the queue.
// By is who offers. The first offer's By is the line's owner; a line that
// was charged is offered again only by its owner, and only at new heads.
type Offer struct {
	By      string
	Line    LineID
	Changes []Change
	Base    LineID
	Alone   bool
	Fix     ParkID
}

// GateAccept is the gate's yes (§5.2). Where the line goes next is the
// machine's decision: waiting, waiting for its base, or parked.
//
// Inert says the gate's classifier found every file of every change of the
// line to be inert content, which no build, test or generator reads (§5.4).
// It is only ever set on what the forge's whole file list shows.
type GateAccept struct {
	Line  LineID
	Inert bool
}

// GateRefuse is the gate's no, with its reason.
type GateRefuse struct {
	Line   LineID
	Reason string
}

// Hold keeps one line out of trains. A hold on a riding line voids its train.
type Hold struct {
	Line   LineID
	Reason string
}

// Release lifts a hold. The line passes the gate again.
type Release struct{ Line LineID }

// ParkLines moves lines aside under a new park. With no Lines it parks the
// whole queue: every waiting line, and every arrival, until the restore.
type ParkLines struct {
	Park   ParkID
	Lines  []LineID
	Cond   string
	Reason string
}

// Restore ends a park. Its lines pass the gate again.
type Restore struct{ Park ParkID }

// Withdraw is the owner taking a line back.
type Withdraw struct{ Line LineID }

// Assemble starts a train from waiting lines. Which lines to take is the
// runner's choice (§5.3); the machine checks that the choice is legal.
type Assemble struct {
	Train TrainID
	Lines []LineID
}

// LeaveOut removes a line from an assembling train, with a reason.
//
// Moved, when set, says why: the builder read new heads for changes of the
// line (pushed to since it was offered, or while it was held). The line is
// re-keyed at them, once, as a head-moved event would re-key it: its check
// ends (I10), it keeps its place, and the cycle is its own, so it counts
// none (row L39).
type LeaveOut struct {
	Train  TrainID
	Line   LineID
	Reason string
	Moved  []Change
}

// PreflightFailed reports a pre-flight failure and the lines blame names.
type PreflightFailed struct {
	Train    TrainID
	Blamed   []LineID
	Evidence string
}

// Built reports that the train is assembled, passed pre-flight, and has the
// given head.
type Built struct {
	Train TrainID
	Head  Head
}

// Opened reports that the train's pull request is open on the given head.
type Opened struct {
	Train TrainID
	PR    string
	Head  Head
}

// CIPassed is CI's report of a pass on the given head.
type CIPassed struct {
	Train TrainID
	Head  Head
}

// CIRerun reports a rerun after a known flake or a timing failure (§5.5).
type CIRerun struct {
	Train TrainID
	Why   string
	// Known says the failure is a recorded flake (class 2), not a timing
	// failure (class 3). A recorded flake never charges a line: when it
	// fails again after its one rerun, nothing is judged (§5.5).
	Known bool
}

// CINoVerdict reports that nothing was judged (§5.5 classes 0 and 1): the
// run was cancelled, skipped, superseded or unreadable, the builder died, or
// the infrastructure failed.
type CINoVerdict struct {
	Train TrainID
	Infra bool
	Why   string
}

// CIFailed reports a deterministic failure and the lines blame names.
type CIFailed struct {
	Train    TrainID
	Blamed   []LineID
	Evidence string
	// TreeWide says the failing test scans the whole tree: it holds every
	// member of a repository-wide set to a rule, so a change breaks it
	// without touching it. Two unrelated trains failing on it are two
	// changes each breaking the set, never a sign that trunk is broken.
	TreeWide bool
}

// JudgeAction is a choice from J1's menu.
type JudgeAction string

const (
	JudgeCharge JudgeAction = "charge"
	JudgeSplit  JudgeAction = "split"
	JudgePark   JudgeAction = "park"
	JudgeRerun  JudgeAction = "rerun"
)

// Judge is the judge's decision at J1 on a train in judging.
type Judge struct {
	Train    TrainID
	Action   JudgeAction
	Lines    []LineID // charge: the lines charged
	Evidence string   // charge: required
	Park     ParkID   // park: the new park
	Cond     string   // park: its restore condition
	Why      string
}

// JudgeTimeout is J1's deadline passing with the judge silent. The default
// applies: park if the same test failed in the previous train with unrelated
// changes, or if the train cannot be split; otherwise split.
type JudgeTimeout struct {
	Train          TrainID
	SameTestBefore bool
}

// HeadMoved reports a push to a change of a line.
type HeadMoved struct {
	Line   LineID
	Change ChangeID
	Head   Head
}

// DisposeKind says what an abort does with a line.
type DisposeKind uint8

const (
	Requeue DisposeKind = iota // back through the gate; the default
	ParkIt                     // parked, with a restore condition
	Return                     // back to its owner, with the abort's reason
)

// Dispose is what becomes of one line of an aborted train.
type Dispose struct {
	Kind DisposeKind
	Park ParkID
	Cond string
}

// Abort cancels a train at any stage before its push (§5.9).
type Abort struct {
	Train  TrainID
	Reason string
	Lines  map[LineID]Dispose
}

// RequeueByHand is the operator's override (§5.9): the lines of a train, or
// one line, go back to waiting, whatever happened to them. A live train is
// aborted first, its lines requeued. A line its train returned, charged or
// not, waits again. Each line's budgets and cycles are reset: its
// no-verdict strikes, its count, its split half, and the heads it was
// charged at. Nothing is charged and nothing is parked; a park that already
// covers the queue still covers the line. A line that landed or was
// dropped, rides another train, or is held or parked keeps its state, and
// only its budgets reset. Train or Line names the target, not both.
type RequeueByHand struct {
	Train  TrainID
	Line   LineID
	Reason string
}

// LandingVerified is the lander's own reading, from the forge, immediately
// before it pushes (I9): the head CI passed, each change's head, and trunk.
type LandingVerified struct {
	Train   TrainID
	Verdict Head
	Heads   map[ChangeID]Head
	Trunk   Head
}

// PushOK reports that trunk was fast-forwarded to the train's head.
type PushOK struct{ Train TrainID }

// PushRefused reports that the fast-forward was refused.
type PushRefused struct {
	Train     TrainID
	Permanent bool
	Why       string
}

// Pause stops new trains (J3). Trains in flight finish.
type Pause struct{ Reason string }

// Resume lifts a pause. If another writer moved trunk, Trunk is where the
// judge found it, and the machine adopts it as a foreign landing.
type Resume struct{ Trunk Head }

// Stale marks a waiting line as stale (§5.2): its forge's declared base and
// its ancestry disagree, so it waits for a rebase and joins no train. Its
// owner is told the reason.
type Stale struct {
	Line   LineID
	Reason string
}

// Fresh clears a line's staleness: its head and its base agree again.
type Fresh struct{ Line LineID }

// HandOver gives a line to another owner (§7): its owner asked, or went
// silent past the deadline and the judge decided. Recorded, with a reason.
type HandOver struct {
	Line LineID
	To   string
	Why  string
}

// RebaseFailed reports that the runner could not rebase a line in conflict
// by itself (§5.2): the conflict is real, or there is no rebase hook. The
// line stays in conflict and its owner is told.
type RebaseFailed struct {
	Line   LineID
	Reason string
}

// LineChecked reports the line check of one line (§5.4): the line
// alone, merged with trunk, on exactly Heads. A verdict on other heads than
// the line has now says nothing and changes nothing. A pass is kept on the
// line. A failure charges a waiting line, and needs its evidence (I6): the
// failing check passes on Trunk alone.
type LineChecked struct {
	Line     LineID
	Heads    []Head // the heads of the line's changes that were checked, in order
	Trunk    Head   // the trunk the line was merged with
	Pass     bool
	Evidence string // a failure's evidence; shown to the owner
	// Conflict, when set, says the line did not merge onto Trunk, and in
	// what. It is neither a pass nor a failure: nobody is charged, the line
	// joins no train, and the runner is asked to rebase it (§5.2).
	Conflict string
	// Version is the line check's version (docs/queue-as-bag.md §5). A
	// verdict of another version than the present one is dropped.
	Version string
}

// CheckerChanged reports a checker's present version (docs/queue-as-bag.md
// §5). Every verdict of another version stops counting: a waiting line's
// green, a claim, and a charge, which is void. A train still assembling
// when the pre-flight changes is void, to run the new one.
type CheckerChanged struct {
	Checker string
	Version string
}

// CheckClaimed is a checker's claim on the line check of one waiting line
// (§5.4): it checks the line at Key, and holds the claim until Until. While
// the claim stands no other checker is given the line, a restarted checker
// included, and a verdict on another key is refused. The claim is in the
// store, so it outlives the checker that made it.
type CheckClaimed struct {
	Line  LineID
	Key   CheckKey
	By    string
	Until time.Time
}

// ClaimExpired ends a claim whose time has passed: its checker died or
// hung. The line may be claimed again. A verdict that arrives later on the
// same key is still a verdict on those heads.
type ClaimExpired struct{ Line LineID }

// ClaimReleased is a checker's giving up its own claim with no verdict
// (§5.4): its infrastructure failed, so it judged nothing. It ends the claim
// as ClaimExpired does, at once and with a reason, and records nothing of
// the line, which is due a check again. Only the claim's own key releases it.
type ClaimReleased struct {
	Line   LineID
	Key    CheckKey
	By     string
	Reason string
}

// Prioritise gives a line a manual priority, the one act that lets a line
// pass an older one (docs/queue-as-bag.md §2). It needs a reason and an
// expiry, and covers the lines the line is stacked on, which must ride
// first. Giving it again replaces it.
type Prioritise struct {
	Line   LineID
	By     string
	Reason string
	Until  time.Time
}

// PriorityEnded ends a line's manual priority: it expired, or was taken
// back.
type PriorityEnded struct {
	Line LineID
	Why  string
}

// DraftOpened records the draft pull request a train is shown by from
// assembly (§5.4): its lines are merged, and its cheap tier runs. No CI
// runs on the draft; Opened promotes it once pre-flight passes.
type DraftOpened struct {
	Train TrainID
	PR    string
}

// ExpireBudget resets a line's retry budgets after their stated time (§5.10).
type ExpireBudget struct{ Line LineID }

// Starving says the runner found a waiting line past a starvation threshold
// (docs/queue-as-bag.md §4): its count, or its queue time. Why says which.
type Starving struct {
	Line LineID
	Why  string
}

func (Offer) event() string           { return "offer" }
func (GateAccept) event() string      { return "gate-accept" }
func (GateRefuse) event() string      { return "gate-refuse" }
func (Hold) event() string            { return "hold" }
func (Release) event() string         { return "release" }
func (ParkLines) event() string       { return "park" }
func (Restore) event() string         { return "restore" }
func (Withdraw) event() string        { return "withdraw" }
func (Assemble) event() string        { return "assemble" }
func (LeaveOut) event() string        { return "leave-out" }
func (PreflightFailed) event() string { return "preflight-failed" }
func (Built) event() string           { return "built" }
func (Opened) event() string          { return "opened" }
func (CIPassed) event() string        { return "ci-passed" }
func (CIRerun) event() string         { return "ci-rerun" }
func (CINoVerdict) event() string     { return "ci-no-verdict" }
func (CIFailed) event() string        { return "ci-failed" }
func (Judge) event() string           { return "judge" }
func (JudgeTimeout) event() string    { return "judge-timeout" }
func (HeadMoved) event() string       { return "head-moved" }
func (Abort) event() string           { return "abort" }
func (RequeueByHand) event() string   { return "requeue" }
func (LandingVerified) event() string { return "landing-verified" }
func (PushOK) event() string          { return "push-ok" }
func (PushRefused) event() string     { return "push-refused" }
func (Pause) event() string           { return "pause" }
func (Resume) event() string          { return "resume" }
func (ExpireBudget) event() string    { return "expire-budget" }
func (Stale) event() string           { return "stale" }
func (Fresh) event() string           { return "fresh" }
func (LineChecked) event() string     { return "line-checked" }
func (HandOver) event() string        { return "hand-over" }
func (RebaseFailed) event() string    { return "rebase-failed" }
func (CheckClaimed) event() string    { return "check-claimed" }
func (ClaimExpired) event() string    { return "claim-expired" }
func (ClaimReleased) event() string   { return "claim-released" }
func (Prioritise) event() string      { return "prioritise" }
func (PriorityEnded) event() string   { return "priority-ended" }
func (DraftOpened) event() string     { return "draft-opened" }
func (Starving) event() string        { return "starving" }
func (CheckerChanged) event() string  { return "checker-changed" }

// Name returns the event's name, for logs and reports.
func Name(e Event) string { return e.event() }

// The reasons Next refuses an event. Test with errors.Is.
var (
	ErrInput     = errors.New("malformed event")
	ErrUnknown   = errors.New("unknown line, train or park")
	ErrDuplicate = errors.New("already present")
	ErrState     = errors.New("no such transition from this state")
	ErrHeld      = errors.New("the line is held")
	ErrLanding   = errors.New("the train is landing: past the point of no return")
	ErrEvidence  = errors.New("a charge needs evidence (I6)")
	ErrReason    = errors.New("a reason is required")
	ErrHead      = errors.New("not the head that was built (I1, I9)")
	ErrChain     = errors.New("breaks the speculation chain (I5)")
	ErrPaused    = errors.New("the queue is paused")
	ErrJoin      = errors.New("these lines may not ride together")
	ErrBudget    = errors.New("retry budget spent")
	ErrTable     = errors.New("transition not in the table (I8)")
	ErrOwner     = errors.New("only the line's owner may do this")
	ErrUnfixed   = errors.New("charged, and not changed since")
	ErrClaimed   = errors.New("the line check is claimed")
)

// EffectKind is what the runner must do in the world.
type EffectKind string

const (
	EffRecord    EffectKind = "record"         // append the transition to the log (I8)
	EffGate      EffectKind = "gate"           // read what the gate needs about a line's changes (§5.2)
	EffBuild     EffectKind = "build"          // merge the train's lines on Expect and run pre-flight (§5.4)
	EffOpenPR    EffectKind = "open-pr"        // open the train's pull request and start CI
	EffRerun     EffectKind = "rerun"          // run the train's failed jobs again
	EffCancelRun EffectKind = "cancel-run"     // stop the train's CI
	EffClosePR   EffectKind = "close-pr"       // close the train's pull request
	EffVerify    EffectKind = "verify-landing" // read verdict, heads and trunk from the forge (I9)
	EffPush      EffectKind = "push"           // fast-forward trunk from Expect to Head
	EffNotify    EffectKind = "notify"         // tell a line's owner
	EffAskJudge  EffectKind = "ask-judge"      // open a judgement point
	EffRebase    EffectKind = "rebase"         // rebase a line in conflict onto trunk, if it can be done without a person (§5.2)
)

// Effect is one thing to do, as data. Next performs none of them.
type Effect struct {
	Kind   EffectKind
	Line   LineID
	Train  TrainID
	Head   Head   // push: the head to land; open-pr: the train's head
	Expect Head   // push: where trunk must be; build: the head to build on
	From   string // record
	To     string // record; notify: the state the line went to, if it moved
	Cause  Cause  // record, notify
	Point  string // ask-judge: J1, J3
	Reason string
}
