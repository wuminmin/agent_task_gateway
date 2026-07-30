package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateConfigRequiresWidthsDimensionsAndExactBoundaries(t *testing.T) {
	cfg := validConcurrencyConfig()
	if err := validateConfig(cfg, true); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	t.Run("missing width", func(t *testing.T) {
		value := cfg
		value.Cases = append([]concurrencyCase(nil), cfg.Cases[:3]...)
		if err := validateConfig(value, true); err == nil || !strings.Contains(err.Error(), "concurrency=16") {
			t.Fatalf("missing width error = %v", err)
		}
	})
	t.Run("not B minus one", func(t *testing.T) {
		value := cfg
		value.Cases = append([]concurrencyCase(nil), cfg.Cases...)
		value.Cases[0].BeforeUsed.Release = 0
		if err := validateConfig(value, true); err == nil || !strings.Contains(err.Error(), "exact B-1") {
			t.Fatalf("boundary error = %v", err)
		}
	})
	t.Run("not three dimensional", func(t *testing.T) {
		value := cfg
		value.Cases = append([]concurrencyCase(nil), cfg.Cases...)
		value.Cases[0].BeforeUsed.Outcome = value.Cases[0].AtBudget.Outcome
		if err := validateConfig(value, true); err == nil || !strings.Contains(err.Error(), "all three dimensions") {
			t.Fatalf("three-dimensional error = %v", err)
		}
	})
}

func TestValidateConfigRequiresTwoDistinctContenderGateways(t *testing.T) {
	cfg := validConcurrencyConfig()
	cfg.Gateway.ContenderURLs = cfg.Gateway.ContenderURLs[:1]
	if err := validateConfig(cfg, true); err == nil || !strings.Contains(err.Error(), "exactly two") {
		t.Fatalf("one-replica error = %v", err)
	}
	cfg = validConcurrencyConfig()
	cfg.Gateway.ContenderURLs[1] = cfg.Gateway.ContenderURLs[0] + "/"
	if err := validateConfig(cfg, true); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate-replica error = %v", err)
	}
}

func TestPrepareTemplateRejectsIDsAndRequiresProvisionContract(t *testing.T) {
	cfg := validConcurrencyConfig()
	for index := range cfg.Cases {
		cfg.Cases[index].RootTaskID = ""
		cfg.Cases[index].PrefixTaskID = ""
		cfg.Cases[index].OverflowTaskID = ""
		cfg.Cases[index].ContenderTaskIDs = nil
	}
	if err := validateConfig(cfg, false); err != nil {
		t.Fatalf("valid prepare template: %v", err)
	}
	cfg.Cases[0].RootTaskID = "already-filled"
	if err := validateConfig(cfg, false); err == nil || !strings.Contains(err.Error(), "must not contain task IDs") {
		t.Fatalf("prepared IDs error = %v", err)
	}
}

func TestStrictConfigJSONRejectsDuplicateTrailingAndNonfiniteNumbers(t *testing.T) {
	cfg := validConcurrencyConfig()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded concurrencyConfig
	if err := decodeStrictJSON(raw, &decoded); err != nil {
		t.Fatalf("strict valid decode: %v", err)
	}
	tests := map[string][]byte{
		"duplicate": append([]byte(`{"schema_version":1,`), raw[1:]...),
		"trailing":  append(append([]byte(nil), raw...), []byte(` {}`)...),
		"nonfinite": []byte(strings.Replace(string(raw), `"request_timeout_ms":30000`, `"request_timeout_ms":1e999`, 1)),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			var value concurrencyConfig
			if err := decodeStrictJSON(input, &value); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}

func TestConcurrencyGatesRequireMeasuredWidthsAndRootLockQueues(t *testing.T) {
	cfg := validConcurrencyConfig()
	cells := make([]concurrencyCell, 0, len(cfg.Cases))
	for _, contract := range cfg.Cases {
		cells = append(cells, concurrencyCell{CaseID: contract.ID, Concurrency: contract.Concurrency,
			BoundaryDimension: contract.BoundaryDimension, Status: "measured", Checks: cellChecks{
				SharedRootFamily: true, FreshRoot: true, BMinusOneCommitted: true, BCommitted: true,
				ThreeDimensionalAtomic: true, RootLockQueueObserved: true, OverflowRejected: true,
				FailureLeftNoPartialCommit: true,
			}, Contention: contentionEvidence{RootLockWaitersObserved: contract.Concurrency}})
	}
	if gates := evaluateConcurrencyGates(cfg, cells); gateAcceptance(gates) != "pass" || len(gates) != 36 {
		t.Fatalf("complete evidence did not pass: %#v", gates)
	}
	cells[1].Checks.RootLockQueueObserved = false
	if gates := evaluateConcurrencyGates(cfg, cells); gateAcceptance(gates) != "fail" {
		t.Fatalf("missing root-lock queue passed: %#v", gates)
	}
}

func TestConcurrencyReportSchemaTwoUsesOnlyObservedLockQueueFields(t *testing.T) {
	if concurrencyReportSchema != 2 {
		t.Fatalf("report schema = %d, want 2", concurrencyReportSchema)
	}
	raw, err := json.Marshal(concurrencyReport{SchemaVersion: concurrencyReportSchema,
		Cells: []concurrencyCell{{Contention: contentionEvidence{RootLockWaitersObserved: 4},
			Checks: cellChecks{RootLockQueueObserved: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{`"schema_version":2`, `"root_lock_waiters_observed":4`,
		`"root_lock_queue_observed":true`} {
		if !strings.Contains(text, required) {
			t.Fatalf("report JSON missing %s: %s", required, text)
		}
	}
	for _, forbidden := range []string{"cas_waiters_observed", "minimum_cas_conflicts",
		"minimum_successful_retries", "cas_retry_proven", "cas_retry_observed"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("report JSON retained inferred field %q: %s", forbidden, text)
		}
	}
}

func TestCampaignFinishedAtUsesNonnegativeElapsedTime(t *testing.T) {
	cfg := validConcurrencyConfig()
	t.Setenv(cfg.Gateway.TokenEnv, "")
	t.Setenv(cfg.ControlDSNEnv, "")
	report, err := runConcurrencyCampaign(context.Background(), cfg, []byte(`{"schema_version":1}`))
	if err == nil {
		t.Fatal("preflight unexpectedly succeeded without credentials")
	}
	if report.StartedAt.IsZero() || report.FinishedAt.IsZero() {
		t.Fatalf("campaign timestamps were not populated: started=%v finished=%v",
			report.StartedAt, report.FinishedAt)
	}
	if report.FinishedAt.Before(report.StartedAt) {
		t.Fatalf("campaign finished before it started: started=%v finished=%v",
			report.StartedAt, report.FinishedAt)
	}
}

func validConcurrencyConfig() concurrencyConfig {
	cfg := concurrencyConfig{SchemaVersion: concurrencyConfigSchema,
		Gateway: gatewayConfig{URL: "http://gateway:8082",
			ContenderURLs: []string{"http://gateway:8082", "http://gateway-peer:8082"}, TokenEnv: "TOKEN"},
		ControlDSNEnv: "CONTROL_DSN", RequestTimeoutMS: 30000, LockWaitTimeoutMS: 5000,
		Provision: &provisionConfig{OAURL: "http://oa:8092", AlicePasswordEnv: "ALICE_PASSWORD",
			BobPasswordEnv: "BOB_PASSWORD", DataProducts: []string{"expense_detail"},
			Columns: map[string][]string{"expense_detail": {"receipt_no", "amount", "city"}},
			Scopes:  map[string]any{"department": []string{"销售部"}}},
	}
	levels := []int{1, 4, 8, 16}
	dimensions := []string{"release", "influence", "outcome", "release"}
	for caseIndex, level := range levels {
		one := concurrencyCase{ID: "case-" + dimensions[caseIndex] + "-c" + string(rune('a'+caseIndex)),
			Concurrency: level, BoundaryDimension: dimensions[caseIndex],
			RootTaskID:     "root-" + string(rune('a'+caseIndex)),
			PrefixTaskID:   "prefix-" + string(rune('a'+caseIndex)),
			OverflowTaskID: "overflow-" + string(rune('a'+caseIndex)),
			PrefixPlan:     json.RawMessage(`{"product":"expense_detail","columns":["receipt_no"]}`),
			ContenderPlan:  json.RawMessage(`{"product":"expense_detail","columns":["receipt_no","amount"]}`),
			OverflowPlan:   json.RawMessage(`{"product":"expense_detail","columns":["receipt_no","amount","city"]}`),
			BeforeUsed:     exposureCounts{Release: 1, Influence: 2, Outcome: 1},
			AtBudget:       exposureCounts{Release: 2, Influence: 3, Outcome: 2}}
		for index := 0; index < level; index++ {
			one.ContenderTaskIDs = append(one.ContenderTaskIDs,
				"contender-"+string(rune('a'+caseIndex))+"-"+string(rune('a'+index)))
		}
		cfg.Cases = append(cfg.Cases, one)
	}
	return cfg
}
