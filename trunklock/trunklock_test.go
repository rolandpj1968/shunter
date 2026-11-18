//go:build unix

package trunklock

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var cancels []context.CancelFunc

func within(d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	cancels = append(cancels, cancel)
	return ctx
}

func TestOneHolderAtATime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trunk.lock")
	a, err := Acquire(within(time.Second), p, Options{Who: "shunter"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := Inspect(p, Options{})
	if err != nil || !st.Held || st.Holder.PID != os.Getpid() || st.Holder.Who != "shunter" || st.Stale != "" {
		t.Fatalf("inspect while held: %+v %v", st, err)
	}
	if _, err := Acquire(within(300*time.Millisecond), p, Options{Who: "legacy queue"}); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "shunter") {
		t.Fatalf("a second holder: %v", err)
	}
	// Release frees it, and the waiting pusher gets it.
	done := make(chan error, 1)
	go func() {
		b, err := Acquire(within(5*time.Second), p, Options{Who: "legacy queue", Poll: 10 * time.Millisecond})
		if err == nil {
			err = b.Release()
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the waiter: %v", err)
	}
	if st, _ := Inspect(p, Options{}); st.Held || st.Record {
		t.Fatalf("after release: %+v", st)
	}
}

// hold takes the flock on its own open file, as another process would, and
// writes rec as its record.
func hold(t *testing.T, p, rec string) *os.File {
	t.Helper()
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	f.Truncate(0)
	f.WriteAt([]byte(rec), 0)
	t.Cleanup(func() { f.Close() })
	return f
}

func TestStaleByAgeIsReportedNotBroken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trunk.lock")
	host, _ := os.Hostname()
	hold(t, p, Holder{PID: os.Getpid(), Host: host, Who: "legacy queue", Since: time.Now().Add(-time.Hour)}.Format())
	_, err := Acquire(within(200*time.Millisecond), p, Options{MaxHold: 10 * time.Minute})
	if !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("acquire: %v", err)
	}
	st, _ := Inspect(p, Options{MaxHold: 10 * time.Minute})
	if !st.Held || st.Stale == "" || st.Holder.Who != "legacy queue" {
		t.Fatalf("inspect: %+v", st)
	}
	// Within its time, the same holder is only busy.
	if st, _ := Inspect(p, Options{MaxHold: 2 * time.Hour}); st.Stale != "" {
		t.Fatalf("stale within its time: %+v", st)
	}
}

func TestOrphanIsStale(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trunk.lock")
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Skip("no true(1)")
	}
	host, _ := os.Hostname()
	hold(t, p, Holder{PID: gone.Process.Pid, Host: host, Who: "legacy queue", Since: time.Now()}.Format())
	st, err := Inspect(p, Options{})
	if err != nil || !st.Held || !strings.Contains(st.Stale, "is gone") {
		t.Fatalf("inspect: %+v %v", st, err)
	}
	if _, err := Acquire(within(200*time.Millisecond), p, Options{}); !errors.Is(err, ErrStale) {
		t.Fatalf("acquire: %v", err)
	}
}

// A shell script holds the lock with flock(1), as a legacy queue would, and
// dies: the kernel frees the lock, and Shunter takes it.
func TestALegacyFlockHolder(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock(1)")
	}
	p := filepath.Join(t.TempDir(), "trunk.lock")
	cmd := exec.Command("flock", p, "sh", "-c", "echo held; sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() })
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "held" {
		t.Fatalf("flock(1) said %q", line)
	}
	if _, err := Acquire(within(300*time.Millisecond), p, Options{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("while flock(1) holds it: %v", err)
	}
	if st, _ := Inspect(p, Options{}); !st.Held || st.Record || st.Stale != "" {
		t.Fatalf("a holder with no record: %+v", st)
	}
	// Kill the holder, flock(1) and its child, by its process group.
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	cmd.Wait()
	l, err := Acquire(within(5*time.Second), p, Options{Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("after the holder died: %v", err)
	}
	l.Release()
}

func TestRecordFormat(t *testing.T) {
	h := Holder{PID: 42, Host: "build-1", Who: "legacy queue", Since: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	got, ok := ParseHolder(h.Format())
	if !ok || got != h {
		t.Fatalf("%+v", got)
	}
	if _, ok := ParseHolder(""); ok {
		t.Fatal("an empty file is a record")
	}
}
