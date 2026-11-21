------------------------------ MODULE Shunter ------------------------------
(***************************************************************************)
(* A skeleton of the state machine of PROTOCOL.md §5.1.                    *)
(*                                                                         *)
(* What is modelled: lines that wait, are held, ride, and are spent;       *)
(* trains that test, wait at J1, go green, land, fail, go void or are      *)
(* aborted; a speculation chain of depth Depth; the lander's check (I9);   *)
(* and an adversary with a budget of faults: CI that reports no verdict or *)
(* a pass that is not one, a forge that refuses the push, producers that   *)
(* push to riding changes, and a judge who holds and aborts at will.       *)
(*                                                                         *)
(* The judge is a nondeterministic choice within its menu.  Safety is      *)
(* checked for every choice; only liveness asks the judge for anything,    *)
(* and then only for its defaults: a deadline at J1 and a release for      *)
(* every hold.                                                             *)
(*                                                                         *)
(* Needs first (I3) is modelled: Needs says which line is stacked on which, *)
(* a line is assembled only behind its base, and a line whose base is      *)
(* returned or dropped goes back too.                                      *)
(*                                                                         *)
(* A train is judged only at the head of the chain (§5.3): a failure       *)
(* reported behind an undecided train is kept ("red") and judged when      *)
(* every train ahead has landed, or dropped when one does not.             *)
(*                                                                         *)
(* The owner-state of §7 is modelled by its rule on heads: a charged line   *)
(* may be offered again, and then only at a head other than the one it was *)
(* charged on.  A push is needed; who pushes and who offers are not        *)
(* modelled.                                                               *)
(*                                                                         *)
(* The line check of §5.4 is modelled: a waiting line is checked alone,    *)
(* its green verdict belongs to the version of its head it was taken on,   *)
(* and no line is assembled without one (I10).                             *)
(*                                                                         *)
(* What is left out, to keep the skeleton small: the gate's detail         *)
(* (offered, waiting-for-base, parked), assembly and pre-flight (a train   *)
(* is born built and in CI), the pause at J3, and the age bound *)
(* on a green line check (trunk moving does not end one here).  A train's  *)
(* head is the train itself: trunk is the sequence of trains landed.       *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS
    Lines,      \* the lines offered
    Needs,      \* pairs <<l, b>>: line l is stacked on line b, its base (I3)
    MaxTrains,  \* trains are numbered 1..MaxTrains as they are assembled
    Depth,      \* trains that may ride at once; 1 is no speculation
    MaxFaults   \* the adversary's budget

ASSUME Needs \subseteq Lines \X Lines
\* A line has at most one base, and is not its own.
ASSUME \A p, q \in Needs : p[1] = q[1] => p = q
ASSUME \A p \in Needs : p[1] # p[2]
ASSUME Depth \in Nat \ {0}
ASSUME MaxFaults \in Nat
\* Enough trains that the runner never runs out: each landing and each
\* charge spends a line, each other ending of a train spends a fault, and
\* an ending takes at most Depth trains with it.  Liveness needs this.
ASSUME MaxTrains >= Depth * (Cardinality(Lines) + MaxFaults)

VARIABLES
    line,     \* line -> its state
    ride,     \* line -> the train it rides, or 0
    ver,      \* line -> the version of its head; a push to it adds one
    green,    \* line -> 0, or 1 + the version its line check passed on
    charged,  \* line -> 0, or 1 + the version it was last charged at
    checking, \* line -> 0, or 1 + the version a line check in flight was started on
    tstate,   \* train -> its state
    cargo,    \* train -> the lines it was assembled from
    asm,      \* train -> line -> the head versions it was assembled at
    checked,     \* train -> line -> the head versions the lander read (I9)
    base,     \* train -> the train whose head it was built on; 0 is trunk's start
    verdict,  \* train -> CI passed on exactly this train's head
    rerun,    \* train -> the judge has used its one rerun
    chain,    \* the live trains, in landing order
    trunk,    \* the trains landed, in order
    used,     \* trains assembled so far
    faults    \* faults spent

vars == <<line, ride, ver, green, charged, checking, tstate, cargo, asm, checked, base, verdict, rerun,
          chain, trunk, used, faults>>

Trains == 1..MaxTrains
Spent  == {"landed", "returned", "dropped"}
Live   == {"testing", "red", "judging", "green", "landing"}
Ended  == {"landed", "failed", "void", "aborted"}

Tip        == IF trunk = <<>> THEN 0 ELSE trunk[Len(trunk)]
Riders(t)  == {l \in Lines : ride[l] = t}
Pos(t)     == CHOOSE i \in 1..Len(chain) : chain[i] = t
Behind(t)  == {chain[i] : i \in (Pos(t) + 1)..Len(chain)}
Landed     == {trunk[i] : i \in 1..Len(trunk)}
BaseOf(l)  == {p[2] : p \in {q \in Needs : q[1] = l}}
First(t)   == chain # <<>> /\ chain[1] = t

TypeOK ==
    /\ line \in [Lines -> {"waiting", "held", "riding"} \cup Spent]
    /\ ride \in [Lines -> 0..MaxTrains]
    /\ ver \in [Lines -> 0..MaxFaults]
    /\ green \in [Lines -> 0..(MaxFaults + 1)]
    /\ charged \in [Lines -> 0..(MaxFaults + 1)]
    /\ checking \in [Lines -> 0..(MaxFaults + 1)]
    /\ tstate \in [Trains -> {"unused"} \cup Live \cup Ended]
    /\ cargo \in [Trains -> SUBSET Lines]
    /\ asm \in [Trains -> [Lines -> 0..MaxFaults]]
    /\ checked \in [Trains -> [Lines -> 0..MaxFaults]]
    /\ base \in [Trains -> 0..MaxTrains]
    /\ verdict \in [Trains -> BOOLEAN]
    /\ rerun \in [Trains -> BOOLEAN]
    /\ chain \in Seq(Trains) /\ Len(chain) <= Depth
    /\ trunk \in Seq(Trains)
    /\ used \in 0..MaxTrains
    /\ faults \in 0..MaxFaults

Init ==
    /\ line = [l \in Lines |-> "waiting"]
    /\ ride = [l \in Lines |-> 0]
    /\ ver = [l \in Lines |-> 0]
    /\ green = [l \in Lines |-> 0]
    /\ charged = [l \in Lines |-> 0]
    /\ checking = [l \in Lines |-> 0]
    /\ tstate = [t \in Trains |-> "unused"]
    /\ cargo = [t \in Trains |-> {}]
    /\ asm = [t \in Trains |-> [l \in Lines |-> 0]]
    /\ checked = [t \in Trains |-> [l \in Lines |-> 0]]
    /\ base = [t \in Trains |-> 0]
    /\ verdict = [t \in Trains |-> FALSE]
    /\ rerun = [t \in Trains |-> FALSE]
    /\ chain = <<>>
    /\ trunk = <<>>
    /\ used = 0
    /\ faults = 0

Fault == faults < MaxFaults /\ faults' = faults + 1
NoFault == UNCHANGED faults

(***************************************************************************)
(* End: train t ends without landing, in state to.  fate says what becomes *)
(* of each of its riders.  Every train behind it in the chain is void, and *)
(* its riders go back to waiting: the head they were built on will never   *)
(* exist (I5).                                                             *)
(***************************************************************************)
End(t, to, fate) ==
    /\ tstate' = [u \in Trains |-> IF u = t THEN to
                                   ELSE IF u \in Behind(t) THEN "void"
                                   ELSE tstate[u]]
    /\ line' = [l \in Lines |-> IF ride[l] = t THEN fate[l]
                                ELSE IF ride[l] \in Behind(t) THEN "waiting"
                                ELSE line[l]]
    /\ ride' = [l \in Lines |-> IF ride[l] = t \/ ride[l] \in Behind(t)
                                THEN 0 ELSE ride[l]]
    /\ chain' = SubSeq(chain, 1, Pos(t) - 1)

Requeue == [l \in Lines |-> "waiting"]

\* I10's test: the line holds a green line check on the head it has now.
Checked(l) == green[l] = ver[l] + 1

\* The line check (§5.4): a waiting line, alone, merged with trunk.  A check
\* takes time: it starts on the head the line has then, and its verdict
\* arrives later, when the head may have moved.
LineCheckStart(l) ==
    /\ line[l] = "waiting" /\ ~Checked(l) /\ checking[l] = 0
    \* a stacked line is checked with its base beneath it, after its base
    /\ \A b \in BaseOf(l) : Checked(b) \/ line[b] \in {"riding", "landed"}
    /\ checking' = [checking EXCEPT ![l] = ver[l] + 1]
    /\ NoFault
    /\ UNCHANGED <<line, ride, ver, tstate, cargo, asm, checked, base, verdict, rerun, chain, trunk, used, green, charged>>

\* The verdict is for the head the line still has.  A pass is kept against
\* the version checked.
LineCheckPass(l) ==
    /\ line[l] = "waiting" /\ checking[l] = ver[l] + 1
    /\ green' = [green EXCEPT ![l] = checking[l]]
    /\ checking' = [checking EXCEPT ![l] = 0]
    /\ NoFault
    /\ UNCHANGED <<line, ride, ver, tstate, cargo, asm, checked, base, verdict, rerun, chain, trunk, used, charged>>

\* A failure charges the line (L33), at the head that was judged; its
\* evidence, the same check passing on trunk alone, is not modelled.  It is
\* the line's own fault, so no fault of the adversary's is spent.
LineCheckFail(l) ==
    /\ line[l] = "waiting" /\ checking[l] = ver[l] + 1
    /\ line' = [line EXCEPT ![l] = "returned"]
    /\ charged' = [charged EXCEPT ![l] = checking[l]]
    /\ checking' = [checking EXCEPT ![l] = 0]
    /\ NoFault
    /\ UNCHANGED <<ride, ver, green, tstate, cargo, asm, checked, base, verdict, rerun, chain, trunk, used>>

\* The verdict is for a head the line no longer has, or the line no longer
\* waits: it is nobody's.  Nothing is kept and nobody is charged; a waiting
\* line is checked again.
LineCheckDropped(l) ==
    /\ checking[l] # 0
    /\ checking[l] # ver[l] + 1 \/ line[l] # "waiting"
    /\ checking' = [checking EXCEPT ![l] = 0]
    /\ NoFault
    /\ UNCHANGED <<line, ride, ver, green, charged, tstate, cargo, asm, checked, base, verdict, rerun, chain, trunk, used>>

\* The runner joins waiting lines into the next train, on the exact head of
\* the train ahead, or on trunk.
Assemble(S) ==
    /\ S # {} /\ \A l \in S : line[l] = "waiting" /\ Checked(l)
    \* I3: a line rides only behind its base: landed, riding already (so in
    \* an earlier train), or taken into this train with it
    /\ \A l \in S : \A b \in BaseOf(l) : line[b] \in {"landed", "riding"} \/ b \in S
    /\ Len(chain) < Depth
    /\ used < MaxTrains
    /\ LET t == used + 1 IN
        /\ used' = t
        /\ tstate' = [tstate EXCEPT ![t] = "testing"]
        /\ cargo' = [cargo EXCEPT ![t] = S]
        /\ asm' = [asm EXCEPT ![t] = ver]
        /\ base' = [base EXCEPT ![t] = IF chain = <<>> THEN Tip
                                       ELSE chain[Len(chain)]]
        /\ line' = [l \in Lines |-> IF l \in S THEN "riding" ELSE line[l]]
        /\ ride' = [l \in Lines |-> IF l \in S THEN t ELSE ride[l]]
        /\ chain' = Append(chain, t)
    /\ NoFault
    /\ UNCHANGED <<ver, checked, verdict, rerun, trunk, green, charged, checking>>

\* CI passes on the train's own head.
CIPass(t) ==
    /\ tstate[t] = "testing"
    /\ tstate' = [tstate EXCEPT ![t] = "green"]
    /\ verdict' = [verdict EXCEPT ![t] = TRUE]
    /\ NoFault
    /\ UNCHANGED <<line, ride, ver, cargo, asm, checked, base, rerun, chain, trunk, used, green, charged, checking>>

\* A fault: the builder reports green, and there is no pass on this head
\* (a pass for another attempt, or a line of test output that looks like a
\* report).  The train goes green all the same; I9 is what stops it.
FalseGreen(t) ==
    /\ tstate[t] = "testing"
    /\ Fault
    /\ tstate' = [tstate EXCEPT ![t] = "green"]
    /\ UNCHANGED <<line, ride, ver, cargo, asm, checked, base, verdict, rerun, chain, trunk, used, green, charged, checking>>

\* I9: the lander reads the verdict, the heads and trunk itself, when the
\* train is first in the chain.  Any mismatch voids the train.
Verify(t) ==
    /\ tstate[t] = "green"
    /\ chain # <<>> /\ chain[1] = t
    /\ NoFault
    /\ IF /\ verdict[t]
          /\ \A l \in Riders(t) : ver[l] = asm[t][l]
          /\ base[t] = Tip
       THEN /\ tstate' = [tstate EXCEPT ![t] = "landing"]
            /\ checked' = [checked EXCEPT ![t] = ver]
            /\ UNCHANGED <<line, ride, chain>>
       ELSE /\ End(t, "void", Requeue)
            /\ UNCHANGED checked
    /\ UNCHANGED <<ver, cargo, asm, base, verdict, rerun, trunk, used, green, charged, checking>>

\* The only action that moves trunk.
Push(t) ==
    /\ tstate[t] = "landing"
    /\ trunk' = Append(trunk, t)
    /\ tstate' = [tstate EXCEPT ![t] = "landed"]
    /\ line' = [l \in Lines |-> IF ride[l] = t THEN "landed" ELSE line[l]]
    /\ ride' = [l \in Lines |-> IF ride[l] = t THEN 0 ELSE ride[l]]
    /\ chain' = Tail(chain)
    /\ NoFault
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, used, green, charged, checking>>

\* A fault: the forge refuses the fast-forward for good.  No fallback merge.
PushRefused(t) ==
    /\ tstate[t] = "landing"
    /\ Fault
    /\ End(t, "void", Requeue)
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used, green, charged, checking>>

\* A fault: the run was cancelled, skipped or unreadable.  Nothing is judged.
NoVerdict(t) ==
    /\ tstate[t] = "testing"
    /\ Fault
    /\ End(t, "void", Requeue)
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used, green, charged, checking>>

\* A failure is reported for a train that is not first.  The train ahead
\* is undecided, and the failure may be its own, inherited: nothing is
\* judged.  The report is kept.
FailBehind(t) ==
    /\ tstate[t] = "testing" /\ ~First(t)
    /\ tstate' = [tstate EXCEPT ![t] = "red"]
    /\ NoFault
    /\ UNCHANGED <<line, ride, ver, cargo, asm, checked, base, verdict, rerun, chain, trunk, used, green, charged, checking>>

\* A deterministic failure, and blame names some lines and not all (or the
\* only one): they return, charged; the rest go again.  Only the first
\* train is judged: one reporting now, or one whose report was kept.
FailBlamed(t, B) ==
    /\ tstate[t] \in {"testing", "red"} /\ First(t)
    /\ B # {} /\ B \subseteq Riders(t)
    /\ B # Riders(t) \/ Cardinality(Riders(t)) = 1
    /\ End(t, "failed", [l \in Lines |-> IF l \in B THEN "returned" ELSE "waiting"])
    /\ NoFault
    /\ charged' = [l \in Lines |-> IF l \in B THEN ver[l] + 1 ELSE charged[l]]
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used, green, checking>>

\* A fault: a failure blame cannot attribute.  The train waits at J1.
FailUnblamed(t) ==
    /\ tstate[t] \in {"testing", "red"} /\ First(t)
    /\ Fault
    /\ tstate' = [tstate EXCEPT ![t] = "judging"]
    /\ UNCHANGED <<line, ride, ver, cargo, asm, checked, base, verdict, rerun, chain, trunk, used, green, charged, checking>>

\* J1's menu.  Charge, with evidence (not modelled), any lines the judge names.
JudgeCharge(t, B) ==
    /\ tstate[t] = "judging"
    /\ B # {} /\ B \subseteq Riders(t)
    /\ End(t, "failed", [l \in Lines |-> IF l \in B THEN "returned" ELSE "waiting"])
    /\ NoFault
    /\ charged' = [l \in Lines |-> IF l \in B THEN ver[l] + 1 ELSE charged[l]]
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used, green, checking>>

\* Split or park: either way every line goes back uncharged.  This is also
\* J1's default when the judge is silent.
JudgeRequeue(t) ==
    /\ tstate[t] = "judging"
    /\ End(t, "failed", Requeue)
    /\ NoFault
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used, green, charged, checking>>

\* Rerun, once.
JudgeRerun(t) ==
    /\ tstate[t] = "judging"
    /\ ~rerun[t]
    /\ rerun' = [rerun EXCEPT ![t] = TRUE]
    /\ tstate' = [tstate EXCEPT ![t] = "testing"]
    /\ NoFault
    /\ UNCHANGED <<line, ride, ver, cargo, asm, checked, base, verdict, chain, trunk, used, green, charged, checking>>

\* A fault: a producer pushes to a change (a returned line's owner, too: a
\* fix is a push).  If it rides, its train is void
\* (I4), unless the push is already in flight: then it lands as tested.
HeadMove(l) ==
    /\ line[l] \in {"waiting", "held", "riding", "returned"}
    /\ Fault
    /\ ver' = [ver EXCEPT ![l] = @ + 1]
    /\ IF line[l] = "riding" /\ tstate[ride[l]] # "landing"
       THEN End(ride[l], "void", Requeue)
       ELSE UNCHANGED <<line, ride, tstate, chain>>
    /\ UNCHANGED <<cargo, asm, checked, base, verdict, rerun, trunk, used, green, charged, checking>>

\* A fault, for liveness only: the judge holds a line.  A hold on a riding
\* line voids its train; a hold cannot reach a train whose push is in flight.
Hold(l) ==
    /\ Fault
    /\ \/ /\ line[l] = "waiting"
          /\ line' = [line EXCEPT ![l] = "held"]
          /\ UNCHANGED <<ride, tstate, chain>>
       \/ /\ line[l] = "riding" /\ tstate[ride[l]] # "landing"
          /\ End(ride[l], "void", [k \in Lines |-> IF k = l THEN "held" ELSE "waiting"])
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used, green, charged, checking>>

\* The only way out of a hold.
Release(l) ==
    /\ line[l] = "held"
    /\ line' = [line EXCEPT ![l] = "waiting"]
    /\ NoFault
    /\ UNCHANGED <<ride, ver, tstate, cargo, asm, checked, base, verdict, rerun, chain, trunk, used, green, charged, checking>>

\* A fault: the judge aborts a train at any stage before its push, and says
\* which lines return; the rest go back to waiting.
Abort(t, R) ==
    /\ tstate[t] \in {"testing", "red", "judging", "green"}
    /\ R \subseteq Riders(t)
    /\ Fault
    /\ End(t, "aborted", [l \in Lines |-> IF l \in R THEN "returned" ELSE "waiting"])
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used, green, charged, checking>>

\* I3: a waiting line whose base was returned or dropped goes back too,
\* uncharged: what it was built on will not land as it is.
BaseLost(l) ==
    /\ line[l] = "waiting"
    /\ \E b \in BaseOf(l) : line[b] \in {"returned", "dropped"}
    /\ line' = [line EXCEPT ![l] = "returned"]
    /\ NoFault
    /\ UNCHANGED <<ride, ver, tstate, cargo, asm, checked, base, verdict, rerun, chain, trunk, used, green, charged, checking>>

\* A fault, in that it spends the adversary's budget: a returned line is
\* offered again.  A charged line only at another head than the one it was
\* charged on: a charge is cleared by a fix, and a fix is a push (§7).
Reoffer(l) ==
    /\ line[l] = "returned"
    /\ charged[l] # ver[l] + 1
    /\ \A b \in BaseOf(l) : line[b] \notin {"returned", "dropped"}
    /\ Fault
    /\ line' = [line EXCEPT ![l] = "waiting"]
    /\ UNCHANGED <<ride, ver, tstate, cargo, asm, checked, base, verdict, rerun, chain, trunk, used, green, charged, checking>>

\* An owner takes a waiting line back.
Withdraw(l) ==
    /\ line[l] = "waiting"
    /\ line' = [line EXCEPT ![l] = "dropped"]
    /\ NoFault
    /\ UNCHANGED <<ride, ver, tstate, cargo, asm, checked, base, verdict, rerun, chain, trunk, used, green, charged, checking>>

Next ==
    \/ \E S \in SUBSET Lines : Assemble(S)
    \/ \E t \in Trains :
        \/ CIPass(t) \/ FalseGreen(t) \/ Verify(t) \/ Push(t) \/ PushRefused(t)
        \/ NoVerdict(t) \/ FailBehind(t) \/ FailUnblamed(t) \/ JudgeRequeue(t) \/ JudgeRerun(t)
        \/ \E B \in SUBSET Lines : FailBlamed(t, B) \/ JudgeCharge(t, B) \/ Abort(t, B)
    \/ \E l \in Lines : HeadMove(l) \/ Hold(l) \/ Release(l) \/ Withdraw(l)
    \/ \E l \in Lines : LineCheckStart(l) \/ LineCheckPass(l) \/ LineCheckFail(l) \/ LineCheckDropped(l)
    \/ \E l \in Lines : BaseLost(l) \/ Reoffer(l)

(***************************************************************************)
(* Fairness, which is what liveness assumes of the world:                  *)
(*   - the runner assembles, checks and pushes when it can;                *)
(*   - the line checker answers for every waiting line (a pass or a        *)
(*     failure: either way the line is not left unchecked for ever);       *)
(*   - CI eventually answers a run it is left to finish;                   *)
(*   - a kept failure is judged once its train is first;                   *)
(*   - a line whose base is lost is returned;                              *)
(*   - J1's deadline passes, and its default applies;                      *)
(*   - every hold is eventually released.                                  *)
(* Nothing is assumed of the adversary but its budget, MaxFaults.          *)
(***************************************************************************)
Fairness ==
    /\ WF_vars(\E S \in SUBSET Lines : Assemble(S))
    /\ \A t \in Trains :
        /\ WF_vars(CIPass(t))
        /\ WF_vars(Verify(t))
        /\ WF_vars(Push(t))
        /\ WF_vars(JudgeRequeue(t))
        /\ WF_vars(tstate[t] = "red" /\ \E B \in SUBSET Lines : FailBlamed(t, B))
    /\ \A l \in Lines : WF_vars(Release(l))
    /\ \A l \in Lines : WF_vars(LineCheckStart(l))
    /\ \A l \in Lines : WF_vars(LineCheckPass(l) \/ LineCheckFail(l))
    /\ \A l \in Lines : WF_vars(LineCheckDropped(l))
    /\ \A l \in Lines : WF_vars(BaseLost(l))

Spec == Init /\ [][Next]_vars /\ Fairness

----------------------------------------------------------------------------
(* Safety. *)

\* I8 in the small: a line rides exactly when one live train in the chain
\* carries it, and the chain is exactly the live trains.
OneRecord ==
    /\ \A l \in Lines :
        line[l] = "riding" <=> (ride[l] # 0 /\ tstate[ride[l]] \in Live)
    /\ \A t \in Trains :
        tstate[t] \in Live <=> (\E i \in 1..Len(chain) : chain[i] = t)
    /\ \A i, j \in 1..Len(chain) : chain[i] = chain[j] => i = j

\* I2: trunk only fast-forwards.  Its history only grows, one train a step.
FastForwardOnly ==
    [][trunk' = trunk \/ \E t \in Trains : trunk' = Append(trunk, t)]_vars

\* I1, I9: nothing lands without a verdict on exactly its head.
TestedTrunk == \A t \in Landed : verdict[t]

\* I5: each landed train was built on the exact head trunk had when it landed.
ExactSpeculation ==
    \A i \in 1..Len(trunk) :
        base[trunk[i]] = IF i = 1 THEN 0 ELSE trunk[i - 1]

\* I4: what lands is what was assembled; the lander saw every head unmoved.
Frozen ==
    \A t \in Trains :
        tstate[t] \in {"landing", "landed"} =>
            \A l \in cargo[t] : checked[t][l] = asm[t][l]

\* I3, needs first: a line rides only behind its base, and lands only
\* after it.
LandedIn(l) == CHOOSE t \in Landed : l \in cargo[t]
TrunkPos(t) == CHOOSE i \in 1..Len(trunk) : trunk[i] = t
NeedsFirst ==
    \A l \in Lines : \A b \in BaseOf(l) :
        /\ line[l] = "riding" =>
              \/ line[b] = "landed"
              \/ line[b] = "riding" /\ Pos(ride[b]) <= Pos(ride[l])
        /\ line[l] = "landed" =>
              line[b] = "landed" /\ TrunkPos(LandedIn(b)) <= TrunkPos(LandedIn(l))

\* §5.3: a train is judged only at the head of the chain.  It enters J1, or
\* ends as failed (a charge, a split, a park), only when it is first: its
\* base is then trunk, and its failure is its own.  A train that reports a
\* failure behind an undecided train charges nobody.
JudgedOnlyAtHead ==
    [][\A t \in Trains :
          tstate'[t] \in {"judging", "failed"} /\ tstate[t] \notin {"judging", "failed"}
              => First(t) /\ base[t] = Tip]_vars

\* I10: a line rides only on a green line check of the very head it was
\* assembled at.  A push to a riding line voids its train (HeadMove), and a
\* push that arrives while the train lands does not land with it (Frozen).
AssembledOnlyChecked ==
    \A l \in Lines : line[l] = "riding" => green[l] = asm[ride[l]][l] + 1

\* A push is never taken as a fix: a line's green is only ever set by a pass
\* of the check on the head it has at that moment.
GreenOnlyByACheck ==
    [][\A l \in Lines : green'[l] # green[l] =>
          line[l] = "waiting" /\ green'[l] = ver[l] + 1]_vars

\* A cancelled train never lands: an ended train stays as it ended, and no
\* aborted train is on trunk.
EndedStaysEnded ==
    [][\A t \in Trains : tstate[t] \in Ended => tstate'[t] = tstate[t]]_vars
CancelledNeverLands == \A t \in Trains : tstate[t] = "aborted" => t \notin Landed

\* A held line is never spent: it rides nothing, and its only way out is
\* back into the queue.
HeldNotRiding == \A l \in Lines : line[l] = "held" => ride[l] = 0
HeldNeverSpent ==
    [][\A l \in Lines : line[l] = "held" => line'[l] \in {"held", "waiting"}]_vars

\* A spent line stays spent, but for a returned line offered again.
SpentStaysSpent ==
    [][\A l \in Lines : line[l] \in Spent =>
          line'[l] = line[l] \/ (line[l] = "returned" /\ line'[l] = "waiting")]_vars

\* A charge is only ever on the head the line has at that moment: a verdict
\* on a head since replaced charges nobody (the line check's, which takes
\* time), and a train's charge is on the heads it was assembled at, which
\* cannot have moved (a push to a riding line voids its train).
ChargedOnlyAtPresentHead ==
    [][\A l \in Lines : charged'[l] # charged[l] => charged'[l] = ver[l] + 1]_vars

\* §7: a charged line never rides again at the head it was charged on.  A
\* push is needed before it is offered again; an offer is not a fix.
NoRideAtChargedHead ==
    \A l \in Lines : line[l] = "riding" => asm[ride[l]][l] + 1 # charged[l]

----------------------------------------------------------------------------
(* Liveness. *)

\* L1: every line is eventually landed, returned or explicitly dropped.
EveryLineIsSpent == \A l \in Lines : <>(line[l] \in Spent)

=============================================================================
