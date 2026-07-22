---------------------------- MODULE ReceiptAudit ----------------------------
EXTENDS Naturals

(***************************************************************************
Split finite model for terminal query receipts and audit binding.  It models
the semantic checks performed by the receipt verifier: charge is bounded by
reservation, before/after budget values agree with the charge, status-specific
result/error fields are legal, and the persisted receipt binds the terminal
audit sequence/hash.
***************************************************************************)

CONSTANTS Requests, Hashes, ErrorCodes, None, MaxBudget, MaxAuditSeq

ASSUME /\ Requests # {}
       /\ Hashes # {}
       /\ ErrorCodes # {}
       /\ None \notin Hashes \cup ErrorCodes
       /\ MaxBudget \in Nat \ {0}
       /\ MaxAuditSeq \in Nat \ {0}

RequestStates == {"NEW", "COMPLETED", "RELEASED", "FAILED", "INDETERMINATE"}
TerminalStates == {"COMPLETED", "RELEASED", "FAILED", "INDETERMINATE"}

VARIABLES requestState,
          reservation,
          charge,
          beforeBudget,
          afterBudget,
          resultHash,
          errorCode,
          terminalAuditSeq,
          terminalAuditHash,
          receiptPersisted,
          receiptAuditSeq,
          receiptAuditHash,
          nextAuditSeq

vars == <<requestState, reservation, charge, beforeBudget, afterBudget,
          resultHash, errorCode, terminalAuditSeq, terminalAuditHash,
          receiptPersisted, receiptAuditSeq, receiptAuditHash, nextAuditSeq>>

Init ==
    /\ requestState = [request \in Requests |-> "NEW"]
    /\ reservation = [request \in Requests |-> 0]
    /\ charge = [request \in Requests |-> 0]
    /\ beforeBudget = [request \in Requests |-> MaxBudget]
    /\ afterBudget = [request \in Requests |-> MaxBudget]
    /\ resultHash = [request \in Requests |-> None]
    /\ errorCode = [request \in Requests |-> None]
    /\ terminalAuditSeq = [request \in Requests |-> 0]
    /\ terminalAuditHash = [request \in Requests |-> None]
    /\ receiptPersisted = [request \in Requests |-> FALSE]
    /\ receiptAuditSeq = [request \in Requests |-> 0]
    /\ receiptAuditHash = [request \in Requests |-> None]
    /\ nextAuditSeq = 1

PersistTerminal(request, status, reserve, actual, before, result, error, hash) ==
    /\ requestState[request] = "NEW"
    /\ nextAuditSeq <= MaxAuditSeq
    /\ reserve \in 0..before
    /\ actual \in 0..reserve
    /\ before \in 0..MaxBudget
    /\ requestState' = [requestState EXCEPT ![request] = status]
    /\ reservation' = [reservation EXCEPT ![request] = reserve]
    /\ charge' = [charge EXCEPT ![request] = actual]
    /\ beforeBudget' = [beforeBudget EXCEPT ![request] = before]
    /\ afterBudget' = [afterBudget EXCEPT ![request] = before - actual]
    /\ resultHash' = [resultHash EXCEPT ![request] = result]
    /\ errorCode' = [errorCode EXCEPT ![request] = error]
    /\ terminalAuditSeq' = [terminalAuditSeq EXCEPT ![request] = nextAuditSeq]
    /\ terminalAuditHash' = [terminalAuditHash EXCEPT ![request] = hash]
    /\ receiptPersisted' = [receiptPersisted EXCEPT ![request] = TRUE]
    /\ receiptAuditSeq' = [receiptAuditSeq EXCEPT ![request] = nextAuditSeq]
    /\ receiptAuditHash' = [receiptAuditHash EXCEPT ![request] = hash]
    /\ nextAuditSeq' = nextAuditSeq + 1

Complete(request) ==
    \E reserve \in 1..MaxBudget,
       actual \in 1..MaxBudget,
       result \in Hashes,
       hash \in Hashes :
        /\ actual <= reserve
        /\ PersistTerminal(request, "COMPLETED", reserve, actual, MaxBudget,
                           result, None, hash)

Release(request) ==
    \E reserve \in 1..MaxBudget,
       error \in ErrorCodes,
       hash \in Hashes :
        PersistTerminal(request, "RELEASED", reserve, 0, MaxBudget,
                        None, error, hash)

Fail(request) ==
    \E reserve \in 1..MaxBudget,
       actual \in 1..MaxBudget,
       error \in ErrorCodes,
       hash \in Hashes :
        /\ actual <= reserve
        /\ PersistTerminal(request, "FAILED", reserve, actual, MaxBudget,
                           None, error, hash)

Indeterminate(request) ==
    \E reserve \in 1..MaxBudget,
       error \in ErrorCodes,
       hash \in Hashes :
        PersistTerminal(request, "INDETERMINATE", reserve, reserve, MaxBudget,
                        None, error, hash)

Next ==
    \E request \in Requests :
        \/ Complete(request)
        \/ Release(request)
        \/ Fail(request)
        \/ Indeterminate(request)

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ requestState \in [Requests -> RequestStates]
    /\ reservation \in [Requests -> 0..MaxBudget]
    /\ charge \in [Requests -> 0..MaxBudget]
    /\ beforeBudget \in [Requests -> 0..MaxBudget]
    /\ afterBudget \in [Requests -> 0..MaxBudget]
    /\ resultHash \in [Requests -> Hashes \cup {None}]
    /\ errorCode \in [Requests -> ErrorCodes \cup {None}]
    /\ terminalAuditSeq \in [Requests -> 0..MaxAuditSeq]
    /\ terminalAuditHash \in [Requests -> Hashes \cup {None}]
    /\ receiptPersisted \in [Requests -> BOOLEAN]
    /\ receiptAuditSeq \in [Requests -> 0..MaxAuditSeq]
    /\ receiptAuditHash \in [Requests -> Hashes \cup {None}]
    /\ nextAuditSeq \in 1..(MaxAuditSeq + 1)

ReceiptExistsForTerminal ==
    \A request \in Requests :
        requestState[request] \in TerminalStates => receiptPersisted[request]

ReceiptBindsTerminalAudit ==
    \A request \in Requests :
        receiptPersisted[request] =>
            /\ receiptAuditSeq[request] = terminalAuditSeq[request]
            /\ receiptAuditHash[request] = terminalAuditHash[request]
            /\ receiptAuditSeq[request] # 0
            /\ receiptAuditHash[request] # None

ChargeWithinReservation ==
    \A request \in Requests : charge[request] <= reservation[request]

BudgetTransitionValid ==
    \A request \in Requests :
        receiptPersisted[request] =>
            beforeBudget[request] = afterBudget[request] + charge[request]

StatusFieldsValid ==
    \A request \in Requests :
        CASE requestState[request] = "COMPLETED" ->
                /\ resultHash[request] # None
                /\ errorCode[request] = None
          [] requestState[request] = "RELEASED" ->
                /\ charge[request] = 0
                /\ resultHash[request] = None
                /\ errorCode[request] # None
          [] requestState[request] = "FAILED" ->
                /\ resultHash[request] = None
                /\ errorCode[request] # None
          [] requestState[request] = "INDETERMINATE" ->
                /\ charge[request] = reservation[request]
                /\ resultHash[request] = None
                /\ errorCode[request] # None
          [] OTHER -> TRUE

=============================================================================
