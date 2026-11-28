// Package store keeps the queue's one record (I8): an append-only log of the
// events the state machine accepted, from which the state is rebuilt, and a
// snapshot that saves replaying the whole log at every start.
//
// The store accepts events, never states. Every write goes through
// state.Next, so a transition that is not a row of the table cannot reach the
// disk. An event the machine refuses is logged as refused, and changes
// nothing.
//
// Layout of a store directory:
//
//	lock           held with flock by the one writer
//	log.jsonl      one record a line: "<crc32c hex> <json>\n", appended and fsynced
//	snapshot.json  the state as at a sequence number; written to a temporary
//	               file, fsynced and renamed into place
//
// A crash can leave the log's last line torn. Open drops a torn last line,
// which was never acknowledged, and refuses a log damaged anywhere else.
//
// Why a log and not SQLite, nor a file per line: the machine is a pure
// function of its events, so the log of events is the record, and the state
// is derived from it. The log is also what the replayer and trace validation
// read, so the store keeps one format for all three. A file per line would
// need several files changed atomically for one event (a train and its
// lines), which a rename can't give. SQLite would give that atomicity, but
// as a cgo or a large pure-Go dependency, to hold a state of a few hundred
// records that fits in memory. The module has no dependencies, and this
// keeps it so.
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/rolandpj1968/shunter/state"
)

// Record kinds.
const (
	KindInit    = "init"    // the queue's first record: its trunk and its bounds
	KindApplied = "applied" // an event the machine accepted
	KindRefused = "refused" // an event the machine refused; nothing changed
	KindDone    = "done"    // effects completed with no event to report
	KindStart   = "start"   // a writer opened the store, with its version
	KindNote    = "note"    // an alert or an escalation, recorded where it is seen (L3)
)

// Record is one line of the log.
type Record struct {
	Seq     uint64        `json:"seq"`
	Time    time.Time     `json:"time"`
	Actor   string        `json:"actor,omitempty"`
	Kind    string        `json:"kind"`
	Event   *Envelope     `json:"event,omitempty"`
	Effects []Keyed       `json:"effects,omitempty"` // applied: what the machine asked for
	Done    []string      `json:"done,omitempty"`    // effect keys this record completes
	Result  string        `json:"result,omitempty"`  // done: what became of them
	Error   string        `json:"error,omitempty"`   // refused: why
	Version string        `json:"version,omitempty"` // start
	Trunk   state.Head    `json:"trunk,omitempty"`   // init
	Config  *state.Config `json:"config,omitempty"`  // init
}

// Keyed is an effect with the key that names it for its whole life. An
// adapter performs a keyed effect at most once in the world however often it
// is asked: the key is how a restart in the middle of an effect stays safe.
type Keyed struct {
	Key   string `json:"key"`
	Seq   uint64 `json:"seq"`
	Index int    `json:"index"`
	state.Effect
}

// Since is when each live train entered its state and each park was made:
// the clocks of the deadlines. It is derived from the log's record times.
// Failures are the most recent trains that failed or went to J1, with their
// evidence: J1's default asks whether the same test failed in the train
// before.
type Since struct {
	Trains map[state.TrainID]time.Time `json:"trains"`
	Parks  map[state.ParkID]time.Time  `json:"parks"`
	// Lines is when each line that is not landed or dropped entered the
	// state it is in: how long a hold has stood, how long a charged line
	// has been with its owner.
	Lines    map[state.LineID]time.Time `json:"lines,omitempty"`
	Failures []Failure                  `json:"failures,omitempty"`
	// Queued is each line's queue time in its ended stints, and Waits the
	// start of the stint it is in: the time it has waited while the queue
	// was not paused (docs/queue-as-bag.md §4). A parked line is not
	// waiting. Nothing but landing or a withdrawal clears it.
	Queued map[state.LineID]time.Duration `json:"queued,omitempty"`
	Waits  map[state.LineID]time.Time     `json:"waits,omitempty"`
	// Checker is the line checker's last sign of life (§5.4): a claim, a
	// verdict, a claim given up, its declared version, or the line-check
	// gate changed. Under the gate, a line waits on it.
	Checker time.Time `json:"checker,omitempty"`
}

// QueueTime is the line's queue time at now.
func (s Since) QueueTime(id state.LineID, now time.Time) time.Duration {
	d := s.Queued[id]
	if at, ok := s.Waits[id]; ok && now.After(at) {
		d += now.Sub(at)
	}
	return d
}

// Failure is a train's deterministic failure, as its record states it.
type Failure struct {
	// TreeWide says the failing test scans the whole tree.
	TreeWide bool           `json:"tree_wide,omitempty"`
	Train    state.TrainID  `json:"train"`
	Lines    []state.LineID `json:"lines"`
	Evidence string         `json:"evidence"`
}

// keepFailures bounds Since.Failures.
const keepFailures = 16

// Options open a store.
type Options struct {
	// Init creates the store when the directory holds none.
	Init *Init
	// Version is the writer's version, recorded at every open (§7).
	Version string
	// Actor is recorded with the start record.
	Actor string
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// SnapshotEvery writes a snapshot after this many records; 256 when 0.
	SnapshotEvery int
}

// Init is a new queue.
type Init struct {
	Trunk  state.Head
	Config state.Config
}

// ErrRefused wraps the machine's refusal of an event. The refusal is logged.
var ErrRefused = errors.New("refused")

// ErrLocked means another writer holds the store (I7, I8).
var ErrLocked = errors.New("store: another writer holds the lock")

// Store is the single writer's handle on the record.
type Store struct {
	dir       string
	lock      *os.File
	log       *os.File
	now       func() time.Time
	every     int
	sinceSnap int

	s       state.State
	seq     uint64
	pending map[string]Keyed
	since   Since
	closed  bool
}

type snapshot struct {
	Seq     uint64      `json:"seq"`
	State   state.State `json:"state"`
	Pending []Keyed     `json:"pending"`
	Since   Since       `json:"since"`
}

const (
	logName  = "log.jsonl"
	snapName = "snapshot.json"
	lockName = "lock"
)

// Open takes the store's lock, rebuilds the state from the snapshot and the
// log, and records the start. It fails if another writer holds the lock.
func Open(dir string, o Options) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lk, err := lockFile(filepath.Join(dir, lockName))
	if err != nil {
		return nil, err
	}
	st := &Store{dir: dir, lock: lk, now: o.Now, every: o.SnapshotEvery,
		pending: map[string]Keyed{}, since: newSince()}
	if st.now == nil {
		st.now = time.Now
	}
	if st.every <= 0 {
		st.every = 256
	}
	fail := func(err error) (*Store, error) {
		if st.log != nil {
			st.log.Close()
		}
		unlock(lk)
		return nil, err
	}
	fresh, err := st.load()
	if err != nil {
		return fail(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, logName), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fail(err)
	}
	st.log = f
	if fresh {
		if o.Init == nil {
			return fail(fmt.Errorf("store: %s holds no queue, and no Init was given", dir))
		}
		c := o.Init.Config
		st.s = state.New(o.Init.Trunk, c)
		c = st.s.Config
		if err := st.append(&Record{Kind: KindInit, Actor: o.Actor, Trunk: o.Init.Trunk, Config: &c}); err != nil {
			return fail(err)
		}
		if err := syncDir(dir); err != nil {
			return fail(err)
		}
	}
	if err := st.append(&Record{Kind: KindStart, Actor: o.Actor, Version: o.Version}); err != nil {
		return fail(err)
	}
	return st, nil
}

func newSince() Since {
	return Since{Trains: map[state.TrainID]time.Time{}, Parks: map[state.ParkID]time.Time{}}
}

// load reads the snapshot and replays the log after it. It reports whether
// the store is new.
func (st *Store) load() (fresh bool, err error) {
	snap, err := readSnapshot(filepath.Join(st.dir, snapName))
	if err != nil {
		return false, err
	}
	if snap != nil {
		st.s, st.seq, st.since = snap.State, snap.Seq, snap.Since
		if st.since.Trains == nil || st.since.Parks == nil {
			st.since = newSince()
		}
		for _, k := range snap.Pending {
			st.pending[k.Key] = k
		}
	}
	recs, good, err := scan(filepath.Join(st.dir, logName))
	if err != nil {
		return false, err
	}
	if good >= 0 {
		// Drop a torn last line: it was never acknowledged.
		if err := os.Truncate(filepath.Join(st.dir, logName), good); err != nil {
			return false, err
		}
	}
	if len(recs) == 0 && snap == nil {
		return true, nil
	}
	started := snap != nil
	for _, r := range recs {
		if r.Seq <= st.seq && started {
			continue
		}
		if started && r.Seq != st.seq+1 {
			return false, fmt.Errorf("store: log jumps from %d to %d", st.seq, r.Seq)
		}
		if !started && r.Kind != KindInit {
			return false, fmt.Errorf("store: the log does not begin with its init record")
		}
		started = true
		if err := st.replay(r); err != nil {
			return false, fmt.Errorf("store: record %d: %w", r.Seq, err)
		}
		st.seq = r.Seq
	}
	return false, nil
}

// replay applies one record that is already on disk.
func (st *Store) replay(r Record) error {
	switch r.Kind {
	case KindInit:
		if r.Config == nil {
			return errors.New("init with no config")
		}
		st.s = state.New(r.Trunk, *r.Config)
	case KindApplied:
		if r.Event == nil {
			return errors.New("applied with no event")
		}
		ev, err := Decode(*r.Event)
		if err != nil {
			return err
		}
		before := st.s
		next, fx, err := state.Next(st.s, ev)
		if err != nil {
			return fmt.Errorf("the machine now refuses an event it once accepted (%w); start the version that wrote the log", err)
		}
		keyed := keyEffects(r.Seq, fx)
		if !reflect.DeepEqual(keyed, r.Effects) && !(len(keyed) == 0 && len(r.Effects) == 0) {
			return fmt.Errorf("the machine now decides %s differently; start the version that wrote the log", state.Name(ev))
		}
		st.s = next
		st.track(before, ev, keyed, r.Time)
	}
	for _, k := range r.Done {
		delete(st.pending, k)
	}
	return nil
}

// keyEffects names each effect by the record that asked for it and its place.
func keyEffects(seq uint64, fx []state.Effect) []Keyed {
	out := make([]Keyed, 0, len(fx))
	for i, e := range fx {
		out = append(out, Keyed{Key: strconv.FormatUint(seq, 10) + "." + strconv.Itoa(i), Seq: seq, Index: i, Effect: e})
	}
	return out
}

// track updates what is derived from an applied record: its pending effects
// and the clocks of the deadlines.
func (st *Store) track(before state.State, ev state.Event, keyed []Keyed, at time.Time) {
	switch e := ev.(type) {
	case state.CheckClaimed, state.LineChecked, state.ClaimReleased, state.RequireCheck:
		st.since.Checker = at
	case state.CheckerChanged:
		if e.Checker == state.CheckerLineCheck {
			st.since.Checker = at
		}
	}
	for _, k := range keyed {
		if k.Kind == state.EffRecord {
			if k.Line != "" && k.Train == "" {
				if st.since.Lines == nil {
					st.since.Lines = map[state.LineID]time.Time{}
				}
				st.since.Lines[k.Line] = at
			}
			if k.Line == "" && k.Train != "" {
				st.since.Trains[k.Train] = at
				if k.To == state.TrainJudging.String() || (k.To == state.TrainFailed.String() && k.Cause == state.CauseCharged) {
					t := st.s.Trains[k.Train]
					st.since.Failures = append(st.since.Failures, Failure{Train: k.Train, Lines: append([]state.LineID(nil), t.Lines...), Evidence: k.Reason, TreeWide: t.TreeWide})
					if n := len(st.since.Failures); n > keepFailures {
						st.since.Failures = append([]Failure(nil), st.since.Failures[n-keepFailures:]...)
					}
				}
			}
			continue
		}
		st.pending[k.Key] = k
	}
	for id, t := range st.s.Trains {
		if !t.State.Live() {
			delete(st.since.Trains, id)
		}
	}
	for id := range st.since.Lines {
		if l := st.s.Lines[id]; l == nil || l.State == state.LineLanded || l.State == state.LineDropped {
			delete(st.since.Lines, id)
		}
	}
	st.queueClock(at)
	for id := range st.s.Parks {
		if before.Parks[id] == nil {
			st.since.Parks[id] = at
		}
	}
	for id := range st.since.Parks {
		if st.s.Parks[id] == nil {
			delete(st.since.Parks, id)
		}
	}
}

// queueClock starts and stops each line's queue time at an applied record.
func (st *Store) queueClock(at time.Time) {
	if st.since.Queued == nil {
		st.since.Queued = map[state.LineID]time.Duration{}
	}
	if st.since.Waits == nil {
		st.since.Waits = map[state.LineID]time.Time{}
	}
	for id, l := range st.s.Lines {
		on := l.State == state.LineWaiting && !st.s.Paused
		start, was := st.since.Waits[id]
		switch {
		case was && !on:
			if at.After(start) {
				st.since.Queued[id] += at.Sub(start)
			}
			delete(st.since.Waits, id)
		case on && !was:
			st.since.Waits[id] = at
		}
		if l.State == state.LineLanded || l.State == state.LineDropped {
			delete(st.since.Queued, id)
		}
	}
}

// State returns the current state. Treat it as a value.
func (st *Store) State() state.State { return st.s }

// Seq returns the sequence number of the last record.
func (st *Store) Seq() uint64 { return st.seq }

// Since returns the clocks of the deadlines.
func (st *Store) Since() Since { return st.since }

// Pending returns the effects asked for and not yet recorded as done, in the
// order they were asked for.
func (st *Store) Pending() []Keyed {
	out := make([]Keyed, 0, len(st.pending))
	for _, k := range st.pending {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].Index < out[j].Index
	})
	return out
}

// IsPending reports whether an effect is still owed.
func (st *Store) IsPending(key string) bool { _, ok := st.pending[key]; return ok }

// Apply runs ev through the machine and records the result, with the effects
// it completes. It returns the new state and the effects now owed. If the
// machine refuses ev, the refusal is recorded (completing done all the same)
// and the error wraps ErrRefused. Any other error is the disk's, and the
// state is unchanged.
func (st *Store) Apply(actor string, ev state.Event, done ...string) (state.State, []Keyed, error) {
	if st.closed {
		return st.s, nil, errors.New("store: closed")
	}
	env, err := Encode(ev)
	if err != nil {
		return st.s, nil, err
	}
	next, fx, nerr := state.Next(st.s, ev)
	if nerr != nil {
		r := &Record{Kind: KindRefused, Actor: actor, Event: &env, Done: done, Error: nerr.Error()}
		if err := st.append(r); err != nil {
			return st.s, nil, err
		}
		for _, k := range done {
			delete(st.pending, k)
		}
		if err := st.settle(); err != nil {
			return st.s, nil, err
		}
		return st.s, nil, fmt.Errorf("%w: %w", ErrRefused, nerr)
	}
	seq := st.seq + 1
	keyed := keyEffects(seq, fx)
	r := &Record{Kind: KindApplied, Actor: actor, Event: &env, Effects: keyed, Done: done}
	if err := st.append(r); err != nil {
		return st.s, nil, err
	}
	before := st.s
	st.s = next
	for _, k := range done {
		delete(st.pending, k)
	}
	st.track(before, ev, keyed, r.Time)
	var owed []Keyed
	for _, k := range keyed {
		if k.Kind != state.EffRecord {
			owed = append(owed, k)
		}
	}
	return st.s, owed, st.settle()
}

// Done records effects completed with no event to report: a notice sent, a
// pull request closed, an effect found stale.
func (st *Store) Done(actor, result string, keys ...string) error {
	if st.closed {
		return errors.New("store: closed")
	}
	if err := st.append(&Record{Kind: KindDone, Actor: actor, Done: keys, Result: result}); err != nil {
		return err
	}
	for _, k := range keys {
		delete(st.pending, k)
	}
	return st.settle()
}

// Note records an alert or an escalation. It changes no state.
func (st *Store) Note(actor, msg string) error {
	if st.closed {
		return errors.New("store: closed")
	}
	if err := st.append(&Record{Kind: KindNote, Actor: actor, Result: msg}); err != nil {
		return err
	}
	return st.settle()
}

// append writes one record and fsyncs it. The record's sequence number and
// time are set here. On error nothing in memory changes.
func (st *Store) append(r *Record) error {
	r.Seq = st.seq + 1
	r.Time = st.now().UTC()
	line, err := encodeLine(r)
	if err != nil {
		return err
	}
	if _, err := st.log.Write(line); err != nil {
		return fmt.Errorf("store: append: %w", err)
	}
	if err := st.log.Sync(); err != nil {
		return fmt.Errorf("store: fsync: %w", err)
	}
	st.seq = r.Seq
	st.sinceSnap++
	return nil
}

// settle writes a snapshot when enough records have passed. It runs after the
// state in memory has caught up with the log, never between the two.
func (st *Store) settle() error {
	if st.sinceSnap < st.every {
		return nil
	}
	return st.Snapshot()
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func encodeLine(r *Record) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("%08x %s\n", crc32.Checksum(b, crcTable), b)), nil
}

// scan reads every whole record of the log. good is the length to truncate
// the file to when its last line is torn, or -1 when it is whole.
func scan(path string) (recs []Record, good int64, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, -1, nil
	}
	if err != nil {
		return nil, -1, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	var off int64
	for n := 1; ; n++ {
		line, rerr := r.ReadBytes('\n')
		if len(line) == 0 && rerr == io.EOF {
			return recs, -1, nil
		}
		if rerr != nil && rerr != io.EOF {
			return nil, -1, rerr
		}
		rec, perr := decodeLine(line)
		if perr != nil {
			// Only the last line may be torn, and only without its newline
			// or with a bad checksum at the very end of the file.
			if _, peek := r.Peek(1); peek == io.EOF {
				return recs, off, nil
			}
			return nil, -1, fmt.Errorf("store: log line %d is damaged: %w", n, perr)
		}
		recs = append(recs, rec)
		off += int64(len(line))
		if rerr == io.EOF {
			return recs, -1, nil
		}
	}
}

func decodeLine(line []byte) (Record, error) {
	var rec Record
	if len(line) == 0 || line[len(line)-1] != '\n' {
		return rec, errors.New("no newline")
	}
	sum, body, ok := bytes.Cut(bytes.TrimSuffix(line, []byte("\n")), []byte(" "))
	if !ok {
		return rec, errors.New("no checksum")
	}
	want, err := strconv.ParseUint(string(sum), 16, 32)
	if err != nil {
		return rec, err
	}
	if crc32.Checksum(body, crcTable) != uint32(want) {
		return rec, errors.New("checksum mismatch")
	}
	err = json.Unmarshal(body, &rec)
	return rec, err
}

// ReadLog returns every whole record of a store's log, without taking the
// lock. It is for audit and replay, never for writing.
func ReadLog(dir string) ([]Record, error) {
	recs, _, err := scan(filepath.Join(dir, logName))
	return recs, err
}

// Snapshot writes the state atomically: a temporary file, fsynced, renamed
// over the old snapshot, and the directory fsynced.
func (st *Store) Snapshot() error {
	b, err := json.Marshal(snapshot{Seq: st.seq, State: st.s, Pending: st.Pending(), Since: st.since})
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(st.dir, snapName), b); err != nil {
		return err
	}
	st.sinceSnap = 0
	return nil
}

func readSnapshot(path string) (*snapshot, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("store: snapshot: %w", err)
	}
	if s.State.Lines == nil || s.State.Trains == nil || s.State.Parks == nil {
		return nil, errors.New("store: snapshot holds no state")
	}
	return &s, nil
}

// WriteFileAtomic writes data to path so that a reader sees the old file or
// the new one, never part of either.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		os.Remove(name)
		return werr
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Close writes a snapshot, closes the log and releases the lock.
func (st *Store) Close() error {
	if st.closed {
		return nil
	}
	err := st.Snapshot()
	st.closed = true
	if cerr := st.log.Close(); err == nil {
		err = cerr
	}
	unlock(st.lock)
	return err
}

// Abandon releases the store as a crash would: no snapshot, no final record.
// Tests use it to prove a restart from the log alone.
func (st *Store) Abandon() {
	st.closed = true
	st.log.Close()
	unlock(st.lock)
}
