package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTaskGrantCoreV1CheckNarrowing(t *testing.T) {
	issuedAt := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	parent := testGrantCoreV1(t, issuedAt)
	candidate := testGrantCoreV1(t, issuedAt)
	candidate.ApprovedProducts = []string{"expense_summary"}
	candidate.ApprovedColumns = map[string][]string{"expense_summary": {"total_amount"}}
	candidate.MandatoryScope = map[string]any{
		"department":   []string{"sales"},
		"expense_date": map[string]string{"from": "2026-02-01", "to": "2026-05-31"},
	}
	candidate.Budget = AuthorizationBudgetV1{
		MaxQueries: 2, MaxResultRows: 20, MaxDBMS: 10_000,
		PerQueryTimeoutMS: 2_000, TaskTTLMS: 300_000,
	}
	candidate.ExpiresAt = issuedAt.Add(5 * time.Minute)
	candidate.SensitivityCeiling = SensitivityLow
	if err := parent.CheckNarrowing(candidate); err != nil {
		t.Fatalf("safe narrowing rejected: %v", err)
	}
}

func TestTaskGrantCoreV1RejectsEveryExpansionDimension(t *testing.T) {
	issuedAt := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	tests := map[string]func(*TaskGrantCoreV1){
		"agent identity": func(candidate *TaskGrantCoreV1) { candidate.AgentID = "agent:other" },
		"datasource id":  func(candidate *TaskGrantCoreV1) { candidate.DatasourceID = "taskgate-other" },
		"schema digest":  func(candidate *TaskGrantCoreV1) { candidate.SchemaDigest = strings.Repeat("d", 64) },
		"product": func(candidate *TaskGrantCoreV1) {
			candidate.ApprovedProducts = append(candidate.ApprovedProducts, "payroll")
			candidate.ApprovedColumns["payroll"] = []string{"salary"}
		},
		"column": func(candidate *TaskGrantCoreV1) {
			candidate.ApprovedColumns["expense_detail"] = append(candidate.ApprovedColumns["expense_detail"], "bank_account")
		},
		"enum scope": func(candidate *TaskGrantCoreV1) {
			candidate.MandatoryScope["department"] = []string{"sales", "finance"}
		},
		"date scope": func(candidate *TaskGrantCoreV1) {
			candidate.MandatoryScope["expense_date"] = map[string]string{"from": "2025-12-31", "to": "2026-06-30"}
		},
		"missing scope": func(candidate *TaskGrantCoreV1) { delete(candidate.MandatoryScope, "department") },
		"unknown scope": func(candidate *TaskGrantCoreV1) { candidate.MandatoryScope["ignored"] = "x" },
		"expiry":        func(candidate *TaskGrantCoreV1) { candidate.ExpiresAt = candidate.ExpiresAt.Add(time.Millisecond) },
		"per-query budget": func(candidate *TaskGrantCoreV1) {
			candidate.Budget.PerQueryTimeoutMS++
		},
		"ttl": func(candidate *TaskGrantCoreV1) { candidate.Budget.TaskTTLMS++ },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			parent := testGrantCoreV1(t, issuedAt)
			candidate := testGrantCoreV1(t, issuedAt)
			mutate(&candidate)
			if err := parent.CheckNarrowing(candidate); !errors.Is(err, ErrGrantExpansion) {
				t.Fatalf("CheckNarrowing error = %v, want ErrGrantExpansion", err)
			}
		})
	}
}

func TestTaskGrantCoreV1DelegationPreservesFamilyAndNarrowsAuthority(t *testing.T) {
	issuedAt := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	parent := testGrantCoreV1(t, issuedAt)
	parent.Budget.MaxReleaseFacts = 100
	parent.Budget.MaxInfluenceFacts = 200
	parent.Budget.ExposureProfileVersion = "taskgate-exposure-v1"
	candidate := parent
	candidate.TaskID = "task-child"
	candidate.RootTaskID = parent.TaskID
	candidate.ParentTaskID = parent.TaskID
	candidate.AgentID = "agent-2"
	candidate.DeclaredObjective = "summarize approved expenses"
	candidate.ManifestDigest = strings.Repeat("d", 64)
	candidate.ApprovedProducts = []string{"expense_summary"}
	candidate.ApprovedColumns = map[string][]string{"expense_summary": {"total_amount"}}
	candidate.MandatoryScope = map[string]any{
		"department":   []string{"sales"},
		"expense_date": map[string]string{"from": "2026-02-01", "to": "2026-05-31"},
	}
	candidate.Budget.MaxQueries = 2
	candidate.Budget.MaxResultRows = 20
	candidate.Budget.MaxDBMS = 10_000
	candidate.Budget.PerQueryTimeoutMS = 2_000
	candidate.Budget.TaskTTLMS = 300_000
	candidate.Budget.MaxReleaseFacts = 40
	candidate.Budget.MaxInfluenceFacts = 80
	candidate.ExpiresAt = issuedAt.Add(5 * time.Minute)
	candidate.SensitivityCeiling = SensitivityLow
	if err := parent.CheckDelegation(candidate); err != nil {
		t.Fatalf("safe delegation rejected: %v", err)
	}

	for name, mutate := range map[string]func(*TaskGrantCoreV1){
		"wrong root":        func(child *TaskGrantCoreV1) { child.RootTaskID = "another-root" },
		"wrong parent":      func(child *TaskGrantCoreV1) { child.ParentTaskID = "another-parent" },
		"different human":   func(child *TaskGrantCoreV1) { child.HumanSubject = "mallory" },
		"release expansion": func(child *TaskGrantCoreV1) { child.Budget.MaxReleaseFacts = 101 },
		"product expansion": func(child *TaskGrantCoreV1) {
			child.ApprovedProducts = append(child.ApprovedProducts, "payroll")
			child.ApprovedColumns["payroll"] = []string{"salary"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := candidate
			invalid.ApprovedProducts = append([]string(nil), candidate.ApprovedProducts...)
			invalid.ApprovedColumns = cloneAuthorizationColumns(candidate.ApprovedColumns)
			mutate(&invalid)
			if err := parent.CheckDelegation(invalid); !errors.Is(err, ErrGrantExpansion) {
				t.Fatalf("CheckDelegation error = %v, want ErrGrantExpansion", err)
			}
		})
	}
}

func testGrantCoreV1(t *testing.T, issuedAt time.Time) TaskGrantCoreV1 {
	t.Helper()
	manifest := AuthorizationManifestV1{
		Version: AuthorizationManifestV1Version, TaskID: "task-1",
		HumanSubject: "alice", AgentID: "agent-1", DeclaredObjective: "compare expenses",
		Products: []string{"expense_detail", "expense_summary"},
		ApprovedColumns: map[string][]string{
			"expense_detail": {"receipt_no", "amount"}, "expense_summary": {"month", "total_amount"},
		},
		MandatoryScope: map[string]any{
			"department": []string{"sales", "marketing"},
			"expense_date": map[string]string{
				"from": "2026-01-01", "to": "2026-06-30",
			},
		},
		Sensitivity: SensitivityHigh,
		Budget: AuthorizationBudgetV1{
			MaxQueries: 5, MaxResultRows: 100, MaxDBMS: 15_000,
			PerQueryTimeoutMS: 5_000, TaskTTLMS: 900_000,
		},
		CatalogVersion: "catalog-v1", CatalogSHA256: strings.Repeat("a", 64),
		DatasourceID: "taskgate-test-expenses", SchemaDigest: strings.Repeat("c", 64),
		CallbackContext: "callback-1", Nonce: strings.Repeat("0", 32),
	}
	core, err := CoreFromManifest(manifest, strings.Repeat("b", 64), issuedAt)
	if err != nil {
		t.Fatalf("CoreFromManifest: %v", err)
	}
	return core
}
