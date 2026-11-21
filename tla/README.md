# The TLA+ model

[`Shunter.tla`](Shunter.tla) is a skeleton of the state machine of [`PROTOCOL.md`](../PROTOCOL.md) §5.1. Its header says what it models and what it leaves out.

**Status: the module parses and passes SANY's semantic check.** TLC runs on it in CI ([`.github/workflows/tlc.yml`](../.github/workflows/tlc.yml)), on `Shunter.cfg`, with a pinned `tla2tools.jar` and a time limit. The job runs only when `tla/` changes.

## What it checks

| Property | Kind | Protocol |
|---|---|---|
| `FastForwardOnly` | action | I2: trunk only fast-forwards |
| `TestedTrunk` | invariant | I1, I9: nothing lands without a verdict on exactly its head |
| `ExactSpeculation` | invariant | I5: each train lands on the exact head it was built on |
| `Frozen` | invariant | I4: what lands is what was assembled |
| `EndedStaysEnded`, `CancelledNeverLands` | action, invariant | a cancelled train never lands |
| `HeldNeverSpent`, `HeldNotRiding` | action, invariant | a held line is never spent |
| `AssembledOnlyChecked` | invariant | I10: a line rides only on a green line check of the head it was assembled at |
| `GreenOnlyByACheck` | action | I10: a push is never taken as a fix; only a pass of the check on the present head sets a line's green |
| `OneRecord` | invariant | I8: a line rides exactly when one live train carries it |
| `EveryLineIsSpent` | liveness | L1: every line is eventually landed, returned or explicitly dropped |

The judge is a nondeterministic choice within its menu (`JudgeCharge`, `JudgeRequeue`, `JudgeRerun`, `Hold`, `Abort`). Safety holds whatever it chooses.

## Fairness

Liveness assumes, and the module states as `Fairness`:

- the runner assembles, checks and pushes when it can;
- the line checker answers for every waiting line, with a pass or a failure;
- CI eventually answers a run it is left to finish;
- J1's deadline passes and its default applies;
- every hold is eventually released;
- the adversary has a finite budget, `MaxFaults`: false greens, lost verdicts, refused pushes, pushes to riding changes, holds and aborts.

`MaxTrains` must be at least `Depth × (lines + MaxFaults)`, so the runner never runs out of trains. The module asserts it.

## Running it

You need Java 11 or later and `tla2tools.jar` from the [TLA+ releases](https://github.com/tlaplus/tlaplus/releases).

```
cd tla
java -XX:+UseParallelGC -cp tla2tools.jar tlc2.TLC -workers auto -deadlock -config Shunter.cfg Shunter.tla
```

To check syntax and semantics only, which takes a second:

```
java -cp tla2tools.jar tla2sany.SANY Shunter.tla
```

[`Shunter.cfg`](Shunter.cfg) is the small configuration: two lines, depth 2, one fault. Liveness checking is the costly part. For a quicker safety-only run, remove `EveryLineIsSpent` from `PROPERTIES`. For more confidence, raise the constants one at a time (three lines; two faults; depth 3), keeping `MaxTrains` at the bound above.

`-deadlock` turns off TLC's deadlock check: a queue whose lines are all spent has nothing left to do, which is its proper end. The CI job passes it.

**Run TLC where memory is plentiful,** such as a CI job. It keeps its state queue on disk under `states/`, and on a machine where that directory is in RAM a larger configuration can exhaust it.

## Next

- Add the gate: `offered`, `waiting-for-base` and `parked`, with restore.
- Add assembly: leave-outs and pre-flight failures.
- Add needs first (I3): a base per line, and a stacked line's check taken with its base beneath it.
- Bound a green line check's age against trunk. Here trunk moving never ends one.
- Validate traces: replay the Go state machine's transition log against this model, so the function and the model can't drift apart.
