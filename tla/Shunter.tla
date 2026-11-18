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
(* What is left out, to keep the skeleton small: the gate's detail         *)
(* (offered, waiting-for-base, parked), assembly and pre-flight (a train   *)
(* is born built and in CI), re-offers, and the pause at J3.  A train's    *)
(* head is the train itself: trunk is the sequence of trains landed.       *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS
    Lines,      \* the lines offered
    MaxTrains,  \* trains are numbered 1..MaxTrains as they are assembled
    Depth,      \* trains that may ride at once; 1 is no speculation
    MaxFaults   \* the adversary's budget

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
    tstate,   \* train -> its state
    cargo,    \* train -> the lines it was assembled from
    asm,      \* train -> line -> the head versions it was assembled at
    checked,  \* train -> line -> the head versions the lander read (I9)
    base,     \* train -> the train whose head it was built on; 0 is trunk's start
    verdict,  \* train -> CI passed on exactly this train's head
    rerun,    \* train -> the judge has used its one rerun
    chain,    \* the live trains, in landing order
    trunk,    \* the trains landed, in order
    used,     \* trains assembled so far
    faults    \* faults spent

vars == <<line, ride, ver, tstate, cargo, asm, checked, base, verdict, rerun,
          chain, trunk, used, faults>>

Trains == 1..MaxTrains
Spent  == {"landed", "returned", "dropped"}
Live   == {"testing", "judging", "green", "landing"}
Ended  == {"landed", "failed", "void", "aborted"}

Tip        == IF trunk = <<>> THEN 0 ELSE trunk[Len(trunk)]
Riders(t)  == {l \in Lines : ride[l] = t}
Pos(t)     == CHOOSE i \in 1..Len(chain) : chain[i] = t
Behind(t)  == {chain[i] : i \in (Pos(t) + 1)..Len(chain)}
Landed     == {trunk[i] : i \in 1..Len(trunk)}

TypeOK ==
    /\ line \in [Lines -> {"waiting", "held", "riding"} \cup Spent]
    /\ ride \in [Lines -> 0..MaxTrains]
    /\ ver \in [Lines -> 0..MaxFaults]
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

\* The runner joins waiting lines into the next train, on the exact head of
\* the train ahead, or on trunk.
Assemble(S) ==
    /\ S # {} /\ \A l \in S : line[l] = "waiting"
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
    /\ UNCHANGED <<ver, checked, verdict, rerun, trunk>>

\* CI passes on the train's own head.
CIPass(t) ==
    /\ tstate[t] = "testing"
    /\ tstate' = [tstate EXCEPT ![t] = "green"]
    /\ verdict' = [verdict EXCEPT ![t] = TRUE]
    /\ NoFault
    /\ UNCHANGED <<line, ride, ver, cargo, asm, checked, base, rerun, chain, trunk, used>>

\* A fault: the builder reports green, and there is no pass on this head
\* (a pass for another attempt, or a line of test output that looks like a
\* report).  The train goes green all the same; I9 is what stops it.
FalseGreen(t) ==
    /\ tstate[t] = "testing"
    /\ Fault
    /\ tstate' = [tstate EXCEPT ![t] = "green"]
    /\ UNCHANGED <<line, ride, ver, cargo, asm, checked, base, verdict, rerun, chain, trunk, used>>

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
    /\ UNCHANGED <<ver, cargo, asm, base, verdict, rerun, trunk, used>>

\* The only action that moves trunk.
Push(t) ==
    /\ tstate[t] = "landing"
    /\ trunk' = Append(trunk, t)
    /\ tstate' = [tstate EXCEPT ![t] = "landed"]
    /\ line' = [l \in Lines |-> IF ride[l] = t THEN "landed" ELSE line[l]]
    /\ ride' = [l \in Lines |-> IF ride[l] = t THEN 0 ELSE ride[l]]
    /\ chain' = Tail(chain)
    /\ NoFault
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, used>>

\* A fault: the forge refuses the fast-forward for good.  No fallback merge.
PushRefused(t) ==
    /\ tstate[t] = "landing"
    /\ Fault
    /\ End(t, "void", Requeue)
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used>>

\* A fault: the run was cancelled, skipped or unreadable.  Nothing is judged.
NoVerdict(t) ==
    /\ tstate[t] = "testing"
    /\ Fault
    /\ End(t, "void", Requeue)
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used>>

\* A deterministic failure, and blame names some lines and not all (or the
\* only one): they return, charged; the rest go again.
FailBlamed(t, B) ==
    /\ tstate[t] = "testing"
    /\ B # {} /\ B \subseteq Riders(t)
    /\ B # Riders(t) \/ Cardinality(Riders(t)) = 1
    /\ End(t, "failed", [l \in Lines |-> IF l \in B THEN "returned" ELSE "waiting"])
    /\ NoFault
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used>>

\* A fault: a failure blame cannot attribute.  The train waits at J1.
FailUnblamed(t) ==
    /\ tstate[t] = "testing"
    /\ Fault
    /\ tstate' = [tstate EXCEPT ![t] = "judging"]
    /\ UNCHANGED <<line, ride, ver, cargo, asm, checked, base, verdict, rerun, chain, trunk, used>>

\* J1's menu.  Charge, with evidence (not modelled), any lines the judge names.
JudgeCharge(t, B) ==
    /\ tstate[t] = "judging"
    /\ B # {} /\ B \subseteq Riders(t)
    /\ End(t, "failed", [l \in Lines |-> IF l \in B THEN "returned" ELSE "waiting"])
    /\ NoFault
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used>>

\* Split or park: either way every line goes back uncharged.  This is also
\* J1's default when the judge is silent.
JudgeRequeue(t) ==
    /\ tstate[t] = "judging"
    /\ End(t, "failed", Requeue)
    /\ NoFault
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used>>

\* Rerun, once.
JudgeRerun(t) ==
    /\ tstate[t] = "judging"
    /\ ~rerun[t]
    /\ rerun' = [rerun EXCEPT ![t] = TRUE]
    /\ tstate' = [tstate EXCEPT ![t] = "testing"]
    /\ NoFault
    /\ UNCHANGED <<line, ride, ver, cargo, asm, checked, base, verdict, chain, trunk, used>>

\* A fault: a producer pushes to a change.  If it rides, its train is void
\* (I4), unless the push is already in flight: then it lands as tested.
HeadMove(l) ==
    /\ line[l] \in {"waiting", "held", "riding"}
    /\ Fault
    /\ ver' = [ver EXCEPT ![l] = @ + 1]
    /\ IF line[l] = "riding" /\ tstate[ride[l]] # "landing"
       THEN End(ride[l], "void", Requeue)
       ELSE UNCHANGED <<line, ride, tstate, chain>>
    /\ UNCHANGED <<cargo, asm, checked, base, verdict, rerun, trunk, used>>

\* A fault, for liveness only: the judge holds a line.  A hold on a riding
\* line voids its train; a hold cannot reach a train whose push is in flight.
Hold(l) ==
    /\ Fault
    /\ \/ /\ line[l] = "waiting"
          /\ line' = [line EXCEPT ![l] = "held"]
          /\ UNCHANGED <<ride, tstate, chain>>
       \/ /\ line[l] = "riding" /\ tstate[ride[l]] # "landing"
          /\ End(ride[l], "void", [k \in Lines |-> IF k = l THEN "held" ELSE "waiting"])
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used>>

\* The only way out of a hold.
Release(l) ==
    /\ line[l] = "held"
    /\ line' = [line EXCEPT ![l] = "waiting"]
    /\ NoFault
    /\ UNCHANGED <<ride, ver, tstate, cargo, asm, checked, base, verdict, rerun, chain, trunk, used>>

\* A fault: the judge aborts a train at any stage before its push, and says
\* which lines return; the rest go back to waiting.
Abort(t, R) ==
    /\ tstate[t] \in {"testing", "judging", "green"}
    /\ R \subseteq Riders(t)
    /\ Fault
    /\ End(t, "aborted", [l \in Lines |-> IF l \in R THEN "returned" ELSE "waiting"])
    /\ UNCHANGED <<ver, cargo, asm, checked, base, verdict, rerun, trunk, used>>

\* An owner takes a waiting line back.
Withdraw(l) ==
    /\ line[l] = "waiting"
    /\ line' = [line EXCEPT ![l] = "dropped"]
    /\ NoFault
    /\ UNCHANGED <<ride, ver, tstate, cargo, asm, checked, base, verdict, rerun, chain, trunk, used>>

Next ==
    \/ \E S \in SUBSET Lines : Assemble(S)
    \/ \E t \in Trains :
        \/ CIPass(t) \/ FalseGreen(t) \/ Verify(t) \/ Push(t) \/ PushRefused(t)
        \/ NoVerdict(t) \/ FailUnblamed(t) \/ JudgeRequeue(t) \/ JudgeRerun(t)
        \/ \E B \in SUBSET Lines : FailBlamed(t, B) \/ JudgeCharge(t, B) \/ Abort(t, B)
    \/ \E l \in Lines : HeadMove(l) \/ Hold(l) \/ Release(l) \/ Withdraw(l)

(***************************************************************************)
(* Fairness, which is what liveness assumes of the world:                  *)
(*   - the runner assembles, checks and pushes when it can;                *)
(*   - CI eventually answers a run it is left to finish;                   *)
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
    /\ \A l \in Lines : WF_vars(Release(l))

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

\* A spent line stays spent (the model has no re-offer).
SpentStaysSpent ==
    [][\A l \in Lines : line[l] \in Spent => line'[l] = line[l]]_vars

----------------------------------------------------------------------------
(* Liveness. *)

\* L1: every line is eventually landed, returned or explicitly dropped.
EveryLineIsSpent == \A l \in Lines : <>(line[l] \in Spent)

=============================================================================
