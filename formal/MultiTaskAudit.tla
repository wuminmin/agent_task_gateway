--------------------------- MODULE MultiTaskAudit ---------------------------
EXTENDS Naturals, FiniteSets, Sequences

(***************************************************************************
Split finite model for multi-task interleavings and the global audit chain.
Each task may have at most one in-flight request.  Revocation and expiry can
race with an in-flight request, but they snapshot the already-started request
set and prevent any new request from starting afterward.  Every terminal query
state is tied to a globally ordered terminal audit event.
***************************************************************************)

CONSTANTS Tasks, Requests, None, MaxEvents

ASSUME /\ Tasks # {}
       /\ Requests # {}
       /\ None \notin Tasks \cup Requests
       /\ MaxEvents \in Nat \ {0}

TaskStates == {"ACTIVE", "REVOKED", "EXPIRED"}
RequestStates == {"NEW", "RESERVED", "EXECUTING", "COMPLETED", "RELEASED", "INDETERMINATE"}
InFlightStates == {"RESERVED", "EXECUTING"}
TerminalRequestStates == {"COMPLETED", "RELEASED", "INDETERMINATE"}
EventKinds == {"RESERVE", "BEGIN_EXECUTE", "COMPLETE", "RELEASE", "INDETERMINATE", "REVOKE", "EXPIRE"}
TerminalEventKinds == {"COMPLETE", "RELEASE", "INDETERMINATE"}

EventType == [seq : 1..MaxEvents,
              task : Tasks,
              request : Requests \cup {None},
              kind : EventKinds,
              prev : 0..MaxEvents,
              head : 1..MaxEvents]

VARIABLES taskState,
          requestState,
          requestTask,
          terminalSnapshot,
          auditLog,
          auditHead

vars == <<taskState, requestState, requestTask, terminalSnapshot,
          auditLog, auditHead>>

RequestsForTask(t) == {request \in Requests : requestTask[request] = t}
InFlightForTask(t) == {request \in Requests :
    /\ requestTask[request] = t
    /\ requestState[request] \in InFlightStates}

CanAppend == Len(auditLog) < MaxEvents

AppendEvent(t, request, kind) ==
    /\ CanAppend
    /\ auditLog' = Append(auditLog,
        [seq |-> Len(auditLog) + 1,
         task |-> t,
         request |-> request,
         kind |-> kind,
         prev |-> Len(auditLog),
         head |-> Len(auditLog) + 1])
    /\ auditHead' = Len(auditLog) + 1

Init ==
    /\ taskState = [t \in Tasks |-> "ACTIVE"]
    /\ requestState = [request \in Requests |-> "NEW"]
    /\ requestTask = [request \in Requests |-> None]
    /\ terminalSnapshot = [t \in Tasks |-> {}]
    /\ auditLog = <<>>
    /\ auditHead = 0

Reserve(request, t) ==
    /\ taskState[t] = "ACTIVE"
    /\ requestState[request] = "NEW"
    /\ Cardinality(InFlightForTask(t)) = 0
    /\ AppendEvent(t, request, "RESERVE")
    /\ requestState' = [requestState EXCEPT ![request] = "RESERVED"]
    /\ requestTask' = [requestTask EXCEPT ![request] = t]
    /\ UNCHANGED <<taskState, terminalSnapshot>>

BeginExecute(request) ==
    LET t == requestTask[request]
    IN  /\ t \in Tasks
        /\ requestState[request] = "RESERVED"
        /\ AppendEvent(t, request, "BEGIN_EXECUTE")
        /\ requestState' = [requestState EXCEPT ![request] = "EXECUTING"]
        /\ UNCHANGED <<taskState, requestTask, terminalSnapshot>>

Complete(request) ==
    LET t == requestTask[request]
    IN  /\ t \in Tasks
        /\ requestState[request] \in InFlightStates
        /\ AppendEvent(t, request, "COMPLETE")
        /\ requestState' = [requestState EXCEPT ![request] = "COMPLETED"]
        /\ UNCHANGED <<taskState, requestTask, terminalSnapshot>>

Release(request) ==
    LET t == requestTask[request]
    IN  /\ t \in Tasks
        /\ requestState[request] = "RESERVED"
        /\ AppendEvent(t, request, "RELEASE")
        /\ requestState' = [requestState EXCEPT ![request] = "RELEASED"]
        /\ UNCHANGED <<taskState, requestTask, terminalSnapshot>>

MarkIndeterminate(request) ==
    LET t == requestTask[request]
    IN  /\ t \in Tasks
        /\ requestState[request] \in InFlightStates
        /\ AppendEvent(t, request, "INDETERMINATE")
        /\ requestState' = [requestState EXCEPT ![request] = "INDETERMINATE"]
        /\ UNCHANGED <<taskState, requestTask, terminalSnapshot>>

Revoke(t) ==
    /\ taskState[t] = "ACTIVE"
    /\ AppendEvent(t, None, "REVOKE")
    /\ taskState' = [taskState EXCEPT ![t] = "REVOKED"]
    /\ terminalSnapshot' = [terminalSnapshot EXCEPT ![t] = RequestsForTask(t)]
    /\ UNCHANGED <<requestState, requestTask>>

Expire(t) ==
    /\ taskState[t] = "ACTIVE"
    /\ AppendEvent(t, None, "EXPIRE")
    /\ taskState' = [taskState EXCEPT ![t] = "EXPIRED"]
    /\ terminalSnapshot' = [terminalSnapshot EXCEPT ![t] = RequestsForTask(t)]
    /\ UNCHANGED <<requestState, requestTask>>

Next ==
    \/ \E request \in Requests, t \in Tasks : Reserve(request, t)
    \/ \E request \in Requests :
        \/ BeginExecute(request)
        \/ Complete(request)
        \/ Release(request)
        \/ MarkIndeterminate(request)
    \/ \E t \in Tasks :
        \/ Revoke(t)
        \/ Expire(t)

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ taskState \in [Tasks -> TaskStates]
    /\ requestState \in [Requests -> RequestStates]
    /\ requestTask \in [Requests -> Tasks \cup {None}]
    /\ terminalSnapshot \in [Tasks -> SUBSET Requests]
    /\ auditLog \in Seq(EventType)
    /\ Len(auditLog) <= MaxEvents
    /\ auditHead \in 0..MaxEvents

SingleInFlightPerTask ==
    \A t \in Tasks : Cardinality(InFlightForTask(t)) <= 1

AuditIsLinear ==
    /\ auditHead = Len(auditLog)
    /\ \A i \in 1..Len(auditLog) :
        /\ auditLog[i].seq = i
        /\ auditLog[i].prev = i - 1
        /\ auditLog[i].head = i

NoNewRequestAfterTaskTerminal ==
    \A t \in Tasks :
        taskState[t] # "ACTIVE" => RequestsForTask(t) = terminalSnapshot[t]

TerminalRequestsHaveTerminalAudit ==
    \A request \in Requests :
        requestState[request] \in TerminalRequestStates =>
            \E i \in 1..Len(auditLog) :
                /\ auditLog[i].request = request
                /\ auditLog[i].kind \in TerminalEventKinds

AuditEventsNameExistingAssignments ==
    \A i \in 1..Len(auditLog) :
        auditLog[i].request # None =>
            requestTask[auditLog[i].request] = auditLog[i].task

=============================================================================
