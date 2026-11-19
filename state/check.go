package state

import "fmt"

// Check tests the invariants that are properties of one state. It returns
// every breach it finds. Next is built so that no reachable state has any;
// the tests and the replayer call Check after every step to prove it.
func Check(s State) []error {
	var errs []error
	fail := func(inv, format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", inv, fmt.Sprintf(format, a...)))
	}

	// I8, one state, one record: a line rides exactly when one live train
	// lists it, and the chain is exactly the live trains.
	riders := map[LineID]TrainID{}
	live := map[TrainID]bool{}
	for _, id := range s.Chain {
		t := s.Trains[id]
		if t == nil || !t.State.Live() || live[id] {
			fail("I8", "the chain names %s, which is not one live train", id)
			continue
		}
		live[id] = true
		for _, l := range t.Lines {
			if other, dup := riders[l]; dup {
				fail("I8", "line %s rides %s and %s", l, other, id)
			}
			riders[l] = id
		}
	}
	for id, t := range s.Trains {
		if t.State.Live() != live[id] {
			fail("I8", "train %s is %s and its place in the chain disagrees", id, t.State)
		}
	}
	for id, l := range s.Lines {
		t, rides := riders[id]
		switch {
		case l.State == LineRiding && (!rides || l.Train != t):
			fail("I8", "line %s is riding, and no live train agrees", id)
		case l.State != LineRiding && (rides || l.Train != ""):
			fail("I8", "line %s is %s and is in train %s%s", id, l.State, t, l.Train)
		}
		// A held line is never in a train, and never spent while held.
		if l.State == LineHeld && rides {
			fail("held", "held line %s rides %s", id, t)
		}
		// I6 and L1: a returned line has a reason; a parked line has a park
		// with a restore condition.
		if l.State == LineReturned && l.Reason == "" {
			fail("I6", "line %s was returned with no reason", id)
		}
		if l.State == LineParked {
			if p := s.Parks[l.Park]; p == nil || p.Cond == "" {
				fail("L1", "line %s is parked with no restore condition", id)
			}
		}
		// I3, needs first: a line rides only behind its base, lands only
		// after it, and doesn't wait on a base that won't land.
		if l.Base != "" {
			b := s.Lines[l.Base]
			switch {
			case b == nil:
				fail("I3", "line %s needs line %s, which is unknown", id, l.Base)
			case l.State == LineLanded && b.State != LineLanded:
				fail("I3", "line %s landed and its base %s is %s", id, l.Base, b.State)
			case l.State == LineRiding && !behind(s, l, b):
				fail("I3", "line %s rides in %s, not behind its base %s (%s)", id, l.Train, l.Base, b.State)
			case l.State == LineWaiting && (b.State == LineReturned || b.State == LineDropped):
				fail("I3", "line %s waits on its base %s, which is %s", id, l.Base, b.State)
			}
		}
	}

	// I5, speculation is exact: each train is built on the exact head of the
	// one ahead, and the first on trunk. I7: at most one train is landing,
	// and it is the first.
	if len(s.Chain) > s.Config.Depth {
		fail("I5", "%d trains ride; the depth is %d", len(s.Chain), s.Config.Depth)
	}
	on := s.Trunk
	for i, id := range s.Chain {
		t := s.Trains[id]
		if t == nil {
			continue
		}
		if t.BaseHead != on {
			fail("I5", "train %s is built on %s; the head ahead of it is %s", id, t.BaseHead, on)
		}
		if t.State == TrainLanding && i != 0 {
			fail("I7", "train %s is landing and is not first", id)
		}
		if t.State >= TrainBuilt && t.State != TrainJudging && t.Head == "" {
			fail("I5", "train %s is %s with no head", id, t.State)
		}
		on = t.Head
	}

	// I1 and I9: every landing the runner made is of a train whose verdict
	// is on exactly its head, verified by the lander. A cancelled train
	// never lands.
	at := Head("")
	for _, ld := range s.Landings {
		at = ld.Head
		if ld.Foreign {
			continue
		}
		t := s.Trains[ld.Train]
		switch {
		case t == nil:
			fail("I1", "landing %s names no train", ld.Head)
		case t.State != TrainLanded:
			fail("I1", "train %s is on trunk and is %s", t.ID, t.State)
		case t.Head != ld.Head || t.Verdict != ld.Head || !t.Verified:
			fail("I1", "train %s landed %s with a verdict on %q (verified: %v)", t.ID, ld.Head, t.Verdict, t.Verified)
		}
	}
	if len(s.Landings) > 0 && at != s.Trunk {
		fail("I2", "trunk is at %s; the last landing is %s", s.Trunk, at)
	}
	for id, t := range s.Trains {
		if (t.State == TrainLanding || t.State == TrainLanded) && (t.Verdict != t.Head || !t.Verified) {
			fail("I9", "train %s is %s without a verified verdict on its head", id, t.State)
		}
	}
	return errs
}

// CheckStep tests the invariants that are properties of a step: what may
// never be undone. fx is what Next returned for the step.
func CheckStep(before, after State, fx []Effect) []error {
	var errs []error
	fail := func(inv, format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", inv, fmt.Sprintf(format, a...)))
	}
	// I2, fast-forward only: trunk's history only grows, one landing a step.
	if len(after.Landings) < len(before.Landings) || len(after.Landings) > len(before.Landings)+1 {
		fail("I2", "trunk's history went from %d landings to %d", len(before.Landings), len(after.Landings))
	} else {
		for i, ld := range before.Landings {
			if after.Landings[i] != ld {
				fail("I2", "landing %d was rewritten", i)
			}
		}
	}
	if len(after.Landings) == len(before.Landings) && after.Trunk != before.Trunk {
		fail("I2", "trunk moved with no landing")
	}
	// A held line is never spent: its only way out is a release, back into
	// the queue.
	released := map[LineID]bool{}
	for _, e := range fx {
		if e.Kind != EffRecord || e.Line == "" || e.From != LineHeld.String() {
			continue
		}
		released[e.Line] = e.Cause == CauseRelease
		if e.Cause != CauseRelease {
			fail("held", "held line %s went to %s by %s", e.Line, e.To, e.Cause)
		}
	}
	for id, b := range before.Lines {
		a := after.Lines[id]
		if a == nil {
			fail("I8", "line %s vanished", id)
			continue
		}
		if b.State == LineHeld && a.State != LineHeld && !released[id] {
			fail("held", "held line %s became %s with no release", id, a.State)
		}
		if b.State == LineLanded && a.State != LineLanded {
			fail("I2", "landed line %s became %s", id, a.State)
		}
		// I4, frozen while riding: a head that moves voids the train.
		if b.State == LineRiding && a.State == LineRiding && b.Train == a.Train {
			for i := range b.Changes {
				if a.Changes[i].Head != b.Changes[i].Head {
					fail("I4", "change %s moved and line %s still rides %s", b.Changes[i].ID, id, a.Train)
				}
			}
		}
	}
	for id, b := range before.Trains {
		a := after.Trains[id]
		if a == nil {
			fail("I8", "train %s vanished", id)
			continue
		}
		// An ended train stays ended; a cancelled train never lands.
		if !b.State.Live() && a.State != b.State {
			fail("ended", "train %s was %s and became %s", id, b.State, a.State)
		}
		if b.Head != "" && a.Head != b.Head {
			fail("I1", "train %s's head changed after it was built", id)
		}
	}
	return errs
}

// behind reports whether riding line l rides behind its base b: b has
// landed, or rides earlier in l's train, or in a train earlier in the chain.
func behind(s State, l, b *Line) bool {
	if b.State == LineLanded {
		return true
	}
	if b.State != LineRiding {
		return false
	}
	if b.Train == l.Train {
		t := s.Trains[l.Train]
		if t == nil {
			return false
		}
		for _, id := range t.Lines {
			if id == b.ID {
				return true
			}
			if id == l.ID {
				return false
			}
		}
		return false
	}
	for _, id := range s.Chain {
		if id == b.Train {
			return true
		}
		if id == l.Train {
			return false
		}
	}
	return false
}
