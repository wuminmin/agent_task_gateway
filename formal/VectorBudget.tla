---------------------------- MODULE VectorBudget ----------------------------
EXTENDS Naturals, FiniteSets

(***************************************************************************
This is a split finite model for TaskGate's vector budget ledger.  It avoids
the larger task/SQL/receipt state space in TaskGate.tla and focuses on the
per-dimension reserve/settle/release/indeterminate accounting rule.
***************************************************************************)

CONSTANTS Dimensions, Requests, MaxBudget

ASSUME /\ Dimensions # {}
       /\ Requests # {}
       /\ MaxBudget \in Nat \ {0}

RequestStates == {"NEW", "RESERVED", "COMPLETED", "RELEASED", "FAILED", "INDETERMINATE"}
TerminalStates == {"COMPLETED", "RELEASED", "FAILED", "INDETERMINATE"}
TaskStates == {"ACTIVE", "ARCHIVED"}

BudgetVector == [Dimensions -> 0..MaxBudget]
Zero == [d \in Dimensions |-> 0]
Limit == [d \in Dimensions |-> MaxBudget]

VecAdd(a, b) == [d \in Dimensions |-> a[d] + b[d]]
VecSub(a, b) == [d \in Dimensions |-> a[d] - b[d]]
VecLeq(a, b) == \A d \in Dimensions : a[d] <= b[d]
Positive(v) == \E d \in Dimensions : v[d] > 0

ReserveOptions == {v \in BudgetVector : Positive(v)}

VARIABLES taskState, used, reserved, requestState, reservation, charged

vars == <<taskState, used, reserved, requestState, reservation, charged>>

NoReserved == reserved = Zero
HardLimitReached == \E d \in Dimensions : used[d] = Limit[d]

Init ==
    /\ taskState = "ACTIVE"
    /\ used = Zero
    /\ reserved = Zero
    /\ requestState = [request \in Requests |-> "NEW"]
    /\ reservation = [request \in Requests |-> Zero]
    /\ charged = [request \in Requests |-> Zero]

Reserve(request) ==
    \E want \in ReserveOptions :
        /\ taskState = "ACTIVE"
        /\ ~HardLimitReached
        /\ requestState[request] = "NEW"
        /\ VecLeq(VecAdd(VecAdd(used, reserved), want), Limit)
        /\ requestState' = [requestState EXCEPT ![request] = "RESERVED"]
        /\ reservation' = [reservation EXCEPT ![request] = want]
        /\ charged' = [charged EXCEPT ![request] = Zero]
        /\ reserved' = VecAdd(reserved, want)
        /\ UNCHANGED <<taskState, used>>

Complete(request) ==
    \E actual \in BudgetVector :
        /\ requestState[request] = "RESERVED"
        /\ VecLeq(actual, reservation[request])
        /\ requestState' = [requestState EXCEPT ![request] = "COMPLETED"]
        /\ charged' = [charged EXCEPT ![request] = actual]
        /\ used' = VecAdd(used, actual)
        /\ reserved' = VecSub(reserved, reservation[request])
        /\ UNCHANGED <<taskState, reservation>>

FailPostExecution(request) ==
    \E actual \in BudgetVector :
        /\ requestState[request] = "RESERVED"
        /\ VecLeq(actual, reservation[request])
        /\ requestState' = [requestState EXCEPT ![request] = "FAILED"]
        /\ charged' = [charged EXCEPT ![request] = actual]
        /\ used' = VecAdd(used, actual)
        /\ reserved' = VecSub(reserved, reservation[request])
        /\ UNCHANGED <<taskState, reservation>>

Release(request) ==
    /\ requestState[request] = "RESERVED"
    /\ requestState' = [requestState EXCEPT ![request] = "RELEASED"]
    /\ charged' = [charged EXCEPT ![request] = Zero]
    /\ reserved' = VecSub(reserved, reservation[request])
    /\ UNCHANGED <<taskState, used, reservation>>

MarkIndeterminate(request) ==
    /\ requestState[request] = "RESERVED"
    /\ requestState' = [requestState EXCEPT ![request] = "INDETERMINATE"]
    /\ charged' = [charged EXCEPT ![request] = reservation[request]]
    /\ used' = VecAdd(used, reservation[request])
    /\ reserved' = VecSub(reserved, reservation[request])
    /\ UNCHANGED <<taskState, reservation>>

ArchiveHardLimit ==
    /\ taskState = "ACTIVE"
    /\ HardLimitReached
    /\ NoReserved
    /\ taskState' = "ARCHIVED"
    /\ UNCHANGED <<used, reserved, requestState, reservation, charged>>

Next ==
    \/ \E request \in Requests :
        \/ Reserve(request)
        \/ Complete(request)
        \/ FailPostExecution(request)
        \/ Release(request)
        \/ MarkIndeterminate(request)
    \/ ArchiveHardLimit

Spec == Init /\ [][Next]_vars /\ WF_vars(ArchiveHardLimit)

TypeOK ==
    /\ taskState \in TaskStates
    /\ used \in BudgetVector
    /\ reserved \in BudgetVector
    /\ requestState \in [Requests -> RequestStates]
    /\ reservation \in [Requests -> BudgetVector]
    /\ charged \in [Requests -> BudgetVector]

VectorBudgetSafety ==
    VecLeq(VecAdd(used, reserved), Limit)

TerminalChargeBounded ==
    \A request \in Requests :
        requestState[request] \in TerminalStates => VecLeq(charged[request], reservation[request])

ReleaseChargesNothing ==
    \A request \in Requests :
        requestState[request] = "RELEASED" => charged[request] = Zero

IndeterminateChargesFullReservation ==
    \A request \in Requests :
        requestState[request] = "INDETERMINATE" => charged[request] = reservation[request]

NoReservationAfterArchive ==
    taskState = "ARCHIVED" =>
        /\ reserved = Zero
        /\ \A request \in Requests : requestState[request] # "RESERVED"

EventualArchiveAtHardLimit ==
    [](taskState = "ACTIVE" /\ HardLimitReached /\ NoReserved =>
        <> (taskState = "ARCHIVED"))

=============================================================================
