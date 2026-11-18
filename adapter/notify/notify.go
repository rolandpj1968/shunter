// Package notify tells people: every notice is appended to an events file,
// one JSON document a line, and is then handed to an optional command hook
// (a chat message, a comment on the change, a page). The file is the record;
// the hook is delivery.
//
// A keyed notice is written once however often it is sent: the keys already
// in the file are read when it is opened. The hook is run for a keyed notice
// until it succeeds once; its failure is the call's failure, so the daemon
// retries it and escalates.
package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rolandpj1968/shunter/adapter/hook"
	"github.com/rolandpj1968/shunter/state"
)

// Notice is one line of the events file, and the hook's input.
type Notice struct {
	Time    time.Time        `json:"time"`
	Key     string           `json:"key,omitempty"`
	To      string           `json:"to"` // "owner" or "judge"
	Line    state.LineID     `json:"line,omitempty"`
	Changes []state.ChangeID `json:"changes,omitempty"`
	Point   string           `json:"point,omitempty"` // judge: J1, J3, alert
	Train   state.TrainID    `json:"train,omitempty"`
	Message string           `json:"message"`
	// Delivered is set on a second record of the same key once the hook
	// has taken it.
	Delivered bool `json:"delivered,omitempty"`
}

// Notifier implements adapter.Notifier.
type Notifier struct {
	path string
	hook hook.Hook
	now  func() time.Time

	mu        sync.Mutex
	written   map[string]bool
	delivered map[string]bool
}

// Open opens (or creates) the events file.
func Open(path string, h hook.Hook, now func() time.Time) (*Notifier, error) {
	if path == "" {
		return nil, errors.New("notify: no events file")
	}
	if now == nil {
		now = time.Now
	}
	n := &Notifier{path: path, hook: h, now: now, written: map[string]bool{}, delivered: map[string]bool{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return n, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var no Notice
		if json.Unmarshal(sc.Bytes(), &no) != nil || no.Key == "" {
			continue // a torn last line is not a notice
		}
		n.written[no.Key] = true
		if no.Delivered {
			n.delivered[no.Key] = true
		}
	}
	return n, sc.Err()
}

// Owner implements adapter.Notifier.
func (n *Notifier) Owner(ctx context.Context, key string, line state.LineID, changes []state.ChangeID, msg string) error {
	return n.send(ctx, Notice{Key: key, To: "owner", Line: line, Changes: changes, Message: msg})
}

// Judge implements adapter.Notifier.
func (n *Notifier) Judge(ctx context.Context, key, point string, t state.TrainID, msg string) error {
	return n.send(ctx, Notice{Key: key, To: "judge", Point: point, Train: t, Message: msg})
}

func (n *Notifier) send(ctx context.Context, no Notice) error {
	no.Time = n.now().UTC()
	n.mu.Lock()
	defer n.mu.Unlock()
	if no.Key == "" || !n.written[no.Key] {
		if err := n.append(no); err != nil {
			return err
		}
		if no.Key != "" {
			n.written[no.Key] = true
		}
	}
	if !n.hook.Configured() || (no.Key != "" && n.delivered[no.Key]) {
		return nil
	}
	res, err := n.hook.Run(ctx, no, nil)
	if err == nil && res.Code != 0 {
		err = fmt.Errorf("notify hook: exit %d: %s", res.Code, res.Stderr)
	}
	if err != nil {
		return err
	}
	if no.Key != "" {
		n.delivered[no.Key] = true
		d := no
		d.Delivered, d.Time = true, n.now().UTC()
		return n.append(d)
	}
	return nil
}

func (n *Notifier) append(no Notice) error {
	b, err := json.Marshal(no)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(n.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
