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
	// A train is judged only at the head of the chain (§5.3): one at J1 is
	// first, and one whose failure waits is behind another and not yet
	// judged, in the state it reported from.
	for i, id := range s.Chain {
		t := s.Trains[id]
		if t == nil {
			continue
		}
		if t.State == TrainJudging && i > 0 {
			fail("§5.3", "train %s is at J1 behind train %s, which is not decided", id, s.Chain[0])
		}
		if t.Behind != nil && (i == 0 || (t.State != TrainAssembling && t.State != TrainTesting)) {
			fail("§5.3", "train %s holds an unjudged failure at place %d, in state %s", id, i+1, t.State)
		}
	}
	// §5.4: a train is never given a lighter tier than its lines allow. A
	// light train holds inert lines only.
	for _, id := range s.Chain {
		t := s.Trains[id]
		if t == nil || !t.Light {
			continue
		}
		for _, lid := range t.Lines {
			if l := s.Lines[lid]; l != nil && l.State == LineRiding && !l.Inert {
				fail("§5.4", "train %s is light, and its line %s is not inert", id, lid)
			}
		}
	}
	for id, t := range s.Trains {
		if t.Behind != nil && !t.State.Live() {
			fail("§5.3", "ended train %s still holds an unjudged failure", id)
		}
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
		// I10, checked before assembled: where the queue requires line
		// checks, a line rides only on the heads its green check was taken
		// on. (A head pushed while its train lands is kept apart, in Late.)
		if s.Config.RequireCheck && l.State == LineRiding && !l.CheckCurrent() {
			fail("I10", "line %s rides with no green line check on its present heads", id)
		}
		// A line in conflict with trunk, or at the heads it was charged on,
		// rides no train (§5.2, §7).
		if l.State == LineRiding && l.Conflict != "" {
			fail("§5.2", "line %s rides in conflict with trunk: %s", id, l.Conflict)
		}
		// A claim is on a waiting line, at the heads it has (§5.4).
		if l.Claim != nil && (l.State != LineWaiting || !l.OnHeads(l.Claim.Key)) {
			fail("§5.4", "line %s is %s and holds a check claim on other heads or out of the queue", id, l.State)
		}
		if l.State == LineRiding && l.AtChargedHeads() {
			fail("§7", "line %s rides at the heads it was charged on", id)
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
	cancelled := map[TrainID]bool{}
	for _, e := range fx {
		if e.Kind == EffCancelRun {
			cancelled[e.Train] = true
		}
	}
	for id, b := range before.Trains {
		a := after.Trains[id]
		if a == nil {
			fail("I8", "train %s vanished", id)
			continue
		}
		// A train built has a CI run, or is about to: one that ends without
		// landing has its run cancelled, since closing its pull request
		// does not stop it. Cancelling a finished run is harmless.
		if b.State >= TrainBuilt && b.State.Live() && !a.State.Live() && a.State != TrainLanded && !cancelled[id] {
			fail("run", "train %s went from %s to %s and its run was not cancelled", id, b.State, a.State)
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
