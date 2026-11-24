# Shunter: a merge queue with a judge in the loop

*Draft 0.2, 18 November 2026. The protocol behind a merge queue that lands work from many concurrent agents onto one branch: its mechanism, the invariants no actor may break, and the points where judgement decides.*

## 1. The problem

Many producers, here AI agents, open pull requests faster than CI can test them one at a time. One branch, called **trunk** here, must only ever contain tested states. CI is slow (20–30 minutes), costly, and fails for many reasons that aren't the change's fault: flaky tests, dying runners, a forge returning server errors, tests that leak state into each other, and trunk itself broken.

Pure mechanism handles the common case and fails the rest badly. A rule like "split the batch when blame names nobody" turns one broken shared fixture into a cascade of half-batches. What was needed was someone to look and say "this isn't any PR's fault: stop". This protocol says where that judgement sits, what it may do, and what happens when it's silent.

## 2. Principles

1. **Trunk only ever advances to a tested state,** by fast-forward to the exact commit CI tested. Nothing is rewritten between test and landing.
2. **Every external call is fallible.** A failure is transient by default. It's permanent only when the refusal follows from what the request means: not a fast-forward, permission denied, a rule violation, a bad reference. Transient failures are retried with backoff, then escalated; they're never charged to a change.
3. **Blame needs evidence.** A change is charged with a failure only when evidence names it: a failing test in files it touches, an error at a line it added, a rule its description breaks. Otherwise the failure belongs to the infrastructure, to trunk, or to judgement.
4. **Rules guard safety; judgement handles the rest.** The invariants (§4) bind every actor, the judge included. Within them, decisions with no clear mechanical answer go to the judge, at defined points (§6), from a fixed menu of actions.
5. **Silence is safe.** Every judgement point has a deadline and a conservative default. A missing judge costs time, never correctness.
6. **Judgements become rules.** Each judgement is recorded with its evidence. When the same judgement recurs, it's promoted into a mechanical rule, and the rulebook grows only from observed judgement.
7. **Nothing waits forever unnoticed.** Every change either lands or returns to its owner with a stated reason.

## 3. Actors

- **Producers** (agents) write changes, run cheap local checks, and offer each change for landing as a **line**. A line is one change; it holds several only when they must land together (P1 below). A **stack**, changes each built on the one before, is offered as a sequence of lines, base first.
- **The runner** is the only actor that writes trunk. It builds batches, runs CI and lands. A batch that has taken its lines is called a **train**, from then until it ends.
- **The judge** decides at the judgement points. Today that's the coordinator, an AI session; any actor that meets §6's contract will do.
- **The forge and CI** (for example, GitHub and its runners) are external and fallible.

**Producer obligations.** Two owner rules bind producers. A line does two jobs, ordering changes and landing them all or none, and most stacks need only the first; joining changes that need only an order into one line made older, independent changes wait on failures in newer ones. [`docs/dependency-graph.md`](docs/dependency-graph.md) is the design behind §5.2's needs and staleness, which the owner adopted.

- **P1, lines are minimal.** A line is one change. A stack is offered as a sequence of lines, base first. A stacked line waits in the queue and rides only behind its base (I3, §5.3). Changes share one line only when neither can pass CI without the other, and the offer says why.
- **P2, stack only when necessary.** A producer branches each change from trunk, unless it builds on an unlanded change's code: it calls, edits or tests something that change adds. A stacked change's offer says why ("needs A's *X*"). Neighbouring files, a shared session or a shared author are no reason.

- **P3, a returned line has an owner, and only the owner says it is done.** Whoever offers a line first owns it. When the line is charged it is with its owner, **being fixed**, until the owner offers it again; that offer is the owner's word that the fix is complete, and it is refused at the heads the line was charged on (I11). Between the charge and that offer the queue takes nothing from the line, however many pushes it sees: in the queue this protocol grew from, a stand-in owner's half-made pushes were re-queued as fixes and failed four trains in a row. A line changes owner only by a recorded **hand-over**, asked for by its owner or decided by the judge when the owner is silent past its deadline (§7). A line returned uncharged (left out, its base lost) had nothing found wrong with it, and anyone may offer it again as it stands.

The reasons are recorded with the line and shown to the judge. The gate doesn't judge them: a wrong reason costs time, never a wrong landing (I1).

## 4. Invariants

These are safety properties. Every reachable state must satisfy them, whatever the producers, the forge or the judge do.

- **I1, tested trunk.** Every commit trunk points to was the head of a CI run that passed with exactly that tree.
- **I2, fast-forward only.** Trunk's history only grows. No landing rewrites or drops a commit already on trunk.
- **I3, needs first.** No change lands before a change it needs: its base, earlier in the same line or in another line (§5.2). A line rides only behind its base: the base has landed, or rides earlier in the same train or in an earlier train. A line lands entirely or not at all: a line of several changes is a declared group whose changes co-require one another (P1).
- **I4, frozen while riding.** A change's head doesn't move while it rides a batch. If it moves, the batch is void and doesn't land. The test is made at landing, not only when CI passes: a batch that waits behind another in a speculation chain is tested again when its turn comes.
- **I5, speculation is exact.** In a speculation chain (§5.3), batch *k* is built on the exact head of batch *k−1*, and lands only if batches 1…*k−1* all landed at exactly the heads it was built on. Batches land in chain order.
- **I6, blame needs evidence.** A change goes back to its owner as charged only when a blame rule or a recorded judgement names it.
- **I7, one writer.** At most one actor fast-forwards trunk.
- **I8, one state, one record.** Every line is in exactly one state of §5.1, held in one store, and so is every train. A transition is a recorded act: the subject, from, to, the actor, the time and the reason. No actor infers a line's state from anything else, and a transition that isn't in the tables of §5.1 can't be written.
- **I10, checked before assembled.** Where the queue requires line checks (§5.4), no line is assembled into a batch without a green line check on exactly the heads its changes have. A push to any of them ends the check. A push is never taken as a fix: a line that a check charged rides again only when the check has passed on its new heads.
- **I11, a charge is cleared by a fix.** A line that was charged never rides again at the heads it was charged on, and is offered again only by its owner (§3, P3). A push alone is not "done": anyone's push, a rebase or half a fix all move a head. Nor is an offer a fix: the heads must have moved.
- **I12, never lighter than its content allows.** A batch is given the light tier of §5.4 only when every line in it is inert content, by a classifier that can only err towards the full tier.
- **I13, a head moves only by its owner, or by a necessary rebase.** The runner moves a change's head only to rebase a waiting line that its check found in conflict with trunk. A line that merges is never touched: a rebase moves the head, and a moved head ends the line's check (I10), so a needless one leaves a line that needed nothing with no verdict.
- **I15, no place.** No actor stores or edits an order of waiting lines. The order is read from facts (a manual priority, a line's age, its stack), and a line that could ride is passed only by a line with a manual priority or by one of an older stack.
- **I9, the lander verifies.** Before it fast-forwards trunk, the runner itself reads from the forge that the batch's CI passed on exactly the head it is about to push, that the head is the one it built, that every change's head is the one it assembled (I4), and that trunk is where the batch was built (I5). It never lands on a builder's report, and no protocol message travels in a channel that tools or tests can write to.

Two consequences are worth stating on their own, because the queue this protocol grew from broke both:

- **A cancelled batch never lands.** Once aborted (§5.9), a batch has no transition out.
- **A held line is never spent.** A hold (§5.1) has one way out, its release, and that leads back into the queue.

Liveness:

- **L1.** Every offered line eventually lands, or returns to its owner with a reason, or is withdrawn by its owner, or is held or parked by a recorded judgement visible to its owner, or waits, visibly, behind a base that is held, parked or stale. A line in any of the first three conditions is **spent**. No line is spent in any other way, and none silently.
- **L2.** If the forge and CI eventually behave, and trunk is healthy, a line with no fault of its own eventually lands.
- **L3, watchers are watched.** Every periodic check the protocol relies on (trunk's health, the age of waiting lines, the judge's own liveness, each producer's) records each run and its result. A check that hasn't run within twice its period, or has failed to run three times, is itself an alert. An error inside a check is never read as "nothing to report".

## 5. Mechanism

### 5.1 State machine

A line and a train are each in exactly one state (I8). The tables below are the whole machine: a transition that isn't a row can't be written. Each row names its trigger, its guard, who decides it (a rule or a judgement point), and the safe default: what happens if the one who decides is silent, or the guard fails.

**States of a line**

| State | Meaning |
|---|---|
| **offered** | A producer has put it forward. The gate (§5.2) hasn't answered. |
| **waiting** | Queued. It may join a train once its base, if it has one, has landed, rides in an earlier train, or joins the same train ahead of it (I3), and while it isn't stale (§5.2). Waiting lines have no place: assembly takes them by the policy of §5.3, and a line's age is its first offer, which nothing changes (I15). |
| **waiting-for-base** | Retired. Only a line queued before needs first (I3) is in it: it waits for its base to land, and then it is waiting (L25). No line enters it now. |
| **held** | A recorded judgement keeps this one line out of trains. It stays visible to its owner, with the reason. A hold on a riding line voids its train, and the rest of the train goes again. Lifting the hold sends the line through the gate again, as old as it was. A held line is never spent. |
| **parked** | Moved aside under a park (§6.4) that has a restore condition. Restored by the runner when the condition holds. |
| **riding** | In a live train. Its stage is the train's state, below: a line "rides as train 1 or 2" by its train's place in the chain, and is "green" or "landing" when its train is. Its heads are frozen (I4). |
| **landed** | Spent: on trunk. |
| **returned** | Spent: back with its owner, who has been told why. The reason is one of: refused at the gate; left out at assembly; charged with a failure, with the evidence; returned by an abort; its base didn't land, uncharged. |
| **dropped** | Spent: withdrawn by its owner. |

A spent line that didn't land may be offered again. That's a new offer, with fresh retry budgets (§5.10); the line keeps its age.

**States of a train**

| State | Meaning |
|---|---|
| **assembling** | It has taken its lines. The merge and the pre-flight (§5.4) are in progress. Lines may still be left out, each with a reason. |
| **built** | Pre-flight passed. Its head is fixed, and the next train in the chain may be built on it (I5). |
| **testing** | Its pull request is open and CI runs on its head. |
| **judging** | It failed, and blame can't attribute the failure. It waits at J1, its lines still riding. |
| **green** | CI has reported a pass on its head. It waits its turn. A green train has not been verified, and a report is not a verdict (I9). |
| **landing** | It is first in the chain, the lander has verified it (I9), and the push is in flight. This is the one state nothing can interrupt: a hold, an abort or a withdrawal is refused until the push is answered. It's bounded by the push's deadline. |
| **landed** | Ended: trunk is at its head. |
| **failed** | Ended: the failure was charged by a blame rule, or settled at J1. |
| **void** | Ended with nothing judged: no verdict, a head moved, a member held or withdrawn, the train ahead didn't land, or the landing check or the push found the world changed. Nobody is charged. |
| **aborted** | Ended by the judge (§5.9). It never lands. |
| **empty** | Ended: every line was left out at assembly, each with its reason. |

Live trains form the **chain**, in landing order: train 1 is built on trunk, and train *k* on train *k−1*'s exact head (I5). When a train ends in any state but landed, every train behind it is void, and its lines pass the gate again.

**Transitions of a line**

| # | From | To | Trigger | Guard | Decided by | Safe default |
|---|---|---|---|---|---|---|
| L1 | none | offered | offer | not live; its changes are in no live line; its base is known | a producer | refused |
| L2 | returned | offered | offer | not live; its changes are in no live line; its base is known; budgets reset (§5.10) | a producer | refused |
| L3 | dropped | offered | offer | not live; its changes are in no live line; its base is known; budgets reset (§5.10) | a producer | refused |
| L4 | offered | waiting | gate-accept | no park covers it | the gate (§5.2) | stays offered |
| L5 | offered | parked | gate-accept | the queue is parked and this is not the park's fix | the gate (§5.2) | stays offered |
| L6 | offered | returned | gate-refuse | a reason is given | the gate (§5.2) | stays offered |
| L7 | waiting | held | hold | a reason is given | the judge, or a human | refused; the line stays where it is |
| L8 | waiting-for-base | held | hold | a reason is given (retired: a line queued before needs first) | the judge, or a human | refused; the line stays where it is |
| L9 | parked | held | hold | a reason is given | the judge, or a human | refused; the line stays where it is |
| L10 | riding | held | hold | a reason is given; its train is not landing; the train goes void | the judge, or a human | refused while landing |
| L11 | held | waiting | release | it is held | the judge, or a human | stays held, visible to its owner |
| L12 | held | parked | release | it is held; a park covers it | the judge, or a human | stays held |
| L13 | waiting | parked | park | the park has a restore condition | J1, J2 or the judge | not parked |
| L14 | parked | waiting | restore | the park exists; no other park covers the line | the runner, when the condition holds; or the judge's restore | a condition the runner can test: stays parked, and the judge is alerted at the deadline; any other: restored at the deadline, with an alert |
| L15 | waiting | riding | assemble | not held, not parked, not stale; a current green line check, where the queue requires one (I10); alone if it must be; one split group per train; its base has landed, rides in an earlier train, or is earlier in this train (I3) | the runner's joining rule (§5.3) | stays waiting |
| L16 | riding | returned | leave-out | the train is assembling; a reason is given | the assembly rules (conflict, caps, its base left out), or a pre-flight hook's leave-out (§5.4) | refused without a reason |
| L17 | riding | returned | preflight-failed, ci-failed, judge (charge) | evidence names the line (I6) | a blame rule (§5.6), or J1 | never charged without evidence |
| L18 | riding | returned | abort (return) | the train is live and not landing | the judge (§5.9) | requeue |
| L19 | riding | waiting | any end of its train that is not a landing | the gate passes it | the gate (§5.2) | the gate decides again |
| L20 | riding | parked | any end of its train that is not a landing | a park covers it | the gate (§5.2) | the gate decides again |
| L21 | riding | waiting | abort (requeue) | the train is live and not landing | the judge (§5.9) | requeue |
| L22 | riding | parked | abort (park) | the train is live and not landing; a restore condition is given | the judge (§5.9) | requeue |
| L23 | riding | landed | push-ok | its train is landing | the runner | none: the push decided |
| L24 | waiting | returned | its base is returned or dropped | always | rule: needs first (I3) | returned with the reason, uncharged |
| L25 | waiting-for-base | waiting | push-ok (of its base) | no park covers it (retired: a line queued before needs first) | the gate (§5.2) | the gate decides again |
| L26 | waiting-for-base | parked | push-ok (of its base) | a park covers it (retired: a line queued before needs first) | the gate (§5.2) | the gate decides again |
| L27 | waiting-for-base | returned | its base is returned or dropped | always (retired: a line queued before needs first) | rule: needs first (I3) | returned with the reason, uncharged |
| L28 | offered | dropped | withdraw | none | its owner | refused; the line stays where it is |
| L29 | waiting | dropped | withdraw | none | its owner | refused; the line stays where it is |
| L30 | waiting-for-base | dropped | withdraw | none (retired: a line queued before needs first) | its owner | refused; the line stays where it is |
| L31 | parked | dropped | withdraw | none | its owner | refused; the line stays where it is |
| L32 | riding | dropped | withdraw | its train is not landing; the train goes void | its owner | refused while landing |
| L33 | waiting | returned | line-checked (fail) | the verdict is on the line's present heads; the failing check passes on trunk alone (I6) | the line check (§5.4) | never charged without evidence |

**Transitions of a train**

| # | From | To | Trigger | Guard | Decided by | Safe default |
|---|---|---|---|---|---|---|
| T1 | none | assembling | assemble | not paused; the chain is shorter than D; the train ahead has a head | the runner (§5.3) | no train |
| T2 | assembling | built | built | it has a line; its head is new | pre-flight (§5.4) | void at the builder's deadline |
| T3 | assembling | empty | leave-out (the last line) | every line was left out, each with a reason | the assembly rules | none needed: a rule decides |
| T4 | assembling | failed | preflight-failed | blame names some lines, not all (or the only one) | a blame rule (§5.6) | none needed: a rule decides |
| T5 | assembling | judging | preflight-failed | blame names none, or all of several | J1 | J1's default (T10) |
| T6 | built | testing | opened | the pull request is on the built head | the runner | void at the deadline |
| T7 | testing | green | ci-passed | the pass is on the built head | CI | no verdict at the deadline |
| T8 | testing | failed | ci-failed | blame names some lines, not all (or the only one) | a blame rule (§5.6) | none needed: a rule decides |
| T9 | testing | judging | ci-failed | blame names none, or all of several | J1 | J1's default (T10) |
| T10 | judging | failed | judge (charge, split, park), judge-timeout | a charge has evidence; a split has two lines | J1 | park if the same test failed before or the train is one line; else split |
| T11 | judging | testing | judge (rerun) | it has a head; one rerun | J1 | no rerun |
| T12 | green | landing | landing-verified | first in the chain; the lander read the pass on this head, every change's head as assembled, and trunk at its base (I9) | the runner | void on any mismatch |
| T13 | landing | landed | push-ok | the forge fast-forwarded trunk | the forge | retried while the refusal is transient |
| T14 | landing | void | push-refused (permanent) | trunk is not where the train was built | rule: no fallback merge (§5.7) | rebuilt on trunk; J3 is told |
| T15 | assembling | void | ci-no-verdict; hold or withdraw of a member; head-moved; the train ahead did not land | nothing was judged | rule: no verdict charges nobody (§5.5) | its lines pass the gate again |
| T16 | assembling | aborted | abort | the train is live and not landing; a reason is given | the judge (§5.9) | runs on |
| T17 | built | void | ci-no-verdict; hold or withdraw of a member; head-moved; the train ahead did not land | nothing was judged | rule: no verdict charges nobody (§5.5) | its lines pass the gate again |
| T18 | built | aborted | abort | the train is live and not landing; a reason is given | the judge (§5.9) | runs on |
| T19 | testing | void | ci-no-verdict; hold or withdraw of a member; head-moved; the train ahead did not land | nothing was judged | rule: no verdict charges nobody (§5.5) | its lines pass the gate again |
| T20 | testing | aborted | abort | the train is live and not landing; a reason is given | the judge (§5.9) | runs on |
| T21 | judging | void | hold or withdraw of a member; head-moved; the train ahead did not land | nothing was judged | rule: no verdict charges nobody (§5.5) | its lines pass the gate again |
| T22 | judging | aborted | abort | the train is live and not landing; a reason is given | the judge (§5.9) | runs on |
| T23 | green | void | landing-verified (mismatch); hold or withdraw of a member; head-moved; the train ahead did not land | nothing was judged | rule: no verdict charges nobody (§5.5) | its lines pass the gate again |
| T24 | green | aborted | abort | the train is live and not landing; a reason is given | the judge (§5.9) | runs on |

Reading the tables:

- **Every way back into the queue goes through the gate.** Rows L11, L12, L14, L19–L22, L25 and L26 are one rule applied to different arrivals: the line goes where the gate would put it now (§5.2).
- **Nothing leaves "held" except by release** (L11, L12). A held line can't join, can't be parked afresh, can't be charged, and can't be withdrawn until it's released. So a hold can never spend a line.
- **Nothing leaves "aborted", "void", "failed", "empty" or "landed".** A late report for an ended train is refused and logged.
- **Only T13 moves trunk,** and only from "landing", which only T12 enters.
- **A line returns only with a reason** (L6, L16–L18, L24, L27, L33), and as charged only with evidence (L17, L33, I6). A line whose base didn't land goes back uncharged (L16, L24).

```
                    ┌───────── held ◀── hold ── (waiting, waiting-for-base, parked, riding)
                    │ release
                    ▼
 offered ──gate──▶ waiting ◀──restore── parked ◀── park / the gate under a park
    │               │  ▲  ▲
    │ refused       │  │  └─ (its base is returned or dropped) ──▶ returned
    ▼          join │  │ requeue (void, the rest of a failed train, abort)
 returned           ▼  │
                  riding ──▶ landed     (its train landed)
                    ├──────▶ returned   (left out, charged with evidence, abort)
                    └──────▶ dropped    (withdrawn; any state but held and landing)

 assembling ──▶ built ──▶ testing ──▶ green ──verified (I9)──▶ landing ──▶ landed
     │             │         │  ▲       │                         │
     │             │         ▼  │ rerun │                         └─ push refused ─▶ void
     ├─▶ empty     │      judging (J1) ─┼─▶ failed
     └─────────────┴─────────┴──────────┴─▶ void | aborted
```

### 5.2 The gate

A line is accepted when:
- its changes are open and from approved producers;
- each change's base has landed, or is earlier in the same line, or is in another live line (the line's **base**: the line it needs);
- it isn't already waiting or riding.

A line whose base is live **waits in the queue** and rides only behind its base (I3, §5.3). Nobody polls. If its base is returned or withdrawn instead, the line returns too, uncharged, with that reason (L24); if it is held or parked then, it returns when it is released or restored.

**A need is read twice, and the readings must agree.** The forge declares each change's base (a pull request's base branch), and ancestry shows it: a change stacked on another must contain that change's head, and a change on trunk must contain no live change's head. Where the two disagree (the base was pushed and the change not rebased onto it, or a change carries another's commits while it targets trunk) the line is **stale**: it waits in the queue, joins no train, and its owner is told why. The gate reads it at the offer, and the runner reads it again while the line waits, so that a rebase clears it and a base pushed later makes it stale. A stale line is a flag on a waiting line, not a state, as "must ride alone" is. Staleness costs time, never a wrong landing (I1).

**The runner rebases what needs no person.** A waiting line that no longer merges onto trunk is not sent back to its owner for that alone: a stale base isn't worth a person's turn, and an owner who has gone quiet strands it. When the line check (§5.4) finds a conflict, nobody is charged; the line joins no train, and the runner asks its rebase hook to rebase it. The hook rebases only what its repository's rule says needs no person (generated files, a counters file, both sides only adding lines), pushes the new heads through whatever tests the producers' own pushes, and answers them; the line is then checked again like any pushed line. A conflict the hook can't resolve, or a queue with no hook, goes to the owner with the files named, uncharged, and the line stays out of trains until a push. A change stacked on a change that has landed is moved onto trunk by the same act. The runner never guesses a resolution: a hook that fails, or answers nothing, leaves the conflict to the owner.

**And it rebases nothing that needs no rebase (I13).** The rebase is asked for only when a line check has just found the line in conflict, once for those heads against that trunk; the same conflict found again asks nothing, and one found against a trunk that has since moved asks again, since what could not be resolved then may be now. A line whose base has landed and which merges as it stands is moved onto trunk on the forge and not rebased. The queue this protocol grew from first rebased every line that was merely behind trunk: each rebase moved the head, each moved head ended the check in flight, and one line went a day with no verdict at all.

**A gate condition holds at every later transition, not only at the offer.** Whatever would refuse, hold or park a line at the gate (a park, a hold, an unlanded base) is tested again whenever the line would enter the queue or join a batch, including a batch rebuilt after a failure or a void. A line that no longer passes goes to the state the gate would give it. It doesn't ride and fail.

### 5.3 Batches and speculation

- **Joining:** the runner joins waiting lines into a batch by policy ([the queue as a bag](docs/queue-as-bag.md) §3): lines with a manual priority, and the lines they are stacked on, first; then every line by the age of the bottom of its stack, then up the stack, so that stacks drain from the bottom. A line's age is its first offer. **A manual priority is the only override:** it names a reason and an expiry, and ends at its expiry, when it is taken back, or when the line is spent. It **skips** a line it can't take (it shares files with the batch, must run alone, or would exceed the caps: a number of lines, and a number of changes, a stack's every change counted) rather than stopping at it. A line that alone reaches the change cap rides alone, so that no line waits for ever. **A stacked line joins only behind its base** (I3): its base has landed, rides in an earlier train of the chain, or has joined this train ahead of it. So a line whose base was skipped, for any reason above or because it is held, parked or stale, is skipped too, and a stale line is skipped. Skipping is safe because a line is taken only after what it needs.
- **A starving line rides alone and first** ([the queue as a bag](docs/queue-as-bag.md) §4). A line's count rises by one for each cycle that riding alone and first would have avoided: its line check finds trunk moved against it, another line ended its batch (charged, pushed to, held or withdrawn, or the batch split or parked at J1), a batch ahead discarded its own, or it was left out at assembly. A failure of its own (a red line check, a charge) halves the count; nothing else lowers it, and a new offer keeps it. A line is **starving** when its count reaches `starve_after_cycles` (3), or, as the backstop, when its queue time (waiting while the queue isn't paused) reaches `starve_after_hours` (12). One line is **forced** at a time, the oldest starving line that waits with nothing beneath it held, parked or spent. While one is, no batch is assembled but one of prioritised lines, or its own: the forced line and what beneath it still waits, alone, when the chain is empty. Its line check goes first. A forced line the runner can't rebase is **stuck**: it stops being forced, keeps its count, and is raised to the judge at once; a push ends it. `starve_alert` (3) starving lines at once are raised to the judge (J4).
- **Speculation, to depth D:** while batch 1 runs, the runner builds batch 2 on batch 1's exact head, batch 3 on batch 2's, and so on up to depth D. All D batches run CI at the same time (I5).
- **Landing is in chain order:** when batch 1 lands, the chain shifts up, batch 2 becomes batch 1, and the runner extends the chain at its tail.
- **Only the first batch is judged.** Batch 1 is built on trunk, so its failure is its own. Batch *k* > 1 is built on a head that may never land, and its failure may be that head's, inherited. When a batch that isn't first reports a failure (in pre-flight or in CI), nothing is charged, split or parked: the report is kept. If every batch ahead lands, the failure was its own and is judged then, as if reported at that moment. If one doesn't, the batch is void with it and the report is dropped, with nobody charged. Draft 0.2 judged the report at once, and so could charge a line for the failure of the batch ahead of it.
- **Nothing is built on a batch decided red:** one whose failure is kept, or one that waits at J1. The runner assembles no batch behind it, and the CI run of a batch decided red is cancelled, so its slot is free. A batch sent to run again at J1 is clear. A batch decided red that isn't first keeps its place; it is judged when the batches ahead land, from the kept report.
- **When batch *j* ends without landing** (it fails, is void, or is aborted): batches *j+1…D* are void, because the head they were built on will never exist. They're rebuilt on trunk, minus whatever blame removed from batch *j*.
- **Choosing D:** the trade-off is throughput against waste. With a batch pass probability *p*, the best case lands D batches per CI cycle, and a failure at position *j* voids D−*j* runs. Deep chains pay when *p* is high (around 0.9), and depth 1 is about the limit when *p* is near 0.5. On metered CI, voided runs cost money; on owned runners they cost only capacity, so D is bounded by free runner slots (D × one batch's jobs) more than by spend. D is set at judgement point J4.
- **Change-based CI:** a batch's CI may run only the jobs its change can affect. "Its change" is measured against the batch beneath it, not trunk, or speculation would count batch 1's change twice.

### 5.4 Pre-flight

There are three tiers of checking, and a fourth the runner may put between the first two. Each answers a different question:

| Tier | Runs on | Question |
|---|---|---|
| **1. Producer pre-checks** | the change **merged with trunk as it is now**, before it's offered as a line | Does this change work at all? |
| **1b. Line check** (optional; the runner's) | each waiting line alone, merged with trunk, while it waits | Does this line, by itself, break something cheap to detect? |
| **2. Batch pre-flight** | the assembled batch, on the runner, before CI is dispatched | Did *combining* these changes break something cheap to detect? |
| **3. CI** | the batch, in full | Is this exact state safe to land? |

**A batch is visible from assembly, and promoted after its cheap tier.** Where the forge can show a draft, the batch's pull request opens as a draft as soon as its lines are merged, while pre-flight runs; a draft starts no CI. When pre-flight passes, the runner **promotes** it: marks it ready, which starts the one CI run. A failed pre-flight comments its evidence on the draft and closes it. So a batch has a visible "assembled, cheap tier running" state before "expensive tier running", and promotion, the step between them, is the runner's act. Without drafts (`[queue] drafts`), the pull request opens at promotion.

**A stack top's tier-1 check counts for the changes below it.** Tier 1 keeps the pass rate up; it isn't what makes trunk safe, since every train is tested in full (tier 3, I1). A change that can't pass without the change above it should be in one line with it (P1).

**Tier 1 tests the change merged with trunk, not the change on its own base.** A change that passes on a stale base and fails once merged is the commonest cheap failure there is: an import trunk dropped, a registry entry trunk now demands, a quality ratchet trunk has moved. Draft 0.2 said "on its own base", and the queue this protocol grew from paid for it: in one five-day sample, two thirds of all batch failures were deterministic checks of one change, each of which failed on that change merged with trunk and on nothing else.

**The line check (tier 1b) is tier 1 run by the runner, so that it need not be taken on trust.** A producer's pre-check is the producer's own copy of the rules, it can be skipped, and the queue holds no record of it. Where a queue turns line checks on:

- **What runs:** every deterministic check of tier 2, on the line alone merged with trunk, with no skip list, and without stopping at the first failure, so the owner gets the whole list in one round.
- **When:** while the line waits, in parallel with batches and with other lines' checks, on capacity the batches aren't using. It holds no batch slot.
- **The verdict belongs to the heads it was taken on.** It is stored against the trunk and the heads checked. A push to any change of the line ends it (I10). Trunk moving does not end a green verdict at once: re-checking every waiting line at every landing costs more than it finds, and what trunk's movement breaks in a line is exactly what tier 2 catches on the true tree. A queue bounds a green verdict's age instead, and refreshes the oldest when idle.
- **A charge is on the heads the line has now, and on that line only.** A check takes time, and its verdict can arrive after a push. A failure on heads the line no longer has is nobody's: it charges nothing and the line is checked again. And the line is the unit of a charge: lines that only share a batch, its rest after a blame, or a half of a split are not one line, and no change is charged for another's failure. (The queue this protocol grew from once checked a batch's rest as one line, judged it on one change's old head, and marked all four of its changes as being fixed.)
- **A failure charges the line, with evidence** (I6): the failing check is run on trunk alone, and passes there. A check that fails on trunk alone is trunk's fault, charges nobody, and is J2's business.
- **A check is claimed, in the store.** A checker asks the runner for a line and is given one with its key, the heads and the trunk, under a claim with a lease; the claim is recorded like any other act (I8). While it stands no other checker is given the line, a checker that restarts included, and a verdict on another key is refused. The verdict ends the claim, and so do a push and the line's leaving the queue. A claim whose lease runs out is given up and the judge told (L3): its checker died or hung. A late verdict on the same key is still a verdict on those heads. (The queue this protocol grew from kept its claims in a feeder's memory: a feeder that restarted saw no claim and handed a line still being checked to a second worker.)
- **No verdict is no verdict:** a timeout, a full disk or an unreadable forge is tried again, and never charges (§5.5, class 0).
- **The gate:** no line is assembled without a current green check (I10; L15's guard; a failure is L33). A line stacked on another is checked with its base beneath it, after its base is green.
- **If the checker dies, the gate opens, loudly.** Tier 2 still runs every check on every batch, so a dead checker costs speed and never safety (L3 watches it).

With line checks on, tier 2 has less to do: only what joining lines can break. Moving a check out of tier 2 on that ground is a J6 decision, made from the record of what each tier caught.

Tier 2 runs on every batch, before any CI is spent. A failure there sends the changes it names back (by the blame rules of §5.6), with no rerun.

**A pre-flight may leave a line out instead.** Some failures say "this line didn't fit this train", not "this line broke it": it doesn't build on the train, or a generated part clashes with another line's (two changes taking the same number). A pre-flight may answer a leave-out for such a line, with a reason, as assembly leaves out a conflict (L16). The line goes back to its owner uncharged, told that it didn't fit; the train is built again without it, and pre-flight runs again on the rest. A leave-out is not a charge (I6), so it needs no evidence beyond its reason, and it uses no retry budget.

**Test scope is an axis of its own.** Every test has one scope, and the scope decides when it runs:
- **Package tests** test one unit. They're selected by the dependency graph: the changed units plus everything that depends on them.
- **Feature tests** exercise one feature or screen, including UI (browser) tests of a single screen. They're tagged by feature area and selected by what changed.
- **Scenario tests** run the product end to end: user journeys, cross-feature flows, demo worlds. They're never selected away. They always run in the batch's CI and before a release.

Test selection pays off only if this hygiene holds. A large integration suite dressed up as a package test defeats it: any change near it selects everything. In one codebase measured over 104 changes, three such packages held 40% of the test time, and half of all changes selected 80% or more of the suite because of them.

**What belongs in pre-flight is an economic test.** A check earns its place if

> P(it catches a failure) × (the CI run it saves + the latency of a failed batch) > its own cost on every batch.

So a pre-flight check is:
- **cheap:** seconds to a few minutes, never comparable to CI;
- **deterministic:** a flaky pre-flight step sends correct changes back with no rerun, which is worse than not having the step;
- **integration-sensitive, or a frequent CI catch:** above all, things a single change's pre-check can't see, such as conflicts, generated files after the merge, counts that ratchet across changes, and rules that fire across modules.

Not in pre-flight: long test suites, anything that needs services or browsers, anything flaky. Those belong to CI.

**A line's tier is chosen by what it changes, and inert content takes a light one.** A change whose every file is inert, read by no build, no test and no generator (prose, say), gains nothing from the full tier: it gets a light check that satisfies the same required status. Two rules bind it:

- **The classifier can only err towards the full tier.** A line is inert only when the forge's *whole* list of the files its changes touch is known, a rename's old name included, and every one of them matches a pattern declared inert and none a pattern declared read (a document a test checks, a page a generator embeds). No rules, a list the forge can't vouch for, a path the classifier can't make sense of, one file under no inert pattern: full. Adding a file to a change never lightens it, and a rule left out costs time, never a check that should have run. A push ends a line's inertness until the gate has read its files again.
- **A batch's tier is its heaviest line's (I12).** A batch is light only when every line in it is inert; one full line makes the whole batch full. So an inert line may **join any batch whatever its size**: it can break nothing the batch's checks look at, counts against neither cap of J4, and rides a batch that is already full.

Where the light check is chosen is the forge's business as much as the runner's: a CI that classifies the batch's own difference by the same rules, erring the same way, satisfies I12 by construction. The runner's classifier decides what joins; I1 still holds, since the light check is a CI run on exactly the batch's head.

**A selection that leaves checks out must over-approximate.** This binds tier 1 as well as tier 3: a change the selection can't classify, or a change to the definition of CI itself, selects everything.

**The selection is data-driven, in both directions:**
- **Promotion:** every failure CI catches is recorded with the test, its location and its run time. A failure CI catches often, which a check could catch in seconds, is promoted into pre-flight.
- **Demotion:** every pre-flight step's catch history is recorded. A step that has caught nothing in a long window (on the order of 150 batches) is dropped.

The review that promotes and demotes is judgement point J6.

### 5.5 Classifying a failure

Each failure gets one class, decided in this order:

0. **No verdict:** the run was cancelled, skipped, superseded or timed out, the builder died, or the result can't be read. Nothing is judged. The batch is void and its lines go back unchanged. Repeated no-verdicts on the same line go to J3 (§5.10). They're never promoted to "deterministic".
1. **Infrastructure:** no check ran (setup, tooling, the runner lost), or the forge refused for a transient reason (§2.2). The batch reruns, or the line goes back unchanged.
2. **Known flake:** a test recorded as flaky failed. One rerun.
3. **Timing:** only timeouts or lost connections. One rerun.
4. **Deterministic:** an assertion, or a compile or lint error. No rerun; go to blame.

**A decisive failure is reported when it is decisive** ([the queue as a bag](docs/queue-as-bag.md) §6). CI reports each job as it ends. A job that fails while the rest of the run goes on is classified at once: if it is class 4, the batch's failure is reported then, blame reads that job's log, and the rest of the run is cancelled. A failure that a rerun could clear (classes 2 and 3) waits for the run to end. Blame from one job may miss a second culprit in a job that hadn't finished; it is found on the next batch, which is cheaper than every failed batch running to its end.

**A recorded flake never charges a line.** A test recorded as flaky is known to fail on code where it should pass, so its failure names no line, however often it fails and however small the batch. When a known flake's one rerun fails again, nothing is judged: the batch is void, its lines go back unchanged (class 0), blame isn't asked, and J1 isn't opened. The lines' no-verdict budget (§5.10) brings a test that keeps failing to the judge (J3, then J5: fix it or quarantine it). Draft 0.3 first made the second failure class 4 and blamed it by files; the queue this protocol grew from then charged a batch of one line for a recorded flake it had not caused.

**A seeded failure is never a flake.** A randomised test (a fuzz or property test) that fails prints the seed that reproduces it. Such a failure is class 4 whatever else it shows, a timeout, a lost connection or a test recorded as flaky: a rerun draws another seed, so its pass says nothing about the seed that failed. The seed is kept in the evidence, so that the failure can be reproduced and filed. If blame names no line, J1 decides as for any deterministic failure; a seed whose bug was already on trunk is J1's or J2's to judge, and the bug is filed either way. The queue this protocol grew from let a fuzz test's rerun pass on a new seed and landed the train as "passed on its rerun", over a real defect the first seed had found.

**A timing failure's rerun that fails again is evidence.** When a timing failure's one rerun fails the same way, the failure is class 4 from then on: it goes to blame (§5.6), which is asked again with the class forced, and names lines by their files as for any deterministic failure. A line that makes a test slow, or makes a flaky test it touches fail every time, goes back to its owner without a judge. Blame that names none, or all of several, goes to J1 as before.

### 5.6 Blame

Blame maps the evidence in a deterministic failure to changes. The most specific rule goes first:
- a rule a change's **description** breaks (for example, "closes an issue whose gap it leaves listed");
- an error at a line a change **added**;
- a failing test in files a change **touches**.

The outcomes:
- **Blame names some lines, but not all:** those return to their owners, and the rest go again whole. A batch of one line that blame names, with evidence, is this case too.
- **Blame names none, or all of several:** that's not a decision blame can make → **judgement point J1** (§6.1).

Blame works in whole lines: a line is charged or cleared together (I3). A line stacked on a returned line goes back with it, uncharged, with that reason: from the train when the train ends (L19 then L24), and left out with it at assembly (L16). Blame that names only the stacked line returns it alone, and its base rides on.

Returning a line **includes telling its owner**, in the same recorded act, with the evidence. A line isn't "returned" until the telling is recorded as delivered or as owed.

**A test that scans the whole tree is touched by every change.** Some tests hold every member of a repository-wide set to a rule: every route registered, every table listed, every file a test reads declared. A change breaks such a test without touching its file or its package, so "no line of the batch touches what failed" says nothing about it. The blame hook marks such a failure (`tree_wide`), by its repository's own rule, a naming convention or a list. A failure so marked is never taken as evidence that trunk is broken (J1), and is never the ground for calling a failure unrelated to its batch.

### 5.7 Landing

- **The lander verifies (I9).** When a green batch is first in the chain, the runner reads from the forge, itself: the head CI passed, each change's head, and trunk. All three must be what the batch was built from. If any differs, the batch is void: nothing is judged, and its lines go back.
- The runner then fast-forwards trunk to the batch's tested head (I1, I2). Transient forge refusals are retried (§2.2), within a budget (§5.10).
- **If the fast-forward is refused permanently, trunk is not where the batch was built.** The batch is void (I1): it's rebuilt on trunk and tested again. There is no fallback merge: a merge onto a trunk that moved lands a tree CI never tested. A second writer on trunk is a breach of I7 and goes to J3.
- A push to a change while its batch is landing arrives too late to void it. The batch lands as tested, the later push is not on trunk, and the owner is told so in the same act.

### 5.8 Flaky tests

Flakes set *p*, the chance that a batch passes on its merits, and *p* sets batch size, speculation depth and waste. With 1,000 tests that each flake 0.01% of the time, about one run in ten fails from flakes alone, so unmanaged flakes cap *p* near 0.9.

- **Flake score.** Each test's failure rate on code where it should pass, kept over a rolling window. The evidence is a rerun that passes, a failure on trunk-equivalent code that passes unchanged, and the nightly order-shuffled and run-twice runs.
- **Cost.** A flake's cost is its score × the batches it fails × the changes in those batches. Fix the most costly first, not the most numerous.
- **Quarantine, bounded.** A test whose score crosses the threshold still runs and is still reported, but doesn't gate a batch. Each quarantined test has an owner, an expiry date and an issue. The quarantine count is a ratchet: it only goes down. When a test's expiry passes, judgement point J5 runs again.
- **Retry the test, not the batch.** Only a quarantined or known-flaky test may be retried in place. Any other failing test gets no retry: a fresh failure is evidence.
- **Prevention.** A new or changed test runs twice, and in shuffled order, before it lands, and a leak audit fails a test that changes shared state (each test counts the shared fixtures before and after it runs). Flakes are cheapest to stop at the change that introduces them.
- **Feeding D.** The judge's choice of speculation depth (J4) reads *p* net of quarantined tests.

### 5.9 Abort

The judge may abort a riding batch at any stage before its push: assembling, pre-flight, CI, at J1, or passed and waiting to land. An abort names what becomes of each line: back to waiting (the default), parked with a restore condition, or returned with the reason. The runner honours it at every stage, lands nothing of an aborted batch, stops its CI, closes its pull request, and voids the batches built on it (I5), which are rebuilt.

**An abort asked for one line acts on that line, not on the train's other lines.** A cancel names a change; it acts on the line that holds it, and on the lines stacked on that line (I3): they go back with it, uncharged, when it is returned, and wait behind it while it is parked. Every other line of the train goes back to waiting, as old as it was. The queue this protocol grew from dropped a whole train of eleven when one of its changes was cancelled, and the other ten were queued again by hand.

An abort is not a failure. It charges no change (I6) and uses no retry budget. Aborting twice is the same as once. The one stage an abort can't reach is the push itself (§5.1, "landing"): the request is refused, and the caller hears that the batch is past the point of no return.

### 5.10 Retry budgets

Every counter that changes how a line or a batch is treated is a field of its record (I8), never a side file: a line's no-verdict strikes, a batch's reruns, a landing's refused pushes. A line's budgets reset when it lands, when it's offered again, and when its head moves. They expire after a stated time. A budget that runs out goes to J3. It never turns into a charge.

## 6. Judgement points

Each point defines what the judge **sees**, what it **may choose**, the **deadline**, and the **default** if the judge is silent. Every choice is recorded with its evidence.

### J1. A deterministic failure that blame can't attribute

- **Sees:** the failure, the batch's changes, the last N batches' results, trunk's recent health (nightly runs, other batches failing on the same test), and the changes' descriptions.
- **May choose:**
  - charge changes X (stating the evidence);
  - split the batch (whole lines, in train order, so that each line rides only behind its base);
  - **park** (§6.4): the failure isn't the batch's, trunk or a shared fixture is broken;
  - rerun once (stating why it might differ).
- **May not choose:** to return a line with no evidence (I6). A batch of one line can't be split, so for it the menu is charge with evidence, park, or rerun.
- **Deadline:** 10 minutes. **Default:**
  - the same test failed in the previous batch with unrelated changes: **park** (trunk or a shared fixture is suspect, J2). **Not when the test scans the whole tree** (§5.6): two unrelated batches failing on it are two changes that each broke the set it holds, and the rules below apply;
  - else the batch is one line: **charge it, by elimination.** It is judged at the head of the chain (§5.3), so its base is trunk, which is tested (I1), and the failure was not in the batch before: the line is the only change there is, and that is its evidence (I6). Draft 0.2 parked the whole queue here. That stopped every line for one line's failure, and when the park's deadline restored the queue the line rode and failed again;
  - else **split.** The halves of a split ride apart until they land.
- **Promotion:** a recurring J1 judgement becomes a blame rule (§5.6). An example is "a PR that closes a listed gap": judged once, a rule thereafter.

### J2. Trunk, or a shared fixture, may be broken

- **Triggered by:** two consecutive batches with unrelated changes failing on the same test, or the nightly run failing.
- **Sees:** the failures, and recent landings touching the failing area.
- **May choose:**
  - park and assign a fix, which lands first and alone;
  - revert a named landing;
  - declare it a flake (recording it).
- **Deadline:** 15 minutes. **Default:** park.

### J3. Escalated retries

- **Triggered by:** a transient failure that persists past its retry budget (§5.10); a line whose batches keep ending with no verdict; a fast-forward refused permanently, or trunk found moved by another writer (I7).
- **May choose:**
  - keep waiting, with a new budget;
  - pause the queue;
  - treat the failure as permanent (stating why);
  - where trunk was moved by another writer: adopt the new trunk, recorded as a foreign landing, once no batch rides.
- **Default:** pause, and alert a human. A pause stops new batches; batches in flight finish.

### J4. Cost against speed

Batch size caps, speculation depth D (§5.3), the starvation thresholds (§5.3), which jobs a batch runs. Many lines starving at once is raised here: the thresholds may be too low for a queue that is only behind. A human sets the bounds; within them, the judge sets D from the recent pass rate and the runner slots free.
- **Sees:** pass rates, queue length, CI spend.
- **May choose:** settings within bounds a human has set.
- **Default:** keep the current settings.

### J5. A newly flaky test: a test bug or a product bug?

- **Triggered by:** a test whose flake score (§5.8) crosses the quarantine threshold for the first time.
- **Sees:** its failure modes, with messages and timings; the code it exercises; recent changes there; the nightly order-shuffled and run-twice results.
- **May choose:**
  - **test bug:** quarantine it and assign a fix;
  - **product bug:** file it as a product defect, and keep the test gating (a race or a missing lock must not be hidden);
  - **infrastructure:** add the failure's signature to the infrastructure list (§5.5).
- **Deadline:** one day. **Default:** quarantine with an expiry, which is safe for throughput, and the expiry brings the question back.

### J6. The pre-flight review

- **Triggered:** periodically (for example weekly), or when a step's catch rate or cost changes sharply.
- **Sees:** the record of what CI caught, with each failure's location and run time; each pre-flight step's catch history and cost; the share of batches failing in CI on things a cheap check could have caught.
- **May choose:**
  - promote a check into pre-flight;
  - demote a step that no longer catches anything;
  - move a check between tier 1 and tier 2 (an integration-insensitive check belongs with the producer).
- **Default:** no change.
- **Constraint:** a step may be promoted only if it's deterministic (§5.4).

### 6.4 Parking

Parking moves the waiting lines aside: arrivals park too, and parked lines count as queued. The batch in flight finishes. The fix lands alone; then the lines come back. Parking is a judgement act, recorded, and always followed by a restore.

Every park carries a **restore condition the runner can test:** a named change has landed, a measure is above a threshold, a time. The runner restores when it holds, and says so. **A condition the runner can test is never met by a clock:** such a park ends when its condition holds, or by the judge's restore, and not otherwise. At its deadline the judge is alerted and the park stays. If the condition can no longer come true (the line it waits for was returned or dropped), the judge is told that the wait can't end. A wait that acts on its own timeout releases lines whose fix hasn't landed. Only a park whose condition can't be tested, words for a person, carries a deadline after which the default restores and alerts. Restored lines pass the gate again (§5.2), as old as they were.

A park may also cover named lines only, as an abort's disposal of them (§5.9). **To keep one line out of batches, use a hold (§5.1), not a park:** a hold names the line, shows its owner the reason, and can't spend it.

## 7. The failure model

The protocol assumes all of the following, and must stay safe under any combination of them:
- the forge refuses, errors, times out, or answers late; a refusal may be transient or permanent;
- CI hangs, loses runners, serves unreadable logs, or reports a result for the wrong attempt;
- tests are flaky, order-dependent, or leak state into each other;
- trunk breaks without any batch's fault;
- producers push to a riding change, retire mid-line, offer duplicate lines, or hang silently (a producer stuck in a tool call holds its work invisibly: each producer needs a liveness signal, and work held by a silent producer is handed on after a deadline);
- the judge is slow, silent or wrong (safety must still hold; only liveness may suffer);
- a hold is forgotten, or an owner goes silent with a charged line. Neither is acted on by a clock: a hold ends only by its release, and a line leaves its owner only by the owner's offer or a recorded hand-over. Both are *raised*: a hold that has stood past its review time, and a charged line that has stayed with its owner, are told to the judge, again each time the period passes (L3). **An owner entry names a live owner.** A producer that has finished, or shows no sign of life (its heartbeat is missing or old), is no owner: a line that waits on it (charged, or stale or in conflict) is raised within the hour as having no owner, not after the period a live owner gets. (In the queue this protocol grew from, two lines stranded for hours because the owner list still named an agent that had finished, so the alarm saw an owner and stayed quiet.);
- the runner restarts at any point. All its state is in one store (I8), and it resumes from what it finds. **To replace it between trains, pause it:** a pause starts no new batch, the batches in flight finish and land, and lines offered meanwhile wait; then stop it, start the new one, and resume. Nothing is held by hand and nothing is killed mid-batch. (The queue this protocol grew from was reloaded by holding every line and killing the runner, and new lines outran the holds twice.)
- a builder's report is wrong, or its output carries text that looks like a report (I9);
- the runner's own code is replaced while it runs. A new version is checked before it's started (it parses, it can execute, it passes its own tests), the running version is recorded in the store, and a version that fails to start leaves the old one in place and alerts;
- a watcher stops, or fails without saying so (L3).

## 8. Comparison with other systems

| | Batching | Speculation | Attribution | Judgement |
|---|---|---|---|---|
| **Bors** (Rust) | yes | no | bisection | none |
| **Zuul** (OpenStack) | dependent pipeline | yes, deep | each change tested on those ahead | none; humans re-trigger |
| **GitHub merge queue** | yes | yes | removes failing group members | none |
| **Uber SubmitQueue** | conflict analysis | probabilistic | build graph | none |
| **This protocol** | yes, with skip-ahead | one level | evidence first, then judgement | explicit points, deadlines, safe defaults, promotion into rules |

The novelty isn't the mechanism, which is a subset of Zuul's. It's treating judgement as a first-class part of the protocol, bounded by invariants and backed by defaults.

## 9. Verification

- **A TLA+ model** of §5 and §6, with the forge, CI and the judge as adversaries. TLC checks the invariants under every interleaving and every failure in §7, and checks L1 under fairness for the forge and CI. [`tla/Shunter.tla`](tla/Shunter.tla) is its skeleton: the state machine of §5.1 without the gate's detail, with fast-forward only, a verdict on exactly the landed head, exact speculation, "a cancelled batch never lands", "a held line is never spent" and L1.
- **The judge is modelled as nondeterministic choice within its menu.** So the model proves safety whatever the judge decides, which is the point of bounding judgement.
- **The state machine is one pure function,** from a state and an event to a new state and the effects to perform. The tables of §5.1 are printed from its transition table, a test fails if any row has no test, and the function refuses to write a transition that isn't a row (I8).
- **Histories are replayed.** A replayer runs recorded events through the function and reports what it refuses. It checks an old queue's logs before a cut-over ([`docs/cutover.md`](docs/cutover.md)), and Shunter's own transition log afterwards.

## 10. Open questions

- **Speculating on failure as well as success** (as SubmitQueue does): alongside "batch 2 on batch 1's head", also build "batch 2 on trunk", for when batch 1 fails. This is exponential in general, but bounded to one alternative it removes the rebuild delay after a failure.
- **The depth policy:** a fixed D, D chosen by the judge (J4), or D computed from the pass rate. Note that adaptive *batch size* was considered and rejected, as pre-splitting without cause; depth is different, because it spends spare capacity and never shrinks a batch.
- Whether J1's default should be "park" rather than "split" whenever any doubt exists.
- How promoted rules are retired when they stop firing.
- Whether a hold or an abort that arrives during a push should be remembered and applied if the push is refused, rather than refused and asked again.
- Whether the halves of a split may take on fresh lines. Today they ride apart from everything, which keeps attribution clean and costs throughput.
- Whether a deterministic pre-flight failure that blame can't attribute deserves one retry on a new base before J1. An old queue did this by habit; here it needs a judgement.

## Change log

**0.3, unreleased.**

- **§5.3, §5.5: fail fast, and nothing built on red.** A job's class 4 failure is reported while the rest of the run goes on (`Report.Early`), and the run of a batch decided red is cancelled. No batch is assembled behind a batch whose failure is kept or which waits at J1 (the model's `NoStackOnRed`).

- **§5.3: a starving line rides alone and first.** A line's count of cycles it didn't cause, halved by its own failure, and its queue time; the forced line; a stuck one raised at once. New event `starving`, and `[queue] starve_after_cycles`, `starve_after_hours`, `starve_alert`.

- **§5.4: a batch is visible from assembly, and promoted after its cheap tier.** New event `draft-opened`, the optional `adapter.Drafter` of a forge (`OpenDraft`, `Promote`), `BuildRequest.Merged`, and `[queue] drafts`.
- **§5.1, §5.3 and I15: the queue is a bag.** Waiting lines have no place; assembly takes them by policy: a manual priority first, then by the age of each stack's bottom. A line keeps its age when it comes back or is offered again. New events `prioritise` and `priority-ended`, command `priority`. Adopted in [the queue as a bag](docs/queue-as-bag.md).
- **§7: an owner entry names a live owner.** A line that waits on an owner with no fresh heartbeat has no owner, and is raised after `ownerless` (1h). New `[owners]` in the configuration: `heartbeats`, `alive`, `ownerless`.
- **§5.4: a line check is claimed, in the store,** with a lease, keyed by the heads and the trunk. New events `check-claimed` and `claim-expired`, command `claim`, deadline `claim` (30m). The model's fault `ClaimExpired` gives up a check in flight.
- **§5.5: a seeded failure is never a flake.** A failure that names the seed of a randomised test is class 4, and its seed is kept in the evidence. The example blame hook's `seeded` patterns find it.

- **I13 and §5.2: the runner rebases only a line in conflict,** once for its heads against a trunk. The model carries it as `NeededRebaseOnly` and `NoRideInConflict`, with the conflict and the rebase as actions.
- **§5.6: a test that scans the whole tree** is marked by the blame hook (`tree_wide`), and its failure in two unrelated batches no longer parks the queue at J1.
- **§7: a hold past its review time and a charged line long with its owner are raised to the judge,** and never acted on. New deadlines `review` (24h) and `owner` (12h).

- **§5.4 and I12, tiers by content:** a line of inert content only takes a light tier and joins any batch whatever its size; a batch is light only when every line is. New package `tier`, `[tiers]` in the configuration, `Line.Inert`, `Train.Light`, and the optional `Filer` of a forge. The model carries I12 as `LightOnlyIfAllInert`.

- **§5.4: a charge is on the heads the line has now, and on that line only.** The model has a line check in flight (`LineCheckStart`, `LineCheckDropped`) and the property `ChargedOnlyAtPresentHead`.

- **P3 and I11, the owner-state:** a charged line is with its owner until its owner offers it again, at heads other than those it was charged on. New event `hand-over`; `Offer` carries who offers. The model carries the rule on heads as `NoRideAtChargedHead`, with re-offers (`Reoffer`), and a third configuration with two faults reaches them.
- **§5.2, the runner rebases what needs no person:** the line check may answer a conflict, which charges nobody; new effect `rebase`, new event `rebase-failed`, and an optional `[hooks.rebase]`.
- **§5.5, a recorded flake never charges a line,** even when its rerun fails again and the batch is one line. A timing failure's second failure is still evidence. This reverses 0.3's own first answer to open question 3 for recorded flakes.

- **§5.3: only the first batch is judged.** A failure reported behind an undecided batch is kept and judged when every batch ahead has landed, or dropped when one doesn't. The model carries it as `JudgedOnlyAtHead`.
- **J1's default for a batch of one line is a charge by elimination,** not a park of the whole queue.
- **§6.4, L14: a park with a condition the runner can test never ends on its deadline.** New command `restore`; `cancel --action park` and `judge park` take `--until landed:<line>`.
- **The model has needs first** (`Needs`, `NeedsFirst`, `BaseLost`), checked on a second configuration with a stack.

- **The line check, and I10** (§5.4; from five days of the script queue's failures): the runner checks each waiting line alone, merged with trunk, and no line is assembled without a current green check. New event `line-checked`, new row L33 (a waiting line charged by its check, with evidence), a new guard on L15, and `require_check` in the queue's configuration. The TLA+ model carries it as `AssembledOnlyChecked`.
- **§5.4, tier 1 is on the change merged with trunk,** not "on its own base".

- **Needs first** (the owner's ruling on [open question 27](docs/open-questions.md); [`docs/dependency-graph.md`](docs/dependency-graph.md)). **I3** is "needs first": a stacked line waits in the queue and rides behind its base, in the same train or a later one. **waiting-for-base is retired** (kept only for lines queued before), its rows offered → waiting-for-base and held → waiting-for-base gone, and the line rows renumbered. **New row L24**, waiting → returned, uncharged, when its base is returned or dropped; L15's guard carries the need; L16 leaves out a line whose base was left out. **§5.2:** a need is read from the forge and from ancestry, and a line where they disagree is **stale** (new events stale and fresh). **§5.3:** a line whose base is skipped is skipped. **§5.4:** a stack top's tier-1 check counts for the changes below it. **L1** allows a line to wait, visibly, behind a held, parked or stale base.
- **§3, producer obligations P1 and P2** (owner rules): lines are minimal, and a change is stacked only when it builds on an unlanded change's code, with the reason in the offer. A line is one change; a stack is a sequence of lines, base first. [`docs/dependency-graph.md`](docs/dependency-graph.md) is the possible successor model.
- **I3** became "lines are whole; bases first", since a stack is no longer one line (now "needs first", above). §5.6's whole-line blame says the same; J1 splits whole lines.

- **§5.4, L16** (then L18): a pre-flight may leave a line out, uncharged, as assembly leaves out a conflict ([open question 25](docs/open-questions.md)).
- **§5.5:** a known flake's or a timing failure's rerun that fails again is class 4, and blame is asked again with the class forced ([open question 3](docs/open-questions.md)).

**0.2, 18 November 2026.** From a review of the script queue this protocol was drawn from, against 0.1.

- **§5.1 is now the state machine:** every state of a line and of a train, and every transition with its guard, its owner and its safe default. It replaces the sketch "States of a line". The tables are normative.
- **New invariants: I8, one state, one record; I9, the lander verifies.** The old queue held a line's state across many files and read a batch's verdict from the batch's own output.
- **I4 is tested at landing,** not only when CI passes.
- **New state "held", for one line** (§5.1). A hold on a riding line voids its batch, and a held line is never spent. In 0.1 only the whole queue could be set aside, and a hold in the old queue silently spent lines.
- **New §5.9, Abort:** at any stage before the push, naming each line's fate, charging nobody. A cancelled batch never lands.
- **New failure class 0, "no verdict"** (§5.5): a cancelled, skipped or unreadable run judges nothing and charges nobody.
- **The fallback merge is gone** (§5.7). 0.1 said I1 still held for the tree; it doesn't when trunk has moved, which is when the fast-forward is refused.
- **The gate holds at every transition** (§5.2), not only at the offer.
- **Blame names lines** (§5.6): a batch of one line that blame names is charged, a batch of one that blame can't name goes to J1 and can't be split, and returning a line includes telling its owner.
- **New §5.10, Retry budgets:** counters are fields of the record, and they reset and expire.
- **Parks carry a restore condition** the runner can test (§6.4).
- **L1 names every end of a line; new L3, watchers are watched.**
- **§5.4:** a selection that leaves checks out must over-approximate, at tier 1 too.
- **§7:** a wrong builder report, the runner's own replacement, and silent watchers join the failure model.
- **J3** gains triggers (repeated no-verdicts, a refused fast-forward, a second writer) and a choice (adopt a foreign trunk).
- **§9** names the model, the pure function and the replayer.

**0.1, 6 November 2026.** First draft.
