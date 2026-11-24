// Package hook runs a repository's own rules as external commands, in any
// language: pre-flight, conflict resolution and blame (docs/cutover.md: the
// old queue's rules move out as hooks, unrewritten).
//
// A hook takes its input as one JSON document on stdin and answers with one
// JSON document on stdout, and an exit code. Its stderr is kept, bounded, for
// the log. A hook that runs past its deadline is killed, with its process
// group.
//
// A hook's answer is advice to an adapter, never a verdict. No hook can make
// a train pass: the verdict comes only from CI, and the lander reads it for
// itself (I9). Each adapter that calls a hook says what of its answer it
// takes.
package hook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Hook is one configured command.
type Hook struct {
	// Command is the program and its arguments. Empty means no hook.
	Command []string
	// Timeout bounds one run. Default 10m.
	Timeout time.Duration
	// Dir is the working directory; the current one when empty.
	Dir string
	// Env is added to the environment.
	Env []string
}

// Configured reports whether there is a command to run.
func (h Hook) Configured() bool { return len(h.Command) > 0 }

// Version is the hook's version for a hook that reports none
// (docs/queue-as-bag.md §5): a hash of its command file and its command
// line. A command file that can't be read is hashed by its name alone. An
// unconfigured hook has no version.
func (h Hook) Version() string {
	if !h.Configured() {
		return ""
	}
	sum := sha256.New()
	path := h.Command[0]
	if !filepath.IsAbs(path) && h.Dir != "" && strings.ContainsRune(path, filepath.Separator) {
		path = filepath.Join(h.Dir, path)
	}
	if b, err := os.ReadFile(path); err == nil {
		sum.Write(b)
	}
	sum.Write([]byte(strings.Join(h.Command, "\x00")))
	return hex.EncodeToString(sum.Sum(nil))[:12]
}

// Result is one run.
type Result struct {
	// Code is the exit code. -1 when the hook was killed.
	Code int
	// Stdout is what it printed, raw.
	Stdout []byte
	// Stderr is the tail of what it printed to stderr.
	Stderr string
}

// ErrNoAnswer is a hook that ran but gave no JSON answer.
var ErrNoAnswer = errors.New("hook: no JSON answer on stdout")

// maxOut bounds what is kept of a hook's output.
const maxOut = 4 << 20

// Run runs the hook with in as its input. When out is not nil, stdout is
// decoded into it; a hook that printed no valid JSON gives ErrNoAnswer
// (wrapped), whatever its exit code. Run returns an error, too, when the hook
// could not be started or ran past its deadline; an exit code other than 0 is
// not an error by itself, it is the hook's answer.
func (h Hook) Run(ctx context.Context, in any, out any) (Result, error) {
	if !h.Configured() {
		return Result{}, errors.New("hook: no command configured")
	}
	body, err := json.Marshal(in)
	if err != nil {
		return Result{}, fmt.Errorf("hook: input: %w", err)
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, h.Command[0], h.Command[1:]...)
	cmd.Dir = h.Dir
	cmd.Env = append(os.Environ(), h.Env...)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr capped
	stdout.max, stderr.max = maxOut, 64<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	setGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	res := Result{Code: 0, Stdout: stdout.Bytes(), Stderr: tail(stderr.String(), 2000)}
	if cctx.Err() != nil {
		res.Code = -1
		return res, fmt.Errorf("hook %s: no answer by its deadline (%s): %w", h.Command[0], timeout, context.DeadlineExceeded)
	}
	var ee *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &ee):
		res.Code = ee.ExitCode()
	default:
		return res, fmt.Errorf("hook %s: %w", h.Command[0], runErr)
	}
	if out == nil {
		return res, nil
	}
	trim := bytes.TrimSpace(res.Stdout)
	if len(trim) == 0 {
		return res, fmt.Errorf("hook %s (exit %d): %w", h.Command[0], res.Code, ErrNoAnswer)
	}
	dec := json.NewDecoder(bytes.NewReader(trim))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return res, fmt.Errorf("hook %s (exit %d): %w: %v", h.Command[0], res.Code, ErrNoAnswer, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return res, fmt.Errorf("hook %s (exit %d): %w: more than one JSON document", h.Command[0], res.Code, ErrNoAnswer)
	}
	return res, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// capped is a buffer that keeps at most max bytes and drops the rest.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		if len(p) > room {
			c.Buffer.Write(p[:room])
		} else {
			c.Buffer.Write(p)
		}
	}
	return len(p), nil
}
