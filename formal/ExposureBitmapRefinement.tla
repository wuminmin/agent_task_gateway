---------------------- MODULE ExposureBitmapRefinement ---------------------
EXTENDS Naturals, FiniteSets

(***************************************************************************
Finite refinement model for TaskGate V4's ordinal bitmap ledger.

FactOf is the immutable dictionary bijection compiled with a snapshot.  The
concrete state stores ordinal sets while the specification state stores FactID
sets.  A request prepares against an epoch and may commit all three dimensions
only when its epoch still equals the root head; otherwise it must refresh.
***************************************************************************)

CONSTANTS Requests, Dimensions, FactA, FactB, OrdinalA, OrdinalB,
          SegmentA, SegmentB, MaxFacts, MaxReplays

Facts == {FactA, FactB}
Ordinals == {OrdinalA, OrdinalB}
Segments == {SegmentA, SegmentB}
FactOf == [ordinal \in Ordinals |-> IF ordinal = OrdinalA THEN FactA ELSE FactB]
SegmentOf == [ordinal \in Ordinals |->
  IF ordinal = OrdinalA THEN SegmentA ELSE SegmentB]

ASSUME /\ Requests # {}
       /\ Dimensions # {}
       /\ FactA # FactB
       /\ OrdinalA # OrdinalB
       /\ SegmentA # SegmentB
       /\ MaxFacts \in Nat \ {0}
       /\ MaxReplays \in Nat

RequestStates == {"NEW", "DERIVED", "CANDIDATE", "SETTLED", "REJECTED"}
TerminalStates == {"SETTLED", "REJECTED"}

Decode(bitmap) == {FactOf[ordinal] : ordinal \in bitmap}
Encode(facts) == {ordinal \in Ordinals : FactOf[ordinal] \in facts}
BitmapOr(left, right) == left \cup right
BitmapAndNot(left, right) == left \ right
Popcount(bitmap) == Cardinality(bitmap)

SegmentOrdinals(segment) == {ordinal \in Ordinals : SegmentOf[ordinal] = segment}
SegmentFacts(segment) == Decode(SegmentOrdinals(segment))

DictionaryBijection ==
    /\ \A left, right \in Ordinals : FactOf[left] = FactOf[right] => left = right
    /\ Decode(Ordinals) = Facts
    /\ \A segment \in Segments :
          Cardinality(SegmentOrdinals(segment)) = Cardinality(SegmentFacts(segment))

VARIABLES requestState,
          factEffect,
          bitmapEffect,
          candidateEpoch,
          priorHead,
          chargedFacts,
          head,
          knownFacts,
          rootEpoch,
          replayCount

vars == <<requestState, factEffect, bitmapEffect, candidateEpoch, priorHead,
          chargedFacts, head, knownFacts, rootEpoch, replayCount>>

EmptyFacts == [dimension \in Dimensions |-> {}]
EmptyBitmaps == [dimension \in Dimensions |-> {}]

Init ==
    /\ requestState = [request \in Requests |-> "NEW"]
    /\ factEffect = [request \in Requests |-> EmptyFacts]
    /\ bitmapEffect = [request \in Requests |-> EmptyBitmaps]
    /\ candidateEpoch = [request \in Requests |-> 0]
    /\ priorHead = [request \in Requests |-> EmptyBitmaps]
    /\ chargedFacts = [request \in Requests |-> EmptyFacts]
    /\ head = EmptyBitmaps
    /\ knownFacts = EmptyFacts
    /\ rootEpoch = 0
    /\ replayCount = [request \in Requests |-> 0]

Derive(request) ==
    \E effect \in [Dimensions -> SUBSET Facts] :
      /\ requestState[request] = "NEW"
      /\ requestState' = [requestState EXCEPT ![request] = "DERIVED"]
      /\ factEffect' = [factEffect EXCEPT ![request] = effect]
      /\ bitmapEffect' = [bitmapEffect EXCEPT
             ![request] = [dimension \in Dimensions |-> Encode(effect[dimension])]]
      /\ UNCHANGED <<candidateEpoch, priorHead, chargedFacts, head, knownFacts,
                      rootEpoch, replayCount>>

Prepare(request) ==
    /\ requestState[request] = "DERIVED"
    /\ requestState' = [requestState EXCEPT ![request] = "CANDIDATE"]
    /\ candidateEpoch' = [candidateEpoch EXCEPT ![request] = rootEpoch]
    /\ UNCHANGED <<factEffect, bitmapEffect, priorHead, chargedFacts, head,
                    knownFacts, rootEpoch, replayCount>>

RefreshAfterConflict(request) ==
    /\ requestState[request] = "CANDIDATE"
    /\ candidateEpoch[request] # rootEpoch
    /\ candidateEpoch' = [candidateEpoch EXCEPT ![request] = rootEpoch]
    /\ UNCHANGED <<requestState, factEffect, bitmapEffect, priorHead,
                    chargedFacts, head, knownFacts, rootEpoch, replayCount>>

Fits(request) ==
    \A dimension \in Dimensions :
      Popcount(BitmapOr(head[dimension], bitmapEffect[request][dimension]))
        <= MaxFacts

CommitCAS(request) ==
    /\ requestState[request] = "CANDIDATE"
    /\ candidateEpoch[request] = rootEpoch
    /\ Fits(request)
    /\ requestState' = [requestState EXCEPT ![request] = "SETTLED"]
    /\ priorHead' = [priorHead EXCEPT ![request] = head]
    /\ chargedFacts' = [chargedFacts EXCEPT
           ![request] = [dimension \in Dimensions |->
             Decode(BitmapAndNot(bitmapEffect[request][dimension], head[dimension]))]]
    /\ head' = [dimension \in Dimensions |->
          BitmapOr(head[dimension], bitmapEffect[request][dimension])]
    /\ knownFacts' = [dimension \in Dimensions |->
          knownFacts[dimension] \cup factEffect[request][dimension]]
    /\ rootEpoch' = rootEpoch + 1
    /\ UNCHANGED <<factEffect, bitmapEffect, candidateEpoch, replayCount>>

RejectOverBudget(request) ==
    /\ requestState[request] = "CANDIDATE"
    /\ candidateEpoch[request] = rootEpoch
    /\ ~Fits(request)
    /\ requestState' = [requestState EXCEPT ![request] = "REJECTED"]
    /\ UNCHANGED <<factEffect, bitmapEffect, candidateEpoch, priorHead,
                    chargedFacts, head, knownFacts, rootEpoch, replayCount>>

ReplayCommittedObservation(request) ==
    /\ requestState[request] = "SETTLED"
    /\ replayCount[request] < MaxReplays
    /\ replayCount' = [replayCount EXCEPT ![request] = @ + 1]
    /\ UNCHANGED <<requestState, factEffect, bitmapEffect, candidateEpoch,
                    priorHead, chargedFacts, head, knownFacts, rootEpoch>>

AllTerminal == \A request \in Requests : requestState[request] \in TerminalStates

Idle ==
    /\ AllTerminal
    /\ UNCHANGED vars

Next ==
    \/ \E request \in Requests : Derive(request)
    \/ \E request \in Requests : Prepare(request)
    \/ \E request \in Requests : RefreshAfterConflict(request)
    \/ \E request \in Requests : CommitCAS(request)
    \/ \E request \in Requests : RejectOverBudget(request)
    \/ \E request \in Requests : ReplayCommittedObservation(request)
    \/ Idle

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ requestState \in [Requests -> RequestStates]
    /\ factEffect \in [Requests -> [Dimensions -> SUBSET Facts]]
    /\ bitmapEffect \in [Requests -> [Dimensions -> SUBSET Ordinals]]
    /\ candidateEpoch \in [Requests -> Nat]
    /\ priorHead \in [Requests -> [Dimensions -> SUBSET Ordinals]]
    /\ chargedFacts \in [Requests -> [Dimensions -> SUBSET Facts]]
    /\ head \in [Dimensions -> SUBSET Ordinals]
    /\ knownFacts \in [Dimensions -> SUBSET Facts]
    /\ rootEpoch \in Nat
    /\ replayCount \in [Requests -> 0..MaxReplays]

DecodeOrRefinement ==
    \A left, right \in SUBSET Ordinals :
      Decode(BitmapOr(left, right)) = Decode(left) \cup Decode(right)

DecodeAndNotRefinement ==
    \A left, right \in SUBSET Ordinals :
      Decode(BitmapAndNot(left, right)) = Decode(left) \ Decode(right)

PopcountRefinement ==
    \A bitmap \in SUBSET Ordinals : Popcount(bitmap) = Cardinality(Decode(bitmap))

DerivationRefinement ==
    \A request \in Requests : requestState[request] # "NEW" =>
      \A dimension \in Dimensions :
        Decode(bitmapEffect[request][dimension]) = factEffect[request][dimension]

LedgerRefinement ==
    \A dimension \in Dimensions : Decode(head[dimension]) = knownFacts[dimension]

ExactNovelCharge ==
    \A request \in Requests : requestState[request] = "SETTLED" =>
      \A dimension \in Dimensions :
        chargedFacts[request][dimension] =
          Decode(BitmapAndNot(bitmapEffect[request][dimension],
                              priorHead[request][dimension]))

TripleBudgetSafety ==
    \A dimension \in Dimensions : Cardinality(knownFacts[dimension]) <= MaxFacts

=============================================================================
