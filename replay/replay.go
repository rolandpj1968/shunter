// Package replay runs a recorded history through the state machine and
// reports what the machine refuses. It is how an old queue's logs, and later
// Shunter's own transition log, are checked against PROTOCOL.md §5.1.
package replay

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/rolandpj1968/shunter/state"
)

// Source yields a history one event at a time. It is shown the current state
// before each event, so an adapter can translate a log that says less than
// the machine needs. A segment is one unit of the log (one train's log, say);
// when a segment ends, the replayer settles what it left open. A nil event
// with a new segment name only opens the segment: a source that reads the
// state sends one first, so that it sees the state after the settling.
type Source interface {
	Next(s state.State) (ev state.Event, segment string, ok bool)
}

// Finding is an event the machine refused, or an invariant a step broke.
type Finding struct {
	Segment string
	Event   string
	Err     error
}

// Unsettled is a train its segment left live: the log ended with no outcome
// for it. The replayer aborts it so the lines can ride again.
type Unsettled struct {
	Segment string
	Train   state.TrainID
	State   state.TrainState
}

// Report is the result of a replay.
type Report struct {
	Segments  int
	Events    int
	Applied   int
	Refused   []Finding   // histories the machine would not have allowed
	Breaches  []Finding   // invariants broken by an accepted step: a bug in the machine
	Unsettled []Unsettled // trains with no recorded outcome
	Pauses    int         // times the history ran into J3 and was resumed
}

// Run replays src from s and returns the final state and the report.
func Run(s state.State, src Source) (state.State, Report) {
	var rep Report
	segment, started := "", false
	var opened []state.TrainID
	settle := func() {
		for _, id := range opened {
			t := s.Trains[id]
			if t == nil || !t.State.Live() {
				continue
			}
			rep.Unsettled = append(rep.Unsettled, Unsettled{segment, id, t.State})
			var ev state.Event = state.Abort{Train: id, Reason: "replay: the log ends with no outcome for this train"}
			if t.State == state.TrainLanding {
				ev = state.PushRefused{Train: id, Permanent: true, Why: "replay: the log ends with the push in flight"}
			}
			if next, _, err := state.Next(s, ev); err == nil {
				s = next
			}
		}
		opened = nil
		if s.Paused {
			rep.Pauses++
			if next, _, err := state.Next(s, state.Resume{}); err == nil {
				s = next
			}
		}
	}
	for {
		ev, seg, ok := src.Next(s)
		if !ok {
			break
		}
		if !started || seg != segment {
			if started {
				settle()
			}
			segment, started = seg, true
			rep.Segments++
		}
		if ev == nil {
			continue
		}
		rep.Events++
		next, fx, err := state.Next(s, ev)
		if err != nil {
			rep.Refused = append(rep.Refused, Finding{segment, state.Name(ev), err})
			continue
		}
		for _, e := range append(state.Check(next), state.CheckStep(s, next, fx)...) {
			rep.Breaches = append(rep.Breaches, Finding{segment, state.Name(ev), e})
		}
		if a, ok := ev.(state.Assemble); ok {
			opened = append(opened, a.Train)
		}
		rep.Applied++
		s = next
	}
	if started {
		settle()
	}
	return s, rep
}

// Slice is a Source over events already in hand, as one segment.
type Slice struct {
	Name   string
	Events []state.Event
	i      int
}

func (s *Slice) Next(state.State) (state.Event, string, bool) {
	if s.i >= len(s.Events) {
		return nil, "", false
	}
	s.i++
	return s.Events[s.i-1], s.Name, true
}

// kinds are the machine's refusals, with the words a report uses for them.
var kinds = []struct {
	err  error
	what string
}{
	{state.ErrEvidence, "a change charged with no evidence (I6)"},
	{state.ErrReason, "a line left out, a train voided or an act made with no reason given"},
	{state.ErrHead, "a verdict or a pull request on a head other than the one built (I1, I9)"},
	{state.ErrHeld, "an act on a held line"},
	{state.ErrLanding, "an act on a train past the point of no return"},
	{state.ErrChain, "a break in the speculation chain (I5)"},
	{state.ErrDuplicate, "a change or a line queued twice"},
	{state.ErrJoin, "lines riding together that may not"},
	{state.ErrBudget, "a retry beyond its budget"},
	{state.ErrPaused, "a train started while the queue was paused"},
	{state.ErrUnknown, "an act on a line or train the record does not have"},
	{state.ErrState, "a transition the table does not have"},
	{state.ErrInput, "a malformed event"},
	{state.ErrTable, "a transition off the table (I8)"},
}

// Kind names the class of a refusal.
func Kind(err error) string {
	for _, k := range kinds {
		if errors.Is(err, k.err) {
			return k.what
		}
	}
	return "other"
}

// Write prints the report. With detail, every finding is listed.
func (r Report) Write(w io.Writer, detail bool) {
	fmt.Fprintf(w, "segments %d, events %d, applied %d, refused %d, unsettled trains %d, pauses at J3 %d, invariant breaches %d\n",
		r.Segments, r.Events, r.Applied, len(r.Refused), len(r.Unsettled), r.Pauses, len(r.Breaches))
	count := map[string]int{}
	for _, f := range r.Refused {
		count[Kind(f.Err)+" ["+f.Event+"]"]++
	}
	printCounts(w, "refused", count)
	count = map[string]int{}
	for _, u := range r.Unsettled {
		count["log ends with its train "+u.State.String()]++
	}
	printCounts(w, "unsettled", count)
	if !detail {
		return
	}
	for _, f := range r.Refused {
		fmt.Fprintf(w, "  refused  %s: %v\n", f.Segment, f.Err)
	}
	for _, u := range r.Unsettled {
		fmt.Fprintf(w, "  unsettled %s: train %s\n", u.Segment, u.State)
	}
	for _, f := range r.Breaches {
		fmt.Fprintf(w, "  BREACH   %s: %s: %v\n", f.Segment, f.Event, f.Err)
	}
}

func printCounts(w io.Writer, label string, count map[string]int) {
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if count[keys[i]] != count[keys[j]] {
			return count[keys[i]] > count[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		fmt.Fprintf(w, "  %s %4d  %s\n", label, count[k], k)
	}
}
