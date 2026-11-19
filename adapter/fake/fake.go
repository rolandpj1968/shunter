// Package fake is an in-memory world for the daemon: a forge, CI, git and a
// notifier in one, with the faults of PROTOCOL.md §7 at hand. Tests drive it:
// they open changes, make CI pass or fail, forge reports, refuse pushes and
// block any call until they release it.
package fake

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/state"
)

// Change is a change on the fake forge.
type Change struct {
	Head state.Head
	Base state.ChangeID
	// Parent is the head this change was built on: its base's head when it
	// was opened. Moving the base's head leaves it, as a push to the base
	// leaves a change stacked on it until it is rebased.
	Parent   state.Head
	Open     bool
	Approved bool
	Landed   bool
}

// Notice is one message sent.
type Notice struct {
	Key   string
	To    string // "owner" or "judge"
	Line  state.LineID
	Train state.TrainID
	Point string
	Msg   string
}

// World is the fake forge, CI, git and notifier.
type World struct {
	mu      sync.Mutex
	trunk   state.Head
	history []state.Head
	changes map[state.ChangeID]*Change

	builds  map[string]adapter.BuildResult // by effect key
	prs     map[string]string              // effect key -> pull request
	open    map[state.TrainID]string       // train -> open pull request
	heads   map[state.TrainID]state.Head   // train -> its built head
	verdict map[state.TrainID]state.Head   // what the forge shows passed (the truth)
	report  map[state.TrainID]adapter.Report
	reruns  map[state.TrainID]int
	cancels map[state.TrainID]int
	nextPR  int

	// Calls counts calls by kind; Acts counts acts that changed the world.
	Calls map[string]int
	Acts  map[string]int
	// Notices are the messages sent, in order.
	Notices []Notice
	// Pruned is the live trains each prune was told, in order.
	Pruned [][]state.TrainID
	// Retargeted is the changes moved onto trunk, in order.
	Retargeted []state.ChangeID

	// BuildHook, when set, decides a build. It may block on ctx.
	BuildHook func(ctx context.Context, req adapter.BuildRequest) (adapter.BuildResult, error)
	// ReblameHook, when set, answers a forced blame (adapter.Reblamer).
	// When nil, a forced blame names nobody.
	ReblameHook func(t state.TrainID, rep adapter.Report, lines []adapter.BuildLine) adapter.Report
	// PushFaults are errors the next pushes return, in order, before trunk
	// moves.
	PushFaults []error
	// AfterPush, when set, runs after a push has moved trunk and before the
	// call returns: a crash between the act and its record.
	AfterPush func(ctx context.Context) error

	blocks  map[string]chan struct{}
	blocked int
	waiters map[chan struct{}]int
}

// Blocked returns how many calls wait on a block now.
func (w *World) Blocked() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.blocked
}

// New returns a world with trunk at the given head.
func New(trunk state.Head) *World {
	return &World{
		trunk: trunk, changes: map[state.ChangeID]*Change{},
		builds: map[string]adapter.BuildResult{}, prs: map[string]string{}, open: map[state.TrainID]string{},
		heads: map[state.TrainID]state.Head{}, verdict: map[state.TrainID]state.Head{}, report: map[state.TrainID]adapter.Report{},
		reruns: map[state.TrainID]int{}, cancels: map[state.TrainID]int{}, nextPR: 100,
		Calls: map[string]int{}, Acts: map[string]int{}, blocks: map[string]chan struct{}{}, waiters: map[chan struct{}]int{},
	}
}

// Set returns the world as the daemon's adapters.
func (w *World) Set() adapter.Set { return adapter.Set{Forge: w, CI: w, Git: w, Notify: w} }

// Open adds an approved, open change on trunk (or on base).
func (w *World) Open(id state.ChangeID, head state.Head, base state.ChangeID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := &Change{Head: head, Base: base, Open: true, Approved: true}
	if b := w.changes[base]; b != nil {
		c.Parent = b.Head
	}
	w.changes[id] = c
}

// Change edits a change.
func (w *World) Change(id state.ChangeID, f func(*Change)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w.changes[id])
}

// Trunk returns trunk and every head it has been at.
func (w *World) Trunk() (state.Head, []state.Head) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.trunk, append([]state.Head(nil), w.history...)
}

// MoveTrunk is another writer moving trunk (a breach of I7).
func (w *World) MoveTrunk(h state.Head) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.trunk = h
	w.history = append(w.history, h)
}

// Pass makes CI pass on the train's head: the forge shows it, and CI reports it.
func (w *World) Pass(t state.TrainID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.verdict[t] = w.heads[t]
	w.report[t] = adapter.Report{Outcome: adapter.Passed, Head: w.heads[t]}
}

// Fail makes CI fail on the train, with blame and evidence.
func (w *World) Fail(t state.TrainID, evidence string, blamed ...state.LineID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.report[t] = adapter.Report{Outcome: adapter.Failed, Head: w.heads[t], Blamed: blamed, Evidence: evidence}
}

// Report sets what CI reports for the train and nothing else: with Passed
// and no Pass, it is a forged verdict line (§7), which the forge does not
// show.
func (w *World) Report(t state.TrainID, r adapter.Report) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.Head == "" {
		r.Head = w.heads[t]
	}
	w.report[t] = r
}

// PR returns the train's open pull request.
func (w *World) PR(t state.TrainID) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.open[t]
}

// Count returns w.Acts[kind] or w.Calls[kind] under the lock.
func (w *World) Count(kind string, acts bool) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if acts {
		return w.Acts[kind]
	}
	return w.Calls[kind]
}

// NoticesTo returns the notices sent to "owner" or "judge" that contain s.
func (w *World) NoticesTo(to, s string) []Notice {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []Notice
	for _, n := range w.Notices {
		if n.To == to && strings.Contains(n.Msg, s) {
			out = append(out, n)
		}
	}
	return out
}

// Block makes every call of kind wait until the returned function is
// called, or its context ends. The release returns once every call it held
// has woken.
func (w *World) Block(kind string) (release func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch := make(chan struct{})
	w.blocks[kind] = ch
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			if w.blocks[kind] == ch {
				delete(w.blocks, kind)
			}
			w.mu.Unlock()
			close(ch)
			for {
				w.mu.Lock()
				n := w.waiters[ch]
				w.mu.Unlock()
				if n == 0 {
					return
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

// enter counts a call and waits on a block. It is called without the lock.
func (w *World) enter(ctx context.Context, kind string) error {
	w.mu.Lock()
	w.Calls[kind]++
	ch := w.blocks[kind]
	if ch != nil {
		w.blocked++
		w.waiters[ch]++
	}
	w.mu.Unlock()
	if ch == nil {
		return nil
	}
	defer func() { w.mu.Lock(); w.blocked--; w.waiters[ch]--; w.mu.Unlock() }()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Describe implements adapter.Forge.
func (w *World) Describe(ctx context.Context, ids []state.ChangeID) ([]adapter.ChangeInfo, error) {
	if err := w.enter(ctx, "describe"); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []adapter.ChangeInfo
	for _, id := range ids {
		c := w.changes[id]
		if c == nil {
			return nil, adapter.Permanent(fmt.Errorf("change %s not found", id))
		}
		out = append(out, adapter.ChangeInfo{ID: id, Head: c.Head, Base: c.Base, Open: c.Open, Approved: c.Approved, Landed: c.Landed})
	}
	return out, nil
}

// OpenPR implements adapter.Forge.
func (w *World) OpenPR(ctx context.Context, key string, t state.TrainID, head, base state.Head) (string, error) {
	if err := w.enter(ctx, "open-pr"); err != nil {
		return "", err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if pr, ok := w.prs[key]; ok {
		return pr, nil
	}
	w.nextPR++
	pr := fmt.Sprint(w.nextPR)
	w.prs[key], w.open[t], w.heads[t] = pr, pr, head
	w.report[t] = adapter.Report{Outcome: adapter.Pending, Head: head}
	w.Acts["open-pr"]++
	return pr, nil
}

// ClosePR implements adapter.Forge.
func (w *World) ClosePR(ctx context.Context, key string, t state.TrainID, reason string) error {
	if err := w.enter(ctx, "close-pr"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.open[t]; ok {
		delete(w.open, t)
		w.Acts["close-pr"]++
	}
	return nil
}

// ReadLanding implements adapter.Forge: the truth, not CI's report.
func (w *World) ReadLanding(ctx context.Context, t state.TrainID, head state.Head, ids []state.ChangeID) (adapter.Reading, error) {
	if err := w.enter(ctx, "verify"); err != nil {
		return adapter.Reading{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	r := adapter.Reading{Verdict: w.verdict[t], Heads: map[state.ChangeID]state.Head{}, Trunk: w.trunk}
	for _, id := range ids {
		if c := w.changes[id]; c != nil {
			r.Heads[id] = c.Head
		}
	}
	return r, nil
}

// Status implements adapter.CI.
func (w *World) Status(ctx context.Context, t state.TrainID, head state.Head, _ []adapter.BuildLine) (adapter.Report, error) {
	if err := w.enter(ctx, "status"); err != nil {
		return adapter.Report{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.report[t]
	if !ok {
		return adapter.Report{}, adapter.Permanent(fmt.Errorf("no run for train %s", t))
	}
	return r, nil
}

// Rerun implements adapter.CI.
func (w *World) Rerun(ctx context.Context, key string, t state.TrainID, head state.Head) error {
	if err := w.enter(ctx, "rerun"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reruns[t]++
	w.report[t] = adapter.Report{Outcome: adapter.Pending, Head: head}
	w.Acts["rerun"]++
	return nil
}

// Reblame implements adapter.Reblamer.
func (w *World) Reblame(ctx context.Context, t state.TrainID, rep adapter.Report, lines []adapter.BuildLine) (adapter.Report, error) {
	if err := w.enter(ctx, "reblame"); err != nil {
		return adapter.Report{}, err
	}
	w.mu.Lock()
	h := w.ReblameHook
	w.mu.Unlock()
	if h == nil {
		return rep, nil
	}
	return h(t, rep, lines), nil
}

// Contains implements adapter.Ancestry: a change's head contains itself,
// its Parent, and what its Parent contains.
func (w *World) Contains(ctx context.Context, head, ancestor state.Head, _ []state.ChangeID) (bool, error) {
	if err := w.enter(ctx, "contains"); err != nil {
		return false, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for seen := 0; head != "" && seen <= len(w.changes); seen++ {
		if head == ancestor {
			return true, nil
		}
		var next state.Head
		for _, c := range w.changes {
			if c.Head == head {
				next = c.Parent
			}
		}
		head = next
	}
	return false, nil
}

// Cancel implements adapter.CI.
func (w *World) Cancel(ctx context.Context, key string, t state.TrainID) error {
	if err := w.enter(ctx, "cancel-run"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancels[t]++
	if r := w.report[t]; r.Outcome == adapter.Pending {
		w.report[t] = adapter.Report{Outcome: adapter.NoVerdict, Head: r.Head, Why: "cancelled"}
	}
	w.Acts["cancel-run"]++
	return nil
}

// Build implements adapter.Git: the head is a name for the merge.
func (w *World) Build(ctx context.Context, key string, req adapter.BuildRequest) (adapter.BuildResult, error) {
	if err := w.enter(ctx, "build"); err != nil {
		return adapter.BuildResult{}, err
	}
	w.mu.Lock()
	if r, ok := w.builds[key]; ok {
		w.mu.Unlock()
		return r, nil
	}
	hook := w.BuildHook
	w.mu.Unlock()
	var r adapter.BuildResult
	if hook != nil {
		var err error
		if r, err = hook(ctx, req); err != nil {
			return r, err
		}
	}
	if r.Head == "" && !r.PreflightFailed {
		var parts []string
		for _, l := range req.Lines {
			if _, out := r.LeftOut[l.ID]; out {
				continue
			}
			for _, c := range l.Changes {
				parts = append(parts, string(c.ID)+"@"+string(c.Head))
			}
		}
		sort.Strings(parts)
		r.Head = state.Head(fmt.Sprintf("%s+%s", req.Base, strings.Join(parts, ",")))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.builds[key] = r
	w.Acts["build"]++
	return r, nil
}

// Push implements adapter.Git: fast-forward only, and idempotent.
func (w *World) Push(ctx context.Context, key string, head, expect state.Head) error {
	if err := w.enter(ctx, "push"); err != nil {
		return err
	}
	w.mu.Lock()
	if w.trunk == head {
		w.mu.Unlock()
		return nil // it took already
	}
	if len(w.PushFaults) > 0 {
		err := w.PushFaults[0]
		w.PushFaults = w.PushFaults[1:]
		w.mu.Unlock()
		return err
	}
	if w.trunk != expect {
		w.mu.Unlock()
		return adapter.Permanent(fmt.Errorf("not a fast-forward: trunk is at %s, not %s", w.trunk, expect))
	}
	w.trunk = head
	w.history = append(w.history, head)
	w.Acts["push"]++
	for id, c := range w.changes {
		if strings.Contains(string(head), string(id)+"@"+string(c.Head)) {
			c.Landed, c.Open = true, false
		}
	}
	after := w.AfterPush
	w.mu.Unlock()
	if after != nil {
		return after(ctx)
	}
	return nil
}

// Owner implements adapter.Notifier.
func (w *World) Owner(ctx context.Context, key string, line state.LineID, changes []state.ChangeID, msg string) error {
	if err := w.enter(ctx, "notify"); err != nil {
		return err
	}
	return w.notice(Notice{Key: key, To: "owner", Line: line, Msg: msg})
}

// Judge implements adapter.Notifier.
func (w *World) Judge(ctx context.Context, key, point string, t state.TrainID, msg string) error {
	if err := w.enter(ctx, "ask-judge"); err != nil {
		return err
	}
	return w.notice(Notice{Key: key, To: "judge", Point: point, Train: t, Msg: point + ": " + msg})
}

func (w *World) notice(n Notice) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n.Key != "" {
		for _, o := range w.Notices {
			if o.Key == n.Key {
				return nil // sent already
			}
		}
	}
	w.Notices = append(w.Notices, n)
	return nil
}

// ErrTransient is a fault that retrying clears.
var ErrTransient = errors.New("the forge answered 502")

// Prune implements adapter.Pruner: it records the live trains it was told.
func (w *World) Prune(ctx context.Context, live []state.TrainID) error {
	if err := w.enter(ctx, "prune"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Pruned = append(w.Pruned, append([]state.TrainID(nil), live...))
	return nil
}

// Retarget implements adapter.Retargeter: the change is on trunk now.
func (w *World) Retarget(ctx context.Context, id state.ChangeID) error {
	if err := w.enter(ctx, "retarget"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if c := w.changes[id]; c != nil && c.Base != "" {
		c.Base = ""
		w.Retargeted = append(w.Retargeted, id)
		w.Acts["retarget"]++
	}
	return nil
}

// Prunes returns a copy of the live trains each prune was told.
func (w *World) Prunes() [][]state.TrainID {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]state.TrainID(nil), w.Pruned...)
}
