package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rolandpj1968/shunter/state"
)

// forge describes changes from a fixed table: a head each, and a base.
type forge map[state.ChangeID]Described

func (f forge) describe(ids []state.ChangeID) ([]Described, error) {
	var out []Described
	for _, id := range ids {
		d, ok := f[id]
		if !ok {
			return nil, errors.New("not found: " + string(id))
		}
		out = append(out, d)
	}
	return out, nil
}

func roundTripDoc() (Document, forge) {
	f := forge{}
	ch := func(id string, base string) state.Change {
		f[state.ChangeID(id)] = Described{ID: state.ChangeID(id), Head: state.Head("h" + id), Base: state.ChangeID(base)}
		return state.Change{ID: state.ChangeID(id), Head: state.Head("h" + id)}
	}
	d := Document{Format: ExportFormat, Lines: []ExportLine{
		{ID: "101", Changes: []state.Change{ch("101", ""), ch("102", "101")}, State: "waiting"},
		{ID: "103", Changes: []state.Change{ch("103", "102")}, Base: "101", State: "waiting-for-base"},
		{ID: "104", Changes: []state.Change{ch("104", "")}, Alone: true, State: "waiting"},
		{ID: "105", Changes: []state.Change{ch("105", "")}, State: "held", Reason: "waits on a design call -- and on a fix"},
		{ID: "106", Changes: []state.Change{ch("106", ""), ch("107", "106")}, State: "parked", Park: "j1-t9"},
		{ID: "108", Changes: []state.Change{ch("108", "")}, Alone: true, State: "parked", Park: "j1-t9"},
		{ID: "109", Changes: []state.Change{ch("109", "")}, State: "parked", Park: "fixture"},
	}, Parks: []ExportPark{
		{ID: "fixture", All: true, Cond: "landed:110", Reason: "the shared fixture is broken"},
		{ID: "j1-t9", Cond: "deadline: the judge reviews", Reason: "parked at J1"},
	}}
	return d, f
}

func TestLineFilesRoundTrip(t *testing.T) {
	d, f := roundTripDoc()
	lf, err := ToLines(d)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := lf.Write(dir); err != nil {
		t.Fatal(err)
	}
	waiting, _ := os.ReadFile(filepath.Join(dir, WaitingFile))
	want := "b 101 102\nb 103\nb 104 alone\nb 105\n"
	if got := strings.SplitN(string(waiting), "\n", 2)[1]; got != want {
		t.Fatalf("waiting.list:\n%s\nwant:\n%s", got, want)
	}
	held, _ := os.ReadFile(filepath.Join(dir, HeldFile))
	if !strings.Contains(string(held), "b 105 -- waits on a design call - and on a fix\n") {
		t.Fatalf("held.list:\n%s", held)
	}
	parked, _ := os.ReadFile(filepath.Join(dir, ParkedFile))
	if !strings.Contains(string(parked), "park fixture all -- landed:110 -- the shared fixture is broken\nb 109\npark j1-t9 -- deadline: the judge reviews -- parked at J1\nb 106 107\nb 108 alone\n") {
		t.Fatalf("parked.list:\n%s", parked)
	}
	back, err := ReadLines(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, lf) {
		t.Fatalf("read back:\n%+v\nwrote:\n%+v", back, lf)
	}
	doc, err := FromLines(back, f.describe)
	if err != nil {
		t.Fatal(err)
	}
	// What comes back is the document, less what a line file cannot carry:
	// the order between waiting and parked lines (parked come last, by
	// park), a waiting-for-base line is offered as waiting (the gate says
	// again that it waits for its base), and a reason's " -- ".
	want2 := d
	want2.Lines = append([]ExportLine(nil), d.Lines...)
	want2.Lines[1].State = "waiting"
	want2.Lines[3].Reason = "waits on a design call - and on a fix"
	want2.Lines[4], want2.Lines[5], want2.Lines[6] = d.Lines[6], d.Lines[4], d.Lines[5]
	if !reflect.DeepEqual(doc, want2) {
		t.Fatalf("round trip:\n%+v\nwant:\n%+v", doc, want2)
	}
	// And it is a document Import accepts.
	plan, err := Import(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Offers) != 7 || plan.Offers[1].Base != "101" {
		t.Fatalf("offers %+v", plan.Offers)
	}
	all := map[state.LineID]bool{}
	for _, o := range plan.Offers {
		all[o.Line] = true
	}
	evs := plan.After(all)
	var holds, parks int
	for _, e := range evs {
		switch e.(type) {
		case state.Hold:
			holds++
		case state.ParkLines:
			parks++
		}
	}
	if holds != 1 || parks != 2 {
		t.Fatalf("after the gate: %d holds, %d parks: %+v", holds, parks, evs)
	}
}

func TestLineFilesRefuse(t *testing.T) {
	for name, body := range map[string][2]string{
		"no b":           {WaitingFile, "101 102\n"},
		"empty line":     {WaitingFile, "b\n"},
		"alone only":     {WaitingFile, "b alone\n"},
		"bad change":     {WaitingFile, "b 1#2\n"},
		"hold no reason": {HeldFile, "b 101\n"},
		"park no cond":   {ParkedFile, "park p1 --  -- why\n"},
		"line no park":   {ParkedFile, "b 101\n"},
		"park bad head":  {ParkedFile, "park p1 some -- c -- r\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if body[0] != WaitingFile {
				os.WriteFile(filepath.Join(dir, WaitingFile), []byte("b 101\n"), 0o644)
			}
			os.WriteFile(filepath.Join(dir, body[0]), []byte(body[1]), 0o644)
			if _, err := ReadLines(dir); err == nil {
				t.Fatal("read")
			}
		})
	}
	// A hold on a line that is not waiting, and a change in two lines.
	f := forge{"1": {ID: "1", Head: "h1"}, "2": {ID: "2", Head: "h2"}}
	if _, err := FromLines(LineFile{Waiting: []PlainLine{{Changes: []state.ChangeID{"1"}}}, Held: []PlainHold{{Changes: []state.ChangeID{"2"}, Reason: "x"}}}, f.describe); err == nil {
		t.Fatal("a hold on no line")
	}
	if _, err := FromLines(LineFile{Waiting: []PlainLine{{Changes: []state.ChangeID{"1"}}, {Changes: []state.ChangeID{"1", "2"}}}}, f.describe); err == nil {
		t.Fatal("a change in two lines")
	}
	// A change named so that it can't be read back is refused on the way out.
	d := Document{Lines: []ExportLine{{ID: "x", Changes: []state.Change{{ID: "has space"}}, State: "waiting"}}}
	if _, err := ToLines(d); err == nil {
		t.Fatal("wrote a change with a space")
	}
}

// Comments, blank lines and "#123" are read as a script queue writes them.
func TestParseWaitingAsAScriptQueueWritesIt(t *testing.T) {
	got, err := ParseWaiting(strings.NewReader("# the list\n\nb #7 8\n  b 9 alone  \n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []PlainLine{{Changes: []state.ChangeID{"7", "8"}}, {Changes: []state.ChangeID{"9"}, Alone: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
}
