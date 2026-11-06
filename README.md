# Shunter

**Shunter assembles trains of changes, lands them on trunk, and knows when to hold the line.**

## The problem

**Turning a high rate of locally-checked changes into one globally-tested history, when the global test is slow, expensive and noisy, and judgement is scarce.**

Two kinds of development team face this problem:

- **Human teams on legacy workflows:** developers open pull requests by hand, CI tests each one, and a human merges it.
- **AI-agent teams on agentic workflows:** many AI agents write code concurrently and open pull requests themselves, with one coordinator (a human or an AI) overseeing them.

Legacy merge workflows quietly rely on conditions that agentic workflows break:

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
- **Change-based test selection** runs only the jobs a change can affect, which lowers cost without weakening the invariant.
- **Flaky-test management** keeps a batch's chance of passing high. That chance limits every other knob (see *The arithmetic of flakiness* below).
- **Judgement** decides what rules can't, at defined points. The coordinator's attention is itself a limit on throughput, so recurring judgements are promoted into rules, and silence falls back to a safe default.

With unlimited money, a team could speculate on every plausible future trunk, and a change would land about one CI run after it was ready, whatever the arrival rate. Money buys latency through speculation; it never removes the need to test each change in the state it lands into. Shunter aims to get as close to that throughput as the CI budget allows.

## How it works

Shunter is a merge queue for codebases where many producers, AI agents especially, open pull requests faster than CI can test them one at a time. It batches changes into trains, speculates one train ahead, and lands only exactly what CI tested, by fast-forward.

What sets it apart is a **judge in the loop**:

- **Rules guard safety.** A few invariants hold whatever happens: trunk only advances to tested states, stacks land whole, a riding change can't move, and blame needs evidence.
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

That's why Shunter treats flaky tests as part of the protocol, not as test hygiene. Each test gets a flake score from its history. Flakes are fixed in order of what they cost the queue, and quarantine is bounded by owners, expiry dates and a ratchet. Only known flakes are retried, test by test, and a judge decides whether a newly flaky test is a test bug or a product bug (see [`PROTOCOL.md`](PROTOCOL.md) §5.7 and J5).

## Status

- [`PROTOCOL.md`](PROTOCOL.md): the protocol, draft 0.1. It covers principles, invariants, mechanism, judgement points, failure model, a comparison with Bors, Zuul, GitHub's merge queue and Uber's SubmitQueue, and verification.
- **Reference implementation:** a set of shell scripts in daily production use on a multi-agent codebase. They're being extracted here.
- **Next:** a TLA+ model of the protocol, in which the judge is a nondeterministic choice within its menu, so model checking shows the invariants hold whatever the judge decides.

## Why "Shunter"

In a railway yard, the shunter moves carriages and wagons about to make up trains, and decides what goes where.
