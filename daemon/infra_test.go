package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/bound"
	"github.com/rolandpj1968/shunter/daemon"
	"github.com/rolandpj1968/shunter/state"
	"github.com/rolandpj1968/shunter/store"
)

// http500 is the forge's intermittent answer in a partial outage: transient.
func http500() error {
	return &bound.HTTPError{Method: "POST", Path: "/repos/o/r/pulls", Status: 500}
}

// TestOpenPRRidesThroughIntermittent500s: a pull request that the forge
// refuses with 500 several times, and then opens, opens; the train tests,
// and nobody is asked anything.
func TestOpenPRRidesThroughIntermittent500s(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.w.OpenPRFaults = []error{http500(), http500(), http500(), http500(), http500()}
	r.queue("a")
	tr := r.trainOf("a")
	r.wantTrain(tr, state.TrainBuilt)
	for i := 0; i < 8 && r.train(tr).State == state.TrainBuilt; i++ {
		r.tick(20 * time.Second) // past each growing, jittered wait
	}
	r.wantTrain(tr, state.TrainTesting)
	if r.w.PR(tr) == "" {
		t.Fatal("no pull request was opened")
	}
	if n := len(r.w.NoticesTo("judge", "")); n != 0 {
		t.Fatalf("the judge was told %d times: %v", n, r.w.NoticesTo("judge", ""))
	}
	r.w.Pass(tr)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
}

// TestPersistent500EndsAsNoVerdictAtTheDeadline: a forge that answers 500
// past the train's deadline ends the train with no verdict, by
// infrastructure: not J1, not a failure, not a charge.
func TestPersistent500EndsAsNoVerdictAtTheDeadline(t *testing.T) {
	r := newRig(t)
	r.open("a")
	for i := 0; i < 1000; i++ {
		r.w.OpenPRFaults = append(r.w.OpenPRFaults, http500())
	}
	r.queue("a")
	tr := r.trainOf("a")
	for i := 0; i < 12 && r.train(tr).State == state.TrainBuilt; i++ {
		r.tick(time.Minute)
	}
	r.wantTrain(tr, state.TrainVoid)
	s := r.state()
	if !strings.Contains(s.Trains[tr].Reason, "no pull request by the deadline") {
		t.Fatalf("void for %q", s.Trains[tr].Reason)
	}
	if s.Lines["a"].ChargedAt != nil || len(r.w.NoticesTo("judge", "J1")) != 0 {
		t.Fatalf("charged at %v; J1 %v", s.Lines["a"].ChargedAt, r.w.NoticesTo("judge", "J1"))
	}
	if st := r.line("a"); st != state.LineRiding && st != state.LineWaiting {
		t.Fatalf("line a is %s, not offered again", st)
	}
}

// TestRequeueByHandAtJ1: a one-line train at J1 whose failure was not its
// line's (the pre-flight could not reach the forge) has no rerun (it has no
// head) and no split. The operator's requeue aborts it, and its line waits
// again: uncharged, nothing parked, its budgets reset; it rides and lands.
func TestRequeueByHandAtJ1(t *testing.T) {
	r := newRig(t)
	r.open("a", "b")
	r.w.BuildHook = func(ctx context.Context, req adapter.BuildRequest) (adapter.BuildResult, error) {
		return adapter.BuildResult{PreflightFailed: true, Evidence: "the pre-flight could not reach the forge"}, nil
	}
	r.queue("a")
	tr := r.trainOf("a")
	r.wantTrain(tr, state.TrainJudging)
	r.w.BuildHook = nil
	msg := r.execErr(daemon.Request{Cmd: "judge", Args: []string{string(tr), "rerun"}, Reason: "the forge's outage"})
	if !strings.Contains(msg, "no head") || !strings.Contains(msg, "requeue") {
		t.Fatalf("rerun refused with %q", msg)
	}
	if r.execErr(daemon.Request{Cmd: "requeue", Args: []string{string(tr)}}) == "" {
		t.Fatal("a requeue with no reason was taken")
	}
	r.exec(daemon.Request{Cmd: "requeue", Args: []string{string(tr)}, Reason: "the forge's outage, not a's"})
	r.wantTrain(tr, state.TrainAborted)
	s := r.state()
	if s.Paused || len(s.Parks) != 0 || s.Lines["a"].NoVerdicts != 0 || s.Lines["a"].ChargedAt != nil {
		t.Fatalf("paused %v, parks %v, line %+v", s.Paused, s.Parks, s.Lines["a"])
	}
	r.queue("b")
	ta := r.trainOf("a")
	if ta == tr {
		t.Fatal("a did not ride again")
	}
	r.w.Pass(ta)
	if tb := r.trainOf("b"); tb != ta {
		r.w.Pass(tb)
	}
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
	r.wantLine("b", state.LineLanded)
	recs, err := store.ReadLog(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range recs {
		found = found || (rec.Kind == store.KindApplied && rec.Event.Type == "requeue")
	}
	if !found {
		t.Fatal("the requeue is not in the store's log")
	}
}

// TestRequeueByHandOfAChargedLine: a line charged for a failure that was
// not its own is put back by its name: it waits again, its charge gone,
// and rides on the same heads.
func TestRequeueByHandOfAChargedLine(t *testing.T) {
	r := newRig(t)
	r.open("a")
	r.queue("a")
	tr := r.trainOf("a")
	r.w.Fail(tr, "the runner lost its disk in a/a.go", "a")
	r.tick(time.Second)
	r.wantLine("a", state.LineReturned)
	r.exec(daemon.Request{Cmd: "requeue", Args: []string{"a"}, Reason: "the runner's disk, not a's"})
	t2 := r.trainOf("a")
	if t2 == "" || t2 == tr {
		t.Fatalf("a rides %q", t2)
	}
	r.w.Pass(t2)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
}

// TestPreflightNoVerdictIsInfra: a pre-flight that says it could not judge
// ends the train with no verdict, by infrastructure: no J1, no charge, and
// the line rides again.
func TestPreflightNoVerdictIsInfra(t *testing.T) {
	r := newRig(t)
	r.open("a")
	once := true
	r.w.BuildHook = func(ctx context.Context, req adapter.BuildRequest) (adapter.BuildResult, error) {
		if once {
			once = false
			return adapter.BuildResult{NoVerdict: "the image registry answered 503"}, nil
		}
		return adapter.BuildResult{}, nil
	}
	r.queue("a")
	s := r.state()
	var void *state.Train
	for _, t := range s.Trains {
		if t.State == state.TrainVoid {
			void = t
		}
	}
	if void == nil || !strings.Contains(void.Reason, "503") {
		t.Fatalf("no train void for the pre-flight's no verdict: %v", s.Trains)
	}
	if s.Lines["a"].ChargedAt != nil || len(r.w.NoticesTo("judge", "")) != 0 {
		t.Fatalf("charged at %v, judge %v", s.Lines["a"].ChargedAt, r.w.NoticesTo("judge", ""))
	}
	tr := r.trainOf("a")
	r.wantTrain(tr, state.TrainTesting)
	r.w.Pass(tr)
	r.tick(time.Second)
	r.wantLine("a", state.LineLanded)
}
