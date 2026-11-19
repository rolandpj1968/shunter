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

**Producer obligations.** Two owner rules bind producers. A line does two jobs, ordering changes and landing them all or none, and most stacks need only the first; joining changes that need only an order into one line made older, independent changes wait on failures in newer ones. [`docs/dependency-graph.md`](docs/dependency-graph.md) evaluates a possible successor model, in which the order is derived from ancestry.

- **P1, lines are minimal.** A line is one change. A stack is offered as a sequence of lines, base first, and a stacked line waits for its base (§5.1, waiting-for-base) while the base is waiting or riding. Changes share one line only when neither can pass CI without the other, and the offer says why.
- **P2, stack only when necessary.** A producer branches each change from trunk, unless it builds on an unlanded change's code: it calls, edits or tests something that change adds. A stacked change's offer says why ("needs A's *X*"). Neighbouring files, a shared session or a shared author are no reason.

The reasons are recorded with the line and shown to the judge. The gate doesn't judge them: a wrong reason costs time, never a wrong landing (I1).

## 4. Invariants

These are safety properties. Every reachable state must satisfy them, whatever the producers, the forge or the judge do.

- **I1, tested trunk.** Every commit trunk points to was the head of a CI run that passed with exactly that tree.
- **I2, fast-forward only.** Trunk's history only grows. No landing rewrites or drops a commit already on trunk.
- **I3, lines are whole; bases first.** A line lands entirely or not at all. No change lands before its base, whether the base is earlier in the same line or another line (P1).
- **I4, frozen while riding.** A change's head doesn't move while it rides a batch. If it moves, the batch is void and doesn't land. The test is made at landing, not only when CI passes: a batch that waits behind another in a speculation chain is tested again when its turn comes.
- **I5, speculation is exact.** In a speculation chain (§5.3), batch *k* is built on the exact head of batch *k−1*, and lands only if batches 1…*k−1* all landed at exactly the heads it was built on. Batches land in chain order.
- **I6, blame needs evidence.** A change goes back to its owner as charged only when a blame rule or a recorded judgement names it.
- **I7, one writer.** At most one actor fast-forwards trunk.
- **I8, one state, one record.** Every line is in exactly one state of §5.1, held in one store, and so is every train. A transition is a recorded act: the subject, from, to, the actor, the time and the reason. No actor infers a line's state from anything else, and a transition that isn't in the tables of §5.1 can't be written.
- **I9, the lander verifies.** Before it fast-forwards trunk, the runner itself reads from the forge that the batch's CI passed on exactly the head it is about to push, that the head is the one it built, that every change's head is the one it assembled (I4), and that trunk is where the batch was built (I5). It never lands on a builder's report, and no protocol message travels in a channel that tools or tests can write to.

Two consequences are worth stating on their own, because the queue this protocol grew from broke both:

- **A cancelled batch never lands.** Once aborted (§5.9), a batch has no transition out.
- **A held line is never spent.** A hold (§5.1) has one way out, its release, and that leads back into the queue.

Liveness:

- **L1.** Every offered line eventually lands, or returns to its owner with a reason, or is withdrawn by its owner, or is held or parked by a recorded judgement visible to its owner. A line in any of the first three conditions is **spent**. No line is spent in any other way, and none silently.
- **L2.** If the forge and CI eventually behave, and trunk is healthy, a line with no fault of its own eventually lands.
- **L3, watchers are watched.** Every periodic check the protocol relies on (trunk's health, the age of waiting lines, the judge's own liveness, each producer's) records each run and its result. A check that hasn't run within twice its period, or has failed to run three times, is itself an alert. An error inside a check is never read as "nothing to report".

## 5. Mechanism

### 5.1 State machine

A line and a train are each in exactly one state (I8). The tables below are the whole machine: a transition that isn't a row can't be written. Each row names its trigger, its guard, who decides it (a rule or a judgement point), and the safe default: what happens if the one who decides is silent, or the guard fails.

**States of a line**

| State | Meaning |
|---|---|
| **offered** | A producer has put it forward. The gate (§5.2) hasn't answered. |
| **waiting** | Queued. It may join a train. Lines wait in the order they were offered, and a line that comes back takes the place it had. |
| **waiting-for-base** | Queued, stacked on another line (its base) that is waiting, riding, held or parked (P1). It joins no train until its base lands, and then it is waiting, in the place it had (L26). |
| **held** | A recorded judgement keeps this one line out of trains. It stays visible to its owner, with the reason. A hold on a riding line voids its train, and the rest of the train goes again. Lifting the hold sends the line through the gate again, to the place it had. A held line is never spent. |
| **parked** | Moved aside under a park (§6.4) that has a restore condition. Restored by the runner when the condition holds. |
| **riding** | In a live train. Its stage is the train's state, below: a line "rides as train 1 or 2" by its train's place in the chain, and is "green" or "landing" when its train is. Its heads are frozen (I4). |
| **landed** | Spent: on trunk. |
| **returned** | Spent: back with its owner, who has been told why. The reason is one of: refused at the gate; left out at assembly; charged with a failure, with the evidence; returned by an abort; its base didn't land. |
| **dropped** | Spent: withdrawn by its owner. |

A spent line that didn't land may be offered again. That's a new offer: a new place in the queue and fresh retry budgets (§5.10).

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
| L4 | offered | waiting | gate-accept | its base has landed, or it has none; no park covers it | the gate (§5.2) | stays offered |
| L5 | offered | waiting-for-base | gate-accept | its base is live | the gate (§5.2) | stays offered |
| L6 | offered | parked | gate-accept | the queue is parked and this is not the park's fix | the gate (§5.2) | stays offered |
| L7 | offered | returned | gate-refuse | a reason is given | the gate (§5.2) | stays offered |
| L8 | waiting | held | hold | a reason is given | the judge, or a human | refused; the line stays where it is |
| L9 | waiting-for-base | held | hold | a reason is given | the judge, or a human | refused; the line stays where it is |
| L10 | parked | held | hold | a reason is given | the judge, or a human | refused; the line stays where it is |
| L11 | riding | held | hold | a reason is given; its train is not landing; the train goes void | the judge, or a human | refused while landing |
| L12 | held | waiting | release | it is held | the judge, or a human | stays held, visible to its owner |
| L13 | held | waiting-for-base | release | it is held; its base has not landed | the judge, or a human | stays held |
| L14 | held | parked | release | it is held; a park covers it | the judge, or a human | stays held |
| L15 | waiting | parked | park | the park has a restore condition | J1, J2 or the judge | not parked |
| L16 | parked | waiting | restore | the park exists; no other park covers the line | the runner, when the condition holds | restored at the park's deadline, with an alert |
| L17 | waiting | riding | assemble | not held, not parked; alone if it must be; one split group per train | the runner's joining rule (§5.3) | stays waiting |
| L18 | riding | returned | leave-out | the train is assembling; a reason is given | the assembly rules (conflict, caps), or a pre-flight hook's leave-out (§5.4) | refused without a reason |
| L19 | riding | returned | preflight-failed, ci-failed, judge (charge) | evidence names the line (I6) | a blame rule (§5.6), or J1 | never charged without evidence |
| L20 | riding | returned | abort (return) | the train is live and not landing | the judge (§5.9) | requeue |
| L21 | riding | waiting | any end of its train that is not a landing | the gate passes it | the gate (§5.2) | the gate decides again |
| L22 | riding | parked | any end of its train that is not a landing | a park covers it | the gate (§5.2) | the gate decides again |
| L23 | riding | waiting | abort (requeue) | the train is live and not landing | the judge (§5.9) | requeue |
| L24 | riding | parked | abort (park) | the train is live and not landing; a restore condition is given | the judge (§5.9) | requeue |
| L25 | riding | landed | push-ok | its train is landing | the runner | none: the push decided |
| L26 | waiting-for-base | waiting | push-ok (of its base) | no park covers it | the gate (§5.2) | the gate decides again |
| L27 | waiting-for-base | parked | push-ok (of its base) | a park covers it | the gate (§5.2) | the gate decides again |
| L28 | waiting-for-base | returned | its base is returned or dropped | always | rule: bases first (I3) | returned with the reason |
| L29 | offered | dropped | withdraw | none | its owner | refused; the line stays where it is |
| L30 | waiting | dropped | withdraw | none | its owner | refused; the line stays where it is |
| L31 | waiting-for-base | dropped | withdraw | none | its owner | refused; the line stays where it is |
| L32 | parked | dropped | withdraw | none | its owner | refused; the line stays where it is |
| L33 | riding | dropped | withdraw | its train is not landing; the train goes void | its owner | refused while landing |

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

- **Every way back into the queue goes through the gate.** Rows L12–L14, L16, L21–L24, L26 and L27 are one rule applied to different arrivals: the line goes where the gate would put it now (§5.2).
- **Nothing leaves "held" except by release** (L12–L14). A held line can't join, can't be parked afresh, can't be charged, and can't be withdrawn until it's released. So a hold can never spend a line.
- **Nothing leaves "aborted", "void", "failed", "empty" or "landed".** A late report for an ended train is refused and logged.
- **Only T13 moves trunk,** and only from "landing", which only T12 enters.
- **A line returns only with a reason** (L7, L18–L20, L28), and as charged only with evidence (L19, I6).

```
                    ┌───────── held ◀── hold ── (waiting, waiting-for-base, parked, riding)
                    │ release
                    ▼
 offered ──gate──▶ waiting ◀──restore── parked ◀── park / the gate under a park
    │               │  ▲  ▲
    │ refused       │  │  └── base landed ── waiting-for-base ── base lost ──▶ returned
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
- each change's base has landed, or is earlier in the same line, or is in another live line (then it waits for its base);
- it isn't already waiting or riding.

A line whose base is queued but not landed **waits for its base** and lines itself when the base lands. Nobody polls. If its base is returned or withdrawn instead, the line returns too, with that reason (I3).

**A gate condition holds at every later transition, not only at the offer.** Whatever would refuse, hold or park a line at the gate (a park, a hold, an unlanded base) is tested again whenever the line would enter the queue or join a batch, including a batch rebuilt after a failure or a void. A line that no longer passes goes to the state the gate would give it. It doesn't ride and fail.

### 5.3 Batches and speculation

- **Joining:** the runner joins waiting lines into a batch, in order. It **skips** a line it can't take (it shares files with the batch, must run alone, or would exceed the caps: a number of lines, and a number of changes, a stack's every change counted) rather than stopping at it. A line that alone reaches the change cap rides alone, so that no line waits for ever. Skipping is safe because no waiting line is stacked on a line that hasn't landed: a stacked line waits for its base outside the queue (§5.2).
- **Speculation, to depth D:** while batch 1 runs, the runner builds batch 2 on batch 1's exact head, batch 3 on batch 2's, and so on up to depth D. All D batches run CI at the same time (I5).
- **Landing is in chain order:** when batch 1 lands, the chain shifts up, batch 2 becomes batch 1, and the runner extends the chain at its tail.
- **When batch *j* ends without landing** (it fails, is void, or is aborted): batches *j+1…D* are void, because the head they were built on will never exist. They're rebuilt on trunk, minus whatever blame removed from batch *j*.
- **Choosing D:** the trade-off is throughput against waste. With a batch pass probability *p*, the best case lands D batches per CI cycle, and a failure at position *j* voids D−*j* runs. Deep chains pay when *p* is high (around 0.9), and depth 1 is about the limit when *p* is near 0.5. On metered CI, voided runs cost money; on owned runners they cost only capacity, so D is bounded by free runner slots (D × one batch's jobs) more than by spend. D is set at judgement point J4.
- **Change-based CI:** a batch's CI may run only the jobs its change can affect. "Its change" is measured against the batch beneath it, not trunk, or speculation would count batch 1's change twice.

### 5.4 Pre-flight

There are three tiers of checking, and each answers a different question:

| Tier | Runs on | Question |
|---|---|---|
| **1. Producer pre-checks** | the change, on its own base, before it's offered as a line | Does this change work at all? |
| **2. Batch pre-flight** | the assembled batch, on the runner, before CI is dispatched | Did *combining* these changes break something cheap to detect? |
| **3. CI** | the batch, in full | Is this exact state safe to land? |

Tier 2 runs on every batch, before any CI is spent. A failure there sends the changes it names back (by the blame rules of §5.6), with no rerun.

**A pre-flight may leave a line out instead.** Some failures say "this line didn't fit this train", not "this line broke it": it doesn't build on the train, or a generated part clashes with another line's (two changes taking the same number). A pre-flight may answer a leave-out for such a line, with a reason, as assembly leaves out a conflict (L18). The line goes back to its owner uncharged, told that it didn't fit; the train is built again without it, and pre-flight runs again on the rest. A leave-out is not a charge (I6), so it needs no evidence beyond its reason, and it uses no retry budget.

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

**A rerun that fails again is evidence.** When a known flake's or a timing failure's one rerun fails the same way, the failure is class 4 from then on: it goes to blame (§5.6), which is asked again with the class forced, and names lines by their files as for any deterministic failure. A line that makes a test slow, or makes a flaky test it touches fail every time, goes back to its owner without a judge. Blame that names none, or all of several, goes to J1 as before.

### 5.6 Blame

Blame maps the evidence in a deterministic failure to changes. The most specific rule goes first:
- a rule a change's **description** breaks (for example, "closes an issue whose gap it leaves listed");
- an error at a line a change **added**;
- a failing test in files a change **touches**.

The outcomes:
- **Blame names some lines, but not all:** those return to their owners, and the rest go again whole. A batch of one line that blame names, with evidence, is this case too.
- **Blame names none, or all of several:** that's not a decision blame can make → **judgement point J1** (§6.1).

Blame works in whole lines: a line is charged or cleared together (I3). Lines stacked on a returned line go back with it, uncharged, with that reason (L28).

Returning a line **includes telling its owner**, in the same recorded act, with the evidence. A line isn't "returned" until the telling is recorded as delivered or as owed.

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

An abort is not a failure. It charges no change (I6) and uses no retry budget. Aborting twice is the same as once. The one stage an abort can't reach is the push itself (§5.1, "landing"): the request is refused, and the caller hears that the batch is past the point of no return.

### 5.10 Retry budgets

Every counter that changes how a line or a batch is treated is a field of its record (I8), never a side file: a line's no-verdict strikes, a batch's reruns, a landing's refused pushes. A line's budgets reset when it lands, when it's offered again, and when its head moves. They expire after a stated time. A budget that runs out goes to J3. It never turns into a charge.

## 6. Judgement points

Each point defines what the judge **sees**, what it **may choose**, the **deadline**, and the **default** if the judge is silent. Every choice is recorded with its evidence.

### J1. A deterministic failure that blame can't attribute

- **Sees:** the failure, the batch's changes, the last N batches' results, trunk's recent health (nightly runs, other batches failing on the same test), and the changes' descriptions.
- **May choose:**
  - charge changes X (stating the evidence);
  - split the batch (whole lines);
  - **park** (§6.4): the failure isn't the batch's, trunk or a shared fixture is broken;
  - rerun once (stating why it might differ).
- **May not choose:** to return a line with no evidence (I6). A batch of one line can't be split, so for it the menu is charge with evidence, park, or rerun.
- **Deadline:** 10 minutes. **Default:** if the same test failed in the previous batch with unrelated changes, or the batch is one line, park. Otherwise split. The halves of a split ride apart until they land.
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

Batch size caps, speculation depth D (§5.3), which jobs a batch runs. A human sets the bounds; within them, the judge sets D from the recent pass rate and the runner slots free.
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

Every park carries a **restore condition the runner can test:** a named change has landed, a measure is above a threshold, a time. The runner restores when it holds, and says so. A park whose condition can't be tested carries a deadline, after which the default restores and alerts. Restored lines pass the gate again (§5.2) and take the places they had.

A park may also cover named lines only, as an abort's disposal of them (§5.9). **To keep one line out of batches, use a hold (§5.1), not a park:** a hold names the line, shows its owner the reason, and can't spend it.

## 7. The failure model

The protocol assumes all of the following, and must stay safe under any combination of them:
- the forge refuses, errors, times out, or answers late; a refusal may be transient or permanent;
- CI hangs, loses runners, serves unreadable logs, or reports a result for the wrong attempt;
- tests are flaky, order-dependent, or leak state into each other;
- trunk breaks without any batch's fault;
- producers push to a riding change, retire mid-line, offer duplicate lines, or hang silently (a producer stuck in a tool call holds its work invisibly: each producer needs a liveness signal, and work held by a silent producer is handed on after a deadline);
- the judge is slow, silent or wrong (safety must still hold; only liveness may suffer);
- the runner restarts at any point. All its state is in one store (I8), and it resumes from what it finds;
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

- **§3, producer obligations P1 and P2** (owner rules): lines are minimal, and a change is stacked only when it builds on an unlanded change's code, with the reason in the offer. A line is one change; a stack is a sequence of lines, base first. [`docs/dependency-graph.md`](docs/dependency-graph.md) is the possible successor model.
- **I3** is "lines are whole; bases first", since a stack is no longer one line. L28's rule and §5.6's whole-line blame say the same; J1 splits whole lines.
- **§5.1, waiting-for-base** names the states its base may be in, and that it rejoins the queue in its place. **§5.2** names a base in another live line, and **§5.3** restates why skipping is safe.

- **§5.4, L18:** a pre-flight may leave a line out, uncharged, as assembly leaves out a conflict ([open question 25](docs/open-questions.md)).
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
