------------------------------ MODULE TaskGate ------------------------------
EXTENDS Naturals, FiniteSets, Sequences

(***************************************************************************
TaskGate's finite-state authorization and accounting model.

The scalar budget represents one independently checked budget dimension.  The
implementation applies the same reserve/settle discipline to query count,
rows, and database time.  A request moves through the connector and durable
settlement phases; crashes are explicit transitions and never reset a request
identifier to NEW.
***************************************************************************)

CONSTANTS Relations, Columns, Scopes, RequestIds, ReceiptIds, None,
          MaxBudget, MaxNarrowings, MaxReplayAttempts,
          MaxDuplicateAttempts, MaxInvalidApprovals

ASSUME /\ Relations # {}
       /\ Columns # {}
       /\ Scopes # {}
       /\ RequestIds # {}
       /\ ReceiptIds # {}
       /\ None \notin Relations \cup Columns \cup Scopes
       /\ MaxBudget \in Nat \ {0}
       /\ MaxNarrowings \in Nat
       /\ MaxReplayAttempts \in Nat
       /\ MaxDuplicateAttempts \in Nat
       /\ MaxInvalidApprovals \in Nat

TaskStates == {"PENDING", "ACTIVE", "REVOKED", "EXPIRED", "COMPLETED", "FAILED"}
TerminalStates == {"REVOKED", "EXPIRED", "COMPLETED", "FAILED"}

RequestStates == {
    "NEW", "RESERVED", "EXECUTING", "RESULT_KNOWN", "SETTLING",
    "RECOVERING", "SETTLED", "RELEASED", "INDETERMINATE"
}

InFlightStates == {"RESERVED", "EXECUTING", "RESULT_KNOWN", "SETTLING", "RECOVERING"}

GrantType == [rels : SUBSET Relations,
              cols : SUBSET Columns,
              scopes : SUBSET Scopes,
              limit : 0..MaxBudget]

EmptyGrant == [rels |-> {}, cols |-> {}, scopes |-> {}, limit |-> 0]
InitialGrant == [rels |-> Relations, cols |-> Columns,
                 scopes |-> Scopes, limit |-> MaxBudget]

VARIABLES taskState,
          approvalValid,
          catalogMatches,
          grantHistory,
          processedReceipts,
          approvalEffects,
          replayAttempts,
          invalidApprovalAttempts,
          duplicateAttempts,
          used,
          reserved,
          requestState,
          requestRelation,
          requestColumn,
          requestScope,
          requestGrant,
          approvalAtStart,
          catalogAtStart,
          executionCount,
          startedRequests,
          terminalStartSnapshot

vars == <<taskState, approvalValid, catalogMatches, grantHistory,
          processedReceipts, approvalEffects, replayAttempts,
          invalidApprovalAttempts, duplicateAttempts, used, reserved,
          requestState, requestRelation, requestColumn, requestScope,
          requestGrant, approvalAtStart, catalogAtStart, executionCount,
          startedRequests, terminalStartSnapshot>>

CurrentGrant == grantHistory[Len(grantHistory)]

NoInFlight == \A request \in RequestIds : requestState[request] \notin InFlightStates

Init ==
    /\ taskState = "PENDING"
    /\ approvalValid = FALSE
    /\ catalogMatches = TRUE
    /\ grantHistory = <<InitialGrant>>
    /\ processedReceipts = {}
    /\ approvalEffects = [receipt \in ReceiptIds |-> 0]
    /\ replayAttempts = 0
    /\ invalidApprovalAttempts = 0
    /\ duplicateAttempts = 0
    /\ used = 0
    /\ reserved = 0
    /\ requestState = [request \in RequestIds |-> "NEW"]
    /\ requestRelation = [request \in RequestIds |-> None]
    /\ requestColumn = [request \in RequestIds |-> None]
    /\ requestScope = [request \in RequestIds |-> None]
    /\ requestGrant = [request \in RequestIds |-> EmptyGrant]
    /\ approvalAtStart = [request \in RequestIds |-> FALSE]
    /\ catalogAtStart = [request \in RequestIds |-> FALSE]
    /\ executionCount = [request \in RequestIds |-> 0]
    /\ startedRequests = {}
    /\ terminalStartSnapshot = {}

NarrowGrant ==
    /\ taskState = "PENDING"
    /\ Len(grantHistory) <= MaxNarrowings
    /\ \E newRelations \in SUBSET CurrentGrant.rels,
          newColumns \in SUBSET CurrentGrant.cols,
          newScopes \in SUBSET CurrentGrant.scopes,
          newLimit \in 0..CurrentGrant.limit :
          /\ \/ newRelations # CurrentGrant.rels
             \/ newColumns # CurrentGrant.cols
             \/ newScopes # CurrentGrant.scopes
             \/ newLimit # CurrentGrant.limit
          /\ grantHistory' = Append(grantHistory,
                 [rels |-> newRelations, cols |-> newColumns,
                  scopes |-> newScopes, limit |-> newLimit])
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts, used, reserved,
                    requestState, requestRelation, requestColumn, requestScope,
                    requestGrant, approvalAtStart, catalogAtStart,
                    executionCount, startedRequests, terminalStartSnapshot>>

ApproveFresh ==
    \E receipt \in ReceiptIds :
        /\ taskState = "PENDING"
        /\ receipt \notin processedReceipts
        /\ taskState' = "ACTIVE"
        /\ approvalValid' = TRUE
        /\ processedReceipts' = processedReceipts \cup {receipt}
        /\ approvalEffects' = [approvalEffects EXCEPT ![receipt] = @ + 1]
        /\ UNCHANGED <<catalogMatches, grantHistory, replayAttempts,
                        invalidApprovalAttempts, duplicateAttempts, used, reserved,
                        requestState, requestRelation, requestColumn, requestScope,
                        requestGrant, approvalAtStart, catalogAtStart,
                        executionCount, startedRequests, terminalStartSnapshot>>

ReplayApproval ==
    /\ replayAttempts < MaxReplayAttempts
    /\ \E receipt \in processedReceipts : TRUE
    /\ replayAttempts' = replayAttempts + 1
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, invalidApprovalAttempts,
                    duplicateAttempts, used, reserved, requestState,
                    requestRelation, requestColumn, requestScope, requestGrant,
                    approvalAtStart, catalogAtStart, executionCount,
                    startedRequests, terminalStartSnapshot>>

RejectInvalidApproval ==
    /\ taskState = "PENDING"
    /\ invalidApprovalAttempts < MaxInvalidApprovals
    /\ invalidApprovalAttempts' = invalidApprovalAttempts + 1
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    duplicateAttempts, used, reserved, requestState,
                    requestRelation, requestColumn, requestScope, requestGrant,
                    approvalAtStart, catalogAtStart, executionCount,
                    startedRequests, terminalStartSnapshot>>

ReserveQuery ==
    \E request \in RequestIds,
       relation \in CurrentGrant.rels,
       column \in CurrentGrant.cols,
       scope \in CurrentGrant.scopes :
        /\ requestState[request] = "NEW"
        /\ taskState = "ACTIVE"
        /\ approvalValid
        /\ catalogMatches
        /\ NoInFlight
        /\ used + reserved + 1 <= CurrentGrant.limit
        /\ requestState' = [requestState EXCEPT ![request] = "RESERVED"]
        /\ requestRelation' = [requestRelation EXCEPT ![request] = relation]
        /\ requestColumn' = [requestColumn EXCEPT ![request] = column]
        /\ requestScope' = [requestScope EXCEPT ![request] = scope]
        /\ requestGrant' = [requestGrant EXCEPT ![request] = CurrentGrant]
        /\ approvalAtStart' = [approvalAtStart EXCEPT ![request] = approvalValid]
        /\ catalogAtStart' = [catalogAtStart EXCEPT ![request] = catalogMatches]
        /\ startedRequests' = startedRequests \cup {request}
        /\ reserved' = reserved + 1
        /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                        processedReceipts, approvalEffects, replayAttempts,
                        invalidApprovalAttempts, duplicateAttempts, used,
                        executionCount, terminalStartSnapshot>>

DuplicateRequest ==
    /\ duplicateAttempts < MaxDuplicateAttempts
    /\ \E request \in RequestIds : requestState[request] # "NEW"
    /\ duplicateAttempts' = duplicateAttempts + 1
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, used, reserved, requestState,
                    requestRelation, requestColumn, requestScope, requestGrant,
                    approvalAtStart, catalogAtStart, executionCount,
                    startedRequests, terminalStartSnapshot>>

BeginConnector(request) ==
    /\ requestState[request] = "RESERVED"
    /\ requestState' = [requestState EXCEPT ![request] = "EXECUTING"]
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts, used, reserved,
                    requestRelation, requestColumn, requestScope, requestGrant,
                    approvalAtStart, catalogAtStart, executionCount,
                    startedRequests, terminalStartSnapshot>>

ConnectorResult(request) ==
    /\ requestState[request] = "EXECUTING"
    /\ requestState' = [requestState EXCEPT ![request] = "RESULT_KNOWN"]
    /\ executionCount' = [executionCount EXCEPT ![request] = @ + 1]
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts, used, reserved,
                    requestRelation, requestColumn, requestScope, requestGrant,
                    approvalAtStart, catalogAtStart, startedRequests,
                    terminalStartSnapshot>>

BeginSettle(request) ==
    /\ requestState[request] = "RESULT_KNOWN"
    /\ requestState' = [requestState EXCEPT ![request] = "SETTLING"]
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts, used, reserved,
                    requestRelation, requestColumn, requestScope, requestGrant,
                    approvalAtStart, catalogAtStart, executionCount,
                    startedRequests, terminalStartSnapshot>>

SettleKnown(request) ==
    /\ requestState[request] \in {"RESULT_KNOWN", "SETTLING", "RECOVERING"}
    /\ requestState' = [requestState EXCEPT ![request] = "SETTLED"]
    /\ used' = used + 1
    /\ reserved' = reserved - 1
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts,
                    requestRelation, requestColumn, requestScope, requestGrant,
                    approvalAtStart, catalogAtStart, executionCount,
                    startedRequests, terminalStartSnapshot>>

DefinitePreConnectorFailure(request) ==
    /\ requestState[request] = "RESERVED"
    /\ requestState' = [requestState EXCEPT ![request] = "RELEASED"]
    /\ reserved' = reserved - 1
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts, used,
                    requestRelation, requestColumn, requestScope, requestGrant,
                    approvalAtStart, catalogAtStart, executionCount,
                    startedRequests, terminalStartSnapshot>>

CrashFromDurableReservation(request) ==
    /\ requestState[request] = "RESERVED"
    /\ requestState' = [requestState EXCEPT ![request] = "INDETERMINATE"]
    /\ executionCount' = [executionCount EXCEPT ![request] = @ + 1]
    /\ used' = used + 1
    /\ reserved' = reserved - 1
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts,
                    requestRelation, requestColumn, requestScope, requestGrant,
                    approvalAtStart, catalogAtStart, startedRequests,
                    terminalStartSnapshot>>

CrashUnknownOutcome(request) ==
    /\ requestState[request] = "EXECUTING"
    /\ requestState' = [requestState EXCEPT ![request] = "INDETERMINATE"]
    /\ executionCount' = [executionCount EXCEPT ![request] = @ + 1]
    /\ used' = used + 1
    /\ reserved' = reserved - 1
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts,
                    requestRelation, requestColumn, requestScope, requestGrant,
                    approvalAtStart, catalogAtStart, startedRequests,
                    terminalStartSnapshot>>

CrashWithKnownResult(request) ==
    /\ requestState[request] \in {"RESULT_KNOWN", "SETTLING"}
    /\ requestState' = [requestState EXCEPT ![request] = "RECOVERING"]
    /\ UNCHANGED <<taskState, approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts, used, reserved,
                    requestRelation, requestColumn, requestScope, requestGrant,
                    approvalAtStart, catalogAtStart, executionCount,
                    startedRequests, terminalStartSnapshot>>

Revoke ==
    /\ taskState = "ACTIVE"
    /\ taskState' = "REVOKED"
    /\ terminalStartSnapshot' = startedRequests
    /\ UNCHANGED <<approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts, used, reserved,
                    requestState, requestRelation, requestColumn, requestScope,
                    requestGrant, approvalAtStart, catalogAtStart,
                    executionCount, startedRequests>>

Expire ==
    /\ taskState \in {"PENDING", "ACTIVE"}
    /\ taskState' = "EXPIRED"
    /\ terminalStartSnapshot' = startedRequests
    /\ UNCHANGED <<approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts, used, reserved,
                    requestState, requestRelation, requestColumn, requestScope,
                    requestGrant, approvalAtStart, catalogAtStart,
                    executionCount, startedRequests>>

RejectTask ==
    /\ taskState = "PENDING"
    /\ taskState' = "FAILED"
    /\ terminalStartSnapshot' = startedRequests
    /\ UNCHANGED <<approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts, used, reserved,
                    requestState, requestRelation, requestColumn, requestScope,
                    requestGrant, approvalAtStart, catalogAtStart,
                    executionCount, startedRequests>>

CompleteTask ==
    /\ taskState = "ACTIVE"
    /\ NoInFlight
    /\ taskState' = "COMPLETED"
    /\ terminalStartSnapshot' = startedRequests
    /\ UNCHANGED <<approvalValid, catalogMatches, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts, used, reserved,
                    requestState, requestRelation, requestColumn, requestScope,
                    requestGrant, approvalAtStart, catalogAtStart,
                    executionCount, startedRequests>>

CatalogDrift ==
    /\ catalogMatches
    /\ catalogMatches' = FALSE
    /\ UNCHANGED <<taskState, approvalValid, grantHistory,
                    processedReceipts, approvalEffects, replayAttempts,
                    invalidApprovalAttempts, duplicateAttempts, used, reserved,
                    requestState, requestRelation, requestColumn, requestScope,
                    requestGrant, approvalAtStart, catalogAtStart,
                    executionCount, startedRequests, terminalStartSnapshot>>

RequestStep ==
    \E request \in RequestIds :
        \/ BeginConnector(request)
        \/ ConnectorResult(request)
        \/ BeginSettle(request)
        \/ SettleKnown(request)
        \/ DefinitePreConnectorFailure(request)
        \/ CrashFromDurableReservation(request)
        \/ CrashUnknownOutcome(request)
        \/ CrashWithKnownResult(request)

Next ==
    \/ NarrowGrant
    \/ ApproveFresh
    \/ ReplayApproval
    \/ RejectInvalidApproval
    \/ ReserveQuery
    \/ DuplicateRequest
    \/ RequestStep
    \/ Revoke
    \/ Expire
    \/ RejectTask
    \/ CompleteTask
    \/ CatalogDrift

Spec == Init /\ [][Next]_vars

(***************************************************************************
Safety properties checked by TLC.
***************************************************************************)

TypeOK ==
    /\ taskState \in TaskStates
    /\ approvalValid \in BOOLEAN
    /\ catalogMatches \in BOOLEAN
    /\ grantHistory \in Seq(GrantType)
    /\ Len(grantHistory) >= 1
    /\ processedReceipts \subseteq ReceiptIds
    /\ approvalEffects \in [ReceiptIds -> Nat]
    /\ replayAttempts \in 0..MaxReplayAttempts
    /\ invalidApprovalAttempts \in 0..MaxInvalidApprovals
    /\ duplicateAttempts \in 0..MaxDuplicateAttempts
    /\ used \in 0..MaxBudget
    /\ reserved \in 0..MaxBudget
    /\ requestState \in [RequestIds -> RequestStates]
    /\ requestRelation \in [RequestIds -> Relations \cup {None}]
    /\ requestColumn \in [RequestIds -> Columns \cup {None}]
    /\ requestScope \in [RequestIds -> Scopes \cup {None}]
    /\ requestGrant \in [RequestIds -> GrantType]
    /\ approvalAtStart \in [RequestIds -> BOOLEAN]
    /\ catalogAtStart \in [RequestIds -> BOOLEAN]
    /\ executionCount \in [RequestIds -> 0..1]
    /\ startedRequests \subseteq RequestIds
    /\ terminalStartSnapshot \subseteq RequestIds

NoExecutionWithoutApproval ==
    \A request \in RequestIds : executionCount[request] > 0 => approvalAtStart[request]

ExecutedWithinGrant ==
    \A request \in RequestIds :
        executionCount[request] > 0 =>
            /\ requestRelation[request] \in requestGrant[request].rels
            /\ requestColumn[request] \in requestGrant[request].cols
            /\ requestScope[request] \in requestGrant[request].scopes

GrantMonotonic ==
    \A index \in 2..Len(grantHistory) :
        /\ grantHistory[index].rels \subseteq grantHistory[index - 1].rels
        /\ grantHistory[index].cols \subseteq grantHistory[index - 1].cols
        /\ grantHistory[index].scopes \subseteq grantHistory[index - 1].scopes
        /\ grantHistory[index].limit <= grantHistory[index - 1].limit

BudgetSafety == used + reserved <= CurrentGrant.limit

ApprovalReplaySafe ==
    \A receipt \in ReceiptIds : approvalEffects[receipt] <= 1

AtMostOnceExecution ==
    \A request \in RequestIds : executionCount[request] <= 1

NoNewQueryAfterTerminal ==
    taskState \in TerminalStates => startedRequests = terminalStartSnapshot

CatalogFailClosed ==
    \A request \in startedRequests : catalogAtStart[request]

SingleInFlight ==
    Cardinality({request \in RequestIds : requestState[request] \in InFlightStates}) <= 1

=============================================================================
