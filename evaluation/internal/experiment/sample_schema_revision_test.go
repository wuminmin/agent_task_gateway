package experiment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	jsonschema "github.com/google/jsonschema-go/jsonschema"

	"taskbound.local/agent-data-gateway/internal/preparedbinding"
)

func TestSampleV1SchemaAndTrackedEvidenceRemainByteCompatible(t *testing.T) {
	const schemaSHA256 = "e94f45981019d4e028c0011adf0100fee3058e0635d01e88fa48a34f40dcf40e"
	schemaPath := filepath.Join("..", "..", "final-v5-wsl2", "schema", "sample-v1.schema.json")
	got, err := FileSHA256(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != schemaSHA256 {
		t.Fatalf("sample-v1 schema SHA-256 = %s, want frozen %s", got, schemaSHA256)
	}

	tests := []struct {
		name   string
		dir    string
		sha256 string
	}{
		{
			name:   "run03",
			dir:    "targeted-p33-artifact-100x4-03-20260809T181354Z-0dc072f9e6be",
			sha256: "efa58673b88b930ffddeaac7398bccb0978fac3316ebc5dd85c181b2cb0c00c1",
		},
		{
			name:   "run04",
			dir:    "targeted-p33-artifact-100x4-04-20260810T014910Z-9316682fa30c",
			sha256: "3ad230ec470581be9def8fc510e578e173207589309320e751878c7e23b7d283",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join("..", "..", "final-v5-wsl2", "raw", test.dir, "raw", "deployment-01.jsonl")
			got, err := FileSHA256(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.sha256 {
				t.Fatalf("retained evidence SHA-256 = %s, want frozen %s", got, test.sha256)
			}
			samples, err := ReadSamples([]string{path})
			if err != nil {
				t.Fatalf("ReadSamples rejected retained v1 evidence: %v", err)
			}
			if len(samples) != 1 {
				t.Fatalf("ReadSamples returned %d samples, want 1", len(samples))
			}
			if samples[0].SchemaVersion != SampleSchemaVersion || samples[0].TaskGateRejectionV1 != nil {
				t.Fatalf("retained sample decoded as schema_version=%d rejection=%v, want unchanged v1", samples[0].SchemaVersion, samples[0].TaskGateRejectionV1 != nil)
			}
		})
	}
}

func TestSampleV1RejectsTaskGateRejectionV1(t *testing.T) {
	instance := validSampleV2JSON(t)
	instance["schema_version"] = float64(SampleSchemaVersion)

	if err := sampleSchemaValidator(t)(instance); err == nil {
		t.Fatal("sample-v1 JSON Schema accepted the v2-only taskgate_rejection_v1 field")
	}
	encoded, err := json.Marshal(instance)
	if err != nil {
		t.Fatal(err)
	}
	var sample Sample
	if err := StrictJSON(encoded, &sample); err != nil {
		t.Fatalf("strict decoder could not exercise the version-aware read path: %v", err)
	}
	if err := sample.Validate(); err == nil {
		t.Fatal("sample-v1 Go validation accepted the v2-only taskgate_rejection_v1 field")
	}

	instance["taskgate_rejection_v1"] = nil
	if err := sampleSchemaValidator(t)(instance); err == nil {
		t.Fatal("sample-v1 JSON Schema treated an explicit null rejection as absence")
	}
	encoded, err = json.Marshal(instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := StrictJSON(encoded, &sample); err == nil {
		t.Fatal("sample-v1 Go decoder treated an explicit null rejection as absence")
	}
}

func TestSampleV2AcceptsOnlyFinalizerRejectedFailures(t *testing.T) {
	validate := sampleV2SchemaValidator(t)
	valid := validSampleV2JSON(t)
	if err := validate(valid); err != nil {
		t.Fatalf("sample-v2 rejected a valid finalizer rejection: %v", err)
	}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	var sample Sample
	if err := StrictJSON(encoded, &sample); err != nil {
		t.Fatalf("strict decoder rejected schema-valid sample-v2: %v", err)
	}
	if err := sample.Validate(); err != nil {
		t.Fatalf("Go validation rejected schema-valid sample-v2: %v", err)
	}

	tests := map[string]func(map[string]any){
		"missing rejection": func(value map[string]any) {
			delete(value, "taskgate_rejection_v1")
		},
		"pass": func(value map[string]any) {
			value["status"] = "pass"
		},
		"invalid status": func(value map[string]any) {
			value["status"] = "invalid"
		},
		"acceptance": func(value map[string]any) {
			value["taskgate_acceptance_v3"] = map[string]any{}
		},
		"null acceptance": func(value map[string]any) {
			value["taskgate_acceptance_v3"] = nil
		},
		"null rejection": func(value map[string]any) {
			value["taskgate_rejection_v1"] = nil
		},
		"unknown top-level field": func(value map[string]any) {
			value["future_sample_member"] = true
		},
		"unknown rejection field": func(value map[string]any) {
			rejectionObject(t, value)["raw_stderr"] = "SENTINEL_REJECTION_SECRET"
		},
		"unknown phase": func(value map[string]any) {
			rejectionObject(t, value)["phase"] = "future_phase"
		},
		"phase gate mismatch": func(value map[string]any) {
			rejectionObject(t, value)["gate_code"] = "unexpected_structural_statements"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			instance := validSampleV2JSON(t)
			mutate(instance)
			if err := validate(instance); err == nil {
				t.Fatal("sample-v2 JSON Schema accepted the forbidden shape")
			}
			encoded, err := json.Marshal(instance)
			if err != nil {
				t.Fatal(err)
			}
			var sample Sample
			if err := StrictJSON(encoded, &sample); err == nil {
				if err := sample.Validate(); err == nil {
					t.Fatal("sample-v2 Go validation accepted the forbidden shape")
				}
			}
		})
	}
}

func TestSampleV2DifferenceUnionIsClosedAndTyped(t *testing.T) {
	validate := sampleV2SchemaValidator(t)
	valid := validSampleV2JSON(t)
	rejection := rejectionObject(t, valid)
	rejection["phase"] = "closed_world_accounting"
	rejection["gate_code"] = "observer_total"
	rejection["differences"] = []any{
		map[string]any{"field": "expected_count", "count": float64(0)},
	}
	if err := validate(valid); err != nil {
		t.Fatalf("sample-v2 rejected a valid bounded count difference: %v", err)
	}

	tests := map[string]func(map[string]any){
		"unknown field": func(value map[string]any) {
			differenceObject(t, value)["sql"] = "SELECT SENTINEL_REJECTION_SECRET"
		},
		"no typed value": func(value map[string]any) {
			delete(differenceObject(t, value), "count")
		},
		"two typed values": func(value map[string]any) {
			differenceObject(t, value)["bool"] = false
		},
		"negative count": func(value map[string]any) {
			differenceObject(t, value)["count"] = float64(-1)
		},
		"count above int64 bound": func(value map[string]any) {
			differenceObject(t, value)["count"] = float64(1e20)
		},
		"null extra union arm": func(value map[string]any) {
			differenceObject(t, value)["bool"] = nil
		},
		"wrong value kind": func(value map[string]any) {
			difference := differenceObject(t, value)
			delete(difference, "count")
			difference["lowercase_sha256"] = strings.Repeat("a", 64)
		},
		"ordinal on aggregate": func(value map[string]any) {
			differenceObject(t, value)["ordinal"] = float64(0)
		},
		"uppercase SHA-256": func(value map[string]any) {
			difference := differenceObject(t, value)
			difference["field"] = "expected_sha256"
			delete(difference, "count")
			difference["lowercase_sha256"] = strings.Repeat("A", 64)
		},
		"unknown prepared member": func(value map[string]any) {
			difference := differenceObject(t, value)
			difference["field"] = "prepared_member"
			delete(difference, "count")
			difference["enum"] = "operation_id"
		},
		"unexpected statement without ordinal": func(value map[string]any) {
			differenceObject(t, value)["field"] = "unexpected_statement_calls"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			instance := validSampleV2JSON(t)
			rejection := rejectionObject(t, instance)
			rejection["phase"] = "closed_world_accounting"
			rejection["gate_code"] = "observer_total"
			rejection["differences"] = []any{
				map[string]any{"field": "expected_count", "count": float64(0)},
			}
			mutate(instance)
			if err := validate(instance); err == nil {
				t.Fatal("sample-v2 JSON Schema accepted an open or ill-typed difference")
			}
		})
	}
}

func TestSampleV2SchemaRequiresDetailedRejectionShapes(t *testing.T) {
	validate := sampleV2SchemaValidator(t)

	t.Run("prepared members", func(t *testing.T) {
		instance := validSampleV2JSON(t)
		rejection := rejectionObject(t, instance)
		rejection["phase"] = "execution_reproduction"
		rejection["gate_code"] = "prepared_binding_members"
		rejection["differences"] = []any{}
		if err := validate(instance); err == nil {
			t.Fatal("sample-v2 schema accepted a prepared mismatch without member names")
		}
	})

	t.Run("candidate census", func(t *testing.T) {
		instance := validSampleV2JSON(t)
		rejection := rejectionObject(t, instance)
		rejection["phase"] = "contract_identification"
		rejection["gate_code"] = "candidate_match_count"
		rejection["failure_kind"] = "mismatch"
		rejection["differences"] = []any{
			map[string]any{"field": "candidate_count", "count": float64(1)},
			map[string]any{"field": "matched_candidate_count", "count": float64(0)},
		}
		if err := validate(instance); err == nil {
			t.Fatal("sample-v2 schema accepted an incomplete zero-match census")
		}
	})

	t.Run("unexpected structural triples", func(t *testing.T) {
		instance := validSampleV2JSON(t)
		rejection := rejectionObject(t, instance)
		rejection["phase"] = "closed_world_accounting"
		rejection["gate_code"] = "unexpected_structural_statements"
		rejection["failure_kind"] = "mismatch"
		rejection["differences"] = []any{
			map[string]any{"field": "actual_count", "count": float64(1)},
			map[string]any{"field": "expected_count", "count": float64(0)},
			map[string]any{"field": "unexpected_statement_calls", "ordinal": float64(0), "count": float64(2)},
			map[string]any{"field": "unexpected_statement_sha256", "ordinal": float64(0), "lowercase_sha256": strings.Repeat("a", 64)},
			map[string]any{"field": "unexpected_statement_toplevel", "ordinal": float64(0), "bool": true},
		}
		if err := validate(instance); err != nil {
			t.Fatalf("sample-v2 schema rejected a complete structural-key triple: %v", err)
		}
		encoded, err := json.Marshal(instance)
		if err != nil {
			t.Fatal(err)
		}
		var sample Sample
		if err := StrictJSON(encoded, &sample); err != nil {
			t.Fatalf("Go decoder rejected schema-valid structural-key evidence: %v", err)
		}
		if err := sample.Validate(); err != nil {
			t.Fatalf("Go validation rejected complete structural-key evidence: %v", err)
		}

		differences := rejection["differences"].([]any)
		rejection["differences"] = differences[:len(differences)-1]
		if err := validate(instance); err == nil {
			t.Fatal("sample-v2 schema accepted an incomplete structural-key triple")
		}
		encoded, err = json.Marshal(instance)
		if err != nil {
			t.Fatal(err)
		}
		if err := StrictJSON(encoded, &sample); err == nil {
			t.Fatal("Go decoder accepted an incomplete structural-key triple")
		}
	})
}

// Standard JSON Schema closes fields, enums, tagged value types, and the
// required presence of each detailed family. Relations between values in two
// array elements (count equality, per-ordinal triples, logical-key uniqueness)
// are closed by the Go evidence reader. These cases prove that second semantic
// layer is mandatory rather than silently treating schema success as enough.
func TestSampleV2GoReaderClosesCrossDifferenceRelations(t *testing.T) {
	validate := sampleV2SchemaValidator(t)
	tests := map[string]func(map[string]any){
		"candidate count equality": func(instance map[string]any) {
			rejection := rejectionObject(t, instance)
			rejection["phase"] = "contract_identification"
			rejection["gate_code"] = "candidate_match_count"
			rejection["failure_kind"] = "mismatch"
			rejection["differences"] = []any{
				map[string]any{"field": "candidate_count", "count": float64(2)},
				map[string]any{"field": "matched_candidate_count", "count": float64(0)},
				map[string]any{"field": "refused_candidate_count", "count": float64(1)},
			}
		},
		"per-ordinal structural triple": func(instance map[string]any) {
			rejection := rejectionObject(t, instance)
			rejection["phase"] = "closed_world_accounting"
			rejection["gate_code"] = "unexpected_structural_statements"
			rejection["failure_kind"] = "mismatch"
			rejection["differences"] = []any{
				map[string]any{"field": "actual_count", "count": float64(1)},
				map[string]any{"field": "expected_count", "count": float64(0)},
				map[string]any{"field": "unexpected_statement_calls", "ordinal": float64(0), "count": float64(1)},
				map[string]any{"field": "unexpected_statement_sha256", "ordinal": float64(0), "lowercase_sha256": strings.Repeat("b", 64)},
				map[string]any{"field": "unexpected_statement_toplevel", "ordinal": float64(1), "bool": true},
			}
		},
		"logical difference key": func(instance map[string]any) {
			rejection := rejectionObject(t, instance)
			rejection["phase"] = "closed_world_accounting"
			rejection["gate_code"] = "observer_total"
			rejection["failure_kind"] = "mismatch"
			rejection["differences"] = []any{
				map[string]any{"field": "expected_count", "count": float64(1)},
				map[string]any{"field": "expected_count", "count": float64(2)},
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			instance := validSampleV2JSON(t)
			mutate(instance)
			if err := validate(instance); err != nil {
				t.Fatalf("structural JSON Schema unexpectedly rejected the semantic fixture: %v", err)
			}
			encoded, err := json.Marshal(instance)
			if err != nil {
				t.Fatal(err)
			}
			var sample Sample
			if err := StrictJSON(encoded, &sample); err == nil {
				t.Fatal("Go evidence reader accepted a cross-difference contradiction")
			}
		})
	}
}

func TestSampleV2SchemaMatchesClosedRejectionEnums(t *testing.T) {
	schema := readSampleV2SchemaObject(t)
	topProperties := objectMap(t, schema["properties"], "sample-v2 properties")
	if _, present := topProperties["taskgate_acceptance_v3"]; present {
		t.Fatal("sample-v2 schema exposes taskgate_acceptance_v3")
	}
	if got := objectMap(t, topProperties["status"], "sample-v2 status")["const"]; got != "fail" {
		t.Fatalf("sample-v2 status const = %v, want fail", got)
	}
	if !containsString(stringArray(t, schema["required"], "sample-v2 required"), "taskgate_rejection_v1") {
		t.Fatal("sample-v2 does not require taskgate_rejection_v1")
	}

	definitions := objectMap(t, schema["$defs"], "sample-v2 definitions")
	rejection := objectMap(t, definitions["taskgate_rejection_v1"], "taskgate_rejection_v1 definition")
	difference := objectMap(t, definitions["taskgate_rejection_difference_v1"], "difference definition")
	for name, object := range map[string]map[string]any{
		"sample-v2":                     schema,
		"taskgate_rejection_v1":         rejection,
		"taskgate rejection difference": difference,
	} {
		if object["additionalProperties"] != false {
			t.Fatalf("%s is not closed with additionalProperties=false", name)
		}
	}

	rejectionProperties := objectMap(t, rejection["properties"], "taskgate_rejection_v1 properties")
	differenceProperties := objectMap(t, difference["properties"], "difference properties")
	preparedMembers := make([]string, 0, len(preparedbinding.PreparedOperationMembers()))
	for _, member := range preparedbinding.PreparedOperationMembers() {
		preparedMembers = append(preparedMembers, member.Code())
	}

	tests := []struct {
		name       string
		properties map[string]any
		property   string
		goValues   []string
	}{
		{"phase", rejectionProperties, "phase", closedWireNames(rejectionPhaseNames[:])},
		{"gate", rejectionProperties, "gate_code", closedWireNames(rejectionGateNames[:])},
		{"failure kind", rejectionProperties, "failure_kind", closedWireNames(rejectionFailureKindNames[:])},
		{"expected source", rejectionProperties, "expected_source", closedWireNames(rejectionSourceNames[:])},
		{"actual source", rejectionProperties, "actual_source", closedWireNames(rejectionSourceNames[:])},
		{"path kind", rejectionProperties, "path_kind", closedWireNames(rejectionPathNames[:])},
		{"target role", rejectionProperties, "target_role", closedWireNames(rejectionTargetRoleNames[:])},
		{"statement class", rejectionProperties, "statement_class", closedWireNames(rejectionStatementClassNames[:])},
		{"difference field", differenceProperties, "field", closedWireNames(rejectionDifferenceFieldNames[:])},
		{"prepared member", differenceProperties, "enum", preparedMembers},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schemaValues := stringArray(t, objectMap(t, test.properties[test.property], test.property)["enum"], test.property+" enum")
			want := append([]string(nil), test.goValues...)
			sort.Strings(schemaValues)
			sort.Strings(want)
			if !reflect.DeepEqual(schemaValues, want) {
				t.Fatalf("schema enum = %v, Go enum = %v", schemaValues, want)
			}
		})
	}

	schemaGatePhases := make(map[string]string)
	branches, ok := rejection["oneOf"].([]any)
	if !ok {
		t.Fatal("taskgate_rejection_v1 oneOf is not an array")
	}
	for index, rawBranch := range branches {
		branch := objectMap(t, rawBranch, "phase/gate branch")
		properties := objectMap(t, branch["properties"], "phase/gate branch properties")
		phase, ok := objectMap(t, properties["phase"], "phase const")["const"].(string)
		if !ok || phase == "" {
			t.Fatalf("phase/gate branch %d has no closed phase const", index)
		}
		for _, gate := range stringArray(t, objectMap(t, properties["gate_code"], "gate enum")["enum"], "gate enum") {
			if previous, duplicate := schemaGatePhases[gate]; duplicate {
				t.Fatalf("gate %s belongs to both %s and %s in sample-v2 schema", gate, previous, phase)
			}
			schemaGatePhases[gate] = phase
		}
	}
	for gate := rejectionGate(1); gate < rejectionGateCount; gate++ {
		gateName := enumName(rejectionGateNames[:], int(gate))
		phaseName := enumName(rejectionPhaseNames[:], int(phaseForRejectionGate(gate)))
		if got := schemaGatePhases[gateName]; got != phaseName {
			t.Fatalf("sample-v2 schema maps gate %s to phase %q, Go maps it to %q", gateName, got, phaseName)
		}
		delete(schemaGatePhases, gateName)
	}
	if len(schemaGatePhases) != 0 {
		t.Fatalf("sample-v2 schema maps gates absent from Go: %v", sortedKeysOfStringMap(schemaGatePhases))
	}
}

func sampleV2SchemaValidator(t *testing.T) func(any) error {
	t.Helper()
	schemaObject := readSampleV2SchemaObject(t)
	schemaBytes, err := json.Marshal(schemaObject)
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatal(err)
	}
	schema.ID = "https://taskgate.local/schema/sample-v2"
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Validate
}

func readSampleV2SchemaObject(t *testing.T) map[string]any {
	t.Helper()
	path := filepath.Join("..", "..", "final-v5-wsl2", "schema", "sample-v2.schema.json")
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(value, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func closedWireNames(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func sortedKeysOfStringMap(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func validSampleV2JSON(t *testing.T) map[string]any {
	t.Helper()
	sample := validTestSample()
	sample.SchemaVersion = TaskGateRejectionSampleSchemaVersion
	sample.Status = "fail"
	sample.ErrorCode = "artifact_measurement_failed"
	instance := sampleJSONInstance(t, sample)
	instance["taskgate_rejection_v1"] = map[string]any{
		"version":         "taskgate-rejection-v1",
		"phase":           "receipt_authentication",
		"gate_code":       "finalizer_instance",
		"failure_kind":    "unavailable",
		"expected_source": "finalizer_verifier",
		"actual_source":   "finalizer_derivation",
	}
	return instance
}

func rejectionObject(t *testing.T, instance map[string]any) map[string]any {
	t.Helper()
	return objectMap(t, instance["taskgate_rejection_v1"], "taskgate_rejection_v1")
}

func differenceObject(t *testing.T, instance map[string]any) map[string]any {
	t.Helper()
	differences, ok := rejectionObject(t, instance)["differences"].([]any)
	if !ok || len(differences) != 1 {
		t.Fatal("differences fixture is not a single-element array")
	}
	return objectMap(t, differences[0], "difference")
}
