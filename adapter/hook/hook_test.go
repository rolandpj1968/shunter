package hook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// script writes a shell hook into a temporary directory.
func script(t *testing.T, body string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{p}
}

type answer struct {
	Pass   bool     `json:"pass"`
	Blamed []string `json:"blamed,omitempty"`
}

func TestInputOnStdinAnswerOnStdout(t *testing.T) {
	h := Hook{Command: script(t, `read in; case "$in" in *'"train":"t1"'*) echo '{"pass":true}';; *) echo '{"pass":false}'; exit 1;; esac`)}
	var a answer
	res, err := h.Run(context.Background(), map[string]string{"train": "t1"}, &a)
	if err != nil || res.Code != 0 || !a.Pass {
		t.Fatalf("res %+v err %v answer %+v", res, err, a)
	}
	a = answer{}
	res, err = h.Run(context.Background(), map[string]string{"train": "t2"}, &a)
	if err != nil || res.Code != 1 || a.Pass {
		t.Fatalf("an exit code is an answer, not an error: res %+v err %v", res, err)
	}
}

func TestNoJSONIsNoAnswer(t *testing.T) {
	for _, body := range []string{`echo "all good"`, `true`, `echo '{"pass":true}{"pass":false}'`, `echo '{"verdict":"passed"}'`} {
		h := Hook{Command: script(t, body)}
		var a answer
		if _, err := h.Run(context.Background(), nil, &a); !errors.Is(err, ErrNoAnswer) {
			t.Errorf("%s: err %v, want no answer", body, err)
		}
	}
}

func TestDeadlineKillsTheGroup(t *testing.T) {
	h := Hook{Command: script(t, `sleep 30 & sleep 30; echo '{"pass":true}'`), Timeout: 200 * time.Millisecond}
	start := time.Now()
	var a answer
	res, err := h.Run(context.Background(), nil, &a)
	if !errors.Is(err, context.DeadlineExceeded) || res.Code != -1 || a.Pass {
		t.Fatalf("res %+v err %v answer %+v", res, err, a)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the kill took %v", time.Since(start))
	}
}

func TestStderrIsKept(t *testing.T) {
	h := Hook{Command: script(t, `echo "it broke here" >&2; exit 3`)}
	res, err := h.Run(context.Background(), nil, nil)
	if err != nil || res.Code != 3 || !strings.Contains(res.Stderr, "it broke here") {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestMissingProgram(t *testing.T) {
	h := Hook{Command: []string{filepath.Join(t.TempDir(), "absent")}}
	if _, err := h.Run(context.Background(), nil, nil); err == nil {
		t.Fatal("a hook that cannot start is an error")
	}
}
