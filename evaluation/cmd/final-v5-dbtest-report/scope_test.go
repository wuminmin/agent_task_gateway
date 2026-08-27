package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllowancesForScopeSelectsOneHarnessOnly(t *testing.T) {
	compose, scope, err := allowancesForScope("compose-gate")
	if err != nil || scope != composeGateScope {
		t.Fatalf("compose-gate: scope %q err %v", scope, err)
	}
	if len(compose) != len(composeGateAllowedSkips) {
		t.Fatalf("compose-gate selected %d allowances, want %d", len(compose), len(composeGateAllowedSkips))
	}
	for _, allowance := range compose {
		if allowance.Scope != composeGateScope {
			t.Fatalf("compose-gate selected a %q allowance", allowance.Scope)
		}
	}
	db, scope, err := allowancesForScope("db-test-env")
	if err != nil || scope != phase0Scope || len(db) != len(allowedSkips) {
		t.Fatalf("db-test-env: %d allowances, scope %q, err %v", len(db), scope, err)
	}
	if _, _, err := allowancesForScope("everything"); err == nil {
		t.Fatal("an unknown scope was accepted")
	}
}

// Every Compose-gate allowance is a scheduled debt or a checked claim: a
// justification, a scope, a milestone, and either the evidence that already
// covers it or the harness that still must run it -- never both, never neither.
func TestComposeGateAllowancesAreDebtsNotWaivers(t *testing.T) {
	seen := map[[2]string]bool{}
	for _, allowance := range composeGateAllowedSkips {
		name := allowance.Package + " " + allowance.Test
		key := [2]string{allowance.Package, allowance.Test}
		if seen[key] {
			t.Fatalf("%s is declared twice", name)
		}
		seen[key] = true
		if allowance.Category == "" || allowance.ReasonSubstring == "" || allowance.Why == "" ||
			allowance.Scope != composeGateScope || allowance.DeferredUntil == "" {
			t.Fatalf("%s is missing category, reason, why, scope or milestone", name)
		}
		satisfied := allowance.SatisfiedByEvidence != ""
		required := allowance.RequiredExternalGate != ""
		if allowance.DeferredUntil == DeferredSatisfiedNow {
			if !satisfied || required {
				t.Fatalf("%s is satisfied now but names no evidence, or names a gate too", name)
			}
		} else if satisfied || !required {
			t.Fatalf("%s is deferred but names evidence, or no external gate", name)
		}
	}
}

func TestSatisfiedByEvidenceIsCheckedAgainstTheRecordAtThisCommit(t *testing.T) {
	root := t.TempDir()
	commit := "0123456789abcdef0123456789abcdef01234567"
	evidence := filepath.Join(root, "docs", "evidence")
	if err := os.MkdirAll(evidence, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(record map[string]any) {
		payload, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(evidence, "dbtest-suite-0123456.json"), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	claim := SkipRecord{Package: repohygienePackage, Test: "TestRepositoryRootCarriesNoTrackedBuildOutput",
		Declared: true, DeferredUntil: DeferredSatisfiedNow, SatisfiedByEvidence: dbtestSuiteRecordAtCommit}
	deferred := SkipRecord{Package: adapterPackage, Test: "TestAttackAdapterLivePreflight", Declared: true,
		DeferredUntil: DeferredV111PublicationCampaign, RequiredExternalGate: campaignGate}

	if problems := checkSatisfiedEvidence(root, commit, []SkipRecord{claim}); len(problems) != 1 ||
		!strings.Contains(problems[0], "no such file") {
		t.Fatalf("a missing record was accepted: %v", problems)
	}
	write(map[string]any{"commit": "ffffffffffffffffffffffffffffffffffffffff", "accepted": true})
	if problems := checkSatisfiedEvidence(root, commit, []SkipRecord{claim}); len(problems) != 1 ||
		!strings.Contains(problems[0], "records commit") {
		t.Fatalf("a record at another commit was accepted: %v", problems)
	}
	write(map[string]any{"commit": commit, "accepted": false})
	if problems := checkSatisfiedEvidence(root, commit, []SkipRecord{claim}); len(problems) != 1 ||
		!strings.Contains(problems[0], "not an accepted record") {
		t.Fatalf("a rejected record was accepted: %v", problems)
	}
	write(map[string]any{"commit": commit, "accepted": true, "skips": []map[string]string{
		{"package": claim.Package, "test": claim.Test}}})
	if problems := checkSatisfiedEvidence(root, commit, []SkipRecord{claim}); len(problems) != 1 ||
		!strings.Contains(problems[0], "skipped that test as well") {
		t.Fatalf("a record that skipped the same test was accepted: %v", problems)
	}
	write(map[string]any{"commit": commit, "accepted": true, "skips": []map[string]string{}})
	if problems := checkSatisfiedEvidence(root, commit, []SkipRecord{claim, deferred}); len(problems) != 0 {
		t.Fatalf("an accepted record at this commit was rejected: %v", problems)
	}
	if problems := checkSatisfiedEvidence("", commit, []SkipRecord{claim}); len(problems) != 1 {
		t.Fatalf("a claim without an evidence root was accepted: %v", problems)
	}
}
