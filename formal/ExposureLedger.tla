--------------------------- MODULE ExposureLedger ---------------------------
EXTENDS Naturals, FiniteSets

(***************************************************************************
Finite split model for TaskGate's root-family three-dimensional exposure ledger.

Every root and delegated child shares knownRelease/knownInfluence/knownOutcome. Query
results are buffered and provenance-derived before an atomic settle either
adds only novel facts to the accounted ledger or rejects without disclosure.
Artifact publication and logical availability are modeled separately in
ArtifactPublication.tla.
***************************************************************************)

CONSTANTS RootTask, ChildTask, Requests, ReleaseFacts, InfluenceFacts, OutcomeFacts,
          PredicateAtoms, CompositeOutcomes,
          MaxRelease, MaxInfluence, MaxOutcome, MaxReplays

ASSUME /\ RootTask # ChildTask
       /\ Requests # {}
       /\ ReleaseFacts # {}
       /\ InfluenceFacts # {}
       /\ OutcomeFacts # {}
       /\ PredicateAtoms \subseteq OutcomeFacts
       /\ CompositeOutcomes \subseteq OutcomeFacts
       /\ PredicateAtoms \cap CompositeOutcomes = {}
       /\ PredicateAtoms \cup CompositeOutcomes = OutcomeFacts
       /\ MaxRelease \in Nat \ {0}
       /\ MaxInfluence \in Nat \ {0}
       /\ MaxOutcome \in Nat \ {0}
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
          bufferedOutcome,
          derivedRelease,
          derivedInfluence,
          derivedOutcome,
          priorKnownRelease,
          priorKnownInfluence,
          priorKnownOutcome,
          chargedRelease,
          chargedInfluence,
          chargedOutcome,
          knownRelease,
          knownInfluence,
          knownOutcome,
          accounted,
          physicalExecutions,
          replayCount

vars == <<active, requestState, requestTask,
          bufferedRelease, bufferedInfluence, bufferedOutcome,
          derivedRelease, derivedInfluence, derivedOutcome,
          priorKnownRelease, priorKnownInfluence, priorKnownOutcome,
          chargedRelease, chargedInfluence, chargedOutcome,
          knownRelease, knownInfluence, knownOutcome, accounted,
          physicalExecutions, replayCount>>

CanStart(task) == active[task] /\ active[RootTask]

Init ==
    /\ active = [task \in Tasks |-> TRUE]
    /\ requestState = [request \in Requests |-> "NEW"]
    /\ requestTask = [request \in Requests |-> RootTask]
    /\ bufferedRelease = [request \in Requests |-> {}]
    /\ bufferedInfluence = [request \in Requests |-> {}]
    /\ bufferedOutcome = [request \in Requests |-> {}]
    /\ derivedRelease = [request \in Requests |-> {}]
    /\ derivedInfluence = [request \in Requests |-> {}]
    /\ derivedOutcome = [request \in Requests |-> {}]
    /\ priorKnownRelease = [request \in Requests |-> {}]
    /\ priorKnownInfluence = [request \in Requests |-> {}]
    /\ priorKnownOutcome = [request \in Requests |-> {}]
    /\ chargedRelease = [request \in Requests |-> {}]
    /\ chargedInfluence = [request \in Requests |-> {}]
    /\ chargedOutcome = [request \in Requests |-> {}]
    /\ knownRelease = {}
    /\ knownInfluence = {}
    /\ knownOutcome = {}
    /\ accounted = [request \in Requests |-> FALSE]
    /\ physicalExecutions = [request \in Requests |-> 0]
    /\ replayCount = [request \in Requests |-> 0]

Reserve(request, task) ==
    /\ requestState[request] = "NEW"
    /\ CanStart(task)
    /\ requestState' = [requestState EXCEPT ![request] = "RESERVED"]
    /\ requestTask' = [requestTask EXCEPT ![request] = task]
    /\ UNCHANGED <<active, bufferedRelease, bufferedInfluence, bufferedOutcome,
                    derivedRelease, derivedInfluence, derivedOutcome,
                    priorKnownRelease, priorKnownInfluence, priorKnownOutcome,
                    chargedRelease, chargedInfluence, chargedOutcome,
                    knownRelease, knownInfluence, knownOutcome, accounted,
                    physicalExecutions, replayCount>>

ExecuteAndBuffer(request) ==
    \E release \in SUBSET ReleaseFacts,
       influence \in SUBSET InfluenceFacts,
       outcome \in SUBSET OutcomeFacts :
        /\ requestState[request] = "RESERVED"
        /\ outcome # {}
        /\ Cardinality(outcome \cap CompositeOutcomes) = 1
        /\ outcome \ PredicateAtoms \subseteq CompositeOutcomes
        /\ requestState' = [requestState EXCEPT ![request] = "BUFFERED"]
        /\ bufferedRelease' = [bufferedRelease EXCEPT ![request] = release]
        /\ bufferedInfluence' = [bufferedInfluence EXCEPT ![request] = influence]
        /\ bufferedOutcome' = [bufferedOutcome EXCEPT ![request] = outcome]
        /\ physicalExecutions' = [physicalExecutions EXCEPT ![request] = @ + 1]
        /\ UNCHANGED <<active, requestTask, derivedRelease,
                        derivedInfluence, derivedOutcome, priorKnownRelease,
                        priorKnownInfluence, priorKnownOutcome, chargedRelease,
                        chargedInfluence, chargedOutcome, knownRelease, knownInfluence, knownOutcome,
                        accounted, replayCount>>

DeriveProvenance(request) ==
    /\ requestState[request] = "BUFFERED"
    /\ requestState' = [requestState EXCEPT ![request] = "DERIVED"]
    /\ derivedRelease' = [derivedRelease EXCEPT ![request] = bufferedRelease[request]]
    /\ derivedInfluence' = [derivedInfluence EXCEPT ![request] = bufferedInfluence[request]]
    /\ derivedOutcome' = [derivedOutcome EXCEPT ![request] = bufferedOutcome[request]]
    /\ UNCHANGED <<active, requestTask, bufferedRelease,
                    bufferedInfluence, bufferedOutcome, priorKnownRelease,
                    priorKnownInfluence, priorKnownOutcome, chargedRelease,
                    chargedInfluence, chargedOutcome, knownRelease, knownInfluence, knownOutcome,
                    accounted, physicalExecutions, replayCount>>

Settle(request) ==
    LET novelRelease == derivedRelease[request] \ knownRelease
        novelInfluence == derivedInfluence[request] \ knownInfluence
        novelOutcome == derivedOutcome[request] \ knownOutcome
    IN  /\ requestState[request] = "DERIVED"
        /\ Cardinality(knownRelease \cup novelRelease) <= MaxRelease
        /\ Cardinality(knownInfluence \cup novelInfluence) <= MaxInfluence
        /\ Cardinality(knownOutcome \cup novelOutcome) <= MaxOutcome
        /\ requestState' = [requestState EXCEPT ![request] = "SETTLED"]
        /\ priorKnownRelease' = [priorKnownRelease EXCEPT ![request] = knownRelease]
        /\ priorKnownInfluence' = [priorKnownInfluence EXCEPT ![request] = knownInfluence]
        /\ priorKnownOutcome' = [priorKnownOutcome EXCEPT ![request] = knownOutcome]
        /\ chargedRelease' = [chargedRelease EXCEPT ![request] = novelRelease]
        /\ chargedInfluence' = [chargedInfluence EXCEPT ![request] = novelInfluence]
        /\ chargedOutcome' = [chargedOutcome EXCEPT ![request] = novelOutcome]
        /\ knownRelease' = knownRelease \cup novelRelease
        /\ knownInfluence' = knownInfluence \cup novelInfluence
        /\ knownOutcome' = knownOutcome \cup novelOutcome
        /\ accounted' = [accounted EXCEPT ![request] = TRUE]
        /\ UNCHANGED <<active, requestTask, bufferedRelease,
                        bufferedInfluence, bufferedOutcome, derivedRelease,
                        derivedInfluence, derivedOutcome, physicalExecutions, replayCount>>

RejectOverBudget(request) ==
    LET novelRelease == derivedRelease[request] \ knownRelease
        novelInfluence == derivedInfluence[request] \ knownInfluence
        novelOutcome == derivedOutcome[request] \ knownOutcome
    IN  /\ requestState[request] = "DERIVED"
        /\ \/ Cardinality(knownRelease \cup novelRelease) > MaxRelease
           \/ Cardinality(knownInfluence \cup novelInfluence) > MaxInfluence
           \/ Cardinality(knownOutcome \cup novelOutcome) > MaxOutcome
        /\ requestState' = [requestState EXCEPT ![request] = "REJECTED"]
        /\ UNCHANGED <<active, requestTask, bufferedRelease,
                        bufferedInfluence, bufferedOutcome, derivedRelease,
                        derivedInfluence, derivedOutcome, priorKnownRelease,
                        priorKnownInfluence, priorKnownOutcome, chargedRelease,
                        chargedInfluence, chargedOutcome, knownRelease, knownInfluence, knownOutcome,
                        accounted, physicalExecutions, replayCount>>

ReleaseBeforeExecution(request) ==
    /\ requestState[request] = "RESERVED"
    /\ requestState' = [requestState EXCEPT ![request] = "RELEASED"]
    /\ UNCHANGED <<active, requestTask, bufferedRelease,
                    bufferedInfluence, bufferedOutcome, derivedRelease,
                    derivedInfluence, derivedOutcome, priorKnownRelease,
                    priorKnownInfluence, priorKnownOutcome, chargedRelease,
                    chargedInfluence, chargedOutcome, knownRelease, knownInfluence, knownOutcome,
                    accounted, physicalExecutions, replayCount>>

Replay(request) ==
    /\ requestState[request] \in TerminalStates
    /\ replayCount[request] < MaxReplays
    /\ replayCount' = [replayCount EXCEPT ![request] = @ + 1]
    /\ UNCHANGED <<active, requestState, requestTask,
                    bufferedRelease, bufferedInfluence, bufferedOutcome,
                    derivedRelease, derivedInfluence, derivedOutcome,
                    priorKnownRelease, priorKnownInfluence, priorKnownOutcome,
                    chargedRelease, chargedInfluence, chargedOutcome,
                    knownRelease, knownInfluence, knownOutcome, accounted,
                    physicalExecutions>>

RevokeRoot ==
    /\ active[RootTask]
    /\ active' = [active EXCEPT ![RootTask] = FALSE]
    /\ UNCHANGED <<requestState, requestTask,
                    bufferedRelease, bufferedInfluence, bufferedOutcome,
                    derivedRelease, derivedInfluence, derivedOutcome,
                    priorKnownRelease, priorKnownInfluence, priorKnownOutcome,
                    chargedRelease, chargedInfluence, chargedOutcome,
                    knownRelease, knownInfluence, knownOutcome, accounted,
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
    /\ bufferedOutcome \in [Requests -> SUBSET OutcomeFacts]
    /\ derivedRelease \in [Requests -> SUBSET ReleaseFacts]
    /\ derivedInfluence \in [Requests -> SUBSET InfluenceFacts]
    /\ derivedOutcome \in [Requests -> SUBSET OutcomeFacts]
    /\ priorKnownRelease \in [Requests -> SUBSET ReleaseFacts]
    /\ priorKnownInfluence \in [Requests -> SUBSET InfluenceFacts]
    /\ priorKnownOutcome \in [Requests -> SUBSET OutcomeFacts]
    /\ chargedRelease \in [Requests -> SUBSET ReleaseFacts]
    /\ chargedInfluence \in [Requests -> SUBSET InfluenceFacts]
    /\ chargedOutcome \in [Requests -> SUBSET OutcomeFacts]
    /\ knownRelease \in SUBSET ReleaseFacts
    /\ knownInfluence \in SUBSET InfluenceFacts
    /\ knownOutcome \in SUBSET OutcomeFacts
    /\ accounted \in [Requests -> BOOLEAN]
    /\ physicalExecutions \in [Requests -> 0..1]
    /\ replayCount \in [Requests -> 0..MaxReplays]

TripleBudgetSafety ==
    /\ Cardinality(knownRelease) <= MaxRelease
    /\ Cardinality(knownInfluence) <= MaxInfluence
    /\ Cardinality(knownOutcome) <= MaxOutcome

AccountedIffSettled ==
    \A request \in Requests : accounted[request] <=> requestState[request] = "SETTLED"

ExactNovelCharge ==
    \A request \in Requests : requestState[request] = "SETTLED" =>
        /\ chargedRelease[request] = derivedRelease[request] \ priorKnownRelease[request]
        /\ chargedInfluence[request] = derivedInfluence[request] \ priorKnownInfluence[request]
        /\ chargedOutcome[request] = derivedOutcome[request] \ priorKnownOutcome[request]

NoChargeWithoutSettlement ==
    \A request \in Requests : requestState[request] # "SETTLED" =>
        /\ chargedRelease[request] = {}
        /\ chargedInfluence[request] = {}
        /\ chargedOutcome[request] = {}

RejectedResultsStayBuffered ==
    \A request \in Requests : requestState[request] = "REJECTED" =>
        /\ ~accounted[request]
        /\ physicalExecutions[request] = 1

DerivedEvidenceMatchesBuffer ==
    \A request \in Requests : requestState[request] \in {"DERIVED", "SETTLED", "REJECTED"} =>
        /\ derivedRelease[request] = bufferedRelease[request]
        /\ derivedInfluence[request] = bufferedInfluence[request]
        /\ derivedOutcome[request] = bufferedOutcome[request]

NovelChargesDoNotOverlap ==
    \A left, right \in Requests : left # right =>
        /\ chargedRelease[left] \cap chargedRelease[right] = {}
        /\ chargedInfluence[left] \cap chargedInfluence[right] = {}
        /\ chargedOutcome[left] \cap chargedOutcome[right] = {}

TaskFamilyNonAmplification ==
    /\ knownRelease = UnionCharges(chargedRelease)
    /\ knownInfluence = UnionCharges(chargedInfluence)
    /\ knownOutcome = UnionCharges(chargedOutcome)

SettledQueriesExecutedOnce ==
    \A request \in Requests : accounted[request] => physicalExecutions[request] = 1

=============================================================================
