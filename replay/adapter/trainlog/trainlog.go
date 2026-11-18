// Package trainlog reads the train logs of a script-driven merge queue and
// turns them into events for the replayer.
//
// A train log is the output of one train's builder: free text, with protocol
// lines among it. A protocol line starts with a marker word:
//
//	LEFT-OUT-WHY <change> <reason>   a change left out of the train, and why
//	LEFT-OUT <change>...             the changes left out, in summary
//	BATCH-READY <head> <change>...   assembled; pre-flight passed; the train's head
//	BATCH-BRANCH <name>              the train's branch
//	BATCH-IN <change>...             the changes in the train
//	BATCH-OPENED <pr> <head>         the train's pull request is open
//	BATCH-RERUN                      failed jobs run again, once
//	BATCH-FLAKY ...                  a test passed only on its retry
//	BATCH-GREEN <pr> <head>          CI passed
//	BATCH-BLAME <change>...          a failure charged to these changes
//	BATCH-AGAIN <change>...          the rest, to go again
//	BATCH-UNBLAMED                   the failure that follows names nobody
//	BATCH-FAILED <group>...          failed; a group is change+change for a stack
//	BATCH-PUSHED <change>            a riding change was pushed to
//	BATCH-INFRA                      infrastructure failed; nothing judged
//	BATCH-SPEC-FAILED                the train ahead failed
//	BATCH-CANCELLED                  the train was cancelled
//
// The log says less than the state machine needs. It has no offers, no gate,
// no judge and no landing, so the adapter supplies them. A line is a stack
// where some log groups changes as one (a+b) and this log names them all,
// and otherwise a single change. A line is offered when first seen, and
// withdrawn and offered again when its changes come back grouped differently.
// A failure the script settled
// by itself is replayed as the decision the script took; and whether a green
// train landed comes from a separate record (Source.Landed).
package trainlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/rolandpj1968/shunter/state"
)

// Marker is one protocol line.
type Marker struct {
	Word string
	Args []string
	Rest string // the text after the word
	Line int
}

// Log is one train's log.
type Log struct {
	Name    string
	Markers []Marker
}

var markerRE = regexp.MustCompile(`^(BATCH-[A-Z]+(?:-[A-Z]+)*|LEFT-OUT(?:-WHY)?)(?: (.*))?$`)

// Parse reads a train log and keeps its protocol lines.
func Parse(name string, r io.Reader) (Log, error) {
	lg := Log{Name: name}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		m := markerRE.FindStringSubmatch(strings.TrimRight(sc.Text(), "\r"))
		if m == nil {
			continue
		}
		lg.Markers = append(lg.Markers, Marker{Word: m[1], Args: strings.Fields(m[2]), Rest: m[2], Line: n})
	}
	return lg, sc.Err()
}

// LandedPRs reads a stats file of JSON lines and returns the train pull
// requests that landed: the "batch" of every row whose "stage" is "landed".
func LandedPRs(r io.Reader) (map[string]bool, error) {
	out := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var row struct {
			Stage string `json:"stage"`
			Batch string `json:"batch"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			continue // a damaged row is not a landing
		}
		if row.Stage == "landed" && row.Batch != "" {
			out[row.Batch] = true
		}
	}
	return out, sc.Err()
}

// Source replays logs in the order given, one segment a log.
type Source struct {
	Logs []Log
	// Landed says whether the train with this pull request landed. If nil,
	// every green train is taken to have landed.
	Landed func(pr string) bool

	// Unrecorded names the logs with no protocol line that names a change:
	// whatever became of their lines, the log does not say.
	Unrecorded []string
	// Ignored counts the marker words the adapter does not understand.
	Ignored map[string]int

	next  int
	name  string
	stack map[string][]string // a change to the stack some log puts it in
	here  map[string]bool     // the changes the current log names
	steps []func(state.State) []state.Event
	queue []state.Event
}

const blameRule = "named by a blame rule in the log"

// Next implements replay.Source.
func (src *Source) Next(s state.State) (state.Event, string, bool) {
	for {
		if len(src.queue) > 0 {
			ev := src.queue[0]
			src.queue = src.queue[1:]
			return ev, src.name, true
		}
		if len(src.steps) > 0 {
			step := src.steps[0]
			src.steps = src.steps[1:]
			src.queue = step(s)
			continue
		}
		if src.next >= len(src.Logs) {
			return nil, "", false
		}
		if src.stack == nil {
			src.learnStacks()
		}
		src.next++
		if src.open(src.Logs[src.next-1]) {
			return nil, src.name, true // open the segment; the steps read the state after it
		}
	}
}

// learnStacks reads every log for groups (a+b): changes that any log treats
// as one stack are one line in all of them.
func (src *Source) learnStacks() {
	src.stack = map[string][]string{}
	for _, lg := range src.Logs {
		for _, m := range lg.Markers {
			if m.Word != "BATCH-FAILED" {
				continue
			}
			for _, g := range m.Args {
				if cs := strings.Split(g, "+"); len(cs) > 1 {
					for _, c := range cs {
						src.stack[c] = cs
					}
				}
			}
		}
	}
}

// line is the line a change belongs to in the current log: its stack if the
// log names every change of it, and otherwise the change alone.
func (src *Source) line(change string) (state.LineID, []string) {
	cs := src.stack[change]
	for _, c := range cs {
		if !src.here[c] {
			cs = nil
			break
		}
	}
	if cs != nil {
		return state.LineID(strings.Join(cs, "+")), cs
	}
	return state.LineID(change), []string{change}
}

// lines maps marker arguments (changes, or groups of them) to lines.
func (src *Source) lines(args []string) []state.LineID {
	var out []state.LineID
	seen := map[state.LineID]bool{}
	for _, a := range args {
		for _, c := range strings.Split(a, "+") {
			if c == "" {
				continue
			}
			if id, _ := src.line(c); !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

func mentioned(lg Log) []string {
	var out []string
	for _, m := range lg.Markers {
		switch m.Word {
		case "LEFT-OUT-WHY":
			if len(m.Args) > 0 {
				out = append(out, m.Args[0])
			}
		case "BATCH-READY":
			if len(m.Args) > 1 {
				out = append(out, m.Args[1:]...)
			}
		case "LEFT-OUT", "BATCH-IN", "BATCH-BLAME", "BATCH-AGAIN", "BATCH-FAILED", "BATCH-PUSHED":
			out = append(out, m.Args...)
		}
	}
	return out
}

// open turns one log into steps. A step sees the state as the replay has
// left it, so it can say what the script did in the machine's terms.
func (src *Source) open(lg Log) bool {
	src.here = map[string]bool{}
	for _, a := range mentioned(lg) {
		for _, c := range strings.Split(a, "+") {
			src.here[c] = true
		}
	}
	lines := src.lines(mentioned(lg))
	if len(lines) == 0 {
		src.Unrecorded = append(src.Unrecorded, lg.Name)
		return false
	}
	lineIDs := src.lines
	src.name = lg.Name
	train := state.TrainID(lg.Name)
	live := func(s state.State) *state.Train {
		if t := s.Trains[train]; t != nil && t.State.Live() {
			return t
		}
		return nil
	}
	known := func(s state.State) bool { return s.Trains[train] != nil }
	left := map[state.LineID]bool{}
	unblamed, moved := false, 0

	// Offers, the gate and the assembly, none of which the log records.
	src.steps = append(src.steps, func(s state.State) []state.Event {
		var evs []state.Event
		var ride []state.LineID
		gone := map[state.LineID]bool{}
		for _, id := range lines {
			l := s.Lines[id]
			o := state.Offer{Line: id}
			// A change that waits in a line of another shape: its owner
			// took that line back and offers this one.
			for _, other := range s.Lines {
				if other.ID == id || gone[other.ID] || other.State.Spent() || other.State == state.LineRiding {
					continue
				}
				for _, c := range other.Changes {
					if strings.Contains("+"+string(id)+"+", "+"+string(c.ID)+"+") {
						gone[other.ID] = true
					}
				}
				if gone[other.ID] {
					evs = append(evs, state.Withdraw{Line: other.ID})
				}
			}
			for _, c := range strings.Split(string(id), "+") {
				o.Changes = append(o.Changes, state.Change{ID: state.ChangeID(c), Head: state.Head("head-of-" + c)})
			}
			switch {
			case l == nil || l.State == state.LineReturned || l.State == state.LineDropped:
				evs = append(evs, o, state.GateAccept{Line: id})
				ride = append(ride, id)
			case l.State == state.LineWaiting:
				ride = append(ride, id)
			default:
				evs = append(evs, o) // refused, and the refusal says why
			}
		}
		if len(ride) > 0 {
			evs = append(evs, state.Assemble{Train: train, Lines: ride})
		}
		return evs
	})

	fail := func(s state.State, blamed []state.LineID, evidence string) []state.Event {
		if t := s.Trains[train]; t != nil && t.State == state.TrainAssembling {
			return []state.Event{state.PreflightFailed{Train: train, Blamed: blamed, Evidence: evidence}}
		}
		return []state.Event{state.CIFailed{Train: train, Blamed: blamed, Evidence: evidence}}
	}

	for _, m := range lg.Markers {
		m := m
		var step func(state.State) []state.Event
		switch m.Word {
		case "LEFT-OUT-WHY":
			if len(m.Args) == 0 {
				continue
			}
			id, _ := src.line(m.Args[0])
			why := strings.TrimSpace(strings.TrimPrefix(m.Rest, m.Args[0]))
			step = func(state.State) []state.Event {
				left[id] = true
				return []state.Event{state.LeaveOut{Train: train, Line: id, Reason: why}}
			}
		case "LEFT-OUT":
			step = func(state.State) []state.Event {
				var evs []state.Event
				for _, id := range lineIDs(m.Args) {
					if !left[id] {
						left[id] = true
						evs = append(evs, state.LeaveOut{Train: train, Line: id})
					}
				}
				return evs
			}
		case "BATCH-READY":
			if len(m.Args) == 0 {
				continue
			}
			step = func(s state.State) []state.Event {
				var evs []state.Event
				if t := live(s); t != nil {
					in := map[state.LineID]bool{}
					for _, id := range lineIDs(m.Args[1:]) {
						in[id] = true
					}
					for _, id := range t.Lines {
						if !in[id] { // in the train, then gone, with no word said
							left[id] = true
							evs = append(evs, state.LeaveOut{Train: train, Line: id})
						}
					}
				}
				return append(evs, state.Built{Train: train, Head: state.Head(m.Args[0])})
			}
		case "BATCH-OPENED":
			if len(m.Args) < 2 {
				continue
			}
			step = func(state.State) []state.Event {
				return []state.Event{state.Opened{Train: train, PR: m.Args[0], Head: state.Head(m.Args[1])}}
			}
		case "BATCH-GREEN":
			if len(m.Args) < 2 {
				continue
			}
			src.steps = append(src.steps, func(s state.State) []state.Event {
				if !known(s) {
					return nil
				}
				return []state.Event{state.CIPassed{Train: train, Head: state.Head(m.Args[1])}}
			})
			step = func(s state.State) []state.Event {
				t := live(s)
				if t == nil || t.State != state.TrainGreen || (src.Landed != nil && !src.Landed(m.Args[0])) {
					return nil
				}
				heads := map[state.ChangeID]state.Head{}
				for _, id := range t.Lines {
					for _, c := range s.Lines[id].Changes {
						heads[c.ID] = c.Head
					}
				}
				return []state.Event{
					state.LandingVerified{Train: train, Verdict: t.Head, Heads: heads, Trunk: s.Trunk},
					state.PushOK{Train: train},
				}
			}
		case "BATCH-RERUN":
			step = func(state.State) []state.Event {
				return []state.Event{state.CIRerun{Train: train, Why: "rerun, by the log"}}
			}
		case "BATCH-UNBLAMED":
			step = func(state.State) []state.Event { unblamed = true; return nil }
		case "BATCH-BLAME":
			step = func(s state.State) []state.Event { return fail(s, lineIDs(m.Args), blameRule) }
		case "BATCH-FAILED":
			src.steps = append(src.steps, func(s state.State) []state.Event {
				if !known(s) {
					return nil
				}
				if unblamed {
					return fail(s, nil, "")
				}
				return fail(s, lineIDs(m.Args), blameRule)
			})
			// What the script did next, with no judge: it split a train of
			// several lines, and sent a train of one back to its owner.
			step = func(s state.State) []state.Event {
				t := live(s)
				if t == nil || t.State != state.TrainJudging {
					return nil
				}
				if len(t.Lines) >= 2 {
					return []state.Event{state.Judge{Train: train, Action: state.JudgeSplit, Why: "split by rule, with no judge"}}
				}
				return []state.Event{state.Judge{Train: train, Action: state.JudgeCharge, Lines: t.Lines}}
			}
		case "BATCH-PUSHED":
			if len(m.Args) == 0 {
				continue
			}
			step = func(state.State) []state.Event {
				moved++
				id, _ := src.line(m.Args[0])
				return []state.Event{state.HeadMoved{Line: id, Change: state.ChangeID(m.Args[0]), Head: state.Head(fmt.Sprintf("head-of-%s-moved-%d", m.Args[0], moved))}}
			}
		case "BATCH-INFRA", "BATCH-SPEC-FAILED":
			why := "infrastructure failed"
			if m.Word == "BATCH-SPEC-FAILED" {
				why = "the train ahead failed"
			}
			step = func(s state.State) []state.Event {
				if live(s) == nil { // already void, by the push reported just before
					return nil
				}
				return []state.Event{state.CINoVerdict{Train: train, Infra: m.Word == "BATCH-INFRA", Why: why}}
			}
		case "BATCH-CANCELLED":
			step = func(state.State) []state.Event {
				return []state.Event{state.Abort{Train: train, Reason: "cancelled, by the log"}}
			}
		case "BATCH-BRANCH", "BATCH-IN", "BATCH-AGAIN", "BATCH-FLAKY":
			continue
		default:
			if src.Ignored == nil {
				src.Ignored = map[string]int{}
			}
			src.Ignored[m.Word]++
			continue
		}
		inner := step
		src.steps = append(src.steps, func(s state.State) []state.Event {
			if !known(s) { // the assembly was refused; one finding, not twenty
				return nil
			}
			return inner(s)
		})
	}
	return true
}
