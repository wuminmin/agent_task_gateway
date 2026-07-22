-------------------------- MODULE RecoveryLiveness --------------------------
EXTENDS Naturals, FiniteSets

(***************************************************************************
Split finite model for conservative recovery.  A request that reaches the
durable recovery state is eventually settled under weak fairness: known
results complete with bounded actual charge, while unknown durable reservations
become indeterminate and charge the full reservation.
***************************************************************************)

CONSTANTS Requests, MaxBudget

ASSUME /\ Requests # {}
       /\ MaxBudget \in Nat \ {0}

RequestStates == {"NEW", "RESERVED", "RESULT_KNOWN", "RECOVERING",
                  "COMPLETED", "RELEASED", "INDETERMINATE"}
TerminalStates == {"COMPLETED", "RELEASED", "INDETERMINATE"}

VARIABLES requestState,
          durableResult,
          reservation,
          charged,
          used,
          reserved

vars == <<requestState, durableResult, reservation, charged, used, reserved>>

Init ==
    /\ requestState = [request \in Requests |-> "NEW"]
    /\ durableResult = [request \in Requests |-> FALSE]
    /\ reservation = [request \in Requests |-> 0]
    /\ charged = [request \in Requests |-> 0]
    /\ used = 0
    /\ reserved = 0

Reserve(request) ==
    \E amount \in 1..MaxBudget :
        /\ requestState[request] = "NEW"
        /\ used + reserved + amount <= MaxBudget
        /\ requestState' = [requestState EXCEPT ![request] = "RESERVED"]
        /\ reservation' = [reservation EXCEPT ![request] = amount]
        /\ charged' = [charged EXCEPT ![request] = 0]
        /\ reserved' = reserved + amount
        /\ UNCHANGED <<durableResult, used>>

ConnectorResult(request) ==
    /\ requestState[request] = "RESERVED"
    /\ requestState' = [requestState EXCEPT ![request] = "RESULT_KNOWN"]
    /\ durableResult' = [durableResult EXCEPT ![request] = TRUE]
    /\ UNCHANGED <<reservation, charged, used, reserved>>

CompleteKnown(request) ==
    \E actual \in 1..reservation[request] :
        /\ requestState[request] = "RESULT_KNOWN"
        /\ requestState' = [requestState EXCEPT ![request] = "COMPLETED"]
        /\ charged' = [charged EXCEPT ![request] = actual]
        /\ used' = used + actual
        /\ reserved' = reserved - reservation[request]
        /\ UNCHANGED <<durableResult, reservation>>

ReleaseBeforeConnector(request) ==
    /\ requestState[request] = "RESERVED"
    /\ ~durableResult[request]
    /\ requestState' = [requestState EXCEPT ![request] = "RELEASED"]
    /\ charged' = [charged EXCEPT ![request] = 0]
    /\ reserved' = reserved - reservation[request]
    /\ UNCHANGED <<durableResult, reservation, used>>

CrashReserved(request) ==
    /\ requestState[request] = "RESERVED"
    /\ ~durableResult[request]
    /\ requestState' = [requestState EXCEPT ![request] = "RECOVERING"]
    /\ UNCHANGED <<durableResult, reservation, charged, used, reserved>>

CrashKnown(request) ==
    /\ requestState[request] = "RESULT_KNOWN"
    /\ durableResult[request]
    /\ requestState' = [requestState EXCEPT ![request] = "RECOVERING"]
    /\ UNCHANGED <<durableResult, reservation, charged, used, reserved>>

RecoverStep ==
    \E request \in Requests :
        /\ requestState[request] = "RECOVERING"
        /\ IF durableResult[request] THEN
              \E actual \in 1..reservation[request] :
                  /\ requestState' =
                      [requestState EXCEPT ![request] = "COMPLETED"]
                  /\ charged' = [charged EXCEPT ![request] = actual]
                  /\ used' = used + actual
                  /\ reserved' = reserved - reservation[request]
                  /\ UNCHANGED <<durableResult, reservation>>
           ELSE
              /\ requestState' =
                  [requestState EXCEPT ![request] = "INDETERMINATE"]
              /\ charged' = [charged EXCEPT ![request] = reservation[request]]
              /\ used' = used + reservation[request]
              /\ reserved' = reserved - reservation[request]
              /\ UNCHANGED <<durableResult, reservation>>

Next ==
    \/ \E request \in Requests :
        \/ Reserve(request)
        \/ ConnectorResult(request)
        \/ CompleteKnown(request)
        \/ ReleaseBeforeConnector(request)
        \/ CrashReserved(request)
        \/ CrashKnown(request)
    \/ RecoverStep

Spec == Init /\ [][Next]_vars /\ WF_vars(RecoverStep)

TypeOK ==
    /\ requestState \in [Requests -> RequestStates]
    /\ durableResult \in [Requests -> BOOLEAN]
    /\ reservation \in [Requests -> 0..MaxBudget]
    /\ charged \in [Requests -> 0..MaxBudget]
    /\ used \in 0..MaxBudget
    /\ reserved \in 0..MaxBudget

BudgetSafety == used + reserved <= MaxBudget

TerminalChargeBounded ==
    \A request \in Requests :
        requestState[request] \in TerminalStates =>
            charged[request] <= reservation[request]

ReleasedChargesZero ==
    \A request \in Requests :
        requestState[request] = "RELEASED" => charged[request] = 0

IndeterminateChargesReservation ==
    \A request \in Requests :
        requestState[request] = "INDETERMINATE" =>
            charged[request] = reservation[request]

KnownRecoveredCompletes ==
    \A request \in Requests :
        /\ requestState[request] \in TerminalStates
        /\ durableResult[request]
        => requestState[request] = "COMPLETED"

RecoveryConverges ==
    \A request \in Requests :
        [](requestState[request] = "RECOVERING" =>
            <>(requestState[request] \in TerminalStates))

=============================================================================
