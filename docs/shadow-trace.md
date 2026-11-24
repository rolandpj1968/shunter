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
| gate divergences, one line a run | 8 | (c) 1, (b) 7 | see below |

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

## The gate divergences: eight runs, traced

The coordinator kept the full report of each run with a gate divergence: eight runs, one diverging line each (one line twice). Seven are one direction (the live queue takes the line, Shunter's gate does not) and one the other.

| the line, as the report says | runs | class | outcome |
|---|---|---|---|
| refused: "stacked on X, which has not landed and is in no live line", X **later in the same list** | 1 | (c) | fixed |
| refused: the same, X **not in the list**, riding in a train the live queue had in flight | 5 | (b) | fixed |
| refused: "not from an approved producer" | 2 (one line) | (b), site configuration | for the site |
| Shunter takes on a green the live queue no longer counts ("line check none") | 1 | (b), most likely | for the site, to confirm |

**A stacked line listed before its base: (c), fixed.** The live queue's list may name a stacked line before its base. `store.FromLines` set a line's base only from lines **earlier** in the file, so the line was offered with no base and the gate refused it as stacked on a change in no live line. This is the import the cut-over itself uses (`shunt import --line-files`), so on the day it would have refused such a line. Now every line of the files is read first; a base is found anywhere in them, and a base is offered before the lines stacked on it (file order otherwise). Test: `TestFromLinesStacksALineListedBeforeItsBase`.

**A base riding in a train in flight: (b), fixed.** The shadow's "now" starts from an empty state holding only the live queue's lists. A line whose base was riding in a train the live queue had open is in no list, so the gate refused the stacked line; in Shunter its base would be a live line riding, and the stacked line would wait behind it (I3). The shadow now puts the history's trains still in flight (a pull request open and no end, a green not yet landed, or merged and not yet opened) in the state as built trains before the gate runs, and a waiting line stacked on one of their lines is offered on it. Those trains count against the depth, as they do in the live queue. Test: `TestShadowGatesALineStackedOnARidingBase`.

**Not from an approved producer: (b), site configuration.** The forge adapter's approval (`approved_file`, `approved_label`, `[repo.author_branches]`) did not include the change's author, whom the live queue's own list approves. The shadow's configuration must carry the same list as the live gate; then the runs agree. Nothing in Shunter changes.

**A green the live queue no longer counts: (b), most likely.** The line's only verdict in the facts is a pass on its present head, and Shunter takes it (I10: a green is current while the line's heads are unchanged). The live queue answered "none". Its verdicts are keyed by more than the heads (by the checker's own version, and by the heads of the open changes beneath the line: the review's (b) 7), and its checker changed that morning, so its older verdicts stopped counting. The check facts the shadow is given carry no checker version, so it can't void them as I14 would. The fix is in the facts: give only the verdicts of the live checker's present version, or give the version with each and declare it (`checker`). To confirm from the live record of that line.

## What remains for B1

- The gate divergences: set the shadow's approved list and the check facts' checker version as the live gate has them, then watch the next runs.
- The `ErrJoin` refusals: confirm the rule from the labelled report.
- Rerun the daily report with the fixed binary. It should show no landed-without-verdict, no `BATCH-DRAFT`, and no unfixed-offer refusals for these trains.
