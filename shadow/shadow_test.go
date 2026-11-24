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
	"github.com/rolandpj1968/shunter/replay"
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
			h('b'): adapter.Passed, h('f'): adapter.Passed, h('8'): adapter.Passed, h('9'): adapter.Passed,
			h('c'): adapter.Failed, // landed by the live queue, and the checks on its head failed
			h('3'): adapter.Failed, h('6'): adapter.Failed,
			h('5'): adapter.NoVerdict, // the live queue charged a cancelled run
			h('7'): adapter.Pending, h('d'): adapter.Pending, h('e'): adapter.Pending,
		},
		changes: map[state.ChangeID]adapter.ChangeInfo{
			"21":  {ID: "21", Head: "h21", Open: true, Approved: true},
			"22":  {ID: "22", Head: "h22", Base: "21", Open: true, Approved: true},
			"23":  {ID: "23", Head: "h23", Open: false, Approved: true},
			"24":  {ID: "24", Head: "h24", Open: true, Approved: false},
			"25":  {ID: "25", Head: "h25", Open: true, Approved: true},
			"26":  {ID: "26", Head: "h26", Base: "99", Open: true, Approved: true},
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
		"918": "agree",
		"917": "agree", // opened as a draft at assembly; its checks are pending
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
	if got := fmt.Sprint(r.lines[h('3')]); got != "[{105 [{105 }] } {106 [{106 h106}] } {107 [{107 }] }]" {
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

// TestShadowLabelsWhatARuleExplains: a divergence or a refusal that follows
// from a rule of Shunter's is counted as intended and names the rule; only
// the rest is reported line by line, and each unexplained refusal says
// where and why.
func TestShadowLabelsWhatARuleExplains(t *testing.T) {
	lgs, landed := logs(t)
	r := &reader{reports: map[state.Head]adapter.Outcome{
		h('3'): adapter.Failed, h('5'): adapter.NoVerdict, h('6'): adapter.Failed, h('c'): adapter.Failed,
	}}
	rep := Run(context.Background(), Input{Logs: lgs, Landed: landed, Read: r, Depth: 2, MaxLines: 1})
	if rep.Intended["unblamed-without-judge"] != 1 || rep.Intended["charged-on-no-verdict"] != 1 {
		t.Errorf("intended %v", rep.Intended)
	}
	for k := range rep.Diverge {
		if intended[k] != "" {
			t.Errorf("an intended kind counted as unexplained: %s", k)
		}
	}
	for _, tr := range rep.Trains {
		if (tr.Rule != "") != (intended[tr.Kind] != "") {
			t.Errorf("train %s: kind %s, rule %q", tr.PR, tr.Kind, tr.Rule)
		}
	}
	rules := map[string]string{}
	for _, f := range rep.Refusals {
		rules[f.Segment] = f.Rule
	}
	if !strings.HasPrefix(rules["05-unblamed-one.out"], "I6") || rules["08-wrong-head.out"] != "" {
		t.Errorf("refusals %+v", rep.Refusals)
	}
	if _, ok := rep.Refused["judge: "+replay.Kind(state.ErrEvidence)]; ok {
		t.Errorf("an intended refusal counted as unexplained: %v", rep.Refused)
	}
	var b bytes.Buffer
	rep.Write(&b, false)
	out := b.String()
	for _, s := range []string{
		"intended: unblamed-without-judge 1  I6, J1",
		"refused in 08-wrong-head.out: ci-passed:",
		"judge: I6: a charge needs evidence",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("the report lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "refused in 05-unblamed-one.out") || strings.Contains(out, "906    head") {
		t.Errorf("the report lists an intended item:\n%s", out)
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

// TestShadowGateIsFedLineChecksAndOwners: with RequireCheck, Shunter's gate
// takes a waiting line only on a green line check for the heads it has now,
// and a line whose change is with its owner is not in the queue at all. Each
// line's answer is compared with the live queue's own, and every difference
// is counted and marked.
func TestShadowGateIsFedLineChecksAndOwners(t *testing.T) {
	ch := func(id string) adapter.ChangeInfo {
		return adapter.ChangeInfo{ID: state.ChangeID(id), Head: state.Head("h" + id), Open: true, Approved: true}
	}
	r := &reader{changes: map[state.ChangeID]adapter.ChangeInfo{}}
	for _, id := range []string{"31", "32", "33", "34", "35", "36", "37", "38", "39"} {
		r.changes[state.ChangeID(id)] = ch(id)
	}
	waiting, err := store.ParseWaiting(strings.NewReader("b 31\nb 32\nb 33\nb 34\nb 35\nb 36 37\nb 38\nb 39\n"))
	if err != nil {
		t.Fatal(err)
	}
	one := func(id, head, verdict, ev string) CheckFact {
		return CheckFact{Changes: []state.ChangeID{state.ChangeID(id)}, Heads: []state.Head{state.Head(head)}, Trunk: "T", Verdict: verdict, Evidence: ev}
	}
	in := Input{Lines: store.LineFile{Waiting: waiting}, Read: r, Depth: 2, MaxLines: 8, RequireCheck: true,
		Checks: []CheckFact{
			one("31", "h31", "pass", ""),
			one("33", "h33", "fail", "lint"),
			one("34", "h34", "conflict", "x.go"),
			one("36", "h36", "pass", ""), one("37", "h37", "pass", ""), // a composite line, each part green alone
			one("38", "old", "pass", ""),                                   // green on a head since replaced
			one("39", "h39", "fail", "lint"), one("39", "h39", "pass", ""), // the last verdict for the heads counts
		},
		Owners: []OwnerFact{{Change: "35", Owner: "ann", Fixing: true, Why: "marked as being fixed"}, {Change: "31", Owner: "bob"}},
		Live: []LiveFact{
			{Changes: []state.ChangeID{"31"}, Takes: true, Why: "green"},
			{Changes: []state.ChangeID{"32"}, Takes: true, Why: "the gate was open"}, // the live queue takes an unchecked line
			{Changes: []state.ChangeID{"33"}, Takes: false, Why: "red"},
			{Changes: []state.ChangeID{"34"}, Takes: false, Why: "conflict"},
			{Changes: []state.ChangeID{"35"}, Takes: true, Why: "green"}, // the live queue takes a line its owner is fixing
			{Changes: []state.ChangeID{"36", "37"}, Takes: true, Why: "green"},
			{Changes: []state.ChangeID{"38"}, Takes: false, Why: "stale"},
			// no answer for 39
		},
	}
	rep := Run(context.Background(), in)
	type want struct {
		gate, check string
		takes       bool
		compare     string
		whyHas      string
	}
	wants := map[string]want{
		"[31]":    {"waiting", "green", true, "agree", ""},
		"[32]":    {"waiting", "none", false, "diverge", "no green line check"},
		"[33]":    {"charged", "charged", false, "agree", "lint"},
		"[34]":    {"waiting", "conflict", false, "agree", "x.go"},
		"[35]":    {"with-owner", "", false, "diverge", "ann"},
		"[36 37]": {"waiting", "green", true, "agree", ""},
		"[38]":    {"waiting", "none", false, "agree", "no green line check"},
		"[39]":    {"waiting", "green", true, "", ""},
	}
	if len(rep.Waiting) != len(wants) {
		t.Fatalf("%d lines reported, want %d: %+v", len(rep.Waiting), len(wants), rep.Waiting)
	}
	for _, l := range rep.Waiting {
		w, ok := wants[fmt.Sprint(l.Changes)]
		if !ok {
			t.Errorf("an unexpected line %v", l.Changes)
			continue
		}
		if l.Gate != w.gate || l.Check != w.check || l.Takes != w.takes || l.Compare != w.compare || !strings.Contains(l.Why, w.whyHas) {
			t.Errorf("%v: gate %q check %q takes %v compare %q why %q; want %+v", l.Changes, l.Gate, l.Check, l.Takes, l.Compare, l.Why, w)
		}
	}
	if rep.Compared != 7 || rep.GateAgree != 5 || rep.GateDiverge != 2 {
		t.Errorf("compared %d, agree %d, diverge %d; want 7, 5, 2", rep.Compared, rep.GateAgree, rep.GateDiverge)
	}
	// Shunter's trains take only what its gate takes.
	took := map[state.LineID]bool{}
	for _, tr := range rep.Assemble {
		for _, id := range tr {
			took[id] = true
		}
	}
	for _, id := range []state.LineID{"32", "33", "34", "35", "38"} {
		if took[id] {
			t.Errorf("line %s is in a train Shunter would assemble", id)
		}
	}
	if !took["31"] || !took["36"] || !took["39"] {
		t.Errorf("the checked lines are not all taken: %v", rep.Assemble)
	}
	var b strings.Builder
	rep.Write(&b, false)
	out := b.String()
	if !strings.Contains(out, "Gate comparison: 7 waiting lines the live queue answered for: agree 5, DIVERGE 2.") ||
		strings.Count(out, " ** ") != 2 || !strings.Contains(out, "DIVERGE\n") {
		t.Errorf("the report:\n%s", out)
	}
	// Without the facts nothing changes: every open line waits and is taken.
	plain := Run(context.Background(), Input{Lines: store.LineFile{Waiting: waiting}, Read: r, Depth: 2, MaxLines: 8})
	for _, l := range plain.Waiting {
		if l.Gate != "waiting" || !l.Takes || l.Check != "" || l.Compare != "" {
			t.Errorf("with no facts, %v: gate %q takes %v check %q compare %q", l.Changes, l.Gate, l.Takes, l.Check, l.Compare)
		}
	}
}
