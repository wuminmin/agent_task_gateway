----------------------- MODULE ArtifactPublication -----------------------
EXTENDS Naturals

(***************************************************************************
Finite model of the recoverable publication protocol that follows atomic
ledger settlement.  STAGED is private and not logically available.  Settle
commits a PENDING intent and expected object hash.  CreateCanonical and
MarkAvailable are deliberately separate: a crash may leave a verified
canonical object while Control still records PENDING.  Recovery may finish
that intent without executing or charging the query again.
Physical downloads after AVAILABLE are intentionally outside the model.
***************************************************************************)

CONSTANTS Requests, ExpectedHash, MaxRetries

ASSUME /\ Requests # {}
       /\ ExpectedHash \in Nat \ {0}
       /\ MaxRetries \in Nat \ {0}

ArtifactStates == {"NONE", "STAGED", "PENDING", "AVAILABLE"}

VARIABLES settled,
          rejected,
          artifactState,
          committedHash,
          canonicalExists,
          canonicalHash,
          physicalExecutions,
          recovered,
          promotionFailed,
          hashMismatchSeen,
          retryRequired,
          retryCount,
          recoveryDeltaRelease,
          recoveryDeltaInfluence,
          recoveryDeltaOutcome

vars == <<settled, rejected, artifactState, committedHash, canonicalExists, canonicalHash,
          physicalExecutions, recovered, promotionFailed, hashMismatchSeen,
          retryRequired, retryCount, recoveryDeltaRelease,
          recoveryDeltaInfluence, recoveryDeltaOutcome>>

Init ==
    /\ settled = [request \in Requests |-> FALSE]
    /\ rejected = [request \in Requests |-> FALSE]
    /\ artifactState = [request \in Requests |-> "NONE"]
    /\ committedHash = [request \in Requests |-> 0]
    /\ canonicalExists = [request \in Requests |-> FALSE]
    /\ canonicalHash = [request \in Requests |-> 0]
    /\ physicalExecutions = [request \in Requests |-> 0]
    /\ recovered = [request \in Requests |-> FALSE]
    /\ promotionFailed = [request \in Requests |-> FALSE]
    /\ hashMismatchSeen = [request \in Requests |-> FALSE]
    /\ retryRequired = [request \in Requests |-> FALSE]
    /\ retryCount = [request \in Requests |-> 0]
    /\ recoveryDeltaRelease = [request \in Requests |-> 0]
    /\ recoveryDeltaInfluence = [request \in Requests |-> 0]
    /\ recoveryDeltaOutcome = [request \in Requests |-> 0]

Stage(request) ==
    /\ artifactState[request] = "NONE"
    /\ ~settled[request]
    /\ ~rejected[request]
    /\ artifactState' = [artifactState EXCEPT ![request] = "STAGED"]
    /\ physicalExecutions' = [physicalExecutions EXCEPT ![request] = 1]
    /\ UNCHANGED <<settled, rejected, committedHash, canonicalExists,
                    canonicalHash, recovered,
                    promotionFailed, hashMismatchSeen, retryRequired, retryCount,
                    recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

Settle(request) ==
    /\ artifactState[request] = "STAGED"
    /\ ~settled[request]
    /\ ~rejected[request]
    /\ settled' = [settled EXCEPT ![request] = TRUE]
    /\ artifactState' = [artifactState EXCEPT ![request] = "PENDING"]
    /\ committedHash' = [committedHash EXCEPT ![request] = ExpectedHash]
    /\ UNCHANGED <<rejected, canonicalExists, canonicalHash,
                    physicalExecutions, recovered,
                    promotionFailed, hashMismatchSeen, retryRequired, retryCount,
                    recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

Reject(request) ==
    /\ artifactState[request] = "STAGED"
    /\ ~settled[request]
    /\ ~rejected[request]
    /\ rejected' = [rejected EXCEPT ![request] = TRUE]
    /\ artifactState' = [artifactState EXCEPT ![request] = "NONE"]
    /\ UNCHANGED <<settled, committedHash, canonicalExists, canonicalHash,
                    physicalExecutions,
                    recovered, promotionFailed, hashMismatchSeen, retryRequired,
                    retryCount, recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

CreateCanonical(request) ==
    /\ settled[request]
    /\ artifactState[request] = "PENDING"
    /\ ~retryRequired[request]
    /\ ~canonicalExists[request]
    /\ canonicalExists' = [canonicalExists EXCEPT ![request] = TRUE]
    /\ canonicalHash' = [canonicalHash EXCEPT ![request] = committedHash[request]]
    /\ UNCHANGED <<settled, rejected, artifactState, committedHash, physicalExecutions,
                    recovered, promotionFailed, hashMismatchSeen, retryRequired,
                    retryCount, recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

MarkAvailable(request) ==
    /\ settled[request]
    /\ artifactState[request] = "PENDING"
    /\ canonicalExists[request]
    /\ canonicalHash[request] = committedHash[request]
    /\ ~retryRequired[request]
    /\ artifactState' = [artifactState EXCEPT ![request] = "AVAILABLE"]
    /\ UNCHANGED <<settled, rejected, committedHash, canonicalExists,
                    canonicalHash, physicalExecutions, recovered,
                    promotionFailed, hashMismatchSeen, retryRequired, retryCount,
                    recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

CanonicalCreateFail(request) ==
    /\ settled[request]
    /\ artifactState[request] = "PENDING"
    /\ ~canonicalExists[request]
    /\ ~retryRequired[request]
    /\ ~promotionFailed[request]
    /\ retryCount[request] < MaxRetries
    /\ promotionFailed' = [promotionFailed EXCEPT ![request] = TRUE]
    /\ retryRequired' = [retryRequired EXCEPT ![request] = TRUE]
    /\ UNCHANGED <<settled, rejected, artifactState, committedHash,
                    canonicalExists, canonicalHash,
                    physicalExecutions, recovered, hashMismatchSeen, retryCount,
                    recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

AvailableCommitFail(request) ==
    /\ settled[request]
    /\ artifactState[request] = "PENDING"
    /\ canonicalExists[request]
    /\ canonicalHash[request] = committedHash[request]
    /\ ~retryRequired[request]
    /\ retryCount[request] < MaxRetries
    /\ promotionFailed' = [promotionFailed EXCEPT ![request] = TRUE]
    /\ retryRequired' = [retryRequired EXCEPT ![request] = TRUE]
    /\ UNCHANGED <<settled, rejected, artifactState, committedHash,
                    canonicalExists, canonicalHash, physicalExecutions,
                    recovered, hashMismatchSeen, retryCount,
                    recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

PromotionHashMismatch(request) ==
    /\ settled[request]
    /\ artifactState[request] = "PENDING"
    /\ ~retryRequired[request]
    /\ ~hashMismatchSeen[request]
    /\ retryCount[request] < MaxRetries
    /\ ~canonicalExists[request]
    /\ canonicalExists' = [canonicalExists EXCEPT ![request] = TRUE]
    /\ canonicalHash' = [canonicalHash EXCEPT ![request] = ExpectedHash + 1]
    /\ hashMismatchSeen' = [hashMismatchSeen EXCEPT ![request] = TRUE]
    /\ retryRequired' = [retryRequired EXCEPT ![request] = TRUE]
    /\ UNCHANGED <<settled, rejected, artifactState, committedHash,
                    physicalExecutions, recovered, promotionFailed, retryCount,
                    recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

RetryPending(request) ==
    /\ settled[request]
    /\ artifactState[request] = "PENDING"
    /\ retryRequired[request]
    /\ retryCount[request] < MaxRetries
    /\ retryRequired' = [retryRequired EXCEPT ![request] = FALSE]
    /\ retryCount' = [retryCount EXCEPT ![request] = @ + 1]
    /\ UNCHANGED <<settled, rejected, artifactState, committedHash,
                    canonicalExists, canonicalHash, physicalExecutions,
                    recovered, promotionFailed,
                    hashMismatchSeen, recoveryDeltaRelease,
                    recoveryDeltaInfluence, recoveryDeltaOutcome>>

Recover(request) ==
    /\ settled[request]
    /\ artifactState[request] = "PENDING"
    /\ canonicalExists[request]
    /\ canonicalHash[request] = committedHash[request]
    /\ ~retryRequired[request]
    /\ artifactState' = [artifactState EXCEPT ![request] = "AVAILABLE"]
    /\ recovered' = [recovered EXCEPT ![request] = TRUE]
    /\ recoveryDeltaRelease' = [recoveryDeltaRelease EXCEPT ![request] = 0]
    /\ recoveryDeltaInfluence' = [recoveryDeltaInfluence EXCEPT ![request] = 0]
    /\ recoveryDeltaOutcome' = [recoveryDeltaOutcome EXCEPT ![request] = 0]
    /\ UNCHANGED <<settled, rejected, committedHash, canonicalExists,
                    canonicalHash, physicalExecutions, promotionFailed,
                    hashMismatchSeen, retryRequired, retryCount>>

Idle == UNCHANGED vars

Next ==
    \/ \E request \in Requests : Stage(request)
    \/ \E request \in Requests : Settle(request)
    \/ \E request \in Requests : Reject(request)
    \/ \E request \in Requests : CreateCanonical(request)
    \/ \E request \in Requests : MarkAvailable(request)
    \/ \E request \in Requests : CanonicalCreateFail(request)
    \/ \E request \in Requests : AvailableCommitFail(request)
    \/ \E request \in Requests : PromotionHashMismatch(request)
    \/ \E request \in Requests : RetryPending(request)
    \/ \E request \in Requests : Recover(request)
    \/ Idle

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ settled \in [Requests -> BOOLEAN]
    /\ rejected \in [Requests -> BOOLEAN]
    /\ artifactState \in [Requests -> ArtifactStates]
    /\ committedHash \in [Requests -> Nat]
    /\ canonicalExists \in [Requests -> BOOLEAN]
    /\ canonicalHash \in [Requests -> Nat]
    /\ physicalExecutions \in [Requests -> 0..1]
    /\ recovered \in [Requests -> BOOLEAN]
    /\ promotionFailed \in [Requests -> BOOLEAN]
    /\ hashMismatchSeen \in [Requests -> BOOLEAN]
    /\ retryRequired \in [Requests -> BOOLEAN]
    /\ retryCount \in [Requests -> 0..MaxRetries]
    /\ recoveryDeltaRelease \in [Requests -> Nat]
    /\ recoveryDeltaInfluence \in [Requests -> Nat]
    /\ recoveryDeltaOutcome \in [Requests -> Nat]

AvailableImpliesSettledCanonicalHashMatch ==
    \A request \in Requests : artifactState[request] = "AVAILABLE" =>
        /\ settled[request]
        /\ canonicalExists[request]
        /\ committedHash[request] = ExpectedHash
        /\ canonicalHash[request] = committedHash[request]

AbsentCanonicalHasZeroHash ==
    \A request \in Requests : ~canonicalExists[request] => canonicalHash[request] = 0

RejectedNotAvailable ==
    \A request \in Requests : rejected[request] => artifactState[request] # "AVAILABLE"

RetryRequiredStaysPending ==
    \A request \in Requests : retryRequired[request] =>
        artifactState[request] = "PENDING"

RecoveryHasZeroLedgerDelta ==
    \A request \in Requests : recovered[request] =>
        /\ recoveryDeltaRelease[request] = 0
        /\ recoveryDeltaInfluence[request] = 0
        /\ recoveryDeltaOutcome[request] = 0

RecoveryDoesNotReexecute ==
    \A request \in Requests : recovered[request] => physicalExecutions[request] = 1

=============================================================================
