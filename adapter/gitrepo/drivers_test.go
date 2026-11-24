package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rolandpj1968/shunter/adapter"
	"github.com/rolandpj1968/shunter/adapter/hook"
	"github.com/rolandpj1968/shunter/state"
)

// keysDriver merges key=value lines key by key: a key the other side
// changed from the ancestor takes the other side's value.
const keysDriver = `#!/bin/sh
awk -F= 'FILENAME==ARGV[1]{o[$1]=$2; next} FILENAME==ARGV[2]{a[$1]=$2; ord[++n]=$1; next}
  {if (!($1 in a)) ord[++n]=$1; if (!($1 in o) || o[$1]!=$2) a[$1]=$2}
  END{for(i=1;i<=n;i++) print ord[i] "=" a[ord[i]]}' "$1" "$2" "$3" > "$2.merged" && mv "$2.merged" "$2"
`

// setDriver merges a file of unique lines as the set of both sides'.
const setDriver = "#!/bin/sh\nsort -u \"$1\" \"$2\" -o \"$1\"\n"

// driverWorld is a trunk with a file of each kind the merge drivers handle,
// and two lines that each change all three where a text merge conflicts:
// gen.txt is generated (merge=regenerate: its driver is false, and the
// conflict hook regenerates it), msgs.txt is merged key by key
// (merge=keys), set.txt is a set of lines (merge=lineset).
func driverWorld(t *testing.T, strategy string) (w *world, base state.Head, lines []adapter.BuildLine) {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	dir := t.TempDir()
	keys, set := filepath.Join(dir, "keys.sh"), filepath.Join(dir, "set.sh")
	for p, body := range map[string]string{keys: keysDriver, set: setDriver} {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w = newWorld(t, Config{Strategy: strategy})
	w.repo.cfg.Env = append(w.repo.cfg.Env, "GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=merge.regenerate.driver", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=merge.keys.driver", "GIT_CONFIG_VALUE_1="+keys+" %O %A %B",
		"GIT_CONFIG_KEY_2=merge.lineset.driver", "GIT_CONFIG_VALUE_2="+set+" %A %B")
	cfg := filepath.Join(dir, "conflict.json")
	if err := os.WriteFile(cfg, []byte(`{"command": ["sh", "-c", "cat a.src b.src > gen.txt"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	script, _ := filepath.Abs("../../hooks/conflict.py")
	w.repo.cfg.Conflict = hook.Hook{Command: []string{py, "-B", script, "--config", cfg}, Dir: w.repo.cfg.Dir, Timeout: time.Minute}
	w.write(".gitattributes", "gen.txt merge=regenerate\nmsgs.txt merge=keys\nset.txt merge=lineset\n")
	w.write("a.src", "a0\n")
	w.write("b.src", "b0\n")
	w.write("gen.txt", "a0\nb0\n")
	w.write("msgs.txt", "a=0\nb=0\n")
	w.write("set.txt", "x\n")
	run(t, w.dev, "add", ".")
	run(t, w.dev, "commit", "--quiet", "-m", "three kinds of file")
	run(t, w.dev, "push", "--quiet", "origin", "main")
	base = w.trunk()
	h1 := w.change("1", "main", map[string]string{"a.src": "a1\n", "gen.txt": "a1\nb0\n", "msgs.txt": "a=1\nb=0\n", "set.txt": "x\ny1\n"})
	h2 := w.change("2", "main", map[string]string{"b.src": "b1\n", "gen.txt": "a0\nb1\n", "msgs.txt": "a=0\nb=2\n", "set.txt": "x\ny2\n"})
	return w, base, []adapter.BuildLine{line("1", ch("1", h1)), line("2", ch("2", h2))}
}

// TestMergeDriversBuildATrain: a train with a clash in each kind of file
// builds as the drivers say, by either strategy: the drivers of [git] env
// apply to a rebase's replay as to a merge.
func TestMergeDriversBuildATrain(t *testing.T) {
	for _, strategy := range []string{"rebase", "merge"} {
		t.Run(strategy, func(t *testing.T) {
			w, base, lines := driverWorld(t, strategy)
			res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: lines})
			if err != nil || len(res.LeftOut) != 0 || res.Head == "" {
				t.Fatalf("%+v %v", res, err)
			}
			for f, want := range map[string]string{"gen.txt": "a1\nb1", "msgs.txt": "a=1\nb=2", "set.txt": "x\ny1\ny2"} {
				if got := w.cat(res.Head, f); got != want {
					t.Errorf("%s is %q, want %q", f, got, want)
				}
			}
		})
	}
}

// TestMergesCountsWhatTheConflictHookResolves: the re-offer's merge test
// applies the drivers too, and a conflict only in files [git] resolvable
// marks (the generated ones, which the conflict hook regenerates at the
// build) is no conflict a person must fix.
func TestMergesCountsWhatTheConflictHookResolves(t *testing.T) {
	w, base, lines := driverWorld(t, "rebase")
	res, err := w.repo.Build(ctx, "k1", adapter.BuildRequest{Train: "t1", Base: base, Lines: lines[:1]})
	if err != nil || res.Head == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if err := w.repo.Push(ctx, "p1", res.Head, base); err != nil {
		t.Fatal(err)
	}
	got, err := w.repo.Merges(ctx, lines[1], res.Head)
	if err != nil || got != "gen.txt" {
		t.Fatalf("without resolvable: %q %v, want only gen.txt in conflict", got, err)
	}
	w.repo.cfg.Resolvable = []string{"merge=regenerate"}
	if got, err := w.repo.Merges(ctx, lines[1], res.Head); err != nil || got != "" {
		t.Fatalf("with gen.txt resolvable: %q %v, want clean", got, err)
	}
	// A change to a file nothing resolves is still a conflict.
	h3 := w.change("3", string(base), map[string]string{"a.src": "a3\n"})
	if got, err := w.repo.Merges(ctx, line("3", ch("3", h3)), res.Head); err != nil || got != "a.src" {
		t.Fatalf("a source in conflict: %q %v", got, err)
	}
}
