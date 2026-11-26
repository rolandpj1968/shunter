// Package adapter is the boundary between the daemon and the world: the
// forge and its pull requests, CI, git, and notices to owners and the judge.
//
// Every call takes a context with a deadline (§2.2: every external call is
// fallible and bounded). An error is transient unless it is wrapped with
// Permanent: a refusal that follows from what the request means (not a
// fast-forward, permission denied, a bad reference).
//
// Every call that changes the world takes the key of the effect that asked
// for it (store.Keyed). An adapter must perform a keyed act at most once in
// the world however often it is called with the same key: the daemon calls
// again after a restart whenever it cannot know whether the first call took.
package adapter

import (
	"context"
	"errors"
	"time"

	"github.com/rolandpj1968/shunter/state"
)

type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// Permanent marks an error as a refusal that retrying will not change.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err}
}

// IsPermanent reports whether err was marked permanent.
func IsPermanent(err error) bool {
	var p permanent
	return errors.As(err, &p)
}

// ChangeInfo is what the forge says about one change.
type ChangeInfo struct {
	ID   state.ChangeID
	Head state.Head
	// Base is the change this one is stacked on, or empty when it is on trunk.
	Base state.ChangeID
	// Open is false for a closed or merged change.
	Open bool
	// Approved is false when the change is from a producer the gate does not
	// accept (§5.2).
	Approved bool
	// Landed is true when the change is already on trunk.
	Landed bool
}

// Reading is the lander's own reading from the forge (I9).
type Reading struct {
	// Verdict is the head on which the forge shows a passing CI run for the
	// train, or empty when it shows none.
	Verdict state.Head
	// Heads is each change's head now.
	Heads map[state.ChangeID]state.Head
	// Trunk is where trunk is now.
	Trunk state.Head
}

// Forge is the code host: changes, the trains' pull requests, and the
// readings the lander makes.
type Forge interface {
	// Describe reads the changes a line is made of.
	Describe(ctx context.Context, ids []state.ChangeID) ([]ChangeInfo, error)
	// OpenPR opens the train's pull request on head, against base, and so
	// starts its CI. Changes are the changes the train carries, in ride
	// order, for the pull request to name. The same key returns the same
	// pull request.
	OpenPR(ctx context.Context, key string, train state.TrainID, changes []state.ChangeID, head, base state.Head) (pr string, err error)
	// ClosePR closes the train's pull request, with the reason. Closing a
	// closed one, or one never opened, is no error.
	ClosePR(ctx context.Context, key string, train state.TrainID, reason string) error
	// ReadLanding reads the verdict on head, the changes' heads and trunk.
	ReadLanding(ctx context.Context, train state.TrainID, head state.Head, changes []state.ChangeID) (Reading, error)
}

// Outcome is CI's report on a train, classified as §5.5 says.
type Outcome uint8

const (
	Pending   Outcome = iota // still running
	Passed                   // passed on Report.Head
	Failed                   // a deterministic failure (§5.5 class 4)
	NoVerdict                // cancelled, skipped, superseded, unreadable (class 0)
	Infra                    // no check ran, or the forge refused transiently (class 1)
	Flaky                    // a known flake, or only timeouts (classes 2 and 3)
)

// Report is one reading of a train's CI. A report is not a verdict (I9): the
// lander reads the verdict for itself before it lands.
type Report struct {
	Outcome  Outcome
	Head     state.Head     // the head the run was on
	Blamed   []state.LineID // Failed: the lines blame names (§5.6), maybe none
	Evidence string         // Failed: the evidence, naming the failing test
	Why      string         // NoVerdict, Infra, Flaky: what happened
	// TreeWide says a Failed outcome is of a test that scans the whole tree
	// (§5.6): no train's failure on it says anything about another's.
	TreeWide bool
	// KnownFlake says a Flaky outcome is a recorded flake (class 2), not a
	// timing failure (class 3): it never charges a line (§5.5).
	KnownFlake bool
	// Early says a Failed outcome was read while other jobs still ran: a
	// decisive failure, reported at once, and the run is cancelled
	// (docs/queue-as-bag.md §6).
	Early bool
	// Checks are the checks read, each the latest run of its name: the
	// evidence behind the outcome, for the log and for shadow running.
	Checks []Check
}

// Check is one check run on a head, as CI reports it.
type Check struct {
	Name       string
	Status     string // queued, in_progress, completed
	Conclusion string // success, failure, cancelled, skipped, timed_out, ...
	Started    time.Time
	URL        string
	Summary    string
	// Suite is the check suite the run belongs to (one workflow run's
	// checks), when the forge says; 0 when it does not.
	Suite int64 `json:",omitempty"`
}

// CI runs a train's checks.
type CI interface {
	// Status reads the latest run on the train's head. The lines are the
	// train's, for blame: a failure can name only lines of the train.
	Status(ctx context.Context, train state.TrainID, head state.Head, lines []BuildLine) (Report, error)
	// Rerun runs the train's failed jobs again.
	Rerun(ctx context.Context, key string, train state.TrainID, head state.Head) error
	// Cancel stops every run of the train. Cancelling nothing is no error.
	Cancel(ctx context.Context, key string, train state.TrainID) error
}

// Reblamer asks blame again with the failure's class forced to deterministic
// (§5.5 class 4). A known flake's or a timing failure's one rerun that fails
// the same way is evidence in its own right: it is blamed by files, as any
// deterministic failure is, rather than sent to J1 as one nobody can name.
// Optional: a CI without it leaves such a failure unblamed, and J1 decides.
//
// rep is the failure as Status read it, with its Outcome already Failed; the
// answer is the same failure with the lines blame names. It can't change the
// outcome: a forced blame that answers anything but deterministic names
// nobody.
type Reblamer interface {
	Reblame(ctx context.Context, train state.TrainID, rep Report, lines []BuildLine) (Report, error)
}

// BuildLine is one line of a train to build, in queue order.
type BuildLine struct {
	ID      state.LineID
	Changes []state.Change
	// Upstream, for a stacked line, is its base's head as recorded: the
	// line's own commits are those after it. A builder that replays
	// commits replays only those; empty for a line on trunk.
	Upstream state.Head `json:",omitempty"`
}

// BuildRequest asks for a train to be merged on Base and checked (§5.4).
type BuildRequest struct {
	Train state.TrainID
	Base  state.Head
	Lines []BuildLine
	// Merged, when set, is called once the lines are merged and the
	// train's branch is at head, before pre-flight runs: from then on the
	// train can be shown (Drafter). A builder may call it again for a
	// round that left lines out.
	Merged func(head state.Head) `json:"-"`
}

// Drafter is a forge that shows a train from assembly (§5.4): a draft pull
// request is opened once its lines are merged, while its cheap tier runs,
// and is promoted, which starts CI, only when pre-flight passes. A draft
// starts no CI. Optional: without it, the pull request opens at promotion.
type Drafter interface {
	// OpenDraft opens the train's pull request on head, against base, as
	// a draft. Changes are as for Forge.OpenPR. The same key returns the
	// same pull request.
	OpenDraft(ctx context.Context, key string, train state.TrainID, changes []state.ChangeID, head, base state.Head) (pr string, err error)
	// Promote marks the draft ready and starts its CI on head, the built
	// head, to which the train's branch has moved.
	Promote(ctx context.Context, key string, train state.TrainID, pr string, head state.Head) error
	// Retitle names on the draft pr the changes the train carries now, in
	// ride order, as OpenDraft names them: its title and its list of
	// changes, with head the built head and base the train's base. The
	// mark of the key the draft was opened with is kept as it is. A build
	// that leaves lines out leaves the draft naming changes it does not
	// carry; the runner retitles it at promotion. It is cosmetic: the
	// runner never holds a train for it.
	Retitle(ctx context.Context, train state.TrainID, pr string, changes []state.ChangeID, head, base state.Head) error
}

// BuildResult is the builder's answer. Lines left out (a conflict, a cap)
// come back with a reason each. A pre-flight failure names the lines its
// blame rules name, which may be none.
type BuildResult struct {
	Head    state.Head
	LeftOut map[state.LineID]string
	// Moved gives the new head of each change the builder found pushed to
	// since its line was offered (or re-keyed). Its line is left out, and
	// re-keyed at the new head once (PROTOCOL.md row L39), not left out
	// again at every assembly at a head it no longer has.
	Moved map[state.ChangeID]state.Head
	// Conflicted names the lines left out for a conflict with the train's
	// base itself, that the conflict hook could not resolve: the line alone
	// does not merge onto Base. A line left out for a conflict only with a
	// line before it in the train is not named. On a train built on trunk,
	// a line named is in conflict with trunk, and goes to the runner's
	// rebase and, if that fails, back to its owner once (PROTOCOL.md row
	// L40), not left out again at every assembly.
	Conflicted      map[state.LineID]bool
	PreflightFailed bool
	Blamed          []state.LineID
	Evidence        string
	// NoVerdict, when set, says the pre-flight could not judge the train:
	// its own infrastructure failed (a service it needs answered 5xx, a
	// host was lost). The train ends with no verdict, by infrastructure
	// (§5.5 class 1): no line is charged, and none spends a budget.
	NoVerdict string
}

// Git builds trains and moves trunk.
type Git interface {
	// Build merges the lines on req.Base and runs pre-flight. The same key
	// gives the same result.
	Build(ctx context.Context, key string, req BuildRequest) (BuildResult, error)
	// Push fast-forwards trunk from expect to head, and nothing else (I2).
	// Trunk already at head is success: the push took. Trunk anywhere else
	// is a permanent refusal: no fallback merge (§5.7).
	Push(ctx context.Context, key string, head, expect state.Head) error
}

// Ancestry reads the commit graph, so that a need read from the forge's
// declared base can be checked against the heads (PROTOCOL.md §5.2: where
// the two disagree, the line is stale). Optional: without it, no line is
// found stale.
type Ancestry interface {
	// Contains reports whether head contains ancestor: ancestor is head, or
	// is in its history. ids are the changes whose heads these are, which
	// the reader may fetch first.
	Contains(ctx context.Context, head, ancestor state.Head, ids []state.ChangeID) (bool, error)
}

// Pruner tidies what the forge holds for trains that have ended: their
// branches (open question 20). Optional. Prune is told the trains still
// live and removes what belongs to any other; it is safe to repeat.
type Pruner interface {
	Prune(ctx context.Context, live []state.TrainID) error
}

// Filer lists the files a change touches, for the gate's tier classifier
// (§5.4): every path added, changed, removed or renamed, a rename's old name
// included. It answers the WHOLE list or an error: a list it cannot vouch for
// as complete is an error, and the line is then full. Optional: a forge
// without it has no light tier.
type Filer interface {
	Files(ctx context.Context, id state.ChangeID) ([]string, error)
}

// Retargeter moves a change stacked on a change that has landed onto trunk
// (open question 21). Optional, and advisory: the gate accepts such a change
// whether or not the retarget takes.
type Retargeter interface {
	Retarget(ctx context.Context, id state.ChangeID) error
}

// Rebaser rebases a waiting line that conflicts with trunk, where that needs
// no person (§5.2): generated files, a counters file, both sides only adding
// lines. Optional. It pushes the rebased heads itself, through whatever the
// producers' own pushes go through.
type Rebaser interface {
	Rebase(ctx context.Context, key string, line BuildLine, trunk state.Head) (RebaseResult, error)
}

// RebaseResult is a rebase's answer: the new head of each change it moved,
// or the conflict that needs a person.
type RebaseResult struct {
	Heads    map[state.ChangeID]state.Head
	Conflict string
}

// Merger tests whether a line merges onto trunk with no person: the
// re-offerer asks it before it offers again a line sent back uncharged, for a
// conflict with trunk or with its base lost (docs/queue-as-bag.md §4).
// Optional: without it, such a line comes back only by an offer.
type Merger interface {
	// Merges answers the files in conflict when the line's heads are
	// merged onto trunk, or "" when they merge cleanly. It changes nothing.
	Merges(ctx context.Context, line BuildLine, trunk state.Head) (conflict string, err error)
}

// OwnerNotice is a notice to a line's owner with what it is about: State is
// the state the line went to (empty when it didn't move: advice), and Cause
// why, so that delivery can act by kind (a charge marks the change as being
// fixed; a conflict asks for a rebase).
type OwnerNotice struct {
	Line    state.LineID
	Changes []state.ChangeID
	State   string
	Cause   state.Cause
	Message string
}

// Teller is a Notifier that takes an owner's notice whole. Optional: the
// runner calls Owner on a notifier without it.
type Teller interface {
	Tell(ctx context.Context, key string, n OwnerNotice) error
}

// Notifier tells people.
type Notifier interface {
	// Owner tells a line's owner what became of it.
	Owner(ctx context.Context, key string, line state.LineID, changes []state.ChangeID, msg string) error
	// Judge opens a judgement point, or raises an alert (point "alert").
	Judge(ctx context.Context, key string, point string, train state.TrainID, msg string) error
}

// Versioner is an adapter that runs checkers whose verdicts are stored: it
// says each one's present version, by its name (state.Checkers), so that a
// verdict of another version stops counting (docs/queue-as-bag.md §5).
type Versioner interface {
	Versions() map[string]string
}

// Admitter is a repository's own rules at the gate (§5.2), asked after the
// gate's own checks pass: it may refuse an offer, with the reason its
// producer is told. An error is no answer, and refuses nothing.
type Admitter interface {
	Admit(ctx context.Context, o Offered) (refuse string, err error)
}

// Offered is a line at the gate: its changes as the forge describes them,
// in stack order, and trunk.
type Offered struct {
	Line    state.LineID    `json:"line"`
	Changes []OfferedChange `json:"changes"`
	Alone   bool            `json:"alone"`
	Trunk   state.Head      `json:"trunk"`
}

// OfferedChange is one change of a line at the gate.
type OfferedChange struct {
	ID   state.ChangeID `json:"id"`
	Head state.Head     `json:"head"`
	// Base is the change it is stacked on, or empty on trunk.
	Base state.ChangeID `json:"base,omitempty"`
}

// Set is every adapter the daemon needs.
type Set struct {
	Forge  Forge
	CI     CI
	Git    Git
	Notify Notifier
	// Admit is optional.
	Admit Admitter
}
