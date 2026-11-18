# The implementation language

*A design decision for the reference implementation. It binds nothing in [`PROTOCOL.md`](../PROTOCOL.md), which is language-neutral.*

## The question

A merge queue looks like scripting: it calls `git` and the forge's CLI, moves files and watches processes. The implementation it replaces is shell. So is a scripting language, Python above all, the natural fit, and is Go the wrong tool?

## What the work is made of

Shunter has three parts, and they differ.

1. **The core logic:** the state machine of `PROTOCOL.md` §5.1, the store, the retry budgets and the judgement deadlines. Pure, small, and where every safety property lives.
2. **The adapters:** the forge, CI and git, each call bounded by a deadline and classified transient or permanent (§2.2). Glue, but glue with rules.
3. **The repository's own rules:** conflict resolution, the pre-flight steps, the blame heuristics. These differ in every codebase and change weekly.

Part 3 is the bulk of what a team edits, and it is scripting by nature. **It stays outside Shunter, as hooks: executables in any language** that take a batch on standard input and answer on standard output. Shell and Python are the natural fit there, and nothing below changes that.

The question is therefore about parts 1 and 2 only: a few thousand lines, about half logic and half bounded glue.

## Where the old queue failed

The record of the shell queue this design grew from sorts its failures into classes. The four largest:

- **a transition or a guard missing on a sibling path** (a cancel honoured in one stage and not in another);
- **an error swallowed** (a failed call read as "nothing to report");
- **an unbounded wait** (one hung call holding the whole queue);
- **the queue's own deployment** (a new version that could not start, or parse).

Almost all of these sat on paths that run rarely: the cancel, the hold, the refusal, the restart. The common path was sound. A language choice should be judged by what it does for the rare path.

## The candidates

| | Python | Go |
|---|---|---|
| **Glue to `git` and the forge** | Shortest. `subprocess.run(..., timeout=)` bounds a call in one argument | About twice the lines. `exec.CommandContext` bounds a call; a deadline is passed down by convention every Go reader knows |
| **Errors** | Loud by default: an unhandled exception stops the act. A large step up from shell | Explicit at each call; a dropped error is caught by the standard linters |
| **The rare path** | A misspelt name or a wrong argument in an error branch is found when that branch first runs, in production. A strict type checker and full branch coverage close most of this, as a discipline the team must keep | Found at build time. A binary that does not compile cannot be deployed |
| **The state machine** | Enums, dataclasses and `match` read well. Exhaustiveness needs the type checker | Typed states and events. Exhaustiveness needs a linter, or a table with a test, which is what Shunter uses |
| **Testing the pure function and the replayer** | Excellent; tables are a few lines each | Excellent; table tests are the idiom |
| **Deployment on one Linux box** | A checkout and an interpreter. With the standard library only, no environment to manage. "Deployed" and "edited" are the same act | One static binary. Deploying is copying a file, rolling back is keeping the last one, and the version is stamped in |
| **Patched often, by agents and people** | Edit and restart; the fastest loop. Also the loop that put an unparseable runner into service | Edit, build, test, restart; the build is a gate nobody can skip |
| **Start-up and recovery** | Either: all state is in the store, and a restart resumes from it | Either |
| **Concurrency** | Enough: the daemon is one loop that reads an event, calls the pure function, stores the result and performs effects | Enough, with more to spare than the design needs |

Others, briefly:
- **Rust** gives the strongest checking of the state machine (sum types, exhaustive matches), and costs the most to patch quickly. The safety argument is carried here by the model and the replayer, not by the type system, so the extra is not worth the friction.
- **TypeScript** sits between the two with a heavier runtime to deploy than either.
- **Shell** is what is being left.

## Recommendation

**Go for the core and the adapters. Any language for the hooks.**

The reasons, in order of weight:

1. **The failures were on rare paths, and Go checks rare paths before they run.** Python checks them when they run, unless a strict type checker and near-complete branch coverage are kept up without lapse. A property of the toolchain is worth more than a discipline of the team, most of all when the team is agents patching quickly.
2. **The build is a deployment gate.** Two of the catalogued failures were a new version of the queue that could not start. A compiled binary makes that class nearly impossible, and makes "which version is running" a fact with one answer.
3. **The scripting observation is right about the part it describes,** and the design keeps it: the scripting-shaped work is in the hooks, which stay scripts. Go is asked to do only the part that is logic and bounded calls.

What is given up: the core's glue is about twice as long as it would be in Python, and the edit-to-running loop for the core is slower by a build and a test run (seconds, on this size of program).

## What would change the answer

- **If the core proves to be mostly glue.** If, once the adapters are written, the state machine and store are under a quarter of the code and the adapters change weekly, the balance tips to Python with a strict type checker in CI.
- **If the people who patch it don't read Go.**

The cost of being wrong is bounded. The state machine is specified three times in language-neutral form: the table in `PROTOCOL.md` §5.1, the TLA+ model, and the replay fixtures. A port is a mechanical exercise checked by replaying the same traces through both.

**The rule either way: one implementation.** A second language is added only as a hook.
