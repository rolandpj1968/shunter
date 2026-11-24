# Cut-over readiness: a review against the script queue

*A read-only review, 24 November 2026, of Shunter at `54f5b96` against the script queue this protocol grew from, as it ran that morning. [`cutover.md`](cutover.md) is the general method; this is the list of what stands between that queue and a cut-over. Names of scripts are that queue's own. Nothing here was run against the live queue.*

## What was read, and what was not

- **Read:** `PROTOCOL.md`, [`queue-as-bag.md`](queue-as-bag.md), `examples/shunter.toml`, the hooks' README, and the daemon's commands and adapters where a claim below depends on them. On the other side, a snapshot of the script queue: its runner, train script, gate, line-check feed, sweep, auto-rebase, feeders, cancel script, units, state files and two train logs. The comments of its two largest scripts were read in full, their code only where a row needed it.
- **The same day's later copies** of the train script, the auto-rebase feed, the line-check feed and a new caretaker feed, for four rules added that morning: a failed auto-rebase returned at once, a line in conflict twice running returned, blame by the directories below a failing package, and quoted paths near a failure. The line-set merge driver and the push-side timing log live in the application's repository and were read there.
- **The shadow's reports:** the latest run, the last two daily reports with their blame comparisons, and the first rows of the summary of 785 runs. Read, not re-derived: no divergence below was traced to its cause here. What they show is B1.
- **Not done:** nothing was run. Every "has" is a reading of code and documents. Where a point could not be settled by reading, the row says "unproved".

## (a) The duties

**Has**: Shunter does it, in the daemon or a shipped hook. **Differently**: the duty is met by another mechanism, and what changes for people is said. **Lacks**: nothing does it. "Site hook" means the mechanism exists and the repository's own rule must be moved into a hook before the day (`cutover.md`, "Before the day", step 3).

| # | Duty of the script queue | Shunter | Notes |
|---|---|---|---|
| 1 | **The gate** (`q.sh`): open, approved author, not `wip`, base landed or lined, not queued twice, no real conflict with trunk | Has, differently | §5.2 covers open, approved, the base, duplicates and staleness. A conflict isn't refused at the offer: the line check finds it and the rebase hook is asked. Three gate rules have no home: the migration-name check (a new migration must sort after trunk's latest), the translation-freeze park, and the line-spread warning. The first belongs in the line check; the second is a hold per line or a pre-flight leave-out; the third is advice |
| 2 | **Rebase at the gate** (`q.sh`'s replay: a change whose own patches replay unchanged is rebased and pushed) | Differently | The rebase hook, asked after a conflict verdict, not at the offer. A stacked change whose base landed is retargeted (I13) |
| 3 | **The line check** (`linecheck-feed.sh`, `linecheck.sh`, `linecheck-remote.sh`) | Has the gate, lacks the worker | I10, claims with leases, verdicts by checker version are in the daemon. Nothing in Shunter *runs* a check: a worker must `shunt claim`, run the steps, and `shunt checked`. The feed's slots, its dispatch to the CI hosts, its fallback and its trunk-alone evidence run are all site code to keep, re-pointed. Six verdicts become three: green, fail, conflict; `infra` is no verdict; `develop-red` is J2's; `waits-base` is the daemon's own ordering. **Blocker B2** |
| 4 | **The gate opens loudly when the checker dies** (a stale feed heartbeat) | Differently | A claim whose lease runs out is told to the judge. The gate does not open by itself: `require_check` is a setting. Decide which is wanted; the protocol says "opens, loudly" (§5.4) |
| 5 | **Assembly** (`coalesce` in the runner, then `batch.sh`) | Has, differently | See (b) rows 5 and 6. The policy order, the caps, alone, needs-first and a forced line are in the daemon. The script queue joins by a real merge in memory; Shunter skips by shared files and finds a real conflict at the build, where the line is left out and **returned**, not left waiting |
| 6 | **Pre-flight lanes** (the repository's step list, run on the CI hosts with a local fallback, a skip list, a usefulness rule, a recall, "never re-run an identical failing pre-flight") | Site hook | The pre-flight hook is one command with a timeout (20 minutes in the example; the lanes' own deadline is 45). The example hook has named, timed steps and a usefulness rule. The dispatch, the fallback, the kept-failure cache and the recall are site code inside the hook. **Blocker B3** |
| 7 | **Regenerate after combine** (generated files rebuilt from the merged sources: on a conflict, when the tree doesn't build, and once for the whole train; the counters file merged key by key; the pre-flight's own commit of improved counters) | Site hook, one point unproved | The conflict hook regenerates on a conflict. Regenerating with **no** file in conflict, and a pre-flight that **commits** to the train, were not found in the build path. If a hook can't add a commit before the head is fixed (T2), the counters step fails every train that improves a counter. **Blocker B3** |
| 8 | **Merge drivers** (regenerate; message catalogues key by key; the new line-set driver) | Differently | Drivers are the repository's `.gitattributes` plus git configuration, which Shunter passes through `[git] env`. The script queue sets them on each pick; Shunter's clone must carry all three. A missing driver silently falls back to git's text merge. **Blocker B4** |
| 9 | **Classifying a failure** (one whole rerun for a flake, a timing failure or infrastructure; the runner's own fault by signature; a recorded flake never blames; a seeded failure is never rerun) | Has | §5.5, and the blame hook's classes. The infrastructure signatures and the flake list are site configuration |
| 10 | **Blame and returns** (by the failing test's own file, then the package's code, then its other test files, then, since that morning, the directories below the package: data, wording, golden inputs; paths a failure quotes, now also a quoted path within 30 lines after an assertion; tables a failure names; names new in the train; the counters walk; the known-gap and translation-floor rules; a stack goes with its blamed base) | Site hook | The generic blame hook covers files, directories with or without their subdirectories, descriptions, seeds and tree-wide tests. The rest are this repository's rules: move them, don't rewrite them. In the shadow's last day the hook and the script agreed on 4 of 7 failed trains; on the other 3 the hook named a line where the script had split blind. Each of those is a charge someone must read before it is trusted. **Blocker B5** |
| 11 | **A split when blame names nobody** | Differently | J1: park if the same test failed before, charge a train of one by elimination, else split. A deadline of 10 minutes replaces the script's immediate split |
| 12 | **Telling the owner** (a comment on the change, the `wip` label, back to draft, a line in the owners' file on the producers' host) | Differently | One notice per return, to the events file and the notify command. Every delivery the owners rely on today is the notify hook's to do. **Blocker B6** |
| 13 | **The re-line sweep** (`relinesweep.sh`: every 10 minutes, a returned change that is clean and green is queued again; a stack by its top's check; three returns for one reason end it) | Lacks, mostly by design | A charged line comes back only by its owner's offer (P3, I11): the sweep's re-queue of a blamed change after "a push" is exactly what P3 forbids. But a line returned **uncharged** (left out for a conflict with its train, its base lost) is also spent, and nothing offers it again. The script queue now leans on the sweep more, not less: row 14's two new returns both end "re-lined as soon as it merges cleanly". **Blocker B7** |
| 14 | **Auto-rebase** (`autorebase-feed.sh`: every open change in conflict, once per head. Since that morning: a rebase the feed can't finish takes the change **out of its line at once**, unblamed (`send_back`); and the line-check feed takes a line found **in conflict twice running** out of its line, with whatever is stacked on it (`conflict_back`). Both tell the owner and leave the sweep to re-line the change once it merges) | Has, opposite in one respect | The rebase hook is asked for a *waiting* line whose check found a conflict, once per heads and trunk (I13); a change that is with its owner is not rebased. On a conflict the hook can't resolve, the owner is told, uncharged, and the line **stays in the queue**, out of trains, until a push. A conflict that recurs raises the line's count; at three it is forced, alone and first, and a forced line that can't be rebased is raised as stuck. So the script queue now sends a conflicting line away and re-admits it; Shunter keeps it and escalates. Both came from the same incident (a line at the head of the queue through eighteen failed rebases). They can't both be the rule: decide before the day. Shunter's is the ruled one (bag §4); if the owner's newer word stands instead, that is a change to the protocol, not a hook |
| 15 | **Alone, and next-alone** (`next-alone.list`, `feed-alone.sh`: a second list, fed one line at a time when the main list is empty) | Has, better | `--alone` is a flag on the line; it waits in the bag at its age. The second list, and its habit of starving alone lines behind a busy queue, go |
| 16 | **Holds** (`queue.hold`) | Has | `shunt hold` with a reason, a review alert, never spent. The script's hold file names changes only; the import needs a reason for each (see (d)) |
| 17 | **Train cancel** (`train-cancel.sh <change> park\|requeue\|drop`) | Has, differently | `shunt cancel`. The default differs: the script parks, Shunter requeues. Shunter acts on the named line and its stack; the train's other lines go back to waiting |
| 18 | **Stack rules** (lines minimal; a stacked line rides with or behind its base; a stack never split) | Has | P1, P2, I3; stacks drain from the bottom. The old `s` lines that land a stack on its own check have no counterpart, and should have none (I1) |
| 19 | **Outside-change approval** (an approved list, a standing rule for one bot's branch) | Has | `approved_file`, `approved_label`, `[repo.author_branches]` |
| 20 | **Owners and no-owner alarms** (`owners.list`, `unlined.sh`, `owner-sync.sh`) | Has, differently | An owner is whoever offers; liveness is a heartbeat file per producer; an ownerless line is raised within the hour. The script's list covers every open change, *queued or not*. Shunter knows only lines it was offered. A change never offered is invisible to it. Keep `unlined.sh` as a detector |
| 21 | **The caretaker and its list** (`orphans.sh`, and since that morning `caretaker-feed.sh`: the list of changes the queue can't move and nobody owns, sent to the producers' host every 10 minutes; the caretaker's heartbeat copied back; and the CI bot's refresh change queued with no agent at all) | Lacks | A producer role, not a queue duty. `shunt handover` is the recorded act it needs, and the heartbeat directory is where its liveness goes. Keep the feed: its list comes from Shunter's ownerless alerts plus row 20's detector, and its third duty becomes one `shunt queue` |
| 22 | **Metrics** (one row a train in the statistics file; step timings; the dashboard push every five minutes; the weekly pre-flight report; failures by layer) | Lacks | Shunter has the store's log and the events file, and writes no statistics row. Dashboards go dark at the cut unless an exporter writes the old row shape from the log. Can follow, if a day without dashboards is acceptable |
| 23 | **Mirrors to the producers' host** (runner and assembly logs, pre-flight logs, the ten-minute heartbeat line) | Lacks | The notify command can mirror notices. The heartbeat line the producers watch for "the queue is down" must be kept: point it at `shunter watch`. **Blocker B6** |
| 24 | **The draft marker** (a train is a draft pull request from assembly, its body updated as steps finish, promoted when pre-flight passes; the train's log says `BATCH-DRAFT <pr> <head>`) | Has, less; and the replay adapter doesn't know the marker | `[queue] drafts`. The body isn't updated per step; a failed pre-flight comments its evidence and closes the draft. Separately, the adapter that reads the script queue's train logs reports the marker as "not understood" (36 times in the last daily report), so the shadow can't see a train that died in pre-flight as a draft that was opened and closed. A small fix, and it comes before B1 can be closed |
| 25 | **The push-side timing log** | Not the queue's | It is written by the producers' pre-push hook in the application's repository. The rebase hook pushes through the same hook, so its pushes are logged too. Nothing to move |
| 26 | **After a landing** (changes labelled and closed; each "fixes" issue closed unless a temporary comment on trunk still cites it; the flaky verdict read afterwards) | Has the first | Changes are labelled and closed. Issues are closed only if the forge does it from the commit messages, with no guard for a comment that still cites the issue; that guard exists because closing such an issue turned trunk red. The flaky verdict is not read. Do both from the notify hook on a landing. Can follow, with the guard first |
| 27 | **Restart and recovery** (the in-flight journal, `recover`, one runner under a lock, the stop file with backoff, a line set aside after three infrastructure stops) | Has, better | One store, keyed effects, a single-writer lock, J3 for exhausted budgets, the outside watcher |
| 28 | **A lone line waits 15 minutes for company** | Lacks | A cost setting (J4), not safety. With owned runners it costs little. Can follow |
| 29 | **The fallback merge** (a green train landed by the forge's merge API when the push is refused) | Lacks, on purpose | §5.7: no fallback merge. It lands commits CI never tested |
| 30 | **The trunk lock** | Has | Both sides already take the same lock. It is a `flock`, so it holds only between processes on **one host** |

## (b) Where §11's mapping is wrong

§11 of the bag note says of itself that it was written from the queue's feedback, not its scripts. Against the scripts:

1. **"Born is the line's first enqueue in the queue's event log, or when its change was first marked ready."** The move journal exists only since 19 November, so older lines have no first enqueue in it. And changes in that queue are **never** marked ready: they stay drafts for life. Use the journal's first `offer`, else the owner's first report of the line, else the change's creation.
2. **"The count is seeded from the last two days of the log."** No one log holds a line's cycles. Conflict verdicts are in the line-check record, skips in the coalesce-skips file, left-outs in the returned file, discarded trains in the runner's log. Seed from the line-check record's conflict verdicts alone, or seed nothing and let the twelve-hour backstop work from born.
3. **"The enqueue script, with ways to place a line."** `q.sh` only appends. Every placement is the runner's: one `put_front` function, called for a split's halves, the rest after a blame, a pushed line, a cancelled run, a held line, a discarded second train and recovery. It is the commonest way a line re-enters, not an override.
4. **"The bounced list, the put-front list ... gone, with every script that writes them."** No script writes them; the runner does, inline. And four more files edit or flag position that §11 doesn't name: `queue.nojoin` (rides alone after a recovery or a no-verdict), `queue.firstseen` (the lone-line wait), `queue.runcancelled` (strikes that expire) and `next-alone.list`. All are dropped at the cut (they are the old queue's memory); `next-alone.list` is carried as the alone flag.
5. **"The rank that fronts an inherited failure's lines."** No rank was found. A second train whose first failed is discarded and its line put at the front by the same `put_front`.
6. **"The assembly script sorts by the policy."** Assembly is two scripts. The runner's `coalesce` chooses: the head of the list, then every later line that **merges** onto the train so far (a real merge in memory, since 13 November; shared files only as the fallback), up to 8 changes and 3 lines, a docs-only line regardless, a head with a stack joined only by its stack. `batch.sh` then builds what it is given. Shunter's skip is by shared files and its conflict is found at the build, where the line is returned. The same input gives different trains and one more return per conflict; expect it in the shadow's comparison.
7. **"The line-check feed ... stores each verdict with the checker's hash."** Right in direction. Missing: the feed's key includes the heads of the open changes *beneath* the line; the gate is a file switch that also opens when the feed's heartbeat is stale; and the check runs on other hosts with a three-hour deadline, against a default claim of 30 minutes. Set the lease to the deadline.
8. **"The sweep's 'red is current while the heads are unchanged'."** That test is the runner's gate, not the sweep's. The sweep is the queue's re-offerer: it reads the returned file, re-queues, climbs stacks, and stops at three returns. §11 maps one sentence of it and not the duty; see row 13. The "twice running" return is not the sweep's either: it is in the line-check feed.
9. **"The auto-rebase feed rebases the forced line first."** The feed isn't ordered by the queue at all: it walks every open change in conflict, queued or not, once per head, with a skip list for changes being rebased by hand. Under I13 the changes that aren't waiting lines stop being rebased.
10. **"The train watcher: on a decisive job failure, cancels the run."** There is no watcher. The wait is inside `batch.sh`, under an explicit rule of 2 November: no fail fast, every run to completion. A second train is never judged: it ends "speculation failed" and is discarded, and a green one is promoted into the first slot. Fail-fast is new behaviour on the day, not a mapping.
11. **"The cancel script puts lines back in the bag."** Its default is to **park** the line in a file of its own, for a person; requeue and drop are options.
12. **Rows §11 lacks:** the hold file; the fallback merge; the closes after a landing; recovery and the in-flight journal; the drain file; draft trains; the lone-line wait; the feeder that turns the producers' inbox into offers and answers each one; and the depth, which is fixed at two.

## (c) What blocks, and what can follow

**Blocks cut-over.** Each is an act with a test, not a design question.

| | Blocker | Done when |
|---|---|---|
| B1 | **The shadow doesn't agree yet.** In the last daily report, of 21 landings Shunter's lander found the passing verdict on the head for 18: three trains the script queue landed, Shunter would not have (I9). Either the script queue landed without a verdict Shunter can read, or Shunter misreads a head that passed; the first is a defect being retired, the second would stall one green train in seven. The machine also refused 12 events of the history (lines riding together that may not, a charge with no evidence, two transitions the table lacks), and the gates diverged in 6 runs of the day. The latest run agrees 4 of 4 and 2 of 2 | Each of the three landings traced to its cause; each kind of refused event decided as `cutover.md` step 2 says; the draft marker understood (row 24); then three consecutive daily reports with every landing's verdict found and every gate divergence explained |
| B2 | **No line-check worker.** | The feed claims from Shunter and answers with `checked --version`; a red, a conflict, an infra and a lease expiry each seen in a dry run |
| B3 | **The pre-flight hook isn't the queue's pre-flight.** | One hook runs the same step list on the CI hosts with the local fallback, inside the hook's timeout; and it is proved, on a scratch repository, that regeneration with no conflict and the counters commit reach the train's head before CI. If a hook can't commit, that is a change to Shunter first |
| B4 | **The clone's merge drivers.** | The three drivers are set in `[git] env`; a test train with a clash in each kind of file builds as the script queue builds it |
| B5 | **The blame hook isn't the queue's blame.** | The last two weeks' failed trains, as fixtures, name the same lines through the hook as the script did, or a better answer that someone has read |
| B6 | **Returns reach nobody yet.** | The notify hook comments on the change, sets what marks it as being fixed, writes the owners' file and the heartbeat line on the producers' host; one return of each reason delivered in a dry run |
| B7 | **An uncharged return is never offered again.** | Either the sweep is kept as a detector that offers a line returned uncharged once it is clean (and never a charged one), or the owners' rule says they do it, and the ownerless alert covers the rest |
| B8 | **The intake.** | The feeder calls `shunt queue` (with `--alone`), answers each offer as it does now, and the old gate script can no longer write a list |
| B9 | **Rollback isn't rehearsed.** | The export-and-map script exists, round-trips, and the whole rollback has been timed under ten minutes on a copy (see (d)) |

**Can follow, in this order:** the statistics exporter and dashboards (22); issue closing with its guard, and the flaky verdict (26); the draft's body per step (24); the lone-line wait (28); status reasons for "why is my line not riding"; the migration-name check moved into the line check (1); a memo for the blame hook on an early failure.

**Not gaps:** the fallback merge (29), the `s` lines (18), every strike and stop file (27), the second list (15).

**Four behaviours change for owners on the day,** and they should hear it before, not find it: a line in conflict is kept and escalated, not returned (14); a charged line comes back only by its owner's word (13); a failure blame can't place waits ten minutes at J1 instead of splitting at once, which the shadow met six times in a day (11); and a cancel requeues by default (17). A failed train may also be cut short (b, 10).

## (d) A cut-over plan

### The drain point

The script queue has a drain of its own: while its drain file exists the runner takes no line, the riding trains finish, and the runner ends. But its alone-feeder restarts a runner that has exited. So, in order:

1. Stop what feeds: the producers' line feeder, the sweep, the auto-rebase feed, the alone-feeder. Snapshot the lists.
2. Create the drain file. The line-check feed keeps running; its verdicts cost nothing and will be void anyway.
3. Wait for the drain point, which is all of: the in-flight journal empty; every line taken since step 1 has a `done` row in the move journal; trunk is at the last landed train's head; no train pull request or branch is open; no auto-rebase push is in flight; the trunk lock is free.
4. Stop the line-check feed and the runner's unit. Snapshot again and reconcile as `cutover.md` says: every line of the first snapshot has landed, is in the lists now, or is in the returned file with a reason.

At the snapshot this review read, two trains rode and two lines waited. A drain from there is one CI run, two with a rerun. Choose an hour like it.

### What runs where

| Piece | Where | Why |
|---|---|---|
| The daemon, its store and clone | the host that runs the script queue's runner | The trunk lock is a `flock`: it excludes the old runner only on the same host. Rollback needs both there |
| The line-check worker | the same host, dispatching to the CI hosts | As now: it only dispatches and records |
| The pre-flight hook | runs on the daemon's host, dispatches to the CI hosts, falls back locally | As now. The hook's timeout must cover queueing on the CI hosts |
| CI | the CI hosts | Unchanged. One required check, read on the train's exact head |
| The rebase hook | the daemon's host, pushing through the producers' own pre-push hook | I13; the push is tested as an owner's would be |
| The intake and the notices | a file on the producers' host each way, carried by the feeder and the notify hook | The producers' protocol doesn't change on the day |
| `shunter watch` | a timer on the daemon's host; its exit status feeds the heartbeat line the producers' host already watches | L3: the watcher is watched from the other machine |

### The state migration

| From | To | How |
|---|---|---|
| the waiting list, in file order | waiting lines | `waiting.list`. Born is read as (b) 1 says; where it can't be, file order stands in for age, once |
| the alone list | waiting lines marked `alone` | the same file |
| the hold file | holds | `held.list`. The file gives no reasons: write one for each, or release it before the cut |
| the parked files (cancelled, the translation freeze) | parks, each with a condition the runner can test | `parked.list`. A park nobody can state a condition for is restored before the cut |
| the owners' list | owners | The line files name changes only, so **an import makes the importer every line's owner.** After the import, one `shunt handover` per line from the owners' list (latest entry wins), and a heartbeat file per live producer. Lines whose entry names nobody get no hand-over and raise the ownerless alert within the hour, which is correct |
| line-check verdicts | **nothing** | Declare the checker's version afresh (`shunt checker line-check <v>`). By I14 no old verdict counts and every imported line is checked again, in policy order. With a short queue that is minutes |
| changes returned and not yet fixed | nothing; they are with their owners | Their next offer comes through the new intake. The rule "never at the heads it was charged on" is lost for these few; the line check stands in for it |
| bounced, no-join, first-seen, strikes, the stop file | dropped | The old queue's memory |
| the flake list, the pre-flight's step history and skip list | carried, inside the hooks | |

Then compare Shunter's status with the second snapshot, line by line, before `resume`.

### Rollback in under ten minutes

`cutover.md` budgets a quarter of an hour. Under ten needs one script, rehearsed, and no waiting for trains:

| Minute | Step |
|---|---|
| 0 | `shunt pause`. Cancel each live train with requeue; a train that is landing is waited for (the push's deadline is five minutes, and it is the one thing not to interrupt) |
| 1 | Stop the daemon, the worker and the feeder's Shunter side |
| 2 | `shunt export --offline --store DIR --line-files OUT` |
| 3 | The map script: `waiting.list` to the old waiting and alone lists, `held.list` to the hold file, parks to the parked files; all under the old list's own lock |
| 4 | Compare: every line of the export is in exactly one old file. Close Shunter's open train pull requests |
| 5 | Remove the drain file; start the old units, the alone-feeder last since it starts the runner |
| 6 to 9 | Watch the old runner take a line and the line-check feed give a verdict. Tell producers which offers to repeat: none, if the feeder's inbox was never changed |

**Kept ready for it:** the old units stopped and not disabled, their scripts untouched, the drain file in place, the map script tested, the old line check's verdict file left as it was. Its verdicts are stale after a day of landings, so the old gate re-checks every line: a rollback is slow to its first train by one line check, never unsafe. The triggers are `cutover.md`'s.

**What a rollback loses:** ages. The old lists have an order and no born; the map script writes lines oldest first, and that is all the old queue can use.

### A first day of "Shunter decides, the script queue shadows"

Only partly feasible, and only the cheap part is worth doing.

- **The old runner can't shadow.** It has no mode that assembles without pushing branches, opening pull requests and spending CI. Leave it stopped.
- **The old line check can.** Without its gate file it only records. Run it on its local workers, not the CI hosts' slots, against a list written from `shunt export --line-files` every few minutes. Each night, compare its verdicts with Shunter's worker's for the same heads. Two checkers that share their steps and disagree is a finding about the worker (B2).
- **The old detectors can.** The no-owner report, the heartbeat and the dashboard push read lists. Pointed at the same export, they watch the new queue with yesterday's alarms, which covers rows 20, 22 and 23 while their replacements are young.
- **The old blame can, offline.** For each train Shunter fails that day, run the old blame over the same logs and compare the names. This is B5's test, continued on live failures.
- **And Shunter's own log** is replayed through the machine that night, as `cutover.md` says; it should refuse nothing.

The day's exit test: every landing was a verified head; no line's state was unaccounted for; the two line checks agreed or each difference was explained; and the old alarms raised nothing Shunter hadn't raised first.
