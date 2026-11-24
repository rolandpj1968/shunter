package state

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"
)

func apply(t *testing.T, s State, evs ...Event) State {
	t.Helper()
	for _, ev := range evs {
		next, _, err := Next(s, ev)
		if err != nil {
			t.Fatalf("%s refused: %v", Name(ev), err)
		}
		if errs := Check(next); len(errs) > 0 {
			t.Fatalf("after %s: %v", Name(ev), errs)
		}
		s = next
	}
	return s
}

func offerBy(by, id string, head Head) Event {
	return Offer{By: by, Line: LineID(id), Changes: []Change{{ID: ChangeID(id), Head: head}}}
}

// charged is a queue in which line a, offered by ann, was charged at a1.
func charged(t *testing.T) State {
	t.Helper()
	return apply(t, New("trunk0", Config{Depth: 2}), cat(offerBy("ann", "a", "a1"), GateAccept{Line: "a"}, testing_("t1", "a"),
		CIFailed{Train: "t1", Blamed: ids("a"), Evidence: "a's test fails"})...)
}

// The owner-state (§7): a charged line is with its owner until its owner
// says it is done. A push is not that, and nobody else's offer is.
func TestOnlyItsOwnerOffersAChargedLineAgainAndOnlyFixed(t *testing.T) {
	s := charged(t)
	if _, _, err := Next(s, offerBy("ann", "a", "a1")); !errors.Is(err, ErrUnfixed) {
		t.Fatalf("offered again at the heads it was charged on: %v", err)
	}
	if _, _, err := Next(s, offerBy("bob", "a", "a2")); !errors.Is(err, ErrOwner) {
		t.Fatalf("offered again by another than its owner: %v", err)
	}
	s2 := apply(t, s, offerBy("ann", "a", "a2"), GateAccept{Line: "a"}, assemble("t2", "a"))
	if s2.Lines["a"].State != LineRiding || s2.Lines["a"].Owner != "ann" {
		t.Fatalf("line a is %s, owned by %q", s2.Lines["a"].State, s2.Lines["a"].Owner)
	}
}

// A line never rides at the heads it was charged on, even when its head
// moves away and back.
func TestAChargedHeadNeverRidesAgain(t *testing.T) {
	s := apply(t, charged(t), offerBy("ann", "a", "a2"), GateAccept{Line: "a"}, HeadMoved{Line: "a", Change: "a", Head: "a1"})
	if _, _, err := Next(s, assemble("t2", "a")); !errors.Is(err, ErrUnfixed) {
		t.Fatalf("assembled at its charged head: %v", err)
	}
}

// A hand-over is the one way a charged line changes owner: recorded, with a
// reason, and told to the line's people.
func TestAHandOverChangesTheOwner(t *testing.T) {
	s := charged(t)
	if _, _, err := Next(s, HandOver{Line: "a", To: "bob"}); !errors.Is(err, ErrReason) {
		t.Fatalf("a hand-over with no reason: %v", err)
	}
	next, fx, err := Next(s, HandOver{Line: "a", To: "bob", Why: "ann is away; the deadline passed"})
	if err != nil {
		t.Fatal(err)
	}
	told := false
	for _, e := range fx {
		told = told || e.Kind == EffNotify
	}
	if !told {
		t.Error("nobody was told of the hand-over")
	}
	if _, _, err := Next(next, offerBy("ann", "a", "a2")); !errors.Is(err, ErrOwner) {
		t.Fatalf("the old owner offered it after the hand-over: %v", err)
	}
	apply(t, next, offerBy("bob", "a", "a2"), GateAccept{Line: "a"})
}

// A line returned uncharged (in a conflict with trunk the rebase couldn't
// resolve, its base lost) is anyone's to offer again, as it stands: nothing
// was found wrong with it.
func TestAnUnchargedReturnIsOfferedAgainAsItIs(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(offerBy("ann", "a", "a1"), GateAccept{Line: "a"},
		LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Conflict: "x.go"}, RebaseFailed{Line: "a", Reason: "a real conflict"})...)
	apply(t, s, offerBy("bob", "a", "a1"), GateAccept{Line: "a"})
}

// A line whose check finds it in conflict with trunk is charged nothing,
// joins no train, and the runner is asked, once, to rebase it (§5.2). A push
// ends the conflict; a rebase that fails sends the line back to its owner.
func TestALineInConflictWaitsForARebase(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(ready("a", "b"))...)
	conflict := LineChecked{Line: "a", Heads: []Head{"a1"}, Trunk: "trunk0", Conflict: "x.go"}
	next, fx, err := Next(s, conflict)
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	for _, e := range fx {
		if e.Kind == EffRebase && e.Line == "a" {
			asked++
		}
	}
	if asked != 1 || next.Lines["a"].State != LineWaiting || next.Lines["a"].Cause == CauseCharged {
		t.Fatalf("rebase asked %d times; line a is %s (%s)", asked, next.Lines["a"].State, next.Lines["a"].Cause)
	}
	if _, _, err := Next(next, assemble("t1", "a")); !errors.Is(err, ErrJoin) {
		t.Fatalf("a line in conflict was assembled: %v", err)
	}
	if _, fx, _ := Next(next, conflict); len(fx) != 0 {
		t.Errorf("the same conflict asked for a second rebase: %v", fx)
	}
	_, fx, err = Next(next, RebaseFailed{Line: "a", Reason: "both sides changed x.go"})
	if err != nil || len(fx) != 2 || fx[1].Kind != EffNotify || fx[0].To != "returned" {
		t.Fatalf("a failed rebase: %v %v", fx, err)
	}
	if _, _, err := Next(s, RebaseFailed{Line: "b", Reason: "r"}); !errors.Is(err, ErrState) {
		t.Fatalf("a failed rebase of a line not in conflict: %v", err)
	}
	apply(t, next, HeadMoved{Line: "a", Change: "a", Head: "a2"}, assemble("t1", "a"))
	// Asked once for these heads against this trunk; again only when a check
	// finds the conflict against a trunk that has moved.
	moved := conflict
	moved.Trunk = "trunk1"
	_, fx, err = Next(next, moved)
	if err != nil || len(fx) != 1 || fx[0].Kind != EffRebase {
		t.Fatalf("a conflict with a new trunk did not ask for a rebase: %v %v", fx, err)
	}
}

// §5.5: a recorded flake never charges a line. When it fails again after its
// one rerun nothing is judged, even for a train of one line; a timing
// failure's second rerun is still refused (it is blamed as deterministic).
func TestARecordedFlakeNeverCharges(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2, NoVerdictBudget: 3}), cat(ready("a"), testing_("t1", "a"),
		CIRerun{Train: "t1", Why: "TestShared is a recorded flake", Known: true})...)
	if _, _, err := Next(s, CIRerun{Train: "t1", Why: "only timeouts"}); !errors.Is(err, ErrBudget) {
		t.Fatalf("a second rerun for a timing failure: %v", err)
	}
	s = apply(t, s, CIRerun{Train: "t1", Why: "TestShared is a recorded flake", Known: true})
	if l := s.Lines["a"]; l.State != LineWaiting || l.Cause == CauseCharged || len(l.ChargedAt) != 0 {
		t.Fatalf("line a is %s (%s) after a recorded flake failed twice", l.State, l.Cause)
	}
	if s.Trains["t1"].State != TrainVoid {
		t.Fatalf("train t1 is %s", s.Trains["t1"].State)
	}
	if s.Lines["a"].NoVerdicts != 1 {
		t.Errorf("the no-verdict budget was not counted: %d", s.Lines["a"].NoVerdicts)
	}
}

// A random walk over owners, charges, conflicts and recorded flakes: Check
// finds no breach, and at the end no line rides in conflict or at a charged
// head (Check tests both in every state on the way).
func TestRandomWalkOfOwnersConflictsAndFlakes(t *testing.T) {
	for seed := int64(1); seed <= 80; seed++ {
		r := rand.New(rand.NewSource(seed))
		s := New("trunk0", Config{Depth: 2, RequireCheck: seed%2 == 0, NoVerdictBudget: 50})
		names := []string{"a", "b", "c", "d"}
		who := []string{"ann", "bob"}
		pick := func() LineID { return LineID(names[r.Intn(len(names))]) }
		trains, charges, reoffers, rebases := 0, 0, 0, 0
		for i := 0; i < 600; i++ {
			var ev Event
			tr := TrainID(fmt.Sprintf("t%d", max(1, trains-r.Intn(2))))
			switch r.Intn(18) {
			case 0, 1, 2:
				id := pick()
				head := Head(fmt.Sprintf("%s%d", id, i))
				if l := s.Lines[id]; l != nil && r.Intn(3) == 0 {
					head = l.Changes[0].Head // offered again as it stands
				}
				ev = Offer{By: who[r.Intn(2)], Line: id, Changes: []Change{{ID: ChangeID(id), Head: head}}}
			case 3, 4:
				ev = GateAccept{Line: pick()}
			case 5, 6, 7:
				id := pick()
				l := s.Lines[id]
				if l == nil {
					continue
				}
				e := LineChecked{Line: id, Heads: []Head{l.Changes[0].Head}, Trunk: s.Trunk}
				switch r.Intn(6) {
				case 0:
					e.Evidence = "e"
				case 1:
					e.Conflict = "x.go"
				default:
					e.Pass = true
				}
				ev = e
			case 8:
				ev = HeadMoved{Line: pick(), Change: ChangeID(pick()), Head: Head(fmt.Sprintf("m%d", i))}
			case 9:
				ev = HandOver{Line: pick(), To: who[r.Intn(2)], Why: "w"}
			case 10, 11:
				w := s.Waiting()
				if len(w) == 0 {
					continue
				}
				trains++
				ev = Assemble{Train: TrainID(fmt.Sprintf("t%d", trains)), Lines: w[:1+r.Intn(len(w))]}
			case 12:
				ev = Built{Train: tr, Head: Head("h-" + string(tr))}
			case 13:
				ev = Opened{Train: tr, PR: "n", Head: Head("h-" + string(tr))}
			case 14:
				ev = CIRerun{Train: tr, Why: "w", Known: r.Intn(2) == 0}
			case 15, 16:
				ev = CIFailed{Train: tr, Blamed: []LineID{pick()}, Evidence: "e"}
			case 17:
				ev = RebaseFailed{Line: pick(), Reason: "r"}
			}
			was := map[LineID]Cause{}
			for id, l := range s.Lines {
				if l.State == LineReturned {
					was[id] = l.Cause
				}
			}
			next, fx, err := Next(s, ev)
			if err != nil {
				continue
			}
			if errs := Check(next); len(errs) > 0 {
				t.Fatalf("seed %d, step %d, %s %+v: %v", seed, i, Name(ev), ev, errs)
			}
			// The runner is asked to rebase a line only when its check has
			// just found it in conflict with trunk: never a line that merges.
			for _, e := range fx {
				if e.Kind != EffRebase {
					continue
				}
				rebases++
				lc, ok := ev.(LineChecked)
				if !ok || lc.Conflict == "" || lc.Line != e.Line || next.Lines[e.Line].Conflict == "" {
					t.Fatalf("seed %d, step %d: a rebase of line %s was asked for by %s %+v", seed, i, e.Line, Name(ev), ev)
				}
			}
			if o, ok := ev.(Offer); ok && was[o.Line] == CauseCharged {
				reoffers++
				old := s.Lines[o.Line]
				if (old.Owner != "" && o.By != old.Owner) || o.Changes[0].Head == old.ChargedAt[0] {
					t.Fatalf("seed %d, step %d: a charged line was offered again by %q at %s (owner %q, charged at %s)", seed, i, o.By, o.Changes[0].Head, old.Owner, old.ChargedAt[0])
				}
			}
			for id, l := range next.Lines {
				if l.State == LineReturned && l.Cause == CauseCharged && was[id] != CauseCharged {
					charges++
				}
			}
			s = next
		}
		if seed == 1 && (charges == 0 || reoffers == 0 || rebases == 0) {
			t.Fatalf("the walk proves nothing: %d charges, %d offers of a charged line, %d rebases asked", charges, reoffers, rebases)
		}
	}
}

// A charge is only ever on the heads the line has at that moment. A line
// check takes time, and its verdict may arrive after a push: a failure on
// heads the line no longer has charges nobody, and the line stays where it
// is to be checked again. A train's charge is on the heads it was assembled
// at, which cannot have moved, since a push to a riding line voids its train.
func TestAChargeIsOnlyOnPresentHeads(t *testing.T) {
	for seed := int64(1); seed <= 80; seed++ {
		r := rand.New(rand.NewSource(seed))
		s := New("trunk0", Config{Depth: 2})
		names := []string{"a", "b", "c"}
		pick := func() LineID { return LineID(names[r.Intn(len(names))]) }
		old := map[LineID]Head{} // a head each line had before its last push
		trains, charges, late := 0, 0, 0
		for i := 0; i < 500; i++ {
			var ev Event
			tr := TrainID(fmt.Sprintf("t%d", max(1, trains-r.Intn(2))))
			switch r.Intn(12) {
			case 0, 1:
				id := pick()
				ev = Offer{By: "ann", Line: id, Changes: []Change{{ID: ChangeID(id), Head: Head(fmt.Sprintf("%s%d", id, i))}}}
			case 2, 3:
				ev = GateAccept{Line: pick()}
			case 4, 5:
				// a failing verdict, on the present head or on one since replaced
				id := pick()
				l := s.Lines[id]
				if l == nil {
					continue
				}
				h := l.Changes[0].Head
				if o, ok := old[id]; ok && r.Intn(2) == 0 {
					h = o
				}
				ev = LineChecked{Line: id, Heads: []Head{h}, Trunk: s.Trunk, Evidence: "e"}
			case 6, 7:
				id := pick()
				if l := s.Lines[id]; l != nil {
					old[id] = l.Changes[0].Head
				}
				ev = HeadMoved{Line: id, Change: ChangeID(id), Head: Head(fmt.Sprintf("m%d", i))}
			case 8:
				w := s.Waiting()
				if len(w) == 0 {
					continue
				}
				trains++
				ev = Assemble{Train: TrainID(fmt.Sprintf("t%d", trains)), Lines: w[:1+r.Intn(len(w))]}
			case 9:
				ev = Built{Train: tr, Head: Head("h-" + string(tr))}
			case 10:
				ev = Opened{Train: tr, PR: "n", Head: Head("h-" + string(tr))}
			case 11:
				ev = CIFailed{Train: tr, Blamed: []LineID{pick()}, Evidence: "e"}
			}
			before := map[LineID]bool{}
			for id, l := range s.Lines {
				before[id] = l.State == LineReturned && l.Cause == CauseCharged
			}
			next, _, err := Next(s, ev)
			if err != nil {
				continue
			}
			lc, isCheck := ev.(LineChecked)
			for id, l := range next.Lines {
				if l.State != LineReturned || l.Cause != CauseCharged || before[id] {
					continue
				}
				charges++
				if len(l.ChargedAt) != 1 || l.ChargedAt[0] != l.Changes[0].Head {
					t.Fatalf("seed %d, step %d: line %s charged at %v with its head at %s", seed, i, id, l.ChargedAt, l.Changes[0].Head)
				}
				if isCheck && (lc.Line != id || lc.Heads[0] != l.Changes[0].Head) {
					t.Fatalf("seed %d, step %d: a verdict on %s at %s charged line %s at %s", seed, i, lc.Line, lc.Heads[0], id, l.Changes[0].Head)
				}
			}
			if isCheck {
				if l := s.Lines[lc.Line]; l != nil && lc.Heads[0] != l.Changes[0].Head {
					late++
				}
			}
			s = next
		}
		if seed == 1 && (charges == 0 || late == 0) {
			t.Fatalf("the walk proves nothing: %d charges, %d verdicts on a replaced head", charges, late)
		}
	}
}
