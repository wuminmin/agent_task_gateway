-------------------------- MODULE SQLAuthorization --------------------------
EXTENDS Naturals, FiniteSets

(***************************************************************************
Split finite model for product-aware SQL authorization.  It abstracts parsing
away and checks the authorization decision for constant, qualified-column, and
unqualified-column query shapes.  Product/column provenance is explicit, and
functions/operators over multi-product expressions must be admitted by every
source product.
***************************************************************************)

CONSTANTS ProductA, ProductB, RelationA, RelationB,
          ColumnA, ColumnB, SharedColumn,
          FunctionA, FunctionB, SharedFunction, SafeFunction,
          OperatorA, OperatorB, SharedOperator, SafeOperator,
          Requests, None

Products == {ProductA, ProductB}
Relations == {RelationA, RelationB}
Columns == {ColumnA, ColumnB, SharedColumn}
Functions == {FunctionA, FunctionB, SharedFunction, SafeFunction}
Operators == {OperatorA, OperatorB, SharedOperator, SafeOperator}
SafeFunctions == {SafeFunction}
SafeOperators == {SafeOperator}

ASSUME /\ Cardinality(Products) = 2
       /\ Cardinality(Relations) = 2
       /\ Cardinality(Columns) = 3
       /\ Cardinality(Functions) = 4
       /\ Cardinality(Operators) = 4
       /\ Requests # {}
       /\ None \notin Products \cup Relations \cup Columns \cup Functions \cup Operators

Modes == {"CONSTANT", "QUALIFIED", "UNQUALIFIED"}
RequestStates == {"NEW", "ACCEPTED", "REJECTED"}

ProductOfRelation(r) ==
    IF r = RelationA THEN ProductA ELSE ProductB

ProductColumns(p) ==
    IF p = ProductA THEN {ColumnA, SharedColumn}
    ELSE {ColumnB, SharedColumn}

ProductFunctions(p) ==
    IF p = ProductA THEN {FunctionA, SharedFunction}
    ELSE {FunctionB, SharedFunction}

ProductOperators(p) ==
    IF p = ProductA THEN {OperatorA, SharedOperator}
    ELSE {OperatorB, SharedOperator}

ColumnProductsIn(c, rels) ==
    {p \in Products :
        /\ c \in ProductColumns(p)
        /\ \E r \in rels : ProductOfRelation(r) = p}

SourcesFor(mode, rels, qrel, cols) ==
    IF mode = "CONSTANT" THEN {}
    ELSE IF mode = "QUALIFIED" THEN
        IF cols = {} THEN {} ELSE {ProductOfRelation(qrel)}
    ELSE
        UNION {ColumnProductsIn(c, rels) : c \in cols}

QualifiedColumnsAllowed(qrel, cols) ==
    \A c \in cols : c \in ProductColumns(ProductOfRelation(qrel))

UnqualifiedColumnsUnique(cols, rels) ==
    \A c \in cols : Cardinality(ColumnProductsIn(c, rels)) = 1

FunctionAllowed(source, fn) ==
    IF source = {}
    THEN fn \in SafeFunctions
    ELSE \A p \in source : fn \in ProductFunctions(p)

OperatorAllowed(source, op) ==
    IF source = {}
    THEN op \in SafeOperators
    ELSE \A p \in source : op \in ProductOperators(p)

ValidShape(mode, rels, qrel, cols) ==
    IF mode = "CONSTANT" THEN
        /\ rels = {}
        /\ cols = {}
    ELSE IF mode = "QUALIFIED" THEN
        /\ rels # {}
        /\ qrel \in rels
        /\ cols # {}
        /\ QualifiedColumnsAllowed(qrel, cols)
    ELSE
        /\ rels # {}
        /\ cols # {}
        /\ UnqualifiedColumnsUnique(cols, rels)

ValidAuthorized(mode, rels, qrel, cols, fn, op) ==
    LET source == SourcesFor(mode, rels, qrel, cols)
    IN  /\ ValidShape(mode, rels, qrel, cols)
        /\ FunctionAllowed(source, fn)
        /\ OperatorAllowed(source, op)

VARIABLES requestState,
          requestMode,
          requestRelations,
          requestQualifiedRelation,
          requestColumns,
          requestFunction,
          requestOperator,
          requestSourceProducts

vars == <<requestState, requestMode, requestRelations,
          requestQualifiedRelation, requestColumns, requestFunction,
          requestOperator, requestSourceProducts>>

Init ==
    /\ requestState = [request \in Requests |-> "NEW"]
    /\ requestMode = [request \in Requests |-> None]
    /\ requestRelations = [request \in Requests |-> {}]
    /\ requestQualifiedRelation = [request \in Requests |-> None]
    /\ requestColumns = [request \in Requests |-> {}]
    /\ requestFunction = [request \in Requests |-> None]
    /\ requestOperator = [request \in Requests |-> None]
    /\ requestSourceProducts = [request \in Requests |-> {}]

Accept(request) ==
    \E mode \in Modes,
       rels \in SUBSET Relations,
       qrel \in Relations,
       cols \in SUBSET Columns,
       fn \in Functions,
       op \in Operators :
        /\ requestState[request] = "NEW"
        /\ ValidAuthorized(mode, rels, qrel, cols, fn, op)
        /\ requestState' = [requestState EXCEPT ![request] = "ACCEPTED"]
        /\ requestMode' = [requestMode EXCEPT ![request] = mode]
        /\ requestRelations' = [requestRelations EXCEPT ![request] = rels]
        /\ requestQualifiedRelation' =
            [requestQualifiedRelation EXCEPT ![request] =
                IF mode = "CONSTANT" THEN None ELSE qrel]
        /\ requestColumns' = [requestColumns EXCEPT ![request] = cols]
        /\ requestFunction' = [requestFunction EXCEPT ![request] = fn]
        /\ requestOperator' = [requestOperator EXCEPT ![request] = op]
        /\ requestSourceProducts' =
            [requestSourceProducts EXCEPT ![request] =
                SourcesFor(mode, rels, qrel, cols)]

Reject(request) ==
    \E mode \in Modes,
       rels \in SUBSET Relations,
       qrel \in Relations,
       cols \in SUBSET Columns,
       fn \in Functions,
       op \in Operators :
        /\ requestState[request] = "NEW"
        /\ ~ValidAuthorized(mode, rels, qrel, cols, fn, op)
        /\ requestState' = [requestState EXCEPT ![request] = "REJECTED"]
        /\ requestMode' = [requestMode EXCEPT ![request] = mode]
        /\ requestRelations' = [requestRelations EXCEPT ![request] = rels]
        /\ requestQualifiedRelation' =
            [requestQualifiedRelation EXCEPT ![request] =
                IF mode = "CONSTANT" THEN None ELSE qrel]
        /\ requestColumns' = [requestColumns EXCEPT ![request] = cols]
        /\ requestFunction' = [requestFunction EXCEPT ![request] = fn]
        /\ requestOperator' = [requestOperator EXCEPT ![request] = op]
        /\ requestSourceProducts' = [requestSourceProducts EXCEPT ![request] = {}]

Next ==
    \E request \in Requests :
        \/ Accept(request)
        \/ Reject(request)

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ requestState \in [Requests -> RequestStates]
    /\ requestMode \in [Requests -> Modes \cup {None}]
    /\ requestRelations \in [Requests -> SUBSET Relations]
    /\ requestQualifiedRelation \in [Requests -> Relations \cup {None}]
    /\ requestColumns \in [Requests -> SUBSET Columns]
    /\ requestFunction \in [Requests -> Functions \cup {None}]
    /\ requestOperator \in [Requests -> Operators \cup {None}]
    /\ requestSourceProducts \in [Requests -> SUBSET Products]

AcceptedQueriesAuthorized ==
    \A request \in Requests :
        requestState[request] = "ACCEPTED" =>
            ValidAuthorized(requestMode[request],
                            requestRelations[request],
                            requestQualifiedRelation[request],
                            requestColumns[request],
                            requestFunction[request],
                            requestOperator[request])

AcceptedSourcesMatchColumns ==
    \A request \in Requests :
        requestState[request] = "ACCEPTED" =>
            requestSourceProducts[request] =
                SourcesFor(requestMode[request],
                           requestRelations[request],
                           requestQualifiedRelation[request],
                           requestColumns[request])

ConstantsUseGlobalSafeList ==
    \A request \in Requests :
        /\ requestState[request] = "ACCEPTED"
        /\ requestSourceProducts[request] = {}
        => /\ requestFunction[request] \in SafeFunctions
           /\ requestOperator[request] \in SafeOperators

UnqualifiedColumnsAreUnique ==
    \A request \in Requests :
        /\ requestState[request] = "ACCEPTED"
        /\ requestMode[request] = "UNQUALIFIED"
        => UnqualifiedColumnsUnique(requestColumns[request],
                                    requestRelations[request])

MultiProductUsesIntersection ==
    \A request \in Requests :
        /\ requestState[request] = "ACCEPTED"
        /\ Cardinality(requestSourceProducts[request]) > 1
        => /\ \A p \in requestSourceProducts[request] :
                  requestFunction[request] \in ProductFunctions(p)
           /\ \A p \in requestSourceProducts[request] :
                  requestOperator[request] \in ProductOperators(p)

=============================================================================
