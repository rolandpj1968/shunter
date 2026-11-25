package daemon

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
)

// A need is read twice (PROTOCOL.md §5.2): from the forge's declared base,
// and from ancestry. Where the two disagree the line is stale: it waits for
// a rebase, joins no train, and its owner is told why.

// restackEvery is how often the runner reads the needs of the lines that
// wait behind a base, or are stale, again: a rebase clears a stale line,
// and a push to a base makes the line behind it stale.
const restackEvery = time.Minute

// needs is what a staleness check reads of the queue, taken in the loop so
// that the check can run outside it.
type needs struct {
	base      state.LineID                  // the line this one needs, if any
	baseHeads map[state.ChangeID]state.Head // the base line's changes at their heads
	live      []liveHead                    // every other live line's changes
}

type liveHead struct {
	change state.ChangeID
	line   state.LineID
	head   state.Head
}

func needsOf(s state.State, l *state.Line) needs {
	n := needs{base: l.Base, baseHeads: map[state.ChangeID]state.Head{}}
	if b := s.Lines[l.Base]; b != nil {
		for _, c := range b.Changes {
			n.baseHeads[c.ID] = c.Head
		}
	}
	for id, o := range s.Lines {
		if id == l.ID || o.State.Spent() || o.State == state.LineNone {
			continue
		}
		for _, c := range o.Changes {
			n.live = append(n.live, liveHead{change: c.ID, line: id, head: c.Head})
		}
	}
	sort.Slice(n.live, func(i, j int) bool { return n.live[i].change < n.live[j].change })
	return n
}

// staleness says why the line is stale, or "" when its declared base and its
// ancestry agree. heads are the line's changes now; declared is its first
// change's base on the forge. An error is a reading that failed: nothing is
// decided on it.
func staleness(ctx context.Context, anc adapter.Ancestry, n needs, changes []state.Change, declared state.ChangeID) (string, error) {
	if len(changes) == 0 {
		return "", nil
	}
	head := changes[0].Head
	ids := []state.ChangeID{changes[0].ID}
	if n.base != "" {
		bh, ok := n.baseHeads[declared]
		if !ok {
			return fmt.Sprintf("it targets %s on the forge, not a change of line %s, the line it was offered behind: offer it again", orTrunk(declared), n.base), nil
		}
		has, err := anc.Contains(ctx, head, bh, append(ids, declared))
		if err != nil {
			return "", err
		}
		if !has {
			return fmt.Sprintf("its head %s doesn't contain its base %s at %s: rebase it onto that head", short(head), declared, short(bh)), nil
		}
		return "", nil
	}
	for _, o := range n.live {
		if o.change == declared {
			return fmt.Sprintf("it targets %s, which is live in line %s, but it was offered on trunk: offer it again", declared, o.line), nil
		}
		has, err := anc.Contains(ctx, head, o.head, append(ids, o.change))
		if err != nil {
			return "", err
		}
		if has {
			return fmt.Sprintf("it carries change %s's commits (line %s) but targets %s: target %s, or rebase it without them", o.change, o.line, orTrunk(declared), o.change), nil
		}
	}
	return "", nil
}

func orTrunk(c state.ChangeID) string {
	if c == "" {
		return "trunk"
	}
	return string(c)
}

func short(h state.Head) string {
	if len(h) > 10 {
		return string(h[:10])
	}
	return string(h)
}

// staleEvent is the event that brings a line's recorded staleness in step
// with a reading: stale with the reason, fresh, or nothing.
func staleEvent(l state.LineID, was, why string) state.Event {
	switch {
	case why != "" && why != was:
		return state.Stale{Line: l, Reason: why}
	case why == "" && was != "":
		return state.Fresh{Line: l}
	}
	return nil
}

// restackCheck is one line to read again: its record and what it needs.
type restackCheck struct {
	line  state.Line
	needs needs
	base  *state.Line // its base line, when the base isn't riding: read again too
	// heads says only the line's heads are read, not its needs: a held
	// line, so that a push while it is held re-keys it while it is held,
	// and it rides at its new head once released (PROTOCOL.md row L39).
	heads bool
}

// restack reads again, outside the loop, the lines that wait behind a base
// or are stale, and their bases: a head that moved is a head-moved event,
// and a line whose staleness changed is a stale or fresh event. It reads
// the heads of every held line too.
func (d *Daemon) restack(now time.Time, s state.State) {
	anc, ok := d.ad.Git.(adapter.Ancestry)
	if !ok || d.ad.Forge == nil || d.restacking || now.Sub(d.restackedAt) < restackEvery {
		return
	}
	var checks []restackCheck
	for _, id := range s.Waiting() {
		l := s.Lines[id]
		if l.Base == "" && l.Stale == "" {
			continue
		}
		c := restackCheck{line: copyLine(l), needs: needsOf(s, l)}
		if b := s.Lines[l.Base]; b != nil && (b.State == state.LineWaiting || b.State == state.LineHeld || b.State == state.LineParked) {
			bc := copyLine(b)
			c.base = &bc
		}
		checks = append(checks, c)
	}
	for _, id := range s.Held() {
		checks = append(checks, restackCheck{line: copyLine(s.Lines[id]), heads: true})
	}
	d.restackedAt = now
	if len(checks) == 0 {
		return
	}
	d.restacking = true
	forge := d.ad.Forge
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Call)
		defer cancel()
		events, err := readNeeds(ctx, forge, anc, checks)
		select {
		case d.results <- result{restack: true, actor: "runner", events: events, err: err}:
		case <-d.quit:
		}
	}()
}

func copyLine(l *state.Line) state.Line {
	c := *l
	c.Changes = append([]state.Change(nil), l.Changes...)
	return c
}

// readNeeds reads each line's and each base's changes from the forge, and
// gives the events that bring the record in step: head-moved first, then
// stale or fresh.
func readNeeds(ctx context.Context, f adapter.Forge, anc adapter.Ancestry, checks []restackCheck) ([]state.Event, error) {
	var out []state.Event
	moved := map[state.ChangeID]state.Head{}
	read := func(l state.Line) ([]adapter.ChangeInfo, error) {
		ids := make([]state.ChangeID, len(l.Changes))
		for i, c := range l.Changes {
			ids[i] = c.ID
		}
		info, err := f.Describe(ctx, ids)
		if err != nil {
			return nil, err
		}
		if len(info) != len(ids) {
			return nil, fmt.Errorf("the forge described %d of %d changes", len(info), len(ids))
		}
		for i, c := range info {
			if c.Head != "" && c.Head != l.Changes[i].Head && moved[c.ID] == "" {
				moved[c.ID] = c.Head
				out = append(out, state.HeadMoved{Line: l.ID, Change: c.ID, Head: c.Head})
			}
		}
		return info, nil
	}
	for _, c := range checks {
		if c.base != nil {
			if _, err := read(*c.base); err != nil {
				return out, err
			}
		}
	}
	for _, c := range checks {
		info, err := read(c.line)
		if err != nil {
			return out, err
		}
		if c.heads {
			continue
		}
		changes := withHeads(c.line.Changes, moved)
		n := c.needs
		for id := range n.baseHeads {
			if m := moved[id]; m != "" {
				n.baseHeads[id] = m
			}
		}
		for i, o := range n.live {
			if m := moved[o.change]; m != "" {
				n.live[i].head = m
			}
		}
		why, err := staleness(ctx, anc, n, changes, info[0].Base)
		if err != nil {
			continue // a reading that failed decides nothing
		}
		if ev := staleEvent(c.line.ID, c.line.Stale, why); ev != nil {
			out = append(out, ev)
		}
	}
	return out, nil
}

func withHeads(cs []state.Change, moved map[state.ChangeID]state.Head) []state.Change {
	out := append([]state.Change(nil), cs...)
	for i, c := range out {
		if m := moved[c.ID]; m != "" {
			out[i].Head = m
		}
	}
	return out
}

// restackDone applies what a re-read found. Each event is tested by the
// machine against the state now: one about a line that has since joined a
// train is refused, and the next read sees it again.
func (d *Daemon) restackDone(r result) {
	d.restacking = false
	if r.err != nil {
		d.log.Printf("reading the needs of stacked lines: %v", r.err)
	}
	for _, ev := range r.events {
		if d.apply(r.actor, ev) != nil && d.fatal != nil {
			return
		}
	}
}
