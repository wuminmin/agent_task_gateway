package finalv5contracts

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

func TestVerifyRepository(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRepository(root); err != nil {
		t.Fatal(err)
	}
}

func TestStrictJSONRejectsDuplicateUnknownAndTrailingContent(t *testing.T) {
	type document struct {
		Value int `json:"value"`
	}
	tests := []string{
		`{"value":1,"value":2}`,
		`{"value":1,"unknown":2}`,
		`{"value":1} {"value":2}`,
	}
	for _, input := range tests {
		var target document
		if err := decodeStrictJSON([]byte(input), &target); err == nil {
			t.Fatalf("decodeStrictJSON accepted invalid input %s", input)
		}
	}
}

func TestWorkloadProtocolExpansionRejectsDuplicateAndIncompleteCells(t *testing.T) {
	for name, input := range map[string]string{
		"duplicate cell": `
schema_version: 2
profiles:
  provsql:
    workloads:
      - id: nonce-join-group
        scales: [1k, 1k]
        modes: [taskgate]
`,
		"missing scales": `
schema_version: 2
profiles:
  provsql:
    workloads:
      - id: nonce-join-group
        scales: []
        modes: [taskgate]
`,
		"empty mode": `
schema_version: 2
profiles:
  provsql:
    workloads:
      - id: nonce-join-group
        scales: [1k]
        modes: [""]
`,
	} {
		t.Run(name, func(t *testing.T) {
			protocol, err := decodeWorkloadProtocol(bytes.NewBufferString(input))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := expandProtocolProfile("provsql", protocol); err == nil {
				t.Fatal("invalid protocol profile was accepted")
			}
		})
	}
}

func TestWorkloadProtocolDecodeRejectsUnknownAndTrailingYAML(t *testing.T) {
	for name, input := range map[string]string{
		"unknown field":     "schema_version: 2\nprofiles: {}\nunknown: true\n",
		"trailing document": "schema_version: 2\nprofiles: {}\n---\nschema_version: 2\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeWorkloadProtocol(bytes.NewBufferString(input)); err == nil {
				t.Fatal("invalid workload protocol was accepted")
			}
		})
	}
}

func TestDependencyAlgebraRejectsMutation(t *testing.T) {
	scale := loadScaleForTest(t)
	mutated := false
	for index := range scale.Cells {
		if scale.Cells[index].Workload != "dependency-e2e" {
			continue
		}
		expected, err := decodeRawObject(scale.Cells[index].Expected)
		if err != nil {
			t.Fatal(err)
		}
		expected["union_dependency_cardinality"] = mustInt(expected, "union_dependency_cardinality") + 1
		scale.Cells[index].Expected = marshalForTest(t, expected)
		mutated = true
		break
	}
	if !mutated {
		t.Fatal("dependency cell not found")
	}
	if err := validateScale(scale); err == nil || !strings.Contains(err.Error(), "N/K/2N-K") {
		t.Fatalf("dependency union mutation was not rejected by the algebra invariant: %v", err)
	}
}

func TestOutcomeScheduleRejectsX1Mutation(t *testing.T) {
	scale := loadScaleForTest(t)
	mutated := false
	for index := range scale.Cells {
		current := &scale.Cells[index]
		if current.Workload != "outcome-merkle" {
			continue
		}
		query, err := decodeRawObject(current.Query)
		if err != nil {
			t.Fatal(err)
		}
		parameters, _ := objectValue(query, "parameters")
		if mustInt(parameters, "candidate_cardinality") != 1 || mustInt(parameters, "overlap_percent") != 50 {
			continue
		}
		expected, err := decodeRawObject(current.Expected)
		if err != nil {
			t.Fatal(err)
		}
		expected["x1_samples_with_overlap"] = int64(16)
		current.Expected = marshalForTest(t, expected)
		mutated = true
		break
	}
	if !mutated {
		t.Fatal("Outcome x1/o50 cell not found")
	}
	if err := validateScale(scale); err == nil || !strings.Contains(err.Error(), "0/15/27/30") {
		t.Fatalf("Outcome x1 mutation was not rejected by the schedule invariant: %v", err)
	}
}

func TestOutcomeScheduleRejectsIncludedWarmups(t *testing.T) {
	scale := loadScaleForTest(t)
	for index := range scale.Cells {
		current := &scale.Cells[index]
		if current.Workload != "outcome-merkle" {
			continue
		}
		measured, err := decodeRawObject(current.Measured)
		if err != nil {
			t.Fatal(err)
		}
		measured["include_warmups"] = true
		current.Measured = marshalForTest(t, measured)
		if err := validateScale(scale); err == nil || !strings.Contains(err.Error(), "warmups") {
			t.Fatalf("included warmups were not rejected: %v", err)
		}
		return
	}
	t.Fatal("Outcome cell not found")
}

func TestArtifactIdentityRejectsBaselineShapeMutation(t *testing.T) {
	root := repositoryRootForTest(t)
	baseline, _, err := loadBaseline(filepath.Join(root, "evaluation", "final-v5-wsl2", "contracts", "baseline-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	artifact, _, err := loadArtifact(filepath.Join(root, "evaluation", "final-v5-wsl2", "contracts", "artifact-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	query, err := decodeRawObject(artifact.Cells[0].Query)
	if err != nil {
		t.Fatal(err)
	}
	parameters, ok := objectValue(query, "parameters")
	if !ok {
		t.Fatal("Artifact query parameters missing")
	}
	parameters["rows"] = mustInt(parameters, "rows") + 1
	artifact.Cells[0].Query = marshalForTest(t, query)
	if err := validateArtifactIdentity(baseline, artifact); err == nil || !strings.Contains(err.Error(), "Baseline S6") {
		t.Fatalf("Artifact/Baseline identity mutation was not rejected: %v", err)
	}
}

func TestJSONReferenceIsRejected(t *testing.T) {
	if err := rejectJSONReference([]byte(`{"nested":{"$ref":"draft.json#/cell"}}`), "$ref"); err == nil {
		t.Fatal("$ref was accepted")
	}
}

func TestCatalogCandidateTaskPolicies(t *testing.T) {
	path := filepath.Join(repositoryRootForTest(t), "evaluation", "final-v5-wsl2", "catalog", "benchmark-contract-v1.yaml")
	candidate, err := catalog.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCatalogTaskPolicies(candidate); err != nil {
		t.Fatal(err)
	}
}

func TestBaselineOrderingPolicyRejectsMutations(t *testing.T) {
	baseline := loadBaselineForTest(t)
	if err := validateBaselineOrdering(baseline); err != nil {
		t.Fatal(err)
	}
	t.Run("canonical workload cannot claim query order", func(t *testing.T) {
		mutated := baseline
		mutated.Cells = append([]cell(nil), baseline.Cells...)
		for index := range mutated.Cells {
			if mutated.Cells[index].Workload != "S2" {
				continue
			}
			query, err := decodeRawObject(mutated.Cells[index].Query)
			if err != nil {
				t.Fatal(err)
			}
			query["total_order_required"] = true
			delete(query, "result_ordering")
			mutated.Cells[index].Query = marshalForTest(t, query)
			if err := validateBaselineOrdering(mutated); err == nil || !strings.Contains(err.Error(), "canonical_typed_row_lexicographic_v1") {
				t.Fatalf("S2 query-order mutation was not rejected: %v", err)
			}
			return
		}
		t.Fatal("S2 cell not found")
	})
	t.Run("query-order workload cannot disable total order", func(t *testing.T) {
		mutated := baseline
		mutated.Cells = append([]cell(nil), baseline.Cells...)
		for index := range mutated.Cells {
			if mutated.Cells[index].Workload != "S1" {
				continue
			}
			query, err := decodeRawObject(mutated.Cells[index].Query)
			if err != nil {
				t.Fatal(err)
			}
			query["total_order_required"] = false
			query["result_ordering"] = "canonical_typed_row_lexicographic_v1"
			mutated.Cells[index].Query = marshalForTest(t, query)
			if err := validateBaselineOrdering(mutated); err == nil || !strings.Contains(err.Error(), "query order") {
				t.Fatalf("S1 canonical-order mutation was not rejected: %v", err)
			}
			return
		}
		t.Fatal("S1 cell not found")
	})
	t.Run("query-order workload cannot omit its ordering mode", func(t *testing.T) {
		mutated := baseline
		mutated.Cells = append([]cell(nil), baseline.Cells...)
		for index := range mutated.Cells {
			if mutated.Cells[index].Workload != "S1" {
				continue
			}
			query, err := decodeRawObject(mutated.Cells[index].Query)
			if err != nil {
				t.Fatal(err)
			}
			delete(query, "result_ordering")
			mutated.Cells[index].Query = marshalForTest(t, query)
			if err := validateBaselineOrdering(mutated); err == nil || !strings.Contains(err.Error(), "explicit query order") {
				t.Fatalf("missing S1 ordering mode was not rejected: %v", err)
			}
			return
		}
		t.Fatal("S1 cell not found")
	})
}

func TestScaleScheduleVersionEqualsOracleConstant(t *testing.T) {
	scale := loadScaleForTest(t)
	design, err := decodeRawObject(scale.OutcomeMerkleDesign)
	if err != nil {
		t.Fatal(err)
	}
	if version := stringValue(design, "schedule_version"); version != finalv5oracle.OutcomeOverlapScheduleVersion {
		t.Fatalf("Scale schedule version %q differs from Oracle %q", version, finalv5oracle.OutcomeOverlapScheduleVersion)
	}
	for _, current := range scale.Cells {
		if current.Workload != "outcome-merkle" {
			continue
		}
		query, err := decodeRawObject(current.Query)
		if err != nil {
			t.Fatal(err)
		}
		parameters, ok := objectValue(query, "parameters")
		if !ok || stringValue(parameters, "schedule_version") != finalv5oracle.OutcomeOverlapScheduleVersion {
			t.Fatalf("Scale cell %s does not bind the Oracle schedule constant", current.Scale)
		}
	}
}

func loadScaleForTest(t *testing.T) scaleDocument {
	t.Helper()
	root := repositoryRootForTest(t)
	document, _, err := loadScale(filepath.Join(root, "evaluation", "final-v5-wsl2", "contracts", "scale-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func loadBaselineForTest(t *testing.T) baselineDocument {
	t.Helper()
	root := repositoryRootForTest(t)
	document, _, err := loadBaseline(filepath.Join(root, "evaluation", "final-v5-wsl2", "contracts", "baseline-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func repositoryRootForTest(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func marshalForTest(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
