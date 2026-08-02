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
)

// TestStageBVerificationSchemaMatchesEvidenceStructs prevents an adapter from
// adding evidence that the retained JSON contract silently treats as an
// unconstrained object.  The runtime validators remain the semantic gate; this
// test binds the eight Stage-B evidence envelopes to their exact Go wire shape.
func TestStageBVerificationSchemaMatchesEvidenceStructs(t *testing.T) {
	path := filepath.Join("..", "..", "final-v5-wsl2", "schema", "sample-v1.schema.json")
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(value, &schema); err != nil {
		t.Fatal(err)
	}
	properties := objectMap(t, schema["properties"], "sample properties")
	definitions := objectMap(t, schema["$defs"], "sample definitions")

	tests := map[string]reflect.Type{
		"scale_verification":       reflect.TypeOf(ScaleVerificationEvidence{}),
		"artifact_verification":    reflect.TypeOf(ArtifactVerificationEvidence{}),
		"rls_verification":         reflect.TypeOf(RLSVerificationEvidence{}),
		"attack_verification":      reflect.TypeOf(AttackVerificationEvidence{}),
		"provsql_verification":     reflect.TypeOf(ProvSQLVerificationEvidence{}),
		"compiler_verification":    reflect.TypeOf(CompilerVerificationEvidence{}),
		"concurrency_verification": reflect.TypeOf(ConcurrencyVerification{}),
		"rq5_verification":         reflect.TypeOf(RQ5VerificationEvidence{}),
	}
	for propertyName, evidenceType := range tests {
		t.Run(propertyName, func(t *testing.T) {
			propertySchema := objectMap(t, properties[propertyName], propertyName)
			branches, ok := propertySchema["anyOf"].([]any)
			if !ok || len(branches) != 2 {
				t.Fatal("verification envelope must have strict-pass and partial-failure branches")
			}
			strictSchema := resolveDefinition(t, objectMap(t, branches[0], propertyName+" strict branch"), definitions)
			partialSchema := resolveDefinition(t, objectMap(t, branches[1], propertyName+" partial branch"), definitions)
			if strictSchema["type"] != "object" || strictSchema["additionalProperties"] != false {
				t.Fatal("verification envelope must be a strict object")
			}

			wantProperties, wantRequired := jsonFields(evidenceType)
			gotProperties := sortedKeys(objectMap(t, strictSchema["properties"], propertyName+" properties"))
			gotRequired := stringArray(t, strictSchema["required"], propertyName+" required")
			sort.Strings(gotRequired)
			if !reflect.DeepEqual(gotProperties, wantProperties) {
				t.Fatalf("schema properties = %v, Go evidence fields = %v", gotProperties, wantProperties)
			}
			if !reflect.DeepEqual(gotRequired, wantRequired) {
				t.Fatalf("schema required = %v, non-omitempty Go fields = %v", gotRequired, wantRequired)
			}
			if partialSchema["type"] != "object" {
				t.Fatal("partial failure envelope must remain an object")
			}
			propertyNames := objectMap(t, partialSchema["propertyNames"], propertyName+" partial propertyNames")
			partialProperties := stringArray(t, propertyNames["enum"], propertyName+" partial property names")
			sort.Strings(partialProperties)
			if !reflect.DeepEqual(partialProperties, wantProperties) {
				t.Fatalf("partial schema properties = %v, Go evidence fields = %v", partialProperties, wantProperties)
			}
		})
	}
}

func TestStageBJSONSchemaRetainsPartialFailuresAndRejectsPartialPasses(t *testing.T) {
	validate := sampleSchemaValidator(t)
	tests := map[string]struct {
		property string
		set      func(*Sample)
	}{
		"scale":       {"scale_verification", func(value *Sample) { value.ScaleVerification = &ScaleVerificationEvidence{} }},
		"artifact":    {"artifact_verification", func(value *Sample) { value.ArtifactVerification = &ArtifactVerificationEvidence{} }},
		"rls":         {"rls_verification", func(value *Sample) { value.RLSVerification = &RLSVerificationEvidence{} }},
		"attack":      {"attack_verification", func(value *Sample) { value.AttackVerification = &AttackVerificationEvidence{} }},
		"provsql":     {"provsql_verification", func(value *Sample) { value.ProvSQLVerification = &ProvSQLVerificationEvidence{} }},
		"compiler":    {"compiler_verification", func(value *Sample) { value.CompilerVerification = &CompilerVerificationEvidence{} }},
		"concurrency": {"concurrency_verification", func(value *Sample) { value.ConcurrencyVerification = &ConcurrencyVerification{} }},
		"rq5":         {"rq5_verification", func(value *Sample) { value.RQ5Verification = &RQ5VerificationEvidence{} }},
	}
	for experimentID, test := range tests {
		t.Run(experimentID, func(t *testing.T) {
			for _, status := range []string{"fail", "invalid"} {
				sample := validTestSample()
				sample.ExperimentID, sample.Status, sample.ErrorCode = experimentID, status, "retained_partial_evidence"
				test.set(&sample)
				instance := sampleJSONInstance(t, sample)
				if err := validate(instance); err != nil {
					t.Fatalf("%s partial evidence was not retained: %v", status, err)
				}

				evidence := objectMap(t, instance[test.property], test.property)
				evidence["unexpected_evidence_field"] = true
				if err := validate(instance); err == nil {
					t.Fatalf("%s partial evidence accepted an unknown field", status)
				}
			}

			partialPass := validTestSample()
			partialPass.ExperimentID = experimentID
			test.set(&partialPass)
			if err := validate(sampleJSONInstance(t, partialPass)); err == nil {
				t.Fatal("pass sample accepted only partial verification evidence")
			}
		})
	}
}

func TestSampleJSONSchemaRequiresProcessAndWarmupIdentity(t *testing.T) {
	validate := sampleSchemaValidator(t)
	instance := sampleJSONInstance(t, validTestSample())
	if err := validate(instance); err != nil {
		t.Fatalf("valid sample: %v", err)
	}
	for _, field := range []string{"process_replicate", "warmup"} {
		mutated := sampleJSONInstance(t, validTestSample())
		delete(mutated, field)
		if err := validate(mutated); err == nil {
			t.Fatalf("sample schema accepted missing %s", field)
		}
	}
}

func resolveDefinition(t *testing.T, schema, definitions map[string]any) map[string]any {
	t.Helper()
	reference, _ := schema["$ref"].(string)
	if reference == "" {
		return schema
	}
	const prefix = "#/$defs/"
	if !strings.HasPrefix(reference, prefix) {
		t.Fatalf("unsupported local reference %q", reference)
	}
	return objectMap(t, definitions[strings.TrimPrefix(reference, prefix)], reference)
}

func sampleSchemaValidator(t *testing.T) func(any) error {
	t.Helper()
	value, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "schema", "sample-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(value, &schema); err != nil {
		t.Fatal(err)
	}
	schema.ID = "https://taskgate.local/schema/sample-v1"
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Validate
}

func sampleJSONInstance(t *testing.T, value Sample) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var instance map[string]any
	if err := json.Unmarshal(encoded, &instance); err != nil {
		t.Fatal(err)
	}
	return instance
}

func objectMap(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object", name)
	}
	return result
}

func stringArray(t *testing.T, value any, name string) []string {
	t.Helper()
	values, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is not an array", name)
	}
	result := make([]string, len(values))
	for index, value := range values {
		var ok bool
		result[index], ok = value.(string)
		if !ok {
			t.Fatalf("%s contains a non-string", name)
		}
	}
	return result
}

func jsonFields(value reflect.Type) (properties, required []string) {
	for index := 0; index < value.NumField(); index++ {
		tag := value.Field(index).Tag.Get("json")
		parts := strings.Split(tag, ",")
		if len(parts) == 0 || parts[0] == "" || parts[0] == "-" {
			continue
		}
		properties = append(properties, parts[0])
		if !containsString(parts[1:], "omitempty") {
			required = append(required, parts[0])
		}
	}
	sort.Strings(properties)
	sort.Strings(required)
	return properties, required
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]any) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
