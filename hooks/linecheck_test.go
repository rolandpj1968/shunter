package hooks_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeShunt answers the worker's calls as the CLI would, and records them:
// a claim prints the file "claim" (empty: nothing due), refused as another
// version until "checker" is called when the file "refuse" exists.
const fakeShunt = `import json, os, sys
d = os.environ["FAKE_DIR"]
with open(os.path.join(d, "calls.jsonl"), "a") as f:
    f.write(json.dumps({"args": sys.argv[1:], "actor": os.environ.get("SHUNTER_ACTOR", "")}) + "\n")
v = sys.argv[1]
if v == "claim":
    if os.path.exists(os.path.join(d, "refuse")) and not os.path.exists(os.path.join(d, "declared")):
        sys.stderr.write('shunt: the line check is at version "old", not "x": its verdict would not be recorded\n')
        sys.exit(1)
    p = os.path.join(d, "claim")
    print(open(p).read().strip() if os.path.exists(p) else "")
elif v == "checker":
    open(os.path.join(d, "declared"), "w").close()
`

// fakeCheck is a site's check that answers as CHECK_MODE says, and keeps
// its input.
const fakeCheck = `import json, os, sys, time
doc = json.load(sys.stdin)
open(os.path.join(os.environ["FAKE_DIR"], "input.json"), "w").write(json.dumps(doc))
m = os.environ.get("CHECK_MODE", "pass")
if m == "pass": print(json.dumps({"verdict": "pass"}))
elif m == "fail": print(json.dumps({"verdict": "fail", "reason": "TestX fails: want 1, got 2"}))
elif m == "fail-silent": print(json.dumps({"verdict": "fail"}))
elif m == "conflict": print(json.dumps({"verdict": "conflict", "reason": "api/x.yaml"}))
elif m == "infra": print(json.dumps({"verdict": "infra", "reason": "the CI host refused the dispatch"}))
elif m == "crash": sys.exit(3)
elif m == "garbage": print("ok")
elif m == "sleep": time.sleep(30)
`

type workerRun struct {
	calls []struct {
		Args  []string `json:"args"`
		Actor string   `json:"actor"`
	}
	input map[string]any
	code  int
}

// runWorker runs linecheck.py once against the fake shunt and check.
func runWorker(t *testing.T, mode, claim string, refuse bool, cfgExtra map[string]any) workerRun {
	t.Helper()
	py := python(t)
	dir := t.TempDir()
	for name, src := range map[string]string{"shunt.py": fakeShunt, "check.py": fakeCheck} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if claim != "" {
		if err := os.WriteFile(filepath.Join(dir, "claim"), []byte(claim), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if refuse {
		if err := os.WriteFile(filepath.Join(dir, "refuse"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]any{
		"shunt": []string{py, "-B", filepath.Join(dir, "shunt.py")}, "actor": "w1",
		"command": []string{py, "-B", filepath.Join(dir, "check.py")}, "version": "v1", "lease": "3h", "timeout": "20m",
	}
	for k, v := range cfgExtra {
		cfg[k] = v
	}
	cfgPath := filepath.Join(dir, "linecheck.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs("linecheck.py")
	cmd := exec.Command(py, "-B", abs, "--config", cfgPath)
	cmd.Env = append(os.Environ(), "FAKE_DIR="+dir, "CHECK_MODE="+mode, "PYTHONDONTWRITEBYTECODE=1")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	var r workerRun
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("linecheck.py: %v\n%s", err, errb.String())
		}
		r.code = ee.ExitCode()
	}
	f, err := os.Open(filepath.Join(dir, "calls.jsonl"))
	if err != nil {
		t.Fatalf("no calls: %v\n%s", err, errb.String())
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c struct {
			Args  []string `json:"args"`
			Actor string   `json:"actor"`
		}
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		r.calls = append(r.calls, c)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "input.json")); err == nil {
		if err := json.Unmarshal(b, &r.input); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r workerRun) answer(t *testing.T) []string {
	t.Helper()
	last := r.calls[len(r.calls)-1]
	if last.Args[0] != "checked" {
		t.Fatalf("no answer: the calls were %v", r.calls)
	}
	return last.Args
}

func flag(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// The worker claims under its version and lease, as its actor, runs the
// check on the claim's key, and answers each verdict on that key.
func TestLineCheckWorkerAnswersEachVerdict(t *testing.T) {
	r := runWorker(t, "pass", "a T0 a-1 a2-1", false, nil)
	claim := r.calls[0]
	if claim.Args[0] != "claim" || flag(claim.Args, "--version") != "v1" || flag(claim.Args, "--lease") != "3h" || claim.Actor != "w1" {
		t.Fatalf("the claim: %+v", claim)
	}
	if got := strs(r.input["heads"]); r.input["line"] != "a" || r.input["trunk"] != "T0" || strings.Join(got, " ") != "a-1 a2-1" || r.input["version"] != "v1" {
		t.Fatalf("the check's input: %v", r.input)
	}
	if a := r.answer(t); strings.Join(a[:5], " ") != "checked a T0 a-1 a2-1" || flag(a, "--action") != "pass" || flag(a, "--version") != "v1" {
		t.Fatalf("pass answered %v", a)
	}
	for mode, want := range map[string][2]string{
		"fail":        {"fail", "TestX fails: want 1, got 2"},
		"conflict":    {"conflict", "api/x.yaml"},
		"infra":       {"infra", "the CI host refused the dispatch"},
		"crash":       {"infra", "the check printed no JSON answer (exit 3)"},
		"garbage":     {"infra", "the check printed no JSON answer (exit 0)"},
		"fail-silent": {"infra", "the check answered fail with no reason: nothing is judged on what isn't stated"},
	} {
		a := runWorker(t, mode, "a T0 a-1", false, nil).answer(t)
		if flag(a, "--action") != want[0] || flag(a, "--reason") != want[1] {
			t.Errorf("%s answered %v", mode, a)
		}
	}
}

// A check past its timeout has judged nothing: infra, promptly.
func TestLineCheckWorkerTimesACheckOut(t *testing.T) {
	start := time.Now()
	a := runWorker(t, "sleep", "a T0 a-1", false, map[string]any{"timeout": "1s"}).answer(t)
	if flag(a, "--action") != "infra" || !strings.Contains(flag(a, "--reason"), "past its timeout") {
		t.Fatalf("a hung check answered %v", a)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("the worker waited out the hung check")
	}
}

// Nothing due: no check runs and nothing is answered.
func TestLineCheckWorkerWithNothingDue(t *testing.T) {
	r := runWorker(t, "pass", "", false, nil)
	if len(r.calls) != 1 || r.input != nil || r.code != 0 {
		t.Fatalf("with nothing due: calls %v, input %v, exit %d", r.calls, r.input, r.code)
	}
}

// A queue at another line-check version: the worker declares its own once
// and claims again; with "declare": false it stops, and says so.
func TestLineCheckWorkerDeclaresItsVersion(t *testing.T) {
	r := runWorker(t, "pass", "a T0 a-1", true, nil)
	var verbs []string
	for _, c := range r.calls {
		verbs = append(verbs, c.Args[0])
	}
	if strings.Join(verbs, " ") != "claim checker claim checked" || strings.Join(r.calls[1].Args, " ") != "checker line-check v1" {
		t.Fatalf("the calls: %v", r.calls)
	}
	r = runWorker(t, "pass", "a T0 a-1", true, map[string]any{"declare": false})
	if len(r.calls) != 1 || r.code != 1 {
		t.Fatalf("with declare off: calls %v, exit %d", r.calls, r.code)
	}
}

// TestLineCheckWorkerDryRun runs the worker against a real daemon on fake
// adapters (the review's done-when for B2): a pass, a red, a conflict, an
// infra answer and a lease that runs out are each seen in the store.
func TestLineCheckWorkerDryRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the daemon")
	}
	py := python(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "shunter")
	if out, err := exec.Command("go", "build", "-o", bin, "../cmd/shunter").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	store, sock := filepath.Join(dir, "store"), filepath.Join(dir, "s.sock")
	srv := exec.Command(bin, "serve", "--store", store, "--socket", sock, "--adapters", "fake", "--trunk", "T0", "--open", "a,b,c,d,e")
	var srvLog bytes.Buffer
	srv.Stdout, srv.Stderr = &srvLog, &srvLog
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Signal(os.Interrupt)
		_ = srv.Wait()
	})
	for i := 0; ; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		if i > 100 {
			t.Fatalf("the daemon never listened:\n%s", srvLog.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	shunt := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(bin, append(args, "--socket", sock)...).CombinedOutput()
		if err != nil {
			t.Fatalf("shunt %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	shunt("pause", "--reason", "a dry run: nothing rides")
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		shunt("queue", id)
	}
	// The site's check: by line, as a dry run's lines are named.
	check := filepath.Join(dir, "check.py")
	src := `import json, sys, time
doc = json.load(sys.stdin)
l = doc["line"]
if l == "e": time.sleep(8)
print(json.dumps({"a": {"verdict": "pass"}, "b": {"verdict": "fail", "reason": "TestB fails"},
    "c": {"verdict": "conflict", "reason": "gen/x.go"}, "d": {"verdict": "infra", "reason": "no runner"},
    "e": {"verdict": "pass"}}[l]))
`
	if err := os.WriteFile(check, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs("linecheck.py")
	work := func(actor, lease string) {
		t.Helper()
		cfg := filepath.Join(dir, actor+".json")
		b, _ := json.Marshal(map[string]any{"shunt": []string{bin}, "socket": sock, "command": []string{py, "-B", check},
			"version_files": []string{"check.py"}, "lease": lease, "timeout": "30s"})
		if err := os.WriteFile(cfg, b, 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(py, "-B", abs, "--config", cfg)
		cmd.Env = append(os.Environ(), "SHUNTER_ACTOR="+actor, "PYTHONDONTWRITEBYTECODE=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("worker %s: %v\n%s", actor, err, out)
		}
	}
	// Lines are claimed oldest first: a to d answer inside their lease; e's
	// check outlasts a lease of 1s, which the daemon's timer ends, and its
	// late verdict on the same key still counts.
	for _, w := range []string{"w1", "w2", "w3", "w4"} {
		work(w, "1m")
	}
	work("w5", "1s")
	st := shunt("status")
	logb, err := os.ReadFile(filepath.Join(store, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(logb)
	for _, want := range []string{
		`"line-checked"`, "TestB fails", "gen/x.go",
		`"claim-released"`, "the line check of d by w4 gave no verdict: no runner",
		`"claim-expired"`, "the line check of e claimed by w5 ran past its lease",
		`"checker-changed"`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the store's log has no %q", want)
		}
	}
	for _, id := range []string{"b", "c"} {
		if regexp.MustCompile(`(?m)^waiting\s+` + id + `$`).MatchString(st) {
			t.Errorf("line %s still waits after its %s", id, map[string]string{"b": "red", "c": "conflict"}[id])
		}
	}
	if !regexp.MustCompile(`(?m)^waiting\s+e$`).MatchString(st) || !regexp.MustCompile(`(?m)^waiting\s+d$`).MatchString(st) {
		t.Error("d or e no longer waits")
	}
	if t.Failed() {
		t.Logf("status:\n%s\nlog:\n%s\ndaemon:\n%s", st, log, srvLog.String())
	}
}
