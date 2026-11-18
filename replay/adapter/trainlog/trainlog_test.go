package trainlog

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rolandpj1968/shunter/replay"
	"github.com/rolandpj1968/shunter/state"
)

func load(t *testing.T) *Source {
	t.Helper()
	paths, err := filepath.Glob("testdata/*.out")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	sort.Strings(paths)
	src := &Source{}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		lg, err := Parse(strings.TrimSuffix(filepath.Base(p), ".out"), f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		src.Logs = append(src.Logs, lg)
	}
	f, err := os.Open("testdata/stats.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	landed, err := LandedPRs(f)
	if err != nil {
		t.Fatal(err)
	}
	src.Landed = func(pr string) bool { return landed[pr] }
	return src
}

func TestParse(t *testing.T) {
	lg, err := Parse("x", strings.NewReader("noise\nBATCH-READY abc 1 2\n  BATCH-GREEN 9 abc\nLEFT-OUT-WHY 3 conflict in a b\nBATCH-UNBLAMED\nBATCHY 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(lg.Markers) != 3 {
		t.Fatalf("markers: %+v", lg.Markers)
	}
	if m := lg.Markers[0]; m.Word != "BATCH-READY" || len(m.Args) != 3 || m.Line != 2 {
		t.Errorf("first marker: %+v", m)
	}
	if m := lg.Markers[1]; m.Word != "LEFT-OUT-WHY" || m.Rest != "3 conflict in a b" {
		t.Errorf("second marker: %+v", m)
	}
}

func TestLandedPRs(t *testing.T) {
	got, err := LandedPRs(strings.NewReader(`{"stage":"landed","batch":"7"}` + "\n\nbroken\n" + `{"stage":"green","batch":"8"}` + "\n"))
	if err != nil || !got["7"] || got["8"] || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
}

// TestReplayFixtures replays the synthetic logs and checks both sides: the
// ordinary histories pass, and each history the protocol forbids is refused
// for the right reason.
func TestReplayFixtures(t *testing.T) {
	src := load(t)
	s, rep := replay.Run(state.New("trunk0", state.Config{}), src)

	if len(rep.Breaches) != 0 {
		t.Fatalf("the machine broke an invariant: %+v", rep.Breaches)
	}
	wantLines := map[state.LineID]state.LineState{
		"101": state.LineLanded, "102": state.LineLanded,
		"103": state.LineReturned, "104": state.LineLanded,
		"105": state.LineLanded, "106": state.LineReturned, "107": state.LineLanded,
		"108":     state.LineWaiting, // charged with no evidence: refused, so not returned
		"109":     state.LineWaiting,
		"110+111": state.LineDropped, "111": state.LineLanded, // a stack, then taken back and offered as one change
		"112": state.LineWaiting, // "green" on a head that was never built
		"114": state.LineLanded, "115": state.LineLanded,
		"116": state.LineWaiting, // green, never landed
		"117": state.LineWaiting, "118": state.LineReturned,
	}
	for id, want := range wantLines {
		l := s.Lines[id]
		if l == nil || l.State != want {
			t.Errorf("line %s: got %v, want %s", id, l, want)
		}
	}
	if s.Lines["109"].Group == 0 {
		t.Errorf("the split left no groups")
	}
	if len(s.Landings) != 5 || s.Trunk != "ffffffffffffffffffffffffffffffffffffffff" {
		t.Errorf("landings %d, trunk %s", len(s.Landings), s.Trunk)
	}

	wantRefused := map[string]error{
		"05-unblamed-one":   state.ErrEvidence, // a lone line sent back with no evidence
		"08-wrong-head":     state.ErrHead,     // a pass on a head nobody built
		"10-dropped-unsaid": state.ErrReason,   // a line dropped from a train with no word
		"12-landed-again":   state.ErrState,    // a landed change riding again
	}
	if len(rep.Refused) != len(wantRefused) {
		t.Errorf("refused %d, want %d: %+v", len(rep.Refused), len(wantRefused), rep.Refused)
	}
	for _, f := range rep.Refused {
		if want := wantRefused[f.Segment]; want == nil || !errors.Is(f.Err, want) {
			t.Errorf("%s: unexpected refusal %v", f.Segment, f.Err)
		}
	}
	wantUnsettled := map[string]state.TrainState{
		"05-unblamed-one": state.TrainJudging, "07-no-verdict": state.TrainTesting,
		"08-wrong-head": state.TrainTesting, "11-green-not-landed": state.TrainGreen,
	}
	if len(rep.Unsettled) != len(wantUnsettled) {
		t.Errorf("unsettled: %+v", rep.Unsettled)
	}
	for _, u := range rep.Unsettled {
		if wantUnsettled[u.Segment] != u.State {
			t.Errorf("%s: unsettled in %s", u.Segment, u.State)
		}
	}
	if len(src.Unrecorded) != 1 || src.Unrecorded[0] != "09-silent" {
		t.Errorf("unrecorded: %v", src.Unrecorded)
	}
	if src.Ignored["BATCH-NEWWORD"] != 1 {
		t.Errorf("ignored: %v", src.Ignored)
	}
	if rep.Segments != 15 {
		t.Errorf("segments: %d", rep.Segments)
	}
	var b strings.Builder
	rep.Write(&b, true)
	if !strings.Contains(b.String(), "charged with no evidence") {
		t.Errorf("report:\n%s", b.String())
	}
}

// TestGreenIsTakenAsLandedWithoutAnOracle covers the default.
func TestGreenIsTakenAsLandedWithoutAnOracle(t *testing.T) {
	src := load(t)
	src.Landed = nil
	s, _ := replay.Run(state.New("trunk0", state.Config{}), src)
	if s.Lines["116"].State != state.LineLanded {
		t.Errorf("line 116 is %s", s.Lines["116"].State)
	}
}
