package store

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/state"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Second); return c.t }

func open(t *testing.T, dir string, every int) *Store {
	t.Helper()
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	st, err := Open(dir, Options{Init: &Init{Trunk: "t0", Config: state.Config{Depth: 2}}, Version: "test", Actor: "test", Now: c.now, SnapshotEvery: every})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func offer(id string) state.Offer {
	return state.Offer{Line: state.LineID(id), Changes: []state.Change{{ID: state.ChangeID(id), Head: state.Head(id + "1")}}}
}

// history is a short life: two lines, one train built and opened, a hold.
func history() []state.Event {
	return []state.Event{
		offer("a"), state.GateAccept{Line: "a"},
		offer("b"), state.GateAccept{Line: "b"},
		offer("c"), state.GateAccept{Line: "c"},
		state.Assemble{Train: "t1", Lines: []state.LineID{"a", "b"}},
		state.Built{Train: "t1", Head: "h1"},
		state.Opened{Train: "t1", PR: "1", Head: "h1"},
		state.Hold{Line: "c", Reason: "under review"},
		state.ParkLines{Park: "p", Cond: "deadline", Reason: "r", Lines: nil},
	}
}

func applyAll(t *testing.T, st *Store, evs []state.Event) {
	t.Helper()
	for _, ev := range evs {
		if _, _, err := st.Apply("test", ev); err != nil {
			t.Fatalf("%s: %v", state.Name(ev), err)
		}
	}
}

func TestEveryWriteGoesThroughNext(t *testing.T) {
	st := open(t, t.TempDir(), 0)
	defer st.Close()
	applyAll(t, st, history()[:2])
	before := st.State()
	seq := st.Seq()
	// A transition that is not a row: a waiting line cannot be released.
	_, _, err := st.Apply("test", state.Release{Line: "a"})
	if !errors.Is(err, ErrRefused) || !errors.Is(err, state.ErrState) {
		t.Fatalf("want a refusal, got %v", err)
	}
	if !reflect.DeepEqual(st.State(), before) {
		t.Fatal("a refused event changed the state")
	}
	if st.Seq() != seq+1 {
		t.Fatal("the refusal was not logged")
	}
	recs, err := ReadLog(st.dir)
	if err != nil {
		t.Fatal(err)
	}
	if last := recs[len(recs)-1]; last.Kind != KindRefused || last.Error == "" {
		t.Fatalf("last record %+v", last)
	}
}

func TestRestartReplaysTheLog(t *testing.T) {
	for _, every := range []int{1000, 3} {
		dir := t.TempDir()
		st := open(t, dir, every)
		applyAll(t, st, history())
		want, pending, since := st.State(), st.Pending(), st.Since()
		st.Abandon() // a crash: no snapshot at close

		st = open(t, dir, every)
		if !reflect.DeepEqual(st.State(), want) {
			t.Fatalf("every %d: state after restart differs", every)
		}
		if !reflect.DeepEqual(st.Pending(), pending) {
			t.Fatalf("every %d: pending after restart: %v, want %v", every, st.Pending(), pending)
		}
		if !reflect.DeepEqual(st.Since(), since) {
			t.Fatalf("every %d: clocks after restart: %v, want %v", every, st.Since(), since)
		}
		// A clean close snapshots; the next open replays nothing.
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		st = open(t, dir, every)
		if !reflect.DeepEqual(st.State(), want) {
			t.Fatalf("every %d: state after a clean restart differs", every)
		}
		st.Close()
	}
}

func TestPendingAndDone(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir, 0)
	_, owed, err := st.Apply("test", offer("a"))
	if err != nil || len(owed) != 1 || owed[0].Kind != state.EffGate {
		t.Fatalf("owed %v, %v", owed, err)
	}
	key := owed[0].Key
	if !st.IsPending(key) {
		t.Fatal("not pending")
	}
	// The gate's answer completes the gate effect in the same record.
	if _, _, err := st.Apply("gate", state.GateAccept{Line: "a"}, key); err != nil {
		t.Fatal(err)
	}
	if st.IsPending(key) {
		t.Fatal("still pending")
	}
	_, owed, _ = st.Apply("test", state.Hold{Line: "a", Reason: "r"})
	if len(owed) != 1 || owed[0].Kind != state.EffNotify {
		t.Fatalf("owed %v", owed)
	}
	if err := st.Done("notify", "sent", owed[0].Key); err != nil {
		t.Fatal(err)
	}
	st.Abandon()
	st = open(t, dir, 0)
	defer st.Close()
	if n := len(st.Pending()); n != 0 {
		t.Fatalf("%d effects pending after restart", n)
	}
}

func TestOneWriter(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir, 0)
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second writer: %v", err)
	}
	st.Close()
	st2 := open(t, dir, 0)
	st2.Close()
}

func TestTornTail(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir, 0)
	applyAll(t, st, history()[:4])
	want := st.State()
	st.Abandon()
	path := filepath.Join(dir, logName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`0badc0de {"seq":99,"kind":"app`) // a write cut short
	f.Close()
	st = open(t, dir, 0)
	if !reflect.DeepEqual(st.State(), want) {
		t.Fatal("state differs after a torn tail")
	}
	applyAll(t, st, history()[4:5])
	st.Abandon()
	st = open(t, dir, 0)
	st.Close()
}

func TestDamageMidLogIsRefused(t *testing.T) {
	dir := t.TempDir()
	st := open(t, dir, 0)
	applyAll(t, st, history()[:4])
	st.Abandon()
	path := filepath.Join(dir, logName)
	b, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(b), "\n")
	lines[2] = strings.Replace(lines[2], "gate", "gatf", 1)
	os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644)
	if _, err := Open(dir, Options{}); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("want damage refused, got %v", err)
	}
}

func TestFreshStoreNeedsInit(t *testing.T) {
	if _, err := Open(t.TempDir(), Options{}); err == nil {
		t.Fatal("opened a store with no queue and no init")
	}
}

func TestWriteFileAtomicLeavesNoTemporaries(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")
	for _, s := range []string{"one", "two"} {
		if err := WriteFileAtomic(p, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(p)
	ents, _ := os.ReadDir(dir)
	if string(got) != "two" || len(ents) != 1 {
		t.Fatalf("got %q with %d entries", got, len(ents))
	}
}

// TestCodecCoversEveryEvent reads the state package's source for every type
// with an event method, and checks each one round-trips.
func TestCodecCoversEveryEvent(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "../state/events.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "event" || fn.Recv == nil {
			continue
		}
		n++
	}
	if n != len(events) {
		t.Fatalf("the state package has %d events; the codec knows %d", n, len(events))
	}
	for _, ev := range []state.Event{
		state.Abort{Train: "t", Reason: "r", Lines: map[state.LineID]state.Dispose{"a": {Kind: state.ParkIt, Park: "p", Cond: "c"}}},
		state.LandingVerified{Train: "t", Verdict: "h", Heads: map[state.ChangeID]state.Head{"a": "a1"}, Trunk: "t0"},
		state.Judge{Train: "t", Action: state.JudgeCharge, Lines: []state.LineID{"a"}, Evidence: "e"},
		offer("a"),
	} {
		env, err := Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Decode(env)
		if err != nil || !reflect.DeepEqual(back, ev) {
			t.Fatalf("%s: %v, %#v", state.Name(ev), err, back)
		}
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	st := open(t, t.TempDir(), 0)
	defer st.Close()
	applyAll(t, st, []state.Event{
		offer("a"), state.GateAccept{Line: "a"},
		offer("b"), state.GateAccept{Line: "b"},
		offer("c"), state.GateAccept{Line: "c"},
		offer("d"), state.GateAccept{Line: "d"},
		state.Hold{Line: "b", Reason: "under review"},
		state.ParkLines{Park: "q", Lines: []state.LineID{"c"}, Cond: "deadline 2026-01-02", Reason: "fixture broken"},
		state.Assemble{Train: "t1", Lines: []state.LineID{"d"}},
	})
	doc := Export(st.State())
	if len(doc.Lines) != 4 || doc.Lines[3].State != "waiting" || doc.Lines[1].State != "held" || doc.Lines[2].Park != "q" {
		t.Fatalf("export %+v", doc)
	}
	plan, err := Import(doc)
	if err != nil {
		t.Fatal(err)
	}
	s := state.New("t0", state.Config{Depth: 2})
	step := func(ev state.Event) {
		var err error
		if s, _, err = state.Next(s, ev); err != nil {
			t.Fatalf("%s: %v", state.Name(ev), err)
		}
	}
	acc := map[state.LineID]bool{}
	for _, o := range plan.Offers {
		step(o)
		step(state.GateAccept{Line: o.Line})
		acc[o.Line] = true
	}
	for _, ev := range plan.After(acc) {
		step(ev)
	}
	again := Export(s)
	if !reflect.DeepEqual(again, doc) {
		t.Fatalf("round trip:\n%+v\n%+v", again, doc)
	}

	bad := doc
	bad.Lines = append([]ExportLine(nil), doc.Lines...)
	bad.Lines[1].Reason = ""
	if _, err := Import(bad); err == nil {
		t.Fatal("imported a hold with no reason")
	}
}
