package state

import (
	"fmt"
	"math/rand"
	"testing"
)

func accept(id string, inert bool) []Event {
	return []Event{offer(id), GateAccept{Line: LineID(id), Inert: inert}}
}

// §5.4: a train's tier is its heaviest line's. It is light only when every
// line the gate classified is inert; one full line makes it full.
func TestATrainIsLightOnlyWhenEveryLineIsInert(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(accept("a", true), accept("b", true), accept("c", false), assemble("t1", "a", "b"))...)
	if !s.Trains["t1"].Light {
		t.Fatal("a train of inert lines only is not light")
	}
	s = apply(t, New("trunk0", Config{Depth: 2}), cat(accept("a", true), accept("c", false), assemble("t1", "a", "c"))...)
	if s.Trains["t1"].Light {
		t.Fatal("a train with a full line is light")
	}
}

// A push ends a line's inertness: it may bring anything, and the line is
// full until the gate has read its files again.
func TestAPushMakesAnInertLineFull(t *testing.T) {
	s := apply(t, New("trunk0", Config{Depth: 2}), cat(accept("a", true), HeadMoved{Line: "a", Change: "a", Head: "a2"}, assemble("t1", "a"))...)
	if s.Lines["a"].Inert || s.Trains["t1"].Light {
		t.Fatalf("after a push: line inert %v, train light %v", s.Lines["a"].Inert, s.Trains["t1"].Light)
	}
	// offered again, it is classified again: the old answer is not kept
	s = apply(t, New("trunk0", Config{Depth: 2}), cat(accept("a", true), Withdraw{Line: "a"}, accept("a", false))...)
	if s.Lines["a"].Inert {
		t.Fatal("a line offered again kept its old tier")
	}
}

// Whatever the order of offers, gate answers, pushes, assemblies, leave-outs
// and train endings, no live train is lighter than its lines allow (Check
// tests it in every state), and light trains do occur.
func TestRandomWalkOfTiers(t *testing.T) {
	lights := 0
	for seed := int64(1); seed <= 80; seed++ {
		r := rand.New(rand.NewSource(seed))
		s := New("trunk0", Config{Depth: 2})
		names := []string{"a", "b", "c", "d"}
		pick := func() LineID { return LineID(names[r.Intn(len(names))]) }
		trains := 0
		for i := 0; i < 500; i++ {
			var ev Event
			tr := TrainID(fmt.Sprintf("t%d", max(1, trains-r.Intn(2))))
			switch r.Intn(12) {
			case 0, 1:
				id := pick()
				ev = Offer{Line: id, Changes: []Change{{ID: ChangeID(id), Head: Head(fmt.Sprintf("%s%d", id, i))}}}
			case 2, 3, 4:
				ev = GateAccept{Line: pick(), Inert: r.Intn(3) > 0}
			case 5:
				ev = HeadMoved{Line: pick(), Change: ChangeID(pick()), Head: Head(fmt.Sprintf("m%d", i))}
			case 6, 7:
				w := s.Waiting()
				if len(w) == 0 {
					continue
				}
				trains++
				ev = Assemble{Train: TrainID(fmt.Sprintf("t%d", trains)), Lines: w[:1+r.Intn(len(w))]}
			case 8:
				ev = LeaveOut{Train: tr, Line: pick(), Reason: "conflict"}
			case 9:
				ev = Built{Train: tr, Head: Head("h-" + string(tr))}
			case 10:
				ev = CINoVerdict{Train: tr, Why: "w"}
			case 11:
				ev = Withdraw{Line: pick()}
			}
			next, _, err := Next(s, ev)
			if err != nil {
				continue
			}
			if errs := Check(next); len(errs) > 0 {
				t.Fatalf("seed %d, step %d, %s %+v: %v", seed, i, Name(ev), ev, errs)
			}
			if a, ok := ev.(Assemble); ok && next.Trains[a.Train].Light {
				lights++
			}
			s = next
		}
	}
	if lights == 0 {
		t.Fatal("no light train in the walk: it proves nothing")
	}
}
