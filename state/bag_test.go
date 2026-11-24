package state

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func order(t *testing.T, s State, want ...string) {
	t.Helper()
	if got := s.Waiting(); !slices.Equal(got, ids(want...)) {
		t.Fatalf("waiting in the order %v, want %v", got, want)
	}
}

// The queue is a bag (docs/queue-as-bag.md §2): a line's age is its first
// offer, and coming back costs nothing and gains nothing. A charged line,
// fixed and offered again, is exactly as old as it was.
func TestAReofferedLineKeepsItsAge(t *testing.T) {
	s := apply(t, charged(t), offerBy("bob", "b", "b1"), GateAccept{Line: "b"})
	s = apply(t, s, offerBy("ann", "a", "a2"), GateAccept{Line: "a"})
	order(t, s, "a", "b")
}

// A line that comes back from a void train is as old as before, and no
// older: it does not go to the front past an older line.
func TestAVoidTrainsLinesKeepTheirAge(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b", "c"), testing_("t1", "b"), CINoVerdict{Train: "t1", Why: "cancelled"})...)
	order(t, s, "a", "b", "c")
}

// A stack sorts by the age of its bottom (§3, step 3): the line stacked on
// the oldest line goes before an unrelated line offered between them.
func TestAStackSortsByItsBottom(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b"), stacked("c", "a"), GateAccept{Line: "c"}, stacked("d", "c"), GateAccept{Line: "d"})...)
	order(t, s, "a", "c", "d", "b")
	// With its bottom riding, the stack's next line is the bottom of what
	// waits, and keeps the bottom's age.
	s = apply(t, s, testing_("t1", "a")...)
	order(t, s, "c", "d", "b")
}

func prioritise(id string) Prioritise {
	return Prioritise{Line: LineID(id), By: "judge", Reason: "fixes trunk", Until: at.Add(2 * time.Hour)}
}

// The one override is a manual priority (§2): it needs a reason and an
// expiry, it puts the line first, and it covers the lines it is stacked
// on, which must ride first.
func TestAManualPriorityIsTheOneOverride(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b", "c"), stacked("d", "b"), GateAccept{Line: "d"})...)
	for _, bad := range []Prioritise{{Line: "d", By: "judge", Until: at}, {Line: "d", By: "judge", Reason: "r"}} {
		if _, _, err := Next(s, bad); !errors.Is(err, ErrReason) {
			t.Fatalf("a priority without a reason or an expiry: %v", err)
		}
	}
	p := apply(t, s, prioritise("d"))
	order(t, p, "b", "d", "a", "c")
	p = apply(t, p, PriorityEnded{Line: "d", Why: "expired"})
	order(t, p, "a", "b", "d", "c")
}

// A priority ends when its line is spent.
func TestAPriorityEndsWithItsLine(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b"), prioritise("b"), Withdraw{Line: "b"})...)
	if s.Lines["b"].Priority != nil {
		t.Fatal("a withdrawn line kept its priority")
	}
	if _, _, err := Next(s, prioritise("b")); !errors.Is(err, ErrState) {
		t.Fatalf("a spent line was given a priority: %v", err)
	}
}

// A train's lines are in the policy's order, a base before what is stacked
// on it, whatever their offers' order.
func TestATrainsLinesAreInPolicyOrder(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b"), stacked("c", "a"), GateAccept{Line: "c"}, assemble("t1", "b", "c", "a"))...)
	if got := s.Trains["t1"].Lines; !slices.Equal(got, ids("a", "c", "b")) {
		t.Fatalf("train t1 holds %v", got)
	}
}
