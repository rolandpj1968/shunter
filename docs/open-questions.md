# Open questions from the daemon

*Ambiguities and gaps in [`PROTOCOL.md`](../PROTOCOL.md) that writing the store and the daemon brought out. Each says what the code does today, so that nothing was decided silently. None of them is settled; each is for the protocol to answer.*

## The state machine

1. **A green train whose landing check can't be read.** T12 and T23 cover a reading that matches and one that doesn't. They don't cover no reading at all, when the forge keeps failing. **Today:** the daemon retries with backoff, escalates to J3 after its budget, and keeps retrying. The train stays green and holds the chain. Should T23 also void it at a deadline, as no verdict?

2. **A push whose deadline passes with its outcome unknown.** §5.1 says landing "is bounded by the push's deadline", but not what happens at that deadline. A push that timed out may have taken. **Today:** it is a transient refusal (T13's default). The next push finds trunk already at the head, the git adapter counts that as success, and the train lands. The protocol could state that a push is idempotent, and that a lander resolves an unknown outcome by reading trunk.

3. **A second flake.** §5.5 allows a known flake or a timing failure one rerun. It doesn't say what follows when the rerun fails the same way. **Today:** the daemon sends it to J1 as a failure that blame can't attribute.

4. **Hold from offered.** No row holds an offered line (L8–L10 start from waiting, waiting-for-base and parked). A cut-over import must carry holds, so it runs the gate on each line at once and holds the line after the gate accepts it. Should L8 also start from offered?

5. **A head that moved before the gate.** §5.2 doesn't say whether a change pushed between its offer and the gate is refused or taken at its new head. **Today:** the gate takes it, with a head-moved event before the acceptance.

## Judgement points and deadlines

6. **Deadlines with no number.** The protocol gives J1 10 minutes, J2 15 minutes and J5 one day. T2 ("the builder's deadline"), T6 (the pull request), T7 ("no verdict at the deadline") and the push have none. **Today's defaults:** builder 30 minutes, pull request 10 minutes, CI 90 minutes, push 5 minutes. All are configurable.

7. **Does a rerun restart the CI deadline?** A rerun ordered at J1 (T11) re-enters testing, so its clock restarts. A rerun after a flake stays in testing, so its clock doesn't restart. Should it?

8. **"The same test failed in the previous batch, with unrelated changes"** (J1's default). The machine records a failure's evidence as one string. **Today:** "the same test" means the same evidence string. "The previous batch" means the failure recorded just before, kept in the store. "Unrelated" means no line in common. Real blame adapters will need a structured identity for a failing test.

9. **Parks whose condition can't be tested.** §6.4 says such a park "carries a deadline", but names none. J1's default park has the condition "deadline: the judge reviews", with no time. **Today:** a condition `landed:<line>` is tested, and every other park is restored after 24 hours, with an alert. Should the protocol require every park to state its deadline as a time?

10. **J2 has no trigger in the machine.** "Two consecutive batches with unrelated changes failing on the same test, or the nightly run failing" isn't an event yet. J1's default park covers the first half in practice. The second half needs a trunk-health watcher (L3) to post an event.

## The CLI

11. **"cancel --action drop".** In the protocol, "dropped" means withdrawn by the owner (L29–L33). An abort can only requeue, park or return a line (§5.9). **Today:** `drop` returns the line to its owner with the reason, which matches what the old queue meant by it. The name may mislead.

12. **What an abort's park restores on.** An abort that parks needs a restore condition (L24). The CLI gives one: a deadline of the default park lifetime, "or a restore by hand". The CLI has no `restore` verb yet. Should it?

## Restarts and versions

13. **A log written by an older version.** At start, the store replays the log after its last snapshot through the current `Next`. If the current version refuses an event the old one accepted, or decides different effects for it, the store refuses to start, with an error naming the cause. A clean stop always writes a snapshot, so an upgrade after a clean stop replays nothing. §7 says a new version is "checked before it's started". Is refusing to start the right reading of that, or should the snapshot win?

14. **Watching the daemon itself (L3).** The daemon records alerts in the store and sends them to the judge. Nothing yet watches the daemon's own loop from outside: its tick, its poll errors, its fatal stop. That belongs with the real adapters and shadow running.
