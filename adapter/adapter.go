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
	// starts its CI. The same key returns the same pull request.
	OpenPR(ctx context.Context, key string, train state.TrainID, head, base state.Head) (pr string, err error)
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
}

// BuildResult is the builder's answer. Lines left out (a conflict, a cap)
// come back with a reason each. A pre-flight failure names the lines its
// blame rules name, which may be none.
type BuildResult struct {
	Head            state.Head
	LeftOut         map[state.LineID]string
	PreflightFailed bool
	Blamed          []state.LineID
	Evidence        string
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

// Notifier tells people.
type Notifier interface {
	// Owner tells a line's owner what became of it.
	Owner(ctx context.Context, key string, line state.LineID, changes []state.ChangeID, msg string) error
	// Judge opens a judgement point, or raises an alert (point "alert").
	Judge(ctx context.Context, key string, point string, train state.TrainID, msg string) error
}

// Set is every adapter the daemon needs.
type Set struct {
	Forge  Forge
	CI     CI
	Git    Git
	Notify Notifier
}
