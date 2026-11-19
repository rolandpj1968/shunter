# Shunter

**Shunter assembles trains of code changes, lands them on a linear trunk, and knows when to hold the line.**

## The problem

**Turning a high rate of locally-checked changes into one globally-tested history, when the global test is slow, expensive and noisy, and judgement is scarce.**

AI-agent teams face this problem. Human teams on legacy workflows don't, except at the scale of the largest monorepos (Google's, Uber's, OpenStack's), which is where Zuul and SubmitQueue came from.

- **On a human team with a legacy workflow,** developers open pull requests by hand, CI tests each one, and a human merges it. Pull requests arrive slowly enough that testing them one at a time, with humans judging each failure, keeps up.
- **On an AI-agent team with an agentic workflow,** many AI agents write code concurrently and open pull requests themselves, with one coordinator (a human or an AI) overseeing them. Pull requests arrive faster than one-at-a-time testing can land them, and the judgement humans supplied for free is missing.

Legacy merge workflows work because they quietly rely on conditions that agentic workflows break:

1. **Arrival rate against test throughput.** A human team opens a few pull requests a day, and CI can test each one separately with time to spare. AI agents open pull requests faster than CI can test them one at a time. When pull requests arrive faster than CI can test them, the queue grows without bound, so an agentic workflow *must* test pull requests in batches.
2. **Batching destroys attribution.** When CI tests one pull request per run, a failure belongs to that pull request. When CI tests twelve pull requests in one run, the failure could belong to any of them. Bisecting the batch costs extra CI runs, and blaming from the evidence in the logs is heuristic.
3. **Rare failures become constant.** A test that fails at random 1% of the time costs a human team a shrug once a month, and costs an AI-agent team a failed batch every hour. A batch passes only if every pull request in it passes, so the chance of a green batch falls exponentially with batch size. In an agentic workflow, flaky tests, errors from the forge (GitHub, for example) and tests leaking state into each other decide throughput. Handling flaky tests, and correcting them, therefore becomes a core part of landing changes, not occasional test hygiene: on an AI-agent team, "just rerun it" doesn't scale (see *The arithmetic of flakiness* below).
4. **Human teams supply judgement for free.** On a human team, each author watches their own pull request and knows whether a failure is theirs, and the team notices "trunk is red, hold off merging" in its chat. AI agents share no such awareness. Each agent optimises locally ("green on my branch, so queue it"), no agent owns the shared state (trunk's health, the shared test fixtures, the generated files), and agents retire in the middle of tasks.
5. **Shared resources are contended at machine speed.** AI agents work without pause, so they hit the same generated files, shared test fixtures and CI runners continuously. Conflicts that a human team meets once a quarter, an AI-agent team meets several times a day.
6. **Judgement becomes the bottleneck.** On a human team, judgement is spread across every developer. On an AI-agent team, judgement is concentrated in the coordinator, whose attention is limited, while the number of decisions grows with the agents' throughput.

In short: **legacy merge workflows assume that verification is cheap relative to the rate of new pull requests, and that judgement is abundant and spread across the team. In agentic workflows, verification is the bottleneck, and judgement is scarce and concentrated in the coordinator.** Shunter's mechanism attacks the first problem: batching, speculation and change-based testing. Shunter's judgement design attacks the second: the coordinator's judgement is spent only where rules can't decide, each decision has a deadline and a safe default, and judgements that recur are promoted into rules.

## What Shunter optimises

**The primary problem is throughput onto a linear trunk.** Trunk is one ordered history, and each change must be tested in the exact state it will land into: trunk plus every change that lands before it. Every landing changes the base for the next change, so the integration test is inherently sequential. A team can test branches in parallel as much as it likes, but CI on a branch shows only that the change works on its own base, not that it works on trunk. Landing is therefore a serial process, and the goal is to push as many tested changes through it per hour as AI agents produce. Throughput must at least match the agents' rate of new pull requests, or the queue grows without bound.

**The secondary problem is the cost of CI:** money on metered runners, and machine time on owned ones.

**One property is never traded away:** trunk only ever holds states that CI has tested (the invariants in [`PROTOCOL.md`](PROTOCOL.md)).

Within those terms, Shunter's knobs trade throughput, latency and cost against each other:

- **Batching** tests several changes in one CI run. It raises throughput and lowers cost, but it blurs which change caused a failure.
- **Speculation** tests the next batches on the expected future trunk, before the current batch lands. It raises throughput and cuts latency, but it costs runs that are thrown away when an earlier batch fails. On metered CI that waste is money; on owned runners it's only spare capacity, so a team on owned runners can speculate deeper.
- **Pre-flight** runs cheap, deterministic checks on the assembled batch before any CI is spent: conflicts, regenerated files, lint, counts across changes, frequent CI catches. Checks are promoted into pre-flight when CI keeps catching what they'd catch in seconds, and dropped when they stop catching anything.
- **Change-based test selection** runs only the jobs a change can affect, which lowers cost without weakening the invariant.
- **Flaky-test management** keeps a batch's chance of passing high. That chance limits every other knob (see *The arithmetic of flakiness* below).
- **Judgement** decides what rules can't, at defined points. The coordinator's attention is itself a limit on throughput, so recurring judgements are promoted into rules, and silence falls back to a safe default.

With unlimited money, a team could speculate on every plausible future trunk, and a change would land about one CI run after it was ready, whatever the arrival rate. Money buys latency through speculation; it never removes the need to test each change in the state it lands into. Shunter aims to get as close to that throughput as the CI budget allows.

## How it works

Shunter is a merge queue for codebases where many producers, AI agents especially, open pull requests faster than CI can test them one at a time. It batches changes into trains, speculates one train ahead, and lands only exactly what CI tested, by fast-forward.

What sets it apart is a **judge in the loop**:

- **Rules guard safety.** A few invariants hold whatever happens: trunk only advances to tested states, lines land whole and bases first, a riding change can't move, and blame needs evidence.
- **Judgement handles the rest.** Where no mechanical rule decides (a failure nobody can be blamed for, a trunk that looks broken, retries that won't clear), the protocol defines a **judgement point**: what the judge sees, the actions it may choose, a deadline, and a safe default if it's silent.
- **Judgements become rules.** Every judgement is recorded with its evidence. Those that recur are promoted into mechanical rules, so the rulebook grows from observed judgement, not speculation.

## The arithmetic of flakiness

A flaky test fails sometimes on code that should pass it. Flakes are common in real test suites: Google has reported that about 1.5% of its test runs produce a flaky result, and that about 16% of its tests show some flakiness. Academic studies of open-source projects put the commonest causes as asynchronous waits, concurrency, and tests that depend on each other's order or leftover state.

On a human team, the usual answer is "just rerun it". A rerun costs a few minutes of one human's attention, the failure is someone's own pull request, and the cost is invisible. On an AI-agent team, the same rerun costs a whole CI run and holds up every pull request in the batch, and it happens all day.

The arithmetic: if a suite has *T* tests, each failing at random with probability *f* on correct code, a run on correct code passes with probability (1 − *f*)^*T*.

| Tests *T* | *f* = 0.001% | *f* = 0.01% | *f* = 0.1% |
|---|---|---|---|
| 500 | 99.5% | 95.1% | 60.6% |
| 1,000 | 99.0% | 90.5% | 36.8% |
| 5,000 | 95.1% | 60.7% | 0.7% |

So a 5,000-test suite where each test flakes once in a thousand runs almost never passes on correct code, however good the changes are. And this is *p*, the chance that a batch passes on its merits before any change in it is actually wrong. Everything else rests on it:

- **Batch size:** a batch also fails when any of its changes is wrong, so *p* only falls as batches grow.
- **Speculation depth:** a failure at position *j* in a chain of *D* speculative batches voids the *D − j* behind it, so the waste grows quickly as *p* falls.
- **Cost:** a batch needs 1/*p* runs on average to go green: 1.1 runs at 90%, 2 at 50%.

That's why Shunter treats flaky tests as part of the protocol, not as test hygiene. Each test gets a flake score from its history. Flakes are fixed in order of what they cost the queue, and quarantine is bounded by owners, expiry dates and a ratchet. Only known flakes are retried, test by test, and a judge decides whether a newly flaky test is a test bug or a product bug (see [`PROTOCOL.md`](PROTOCOL.md) §5.8 and J5).

## Status

- [`PROTOCOL.md`](PROTOCOL.md): the protocol, draft 0.2. It covers principles, invariants, the state machine of a line and of a train, the mechanism, judgement points, the failure model, a comparison with Bors, Zuul, GitHub's merge queue and Uber's SubmitQueue, and verification.
- [`state/`](state): the state machine as one pure Go function, `Next(state, event) (state, effects, error)`. Effects are data, illegal transitions are errors, and the protocol's transition tables are printed from its table.
- [`replay/`](replay): a replayer that runs a recorded history through the state machine and reports what it refuses, with an adapter for the train logs of a script-driven queue. Try it: `go run ./cmd/shunter-replay -stats stats.jsonl logs/*.out`.
- [`tla/Shunter.tla`](tla/Shunter.tla): the TLA+ skeleton, in which the judge is a nondeterministic choice within its menu, so model checking shows the invariants hold whatever the judge decides. [`tla/README.md`](tla/README.md) says how to run it.
- [`store/`](store): the one record (I8). An append-only, checksummed log of the events the machine accepted, with atomic snapshots, a single-writer lock, replay on start, and export and import for a cut-over, as a JSON document or as plain line files.
- [`daemon/`](daemon) and [`cmd/shunter`](cmd/shunter): the runner. One loop takes events from adapters, timers and the CLI, applies them through the store, and performs the machine's effects through the adapters. Effects are keyed, so a restart mid-effect is safe. The deadlines of the judgement points are timers with their defaults. `shunter serve` runs it, and `shunt` (the same binary) commands it: `queue`, `hold`, `release`, `cancel`, `status`, `export`, `import` and more.
- [`adapter/`](adapter): the interfaces to the forge, CI, git and notices, an in-memory fake world ([`adapter/fake`](adapter/fake)) for the daemon's end-to-end tests, and the real adapters:
  - [`adapter/github`](adapter/github): pull requests and CI over GitHub's REST API. CI's verdict is read from the check runs on the train's exact head; a cancelled or skipped run is no verdict, never a failure. A read-only mode refuses every write before it is sent.
  - [`adapter/gitrepo`](adapter/gitrepo): builds trains in a clone it owns (a linear rebase, or merges) and lands by a fast-forward push under a lease. A push of unknown outcome is settled by reading trunk.
  - [`adapter/notify`](adapter/notify): an append-only events file, and an optional command that delivers each notice.
  - [`adapter/hook`](adapter/hook): the repository's own rules as commands in any language, JSON in and JSON out: pre-flight, conflict resolution and blame. A hook advises; it never gives a verdict.
  - [`adapter/bound`](adapter/bound): every call bounded and retried with backoff, and every error classed as transient or permanent.
- [`hooks/`](hooks): generic example hooks, in Python, each tested with synthetic fixtures: a pre-flight of named, timed, recorded steps with a usefulness rule and blame by bisection over the train's lines; a conflict hook that regenerates the files a git attribute marks as generated; and blame by line from the files and directories a failure names. [`hooks/README.md`](hooks/README.md) says how a project plugs in its own.
- [`trunklock/`](trunklock): the trunk push lock, one `flock` that Shunter and every other pusher hold to push trunk, with a stale-lock rule that reports and never breaks.
- [`watch/`](watch): `shunter watch`, the outside watcher, run from a timer: is the daemon alive and making progress? It raises the notify hook when not.
- [`config/`](config): the one configuration file, in TOML. [`examples/shunter.toml`](examples/shunter.toml) documents every key. `shunter serve --config FILE` runs the daemon on the real adapters.
- [`shadow/`](shadow): `shunter shadow` runs beside a live queue with no side effect. It replays the live queue's train logs through the machine and logs what Shunter would have done, reads CI on each train's head through its own adapter and compares the outcome with what the live queue recorded, and puts the lines the live queue holds waiting through its own gate.
- [`docs/open-questions.md`](docs/open-questions.md): the gaps in the protocol that the daemon brought out, and what the code does about each meanwhile.
- [`docs/cutover.md`](docs/cutover.md): how a team moves from an ad-hoc script queue to Shunter by hard cut-over. [`docs/implementation-language.md`](docs/implementation-language.md): why the core is Go and the hooks are scripts.
- **The first implementation** was a set of shell scripts in daily production use on a multi-agent codebase. The protocol was drawn from it, and 0.2 from a review of where it failed.
- **CI** runs the Go tests, and TLC on the TLA+ model ([`.github/workflows`](.github/workflows)).
- **Next:** days of shadow running beside a live queue with the repository's hooks configured, the cut-over, and trace validation of the Go function against the TLA+ model.

## Why "Shunter"

In a railway yard, the shunter moves carriages and wagons about to make up trains, and decides what goes where.
