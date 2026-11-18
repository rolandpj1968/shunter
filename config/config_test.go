package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExampleLoads(t *testing.T) {
	f, err := Load("../examples/shunter.toml")
	if err != nil {
		t.Fatal(err)
	}
	if f.Repo.Owner != "example-org" || f.Repo.Trunk != "main" || f.Queue.Depth != 2 || f.CI.Checks[0] != "check" {
		t.Fatalf("%+v", f)
	}
	if f.Deadlines.CI.Duration != 90*time.Minute || f.Retry.Backoff.Duration != 2*time.Second {
		t.Fatalf("deadlines %+v retry %+v", f.Deadlines, f.Retry)
	}
	d := f.Daemon("v")
	if d.CI != 90*time.Minute || d.Push != 5*time.Minute || d.MaxLines != 8 || d.Retries != 3 {
		t.Fatalf("daemon %+v", d)
	}
	abs, _ := filepath.Abs("../examples/hooks/preflight")
	if h := f.hook(f.Hooks.Preflight, f.Git.Dir); h.Command[0] != abs || h.Dir != f.Git.Dir || h.Timeout != 20*time.Minute {
		t.Fatalf("hook %+v", h)
	}
	if f.Socket != "/var/lib/shunter/store/shunter.sock" {
		t.Fatalf("socket %s", f.Socket)
	}
}

func write(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "shunter.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `
store = "store"
[repo]
owner = "o"
name = "r"
trunk = "main"
[git]
dir = "clone"
[ci]
checks = ["check"]
`

func TestDefaultsAndRelativePaths(t *testing.T) {
	p := write(t, minimal)
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(p)
	if f.Store != filepath.Join(dir, "store") || f.Git.Dir != filepath.Join(dir, "clone") || f.Notify.Events != filepath.Join(dir, "store", "events.jsonl") {
		t.Fatalf("paths %s %s %s", f.Store, f.Git.Dir, f.Notify.Events)
	}
	if f.Git.Strategy != "rebase" || f.CI.Mode != "checks" || f.Repo.TokenEnv != "GH_TOKEN" || f.Git.BranchPrefix != "shunter/" {
		t.Fatalf("defaults %+v", f)
	}
}

func TestRefusals(t *testing.T) {
	cases := map[string]string{
		"unknown keys":       minimal + "\n[deadlines]\nci_deadline = \"5m\"\n",
		"store is required":  strings.Replace(minimal, `store = "store"`, "", 1),
		"ci.checks":          strings.Replace(minimal, `checks = ["check"]`, "", 1),
		"needs ci.workflow":  strings.Replace(minimal, `[ci]`, "[ci]\nmode = \"dispatch\"", 1),
		"git.strategy":       strings.Replace(minimal, `dir = "clone"`, "dir = \"clone\"\nstrategy = \"squash\"", 1),
		"must contain {id}":  strings.Replace(minimal, `dir = "clone"`, "dir = \"clone\"\nchange_ref = \"refs/pull/head\"", 1),
		"invalid duration":   minimal + "\n[deadlines]\nci = \"soon\"\n",
		"only github so far": strings.Replace(minimal, `owner = "o"`, "owner = \"o\"\nforge = \"elsewhere\"", 1),
	}
	for want, body := range cases {
		_, err := Load(write(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}
