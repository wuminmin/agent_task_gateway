package domain

import (
	"errors"
	"testing"
	"time"
)

func testGrant(now time.Time) TaskGrant {
	return TaskGrant{
		TaskID:           "task-1",
		Subject:          "alice",
		Purpose:          "analyze travel expenses",
		ApprovedProducts: []string{"expense_summary", "expense_detail"},
		ApprovedColumns: map[string][]string{
			"expense_summary": {"month", "total_amount"},
			"expense_detail":  {"claim_number", "amount"},
		},
		MandatoryScope: map[string]any{
			"department": []string{"sales"},
		},
		SensitivityCeiling: SensitivityHigh,
		Budget: Budget{
			MaxQueries:      5,
			MaxRows:         100,
			MaxDBTime:       15 * time.Second,
			PerQueryTimeout: 5 * time.Second,
			TaskTTL:         15 * time.Minute,
		},
		ExpiresAt:       now.Add(15 * time.Minute),
		CatalogVersion:  "2026.07.21",
		ApprovalReceipt: "approval-1",
	}
}

func TestTaskGrantNarrowing(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	parent := testGrant(now)
	candidate := testGrant(now)
	candidate.ApprovedProducts = []string{"expense_summary"}
	candidate.ApprovedColumns = map[string][]string{
		"expense_summary": {"total_amount"},
	}
	candidate.MandatoryScope = map[string]any{
		"department":   []string{"sales"},
		"expense_date": map[string]string{"from": "2026-01-01", "to": "2026-06-30"},
	}
	candidate.SensitivityCeiling = SensitivityLow
	candidate.Budget.MaxQueries = 2
	candidate.ExpiresAt = now.Add(10 * time.Minute)
	if err := parent.CheckNarrowing(candidate); err != nil {
		t.Fatalf("safe narrowing rejected: %v", err)
	}
}

func TestTaskGrantRejectsExpansion(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	tests := map[string]func(*TaskGrant){
		"product": func(candidate *TaskGrant) {
			candidate.ApprovedProducts = append(candidate.ApprovedProducts, "payroll")
			candidate.ApprovedColumns["payroll"] = []string{"salary"}
		},
		"column": func(candidate *TaskGrant) {
			candidate.ApprovedColumns["expense_detail"] = append(candidate.ApprovedColumns["expense_detail"], "bank_account")
		},
		"scope": func(candidate *TaskGrant) {
			candidate.MandatoryScope = map[string]any{}
		},
		"expiry": func(candidate *TaskGrant) {
			candidate.ExpiresAt = candidate.ExpiresAt.Add(time.Minute)
		},
		"budget": func(candidate *TaskGrant) {
			candidate.Budget.MaxRows++
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			parent := testGrant(now)
			candidate := testGrant(now)
			mutate(&candidate)
			if err := parent.CheckNarrowing(candidate); !errors.Is(err, ErrGrantExpansion) {
				t.Fatalf("CheckNarrowing error = %v, want ErrGrantExpansion", err)
			}
		})
	}
}

func TestTaskGrantValidateAtExpiry(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	grant := testGrant(now)
	if err := grant.ValidateAt(grant.ExpiresAt); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("ValidateAt expiry error = %v", err)
	}
}
