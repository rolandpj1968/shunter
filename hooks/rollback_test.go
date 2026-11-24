package hooks_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRollbackRehearsal rehearses examples/rollback.sh (the review's
// done-when for B9) on a daemon on fake adapters with trains riding, lines
// waiting, one alone and one held: the trains are cancelled with requeue,
// the daemon stopped, the line files exported and mapped into an old
// queue's lists, every line found there once, and the old queue started,
// all well inside ten minutes. The export round-trips: imported into a
// fresh daemon and exported again, it is the same.
func TestRollbackRehearsal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the daemon")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	dir := t.TempDir()
	d := serveFake(t, dir, "a", "c", "d", "e", "f", "g")
	for _, id := range []string{"a", "c", "d", "e"} {
		d.shunt(t, "queue", id)
	}
	d.shunt(t, "queue", "f", "--alone")
	d.shunt(t, "queue", "g")
	d.shunt(t, "hold", "g", "--reason", "waits for a decision")
	waitFor(t, func() bool { return strings.Contains(d.shunt(t, "status"), "train 2") })

	old := filepath.Join(dir, "old")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	// The old queue: a waiting list and an alone list, and a hold file.
	mapScript := filepath.Join(dir, "map.sh")
	if err := os.WriteFile(mapScript, []byte(`#!/usr/bin/env bash
set -eu
old=$1; out=$2
grep -E '^b ' "$out/waiting.list" | grep -v ' alone$' > "$old/queue.list" || true
grep -E '^b .* alone$' "$out/waiting.list" | sed 's/ alone$//' > "$old/next-alone.list" || true
sed -n 's/^b \(.*\) -- .*/\1/p' "$out/held.list" > "$old/queue.hold" 2>/dev/null || true
`), 0o755); err != nil {
		t.Fatal(err)
	}
	pid := d.srv.Process.Pid
	cmd := exec.Command("bash", "../examples/rollback.sh", filepath.Join(dir, "out"))
	cmd.Env = append(os.Environ(),
		"SHUNT="+d.bin, "SHUNTER_SOCKET="+d.sock, "SHUNTER_STORE="+d.store,
		"STOP_CMD=kill -INT "+itoa(pid)+"; while kill -0 "+itoa(pid)+" 2>/dev/null; do sleep 0.2; done",
		"MAP_CMD="+mapScript+" "+old,
		"CHECK_CMD=cat "+old+"/queue.list "+old+"/next-alone.list",
		"START_CMD=touch "+old+"/started")
	start := time.Now()
	out, err := cmd.CombinedOutput()
	t.Logf("rollback.sh, %s:\n%s", time.Since(start).Round(time.Millisecond), out)
	if err != nil {
		t.Fatalf("rollback.sh: %v", err)
	}
	if !regexp.MustCompile(`done in \d+s\n`).Match(out) || strings.Contains(string(out), "OVER") {
		t.Fatal("the rollback did not finish inside its budget")
	}
	if _, err := os.Stat(filepath.Join(old, "started")); err != nil {
		t.Fatal("the old queue was not started")
	}
	q, _ := os.ReadFile(filepath.Join(old, "queue.list"))
	a, _ := os.ReadFile(filepath.Join(old, "next-alone.list"))
	h, _ := os.ReadFile(filepath.Join(old, "queue.hold"))
	if n := strings.Count(string(q), "\n"); n != 5 || strings.TrimSpace(string(a)) != "b f" || strings.TrimSpace(string(h)) != "g" {
		t.Fatalf("the old queue: %d waiting lines\n%s\nalone %q, held %q", n, q, a, h)
	}

	// The round trip: the export, imported into a fresh daemon, exports the same.
	d2 := serveFake(t, t.TempDir(), "a", "c", "d", "e", "f", "g")
	d2.shunt(t, "pause", "--reason", "round trip")
	d2.shunt(t, "import", "--line-files", filepath.Join(dir, "out"))
	again := filepath.Join(dir, "again")
	d2.shunt(t, "export", "--line-files", again)
	for _, f := range []string{"waiting.list", "held.list"} {
		x, _ := os.ReadFile(filepath.Join(dir, "out", f))
		y, _ := os.ReadFile(filepath.Join(again, f))
		if lines(x) != lines(y) {
			t.Errorf("%s does not round-trip:\n%s\n--\n%s", f, x, y)
		}
	}
}

func lines(b []byte) string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func itoa(n int) string { return strconv.Itoa(n) }

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timed out")
}
