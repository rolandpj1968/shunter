package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
)

// reofferEvery is how often the re-offerer looks at the lines sent back
// uncharged.
const reofferEvery = 5 * time.Minute

// uncharged reports whether a returned line went back for no fault of its
// own, and so is the runner's to offer again once it is clean: a conflict
// with trunk only a person could resolve (L36), or its base lost. A charged
// line comes back only by its owner's word (P3, I11), a refused one by a new
// offer, and one the judge returned by the judge's.
func uncharged(l *state.Line) bool {
	return l.State == state.LineReturned && (l.Cause == state.CauseConflict || l.Cause == state.CauseBaseLost)
}

// reofferCheck is one line to look at, outside the loop.
type reofferCheck struct {
	line state.Line
	live map[state.ChangeID]state.LineID
	// tried is the key of the heads and trunk last found in conflict.
	tried string
}

// reoffered is what a look found: the lines that merge now, as the forge
// describes them, and the keys found in conflict.
type reoffered struct {
	clean map[state.LineID][]adapter.ChangeInfo
	tried map[state.LineID]string
}

// reoffer offers again, at its first offer's age, each line sent back
// uncharged once its changes are open, its base is live or landed, and it
// merges onto trunk with no person (owner's ruling of 24 Nov,
// docs/queue-as-bag.md §4; docs/cutover-readiness.md B7). The gate then runs
// on it as on any offer.
func (d *Daemon) reoffer(now time.Time, s state.State) {
	m, ok := d.ad.Git.(adapter.Merger)
	if !ok || d.ad.Forge == nil || d.reoffering || now.Sub(d.reofferedAt) < reofferEvery {
		return
	}
	var checks []reofferCheck
	for _, l := range s.Lines {
		if uncharged(l) {
			checks = append(checks, reofferCheck{line: copyLine(l), live: liveChanges(s, l.ID), tried: d.mergeTried[l.ID]})
		}
	}
	d.reofferedAt = now
	if len(checks) == 0 {
		return
	}
	d.reoffering = true
	forge, trunk := d.ad.Forge, s.Trunk
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Call)
		defer cancel()
		found := lookAgain(ctx, forge, m, trunk, checks)
		select {
		case d.results <- result{reoffer: &found, actor: "re-offerer"}:
		case <-d.quit:
		}
	}()
}

// lookAgain reads each line from the forge and tests its merge. A reading
// that fails decides nothing: the line is looked at again next time.
func lookAgain(ctx context.Context, f adapter.Forge, m adapter.Merger, trunk state.Head, checks []reofferCheck) reoffered {
	out := reoffered{clean: map[state.LineID][]adapter.ChangeInfo{}, tried: map[state.LineID]string{}}
	for _, c := range checks {
		ids := make([]state.ChangeID, len(c.line.Changes))
		for i, ch := range c.line.Changes {
			ids[i] = ch.ID
		}
		info, err := f.Describe(ctx, ids)
		if err != nil || len(info) != len(ids) || !allOpen(info) {
			continue
		}
		if b := info[0].Base; b != "" && c.live[b] == "" {
			base, err := f.Describe(ctx, []state.ChangeID{b})
			if err != nil || len(base) != 1 || !base[0].Landed {
				continue
			}
		}
		line := adapter.BuildLine{ID: c.line.ID}
		key := []string{string(trunk)}
		for _, ch := range info {
			line.Changes = append(line.Changes, state.Change{ID: ch.ID, Head: ch.Head})
			key = append(key, string(ch.Head))
		}
		k := strings.Join(key, " ")
		if k == c.tried {
			continue
		}
		conflict, err := m.Merges(ctx, line, trunk)
		switch {
		case err != nil:
		case conflict != "":
			out.tried[c.line.ID] = k
		default:
			out.clean[c.line.ID] = info
		}
	}
	return out
}

func allOpen(info []adapter.ChangeInfo) bool {
	for _, c := range info {
		if !c.Open {
			return false
		}
	}
	return true
}

// reofferDone offers again each line found clean that is still sent back
// uncharged, as its owner, so that it stays with them.
func (d *Daemon) reofferDone(r result) {
	d.reoffering = false
	for id, k := range r.reoffer.tried {
		d.mergeTried[id] = k
	}
	for id, info := range r.reoffer.clean {
		s := d.st.State()
		l := s.Lines[id]
		if l == nil || !uncharged(l) {
			continue
		}
		delete(d.mergeTried, id)
		o := OfferFor(s, info, l.Alone)
		o.By = l.Owner
		if d.apply(r.actor, o) != nil && d.fatal != nil {
			return
		}
	}
}
