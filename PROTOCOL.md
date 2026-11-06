# Shunter: a merge queue with a judge in the loop

*Draft 0.1, 6 November 2026. The protocol behind a merge queue that lands work from many concurrent agents onto one branch: its mechanism, the invariants no actor may break, and the points where judgement decides.*

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

- **Producers** (agents) write changes, run cheap local checks, and offer a change for landing as a **line**: one change, or a stack of changes that must land together.
- **The runner** is the only actor that writes trunk. It builds batches, runs CI and lands.
- **The judge** decides at the judgement points. Today that's the coordinator, an AI session; any actor that meets §6's contract will do.
- **The forge and CI** (for example, GitHub and its runners) are external and fallible.

## 4. Invariants

These are safety properties. Every reachable state must satisfy them, whatever the producers, the forge or the judge do.

- **I1, tested trunk.** Every commit trunk points to was the head of a CI run that passed with exactly that tree.
- **I2, fast-forward only.** Trunk's history only grows. No landing rewrites or drops a commit already on trunk.
- **I3, stacks are whole.** A stack lands entirely or not at all, and never in an order that lands a change before its base.
- **I4, frozen while riding.** A change's head doesn't move while it rides a batch. If it moves, the batch is void and doesn't land.
- **I5, speculation is exact.** In a speculation chain (§5.3), batch *k* is built on the exact head of batch *k−1*, and lands only if batches 1…*k−1* all landed at exactly the heads it was built on. Batches land in chain order.
- **I6, blame needs evidence.** A change goes back to its owner as charged only when a blame rule or a recorded judgement names it.
- **I7, one writer.** At most one actor fast-forwards trunk.

Liveness:

- **L1.** Every offered line eventually lands, or returns to its owner with a reason, or is held by a recorded judgement visible to its owner.
- **L2.** If the forge and CI eventually behave, and trunk is healthy, a line with no fault of its own eventually lands.

## 5. Mechanism

### 5.1 States of a line

```
offered ──gate──▶ waiting ──join──▶ riding ──CI pass──▶ landed
   │                │  ▲               │
   │ refused        │  │ parked /      ├─CI fail, blamed──▶ returned (to its owner, with evidence)
   ▼                ▼  │ restored      ├─CI fail, unblamed──▶ judgement (§6.1)
returned         waiting-for-base      ├─infrastructure──▶ waiting (retried)
                 (lines itself when    └─head moved──▶ waiting (void batch)
                  its base lands)
```

### 5.2 The gate

A line is accepted when:
- its changes are open and from approved producers;
- each change's base has landed, or is earlier in the same line;
- it isn't already waiting or riding.

A line whose base is queued but not landed **waits for its base** and lines itself when the base lands. Nobody polls.

### 5.3 Batches and speculation

- **Joining:** the runner joins waiting lines into a batch, in order. It **skips** a line it can't take (it shares files with the batch, must run alone, or would exceed the caps) rather than stopping at it. Skipping is safe because no line is stacked on another line (§5.2).
- **Speculation, to depth D:** while batch 1 runs, the runner builds batch 2 on batch 1's exact head, batch 3 on batch 2's, and so on up to depth D. All D batches run CI at the same time (I5).
- **Landing is in chain order:** when batch 1 lands, the chain shifts up, batch 2 becomes batch 1, and the runner extends the chain at its tail.
- **When batch *j* fails, or lands any other way:** batches *j+1…D* are void, because the head they were built on will never exist. They're rebuilt on trunk, minus whatever blame removed from batch *j*.
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

**The selection is data-driven, in both directions:**
- **Promotion:** every failure CI catches is recorded with the test, its location and its run time. A failure CI catches often, which a check could catch in seconds, is promoted into pre-flight.
- **Demotion:** every pre-flight step's catch history is recorded. A step that has caught nothing in a long window (on the order of 150 batches) is dropped.

The review that promotes and demotes is judgement point J6.

### 5.5 Classifying a failure

Each failure gets one class, decided in this order:
1. **Infrastructure:** no check ran (setup, tooling, the runner lost), or the forge refused for a transient reason (§2.2). The batch reruns, or the line goes back unchanged.
2. **Known flake:** a test recorded as flaky failed. One rerun.
3. **Timing:** only timeouts or lost connections. One rerun.
4. **Deterministic:** an assertion, or a compile or lint error. No rerun; go to blame.

### 5.6 Blame

Blame maps the evidence in a deterministic failure to changes. The most specific rule goes first:
- a rule a change's **description** breaks (for example, "closes an issue whose gap it leaves listed");
- an error at a line a change **added**;
- a failing test in files a change **touches**.

The outcomes:
- **Blame names some changes, but not all:** those return to their owners, and the rest go again whole.
- **Blame names none, or all:** that's not a decision blame can make → **judgement point J1** (§6.1).

### 5.7 Landing

- The runner fast-forwards trunk to the batch's tested head (I1, I2). Transient forge refusals are retried (§2.2).
- If the fast-forward is refused permanently, a judgement-free fallback lands the batch by the forge's merge (I1 still holds for the tree, though not for commit identity), voids speculation, and resumes speculation once nothing rides on the old head.

### 5.8 Flaky tests

Flakes set *p*, the chance that a batch passes on its merits, and *p* sets batch size, speculation depth and waste. With 1,000 tests that each flake 0.01% of the time, about one run in ten fails from flakes alone, so unmanaged flakes cap *p* near 0.9.

- **Flake score.** Each test's failure rate on code where it should pass, kept over a rolling window. The evidence is a rerun that passes, a failure on trunk-equivalent code that passes unchanged, and the nightly order-shuffled and run-twice runs.
- **Cost.** A flake's cost is its score × the batches it fails × the changes in those batches. Fix the most costly first, not the most numerous.
- **Quarantine, bounded.** A test whose score crosses the threshold still runs and is still reported, but doesn't gate a batch. Each quarantined test has an owner, an expiry date and an issue. The quarantine count is a ratchet: it only goes down. When a test's expiry passes, judgement point J5 runs again.
- **Retry the test, not the batch.** Only a quarantined or known-flaky test may be retried in place. Any other failing test gets no retry: a fresh failure is evidence.
- **Prevention.** A new or changed test runs twice, and in shuffled order, before it lands, and a leak audit fails a test that changes shared state (each test counts the shared fixtures before and after it runs). Flakes are cheapest to stop at the change that introduces them.
- **Feeding D.** The judge's choice of speculation depth (J4) reads *p* net of quarantined tests.

## 6. Judgement points

Each point defines what the judge **sees**, what it **may choose**, the **deadline**, and the **default** if the judge is silent. Every choice is recorded with its evidence.

### J1. A deterministic failure that blame can't attribute

- **Sees:** the failure, the batch's changes, the last N batches' results, trunk's recent health (nightly runs, other batches failing on the same test), and the changes' descriptions.
- **May choose:**
  - charge changes X (stating the evidence);
  - split the batch (whole stacks);
  - **park** (§6.4): the failure isn't the batch's, trunk or a shared fixture is broken;
  - rerun once (stating why it might differ).
- **Deadline:** 10 minutes. **Default:** if the same test failed in the previous batch with unrelated changes, park. Otherwise split.
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

- **Triggered by:** a transient failure that persists past its retry budget.
- **May choose:**
  - keep waiting, with a new budget;
  - pause the queue;
  - treat the failure as permanent (stating why).
- **Default:** pause, and alert a human.

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

## 7. The failure model

The protocol assumes all of the following, and must stay safe under any combination of them:
- the forge refuses, errors, times out, or answers late; a refusal may be transient or permanent;
- CI hangs, loses runners, serves unreadable logs, or reports a result for the wrong attempt;
- tests are flaky, order-dependent, or leak state into each other;
- trunk breaks without any batch's fault;
- producers push to a riding change, retire mid-line, or offer duplicate lines;
- the judge is slow, silent or wrong (safety must still hold; only liveness may suffer);
- the runner restarts at any point. All its state is on disk, and it resumes from what it finds.

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

- **A TLA+ model** of §5 and §6, with the forge, CI and the judge as adversaries. TLC checks I1–I7 under every interleaving and every failure in §7, and checks L1 under fairness for the forge and CI.
- **The judge is modelled as nondeterministic choice within its menu.** So the model proves safety whatever the judge decides, which is the point of bounding judgement.

## 10. Open questions

- **Speculating on failure as well as success** (as SubmitQueue does): alongside "batch 2 on batch 1's head", also build "batch 2 on trunk", for when batch 1 fails. This is exponential in general, but bounded to one alternative it removes the rebuild delay after a failure.
- **The depth policy:** a fixed D, D chosen by the judge (J4), or D computed from the pass rate. Note that adaptive *batch size* was considered and rejected, as pre-splitting without cause; depth is different, because it spends spare capacity and never shrinks a batch.
- Whether J1's default should be "park" rather than "split" whenever any doubt exists.
- How promoted rules are retired when they stop firing.
