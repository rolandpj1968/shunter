// Package trunklock is the trunk push lock: one lock file that Shunter and
// every other pusher must hold to push trunk, so that two writers never push
// at once, even by mistake (I7; docs/cutover.md).
//
// The lock is an exclusive flock(2) on the file, which a shell script can
// take with flock(1):
//
//	flock -w 300 /var/lib/shunter/trunk.lock git push origin "$sha:refs/heads/main"
//
// The kernel releases a flock when the last descriptor of the open file is
// closed, so a holder that dies, however it dies, frees the lock: there is
// no stale lock left by a dead process to break. Two things can still hold
// it too long, and that is the stale-lock rule:
//
//   - an orphan: a child that inherited the holder's descriptor and outlived
//     it. The holder's record names a process that is gone, on this host,
//     while the lock is still held.
//   - a hung holder: alive, but holding the lock longer than MaxHold.
//
// A stale lock is reported, never broken: breaking a lock a live process
// holds is how two writers push at once. Acquire fails with ErrStale, which
// names the holder, and the watcher (shunter watch) raises it. A person ends
// the holder.
//
// While it holds the lock, a holder writes its record into the file: pid,
// host, who, and since when. The record is for people and for the stale-lock
// rule; the flock is the lock. A holder that writes no record (a bare flock(1))
// is still a holder, and can only be found stale by age if it writes one.
// The file is never removed or replaced: a lock on a replaced file is no lock,
// so Acquire checks it still holds the file at the path, and retries if not.
package trunklock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrBusy means another pusher holds the lock, and holds it in time.
var ErrBusy = errors.New("trunklock: another pusher holds the trunk lock")

// ErrStale means the lock is held by an orphan or held past MaxHold. It is
// not broken.
var ErrStale = errors.New("trunklock: the trunk lock is stale")

// Options are a lock's bounds.
type Options struct {
	// Who names the holder in its record: "shunter", "legacy queue".
	Who string
	// MaxHold is the longest a push holds the lock. Held longer, the lock is
	// stale. Default 15 minutes.
	MaxHold time.Duration
	// Poll is how often Acquire tries again while it waits. Default 200ms.
	Poll time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.MaxHold <= 0 {
		o.MaxHold = 15 * time.Minute
	}
	if o.Poll <= 0 {
		o.Poll = 200 * time.Millisecond
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Who == "" {
		o.Who = "shunter"
	}
	return o
}

// Holder is a holder's record.
type Holder struct {
	PID   int
	Host  string
	Who   string
	Since time.Time
}

func (h Holder) String() string {
	if h.PID == 0 && h.Who == "" {
		return "a holder that left no record"
	}
	return fmt.Sprintf("%s (pid %d on %s) since %s", orDash(h.Who), h.PID, orDash(h.Host), h.Since.UTC().Format(time.RFC3339))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Format writes a record: "pid=<n> host=<name> who=<name> since=<RFC 3339>".
// Who has no spaces.
func (h Holder) Format() string {
	return fmt.Sprintf("pid=%d host=%s who=%s since=%s\n", h.PID, h.Host, strings.ReplaceAll(h.Who, " ", "_"), h.Since.UTC().Format(time.RFC3339))
}

// ParseHolder reads a record. An empty file is no record.
func ParseHolder(s string) (Holder, bool) {
	var h Holder
	ok := false
	for _, f := range strings.Fields(s) {
		k, v, has := strings.Cut(f, "=")
		if !has {
			continue
		}
		switch k {
		case "pid":
			h.PID, _ = strconv.Atoi(v)
			ok = true
		case "host":
			h.Host = v
		case "who":
			h.Who = strings.ReplaceAll(v, "_", " ")
			ok = true
		case "since":
			h.Since, _ = time.Parse(time.RFC3339, v)
		}
	}
	return h, ok
}

// Lock is the lock, held.
type Lock struct {
	f    *os.File
	path string
}

// Status is what Inspect finds.
type Status struct {
	Held   bool
	Holder Holder
	Record bool // the file holds a record
	// Stale says why a held lock is stale; empty when it is not.
	Stale string
}

func (s Status) String() string {
	switch {
	case !s.Held && s.Record:
		return "free (a record of a past holder remains: " + s.Holder.String() + ")"
	case !s.Held:
		return "free"
	case s.Stale != "":
		return "STALE: held by " + s.Holder.String() + ": " + s.Stale
	default:
		return "held by " + s.Holder.String()
	}
}

// Acquire takes the lock, waiting until ctx ends. It returns ErrStale when
// it gave up on a stale lock and ErrBusy on any other; both name the holder.
func Acquire(ctx context.Context, path string, o Options) (*Lock, error) {
	o = o.withDefaults()
	for {
		l, err := try(path, o)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, errHeld) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			st, ierr := Inspect(path, o)
			if ierr != nil {
				return nil, fmt.Errorf("%w (%v)", ErrBusy, ierr)
			}
			if st.Stale != "" {
				return nil, fmt.Errorf("%w: %s", ErrStale, st)
			}
			return nil, fmt.Errorf("%w: %s", ErrBusy, st)
		case <-time.After(o.Poll):
		}
	}
}

var errHeld = errors.New("held")

func try(path string, o Options) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("trunklock: %w", err)
	}
	ok, err := lockNB(f, true)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !ok {
		f.Close()
		return nil, errHeld
	}
	// The file at the path must still be the one locked: a lock on a file
	// someone replaced or removed is no lock.
	if same, err := samePath(f, path); err != nil || !same {
		unlock(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("trunklock: %w", err)
		}
		return nil, errHeld
	}
	host, _ := os.Hostname()
	rec := Holder{PID: os.Getpid(), Host: host, Who: o.Who, Since: o.Now()}.Format()
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(rec), 0)
	}
	return &Lock{f: f, path: path}, nil
}

// Release clears the record and releases the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	l.f.Truncate(0)
	unlock(l.f)
	err := l.f.Close()
	l.f = nil
	return err
}

// Inspect reports the lock without taking it, and applies the stale-lock
// rule. To see whether the lock is held it takes a shared lock for a moment,
// which a pusher waiting for the lock sees as held, and tries again.
func Inspect(path string, o Options) (Status, error) {
	o = o.withDefaults()
	var st Status
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("trunklock: %w", err)
	}
	defer f.Close()
	b := make([]byte, 512)
	n, _ := f.ReadAt(b, 0)
	st.Holder, st.Record = ParseHolder(string(b[:n]))
	got, err := lockNB(f, false)
	if err != nil {
		return st, err
	}
	if got {
		unlock(f)
		return st, nil
	}
	st.Held = true
	host, _ := os.Hostname()
	switch {
	case st.Record && st.Holder.PID > 0 && st.Holder.Host == host && !alive(st.Holder.PID):
		st.Stale = fmt.Sprintf("its recorded holder, pid %d, is gone: a process that inherited its descriptor holds the lock (find it with fuser or lsof on the file)", st.Holder.PID)
	case st.Record && !st.Holder.Since.IsZero() && o.Now().Sub(st.Holder.Since) > o.MaxHold:
		st.Stale = fmt.Sprintf("held for %s, longer than the %s a push may take", o.Now().Sub(st.Holder.Since).Round(time.Second), o.MaxHold)
	}
	return st, nil
}
