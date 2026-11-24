# Shadow divergences, traced (cutover blocker B1)

The cutover review's B1 asks that every divergence the shadow reports is traced and put in one of three classes:

- **(a) intended:** Shunter applies a rule the live queue lacks. The rule is named. It does not block.
- **(b) an adapter gap:** the replay or the shadow misreads the live queue's record. Fixed, with a test.
- **(c) a Shunter bug:** fixed, with a test.

Only (b) and (c) block the cutover. This note traces the daily shadow report of 24 Nov 04:00Z (the 24 hours before it: 40 train logs, 34 trains with a pull request) and the gate comparisons of the shadow's ten-minute runs.

## Summary

| what the report said | count | class | outcome |
|---|---|---|---|
| a marker word not understood (`BATCH-DRAFT`) | 36 | (b) | fixed |
| landed-without-verdict | 3 | (c) | fixed |
| refused: offer "other", gate-accept and assemble "a transition the table does not have" | 2 + 2 + 2 | (b) | fixed |
| refused: judge "a change charged with no evidence (I6)" | 3 | (a) | I6 |
| refused: assemble "lines riding together that may not" | 3 | (a), by mechanism | see below |
| unblamed-without-judge | 6 | (a) | I6, J1 |
| gate divergences, one run each | 6 | not traced | see below |

## The three landings with no passing verdict: (c), fixed

Three trains the live queue landed read as "pending" to Shunter's lander, so it would not have landed them (I9). On each head the train's required check had passed, in the train's own CI run, before it landed. Each head then became trunk, and trunk's own workflows ran on it: the three heads carried 184, 211 and 189 check runs. The GitHub adapter read one page of 100. The required check was on the second page, so the adapter found no required check, and `Classify` said pending.

The adapter now reads every page of a head's check runs and of its workflow runs, to a bound (`maxListPages`); a listing longer than the bound is an error, not a verdict. Test: `TestStatusReadsEveryPage` (adapter/github).

A live Shunter meets this less often than the shadow, which reads heads hours after they landed, but a train head with more than 100 check runs (a large matrix, many reruns) would have stalled it the same way.

## BATCH-DRAFT: (b), fixed

The live queue opens a train's pull request as a draft at assembly and promotes it after pre-flight, and its logs mark the draft with `BATCH-DRAFT <pr> <head>`. The replay adapter did not know the word. It now replays it as `draft-opened`; `BATCH-OPENED` promotes the draft as before. Test: `TestDraftFromAssembly` and the fixture `17-draft.out`, a real train log's protocol lines (trainlog).

## The six refusals in three pairs: (b), fixed

The refused offer ("other"), the refused gate-accept and the refused assembly are one cause, met twice. A train log carries no heads, so the adapter offered every line at the same made-up heads. A line charged in one train and trained again in a later one, after its owner's fix, was refused at the offer as unfixed (I11, counted as "other"); the gate-accept then found the line still returned, and the whole assembly was refused, so the rest of that train's history was skipped.

A charged line rides again only after its owner pushes, so the adapter now gives a charged line new heads when the log trains it again. If the live queue ever re-trained a charged line without a push, the replay cannot see it: the log has no heads to tell. Test: the fixture `18-charged-fixed.out`, which before the fix gave exactly these three refusals.

## The three charges with no evidence: (a), I6

The live queue sends a train of one line back to its owner when its failure names nobody. The adapter replays that as a charge, and the machine refuses it: a charge needs a blame rule or a recorded judgement that names the line (I6). Shunter asks the judge at J1 instead. These are the same trains as three of the six unblamed-without-judge divergences.

## The three assemblies of lines that may not ride together: (a), by mechanism

`ErrJoin` has a few causes: a line marked alone, the halves of a split, a starving line, a base not yet landed (I3), a line in conflict with trunk, a line with no current green check (I10). In replay the likely one is the split. When a train of several lines fails with nobody named, the live queue splits it, and the adapter replays that as a split, which gives the halves groups. The halves then ride apart until they land (J1). The live queue puts them back together freely, and the machine refuses that assembly. The ten-minute runs that reported these refusals came just after an unblamed split.

Every `ErrJoin` cause is a Shunter rule, so all of them are class (a). But the daily report gave only the count, not the message, so which cause each one had is inferred, not read. The labelled report (next) prints each unexplained refusal with its log and message, and counts the intended ones by rule, so the next daily run says which rule each one met.

## The six unblamed-without-judge: (a), I6 and J1

Blame named nobody. The live queue split the train or sent its lines back; Shunter asks the judge (J1) and charges nobody without evidence (I6). This costs a J1 wait each time, about six a day at the present failure rate (the review's point on J1 latency).

## The six gate divergences: not traced

The shadow's ten-minute runs found one waiting line each, in six runs over 24 hours, where Shunter's gate and the live queue's disagreed on whether the line could ride. The latest runs agree. Only the summary row of each run is kept, not the run's report, so the lines and the reasons on each side are lost. They cannot be traced from what is kept.

What is needed: keep the report of every run that has a gate divergence, not only the latest. The report already names the line, the live queue's reason and Shunter's (marked `**`). Until a divergence is traced, the gate stays an open item for B1.

## What remains for B1

- The gate divergences: keep the report of each diverging run, then trace the next ones.
- The `ErrJoin` refusals: confirm the rule from the labelled report.
- Rerun the daily report with the fixed binary. It should show no landed-without-verdict, no `BATCH-DRAFT`, and no unfixed-offer refusals for these trains.
