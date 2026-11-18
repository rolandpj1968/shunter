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
)

// reader is a read-only forge and CI with canned answers.
type reader struct {
	reports map[state.Head]adapter.Outcome
	changes map[state.ChangeID]adapter.ChangeInfo
	reads   int
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

func (r *reader) Status(_ context.Context, _ state.TrainID, head state.Head, _ []adapter.BuildLine) (adapter.Report, error) {
	r.reads++
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
		},
	}
	waiting, err := ParseLines(strings.NewReader("b 21 22\n# a comment\n\nb 23\nb 24\nb #25\ns 26\n"))
	if err != nil {
		t.Fatal(err)
	}
	rep := Run(context.Background(), Input{Logs: lgs, Landed: landed, Waiting: waiting, Read: r, Depth: 2, MaxLines: 1})

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

func TestParseLines(t *testing.T) {
	got, err := ParseLines(strings.NewReader("b 1 2 3\n4 5\nx\n#\n"))
	if err != nil || fmt.Sprint(got) != "[[1 2 3] [4 5]]" {
		t.Fatalf("%v %v", got, err)
	}
}
