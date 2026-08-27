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

func TestCarryProfileOnlyRoutesKeepsReviewedNarrowRouteAcrossAFreeze(t *testing.T) {
	defaultRoute := catalog.ApprovalRoute{Sensitivity: domain.SensitivityLow,
		Mode: domain.ApprovalModeManual, Approver: "bob", BudgetProfile: "live-default"}
	profileOnly := catalog.ApprovalRoute{Sensitivity: domain.SensitivityLow, Products: []string{"a", "b", "c"},
		Mode: domain.ApprovalModeManual, Approver: "bob", BudgetProfile: "profile-only-budget"}
	staleDefault := defaultRoute
	staleDefault.BudgetProfile = "stale-default"
	profile := &catalog.Catalog{ApprovalRoutes: []catalog.ApprovalRoute{staleDefault, profileOnly},
		BudgetProfiles: []catalog.BudgetProfile{
			{Name: "stale-default", MaxRows: 500},
			{Name: "profile-only-budget", MaxQueries: 1, MaxRows: 3},
		}}
	live := &catalog.Catalog{ApprovalRoutes: []catalog.ApprovalRoute{defaultRoute},
		BudgetProfiles: []catalog.BudgetProfile{{Name: "live-default", MaxRows: 400_000}}}

	carried := carryProfileOnlyRoutes(live, profile)
	if len(carried.ApprovalRoutes) != 2 || carried.ApprovalRoutes[0].BudgetProfile != "live-default" ||
		carried.ApprovalRoutes[1].BudgetProfile != "profile-only-budget" {
		t.Fatalf("routes = %+v", carried.ApprovalRoutes)
	}
	if len(carried.BudgetProfiles) != 2 || carried.BudgetProfiles[1].MaxQueries != 1 ||
		carried.BudgetProfiles[1].MaxRows != 3 {
		t.Fatalf("budgets = %+v", carried.BudgetProfiles)
	}
	if len(live.ApprovalRoutes) != 1 || len(live.BudgetProfiles) != 1 {
		t.Fatal("live Catalog was mutated")
	}

	// A live route for the same exact Product set wins over the profile copy.
	reviewed := profileOnly
	reviewed.BudgetProfile = "reviewed-wide"
	liveWithRoute := &catalog.Catalog{ApprovalRoutes: []catalog.ApprovalRoute{defaultRoute, reviewed},
		BudgetProfiles: []catalog.BudgetProfile{{Name: "live-default"}, {Name: "reviewed-wide", MaxRows: 100}}}
	carried = carryProfileOnlyRoutes(liveWithRoute, profile)
	if len(carried.ApprovalRoutes) != 2 || carried.ApprovalRoutes[1].BudgetProfile != "reviewed-wide" {
		t.Fatalf("live route was overridden: %+v", carried.ApprovalRoutes)
	}
}
