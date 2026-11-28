package state

import (
	"errors"
	"testing"
)

func gateOn(why string) Event  { return RequireCheck{On: true, Reason: why} }
func gateOff(why string) Event { return RequireCheck{On: false, Reason: why} }

// The gate is turned on at run time, with lines already waiting: a line with
// no current green line check stays waiting until its check passes; a line
// that has one may ride at once.
func TestRequireCheckOnHoldsUncheckedLines(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b"), pass("a", "a1"), gateOn("the checker is live"))...)
	if !s.Config.RequireCheck {
		t.Fatal("the gate is not on")
	}
	if _, _, err := Next(s, assemble("t1", "b")); !errors.Is(err, ErrJoin) {
		t.Fatalf("an unchecked line was assembled under the gate: %v", err)
	}
	if s.CanRide(s.Lines["b"], nil) {
		t.Fatal("the joining rule would take an unchecked line")
	}
	s = apply(t, s, built("t1", "a")...)
	if !s.Trains["t1"].Gated {
		t.Fatal("a train assembled under the gate is not marked so")
	}
	s = apply(t, s, pass("b", "b1"), assemble("t2", "b"))
	if s.Lines["b"].State != LineRiding {
		t.Fatalf("b is %s once checked", s.Lines["b"].State)
	}
}

// A train already assembling or riding when the gate is turned on is not
// affected: it lands on what it was assembled with, and I10 holds of it.
func TestRequireCheckOnLeavesTrainsInFlight(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b", "c"), green("t1", "a"), assemble("t2", "b"), gateOn("r"))...)
	if s.Trains["t1"].Gated || s.Trains["t2"].Gated {
		t.Fatal("a train assembled before the gate is marked as gated")
	}
	if s.Trains["t1"].State != TrainGreen || s.Trains["t2"].State != TrainAssembling {
		t.Fatalf("trains in flight changed: t1 %s, t2 %s", s.Trains["t1"].State, s.Trains["t2"].State)
	}
	s = apply(t, s, verify(s, "t1"), PushOK{Train: "t1"})
	if s.Lines["a"].State != LineLanded {
		t.Fatalf("a is %s: the riding train did not land", s.Lines["a"].State)
	}
	s = apply(t, s, Built{Train: "t2", Head: "h-t2"}, Opened{Train: "t2", PR: "pr-t2", Head: "h-t2"})
	if s.Trains["t2"].State != TrainTesting {
		t.Fatalf("t2 is %s", s.Trains["t2"].State)
	}
	if _, _, err := Next(s, assemble("t3", "c")); !errors.Is(err, ErrJoin) {
		t.Fatalf("an unchecked line joined a new train: %v", err)
	}
}

// Turned off, the gate releases the waiting lines it held at once; trains
// assembled under it keep I10.
func TestRequireCheckOffReleasesLines(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2, RequireCheck: true}), cat(ready("a", "b"), pass("a", "a1"), built("t1", "a"), gateOff("the checker is down"))...)
	if s.Config.RequireCheck {
		t.Fatal("the gate is still on")
	}
	if !s.CanRide(s.Lines["b"], nil) {
		t.Fatal("an unchecked line is still held with the gate off")
	}
	s = apply(t, s, assemble("t2", "b"))
	if s.Lines["b"].State != LineRiding || s.Trains["t2"].Gated {
		t.Fatalf("b is %s, t2 gated %v", s.Lines["b"].State, s.Trains["t2"].Gated)
	}
	if !s.Trains["t1"].Gated {
		t.Fatal("t1 lost its mark")
	}
}

func TestRequireCheckRefusals(t *testing.T) {
	s := New("trunk0", Config{Depth: 2})
	if _, _, err := Next(s, RequireCheck{On: true}); !errors.Is(err, ErrReason) {
		t.Fatalf("no reason: %v", err)
	}
	if _, _, err := Next(s, gateOff("r")); !errors.Is(err, ErrState) {
		t.Fatalf("off when off: %v", err)
	}
	s = apply(t, s, gateOn("r"))
	if _, _, err := Next(s, gateOn("r")); !errors.Is(err, ErrState) {
		t.Fatalf("on when on: %v", err)
	}
}

// I10 is a property of trains assembled under the gate: a line of a gated
// train riding with no current check still breaks it.
func TestI10HoldsOfGatedTrains(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2, RequireCheck: true}), cat(ready("a"), pass("a", "a1"), assemble("t1", "a"))...)
	s.Lines["a"].Checked = nil
	if len(Check(s)) == 0 {
		t.Fatal("a gated train's line rides unchecked and I10 holds")
	}
	s.Trains["t1"].Gated = false
	if errs := Check(s); len(errs) != 0 {
		t.Fatalf("an ungated train broke I10: %v", errs)
	}
}
