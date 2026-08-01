----------------------- MODULE ArtifactPublication -----------------------
EXTENDS Naturals

(***************************************************************************
Finite model of the recoverable publication protocol that follows atomic
ledger settlement.  STAGED is private and not logically available.  Settle
commits a PENDING intent and expected object hash.  Promote and Recover may
make that intent AVAILABLE without executing or charging the query again.
Physical downloads after AVAILABLE are intentionally outside the model.
***************************************************************************)

CONSTANTS Requests, ExpectedHash

ASSUME /\ Requests # {}
       /\ ExpectedHash \in Nat \ {0}

ArtifactStates == {"NONE", "STAGED", "PENDING", "AVAILABLE"}

VARIABLES settled,
          rejected,
          artifactState,
          committedHash,
          objectHash,
          physicalExecutions,
          recovered,
          recoveryDeltaRelease,
          recoveryDeltaInfluence,
          recoveryDeltaOutcome

vars == <<settled, rejected, artifactState, committedHash, objectHash,
          physicalExecutions, recovered, recoveryDeltaRelease,
          recoveryDeltaInfluence, recoveryDeltaOutcome>>

Init ==
    /\ settled = [request \in Requests |-> FALSE]
    /\ rejected = [request \in Requests |-> FALSE]
    /\ artifactState = [request \in Requests |-> "NONE"]
    /\ committedHash = [request \in Requests |-> 0]
    /\ objectHash = [request \in Requests |-> 0]
    /\ physicalExecutions = [request \in Requests |-> 0]
    /\ recovered = [request \in Requests |-> FALSE]
    /\ recoveryDeltaRelease = [request \in Requests |-> 0]
    /\ recoveryDeltaInfluence = [request \in Requests |-> 0]
    /\ recoveryDeltaOutcome = [request \in Requests |-> 0]

Stage(request) ==
    /\ artifactState[request] = "NONE"
    /\ ~settled[request]
    /\ ~rejected[request]
    /\ artifactState' = [artifactState EXCEPT ![request] = "STAGED"]
    /\ physicalExecutions' = [physicalExecutions EXCEPT ![request] = 1]
    /\ UNCHANGED <<settled, rejected, committedHash, objectHash, recovered,
                    recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

Settle(request) ==
    /\ artifactState[request] = "STAGED"
    /\ ~settled[request]
    /\ ~rejected[request]
    /\ settled' = [settled EXCEPT ![request] = TRUE]
    /\ artifactState' = [artifactState EXCEPT ![request] = "PENDING"]
    /\ committedHash' = [committedHash EXCEPT ![request] = ExpectedHash]
    /\ UNCHANGED <<rejected, objectHash, physicalExecutions, recovered,
                    recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

Reject(request) ==
    /\ artifactState[request] = "STAGED"
    /\ ~settled[request]
    /\ ~rejected[request]
    /\ rejected' = [rejected EXCEPT ![request] = TRUE]
    /\ artifactState' = [artifactState EXCEPT ![request] = "NONE"]
    /\ UNCHANGED <<settled, committedHash, objectHash, physicalExecutions,
                    recovered, recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

Promote(request) ==
    /\ settled[request]
    /\ artifactState[request] = "PENDING"
    /\ artifactState' = [artifactState EXCEPT ![request] = "AVAILABLE"]
    /\ objectHash' = [objectHash EXCEPT ![request] = committedHash[request]]
    /\ UNCHANGED <<settled, rejected, committedHash, physicalExecutions,
                    recovered, recoveryDeltaRelease, recoveryDeltaInfluence,
                    recoveryDeltaOutcome>>

Recover(request) ==
    /\ settled[request]
    /\ artifactState[request] = "PENDING"
    /\ artifactState' = [artifactState EXCEPT ![request] = "AVAILABLE"]
    /\ objectHash' = [objectHash EXCEPT ![request] = committedHash[request]]
    /\ recovered' = [recovered EXCEPT ![request] = TRUE]
    /\ recoveryDeltaRelease' = [recoveryDeltaRelease EXCEPT ![request] = 0]
    /\ recoveryDeltaInfluence' = [recoveryDeltaInfluence EXCEPT ![request] = 0]
    /\ recoveryDeltaOutcome' = [recoveryDeltaOutcome EXCEPT ![request] = 0]
    /\ UNCHANGED <<settled, rejected, committedHash, physicalExecutions>>

Idle == UNCHANGED vars

Next ==
    \/ \E request \in Requests : Stage(request)
    \/ \E request \in Requests : Settle(request)
    \/ \E request \in Requests : Reject(request)
    \/ \E request \in Requests : Promote(request)
    \/ \E request \in Requests : Recover(request)
    \/ Idle

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ settled \in [Requests -> BOOLEAN]
    /\ rejected \in [Requests -> BOOLEAN]
    /\ artifactState \in [Requests -> ArtifactStates]
    /\ committedHash \in [Requests -> Nat]
    /\ objectHash \in [Requests -> Nat]
    /\ physicalExecutions \in [Requests -> 0..1]
    /\ recovered \in [Requests -> BOOLEAN]
    /\ recoveryDeltaRelease \in [Requests -> Nat]
    /\ recoveryDeltaInfluence \in [Requests -> Nat]
    /\ recoveryDeltaOutcome \in [Requests -> Nat]

AvailableImpliesSettled ==
    \A request \in Requests : artifactState[request] = "AVAILABLE" => settled[request]

AvailableHashMatches ==
    \A request \in Requests : artifactState[request] = "AVAILABLE" =>
        /\ committedHash[request] = ExpectedHash
        /\ objectHash[request] = committedHash[request]

RejectedNotAvailable ==
    \A request \in Requests : rejected[request] => artifactState[request] # "AVAILABLE"

RecoveryHasZeroLedgerDelta ==
    \A request \in Requests : recovered[request] =>
        /\ recoveryDeltaRelease[request] = 0
        /\ recoveryDeltaInfluence[request] = 0
        /\ recoveryDeltaOutcome[request] = 0

RecoveryDoesNotReexecute ==
    \A request \in Requests : recovered[request] => physicalExecutions[request] = 1

=============================================================================
