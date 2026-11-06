# Shunter

**Shunter assembles trains of changes, lands them on trunk, and knows when to hold the line.**

## The problem

**Turning a high rate of locally-checked changes into one globally-tested history, when the global test is slow, expensive and noisy, and judgement is scarce.**

Human merge workflows solve the same problem, but they quietly rely on conditions that many concurrent agents break:

1. **Arrivals are far below test throughput.** A team opens a few PRs a day, and testing each one alone is fine. Agents open them faster than CI can test them one at a time. When arrivals exceed the service rate, the queue grows without bound, so you *must* batch.
2. **Batching destroys attribution.** One PR, one CI run: a failure is that PR's. Twelve PRs in one run: whose failure is it? Bisection costs extra runs, and blame from evidence is heuristic.
3. **Rare failures become constant.** A test that flakes 1% of the time is a monthly shrug for a team, and hourly for agents. A batch passes only if all of it does, so the odds of a green batch fall exponentially with its size. Flakes, forge errors and tests leaking state decide throughput.
4. **People supply judgement for free.** An author watches their PR and knows whether a failure is theirs; a team notices "trunk is red, hold off" in chat. Agents share no such awareness. Each one optimises locally ("green on my branch, queue it"), nobody owns the shared state, and agents retire mid-task.
5. **Shared resources are contended at machine speed.** Generated files, shared test fixtures and the CI pool are hit continuously, so conflicts a team meets once a quarter happen several times a day.
6. **Judgement becomes the bottleneck.** In a team, judgement is spread across everyone. With agents, it's concentrated in a coordinator with limited attention, while the number of decisions grows with throughput.

In short: **human merge protocols assume verification is cheap relative to arrivals, and that judgement is abundant and spread across everyone. With agents, verification is the bottleneck, and judgement is scarce and centralised.** Shunter's mechanism attacks the first: batching, speculation and change-based testing. Its judgement design attacks the second: judgement spent only where rules can't decide, with deadlines, safe defaults, and recurring judgements promoted into rules.

## How it works

Shunter is a merge queue for codebases where many producers, AI agents especially, open pull requests faster than CI can test them one at a time. It batches changes into trains, speculates one train ahead, and lands only exactly what CI tested, by fast-forward.

What sets it apart is a **judge in the loop**:

- **Rules guard safety.** A few invariants hold whatever happens: trunk only advances to tested states, stacks land whole, a riding change can't move, and blame needs evidence.
- **Judgement handles the rest.** Where no mechanical rule decides (a failure nobody can be blamed for, a trunk that looks broken, retries that won't clear), the protocol defines a **judgement point**: what the judge sees, the actions it may choose, a deadline, and a safe default if it's silent.
- **Judgements become rules.** Every judgement is recorded with its evidence. Those that recur are promoted into mechanical rules, so the rulebook grows from observed judgement, not speculation.

## Status

- [`PROTOCOL.md`](PROTOCOL.md): the protocol, draft 0.1. It covers principles, invariants, mechanism, judgement points, failure model, a comparison with Bors, Zuul, GitHub's merge queue and Uber's SubmitQueue, and verification.
- **Reference implementation:** a set of shell scripts in daily production use on a multi-agent codebase. They're being extracted here.
- **Next:** a TLA+ model of the protocol, in which the judge is a nondeterministic choice within its menu, so model checking shows the invariants hold whatever the judge decides.

## Why "Shunter"

In a railway yard, the shunter moves carriages and wagons about to make up trains, and decides what goes where.
