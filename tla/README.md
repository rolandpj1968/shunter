# The TLA+ model

[`Shunter.tla`](Shunter.tla) is a skeleton of the state machine of [`PROTOCOL.md`](../PROTOCOL.md) §5.1. Its header says what it models and what it leaves out. **It runs ahead of the protocol:** it carries the proposal of [`docs/queue-as-bag.md`](../docs/queue-as-bag.md) (the queue as a bag, starving lines, verdicts by checker, no train built on a train decided red), which the owner has not yet ruled on.

**Status: the module parses and passes SANY's semantic check.** TLC runs on it in CI ([`.github/workflows/tlc.yml`](../.github/workflows/tlc.yml)), on `Shunter.cfg`, with a pinned `tla2tools.jar` and a time limit. The job runs only when `tla/` changes.

## What it checks

| Property | Kind | Protocol |
|---|---|---|
| `FastForwardOnly` | action | I2: trunk only fast-forwards |
| `TestedTrunk` | invariant | I1, I9: nothing lands without a verdict on exactly its head |
| `ExactSpeculation` | invariant | I5: each train lands on the exact head it was built on |
| `Frozen` | invariant | I4: what lands is what was assembled |
| `EndedStaysEnded`, `CancelledNeverLands` | action, invariant | a cancelled train never lands |
| `SpentStaysSpent` | action | a spent line stays spent, but for a returned line offered again |
| `HeldNeverSpent`, `HeldNotRiding` | action, invariant | a held line is never spent |
| `NeededRebaseOnly` | action | I13: a head moves only by its owner's push, or by the runner's rebase of a waiting line in conflict |
| `NoRideInConflict` | invariant | a line in conflict with trunk rides no train |
| `LightOnlyIfAllInert` | invariant | I12: a train has the light tier only while every line in it is inert |
| `NeedsFirst` | invariant | I3: a line rides only behind its base, and lands only after it |
| `JudgedOnlyAtHead` | action | §5.3: a train enters J1 or ends as failed only when it is first in the chain, on trunk |
| `ChargedOnlyAtPresentHead` | action | a charge is only ever on the head the line has at that moment: a line check's verdict on a head since replaced charges nobody. A charge is otherwise only dropped, as void |
| `VerdictsByPresentChecker` | action | proposed I14: a line is taken only on a green of the present checker; a charge is made by the present checker or the judge; a charge is dropped only when its checker has been replaced |
| `NoJump` | action | proposed I15: a line that could ride is left waiting, while a line behind it in the order is taken, only for a manual priority or for a starving line and what it stands on |
| `ForcedFirst` | action | proposed L4: while a starving line is forced, every train assembled holds it and what it stands on, and other lines may ride behind it (owner's ruling of 26 Nov; it rode alone, with the chain empty); only prioritised lines go before it |
| `CountFallsOnlyByACharge` | action | a line's count of cycles it did not cause goes down only by a failure of its own |
| `NoStackOnRed` | action | no train is assembled while a train in the chain is decided red: its failure kept, or waiting at J1 |
| `NoRideAtChargedHead` | invariant | I11: a charged line never rides again at the head it was charged on |
| `AssembledOnlyChecked` | invariant | I10: a line rides only on a green line check of the head it was assembled at |
| `GreenOnlyByACheck` | action | I10: a push is never taken as a fix; only a pass of the check on the present head, by the present checker, sets a line's green |
| `OneRecord` | invariant | I8: a line rides exactly when one live train carries it |
| `EveryLineIsSpent` | liveness | L1: every line is eventually landed, returned or explicitly dropped |

The judge is a nondeterministic choice within its menu (`JudgeCharge`, `JudgeRequeue`, `JudgeRerun`, `Hold`, `Abort`, `Prioritise`). Safety holds whatever it chooses.

## Fairness

Liveness assumes, and the module states as `Fairness`:

- the runner assembles, checks and pushes when it can;
- the line checker starts a check of every waiting line, and answers it with a pass or a failure, or drops a verdict that is for a head since replaced;
- CI eventually answers a run it is left to finish;
- a failure kept behind an undecided train is judged once its train is first;
- a line whose base is lost is returned;
- a line in conflict is rebased (every conflict in the model is one the runner can resolve);
- a charge by a checker since replaced is voided;
- J1's deadline passes and its default applies;
- every hold is eventually released;
- the adversary has a finite budget, `MaxFaults`: false greens, lost verdicts, refused pushes, pushes to riding changes, holds, aborts, manual priorities, changes of checker, and a line checker that dies with its claim standing or gives it up with no verdict (`ClaimExpired`).

`MaxTrains` must be at least `Depth × (lines × (MaxFaults + 1) + MaxFaults)`, so the runner never runs out of trains: a change of checker voids charges, and so can give every line back once. The module asserts it.

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

[`Shunter.cfg`](Shunter.cfg) is the small configuration: two lines, neither stacked, one of them inert, depth 2, one fault, and a line starving at its first cycle (`StarveAt = 1`), so that one fault reaches the forced line. [`Stack.cfg`](Stack.cfg) is the same with the second line stacked on the first; it runs on [`Stack.tla`](Stack.tla), which names the stack. [`Reoffer.cfg`](Reoffer.cfg) is `Shunter.cfg` with two faults, which is what it takes to push a fix to a charged line and offer it again, and `StarveAt = 2`, so that a count is halved (about a million states and three minutes with its liveness check, so it is CI's to run, not a laptop's). CI runs all three, one job each. Liveness checking is the costly part. For a quicker safety-only run, remove `EveryLineIsSpent` from `PROPERTIES`. For more confidence, raise the constants one at a time (three lines; two faults; depth 3), keeping `MaxTrains` at the bound above.

`Witness-forced.cfg` and `Witness-void.cfg` are not checks: each names a property that is false of the model (no starving line is ever forced past an older one; no charge is ever voided), and CI requires TLC to report it violated, so that the properties about forced lines and void charges can't hold emptily.

`-deadlock` turns off TLC's deadlock check: a queue whose lines are all spent has nothing left to do, which is its proper end. The CI job passes it.

**Run TLC where memory is plentiful,** such as a CI job. It keeps its state queue on disk under `states/`, and on a machine where that directory is in RAM a larger configuration can exhaust it.

## Next

- Add the gate: `offered`, `waiting-for-base` and `parked`, with restore.
- Add assembly: leave-outs and pre-flight failures.
- Bound a green line check's age against trunk. Here trunk moving never ends one.
- Validate traces: replay the Go state machine's transition log against this model, so the function and the model can't drift apart.
