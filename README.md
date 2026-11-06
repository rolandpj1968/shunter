# Shunter

**Shunter assembles trains of changes, lands them on trunk, and knows when to hold the line.**

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
