package shadow

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/replay/adapter/trainlog"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/store"
)

// reader is a read-only forge and CI with canned answers.
type reader struct {
	reports map[state.Head]adapter.Outcome
	changes map[state.ChangeID]adapter.ChangeInfo
	reads   int
	// lines are the lines each Status call was given, by head.
	lines map[state.Head][]adapter.BuildLine
}

func (r *reader) Describe(_ context.Context, ids []state.ChangeID) ([]adapter.ChangeInfo, error) {
	var out []adapter.ChangeInfo
	for _, id := range ids {
		r.reads++
		c, ok := r.changes[id]
		if !ok {
			return nil, adapter.Permanent(fmt.Errorf("change %s not found", id))
		}
		out = append(out, c)
	}
	return out, nil
}

func (r *reader) Status(_ context.Context, _ state.TrainID, head state.Head, lines []adapter.BuildLine) (adapter.Report, error) {
	r.reads++
	if r.lines == nil {
		r.lines = map[state.Head][]adapter.BuildLine{}
	}
	r.lines[head] = lines
	return adapter.Report{Outcome: r.reports[head], Head: head}, nil
}

func h(c byte) state.Head { return state.Head(strings.Repeat(string(c), 40)) }

func logs(t *testing.T) ([]trainlog.Log, func(string) bool) {
	dir := "../replay/adapter/trainlog/testdata"
	paths, _ := filepath.Glob(filepath.Join(dir, "*.out"))
	sort.Strings(paths)
	var out []trainlog.Log
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		lg, err := trainlog.Parse(filepath.Base(p), f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, lg)
	}
	f, err := os.Open(filepath.Join(dir, "stats.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	landed, err := trainlog.LandedPRs(f)
	if err != nil {
		t.Fatal(err)
	}
	return out, func(pr string) bool { return landed[pr] }
}

func TestShadowComparesHistoryAndGatesTheWaitingLines(t *testing.T) {
	lgs, landed := logs(t)
	r := &reader{
		reports: map[state.Head]adapter.Outcome{
			h('1'): adapter.Passed, h('2'): adapter.Passed, h('4'): adapter.Passed, h('a'): adapter.Passed,
			h('b'): adapter.Passed, h('f'): adapter.Passed, h('8'): adapter.Passed,
			h('c'): adapter.Failed, // landed by the live queue, and the checks on its head failed
			h('3'): adapter.Failed, h('6'): adapter.Failed,
			h('5'): adapter.NoVerdict, // the live queue charged a cancelled run
			h('7'): adapter.Pending, h('d'): adapter.Pending, h('e'): adapter.Pending,
		},
		changes: map[state.ChangeID]adapter.ChangeInfo{
			"21": {ID: "21", Head: "h21", Open: true, Approved: true},
			"22": {ID: "22", Head: "h22", Base: "21", Open: true, Approved: true},
			"23": {ID: "23", Head: "h23", Open: false, Approved: true},
			"24": {ID: "24", Head: "h24", Open: true, Approved: false},
			"25": {ID: "25", Head: "h25", Open: true, Approved: true},
			"26": {ID: "26", Head: "h26", Base: "99", Open: true, Approved: true},
			"106": {ID: "106", Head: "h106", Open: false, Approved: true},
		},
	}
	waiting, err := store.ParseWaiting(strings.NewReader("b 21 22\n# a comment\n\nb 23\nb 24\nb #25\nb 26\n"))
	if err != nil {
		t.Fatal(err)
	}
	rep := Run(context.Background(), Input{Logs: lgs, Landed: landed, Lines: store.LineFile{Waiting: waiting}, Read: r, Depth: 2, MaxLines: 1})

	got := map[string]string{}
	for _, tr := range rep.Trains {
		got[tr.PR] = tr.Kind
	}
	want := map[string]string{
		"901": "agree", "902": "agree", "904": "agree", "910": "agree", "911": "agree", "916": "agree",
		"912": "landed-without-verdict",
		"903": "agree",
		"905": "charged-on-no-verdict",
		"906": "unblamed-without-judge",
		"907": "agree",
		"908": "green-on-another-head",
		"914": "agree",
		"915": "agree",
	}
	for pr, k := range want {
		if got[pr] != k {
			t.Errorf("train %s: %q, want %q", pr, got[pr], k)
		}
	}
	if len(rep.Trains) != len(want) {
		t.Errorf("%d trains read, want %d", len(rep.Trains), len(want))
	}
	if rep.Effects["push"] == 0 || rep.Effects["build"] == 0 || rep.Effects["ask-judge J1"] == 0 {
		t.Errorf("effects %v", rep.Effects)
	}

	// The blame hook gets each line's changes, with their heads read from
	// the forge where it can: a line id alone is nothing to blame by.
	for head, ls := range r.lines {
		for _, l := range ls {
			if len(l.Changes) == 0 {
				t.Errorf("head %s: line %s given to Status with no changes", head[:4], l.ID)
			}
		}
	}
	if got := fmt.Sprint(r.lines[h('3')]); got != "[{105 [{105 }]} {106 [{106 h106}]} {107 [{107 }]}]" {
		t.Errorf("train 903's lines for blame: %s", got)
	}
	gates := []string{}
	for _, l := range rep.Waiting {
		gates = append(gates, l.Gate)
	}
	if fmt.Sprint(gates) != "[waiting refused refused waiting refused]" {
		t.Errorf("gates %v: %+v", gates, rep.Waiting)
	}
	if fmt.Sprint(rep.Assemble) != "[[21] [25]]" {
		t.Errorf("would assemble %v", rep.Assemble)
	}
	var b bytes.Buffer
	rep.Write(&b, false)
	for _, s := range []string{"landed-without-verdict", "charged-on-no-verdict", "Train 2 would take 1 lines: [25]", "not open"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("the report lacks %q:\n%s", s, b.String())
		}
	}
}

// TestShadowReadsAloneHoldsAndParks: the live queue's lines come as line
// files, and a line that must ride alone, a held line and a parked one are
// read as such: none of them joins another line's train.
func TestShadowReadsAloneHoldsAndParks(t *testing.T) {
	r := &reader{changes: map[state.ChangeID]adapter.ChangeInfo{}}
	for _, id := range []state.ChangeID{"31", "32", "33", "34", "35", "36", "37"} {
		r.changes[id] = adapter.ChangeInfo{ID: id, Head: state.Head("h" + id), Open: true, Approved: true}
	}
	r.changes["38"] = adapter.ChangeInfo{ID: "38", Head: "h38", Open: false, Approved: true}
	dir := t.TempDir()
	for name, body := range map[string]string{
		store.WaitingFile: "b 31\nb 32 alone\nb 33\nb 34\nb 38\nb 35\n",
		store.HeldFile:    "b 33 -- waiting for a review\n",
		store.ParkedFile:  "park p1 -- landed:99 -- trunk is red\nb 36\nb 37 alone\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lf, err := store.ReadLines(dir)
	if err != nil {
		t.Fatal(err)
	}
	rep := Run(context.Background(), Input{Lines: lf, Read: r, Depth: 3, MaxLines: 8})
	gates := map[string]string{}
	for _, l := range rep.Waiting {
		gates[fmt.Sprint(l.Changes)] = l.Gate
		if fmt.Sprint(l.Changes) == "[32]" && !l.Alone {
			t.Errorf("32 lost its alone flag")
		}
	}
	want := map[string]string{"[31]": "waiting", "[32]": "waiting", "[33]": "held", "[34]": "waiting",
		"[38]": "refused", "[35]": "waiting", "[36]": "parked", "[37]": "parked"}
	if fmt.Sprint(gates) != fmt.Sprint(want) {
		t.Errorf("gates %v, want %v", gates, want)
	}
	// 31, 34 and 35 ride together; 32 rides alone after them; 33 is held
	// and 36, 37 are parked.
	if fmt.Sprint(rep.Assemble) != "[[31 34 35] [32]]" {
		t.Errorf("would assemble %v", rep.Assemble)
	}
}
