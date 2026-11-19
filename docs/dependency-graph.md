# Lines as a dependency graph

*A design note for the owner to rule on ([open question 27](open-questions.md)). It binds nothing in [`PROTOCOL.md`](../PROTOCOL.md) until the owner rules. It evaluates replacing the line, as the unit of ordering, with an explicit graph of what each change needs, and recommends a first step.*

## 1. The problem

A **line** (§3) is "one change, or a stack of changes that must land together". It does two jobs at once:

- it **orders** changes: a stack's upper change needs its lower one first;
- it **lands them atomically**: all of the line, or none of it (I3).

Most stacks need only the first job. A producer stacks change B on change A because B calls code A adds; A doesn't need B at all. Putting both in one line also gives them the second job, and that costs time: when B fails, A goes back with it, though nothing is wrong with A. Producers also joined changes into one line that didn't depend on each other at all (neighbouring files, one session, one author). An older, independent change then waited, sometimes for many hours, on failures in newer changes stacked on it.

So the line is an inaccurate dependency graph. It records "these go together" where the truth is usually "this one goes after that one".

The interim rule, now producer obligation P1 in §3: **lines are minimal.** A line is one change. A stack is offered as a sequence of lines, base first, and a line stacked on another waits for its base (`waiting-for-base`). Changes share a line only when neither can pass CI without the other, and the offer says why. A second obligation, P2, goes with it: **stack only when necessary.** A producer branches each change from trunk unless it builds on an unlanded change's code, and the offer says why.

Under the interim rule, ordering and atomicity are already separate: ordering is the base link between lines, atomicity is the line. What it doesn't do is let a stacked line ride before its base has *landed*. B waits a full train behind A, even when both could ride in one train with A first. This note asks how far to take the separation.

## 2. The model

**Nodes are changes.** Each change has an id, a head, and a state, as today.

**A "needs" edge, derived.** B **needs** A when A has not landed and B's head contains A's head: `git merge-base --is-ancestor A.head B.head`, for A and B both live. The edge is mechanical. Nobody declares it, and nobody can forget it. It is derived again whenever a head moves.

**A "co-requires" edge, declared.** A producer may declare that A and B co-require each other: neither can pass CI without the other (a change to an interface and its only caller, split for review). Co-requires is symmetric, and a set of changes connected by co-requires edges is one **group**: the strongly connected component of the co-requires relation.

**The landing unit is the group.** A change with no co-requires edge is a group of one. A group lands entirely or not at all; within it, changes land in needs order. "Line" is kept as the name of the landing unit: **a line is one change, or a group of changes that co-require one another.** Today's multi-change line, offered as `b 101 102`, *is* the declaration of a group, so no new syntax is needed for co-requires.

**Needs between lines.** Line M needs line L when some change of M needs some change of L. Ancestry is acyclic, so needs edges between changes are acyclic. Needs edges between lines can form a cycle only when a group's members are interleaved with another line's in ancestry (A ← X ← B, with A and B one group and X another line). That cycle means X can't land without A and B, and B can't land without X: X is in fact part of the group. The gate refuses such an offer, with the reason "X sits between members of one group: offer it in the group, or rebase it out". Nothing is merged silently.

**Needs are a liveness device, not a safety one.** I1 already guarantees trunk only ever holds a tree CI passed. A wrong needs edge can make a change wait, or ride in a train whose CI fails; it can never put an untested tree on trunk. This bounds the cost of every mistake in deriving or declaring edges.

## 3. Train assembly

**The ready frontier.** The joining rule (§5.3) walks the waiting lines in queue order, as today, and takes line M into train *k* when:

- every line M needs has **landed**, or **rides in an earlier train of the chain** (1…*k*−1), or **has already been taken into train *k***; and
- the existing tests pass: not held or parked, not alone unless first, inside the caps, of the same split half.

A line taken into a train goes after everything it needs, because the walk is in queue order and a line is taken only after its needs. A train's line order is therefore a topological order of the needs graph, and the build replays it in that order.

**Skipped needs propagate.** A line the walk skips (held, parked, must ride alone, past a cap, of another split half) has not been taken. Any later line that needs it fails the first test, and is skipped too, in the same walk. A line is never taken into a train whose build would have to leave it out because its base is missing. (The live script queue showed the waste of the opposite: it skipped a held or capped line, took a later line stacked on it, and the build left that line out, costing a slot and a round trip.) This falls out of the rule; it needs no extra case.

**Queue order and needs order.** A base is normally offered before what is stacked on it. If a need comes back behind its dependents (it was returned and offered again), the dependents are skipped until it is taken; they keep their places. At worst this costs one train, never a wrong build.

**Speculation is unchanged.** Train *k* is built on train *k*−1's exact head (I5), so a line that needs a line in train *k*−1 is built on top of it. If train *k*−1 doesn't land, train *k* is void (§5.3), its lines go back, and the walk runs again.

### Blame

Blame stays evidence first (§5.6) and works in whole lines. One rule is added:

- **A dependent goes with what it needs, uncharged.** When a line is returned (charged with evidence, left out at assembly, returned by an abort), every line in the same train that needs it, directly or through others, is returned too, with the reason "its base didn't land" (a reason §5.1 already lists). It is not charged: blame named its base, not it. Lines in later trains are void anyway, and the walk then skips their dependents: their need is no longer live.

What this buys: blame that names B, stacked on A, returns B alone, and A lands with the rest. Under a stack-as-one-line, A went back with B.

### Bisection, J1 splits and flake reruns

- **A split keeps needs closed.** J1's split (§6.1) divides a train's lines into halves "of whole stacks" today. In the graph, a half must contain the needs of every line in it. Splitting the train's line order at a point does that by construction: the first half is needs-closed, and the second half's needs are in the first half or already earlier. The halves ride as consecutive trains, first half first, so that the second is built on the first (I5). Today the halves ride apart from everything; with needs they must ride in order.
- **Pre-flight bisection** already works on the train's line order (the tips of each line, in order), so it bisects a topological order and stays correct: the first line at whose tip a step fails is the first that tips it, and its dependents go with it, uncharged.
- **Flake reruns (§5.5)** are per train and unchanged. A forced blame after a second flake names lines by files; a named line's dependents go with it, uncharged, by the rule above.
- **Blame naming "all of several"** stays J1's. Note one new shape: blame naming only A, when B (needing A) touches the same files, is still a charge on A alone with B following uncharged. B's owner is told its base was returned and why.

## 4. What it changes in the protocol

The smallest set of changes that makes the model the protocol. Each would be one edit to [`PROTOCOL.md`](../PROTOCOL.md).

| Where | Today | Under the graph |
|---|---|---|
| §3, a **line** | one change; a stack waits for its base (the interim rule) | one change, or a group of changes that co-require one another. Its **needs** are derived from ancestry |
| §5.2, the gate | each change's base has landed, or is earlier in the same line; a base that is live puts the line in waiting-for-base | each change's needs are landed or live; a needs cycle between lines is refused (§2 above) |
| §5.1, **waiting-for-base** | queued until its base *lands*, then lines itself (L26, L27) | **retired.** A line whose needs are live is **waiting**; the joining rule skips it until its needs are landed or riding earlier. Kept as a state only to replay old logs |
| §5.1, new row | none | **waiting → returned**: a line it needs is returned or dropped (L28, extended from waiting-for-base to waiting) |
| §5.1, new row | none | **riding → returned**: a line it needs in the same train is returned (charged, left out, or by abort), reason "its base didn't land", uncharged |
| §5.1, L17 (join) | not held, not parked; alone if it must be | adds: every line it needs is landed, rides earlier in the chain, or is earlier in this train |
| **I3** | lines are whole; bases first: a line lands entirely or not at all, and no change before its base | **needs first:** no change lands before a change it needs, and a group lands entirely or not at all |
| §5.3, skipping | "safe because no line is stacked on another line" | safe because a line is taken only after its needs, and a line whose need is skipped is skipped too |
| §5.6, blame | "a stack is charged or cleared together" | a group is charged or cleared together; a line that needs a returned line goes with it, uncharged |
| J1, split | whole stacks; halves ride apart | whole lines, each half needs-closed; the halves ride as consecutive trains |

Unchanged: I1, I2, I4–I9, every train state and transition, abort, retry budgets, the landing check, and J2–J6.

**Holds.** A hold still names one line, and a held line is still never spent. What is new is that a hold delays its dependents too, which the owners of those dependents didn't ask for. So status shows, for every waiting line the walk skips, the line it waits on and why ("waits on A: held, *reason*"). L1 needs one word more: a line may also wait on a need that is held or parked by a recorded judgement, visibly. A hold on a riding line voids its train, as today, and its dependents in that train go back to waiting, not returned: the held line isn't spent.

**Parking.** A park of the whole queue covers dependents as it covers everything. A park of named lines (an abort's disposal) leaves their dependents waiting and skipped, visible as above.

**"A line that comes back takes the place it had."** Unchanged, and now cheaper to honour: a dependent ahead of its need in queue order is simply skipped until the need is taken.

**The age watcher.** It reports a line's age from its offer, as today, and adds the chain of needs it waits on. Its stall alarm ("lines waiting with nothing in flight") counts only lines the walk could take; a line skipped for a held need is reported as **blocked by** that hold, not as a stall. A line whose needs have all landed, and that has waited longer than the threshold, is an alarm whatever else is true.

**The TLA+ model** needs I3 restated and the joining guard; the skeleton has no stacks today, so the change there is an addition.

## 5. Failure and edge cases

**A needed change returned or withdrawn while its dependents wait.** The choices are: wait, return, or retarget.

- *Return* (recommended). The dependents' heads contain the needed change's old commits. Whatever its owner does next (a fix, a rebase, a withdrawal) the dependents must be rebased to match, which is their owners' act. Returning them at once, with the reason, tells them now instead of after the base is offered again and the ancestry no longer matches. This is today's L28, applied to waiting as well as waiting-for-base.
- *Wait* would leave dependents waiting on a change that is no longer live, which the gate can't test (I8: no state inferred). It would need a new state and a deadline.
- *Retarget* (rebase onto trunk) can't be done for them in general: their code builds on the returned change. It is the producer's act, and §6 below covers the case where it is safe.

**A needed change rebased: the ancestry edge breaks.** A's owner pushes a new A. B still contains A's *old* commits, not A's new head, so the derived edge from B to A disappears, though B still needs A. Treating B as independent would build B with A's stale commits. So the edge is derived from two signals: ancestry, and the change's declared base on the forge (B's base branch is A's branch). When they disagree (the forge says B is stacked on A, ancestry says B doesn't contain A's head) B is **stale on A**: the gate takes it as waiting but the walk skips it, and its owner is told "rebase onto A's new head". It can't ride until it is rebased. A head move of B restarts the check. In the commit graph, B's own commits are those after its declared base's head; any other commit it carries that is neither trunk's nor a live change's is the stale content, and the message can name it.

**Forks: two changes on one base.** B and C both stacked on A, independent of each other. Both need A; neither needs the other. They may ride in one train after A, or in different trains. Blame naming B returns B alone, and C lands. This is the case lines handled worst (a producer had to pick an order, B on C or C on B, and so invented an edge), and the graph handles it with no extra rule. A change that merges two others (a diamond) contains both their heads and needs both.

**A cycle of co-requires.** Co-requires is a grouping relation, so a cycle in it is just a group: A–B, B–C, C–A is one group {A, B, C}. Nothing is refused. The groups to watch are large ones: a group is one line, so a group past the change cap rides alone (as a long line does today, §5.3), and a group that keeps growing is a sign that the declaration is being used for convenience. The gate may refuse a group above a stated size, as a configuration choice.

**Stale ancestry after the base lands as new commits.** With `strategy = "rebase"`, or a forge that squashes, A lands on trunk as new commits A′. B's head still contains A's original commits. The derived edge is computed only among **live** changes: A has landed, so it is no longer a node, and B's need is met by the store's record that A landed, not by trunk's ancestry. When B is built, the rebase drops the commits whose patch is already on trunk ([open question 21](open-questions.md)); a commit whose patch changed in A's train (a conflict resolved) leaves a conflict, which B's owner resolves by rebasing. The forge retarget of question 21 keeps the declared-base signal in step.

## 6. False edges, and producers' part

A derived edge is only as true as the producer's stacking. A change stacked for convenience (neighbouring files, one session, one author) carries a false edge that no queue can see past: B waits for A, and goes back when A does, for no reason.

**(a) Producer guidance** (P2, §3). Branch each change from trunk, unless it builds on an unlanded change's code: it calls, edits or tests something that change adds. When a change is stacked, the offer says why, in one line: "needs A's *X*". Neighbouring files, a shared session or a shared author are no reason. The reason is recorded with the line and shown when the edge delays it.

**(b) Can the runner detect a likely-false edge?** Cheaply, in tiers:

1. **Disjoint files.** B's own diff (its declared base's head to B's head) touches no file A's diff touches. Seconds to compute. A strong hint, not proof: B may call a function A adds in another file.
2. **A clean replay onto trunk.** B's own commits replay onto trunk without A, with no conflict. Seconds. Stronger, still not proof.
3. **The producer's own check, without A.** The replayed B passes the tier-1 pre-check (§5.4) or a configured pre-flight step. Minutes. Strong evidence that B doesn't need A to build; tests may still need it.

A stated reason ("needs A's *X*") overrides all three: the producer has said what the edge is for.

**What to do with a likely-false edge.** Never unstack silently. Unstacking changes what the producer offered, and a wrong unstack costs a failed train. The order of response:

- **Warn**, at the gate: "B is stacked on A, but its diff touches none of A's files and it replays cleanly onto trunk; if it doesn't need A, rebase it onto trunk". This alone fixes most convenience stacks, at no risk. It is recorded, so that a producer that keeps stacking for convenience is visible.
- **Offer to unstack, with consent.** With `strategy = "rebase"`, the runner already replays each change's own commits onto the train. So the runner can drop the edge without touching B's branch: B rides on its own commits, without A, and CI tests exactly that tree (I1). It is a recorded act, asked of B's owner (who knows) and, failing an answer by a deadline, of the judge. The default on silence is to keep the edge: silence costs time, never a wrong landing (§2.5). Under `strategy = "merge"`, B's head carries A's commits and an edge can't be dropped by the runner: only the producer can rebase.
- **Promotion.** If the judge keeps consenting to unstack on the same evidence (tier 3 passing, say), that judgement can become a rule (§2.6): unstack with a notice, and let the owner object.

**(c) Should declared needs override ancestry?** In one direction only.

- **Declared needs may add edges.** B may need A without containing it: B's tests need data A's change introduces, or A must be live on trunk first. A declared need is safe (it only delays B) and is honoured as a derived one is.
- **Declared needs may not remove a derived edge by themselves.** Under `strategy = "merge"`, B's head carries A's commits, and landing B lands A's code: the edge is a fact of the tree, not an opinion. Under `strategy = "rebase"`, a producer's declaration "B doesn't need A" is the consent of (b), and is honoured as the recorded unstack act, with CI as the check. The better fix is still the producer's: rebase B onto trunk, and the edge is gone.

## 7. Cost

**CI per train is unchanged.** A train runs one CI, whatever its lines. Minimal lines put more lines in each train, so the change cap (`max_changes`) matters more than the line cap.

**A producer check per change, or per stack top?** The live queue lets a stack top's green check count for the changes it contains: A's tier-1 pre-check is B's, run on A and B together. Under minimal lines A is its own line, and may ride without B. Should A need its own green?

- **Keep it** (recommended). The tier-1 check is a filter that keeps the pass rate *p* up (§5.8); it isn't what makes trunk safe. A rides in a train whose CI tests A without B, so I1 holds either way. The risk is only that A fails alone where B would have fixed it, and that is exactly the co-requires case, which the producer should have declared. When it happens, the failed train's blame names A with the evidence, and the lesson is a declaration.
- **Drop it** and require a green per change: *k* producer runs for a stack of *k* instead of one. The gain is a slightly higher *p* for stacks whose base is broken alone, which is rare once co-requires is declared.
- **A middle way**, if the rare case recurs: require a change's own green only when it is offered without what is stacked on it, or when the train it rides leaves its dependents behind. That keeps the common case at one run per stack.

**Deriving the edges** costs one ancestry test per pair of live changes on the same base chain, cached by heads: tens of `merge-base` calls, milliseconds each, again only when a head moves.

## 8. Recommendation

1. **Keep the interim producer rules in the protocol** (lines are minimal; stack only when necessary). They are the graph with every edge pointing at a landed base, and they ask nothing new of the runner. Done.
2. **First implementable step: needs from ancestry, and the ready frontier.** Derive needs between lines from the forge's declared base and from ancestry, and treat disagreement as stale (§5). The gate puts a stacked line in **waiting** when its needs are live. The joining rule takes a line when every line it needs has landed, rides earlier in the chain, or is earlier in this train, and so skips a line whose need was skipped. A line whose need is returned or dropped is returned, uncharged; a line riding with a returned need goes with it, uncharged. Groups stay what multi-change lines are today: declared co-requires, landed whole. This is one new guard (L17), two extended rows (L28 to waiting; a dependent in the same train), I3 restated, waiting-for-base retired, and status showing what each waiting line waits on. It saves a train's latency for every stacked change, and returns only what blame named.
3. **Then false-edge warnings** at the gate (tiers 1 and 2 of §6), recorded, with no action but the message.
4. **Then, if the warnings show convenience stacks persist,** the consented unstack under `strategy = "rebase"`.
5. **Not recommended now:** dropping the stack top's green (§7), and declared removal of a derived edge other than as the consented unstack.

What the owner rules on: whether to take step 2, and, within it, return (recommended) or wait for dependents of a returned change.
