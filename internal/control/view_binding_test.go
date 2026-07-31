package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
	"taskbound.local/agent-data-gateway/internal/viewbinding"
)

var viewBindingTestDigest, viewBindingTestCanonical = makeViewBindingTestEvidence()

func makeViewBindingTestEvidence() (string, json.RawMessage) {
	set, err := viewbinding.New([]viewbinding.ProductContract{{
		Product: "expense_summary", CanonicalPlanDigest: strings.Repeat("a", 64),
		DependencyDigest: strings.Repeat("b", 64), InterfaceDigest: strings.Repeat("c", 64),
	}})
	if err != nil {
		panic(err)
	}
	canonical, err := json.Marshal(set)
	if err != nil {
		panic(err)
	}
	digest, err := set.Digest()
	if err != nil {
		panic(err)
	}
	return digest, canonical
}

func TestValidateGrantViewBindingRejectsTamperedOrNonCanonicalEvidence(t *testing.T) {
	valid := *viewBindingApproval("task_validation", time.Now().Add(time.Hour)).Grant
	if err := validateGrantViewBinding(valid); err != nil {
		t.Fatalf("valid binding: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*TaskGrant)
	}{
		{name: "tampered canonical content", mutate: func(grant *TaskGrant) {
			var set viewbinding.Set
			if err := json.Unmarshal(grant.ViewBindingSet.CanonicalJSON, &set); err != nil {
				t.Fatal(err)
			}
			set.Products[0].InterfaceDigest = strings.Repeat("d", 64)
			grant.ViewBindingSet.CanonicalJSON, _ = json.Marshal(set)
		}},
		{name: "unknown canonical field", mutate: func(grant *TaskGrant) {
			grant.ViewBindingSet.CanonicalJSON = append(grant.ViewBindingSet.CanonicalJSON[:len(grant.ViewBindingSet.CanonicalJSON)-1], []byte(`,"unknown":true}`)...)
		}},
		{name: "noncanonical whitespace", mutate: func(grant *TaskGrant) {
			grant.ViewBindingSet.CanonicalJSON = append([]byte(" "), grant.ViewBindingSet.CanonicalJSON...)
		}},
		{name: "unsorted dependencies", mutate: func(grant *TaskGrant) {
			grant.ViewBindingSet.Dependencies[0], grant.ViewBindingSet.Dependencies[1] =
				grant.ViewBindingSet.Dependencies[1], grant.ViewBindingSet.Dependencies[0]
		}},
		{name: "missing dependencies", mutate: func(grant *TaskGrant) {
			grant.ViewBindingSet.Dependencies = nil
		}},
		{name: "binding product mismatch", mutate: func(grant *TaskGrant) {
			grant.ApprovedProducts = []string{"another_product"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			grant := valid
			binding := *valid.ViewBindingSet
			binding.CanonicalJSON = append(json.RawMessage(nil), valid.ViewBindingSet.CanonicalJSON...)
			binding.Dependencies = append([]TaskViewDependency(nil), valid.ViewBindingSet.Dependencies...)
			grant.ViewBindingSet = &binding
			grant.ApprovedProducts = append([]string(nil), valid.ApprovedProducts...)
			test.mutate(&grant)
			if err := validateGrantViewBinding(grant); err == nil {
				t.Fatal("invalid view binding was accepted")
			}
		})
	}
}

func TestTaskViewBindingPersistsWithGrantAndFailsReservationsClosedAfterDrift(t *testing.T) {
	ctx := context.Background()
	clock := fixedClock{value: time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)}
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 41), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_view_bound", expires)

	callback := viewBindingApproval("task_view_bound", expires)
	if _, err := store.ApplyApprovalCallback(ctx, callback); err != nil {
		t.Fatalf("ApplyApprovalCallback: %v", err)
	}
	grant, err := store.GetGrant(ctx, "task_view_bound")
	if err != nil {
		t.Fatalf("GetGrant: %v", err)
	}
	if grant.ViewBindingDigest != viewBindingTestDigest {
		t.Fatalf("grant view binding digest = %q", grant.ViewBindingDigest)
	}
	status, err := store.GetTaskViewBindingStatus(ctx, "task_view_bound")
	if err != nil {
		t.Fatalf("GetTaskViewBindingStatus: %v", err)
	}
	if status.Status != TaskViewBindingActive || status.BoundDigest != viewBindingTestDigest ||
		status.ObservedDigest != "" || status.DetectedAt != nil {
		t.Fatalf("initial view binding status = %+v", status)
	}
	var bindingSets, dependencies int
	var storedCanonical []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM view_binding_sets WHERE digest=$1`, viewBindingTestDigest).Scan(&bindingSets); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT canonical_json FROM view_binding_sets WHERE digest=$1`, viewBindingTestDigest).Scan(&storedCanonical); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM task_view_dependencies WHERE task_id=$1`, "task_view_bound").Scan(&dependencies); err != nil {
		t.Fatal(err)
	}
	if bindingSets != 1 || dependencies != 2 || !bytes.Equal(storedCanonical, viewBindingTestCanonical) {
		t.Fatalf("binding evidence counts = sets:%d dependencies:%d", bindingSets, dependencies)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO task_view_dependencies(task_id, binding_digest, product, dependency_key)
VALUES ($1, $2, $3, $4)`, "task_view_bound", viewBindingTestDigest, "expense_summary", "warehouse.late_append"); err == nil {
		t.Fatal("sealed task dependency set allowed an append")
	}

	request := testReserveRequest(ReserveRequest{
		QueryID: "query_view_bound", TaskID: "task_view_bound", RequestID: "request-view-bound",
		Actor: "alice_task_view_bound", RequestDigest: "request-digest", SQLFingerprint: "sql-fingerprint",
		CatalogVersion: "catalog-v1", ViewBindingDigest: viewBindingTestDigest,
		RequestedRows: 1, RequestedDBMS: 10,
	})
	reservation, err := store.ReserveBudget(ctx, request)
	if err != nil {
		t.Fatalf("ReserveBudget: %v", err)
	}
	if reservation.Replay {
		t.Fatal("first reservation was a replay")
	}
	record, err := store.GetQuery(ctx, reservation.QueryID)
	if err != nil || record.ViewBindingDigest != viewBindingTestDigest {
		t.Fatalf("query view binding evidence = %+v, %v", record, err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE query_records SET view_binding_digest=$1 WHERE id=$2`, strings.Repeat("c", 64), record.ID); err == nil {
		t.Fatal("reserved query view binding identity was mutable")
	}
	if _, err := store.ReleaseBudget(ctx, record.ID, "TEST_RELEASE"); err != nil {
		t.Fatalf("ReleaseBudget: %v", err)
	}

	observed := strings.Repeat("d", 64)
	status, err = store.MarkTaskViewSemanticChanged(ctx, "task_view_bound", observed)
	if err != nil {
		t.Fatalf("MarkTaskViewSemanticChanged: %v", err)
	}
	if status.Status != TaskViewBindingRequireRebind || status.ObservedDigest != observed || status.DetectedAt == nil {
		t.Fatalf("stale view binding status = %+v", status)
	}
	firstDetectedAt := *status.DetectedAt
	status, err = store.MarkTaskViewSemanticChanged(ctx, "task_view_bound", strings.Repeat("e", 64))
	if err != nil {
		t.Fatalf("idempotent MarkTaskViewSemanticChanged: %v", err)
	}
	if status.ObservedDigest != observed || status.DetectedAt == nil || !status.DetectedAt.Equal(firstDetectedAt) {
		t.Fatalf("repeat drift replaced first evidence: %+v", status)
	}
	var driftEvents int
	if err := store.DB().QueryRowContext(ctx, `
SELECT count(*) FROM audit_events WHERE task_id=$1 AND event_type='TASK_VIEW_SEMANTIC_CHANGED'`, "task_view_bound").Scan(&driftEvents); err != nil {
		t.Fatal(err)
	}
	if driftEvents != 1 {
		t.Fatalf("semantic drift audit events = %d, want 1", driftEvents)
	}
	if _, err := store.DB().ExecContext(ctx, `
UPDATE task_view_binding_status SET status='ACTIVE', observed_digest='', detected_at=NULL WHERE task_id=$1`, "task_view_bound"); err == nil {
		t.Fatal("REQUIRE_REBIND status was mutable")
	}

	// Exact request-id replay remains observational after drift.
	replay, err := store.ReserveBudget(ctx, request)
	if err != nil || !replay.Replay || replay.Record == nil || replay.Record.ID != record.ID {
		t.Fatalf("reservation replay after drift = %+v, %v", replay, err)
	}
	newRequest := request
	newRequest.QueryID = "query_view_stale"
	newRequest.RequestID = "request-view-stale"
	newRequest.RequestDigest = "request-digest-stale"
	if _, err := store.ReserveBudget(ctx, newRequest); !errors.Is(err, ErrViewSemanticChanged) {
		t.Fatalf("stale reservation error = %v, want ErrViewSemanticChanged", err)
	}
	if CodeOf(ErrViewSemanticChanged) != CodeViewSemanticChanged {
		t.Fatalf("view semantic error code = %s", CodeOf(ErrViewSemanticChanged))
	}
	if _, err := store.GetQuery(ctx, newRequest.QueryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale request created a query record: %v", err)
	}
}

func TestLegacyGrantWithoutViewBindingRemainsUsable(t *testing.T) {
	ctx := context.Background()
	clock := fixedClock{value: time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)}
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 42), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_view_legacy", expires)
	approveTask(t, store, "task_view_legacy", expires, BudgetLimits{Queries: 1, Rows: 10, DBMS: 100})

	grant, err := store.GetGrant(ctx, "task_view_legacy")
	if err != nil || grant.ViewBindingDigest != "" {
		t.Fatalf("legacy grant = %+v, %v", grant, err)
	}
	if _, err := store.GetTaskViewBindingStatus(ctx, "task_view_legacy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy task binding status = %v, want ErrNotFound", err)
	}
	if _, err := store.ReserveBudget(ctx, testReserveRequest(ReserveRequest{
		QueryID: "query_view_legacy", TaskID: "task_view_legacy", RequestID: "request-view-legacy",
		Actor: "alice_task_view_legacy", RequestDigest: "request-digest", SQLFingerprint: "sql-fingerprint",
		CatalogVersion: "catalog-v1", RequestedRows: 1, RequestedDBMS: 10,
	})); err != nil {
		t.Fatalf("legacy ReserveBudget: %v", err)
	}
}

func TestTamperedViewBindingEvidenceRollsBackGrantActivation(t *testing.T) {
	ctx := context.Background()
	clock := fixedClock{value: time.Date(2026, 7, 31, 11, 0, 0, 0, time.UTC)}
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 43), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_view_first", expires)
	createAwaitingApprovalTask(t, store, "task_view_tampered", expires)
	if _, err := store.ApplyApprovalCallback(ctx, viewBindingApproval("task_view_first", expires)); err != nil {
		t.Fatal(err)
	}
	tampered := viewBindingApproval("task_view_tampered", expires)
	var changed viewbinding.Set
	if err := json.Unmarshal(tampered.Grant.ViewBindingSet.CanonicalJSON, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Products[0].InterfaceDigest = strings.Repeat("d", 64)
	tampered.Grant.ViewBindingSet.CanonicalJSON, _ = json.Marshal(changed)
	if _, err := store.ApplyApprovalCallback(ctx, tampered); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered binding error = %v, want ErrInvalid", err)
	}
	task, err := store.GetTask(ctx, "task_view_tampered")
	if err != nil || task.State != TaskAwaitingApproval {
		t.Fatalf("tampered task state = %+v, %v", task, err)
	}
	if _, err := store.GetGrant(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("collision persisted a grant: %v", err)
	}
}

func viewBindingApproval(taskID string, expires time.Time) ApprovalCallback {
	return ApprovalCallback{
		EventID: "oa_" + taskID, RawPayload: []byte(`{"decision":"approved"}`),
		ExpectedState: TaskAwaitingApproval, NewState: TaskActive, Response: []byte(`{"ok":true}`),
		Event: ApprovalEvent{TaskID: taskID, Actor: "bob", Decision: "approved", Payload: []byte(`{"route":"manual"}`)},
		Grant: &TaskGrant{
			TaskID: taskID, Subject: "alice_" + taskID, Purpose: "travel analysis",
			ApprovedProducts: []string{"expense_summary"},
			ApprovedColumns:  map[string][]string{"expense_summary": {"month", "amount"}},
			MandatoryScope:   []byte(`{"department":"sales"}`), SensitivityCeiling: "internal",
			Budget: BudgetLimits{Queries: 3, Rows: 20, DBMS: 1000}, ExpiresAt: expires,
			CatalogVersion: "catalog-v1", CatalogDigest: controlTestDigest,
			DatasourceID: "taskgate-test-expenses", SchemaDigest: controlTestDigest,
			ViewBindingDigest: viewBindingTestDigest,
			ViewBindingSet: &ViewBindingSet{
				Digest: viewBindingTestDigest, ProfileVersion: viewbinding.Version,
				CanonicalJSON: append(json.RawMessage(nil), viewBindingTestCanonical...),
				Dependencies: []TaskViewDependency{
					{Product: "expense_summary", DependencyKey: "reporting.monthly_sales"},
					{Product: "expense_summary", DependencyKey: "warehouse.orders"},
				},
			},
			ApprovalReceipt: "receipt_" + taskID,
		},
	}
}
