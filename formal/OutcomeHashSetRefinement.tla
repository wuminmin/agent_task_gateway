--------------------- MODULE OutcomeHashSetRefinement ---------------------
EXTENDS Naturals, FiniteSets

(***************************************************************************)
(* Small refinement model for the V5 content-addressed outcome set.       *)
(* Physical manifests abstract to Decode[root]; settlement must implement  *)
(* exact mathematical difference/union and retry from the winning root.    *)
(***************************************************************************)

CONSTANTS Facts, CandidateA, CandidateB, MaxOutcome

ASSUME /\ Facts # {}
       /\ CandidateA \subseteq Facts
       /\ CandidateB \subseteq Facts
       /\ CandidateA # {}
       /\ CandidateB # {}
       /\ MaxOutcome \in Nat \ {0}

VARIABLES root, pending, priorRoot, charged, committed, collision, frozenRoot

vars == <<root, pending, priorRoot, charged, committed, collision, frozenRoot>>

Init ==
    /\ root = {}
    /\ pending = [a |-> CandidateA, b |-> CandidateB]
    /\ priorRoot = [a |-> {}, b |-> {}]
    /\ charged = [a |-> {}, b |-> {}]
    /\ committed = [a |-> FALSE, b |-> FALSE]
    /\ collision = FALSE
    /\ frozenRoot = {}

Settle(id) ==
    LET novel == pending[id] \ root IN
    /\ ~committed[id]
    /\ ~collision
    /\ Cardinality(root \cup novel) <= MaxOutcome
    /\ charged' = [charged EXCEPT ![id] = novel]
    /\ priorRoot' = [priorRoot EXCEPT ![id] = root]
    /\ root' = root \cup pending[id]
    /\ committed' = [committed EXCEPT ![id] = TRUE]
    /\ UNCHANGED <<pending, collision, frozenRoot>>

DetectCollision ==
    /\ ~collision
    /\ collision' = TRUE
    /\ frozenRoot' = root
    /\ UNCHANGED <<root, pending, priorRoot, charged, committed>>

Idle == UNCHANGED vars

Next == Settle("a") \/ Settle("b") \/ DetectCollision \/ Idle
Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ root \subseteq Facts
    /\ pending \in [{"a", "b"} -> SUBSET Facts]
    /\ priorRoot \in [{"a", "b"} -> SUBSET Facts]
    /\ charged \in [{"a", "b"} -> SUBSET Facts]
    /\ committed \in [{"a", "b"} -> BOOLEAN]
    /\ collision \in BOOLEAN
    /\ frozenRoot \subseteq Facts

ExactDifference ==
    \A id \in {"a", "b"} : committed[id] => charged[id] = pending[id] \ priorRoot[id]

NoDoubleCharge == charged["a"] \cap charged["b"] = {}
BudgetSafety == Cardinality(root) <= MaxOutcome
CollisionFailClosed == collision => root = frozenRoot

=============================================================================
