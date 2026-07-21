package approval

import "testing"

func TestAuthorizationSnapshotStableAndTamperEvident(t *testing.T) {
	first := testDraftRequest()
	first.ApprovedColumns = map[string][]string{
		"expense_summary": {"month", "total_amount"},
		"expense_detail":  {"receipt_no", "amount"},
	}
	first.MandatoryScope = map[string]any{
		"department":   []string{"销售部"},
		"expense_date": map[string]string{"from": "2026-01-01", "to": "2026-06-30"},
	}
	second := testDraftRequest()
	second.ApprovedColumns = map[string][]string{
		"expense_detail":  {"receipt_no", "amount"},
		"expense_summary": {"month", "total_amount"},
	}
	second.MandatoryScope = map[string]any{
		"expense_date": map[string]any{"to": "2026-06-30", "from": "2026-01-01"},
		"department":   []any{"销售部"},
	}

	firstHash, err := AuthorizationSnapshotSHA256(first)
	if err != nil {
		t.Fatalf("hash first snapshot: %v", err)
	}
	secondHash, err := AuthorizationSnapshotSHA256(second)
	if err != nil {
		t.Fatalf("hash second snapshot: %v", err)
	}
	if firstHash != secondHash {
		t.Fatalf("equivalent JSON maps produced different hashes: %s != %s", firstHash, secondHash)
	}
	first.AuthorizationSnapshotSHA256 = firstHash
	if err := ValidateAuthorizationSnapshot(first); err != nil {
		t.Fatalf("validate untampered snapshot: %v", err)
	}

	first.ApprovedColumns["expense_detail"][1] = "employee_name"
	if err := ValidateAuthorizationSnapshot(first); err == nil {
		t.Fatal("tampered approved columns retained a valid snapshot digest")
	}
}

func TestValidateAuthorizationSnapshotRejectsBudgetTampering(t *testing.T) {
	request := testDraftRequest()
	hash, err := AuthorizationSnapshotSHA256(request)
	if err != nil {
		t.Fatalf("hash snapshot: %v", err)
	}
	request.AuthorizationSnapshotSHA256 = hash
	request.Budget.MaxRows++
	if err := ValidateAuthorizationSnapshot(request); err == nil {
		t.Fatal("tampered budget retained a valid snapshot digest")
	}
}

func testDraftRequest() DraftRequest {
	return DraftRequest{
		TaskID: "task-1", Requester: "alice", Objective: "compare expenses",
		DataProducts: []string{"expense_summary", "expense_detail"},
		ApprovedColumns: map[string][]string{
			"expense_summary": {"month", "total_amount"},
			"expense_detail":  {"receipt_no", "amount"},
		},
		MandatoryScope: map[string]any{"department": []string{"销售部"}},
		Sensitivity:    "high",
		Budget: DraftBudget{
			MaxQueries: 3, MaxRows: 50, MaxDBMS: 15_000, QueryTimeoutMS: 5_000, TaskTTLMS: 900_000,
		},
		ApprovalMode: "manual", Approver: "bob", CatalogVersion: "v1", CallbackContext: "callback-1",
	}
}
