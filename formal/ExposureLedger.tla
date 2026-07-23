--------------------------- MODULE ExposureLedger ---------------------------
EXTENDS Naturals, FiniteSets

(***************************************************************************
Finite split model for TaskGate's root-family dual exposure ledger.

Every root and delegated child shares knownRelease/knownInfluence. Query
results are buffered and provenance-derived before an atomic settle either
adds only novel facts and permits delivery or rejects without disclosure.
***************************************************************************)

CONSTANTS RootTask, ChildTask, Requests, ReleaseFacts, InfluenceFacts,
          MaxRelease, MaxInfluence, MaxReplays

ASSUME /\ RootTask # ChildTask
       /\ Requests # {}
       /\ ReleaseFacts # {}
       /\ InfluenceFacts # {}
       /\ MaxRelease \in Nat \ {0}
       /\ MaxInfluence \in Nat \ {0}
       /\ MaxReplays \in Nat

Tasks == {RootTask, ChildTask}
RequestStates == {"NEW", "RESERVED", "BUFFERED", "DERIVED",
                  "SETTLED", "REJECTED", "RELEASED"}
TerminalStates == {"SETTLED", "REJECTED", "RELEASED"}

VARIABLES active,
          requestState,
          requestTask,
          bufferedRelease,
          bufferedInfluence,
          derivedRelease,
          derivedInfluence,
          priorKnownRelease,
          priorKnownInfluence,
          chargedRelease,
          chargedInfluence,
          knownRelease,
          knownInfluence,
          delivered,
          physicalExecutions,
          replayCount

vars == <<active, requestState, requestTask,
          bufferedRelease, bufferedInfluence,
          derivedRelease, derivedInfluence,
          priorKnownRelease, priorKnownInfluence,
          chargedRelease, chargedInfluence,
          knownRelease, knownInfluence, delivered,
          physicalExecutions, replayCount>>

CanStart(task) == active[task] /\ active[RootTask]

Init ==
    /\ active = [task \in Tasks |-> TRUE]
    /\ requestState = [request \in Requests |-> "NEW"]
    /\ requestTask = [request \in Requests |-> RootTask]
    /\ bufferedRelease = [request \in Requests |-> {}]
    /\ bufferedInfluence = [request \in Requests |-> {}]
    /\ derivedRelease = [request \in Requests |-> {}]
    /\ derivedInfluence = [request \in Requests |-> {}]
    /\ priorKnownRelease = [request \in Requests |-> {}]
    /\ priorKnownInfluence = [request \in Requests |-> {}]
    /\ chargedRelease = [request \in Requests |-> {}]
    /\ chargedInfluence = [request \in Requests |-> {}]
    /\ knownRelease = {}
    /\ knownInfluence = {}
    /\ delivered = [request \in Requests |-> FALSE]
    /\ physicalExecutions = [request \in Requests |-> 0]
    /\ replayCount = [request \in Requests |-> 0]

Reserve(request, task) ==
    /\ requestState[request] = "NEW"
    /\ CanStart(task)
    /\ requestState' = [requestState EXCEPT ![request] = "RESERVED"]
    /\ requestTask' = [requestTask EXCEPT ![request] = task]
    /\ UNCHANGED <<active, bufferedRelease, bufferedInfluence,
                    derivedRelease, derivedInfluence,
                    priorKnownRelease, priorKnownInfluence,
                    chargedRelease, chargedInfluence,
                    knownRelease, knownInfluence, delivered,
                    physicalExecutions, replayCount>>

ExecuteAndBuffer(request) ==
    \E release \in SUBSET ReleaseFacts,
       influence \in SUBSET InfluenceFacts :
        /\ requestState[request] = "RESERVED"
        /\ requestState' = [requestState EXCEPT ![request] = "BUFFERED"]
        /\ bufferedRelease' = [bufferedRelease EXCEPT ![request] = release]
        /\ bufferedInfluence' = [bufferedInfluence EXCEPT ![request] = influence]
        /\ physicalExecutions' = [physicalExecutions EXCEPT ![request] = @ + 1]
        /\ UNCHANGED <<active, requestTask, derivedRelease,
                        derivedInfluence, priorKnownRelease,
                        priorKnownInfluence, chargedRelease,
                        chargedInfluence, knownRelease, knownInfluence,
                        delivered, replayCount>>

DeriveProvenance(request) ==
    /\ requestState[request] = "BUFFERED"
    /\ requestState' = [requestState EXCEPT ![request] = "DERIVED"]
    /\ derivedRelease' = [derivedRelease EXCEPT ![request] = bufferedRelease[request]]
    /\ derivedInfluence' = [derivedInfluence EXCEPT ![request] = bufferedInfluence[request]]
    /\ UNCHANGED <<active, requestTask, bufferedRelease,
                    bufferedInfluence, priorKnownRelease,
                    priorKnownInfluence, chargedRelease,
                    chargedInfluence, knownRelease, knownInfluence,
                    delivered, physicalExecutions, replayCount>>

Settle(request) ==
    LET novelRelease == derivedRelease[request] \ knownRelease
        novelInfluence == derivedInfluence[request] \ knownInfluence
    IN  /\ requestState[request] = "DERIVED"
        /\ Cardinality(knownRelease \cup novelRelease) <= MaxRelease
        /\ Cardinality(knownInfluence \cup novelInfluence) <= MaxInfluence
        /\ requestState' = [requestState EXCEPT ![request] = "SETTLED"]
        /\ priorKnownRelease' = [priorKnownRelease EXCEPT ![request] = knownRelease]
        /\ priorKnownInfluence' = [priorKnownInfluence EXCEPT ![request] = knownInfluence]
        /\ chargedRelease' = [chargedRelease EXCEPT ![request] = novelRelease]
        /\ chargedInfluence' = [chargedInfluence EXCEPT ![request] = novelInfluence]
        /\ knownRelease' = knownRelease \cup novelRelease
        /\ knownInfluence' = knownInfluence \cup novelInfluence
        /\ delivered' = [delivered EXCEPT ![request] = TRUE]
        /\ UNCHANGED <<active, requestTask, bufferedRelease,
                        bufferedInfluence, derivedRelease,
                        derivedInfluence, physicalExecutions, replayCount>>

RejectOverBudget(request) ==
    LET novelRelease == derivedRelease[request] \ knownRelease
        novelInfluence == derivedInfluence[request] \ knownInfluence
    IN  /\ requestState[request] = "DERIVED"
        /\ \/ Cardinality(knownRelease \cup novelRelease) > MaxRelease
           \/ Cardinality(knownInfluence \cup novelInfluence) > MaxInfluence
        /\ requestState' = [requestState EXCEPT ![request] = "REJECTED"]
        /\ UNCHANGED <<active, requestTask, bufferedRelease,
                        bufferedInfluence, derivedRelease,
                        derivedInfluence, priorKnownRelease,
                        priorKnownInfluence, chargedRelease,
                        chargedInfluence, knownRelease, knownInfluence,
                        delivered, physicalExecutions, replayCount>>

ReleaseBeforeExecution(request) ==
    /\ requestState[request] = "RESERVED"
    /\ requestState' = [requestState EXCEPT ![request] = "RELEASED"]
    /\ UNCHANGED <<active, requestTask, bufferedRelease,
                    bufferedInfluence, derivedRelease,
                    derivedInfluence, priorKnownRelease,
                    priorKnownInfluence, chargedRelease,
                    chargedInfluence, knownRelease, knownInfluence,
                    delivered, physicalExecutions, replayCount>>

Replay(request) ==
    /\ requestState[request] \in TerminalStates
    /\ replayCount[request] < MaxReplays
    /\ replayCount' = [replayCount EXCEPT ![request] = @ + 1]
    /\ UNCHANGED <<active, requestState, requestTask,
                    bufferedRelease, bufferedInfluence,
                    derivedRelease, derivedInfluence,
                    priorKnownRelease, priorKnownInfluence,
                    chargedRelease, chargedInfluence,
                    knownRelease, knownInfluence, delivered,
                    physicalExecutions>>

RevokeRoot ==
    /\ active[RootTask]
    /\ active' = [active EXCEPT ![RootTask] = FALSE]
    /\ UNCHANGED <<requestState, requestTask,
                    bufferedRelease, bufferedInfluence,
                    derivedRelease, derivedInfluence,
                    priorKnownRelease, priorKnownInfluence,
                    chargedRelease, chargedInfluence,
                    knownRelease, knownInfluence, delivered,
                    physicalExecutions, replayCount>>

AllTerminal == \A request \in Requests : requestState[request] \in TerminalStates

Idle ==
    /\ AllTerminal
    /\ UNCHANGED vars

Next ==
    \/ \E request \in Requests, task \in Tasks : Reserve(request, task)
    \/ \E request \in Requests :
        \/ ExecuteAndBuffer(request)
        \/ DeriveProvenance(request)
        \/ Settle(request)
        \/ RejectOverBudget(request)
        \/ ReleaseBeforeExecution(request)
        \/ Replay(request)
    \/ RevokeRoot
    \/ Idle

Spec == Init /\ [][Next]_vars

UnionCharges(mapping) == UNION {mapping[request] : request \in Requests}

TypeOK ==
    /\ active \in [Tasks -> BOOLEAN]
    /\ requestState \in [Requests -> RequestStates]
    /\ requestTask \in [Requests -> Tasks]
    /\ bufferedRelease \in [Requests -> SUBSET ReleaseFacts]
    /\ bufferedInfluence \in [Requests -> SUBSET InfluenceFacts]
    /\ derivedRelease \in [Requests -> SUBSET ReleaseFacts]
    /\ derivedInfluence \in [Requests -> SUBSET InfluenceFacts]
    /\ priorKnownRelease \in [Requests -> SUBSET ReleaseFacts]
    /\ priorKnownInfluence \in [Requests -> SUBSET InfluenceFacts]
    /\ chargedRelease \in [Requests -> SUBSET ReleaseFacts]
    /\ chargedInfluence \in [Requests -> SUBSET InfluenceFacts]
    /\ knownRelease \in SUBSET ReleaseFacts
    /\ knownInfluence \in SUBSET InfluenceFacts
    /\ delivered \in [Requests -> BOOLEAN]
    /\ physicalExecutions \in [Requests -> 0..1]
    /\ replayCount \in [Requests -> 0..MaxReplays]

DualBudgetSafety ==
    /\ Cardinality(knownRelease) <= MaxRelease
    /\ Cardinality(knownInfluence) <= MaxInfluence

NoDeliveryBeforeSettle ==
    \A request \in Requests : delivered[request] <=> requestState[request] = "SETTLED"

ExactNovelCharge ==
    \A request \in Requests : requestState[request] = "SETTLED" =>
        /\ chargedRelease[request] = derivedRelease[request] \ priorKnownRelease[request]
        /\ chargedInfluence[request] = derivedInfluence[request] \ priorKnownInfluence[request]

NoChargeWithoutSettlement ==
    \A request \in Requests : requestState[request] # "SETTLED" =>
        /\ chargedRelease[request] = {}
        /\ chargedInfluence[request] = {}

RejectedResultsStayBuffered ==
    \A request \in Requests : requestState[request] = "REJECTED" =>
        /\ ~delivered[request]
        /\ physicalExecutions[request] = 1

DerivedEvidenceMatchesBuffer ==
    \A request \in Requests : requestState[request] \in {"DERIVED", "SETTLED", "REJECTED"} =>
        /\ derivedRelease[request] = bufferedRelease[request]
        /\ derivedInfluence[request] = bufferedInfluence[request]

NovelChargesDoNotOverlap ==
    \A left, right \in Requests : left # right =>
        /\ chargedRelease[left] \cap chargedRelease[right] = {}
        /\ chargedInfluence[left] \cap chargedInfluence[right] = {}

TaskFamilyNonAmplification ==
    /\ knownRelease = UnionCharges(chargedRelease)
    /\ knownInfluence = UnionCharges(chargedInfluence)

DeliveredQueriesExecutedOnce ==
    \A request \in Requests : delivered[request] => physicalExecutions[request] = 1

=============================================================================
