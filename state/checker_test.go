package state

import (
	"errors"
	"testing"
	"time"
)

func version(c, v string) CheckerChanged { return CheckerChanged{Checker: c, Version: v} }

// A green counts only while its line check is the present one
// (docs/queue-as-bag.md §5, the model's VerdictsByPresentChecker). A verdict
// or a claim of a replaced line check is dropped.
func TestAGreenIsItsCheckersOnly(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2, RequireCheck: true}), cat(version(CheckerLineCheck, "v1"), ready("a", "b"),
		LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Pass: true, Version: "v1"},
		CheckClaimed{Line: "b", Key: CheckKey{Heads: []Head{"b1"}, Trunk: "trunk0", Checker: "v1"}, By: "w1", Until: at.Add(time.Hour)})...)
	if _, _, err := Next(s, version("no-such", "v2")); !errors.Is(err, ErrInput) {
		t.Fatalf("an unknown checker: %v", err)
	}
	s = apply(t, s, version(CheckerLineCheck, "v2"))
	if s.Lines["a"].CheckCurrent() || s.Lines["b"].Claim != nil {
		t.Fatalf("a replaced checker's green or claim stands: %+v %+v", s.Lines["a"].Checked, s.Lines["b"].Claim)
	}
	if _, _, err := Next(s, assemble("t1", "a")); !errors.Is(err, ErrJoin) {
		t.Fatalf("assembled on a replaced checker's green: %v", err)
	}
	s = apply(t, s, LineChecked{Line: "b", Heads: []Head{"b1"}, Trunk: "trunk0", Evidence: "old rule", Version: "v1"})
	if s.Lines["b"].State != LineWaiting {
		t.Fatalf("a replaced checker's red charged b: %s", s.Lines["b"].State)
	}
	if _, _, err := Next(s, CheckClaimed{Line: "b", Key: CheckKey{Heads: []Head{"b1"}, Trunk: "trunk0", Checker: "v1"}, By: "w1", Until: at.Add(time.Hour)}); !errors.Is(err, ErrState) {
		t.Fatalf("a claim by a replaced checker: %v", err)
	}
	s = apply(t, s, LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Pass: true, Version: "v2"}, assemble("t1", "a"))
	// One that rides keeps its green: its train judges it.
	s = apply(t, s, version(CheckerLineCheck, "v3"))
	if !s.Lines["a"].CheckCurrent() {
		t.Fatal("a riding line lost its green")
	}
}

// A charge by a replaced checker is void: the line goes back through the
// gate as it stands, parked if the queue is. A charge by the judge stands.
func TestAChargeByAReplacedCheckerIsVoid(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(version(CheckerLineCheck, "v1"), version(CheckerBlame, "b1"), ready("a", "b", "c", "d"),
		LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Evidence: "a's test fails", Version: "v1"},
		testing_("t1", "b", "c"), CIFailed{Train: "t1", Blamed: ids("b"), Evidence: "b's test fails"},
		testing_("t2", "d"), CIFailed{Train: "t2", Evidence: "a test fails"},
		Judge{Train: "t2", Action: JudgeCharge, Lines: ids("d"), Evidence: "the judge read the log"})...)
	for _, id := range []LineID{"a", "b", "d"} {
		if l := s.Lines[id]; l.State != LineReturned || l.Cause != CauseCharged {
			t.Fatalf("line %s is %s (%s)", id, l.State, l.Cause)
		}
	}
	if s.Lines["b"].ChargedBy != (Verdict{Checker: CheckerBlame, Version: "b1"}) || s.Lines["d"].ChargedBy != (Verdict{}) {
		t.Fatalf("charged by %+v and %+v", s.Lines["b"].ChargedBy, s.Lines["d"].ChargedBy)
	}
	s = apply(t, s, version(CheckerLineCheck, "v2"))
	if l := s.Lines["a"]; l.State != LineWaiting || l.Cause != CauseChargeVoid || len(l.ChargedAt) != 0 {
		t.Fatalf("line a after its checker changed: %s (%s) %v", l.State, l.Cause, l.ChargedAt)
	}
	if s.Lines["b"].State != LineReturned {
		t.Fatal("a change of the line check voided a charge by blame")
	}
	s = apply(t, s, ParkLines{Park: "p", Cond: "deadline: the judge", Reason: "trunk is red"}, version(CheckerBlame, "b2"))
	if l := s.Lines["b"]; l.State != LineParked || l.Cause != CauseChargeVoid {
		t.Fatalf("line b after blame changed, the queue parked: %s (%s)", l.State, l.Cause)
	}
	if s.Lines["d"].State != LineReturned {
		t.Fatal("a change of software voided the judge's charge")
	}
}

// A train still assembling when the pre-flight changes runs the new one.
func TestATrainAssemblingRunsANewPreflight(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a"), assemble("t1", "a"), version(CheckerPreflight, "p2"))...)
	if s.Trains["t1"].State != TrainVoid || s.Lines["a"].State != LineWaiting {
		t.Fatalf("train t1 is %s, line a %s", s.Trains["t1"].State, s.Lines["a"].State)
	}
}
