------------------------------- MODULE Stack -------------------------------
(***************************************************************************)
(* Shunter with a stack: line l2 is stacked on line l1 (needs first, I3).  *)
(* Stack.cfg checks it.  The constants l1 and l2 are the model values of   *)
(* Lines, named here so that Needs can be written.                         *)
(***************************************************************************)
EXTENDS Shunter

CONSTANTS l1, l2

StackNeeds == {<<l2, l1>>}

=============================================================================
