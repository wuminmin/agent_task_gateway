package main

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/domain"
)

func TestRefreshMatchingRouteBudgets(t *testing.T) {
	matched := catalog.ApprovalRoute{Sensitivity: domain.SensitivityLow, Products: []string{"shared"},
		Mode: domain.ApprovalModeManual, Approver: "bob", BudgetProfile: "shared-budget"}
	profileOnly := catalog.ApprovalRoute{Sensitivity: domain.SensitivityLow, Products: []string{"a", "b"},
		Mode: domain.ApprovalModeManual, Approver: "bob", BudgetProfile: "profile-only-budget"}
	profile := &catalog.Catalog{ApprovalRoutes: []catalog.ApprovalRoute{matched, profileOnly},
		BudgetProfiles: []catalog.BudgetProfile{
			{Name: "shared-budget", MaxRows: 16},
			{Name: "profile-only-budget", MaxRows: 3},
		}}
	live := &catalog.Catalog{ApprovalRoutes: []catalog.ApprovalRoute{matched},
		BudgetProfiles: []catalog.BudgetProfile{{Name: "shared-budget", MaxRows: 200_000}}}

	refreshed := refreshMatchingRouteBudgets(profile, live)
	if refreshed.BudgetProfiles[0].MaxRows != 200_000 {
		t.Fatalf("matched route retained %d rows", refreshed.BudgetProfiles[0].MaxRows)
	}
	if refreshed.BudgetProfiles[1].MaxRows != 3 {
		t.Fatalf("profile-only route moved to %d rows", refreshed.BudgetProfiles[1].MaxRows)
	}
	if profile.BudgetProfiles[0].MaxRows != 16 {
		t.Fatalf("source profile was mutated to %d rows", profile.BudgetProfiles[0].MaxRows)
	}
}
