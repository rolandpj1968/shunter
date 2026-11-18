package store

import (
	"errors"
	"fmt"
	"sort"

	"github.com/rolandpj1968/shunter/state"
)

// ExportFormat names the document Export writes and Import reads.
const ExportFormat = "shunter-export/1"

// Document is the queue at rest, for a cut-over in either direction
// (docs/cutover.md): the lines that are live, in queue order, with their
// holds and parks. Trains are not carried: what is in motion is drained, and
// a riding line is exported as waiting.
type Document struct {
	Format string       `json:"format"`
	Trunk  state.Head   `json:"trunk"`
	Lines  []ExportLine `json:"lines"`
	Parks  []ExportPark `json:"parks,omitempty"`
}

// ExportLine is one live line.
type ExportLine struct {
	ID      state.LineID   `json:"id"`
	Changes []state.Change `json:"changes"`
	Base    state.LineID   `json:"base,omitempty"`
	Alone   bool           `json:"alone,omitempty"`
	Fix     state.ParkID   `json:"fix,omitempty"`
	// State is waiting, waiting-for-base, held or parked. An offered or a
	// riding line is exported as waiting.
	State  string       `json:"state"`
	Reason string       `json:"reason,omitempty"` // held: the hold's reason
	Park   state.ParkID `json:"park,omitempty"`   // parked, or held under a park
}

// ExportPark is a park in force.
type ExportPark struct {
	ID     state.ParkID `json:"id"`
	All    bool         `json:"all,omitempty"`
	Cond   string       `json:"cond"`
	Reason string       `json:"reason,omitempty"`
}

// Export writes the queue at rest.
func Export(s state.State) Document {
	d := Document{Format: ExportFormat, Trunk: s.Trunk, Lines: []ExportLine{}}
	ids := make([]state.LineID, 0, len(s.Lines))
	for id, l := range s.Lines {
		if !l.State.Spent() && l.State != state.LineNone {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return s.Lines[ids[i]].Seq < s.Lines[ids[j]].Seq })
	used := map[state.ParkID]bool{}
	for _, id := range ids {
		l := s.Lines[id]
		e := ExportLine{ID: l.ID, Changes: append([]state.Change(nil), l.Changes...), Base: l.Base, Alone: l.Alone, Fix: l.Fix}
		switch l.State {
		case state.LineHeld:
			e.State, e.Reason, e.Park = l.State.String(), l.Reason, l.Park
		case state.LineParked:
			e.State, e.Park = l.State.String(), l.Park
		case state.LineWaitingBase:
			e.State = l.State.String()
		default:
			e.State = state.LineWaiting.String()
		}
		if e.Park != "" {
			used[e.Park] = true
		}
		d.Lines = append(d.Lines, e)
	}
	pids := make([]state.ParkID, 0, len(s.Parks))
	for id := range s.Parks {
		pids = append(pids, id)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	for _, id := range pids {
		p := s.Parks[id]
		if p.All || used[id] {
			d.Parks = append(d.Parks, ExportPark{ID: p.ID, All: p.All, Cond: p.Cond, Reason: p.Reason})
		}
	}
	return d
}

// Plan is how a document enters a queue: every line is offered again, in its
// order, and passes the gate again (nothing enters on the exporter's say-so).
// Then, for the lines the gate accepted, the parks and holds are placed.
type Plan struct {
	Offers []state.Offer
	// After returns the parks and holds to place once the gate has
	// answered, given which lines it accepted.
	After func(accepted map[state.LineID]bool) []state.Event
}

// Import checks a document and returns its plan. A hold with no reason and a
// park with no restore condition are refused: they are asked about, not
// carried (docs/cutover.md).
func Import(d Document) (Plan, error) {
	if d.Format != ExportFormat {
		return Plan{}, fmt.Errorf("import: format %q, want %q", d.Format, ExportFormat)
	}
	parks := map[state.ParkID]ExportPark{}
	all := 0
	for _, p := range d.Parks {
		if p.ID == "" || p.Cond == "" {
			return Plan{}, fmt.Errorf("import: park %q has no restore condition", p.ID)
		}
		if p.All {
			all++
		}
		parks[p.ID] = p
	}
	if all > 1 {
		return Plan{}, errors.New("import: more than one park of the whole queue")
	}
	var plan Plan
	seen := map[state.LineID]bool{}
	for _, l := range d.Lines {
		if seen[l.ID] {
			return Plan{}, fmt.Errorf("import: line %s appears twice", l.ID)
		}
		seen[l.ID] = true
		switch l.State {
		case "waiting", "waiting-for-base":
		case "held":
			if l.Reason == "" {
				return Plan{}, fmt.Errorf("import: line %s is held with no reason", l.ID)
			}
		case "parked":
			if _, ok := parks[l.Park]; !ok {
				return Plan{}, fmt.Errorf("import: line %s is parked under %q, which has no restore condition here", l.ID, l.Park)
			}
		default:
			return Plan{}, fmt.Errorf("import: line %s is %q", l.ID, l.State)
		}
		plan.Offers = append(plan.Offers, state.Offer{Line: l.ID, Changes: l.Changes, Base: l.Base, Alone: l.Alone, Fix: l.Fix})
	}
	plan.After = func(accepted map[state.LineID]bool) []state.Event {
		var evs []state.Event
		named := map[state.ParkID][]state.LineID{}
		for _, l := range d.Lines {
			if accepted[l.ID] && l.Park != "" && !parks[l.Park].All {
				named[l.Park] = append(named[l.Park], l.ID)
			}
		}
		for _, p := range d.Parks {
			switch {
			case p.All:
				evs = append(evs, state.ParkLines{Park: p.ID, Cond: p.Cond, Reason: p.Reason})
			case len(named[p.ID]) > 0:
				evs = append(evs, state.ParkLines{Park: p.ID, Lines: named[p.ID], Cond: p.Cond, Reason: p.Reason})
			}
		}
		for _, l := range d.Lines {
			if accepted[l.ID] && l.State == "held" {
				evs = append(evs, state.Hold{Line: l.ID, Reason: l.Reason})
			}
		}
		return evs
	}
	return plan, nil
}
