# The queue as a bag

*A design note, adopted by the owner's ruling of 24 November (§9). It answers five pieces of feedback from the queue this protocol grew from: waiting lines are a bag and assembly picks by policy; stacks drain from the bottom; a starving line rides alone; a verdict belongs to the version of the checker that gave it; and a train decided red is cut short and never built on. The model in [`tla/`](../tla/) carries the proposal so that CI checks it. [`PROTOCOL.md`](../PROTOCOL.md) and the daemon take it item by item, as §10 lists.*

## 1. The problem

**Order is a position, and positions are edited.** §5.1 says "lines wait in the order they were offered, and a line that comes back takes the place it had". The daemon holds that as `Line.Seq`. The script queue this protocol grew from held it as a file read top to bottom, and then grew four ways to edit it: a list of bounced lines that go to the front, a put-front list, a rank that sent an inherited failure's lines to the front, and reordering by hand. Each was added for a good case and each jumped lines that had no claim: the bounced list put a line in conflict ahead of older green lines, and would have done the same for a line returned for its own defect.

**Nothing bounds how often a line goes round.** One line went eleven hours and twenty-one conflict verdicts: each landing reopened its conflict before it could reach a train. L2 promises that a line with no fault of its own eventually lands, and nothing in the mechanism makes it so.

**A verdict outlives the checker that gave it.** A line was charged by a line check that had a defect. The check was fixed, and the line kept being returned, because a red verdict was "current while the heads are unchanged". Nobody can tell which earlier verdicts a changed checker would give again.

**A train runs on after it is decided, and is built on.** A train whose CI has a failure that no rerun can clear still runs to the end, and the next train is built on it.

## 2. The bag

**Waiting lines have no place.** A waiting line carries facts, and assembly reads them. Nothing stores an order, so nothing can edit one.

| Fact | Set | Changed by |
|---|---|---|
| **born** | the time of the line's first offer | nothing, ever: not a return, a new offer, a rebase or a push |
| **count** | 0 | cycles the line didn't cause add one; a failure of its own halves it (§4) |
| **queue time** | 0 | runs while the line waits or rides; stops while it is held, parked or with its owner, and while the queue is paused (§4) |
| **priority** | none | a manual act, with a reason and an expiry (below) |

The rest of what assembly reads is already a field of the line: its base, its changes, its line check and the heads it was taken on, whether it is in conflict, stale, or must ride alone.

**A line's age is born, and coming back costs nothing and gains nothing.** A line that rode a train which went void, was left out, or was returned and offered again is exactly as old as before. That retires "takes the place it had" and "a new place in the queue" (§5.1) together: both describe an edit to a position. It also retires every reason the script queue had to put a line "at the front": an old line is near the front because it is old.

**The one override is a manual priority.** A person or the judge may give a line a priority. The act names the line, a reason and an expiry, and is recorded like a hold (I8). It covers the lines the named line is stacked on, since they must ride first. It ends at its expiry, when it is taken back, or when the line is spent; an expired priority is simply no longer read. A priority without a reason and an expiry is refused. Nothing else lets a line pass an older one that could ride.

## 3. The assembly policy

Assembly is a pure function: from the bag, the chain, the settings and the time, to the lines of the next train and, for every waiting line not taken, the reason. The reasons are shown in the status, so that "why is my line not riding" has an answer that needs no log.

**A line can ride** when it is waiting, not held, parked or stale, not in conflict, holds a current green line check (I10, and §5 below), and its base has landed, rides in an earlier train, or is taken into this train ahead of it (I3).

**The order.** Lines are taken in this order, and a line that can't be taken is skipped, with its reason, as today (§5.3):

1. **Lines with a manual priority**, oldest first.
2. **The forced line**, if a line is starving (§4). While one is, this step and step 1 are the whole policy.
3. **Everything else, by the age of its stack.** A line's key is the born of the bottom waiting line of its stack, then its depth in the stack. An unstacked line's key is its own born.

**Stacks drain from the bottom.** Step 3's key puts a whole waiting stack where its bottom is, bottom first. A stack then rides in one train when the caps allow, and in consecutive trains when they don't, and each landing leaves less to rebuild. Under the order of offers alone, the upper lines of a stack wait behind every unrelated line offered in between, trunk moves under them, and a four-deep stack was rebuilt four times over. A line counts as stacked only by ancestry, which the gate already reads (§5.2), so a stack can't be declared to gain a place. The producer's side of this is P2: stack only when necessary, and keep a stack shallow unless its base already rides.

**What may skip a line** is unchanged: it shares files with a line already taken, it must ride alone, a cap is reached (lines, or changes), or its base was skipped. A skip is not a jump: it has a stated reason, the line stays as old as it was, and the caps' own rule stands (a line that alone reaches a cap rides alone).

**The line checker reads the same order.** Checks are started in policy order, the forced line's first. Otherwise the oldest line can wait behind the checker instead of behind the queue.

**Size and freshness.** The caps of J4 are the only use of size. A green line check older than the queue's age bound is refreshed in policy order; it isn't a reason to pass the line.

## 4. Starving lines ride alone

**The count.** A line's count goes up by one for each cycle that riding alone and first would have avoided:

- its line check finds it in conflict with trunk, because trunk moved against it;
- its train ended because of another line in it: one was charged, pushed to, held or withdrawn, or the train was split at J1;
- its train was discarded because a train ahead didn't land;
- it was left out at assembly because it didn't fit with the lines taken.

It doesn't go up for a no-verdict or an infrastructure failure of its own train (riding alone wouldn't have helped; §5.10's budget and J3 own that), for an abort of its own train (the judge's act), when its base is lost, or when it is left out at assembly because its own head moved (PROTOCOL.md L39: it is re-keyed at the new head, once, and riding alone wouldn't have helped).

**A failure of the line's own halves the count.** A red line check (L33) or a charge in a train (L17) means the line needs its owner's fix, not priority. Halving, and not clearing, keeps a line that alternates between real fixes and other lines' conflicts from starving. Nothing else lowers the count: not a push, a rebase, a new offer or a hand-over. This is an exception to §5.10, whose budgets reset when a head moves; a rebase moves the head of exactly the line this count exists for.

**Starving.** A line is starving when its count reaches `starve_after_cycles`, or, as the backstop, when its queue time reaches `starve_after_hours`. Both are settings, and 0 turns either off.

**The forced line.** One line is forced at a time: the oldest starving line that is the queue's to move (it waits, and nothing beneath it is held, parked or with its owner). While a line is forced:

1. **No other train is assembled,** but one of prioritised lines. Trains already riding finish; they are at most D, so they can reopen the forced line's conflict at most D more times.
2. **The runner makes it ready.** If it is in conflict, the rebase hook is asked at once; its line check goes first, against trunk as it is.
3. **It rides alone, first in the chain:** in a train of that line and whatever beneath it in its stack still waits, assembled when the chain is empty. No line beside it can fail it, and no train ahead can discard it. Other trains may be built behind it as usual.
4. **If it fails then, the failure is its own.** It is charged by elimination (J1's rule for a train of one line), its count is halved, and it goes to its owner.

**When the runner can't make it ready, it is raised at once.** A conflict the hook can't resolve needs a person. The line goes back to its owner, uncharged, so the queue moves; it keeps its age, its count and its starving for when it is offered again, once it merges; and it is raised to the judge as starving and stuck, without waiting for the owner's deadline (§7). A line with no live owner is the judge's to hand over. This is how an old, ownerless change gets through in time: the queue does everything that needs no person, and what does need one is asked for at once and by name.

**Conflicts by kind** (owner's ruling, 24 Nov). The two kinds of conflict go different ways. A conflict with **trunk** that the rebase hook can't resolve sends the line back to its owner, forced or not: only a person can fix it, and riding alone can't make it merge. Offered again once it merges cleanly, it keeps its first offer's age. A conflict with **other lines of a batch** leaves the line out of that batch, and it stays in the bag at its age; the count above rises, and at three cycles it rides alone and first, where no other line can conflict with it.

**Many lines starving at once is not starvation.** After a long outage every line may pass the age bound together, and forcing them one by one would serialise the queue when it is furthest behind. Three things bound that: queue time doesn't run while the queue is paused or parked (and J3's default for a persistent failure is a pause); one line is forced at a time, and the others ride normally behind it; and more than `starve_alert` starving lines at once is raised to the judge (J4), who may raise the thresholds.

## 5. A verdict is its checker's

**Every stored verdict names the checker that gave it and that checker's version.** A verdict is read only through one test: is its checker's version the present one? If not, the verdict isn't there. Nothing is voided by an act, so nothing can be forgotten or voided by hand: staleness is derived, as a line check's is from the heads it was taken on.

**The checkers and their versions.** Each hook reports its own version in its answer (a hash of its script and step list, by its repository's rule); for a hook that reports none, the runner hashes the hook's command file and its settings. The runner's own classifier (§5.5) and blame rules have the runner's build and rule-set version.

| Verdict | Its checker | When the checker changes |
|---|---|---|
| a green line check | the line check | not current: the line is checked again before it can ride (I10) |
| a line check in flight | the line check | its answer is dropped, as one for a replaced head is |
| a charge by the line check (L33) | the line check | void, below |
| a pre-flight pass or failure | the pre-flight hook | a train still assembling runs pre-flight again; a train already in CI runs on, since CI is the full check on its exact head (I1) |
| a charge by a blame rule (L17) | the blame hook, or the runner's rules | void, below |
| a flake score | the classifier | its window starts again |
| a kept failure report (§5.3) | CI | classified by the present classifier when it is judged, which is when it is read anyway |

**A void charge.** The line is no longer charged. If its heads are the ones it was charged on, it goes back through the gate to waiting, is checked again as it stands, and its owner is told. If its heads have moved, its owner was part-way through a fix: the line stays with the owner, uncharged, to be offered when the owner says it is done (P3). I11 becomes "never rides again at the heads it was charged on, while the charge stands".

**What is not void.** A hold, a park, a priority, a quarantine decided at J5, a hand-over and a charge made by the judge at J1 are recorded judgements, not inferences of the software. Each already has its own review time or expiry. A change of the software doesn't touch them.

**The cost is bounded and visible.** A change of the line check sends every waiting line through the check again, in policy order, and the status says so. That is the price of not knowing which verdicts would differ.

## 6. Decided red

**A train is judged as soon as its failure is decisive.** CI reports each job as it ends. A failed job is classified at once (§5.5). A deterministic failure (class 4, which includes a failure that names a seed) is decisive: the train's `ci-failed` is reported then, the rest of the run is cancelled, blame reads that job's log, and the slot is free. A failure that a rerun could clear (classes 2 and 3) waits for the run to end, as today. Blame from one job may miss a second culprit in a job that hadn't finished; it is found on the next train, and that is cheaper than every failed train running to its end.

**Nothing is built on a train decided red.** A train whose failure is kept (it isn't first, §5.3) or which waits at J1 is decided red. Assembly builds no train behind one. A train sent to run again is clear, which is the optimistic reading. A train decided red that isn't first keeps its place and frees its CI slot; it is judged when the trains ahead land, from the kept report, as today.

## 7. Fairness

- **No jumping.** A line that could ride is passed only by a line with a manual priority, by the forced line, or by a line of an older stack. The first is a recorded act with an expiry, the second is bounded by the thresholds, and the third is the order itself.
- **No starvation.** Each cycle that passes a line for another's doing adds to its count. At the threshold the queue stops for it, readies it and rides it alone and first, where only its own failure or the infrastructure can stop it. Its own failure sends it to its owner; an infrastructure failure leaves it forced, and it rides again. A line that keeps losing rebase races is the first case.
- **A manual priority can starve nothing for long.** It expires, and every train it sends past a line adds to nothing that line can't recover: the line is as old as it was, and oldest goes first when the priority ends.

## 8. The model

[`tla/Shunter.tla`](../tla/Shunter.tla) carries the proposal. What it adds and checks:

| Property | Says |
|---|---|
| `NoJump` | a line that could ride is left waiting, while a line behind it in the order is taken, only for a manual priority or for the forced line and what it stands on |
| `ForcedFirst` | while a line is forced, the only train assembled is its own, alone, with the chain empty; only prioritised lines go before it |
| `CountFallsOnlyByACharge` | a line's count goes down only by a failure of its own |
| `VerdictsByPresentChecker` | a line is taken only on a green of the present checker; a charge is made by the present checker or the judge; a charge is dropped only when its checker has been replaced |
| `NoStackOnRed` | no train is assembled while a train in the chain is decided red |
| `EveryLineIsSpent` | (as before) holds with all of the above: the policy can't leave a line waiting for ever |

`GreenOnlyByACheck` and `ChargedOnlyAtPresentHead` are restated for versions and void charges. The model leaves out the clock (so the age backstop and a priority's expiry time), caps and shared files (a train is any front of the order), and the depth of a stack beyond its configurations. One checker version stands for every rule that judges.

## 9. Ruled

The owner ruled on 24 November: yes to the five that shape the design (1, 2, 4, 6 and 9), and the other seven take their recommendations. Each question keeps its recommendation and its reasons; the ruling follows it.

1. **What is a line's age?** *Recommended: its first offer, for good.* The alternative restarts age when a charged line is offered again. That punishes the owner for the queue's catch and brings back an edited position. With born fixed, a fixed line returns exactly as old as it is, ahead of nobody older. **Ruled: its first offer, for good.**
2. **Does a stack sort by its bottom's age (§3, step 3)?** *Recommended: yes.* It is what drains stacks from the bottom. Its cost is that a stack's upper lines pass unrelated lines offered between; ancestry keeps that honest and the count catches any line it delays. **Ruled: yes.**
3. **Halve or clear the count on the line's own failure?** *Recommended: halve.* **Ruled: halve, as recommended.**
4. **The thresholds.** *Recommended: `starve_after_cycles = 3`, `starve_after_hours = 12`, `starve_alert = 3`.* The case that prompted this had twenty-one cycles in eleven hours; three would have forced it within the first hour or two. **Ruled: three cycles and twelve hours of queue time; `starve_alert = 3`, as recommended.**
5. **Should the age backstop count hours, or landings that passed the line?** *Recommended: hours of queue time, as asked, with the clock stopped as in §4.* Landings passed is immune to outages by construction and could be modelled; hours are what an operator reads on the status. Revisit if an outage ever forces lines in numbers. **Ruled: hours of queue time, as recommended.**
6. **Does a forced line stop every other train, or only lines that touch its files?** *Recommended: every train, to begin with.* It is certain, simple and rare. Narrowing it to shared files is a later economy, from data. **Ruled: every other train.**
7. **May forced lines ride in a chain, each alone, one behind the other?** *Recommended: not yet.* The strict rule (the chain empty) is the one the model checks. Chaining them costs a discarded train when the first fails. **Ruled: not yet, as recommended.**
8. **Which outranks: a manual priority or the forced line?** *Recommended: the priority.* It is a person's recorded act with an expiry, and a park (§6.4) remains the way to land a fix for trunk ahead of everything. **Ruled: the priority, as recommended.**
9. **Does a change of the blame rules void charges made in trains, as well as the line check's?** *Recommended: yes, as the owner put it: every verdict inferred by an earlier version.* Its cost is that a line charged in a train and not yet fixed rides once more and, if the charge was right, fails once more. **Ruled: yes; a change of the blame rules voids charges made in trains too.**
10. **How fine is a checker's version?** *Recommended: one version per checker, reported by its hook.* One version for the whole scripts directory is simpler and voids everything on any change to any script; it is offered as a setting. **Ruled: one version per checker, as recommended.**
11. **Is a train at J1 decided red?** *Recommended: yes, until the judge orders a rerun.* Its failure is deterministic and unattributed; the common outcomes all end it. **Ruled: yes, as recommended.**
12. **Should a train decided red, but not first, be replaced at once?** *Recommended: no.* Its failure may be inherited, so it can't be judged, and replacing it needs a second chain (§10's "speculating on failure"). Keeping its place costs a chain position, not a CI slot. **Ruled: no, as recommended.**

## 10. Changes to the protocol

- **§5.1:** *waiting* loses "in the order they were offered … takes the place it had"; a spent line offered again keeps its born and its count. L15's guard gains "by the policy of §5.3". A new row: returned → waiting, "charge void", by the rule of I14. *held* and §6.4 lose "to the place it had".
- **§5.3:** the joining rule becomes §3 above; "nothing is built on a train decided red"; a new part for starving lines.
- **§5.4, §5.5:** a verdict's checker and version; a decisive failure is reported when it is decisive.
- **§5.10:** the count and queue time are fields of the line that a moved head doesn't reset.
- **Invariants:** I10 and I11 read "current" and "while the charge stands"; **I14, a verdict is its checker's**; **I15, no place**: no actor stores or edits an order of waiting lines, and a line that could ride is passed only as §7 says. **L4, no starvation.**
- **Judgement points:** J4 gains the thresholds and the alert; §7 gains "a starving line the runner can't ready is raised at once".
- **The daemon:** `Line.Seq` gives way to born, the count, queue time and a priority; `Join` becomes the policy function, returning reasons; verdicts gain their checker and version; the CI adapter reports a decisive job; the store's line files lose their order.

## 11. Mapping for a script queue

For the script queue this protocol grew from, in the order to build. It was written from that queue's feedback, not from a reading of its scripts, so each row needs checking against them.

| Today | Becomes |
|---|---|
| the queue file, read top to bottom | a bag: one record a line with born, count, queue time, and a priority if any. Its order in the file means nothing. Migration: born is the line's first enqueue in the queue's event log (or, failing that, when its change was first marked ready); the count is seeded from the last two days of the log, so that lines starving now are seen at once |
| the enqueue script, with ways to place a line | takes no position. It records born once. `--priority` needs `--reason` and `--until`, and is the only override |
| the bounced list, the put-front list | gone, with every script that writes them. A line coming back from a discarded or inherited train is just in the bag: its age puts it where it belongs and its count goes up |
| the rank that fronts an inherited failure's lines | gone |
| reordering by hand | gone; the status shows the policy's order and each line's reason |
| the assembly script | sorts by the policy (§3), records why each line was skipped, runs the forced-line procedure (§4), and builds nothing on a train decided red (§6) |
| the line-check feed | starts checks in policy order, the forced line first; stores each verdict with the checker's hash, and reads a verdict only when the hash is the present one |
| the sweep's "red is current while the heads are unchanged" | "…and the checker is unchanged"; a void charge puts the line back, or leaves it with its owner if the heads moved (§5) |
| the auto-rebase feed | rebases the forced line first, at once |
| the train watcher | on a decisive job failure, cancels the run and reports the failure; at promotion, reads the promoted train's run before a train is built on it |
| the cancel script | puts lines back in the bag with nothing to restore, since there is no place to restore |
