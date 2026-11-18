# Example hooks

A repository's own rules stay the repository's: how its pre-flight runs, which conflicts it can resolve, and how a failure is blamed. Shunter calls them as **hooks**, commands in any language that read one JSON document on stdin and answer one on stdout ([`adapter/hook`](../adapter/hook)). This directory holds three generic ones, in Python with the standard library only, each configured by a JSON file and tested with synthetic fixtures ([`hooks_test.go`](hooks_test.go), [`testdata/`](testdata)).

| Hook | Shunter calls it | It does |
|---|---|---|
| [`preflight.py`](preflight.py) | on the assembled train, before CI | runs named steps in order, times and records each, skips the ones that never catch anything, and blames by bisection or by the files the failure names |
| [`conflict.py`](conflict.py) | when a change conflicts with the train | resolves a conflict only in files a git attribute marks as generated, by regenerating them; anything else stays a conflict |
| [`blame.py`](blame.py) | when CI fails on a train's head | names the lines the failure points at, by the files and directories it names; says when it is a flake, and when nothing can be judged |

## A hook advises

No hook can make a train pass: the verdict comes only from CI's checks on the train's exact head. A hook that crashes, hangs past its deadline or prints no JSON has said nothing, and Shunter treats that as no verdict, never as a charge against a line. Lines a hook names that aren't in the train are ignored. Blame that names no line, or every line of several, is no blame: J1 decides, and its default splits the train in halves by line.

## Plugging them in

In `shunter.toml`:

```toml
[hooks.preflight]
command = ["python3", "hooks/preflight.py", "--config", "hooks/preflight.json"]
timeout = "20m"

[hooks.conflict]
command = ["python3", "hooks/conflict.py", "--config", "hooks/conflict.json"]

[hooks.blame]
command = ["python3", "hooks/blame.py", "--config", "hooks/blame.json"]
```

Each script's docstring lists its configuration keys, with an example. Relative paths in a configuration are relative to that file.

### pre-flight

```json
{
  "steps": [
    {"name": "generate", "command": ["make", "generate-check"], "timeout": "5m"},
    {"name": "lint", "command": ["make", "lint"]},
    {"name": "ratchet", "command": ["./scripts/ratchet.sh"], "bisect": true}
  ],
  "always": ["generate"],
  "record": "/var/lib/shunter/preflight-steps.tsv",
  "recalled": "/var/lib/shunter/preflight-recalled.tsv",
  "usefulness": {"days": 7, "min_runs": 100, "min_seconds": 2},
  "output_dir": "/var/lib/shunter/preflight-logs"
}
```

- **Steps** run in order in Shunter's clone, at the train's head, and stop at the first failure. Each has `SHUNTER_TRAIN`, `SHUNTER_BASE` and `SHUNTER_HEAD` in its environment. A step must leave the tree as it found it, or commit what it changes.
- **Per-step timing.** Every step run appends a row to `record`: time, train, step, seconds, exit code. That file is how a team finds the steps that never fail, to drop them, and the slow ones.
- **The usefulness rule.** A step with no catch in the last `days`, at least `min_runs` runs in them, and a median over `min_seconds`, is skipped; CI still runs the check. A skipped step writes no row, so once it has been skipped for `days` it has no rows in the window and runs again, which samples it. `always` steps always run, `never` steps never run here, and a step named in the `recalled` file in the window runs whatever its record (write a row, `time<TAB>step<TAB>why`, when CI fails in what a skipped step covers).
- **Blame by bisection.** A step marked `"bisect": true` is run again at each line's tip in the train (Shunter passes `tips`), by bisection, to find the first line at whose tip it fails. That line is blamed: with the lines before it, it tipped the check. A step that fails at the train's base too blames nobody, since trunk is broken. Use it for checks that measure the whole tree (a ratchet, a count, a registry), where the failure names no file.
- **Blame by files.** Any other failing step's output is read for the files it names, with the rules below, against each line's files in the train.

### conflict

```json
{"attribute": "merge", "value": "regenerate", "command": ["make", "generate"], "timeout": "10m"}
```

with `.gitattributes`:

```
api/gen/**       merge=regenerate
web/src/gen/**   merge=regenerate
```

Every conflicted file must be marked, or the line is left out with the files named. Marked files take the train's side, then the command regenerates them from the sources as merged, and the generated files it changed are staged. A change the command makes to any file that isn't marked is undone. So that a generated file always surfaces as a conflict instead of being merged as text, give its merge driver as `false` in the environment of Shunter's clone:

```toml
[git]
env = ["GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=merge.regenerate.driver", "GIT_CONFIG_VALUE_0=false"]
```

Generated files can also be stale only in combination, with nothing conflicting. That is a pre-flight step: regenerate, then fail if the tree changed.

### blame

```json
{
  "git_dir": "/var/lib/shunter/clone",
  "trunk_ref": "refs/remotes/origin/main",
  "fetch": ["origin"],
  "log_command": ["./scripts/failed-job-logs"],
  "patterns": [{"regex": "^FAIL\\s+example\\.com/mod/(\\S+)\\s", "kind": "dir"}],
  "test_files": ["_test\\.go$", "\\.spec\\.ts$"],
  "timing": ["panic: test timed out", "context deadline exceeded"],
  "deterministic": ["Error Trace:", "\\.go:\\d+:\\d+: "],
  "flaky": ["connection refused", "connection reset"]
}
```

- **The failure's text** is CI's evidence, the failed check runs' names and summaries, and the output of `log_command`, which gets the hook's input on its stdin (to fetch the failed jobs' logs, say). A log that can't be read answers `no-verdict`: nothing is judged on what could not be read.
- **Each line's files** are what its last change's head changes since it left trunk, read with git in `git_dir` (a clone holding the changes: Shunter's own works, read-only). A head that isn't there is fetched from the `fetch` remotes; one that still can't be read answers `no-verdict`.
- **Flake or not** (optional): a `timing` pattern makes the failure flaky (Shunter reruns it once); else a `deterministic` pattern makes it deterministic; else a `flaky` pattern makes it flaky.

### The blame rules

Shared by `blame.py` and `preflight.py` ([`shunterhooks.py`](shunterhooks.py), `BlameRules`). The failure names files (a path with a slash and an extension, not part of a URL), directories (from configured `patterns` of kind `dir`, such as a test runner's `FAIL <package>` line, with `strip` to drop a module prefix) and test files by bare name (`foo_test.go:88`). Then:

1. A line is named when it touches a file the failure names.
2. For each directory the failure names: the lines touching one of the failing test files there; else, the lines touching the directory's other files (the code under test); else, the lines touching any of its files. If the failure names no test file, the lines touching any file in the directory.
3. If that names every line of a train of several: **a file the failure names that only one line touched** names that line alone.

A result naming no line, or every line, is no blame, and J1 splits the train by line. A stack is one line, so it is never split.

## Writing your own

Copy a script, or write one in any language, to the contract in [`examples/shunter.toml`](../examples/shunter.toml): read the input from stdin, print one JSON answer, exit. Print anything else to stderr; Shunter keeps its tail in the log. Move the old queue's rules into hooks unchanged first, so that only one thing changes at the cut-over ([`docs/cutover.md`](../docs/cutover.md)), and test them against fixtures of real failures, made synthetic.
