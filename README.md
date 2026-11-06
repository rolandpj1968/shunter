# Shunter

**Shunter assembles trains of changes, lands them on trunk, and knows when to hold the line.**

## The problem

**Turning a high rate of locally-checked changes into one globally-tested history, when the global test is slow, expensive and noisy, and judgement is scarce.**

Two kinds of development team face this problem:

- **Human teams on legacy workflows:** developers open pull requests by hand, CI tests each one, and a person merges it.
- **AI-agent teams on agentic workflows:** many AI agents write code concurrently and open pull requests themselves, with one coordinator (a person or an AI) overseeing them.

Legacy merge workflows quietly rely on conditions that agentic workflows break:

1. **Arrival rate against test throughput.** A human team opens a few pull requests a day, and CI can test each one separately with time to spare. AI agents open pull requests faster than CI can test them one at a time. When pull requests arrive faster than CI can test them, the queue grows without bound, so an agentic workflow *must* test pull requests in batches.
2. **Batching destroys attribution.** When CI tests one pull request per run, a failure belongs to that pull request. When CI tests twelve pull requests in one run, the failure could belong to any of them. Bisecting the batch costs extra CI runs, and blaming from the evidence in the logs is heuristic.
3. **Rare failures become constant.** A test that fails at random 1% of the time costs a human team a shrug once a month, and costs an AI-agent team a failed batch every hour. A batch passes only if every pull request in it passes, so the chance of a green batch falls exponentially with batch size. In an agentic workflow, flaky tests, errors from the forge (GitHub, for example) and tests leaking state into each other decide throughput.
4. **Human teams supply judgement for free.** On a human team, each author watches their own pull request and knows whether a failure is theirs, and the team notices "trunk is red, hold off merging" in its chat. AI agents share no such awareness. Each agent optimises locally ("green on my branch, so queue it"), no agent owns the shared state (trunk's health, the shared test fixtures, the generated files), and agents retire in the middle of tasks.
5. **Shared resources are contended at machine speed.** AI agents work without pause, so they hit the same generated files, shared test fixtures and CI runners continuously. Conflicts that a human team meets once a quarter, an AI-agent team meets several times a day.
6. **Judgement becomes the bottleneck.** On a human team, judgement is spread across every developer. On an AI-agent team, judgement is concentrated in the coordinator, whose attention is limited, while the number of decisions grows with the agents' throughput.

In short: **legacy merge workflows assume that verification is cheap relative to the rate of new pull requests, and that judgement is abundant and spread across the team. In agentic workflows, verification is the bottleneck, and judgement is scarce and concentrated in the coordinator.** Shunter's mechanism attacks the first problem: batching, speculation and change-based testing. Shunter's judgement design attacks the second: the coordinator's judgement is spent only where rules can't decide, each decision has a deadline and a safe default, and judgements that recur are promoted into rules.

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
