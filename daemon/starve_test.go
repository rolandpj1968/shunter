package daemon_test

import (
	"slices"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/tier"
)

// A line starves by its queue time as the backstop (docs/queue-as-bag.md
// §4). Queue time doesn't run while the queue is paused. Starving lines at
// StarveAlert at once are raised to the judge.
func TestALineStarvesByQueueTimeAndTheJudgeIsTold(t *testing.T) {
	r := newRigWith(t, 1, tier.Rules{}, func(r *rig) {
		r.config = func(c *daemon.Config) { c.StarveAfter, c.StarveAlert = 12*time.Hour, 1 }
	})
	r.open("a", "b", "c")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather the queue"})
	for _, id := range []string{"a", "b", "c"} {
		r.queue(id)
	}
	r.tick(13 * time.Hour)
	if r.state().Lines["c"].Starving {
		t.Fatal("queue time ran while the queue was paused")
	}
	r.exec(daemon.Request{Cmd: "resume"})
	r.wantLine("c", state.LineWaiting)
	r.tick(12 * time.Hour)
	s := r.state()
	if !s.Lines["c"].Starving || s.Forced() != "c" {
		t.Fatalf("c is not forced: %+v", s.Lines["c"])
	}
	if !r.alerted("1 lines are starving at once") {
		t.Fatal("the judge was not told")
	}
	// It leads the next train, behind the trains that ride.
	if got, want := daemon.Join(s, daemon.Caps{Lines: 1}), []state.LineID{"c"}; !slices.Equal(got, want) {
		t.Fatalf("join took %v, want %v", got, want)
	}
}

func apply(t *testing.T, s state.State, evs ...state.Event) state.State {
	t.Helper()
	for _, ev := range evs {
		next, _, err := state.Next(s, ev)
		if err != nil {
			t.Fatalf("%s: %v", state.Name(ev), err)
		}
		s = next
	}
	return s
}

// While a line is forced, join takes it first, with what beneath it still
// waits, past the caps, and fills the train behind it in the policy's
// order, up to the caps; a prioritised line still goes first.
func TestJoinLeadsWithTheForcedLine(t *testing.T) {
	s := apply(t, waiting(t, 1, 1, 1),
		state.Offer{Line: "l4", Base: "l3", Changes: []state.Change{{ID: "c4", Head: "h"}}},
		state.GateAccept{Line: "l4"},
		state.Starving{Line: "l4", Why: "3 cycles"})
	if got, want := daemon.Join(s, daemon.Caps{Lines: 1}), []state.LineID{"l3", "l4"}; !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
	if got, want := daemon.Join(s, daemon.Caps{Lines: 3}), []state.LineID{"l3", "l4", "l1"}; !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
	next := apply(t, s, state.Prioritise{Line: "l2", By: "judge", Reason: "fixes trunk", Until: time.Now().Add(time.Hour)})
	if got, want := daemon.Join(next, daemon.Caps{Lines: 8}), []state.LineID{"l2"}; !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

// Two starving lines lead the same train, oldest first, and the train fills
// behind them up to the cap.
func TestJoinTakesEveryStarvingLineFirst(t *testing.T) {
	s := apply(t, waiting(t, 1, 1, 1, 1, 1),
		state.Starving{Line: "l4", Why: "3 cycles"}, state.Starving{Line: "l2", Why: "12h"})
	if got, want := daemon.Join(s, daemon.Caps{Lines: 3}), []state.LineID{"l2", "l4", "l1"}; !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

// A starving line marked alone rides alone; one that alone reaches the
// change cap does too. Neither is a reason of starving's.
func TestJoinKeepsTheOtherReasonsForAlone(t *testing.T) {
	marked := apply(t, waiting(t, 1, -1, 1), state.Starving{Line: "l2", Why: "3 cycles"})
	if got, want := daemon.Join(marked, daemon.Caps{Lines: 8}), []state.LineID{"l2"}; !slices.Equal(got, want) {
		t.Fatalf("marked alone: %v, want %v", got, want)
	}
	big := apply(t, waiting(t, 1, 4, 1), state.Starving{Line: "l2", Why: "3 cycles"})
	if got, want := daemon.Join(big, daemon.Caps{Lines: 8, Changes: 4}), []state.LineID{"l2"}; !slices.Equal(got, want) {
		t.Fatalf("at the change cap: %v, want %v", got, want)
	}
}

// Sometimes alone (owner's ruling of 26 Nov): a starving line with no
// failure blame could not clear it from leads a full train; with
// StarveAlone of them since its last offer, it rides alone, and, not
// forced, it waits for its turn rather than ride in company.
func TestJoinRidesAStarvingLineAloneWhenCompanyIsTheDoubt(t *testing.T) {
	caps := daemon.Caps{Lines: 8, StarveAlone: 1}
	s := apply(t, waiting(t, 1, 1, 1, 1), state.Starving{Line: "l2", Why: "3 cycles"})
	if got, want := daemon.Join(s, caps), []state.LineID{"l2", "l1", "l3", "l4"}; !slices.Equal(got, want) {
		t.Fatalf("no doubt: %v, want %v", got, want)
	}
	// Its train fails, and blame names nobody: J1, and the judge aborts.
	s = apply(t, s, state.Assemble{Train: "t1", Lines: []state.LineID{"l2", "l3"}},
		state.Built{Train: "t1", Head: "h1"}, state.Opened{Train: "t1", PR: "p1", Head: "h1"},
		state.CIFailed{Train: "t1", Evidence: "something fails"}, state.Abort{Train: "t1", Reason: "the judge"})
	if n := s.Lines["l2"].Unclear; n != 1 {
		t.Fatalf("unclear %d", n)
	}
	if got, want := daemon.Join(s, caps), []state.LineID{"l2"}; !slices.Equal(got, want) {
		t.Fatalf("after an unattributed failure: %v, want %v", got, want)
	}
	if got, want := daemon.Join(s, daemon.Caps{Lines: 8}), []state.LineID{"l2", "l1", "l3", "l4"}; !slices.Equal(got, want) {
		t.Fatalf("with the rule off: %v, want %v", got, want)
	}
	// Starving but not forced, it is skipped meanwhile; l3, in the same
	// failure but not starving, rides in company as any line does.
	s = apply(t, s, state.Starving{Line: "l1", Why: "12h"})
	if got, want := daemon.Join(s, caps), []state.LineID{"l1", "l3", "l4"}; !slices.Equal(got, want) {
		t.Fatalf("l1 forced: %v, want %v", got, want)
	}
}

// A starving line the runner can't rebase is raised at once: it needs a
// person.
func TestAStarvingLineTheRunnerCannotRebaseIsRaised(t *testing.T) {
	r := newRigWith(t, 1, tier.Rules{}, func(r *rig) {
		r.config = func(c *daemon.Config) { c.StarveCycles = 1 }
	})
	r.open("a")
	r.exec(daemon.Request{Cmd: "pause", Reason: "gather the queue"})
	r.queue("a")
	r.exec(daemon.Request{Cmd: "checked", Args: []string{"a", "T0", "a-1"}, Action: "conflict", Reason: "x.go"})
	r.tick(time.Minute)
	r.tick(time.Minute)
	if !r.alerted("starving and stuck") {
		t.Fatal("the judge was not told")
	}
}

// A starving line that can't ride now never blocks other trains (issue
// #58): stale, in conflict with trunk, or with no current green line check,
// it is skipped, and the ready lines assemble as normal. Once it is ready,
// it rides first again. Each train is also one the state machine accepts.
func TestAnUnreadyStarvingLineBlocksNoTrain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checked bool // l1 starts with a green line check
		unready []state.Event
		ready   []state.Event
	}{
		{"stale", true, []state.Event{state.Stale{Line: "l1", Reason: "its head and base disagree"}},
			[]state.Event{state.Fresh{Line: "l1"}}},
		{"in conflict with trunk", false, []state.Event{state.LineChecked{Line: "l1", Heads: []state.Head{"h"}, Trunk: "T0", Conflict: "x.go"}},
			[]state.Event{state.HeadMoved{Line: "l1", Change: "c-l1", Head: "h2"}, state.LineChecked{Line: "l1", Heads: []state.Head{"h2"}, Trunk: "T0", Pass: true}}},
		{"no line check", false, nil,
			[]state.Event{state.LineChecked{Line: "l1", Heads: []state.Head{"h"}, Trunk: "T0", Pass: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := state.New("T0", state.Config{Depth: 2, RequireCheck: true})
			for _, id := range []state.LineID{"l1", "l2", "l3", "l4"} {
				s = apply(t, s, state.Offer{Line: id, Changes: []state.Change{{ID: state.ChangeID("c-" + string(id)), Head: "h"}}}, state.GateAccept{Line: id})
				if id != "l1" || tc.checked {
					s = apply(t, s, state.LineChecked{Line: id, Heads: []state.Head{"h"}, Trunk: "T0", Pass: true})
				}
			}
			s = apply(t, s, tc.unready...)
			s = apply(t, s, state.Starving{Line: "l1", Why: "3 cycles"})
			caps := daemon.Caps{Lines: 8, StarveAlone: 1}
			if f := s.Forced(); f != "" {
				t.Fatalf("an unready line is forced: %q", f)
			}
			got := daemon.Join(s, caps)
			if want := []state.LineID{"l2", "l3", "l4"}; !slices.Equal(got, want) {
				t.Fatalf("join took %v, want %v", got, want)
			}
			apply(t, s, state.Assemble{Train: "t1", Lines: got})
			// Ready, it is forced again and leads the next train.
			s = apply(t, s, tc.ready...)
			if f := s.Forced(); f != "l1" {
				t.Fatalf("ready, forced %q", f)
			}
			got = daemon.Join(s, caps)
			if want := []state.LineID{"l1", "l2", "l3", "l4"}; !slices.Equal(got, want) {
				t.Fatalf("ready, join took %v, want %v", got, want)
			}
			apply(t, s, state.Assemble{Train: "t1", Lines: got})
		})
	}
}

// Of two starving lines, an older one that can't ride does not hold back
// a younger one that can: the younger is forced and leads.
func TestAnUnreadyStarvingLineDoesNotHoldBackAReadyOne(t *testing.T) {
	s := apply(t, waiting(t, 1, 1, 1, 1),
		state.Stale{Line: "l1", Reason: "its head and base disagree"},
		state.Starving{Line: "l1", Why: "12h"}, state.Starving{Line: "l3", Why: "3 cycles"})
	if f := s.Forced(); f != "l3" {
		t.Fatalf("forced %q, want l3", f)
	}
	got := daemon.Join(s, daemon.Caps{Lines: 8})
	if want := []state.LineID{"l3", "l2", "l4"}; !slices.Equal(got, want) {
		t.Fatalf("join took %v, want %v", got, want)
	}
	apply(t, s, state.Assemble{Train: "t1", Lines: got})
}
