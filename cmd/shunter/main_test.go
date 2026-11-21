package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestWatchNoNotify: with --no-notify the watcher raises its findings on
// stderr only. The notify hook is never run, and the watch state is not
// changed, so a check of a configuration can't reach the people the hook
// tells.
func TestWatchNoNotify(t *testing.T) {
	dir := t.TempDir()
	sent := filepath.Join(dir, "sent.jsonl")
	notify := filepath.Join(dir, "notify.sh")
	if err := os.WriteFile(notify, []byte("#!/bin/sh\ncat >> "+sent+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "shunter.toml")
	body := `store = "` + filepath.Join(dir, "no-store") + `"
[repo]
forge = "github"
owner = "example-org"
name = "example"
trunk = "main"
[git]
dir = "` + dir + `"
[ci]
checks = ["check"]
[notify]
command = ["` + notify + `"]
`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, fl, err := parse([]string{"--config", cfg, "--no-probe", "--no-notify", "--state", filepath.Join(dir, "watch.json")})
	if err != nil {
		t.Fatal(err)
	}
	// No store, so there is a finding to raise; the check itself may fail.
	if code := watchRun(fl); code == 0 {
		t.Fatalf("a watch with no daemon exited 0")
	}
	if _, err := os.Stat(sent); !os.IsNotExist(err) {
		t.Fatalf("the notify hook ran: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "watch.json")); !os.IsNotExist(err) {
		t.Fatalf("the watch state was written: %v", err)
	}
	// Without the flag, the same run does reach the hook.
	_, fl, _ = parse([]string{"--config", cfg, "--no-probe", "--state", filepath.Join(dir, "watch.json")})
	watchRun(fl)
	if _, err := os.Stat(sent); err != nil {
		t.Fatalf("the notify hook did not run without --no-notify: %v", err)
	}
}

// TestEveryFlagTheCommandReadsIsOneItParses: a flag main reads (fl["name"])
// and parse does not know is a flag nobody can give. (--until was read by
// cancel and judge and refused by parse: it could be set only in tests that
// built the request by hand.)
func TestEveryFlagTheCommandReadsIsOneItParses(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	read := regexp.MustCompile(`fl\["([a-z-]+)"\]`).FindAllStringSubmatch(string(src), -1)
	if len(read) < 10 {
		t.Fatalf("only %d flags found in main.go: the scan is broken", len(read))
	}
	for _, m := range read {
		if !boolFlags[m[1]] && !valueFlags[m[1]] {
			t.Errorf("main reads --%s, and parse refuses it", m[1])
		}
	}
	if _, fl, err := parse([]string{"x", "--until", "landed:fix", "--checks=c.jsonl", "--owners", "o.jsonl", "--live", "l.jsonl"}); err != nil ||
		fl["until"] != "landed:fix" || fl["checks"] != "c.jsonl" || fl["owners"] != "o.jsonl" || fl["live"] != "l.jsonl" {
		t.Errorf("parse: %v %v", fl, err)
	}
}
