package state

import (
	"errors"
	"testing"
	"time"
)

var at = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

func claim(id, by string, head Head) CheckClaimed {
	return CheckClaimed{Line: LineID(id), Key: CheckKey{Heads: []Head{head}, Trunk: "trunk0"}, By: by, Until: at.Add(30 * time.Minute)}
}

// §5.4: one claim stands on a line's check. Another checker is refused,
// a verdict on another key is refused, and the claim's own verdict ends it.
func TestOneClaimStandsOnALineCheck(t *testing.T) {
	s := checkedQueue(t, cat(ready("a"), claim("a", "w1", "a1"))...)
	if _, _, err := Next(s, claim("a", "w2", "a1")); !errors.Is(err, ErrClaimed) {
		t.Fatalf("a second checker took a claimed line: %v", err)
	}
	// The same checker renews its claim on the same key.
	s = checkedQueue(t, cat(ready("a"), claim("a", "w1", "a1"), claim("a", "w1", "a1"))...)
	other := LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk9", Pass: true}
	if _, _, err := Next(s, other); !errors.Is(err, ErrClaimed) {
		t.Fatalf("a verdict on another trunk was taken under the claim: %v", err)
	}
	s = checkedQueue(t, cat(ready("a"), claim("a", "w1", "a1"), pass("a", "a1"))...)
	if l := s.Lines["a"]; l.Claim != nil || !l.CheckCurrent() {
		t.Fatalf("the claim's verdict: claim %v, green %v", l.Claim, l.CheckCurrent())
	}
}

// A claim ends with a push, with the line leaving the queue, and at its
// lease's end; then another checker may claim it.
func TestAClaimEndsWithAPushALeaveOrItsLease(t *testing.T) {
	s := checkedQueue(t, cat(ready("a"), claim("a", "w1", "a1"), HeadMoved{Line: "a", Change: "a", Head: "a2"})...)
	if s.Lines["a"].Claim != nil {
		t.Fatal("the claim outlived a push")
	}
	s = checkedQueue(t, cat(ready("a"), claim("a", "w1", "a1"), Hold{Line: "a", Reason: "r"})...)
	if s.Lines["a"].Claim != nil {
		t.Fatal("the claim outlived the line's leaving the queue")
	}
	s = checkedQueue(t, cat(ready("a"), claim("a", "w1", "a1"), ClaimExpired{Line: "a"}, claim("a", "w2", "a1"))...)
	if c := s.Lines["a"].Claim; c == nil || c.By != "w2" {
		t.Fatalf("after the lease, the claim is %+v", c)
	}
	// A verdict from the checker whose lease ran out, on the same key, is
	// a verdict on those heads.
	s = checkedQueue(t, cat(ready("a"), claim("a", "w1", "a1"), ClaimExpired{Line: "a"}, pass("a", "a1"))...)
	if !s.Lines["a"].CheckCurrent() {
		t.Fatal("a late verdict on the present heads was lost")
	}
}

// A claim is refused on heads the line does not have, on a line that waits
// for a rebase, and on a stacked line before its base is checked.
func TestAClaimIsOnlyForALineDueACheck(t *testing.T) {
	s := checkedQueue(t, cat(ready("a"))...)
	if _, _, err := Next(s, claim("a", "w1", "a0")); !errors.Is(err, ErrState) {
		t.Fatalf("a claim on other heads: %v", err)
	}
	if _, _, err := Next(s, CheckClaimed{Line: "a", Key: CheckKey{Heads: []Head{"a1"}, Trunk: "trunk0"}, By: "w1"}); !errors.Is(err, ErrReason) {
		t.Fatalf("a claim with no lease: %v", err)
	}
	s = checkedQueue(t, cat(ready("a"), LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Conflict: "x.go"})...)
	if _, _, err := Next(s, claim("a", "w1", "a1")); !errors.Is(err, ErrState) {
		t.Fatalf("a claim on a line in conflict: %v", err)
	}
	s = checkedQueue(t, Offer{Line: "a", Changes: []Change{{ID: "a", Head: "a1"}}}, GateAccept{Line: "a"},
		Offer{Line: "b", Changes: []Change{{ID: "b", Head: "b1"}}, Base: "a"}, GateAccept{Line: "b"})
	if _, _, err := Next(s, claim("b", "w1", "b1")); !errors.Is(err, ErrState) {
		t.Fatalf("a stacked line claimed before its base was checked: %v", err)
	}
	s = checkedQueue(t, Offer{Line: "a", Changes: []Change{{ID: "a", Head: "a1"}}}, GateAccept{Line: "a"}, pass("a", "a1"),
		Offer{Line: "b", Changes: []Change{{ID: "b", Head: "b1"}}, Base: "a"}, GateAccept{Line: "b"}, claim("b", "w1", "b1"))
	if s.Lines["b"].Claim == nil {
		t.Fatal("no claim on a stacked line over a checked base")
	}
}

// A claim made under a declared line check takes that check's verdict: the
// claim keeps the version it was given, and the verdict names it.
func TestAClaimUnderAVersionTakesItsVerdict(t *testing.T) {
	key := CheckKey{Heads: []Head{"a1"}, Trunk: "trunk0", Checker: "v1"}
	s := checkedQueue(t, cat(version(CheckerLineCheck, "v1"), ready("a"),
		CheckClaimed{Line: "a", Key: key, By: "w1", Until: at.Add(time.Hour)},
		LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Pass: true, Version: "v1"})...)
	if l := s.Lines["a"]; l.Claim != nil || !l.CheckCurrent() {
		t.Fatalf("the claim's own verdict: claim %+v, green %v", l.Claim, l.CheckCurrent())
	}
}

// §5.4: a checker that can give no verdict (its infrastructure failed)
// gives its claim up at once, with a reason, instead of letting the lease
// run out. Nothing is recorded of the line: it is due a check again. Only
// the claim's own key gives it up, and a reason is required.
func TestAClaimIsReleasedWithNoVerdict(t *testing.T) {
	key := CheckKey{Heads: []Head{"a1"}, Trunk: "trunk0"}
	s := checkedQueue(t, cat(ready("a"), claim("a", "w1", "a1"))...)
	if _, _, err := Next(s, ClaimReleased{Line: "a", Key: key, By: "w1"}); !errors.Is(err, ErrReason) {
		t.Fatalf("a release with no reason: %v", err)
	}
	other := CheckKey{Heads: []Head{"a1"}, Trunk: "trunk9"}
	if _, _, err := Next(s, ClaimReleased{Line: "a", Key: other, By: "w1", Reason: "runner lost"}); !errors.Is(err, ErrClaimed) {
		t.Fatalf("a release on another key: %v", err)
	}
	s = checkedQueue(t, cat(ready("a"), claim("a", "w1", "a1"), ClaimReleased{Line: "a", Key: key, By: "w1", Reason: "runner lost"})...)
	if l := s.Lines["a"]; l.Claim != nil || l.CheckCurrent() || l.State != LineWaiting || len(l.ChargedAt) != 0 {
		t.Fatalf("after a release: %+v", l)
	}
	if _, _, err := Next(s, ClaimReleased{Line: "a", Key: key, By: "w1", Reason: "again"}); !errors.Is(err, ErrState) {
		t.Fatalf("a release with no claim standing: %v", err)
	}
	s = apply(t, s, claim("a", "w2", "a1"))
	if c := s.Lines["a"].Claim; c == nil || c.By != "w2" {
		t.Fatalf("after a release, the claim is %+v", c)
	}
}
