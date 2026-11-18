# Cutting over from a script queue

*How a team moves from an ad-hoc merge queue, usually a set of scripts and state files, to Shunter. Generic: adapt the names to your own queue.*

## Why a hard cut-over

There are two ways to change queues: run both for a while, or stop one and start the other. Shunter recommends the second.

- **Two writers break I7.** Trunk has one writer. Two queues that each believe they are it will sooner or later push at the same moment, and one of them will have tested a trunk that no longer exists.
- **A script queue's state can't be shared.** It lives in many files, in lines identified by their text, and in the memory of running processes. A second system can't read it reliably while the first still writes it.
- **The stop is short.** Producers keep working throughout. They only can't land, for about as long as two CI runs.

What makes a hard cut-over safe is that both queues move trunk only by fast-forward to a tested head. There is never anything to undo on trunk, whichever queue made the last landing.

## Before the day

1. **Freeze the old queue to safety fixes.** Every other change to it is effort that will be thrown away, and a moving target for the replay below.
2. **Replay its history.** Run the old queue's train logs through the state machine:

   ```
   go run ./cmd/shunter-replay -stats stats.jsonl logs/*.out
   ```

   The report lists the histories the machine refuses, by kind: a change returned with no evidence, a train with no recorded outcome, a line that vanished without a word. Each is one of three things. Decide which, for every kind, before the day:
   - a defect of the old queue that Shunter is meant to end: nothing to do;
   - a habit your team relies on that the protocol doesn't allow: change the habit, or raise it against the protocol;
   - something the adapter can't see in your logs: note it and move on.
3. **Cut the repository's own rules out as hooks.** Conflict resolution, the pre-flight steps and the blame heuristics stay yours, in whatever language they're in ([`implementation-language.md`](implementation-language.md)). On day one they should be the same code the old queue ran, moved and not rewritten, so that only one thing changes.
4. **Run in shadow.** For a few days Shunter reads the same inputs, lands nothing, and logs what it would have assembled, blamed and judged. Review the differences from what the old queue did, daily. Shadow mode must be unable to push: give it no credentials that can write trunk.

   ```
   shunter shadow --config shunter.toml --lines waiting.list --stats stats.jsonl \
       --since 6h --effects would-do.jsonl logs/*.out
   ```

   It has no git adapter and no notifier, and its forge client refuses every request but a read before it is sent. The report counts, for every train that opened a pull request, whether Shunter's own reading of the checks on the train's head agrees with the outcome the old queue recorded, and names each divergence by kind: a landing whose head shows no passing verdict, a failure charged on a run that was cancelled, a failure split or returned with nobody named. It lists the effects the machine would have asked for, and puts the lines waiting now through Shunter's gate.
5. **Make both queues take the same lock.** One exclusive lock that either runner must hold to push. Then both can never write, even by mistake.
6. **Write and test the export.** A command that writes Shunter's waiting lines in the old queue's own format. Rollback depends on it, so prove it works before you need it.
7. **Choose the moment.** A quiet hour with a short queue, with no park in force and no hold you don't understand.

## What is drained, and what is carried

The rule: **drain what is in motion; carry what is at rest; drop what is only memory.**

| In the old queue | At cut-over | Why |
|---|---|---|
| Trains in flight | **Drained.** Stop taking lines, and let them finish under the old rules | A train half-run can't be handed over: its state is in the old runner's memory |
| Waiting lines | **Carried, by offering them again,** in their order | The gate runs again on each (§5.2), so nothing enters on the old queue's say-so |
| "Must ride alone", "goes first" | Carried as flags on the offer | Only for lines that are still live |
| Lines waiting for a base | Offered again | Shunter holds them as waiting-for-base and lines them itself |
| Held changes | Carried as holds, each with its reason | A hold with no reason is asked about, not carried |
| Lines set aside | Carried as parked, **each with a restore condition** | "Someone will put them back" isn't a condition. If you can't state one, restore them before the cut |
| Answers owed to producers | Settled during the drain; the rest carried by name | No producer should be left waiting on a reply from a system that has stopped |
| The producers' inbox | **Not carried.** A new inbox, for the new protocol; the old one is frozen | Otherwise an old offer is read twice |
| Returned changes | Archived; any that isn't final is imported | |
| The known-flake list and its history | Imported | The flake score needs its window (§5.8) |
| Strikes, retry counters, stop flags | **Dropped.** They start empty | They're the old queue's memory of its own troubles (§5.10) |
| Statistics | Keep the same row shape, so dashboards carry on | |
| Pre-flight catch history | Left where it is; the hook keeps writing it | |

## The day

1. **Announce the stop.** Producers keep working. They can't land for about two hours.
2. **Drain.** Stop whatever feeds the old queue, then move its waiting list aside under its own lock. The old runner finishes what rides and stops on an empty list. Allow one CI run, plus one rerun. **A line the old runner puts back during the drain goes into the list you moved aside from, not the one you hold:** a train that fails while draining returns its lines to the old list. Copy the waiting list before the drain and again after the runner stops, and carry the union, less what landed. A script queue that rewrites its list in place can lose a line put back in that window, and nothing records the loss.
3. **Snapshot.** Archive every state file of the old queue as it stands.
4. **Stop the old services.** Stop them; don't remove them.
5. **Import.** Offer the carried lines in order, then place the holds and parks. Compare Shunter's status with the snapshot line by line. Every line of the old queue must be accounted for: waiting, held, parked, or deliberately returned with a reason. This is I8's first test.
6. **Start Shunter** under the shared lock.
7. **Prove it.** Offer one harmless line, a change to documentation, and watch it through every state to landed. Then open the inbox.

## Rollback

**Kept ready:** the old scripts untouched, their services stopped and not removed, the snapshot, and the tested export.

**The act:**

1. Stop Shunter. Abort the trains it has in flight (§5.9), so their lines are waiting.
2. Export the waiting lines in the old queue's format, into its list.
3. Start the old services.
4. Tell producers which offers, made through the new inbox, they must make again.

**Why it's cheap:** there's nothing to undo on trunk, and the shared lock means the two were never both writing.

**When to roll back.** Decide the triggers before the day, so that nobody has to argue for them during it:

- **any doubt that a landing was tested:** at once;
- **a line whose state the status can't account for;**
- **no landing for an hour** with lines waiting and CI healthy.

A rollback costs about a quarter of an hour, plus the offers made again. A wrong landing costs more than that, so the first trigger has no threshold.

## After

- Keep the old scripts for a week, stopped. Then delete them, so that nobody runs them by habit.
- Replay Shunter's own transition log through the state machine nightly. It should refuse nothing, and anything it does refuse is a bug in one of the two.
- Review what the judge decided in the first week (§2.6). The judgements that recur are the next rules.
