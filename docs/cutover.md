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
3. **Cut the repository's own rules out as hooks.** Conflict resolution, the pre-flight steps and the blame heuristics stay yours, in whatever language they're in ([`implementation-language.md`](implementation-language.md)). On day one they should be the same rules the old queue ran, moved and not rewritten, so that only one thing changes. [`hooks/`](../hooks) has generic ones to start from: a pre-flight of named, timed, recorded steps with a usefulness rule and blame by bisection; a conflict hook that regenerates files a git attribute marks; and blame by line from the files a failure names. Configure them to your rules, and test them against fixtures of your own past failures, made synthetic.
4. **Run in shadow.** For a few days Shunter reads the same inputs, lands nothing, and logs what it would have assembled, blamed and judged. Review the differences from what the old queue did, daily. Shadow mode must be unable to push: give it no credentials that can write trunk.

   ```
   shunter shadow --config shunter.toml --lines waiting.list --stats stats.jsonl \
       --since 6h --effects would-do.jsonl logs/*.out
   ```

   It has no git adapter and no notifier, and its forge client refuses every request but a read before it is sent. The report counts, for every train that opened a pull request, whether Shunter's own reading of the checks on the train's head agrees with the outcome the old queue recorded, and names each divergence by kind: a landing whose head shows no passing verdict, a failure charged on a run that was cancelled, a failure split or returned with nobody named. It lists the effects the machine would have asked for, and puts the lines waiting now through Shunter's gate.
5. **Make both queues take the same lock.** One exclusive lock that either runner must hold to push trunk. Then both can never write, even by mistake. See [The trunk lock](#the-trunk-lock) below.
6. **Write and test the export.** Shunter writes its queue at rest as plain line files ([The line files](#the-line-files) below), the shape a script queue keeps. Map them into the old queue's own files with a small script, and round-trip it: export, map, read back with the old queue's tools, and compare. Rollback depends on it, so prove it works before you need it.
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
2. **Drain.** Stop whatever feeds the old queue, then move its waiting list aside under its own lock. The old runner finishes what rides and stops on an empty list. Allow one CI run, plus one rerun.

   **Every line moved during the drain must be journalled, and the waiting list compared before and after.** A drain is the moment a queue is most likely to lose a line without a word. A train that fails while draining puts its lines back, a split puts back two halves, a conflict leaves one out: each goes into the list you moved aside from, not the one you hold, and a script queue that rewrites its list in place can drop a line put back in that window. So:

   - **Journal every move.** Each line the old runner takes, puts back, splits, leaves out or lands during the drain is appended, with the time and what became of it, to a journal that nothing rewrites. If the old runner doesn't journal its moves, wrap its list writes so that they do, before the day.
   - **Snapshot before the drain starts:** the waiting list (the one you move aside), the held and parked lists, and the lines riding in trains.
   - **Snapshot again after the runner stops:** the old list, which now holds whatever was put back.
   - **Carry the union** of the list you moved aside and the old list after the runner stops, less what landed.
   - **Compare.** Every line that was waiting or riding when the drain began is now exactly one of: landed on trunk, in the carried union, or returned to its owner with a reason in the journal. Every line in the carried union was in the first snapshot, or is in the journal as put back. A line that is none of these was lost: find it before going on.

3. **Snapshot.** Archive every state file of the old queue as it stands.
4. **Stop the old services.** Stop them; don't remove them.
5. **Import.** Offer the carried lines in order, then place the holds and parks: write them as line files and `shunt import --line-files DIR`. Compare Shunter's status with the snapshot line by line. Every line of the old queue must be accounted for: waiting, held, parked, or deliberately returned with a reason. This is I8's first test.
6. **Start Shunter** under the shared lock, and start the watcher's timer (`shunter watch`, below).
7. **Prove it.** Offer one harmless line, a change to documentation, and watch it through every state to landed. Then open the inbox.

## Rollback

**Kept ready:** the old scripts untouched, their services stopped and not removed, the snapshot, and the tested export.

**The act:**

1. Stop Shunter. Abort the trains it has in flight (§5.9), so their lines are waiting.
2. Export the waiting lines, `shunt export --offline --store DIR --line-files OUT`, and map them into the old queue's lists. Compare before starting it: every line in the export is in the old queue's lists.
3. Start the old services.
4. Tell producers which offers, made through the new inbox, they must make again.

**Why it's cheap:** there's nothing to undo on trunk, and the shared lock means the two were never both writing.

**When to roll back.** Decide the triggers before the day, so that nobody has to argue for them during it:

- **any doubt that a landing was tested:** at once;
- **a line whose state the status can't account for;**
- **no landing for an hour** with lines waiting and CI healthy.

A rollback costs about a quarter of an hour, plus the offers made again. A wrong landing costs more than that, so the first trigger has no threshold.

## The trunk lock

One file that every pusher of trunk holds, with an exclusive `flock(2)`, for the length of a push: Shunter, the old queue during the cut-over and after a rollback, and a person pushing a hotfix by hand. Shunter takes it for each push when `[git] trunk_lock` is set ([`trunklock`](../trunklock)); a shell script takes it with `flock(1)`:

```sh
# Hold the trunk lock for exactly the push, waiting at most five minutes.
flock -w 300 /var/lib/shunter/trunk.lock \
    git push --no-verify origin "$sha:refs/heads/main"
```

- **A holder that dies frees the lock.** The kernel releases a flock when the last descriptor of the open file closes, however the process ends, so there is no lock left by a dead process to clean up.
- **Never hold it on a descriptor a long-lived child inherits.** A child that outlives the holder (a background process, a daemon a test started) keeps the lock. `flock FILE command` is safe: the lock lasts as long as the command. Opening the file on a descriptor of the script itself and running other commands under it is not.
- **Never remove or replace the file.** A lock on a replaced file is no lock. Shunter checks, after taking it, that the file it locked is still the one at the path.
- **The record.** While it holds the lock, Shunter writes one line into the file: `pid=<n> host=<name> who=<name> since=<RFC 3339>`, and clears it on release. A script may write the same, for people and for the stale-lock rule: `echo "pid=$$ host=$(hostname) who=legacy since=$(date -u +%FT%TZ)" > /var/lib/shunter/trunk.lock` while it holds the lock.
- **The stale-lock rule.** A lock held by an orphan (its record names a process that is gone, on this host) or held longer than `trunk_lock_max_hold` (default 15 minutes) is stale. A stale lock is **reported, never broken**: breaking a lock that a live process holds is exactly how two writers push at once. Shunter's push fails, as a transient failure the daemon retries and escalates, with the holder named; the watcher raises it. A person ends the holder (`fuser` or `lsof` on the file finds it), and the lock is free.

## The line files

`shunt export --line-files DIR` writes the queue at rest as three files, and `shunt import --line-files DIR` reads them:

```
# waiting.list: one line a line, in queue order; held lines too, in their places
b 101 102
b 103
b 104 alone
# held.list: the held lines, each with its reason
b 103 -- waits on a design call
# parked.list: each park with its restore condition and reason, then its lines
park j1-t9 -- deadline: the judge reviews -- parked at J1
b 106 107
```

A line is its changes in stack order; `alone` marks a line that must ride alone. They name changes only: on the way in, Shunter reads each change's head and base from the forge again, and every line passes the gate again, so nothing enters on the file's say-so. What the files can't carry is dropped on the way out: a line's fix flag, and a hold on a line that is also parked (the park is kept). The round trip is tested ([`store/lines_test.go`](../store/lines_test.go)).

## Watching

Run `shunter watch --config FILE` from a timer, every five minutes. It checks the daemon from outside, sharing no code path with its loop: the store's lock is held (a daemon runs); the daemon answers a status command on its socket (its loop turns); it hasn't restarted again and again; no effect is owed, and no live train has stayed in one state, past its deadline plus grace; lines aren't waiting with nothing in flight and nothing recorded; the trunk lock isn't stale. Each finding goes to the notify hook once, again every hour while it stands, and once more when it clears; the command exits 1 while anything is wrong. A finding is a reason to look, and the rollback triggers below still decide a rollback.

## After

- Keep the old scripts for a week, stopped. Then delete them, so that nobody runs them by habit.
- Replay Shunter's own transition log through the state machine nightly. It should refuse nothing, and anything it does refuse is a bug in one of the two.
- Review what the judge decided in the first week (§2.6). The judgements that recur are the next rules.
