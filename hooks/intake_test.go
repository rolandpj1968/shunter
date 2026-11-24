package hooks_test

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// daemonRig is the daemon built and served on fake adapters, with the
// changes given open on trunk, for a dry run of a worker against it.
type daemonRig struct {
	bin, store, sock string
	srv              *exec.Cmd
	log              *bytes.Buffer
}

func serveFake(t *testing.T, dir string, open ...string) *daemonRig {
	t.Helper()
	r := &daemonRig{bin: filepath.Join(dir, "shunter"), store: filepath.Join(dir, "store"), sock: filepath.Join(dir, "s.sock"), log: &bytes.Buffer{}}
	if out, err := exec.Command("go", "build", "-o", r.bin, "../cmd/shunter").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	r.srv = exec.Command(r.bin, "serve", "--store", r.store, "--socket", r.sock, "--adapters", "fake", "--trunk", "T0", "--open", strings.Join(open, ","))
	r.srv.Stdout, r.srv.Stderr = r.log, r.log
	if err := r.srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.stop)
	for i := 0; ; i++ {
		if c, err := net.Dial("unix", r.sock); err == nil {
			c.Close()
			return r
		}
		if i > 100 {
			t.Fatalf("the daemon never listened:\n%s", r.log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (r *daemonRig) stop() {
	if r.srv.ProcessState == nil {
		_ = r.srv.Process.Signal(os.Interrupt)
		_ = r.srv.Wait()
	}
}

func (r *daemonRig) shunt(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(r.bin, append(args, "--socket", r.sock)...).CombinedOutput()
	if err != nil {
		t.Fatalf("shunt %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestIntakeDryRun offers a producers' lines file to a real daemon (the
// review's done-when for B8): each new name is offered once, as its owner,
// with --alone where the row says so; its answer file says QUEUED, or
// REFUSED with the reason; a daemon that is down is no answer, and the row
// is offered on the next run.
func TestIntakeDryRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the daemon")
	}
	py := python(t)
	dir := t.TempDir()
	d := serveFake(t, dir, "a", "d", "c")
	d.shunt(t, "pause", "--reason", "a dry run: nothing rides")
	lines, answers := filepath.Join(dir, "offers.list"), filepath.Join(dir, "lines")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(lines, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("# producers append here\np1-1 b a\np2-2 b d alone\np1-3 b a\np3-4 b z\n")
	cfg := filepath.Join(dir, "intake.json")
	b, _ := json.Marshal(map[string]any{"shunt": []string{d.bin}, "socket": d.sock, "lines": lines, "answers": answers,
		"drop": []string{"b"}, "owner": `^(.+)-[0-9]+$`})
	if err := os.WriteFile(cfg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs("intake.py")
	run := func() (int, string) {
		t.Helper()
		cmd := exec.Command(py, "-B", abs, "--config", cfg)
		cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		} else if err != nil {
			t.Fatal(err)
		}
		return 0, string(out)
	}
	answer := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(answers, name+".done"))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	if code, out := run(); code != 0 {
		t.Fatalf("intake: exit %d\n%s", code, out)
	}
	if got := answer("p1-1"); got != "QUEUED b a" {
		t.Errorf("p1-1: %q", got)
	}
	if got := answer("p2-2"); got != "QUEUED b d" {
		t.Errorf("p2-2: %q", got)
	}
	if got := answer("p1-3"); !strings.HasPrefix(got, "REFUSED\nrefused: offer:") {
		t.Errorf("a change queued twice: %q", got)
	}
	if got := answer("p3-4"); !strings.HasPrefix(got, "REFUSED\nrefused:") || !strings.Contains(got, "z") {
		t.Errorf("a change the forge doesn't know: %q", got)
	}
	logb, err := os.ReadFile(filepath.Join(d.store, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(logb)
	for _, want := range []string{`"By":"p1","Line":"a"`, `"By":"p2","Line":"d"`, `"Alone":true`} {
		if !strings.Contains(log, want) {
			t.Errorf("the store's log has no %s", want)
		}
	}
	if strings.Count(log, `"kind":"applied","event":{"type":"offer"`) != 2 {
		t.Errorf("offers in the log: %d, want 2", strings.Count(log, `"kind":"applied","event":{"type":"offer"`))
	}
	// Answered names are not offered again; a down daemon answers nothing.
	d.stop()
	write("p1-1 b a\np2-2 b d alone\np1-3 b a\np3-4 b z\np4-5 b c\n")
	if code, _ := run(); code != 1 || answer("p4-5") != "" {
		t.Fatalf("with the daemon down: exit %d, answer %q", code, answer("p4-5"))
	}
	d2 := serveFake(t, t.TempDir(), "c")
	b, _ = json.Marshal(map[string]any{"shunt": []string{d2.bin}, "socket": d2.sock, "lines": lines, "answers": answers,
		"drop": []string{"b"}, "owner": `^(.+)-[0-9]+$`})
	if err := os.WriteFile(cfg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := run(); code != 0 || answer("p4-5") != "QUEUED b c" {
		t.Fatalf("next run: exit %d, answer %q\n%s", code, answer("p4-5"), out)
	}
}
