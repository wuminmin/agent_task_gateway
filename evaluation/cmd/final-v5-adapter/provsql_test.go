package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
)

func provSQLTestOperation(mode string) experiment.AdapterOperation {
	positions := map[string]int{"direct": 1, "provsql": 2, "taskgate": 3}
	return experiment.AdapterOperation{
		SchemaVersion: 1, CampaignClass: "publication", CampaignID: "provsql-campaign",
		DeploymentID: "deployment-01", ExperimentID: "provsql",
		CellID: "nonce-join-group/1k/" + mode, SampleID: "provsql-sample-" + mode,
		Iteration: 1, ProcessReplicate: 1, OrderPosition: positions[mode], RandomSeed: 20260801,
		PairID: "provsql-pair-1", PairedSystemOrder: "direct,provsql,taskgate",
		RootGroupID: "provsql-root-1", WorkloadID: "nonce-join-group", Scale: "1k", Mode: mode,
	}
}

func completeProvSQLTestBinding(t *testing.T) *provSQLDeploymentBinding {
	t.Helper()
	binding := &provSQLDeploymentBinding{
		FixtureVersion: provsqlfixture.Version, FixtureSQLSHA256: provsqlfixture.FixtureSQLSHA256(),
		EnableSQLSHA256: provsqlfixture.EnableSQLSHA256(), DatasetSHA256: provsqlfixture.ExpectedDatasetSHA256(),
		DatasetProbeSQLSHA256:         provsqlfixture.DatasetProbeSQLSHA256(),
		BusinessDatasetProbeSQLSHA256: provsqlfixture.BusinessDatasetProbeSQLSHA256(),
		Task: boundTaskRequest{
			Objective:    "frozen ProvSQL three-arm unit binding",
			DataProducts: []string{"provsql_orders", "provsql_lineitem", "provsql_nonce"},
			Columns: map[string][]string{
				"provsql_orders":   {"orderkey", "status", "partition_key"},
				"provsql_lineitem": {"orderkey", "linenumber", "extendedprice", "partition_key"},
				"provsql_nonce":    {"nonce_id", "partition_key"},
			},
			Scopes:          map[string][]string{"partition_key": {"1"}},
			VisibleRelation: "reporting.provsql_orders", CompanionRelation: "taskgate_ordinal.provsql_orders_v1",
		},
		TaskGate: make(map[string]boundQueryExpectation, 105),
	}
	for _, scale := range []string{"1k", "10k", "45k"} {
		rows, err := provsqlfixture.ExpectedResultRows(scale)
		if err != nil {
			t.Fatal(err)
		}
		resultSHA, err := experiment.CanonicalResultHash(rows)
		if err != nil {
			t.Fatal(err)
		}
		for _, phase := range []struct {
			warmup bool
			count  int
		}{{warmup: true, count: 5}, {warmup: false, count: 30}} {
			for iteration := 1; iteration <= phase.count; iteration++ {
				nonce, err := provsqlfixture.Nonce(scale, 1, iteration, phase.warmup)
				if err != nil {
					t.Fatal(err)
				}
				logical, err := provsqlfixture.LogicalSQL(scale, nonce)
				if err != nil {
					t.Fatal(err)
				}
				key := provSQLBindingKey(scale, nonce)
				binding.TaskGate[key] = boundQueryExpectation{
					SQL: logical, ExpectedRows: provsqlfixture.ExpectedRows,
					ExpectedColumns: provsqlfixture.ExpectedColumns, ExpectedResultSHA256: resultSHA,
					DependencyFacts: int64(1000 + len(binding.TaskGate)), DependencySetSHA256: sha("dependency/" + key),
					ExpectedVisibleCalls: 1, ExpectedCompanionCalls: 1,
				}
			}
		}
	}
	return binding
}

func TestProvSQLVisibleOracleParserIsStrict(t *testing.T) {
	row, err := parseProvSQLVisibleJSON(`[1,"123.40",15,5]`)
	if err != nil || len(row) != 4 || row[0] != int64(1) || row[1] != "123.40" || row[2] != int64(15) || row[3] != int64(5) {
		t.Fatalf("parsed row = %#v, err=%v", row, err)
	}
	for _, invalid := range []string{
		`[1,"123.4",15,5]`, `[1,123.40,15,5]`, `["1","123.40",15,5]`,
		`[1,"123.40",15.5,5]`, `[1,"123.40",15,5,6]`, `[1,"123.40",15,5] {}`,
	} {
		if row, err := parseProvSQLVisibleJSON(invalid); err == nil {
			t.Fatalf("invalid visible oracle was accepted: %q => %#v", invalid, row)
		}
	}
}

func TestProvSQLAggregateRootParserIsStrict(t *testing.T) {
	row, err := parseProvSQLAggregateJSON(`[1,"925b8b69-e91a-59fd-ab97-29f40db3f759","f88c3a84-7e51-5a6b-9e67-6d40af1fe18b","4b84bbf1-a46f-5b1a-940c-12794e36433f"]`)
	if err != nil || row.status != 1 || row.roots[0] != "925b8b69-e91a-59fd-ab97-29f40db3f759" ||
		row.roots[2] != "4b84bbf1-a46f-5b1a-940c-12794e36433f" {
		t.Fatalf("parsed aggregate row = %#v, err=%v", row, err)
	}
	for _, invalid := range []string{
		`[1,"925b8b69-e91a-59fd-ab97-29f40db3f759","f88c3a84-7e51-5a6b-9e67-6d40af1fe18b"]`,
		`["1","925b8b69-e91a-59fd-ab97-29f40db3f759","f88c3a84-7e51-5a6b-9e67-6d40af1fe18b","4b84bbf1-a46f-5b1a-940c-12794e36433f"]`,
		`[1,"not-a-root","f88c3a84-7e51-5a6b-9e67-6d40af1fe18b","4b84bbf1-a46f-5b1a-940c-12794e36433f"]`,
		`[1,"925b8b69-e91a-59fd-ab97-29f40db3f759","f88c3a84-7e51-5a6b-9e67-6d40af1fe18b","4b84bbf1-a46f-5b1a-940c-12794e36433f"] {}`,
	} {
		if row, err := parseProvSQLAggregateJSON(invalid); err == nil {
			t.Fatalf("invalid aggregate root row was accepted: %q => %#v", invalid, row)
		}
	}
}

func TestProvSQLFieldDescriptionsBindEveryCarrierOID(t *testing.T) {
	direct := []pgconn.FieldDescription{
		{Name: "row_json", DataTypeOID: 25}, {Name: "price_provenance", DataTypeOID: 1700},
		{Name: "line_provenance", DataTypeOID: 20}, {Name: "count_provenance", DataTypeOID: 20},
	}
	if err := validateProvSQLFieldDescriptions(direct, false, provSQLSystem{}); err != nil {
		t.Fatal(err)
	}
	prov := []pgconn.FieldDescription{
		{Name: "row_json", DataTypeOID: 25}, {Name: "price_provenance", DataTypeOID: 91000},
		{Name: "line_provenance", DataTypeOID: 91000}, {Name: "count_provenance", DataTypeOID: 91000},
		{Name: "provsql", DataTypeOID: 2950},
	}
	system := provSQLSystem{AggTokenOID: 91000, UUIDOID: 2950}
	if err := validateProvSQLFieldDescriptions(prov, true, system); err != nil {
		t.Fatal(err)
	}
	badName := append([]pgconn.FieldDescription(nil), prov...)
	badName[4].Name = "hidden_token"
	if err := validateProvSQLFieldDescriptions(badName, true, system); err == nil {
		t.Fatal("renamed hidden provenance carrier was accepted")
	}
	badOID := append([]pgconn.FieldDescription(nil), prov...)
	badOID[2].DataTypeOID = 2950
	if err := validateProvSQLFieldDescriptions(badOID, true, system); err == nil {
		t.Fatal("aggregate carrier with UUID OID was accepted as agg_token")
	}
}

func TestProvSQLTaskGateEvidenceEncodesNoFieldOIDsAsAnEmptyArray(t *testing.T) {
	operation := provSQLTestOperation("taskgate")
	spec, err := provsqlfixture.ParseScale(operation.Scale)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := provsqlfixture.Nonce(operation.Scale, operation.ProcessReplicate,
		operation.Iteration, operation.Warmup)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &provSQLAdapter{binding: adapterDeploymentBinding{
		FileSHA256: strings.Repeat("7", 64), SectionSHA256: strings.Repeat("8", 64),
	}, datasetSHA256: provsqlfixture.ExpectedDatasetSHA256()}
	evidence := adapter.provSQLVerification(operation, boundQueryExpectation{}, spec, nonce,
		"taskgate_released_parquet_v8", provSQLSystem{}, provSQLExecution{})
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	fieldOIDs, ok := document["field_oids"].([]any)
	if !ok || len(fieldOIDs) != 0 {
		t.Fatalf("TaskGate field_oids = %#v, want a schema-valid empty array", document["field_oids"])
	}
}

func TestProvSQLBindingRequiresAll105ExactRealCells(t *testing.T) {
	binding := completeProvSQLTestBinding(t)
	if err := validateProvSQLDeploymentBinding(binding); err != nil {
		t.Fatal(err)
	}
	nonce, _ := provsqlfixture.Nonce("45k", 1, 30, false)
	if _, err := validateProvSQLCellBinding(binding, "45k", nonce); err != nil {
		t.Fatal(err)
	}

	missing := completeProvSQLTestBinding(t)
	delete(missing.TaskGate, provSQLBindingKey("1k", 1))
	if err := validateProvSQLDeploymentBinding(missing); err == nil {
		t.Fatal("binding missing one warmup nonce was accepted")
	}
	extra := completeProvSQLTestBinding(t)
	extra.TaskGate["1k/999"] = extra.TaskGate[provSQLBindingKey("1k", 1)]
	if err := validateProvSQLDeploymentBinding(extra); err == nil {
		t.Fatal("binding with an undeclared nonce cell was accepted")
	}
	mutated := completeProvSQLTestBinding(t)
	key := provSQLBindingKey("10k", 401)
	cell := mutated.TaskGate[key]
	cell.SQL += " "
	mutated.TaskGate[key] = cell
	if err := validateProvSQLDeploymentBinding(mutated); err == nil {
		t.Fatal("binding with non-canonical SQL was accepted")
	}
	zeroCalls := completeProvSQLTestBinding(t)
	cell = zeroCalls.TaskGate[key]
	cell.ExpectedVisibleCalls, cell.ExpectedCompanionCalls = 0, 0
	zeroCalls.TaskGate[key] = cell
	if err := validateProvSQLDeploymentBinding(zeroCalls); err == nil {
		t.Fatal("binding without exact observed Business calls was accepted")
	}
}

func TestProvSQLCircuitSequenceRejectsReuseAndRegression(t *testing.T) {
	newAdapter := func() *provSQLAdapter {
		return &provSQLAdapter{sequence: provSQLSequenceState{
			seenNonces: map[int64]bool{}, seenRepresentations: map[string]bool{},
		}}
	}
	first := provSQLExecution{Before: provSQLMetrics{Gates: 10, ArtifactBytes: 100},
		After: provSQLMetrics{Gates: 20, ArtifactBytes: 200}, RepresentationSHA: strings.Repeat("a", 64)}
	second := provSQLExecution{Before: provSQLMetrics{Gates: 20, ArtifactBytes: 200},
		After: provSQLMetrics{Gates: 30, ArtifactBytes: 300}, RepresentationSHA: strings.Repeat("b", 64)}
	adapter := newAdapter()
	if err := adapter.validateAndAdvanceProvSQLSequence(101, first); err != nil {
		t.Fatal(err)
	}
	if err := adapter.validateAndAdvanceProvSQLSequence(102, second); err != nil {
		t.Fatal(err)
	}
	plateau := newAdapter()
	if err := plateau.validateAndAdvanceProvSQLSequence(101, provSQLExecution{
		Before: provSQLMetrics{Gates: 10, ArtifactBytes: 100},
		After:  provSQLMetrics{Gates: 20, ArtifactBytes: 100}, RepresentationSHA: strings.Repeat("c", 64),
	}); err != nil {
		t.Fatalf("real mmap allocation plateau was rejected: %v", err)
	}
	if err := adapter.validateAndAdvanceProvSQLSequence(102, provSQLExecution{
		Before: second.After, After: provSQLMetrics{Gates: 40, ArtifactBytes: 400},
		RepresentationSHA: strings.Repeat("c", 64),
	}); err == nil {
		t.Fatal("reused nonce was accepted")
	}

	for name, invalid := range map[string]provSQLExecution{
		"representation reuse": {Before: first.After, After: second.After, RepresentationSHA: first.RepresentationSHA},
		"no gate growth":       {Before: first.After, After: provSQLMetrics{Gates: 20, ArtifactBytes: 300}, RepresentationSHA: second.RepresentationSHA},
		"byte regression":      {Before: first.After, After: provSQLMetrics{Gates: 30, ArtifactBytes: 199}, RepresentationSHA: second.RepresentationSHA},
		"cross-op regression":  {Before: provSQLMetrics{Gates: 19, ArtifactBytes: 199}, After: second.After, RepresentationSHA: second.RepresentationSHA},
	} {
		one := newAdapter()
		if err := one.validateAndAdvanceProvSQLSequence(101, first); err != nil {
			t.Fatal(err)
		}
		if err := one.validateAndAdvanceProvSQLSequence(102, invalid); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestProvSQLFailureAndInvariantPathsRetainPartialEvidence(t *testing.T) {
	operation := provSQLTestOperation("direct")
	binding := completeProvSQLTestBinding(t)
	nonce, _ := provsqlfixture.Nonce("1k", 1, 1, false)
	expected, err := validateProvSQLCellBinding(binding, "1k", nonce)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := provsqlfixture.ParseScale("1k")
	system := provSQLSystem{PostgreSQLVersion: "16.14", PostgreSQLVersionNum: "160014",
		StatementTimeoutMS: provsqlfixture.StatementTimeout, ClientMinMessages: "error", LogMinMessages: "error", UUIDOID: 2950}
	execution := provSQLExecution{AvailableMS: 1, FullDrainMS: 2, Rows: 2, Columns: 4,
		ResultSHA256: strings.Repeat("f", 64), TypedDrainFields: 8,
		TypedDrainSHA256: strings.Repeat("e", 64), FieldOIDs: []uint32{25, 1700, 20, 20}}
	adapter := &provSQLAdapter{binding: adapterDeploymentBinding{SectionSHA256: strings.Repeat("d", 64)},
		datasetSHA256: provsqlfixture.ExpectedDatasetSHA256()}
	failed := adapter.retainedProvSQLFailure(operation, expected, spec, nonce, "direct_complete_typed_drain",
		system, execution, "external_query_or_drain", "provsql_direct_measurement_failed", errors.New("typed drain failed"))
	if failed.Status != "fail" || failed.ErrorCode != "provsql_direct_measurement_failed" ||
		failed.RowCount != 2 || failed.ResultSHA256 != execution.ResultSHA256 || failed.ProvSQLVerification == nil ||
		failed.ProvSQLVerification.TypedDrainFields != 8 || failed.ProvSQLVerification.FailureStage != "external_query_or_drain" {
		t.Fatalf("real execution partial evidence was discarded: %+v", failed)
	}

	failed.Status = "pass"
	failed.ErrorCode, failed.Reason = "", ""
	failed.Counters = map[string]int64{}
	failed.Counters["retained_marker"] = 7
	invariant := retainedProvSQLInvariantFailure(failed, errors.New("evidence invariant failed"))
	if invariant.Status != "fail" || invariant.ErrorCode != "provsql_evidence_invariant_failed" ||
		invariant.Counters["retained_marker"] != 7 || invariant.ResultSHA256 != execution.ResultSHA256 ||
		invariant.ProvSQLVerification == nil || invariant.ProvSQLVerification.FailureStage != "adapter_evidence_invariant" {
		t.Fatalf("invariant failure discarded measured evidence: %+v", invariant)
	}
	invalid := invalidSample(operation, "provsql_cell_binding_invalid")
	if invalid.Status != "invalid" || invalid.ErrorCode != "provsql_cell_binding_invalid" {
		t.Fatalf("pre-execution invalid cell was not retained distinctly: %+v", invalid)
	}
}

func TestProvSQLConstructorSatisfiesRealFactoryContract(t *testing.T) {
	var factory adapterFactory = newProvSQLAdapter
	if factory == nil {
		t.Fatal("ProvSQL factory is nil")
	}
}
