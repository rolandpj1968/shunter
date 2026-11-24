# Example hooks

A repository's own rules stay the repository's: how its pre-flight runs, which conflicts it can resolve, and how a failure is blamed. Shunter calls them as **hooks**, commands in any language that read one JSON document on stdin and answer one on stdout ([`adapter/hook`](../adapter/hook)). This directory holds three generic ones, in Python with the standard library only, each configured by a JSON file and tested with synthetic fixtures ([`hooks_test.go`](hooks_test.go), [`testdata/`](testdata)).

| Hook | Shunter calls it | It does |
|---|---|---|
| [`preflight.py`](preflight.py) | on the assembled train, before CI | runs named steps in order, times and records each, skips the ones that never catch anything, and blames by bisection or by the files the failure names, or leaves those lines out |
| [`conflict.py`](conflict.py) | when a change conflicts with the train | resolves a conflict only in files a git attribute marks as generated, by regenerating them; anything else stays a conflict |
| [`blame.py`](blame.py) | when CI fails on a train's head | names the lines the failure points at, by the files and directories it names; says when it is a flake, and when nothing can be judged |

## A hook advises

No hook can make a train pass: the verdict comes only from CI's checks on the train's exact head. A hook that crashes, hangs past its deadline or prints no JSON has said nothing, and Shunter treats that as no verdict, never as a charge against a line. Lines a hook names that aren't in the train are ignored. Blame that names no line, or every line of several, is no blame: J1 decides, and its default splits the train in halves by line.

## Plugging them in

In `shunter.toml`:

```toml
[hooks.preflight]
command = ["python3", "-B", "hooks/preflight.py", "--config", "hooks/preflight.json"]
timeout = "20m"

[hooks.conflict]
command = ["python3", "-B", "hooks/conflict.py", "--config", "hooks/conflict.json"]

[hooks.blame]
command = ["python3", "-B", "hooks/blame.py", "--config", "hooks/blame.json"]
```

`-B`, and each script itself, keep Python from writing `__pycache__` into the directory the hook runs from, which for the pre-flight and conflict hooks is Shunter's clone. Each script's docstring lists its configuration keys, with an example. Relative paths in a configuration are relative to that file.

### pre-flight

```json
{
  "steps": [
    {"name": "generate", "command": ["make", "generate-check"], "timeout": "5m"},
    {"name": "lint", "command": ["make", "lint"]},
    {"name": "regenerate", "command": ["make", "generate"],
     "commit": {"message": "regenerate the combined train", "attribute": "merge", "value": "regenerate"}},
    {"name": "ratchet", "command": ["./scripts/ratchet.sh"], "blame": "bisect",
     "commit": {"message": "record what the train improves", "paths": ["counters.json"]}},
    {"name": "gate", "command": ["./scripts/gate.sh"], "blame": "none"}
  ],
  "always": ["generate"],
  "record": "/var/lib/shunter/preflight-steps.tsv",
  "recalled": "/var/lib/shunter/preflight-recalled.tsv",
  "usefulness": {"days": 7, "min_runs": 100, "min_seconds": 2},
  "output_dir": "/var/lib/shunter/preflight-logs"
}
```

- **Steps** run in order in Shunter's clone, at the train's head, and stop at the first failure. Each has `SHUNTER_TRAIN`, `SHUNTER_BASE` and `SHUNTER_HEAD` in its environment. A step must leave the tree as it found it, or say what it may commit.
- **Commit to the train.** A step's `"commit"` commits what it changes when it passes, on top of the train, and Shunter takes the last commit as the train's head: CI judges it and trunk takes it ([PROTOCOL.md](../PROTOCOL.md) §5.4). Give a `"message"` (`{train}` is the train) and what it may change: `"paths"` (files or directories) or an `"attribute"` with its `"value"` (as `.gitattributes` marks generated files). Any other change the step makes is undone, and said on stderr; a step that changes nothing allowed commits nothing. Later steps see the new head as `SHUNTER_HEAD`. Use it for a regeneration that nothing conflicted on (generated files stale only in combination), and for counters the combination improves. The commit is git's own, so give the hook a committer identity in its `env`. A hook may only add commits: Shunter refuses a head that dropped or rewrote the train's commits, or merged anything in.
- **Per-step timing.** Every step run appends a row to `record`: time, train, step, seconds, exit code. That file is how a team finds the steps that never fail, to drop them, and the slow ones.
- **The usefulness rule.** A step with no catch in the last `days`, at least `min_runs` runs in them, and a median over `min_seconds`, is skipped; CI still runs the check. A skipped step writes no row, so once it has been skipped for `days` it has no rows in the window and runs again, which samples it. `always` steps always run, `never` steps never run here, and a step named in the `recalled` file in the window runs whatever its record (write a row, `time<TAB>step<TAB>why`, when CI fails in what a skipped step covers).
- **Blame, per step.** A step's `"blame"` says how its failure is blamed: `"files"` (the default), `"bisect"` or `"none"`. With `"none"` nobody is named, and J1 decides; its default splits the train in halves by line. Use it for a step whose failure says nothing about which line caused it, where the old queue split the batch, so that the rules move unchanged. Any other value is a configuration error: the hook answers nothing, which Shunter reads as no verdict.
- **Leave out, instead of blame.** A step's `"answer"` is `"blame"` (the default) or `"leave-out"`. With `"leave-out"`, the lines the step's blame finds are left out of the train, each with the step's output as the reason, and not charged: their owners hear that the change didn't fit this train. Shunter builds the train again without them and runs pre-flight again. Use it for a check that fails on a combination rather than on a change, such as two changes taking the same generated number. It needs `"files"` or `"bisect"`; with `"none"` it is a configuration error.
- **Blame by bisection.** A step with `"blame": "bisect"` (or the older `"bisect": true`) is run again at each line's tip in the train (Shunter passes `tips`), by bisection, to find the first line at whose tip it fails. That line is blamed: with the lines before it, it tipped the check. A step that fails at the train's base too blames nobody, since trunk is broken. Use it for checks that measure the whole tree (a ratchet, a count, a registry), where the failure names no file.
- **Blame by files.** A `"files"` step's output is read for the files it names, with the rules below, against each line's files in the train.

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

Other merge drivers (a message catalogue merged key by key, a file of unique lines merged as a set) are set the same way, each `GIT_CONFIG_KEY_n=merge.<name>.driver` with its command: they apply to a build by either strategy, and to the re-offer's merge test. Set `[git] resolvable = ["merge=regenerate"]` too, so that a line sent back for a conflict with trunk only in generated files is offered again: the conflict hook resolves those at the build.

Generated files can also be stale only in combination, with nothing conflicting. That is a pre-flight step that regenerates and commits (`"commit"`, above).

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
- **A forced class.** When a known flake's or a timing failure's one rerun fails again, Shunter asks once more with `"force_class": "deterministic"` (PROTOCOL.md §5.5). The hook then skips the flake test below and names lines by the rules, as for any deterministic failure.
- **Flake or not** (optional): a `timing` pattern makes the failure flaky (Shunter reruns it once); else a `deterministic` pattern makes it deterministic; else a `flaky` pattern makes it flaky.
- **A seed is never a flake.** A failure that matches a `seeded` pattern is deterministic before any of the patterns above is asked, and its evidence leads with the seeds it names (PROTOCOL.md §5.5): a rerun draws another seed, and its pass proves nothing. The default, `\b(?:[A-Z][A-Z0-9_]*_)?SEED=\S+`, finds `SEED=…` and `ANY_NAME_SEED=…`; `"seeded": []` turns it off.

### The blame rules

Shared by `blame.py` and `preflight.py` ([`shunterhooks.py`](shunterhooks.py), `BlameRules`). The failure names files (a path with a slash and an extension, not part of a URL), directories (from configured `patterns` of kind `dir`, such as a test runner's `FAIL <package>` line, with `strip` to drop a module prefix, and `"subdirs": true` to take everything under the directory, not only its own files), path prefixes (kind `prefix`; give `"path"` for a failure that names no file, such as a count below its floor naming the directory it counts), test files by bare name (`foo_test.go:88`), and things a change's description says (kind `description`, below). Then:

0. **A description rule**, the most specific (PROTOCOL.md §5.6): a `description` pattern's group is what the failure names, and a line is named when its changes' title or description matches `describes`, with `{}` standing for that name:

   ```json
   {"regex": "issue #(\\d+) is still listed as open", "kind": "description", "describes": "(closes|fixes) #{}\\b"}
   ```

   Shunter gives each line its changes' titles and descriptions, as `text`. If this names some lines, but not every line of several, they are the blame and the rules below aren't asked.
1. A line is named when it touches a file the failure names, or a file under a prefix it names; or when its added lines carry text an `added` pattern finds in the failure (a refusal's wording, an attribute a browser test looked for). Its group, or with `"tokens"` each match of that regex in the group, is the text.
2. For each directory the failure names: the lines touching one of the failing test files there; else, the lines touching the directory's other files (the code under test); else, the lines touching any of its files. If the failure names no test file, the lines touching any file in the directory. With `"subdirs": "below"`, a last tier: if no line touches the directory itself, the lines touching a directory below it (data, wording, golden inputs the code reads).
3. If that names every line of a train of several: **a file the failure names that only one line touched** names that line alone.
4. If nothing above names a line, the `added` patterns with `"fallback": true`: a name the failure gives that a line adds (a constant a linter says a switch lacks, a table a registry test lists). With `"new"` (pathspecs, such as `["*.go", ":!gen"]`), only names no such file at the base holds count, so a line that merely uses an old name is not named.

Each line's added lines are its own `added` in the input when given, else read with git as its files are (the blame hook), or from the tips (the pre-flight).

A result naming no line, or every line, is no blame, and J1 splits the train by line. A stack is one line, so it is never split.

## The line-check worker

[`linecheck.py`](linecheck.py) is not a hook: Shunter doesn't call it, it calls Shunter (PROTOCOL.md §5.4). Each run claims one waiting line's check (`shunt claim --lease --version`), runs the site's check on the claim's key, and answers `shunt checked ... --action pass|fail|conflict|infra --version`. Run it from a timer, or with `--every 30s`, one process per slot, each with its own `actor`.

```json
{"shunt": ["shunt"], "socket": "/var/lib/shunter/shunter.sock", "actor": "line-check-1",
 "command": ["./scripts/check-line"], "version_files": ["scripts/check-line"], "lease": "3h", "timeout": "2h50m"}
```

- **The check** reads `{"line", "trunk", "heads", "version"}` and answers `{"verdict": "pass|fail|conflict|infra", "reason": "..."}`: a fail's evidence, a conflict's files, or why there is no verdict. A crash, a timeout, no JSON, or a fail or conflict with no reason is **infra**.
- **Infra is no verdict.** The claim ends at once instead of at its lease's end, nothing is recorded of the line, the judge is told, and the line is not given to a worker asking for any line for `[deadlines] unanswered` (10m). A trunk that fails by itself is infra too.
- **The version** is `version`, or a hash of `command` and `version_files`. A worker whose version isn't the queue's declares its own (`shunt checker line-check V`), which stops every older green counting; `"declare": false` makes it stop instead.
- **`timeout` under `lease`**, or a slow check's claim is given to another worker. A verdict that comes after its lease on the same heads still counts.

## Writing your own

Copy a script, or write one in any language, to the contract in [`examples/shunter.toml`](../examples/shunter.toml): read the input from stdin, print one JSON answer, exit. Print anything else to stderr; Shunter keeps its tail in the log. Move the old queue's rules into hooks unchanged first, so that only one thing changes at the cut-over ([`docs/cutover.md`](../docs/cutover.md)), and test them against fixtures of real failures, made synthetic.
