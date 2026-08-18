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

func TestSampleV1SchemaAndTrackedEvidenceRemainVersionCompatible(t *testing.T) {
	const schemaSHA256 = "0cc18e7bc68a8659f922e260b6db3353dcdfc57f939ac2f69b043a1974475a2e"
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

func TestSampleV2RejectionSchemaRemainsVersionCompatible(t *testing.T) {
	const schemaSHA256 = "ad35086338dc6ff19fedd5433b8071168cca2da624ced40ad61bbbf6afb10dfd"
	path := filepath.Join("..", "..", "final-v5-wsl2", "schema", "sample-v2.schema.json")
	got, err := FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != schemaSHA256 {
		t.Fatalf("sample-v2 rejection schema SHA-256 = %s, want frozen %s", got, schemaSHA256)
	}
}

func TestSampleV3IsAnExplicitPostAcceptanceRevision(t *testing.T) {
	v3 := readSampleSchemaObject(t, "sample-v3.schema.json")
	v3Properties := objectMap(t, v3["properties"], "sample-v3 properties")
	statusValues := stringArray(t, objectMap(t, v3Properties["status"], "sample-v3 status")["enum"], "sample-v3 status enum")
	sort.Strings(statusValues)
	if v3["$id"] != "taskgate-final-v5-sample-v3" ||
		objectMap(t, v3Properties["schema_version"], "sample-v3 schema_version")["const"] != float64(FinalizedSampleSchemaVersion) ||
		!reflect.DeepEqual(statusValues, []string{"fail", "pass"}) ||
		!containsString(stringArray(t, v3["required"], "sample-v3 required"), "taskgate_acceptance_v3") {
		t.Fatal("sample-v3 does not declare its own post-acceptance PASS/FAIL wire")
	}
	if _, present := v3Properties["taskgate_rejection_v1"]; present {
		t.Fatal("sample-v3 exposes the rejection-only sample-v2 member")
	}

	scaleAlternatives, ok := objectMap(t, v3Properties["scale_verification"], "sample-v3 scale verification")["anyOf"].([]any)
	if !ok || len(scaleAlternatives) == 0 {
		t.Fatal("sample-v3 scale verification has no current alternative")
	}
	scaleSchema := objectMap(t, scaleAlternatives[0], "sample-v3 scale verification current alternative")
	scaleProperties := objectMap(t, scaleSchema["properties"], "sample-v3 scale verification properties")
	versions := stringArray(t, objectMap(t, scaleProperties["version"], "sample-v3 scale version")["enum"],
		"sample-v3 scale versions")
	sort.Strings(versions)
	if !reflect.DeepEqual(versions, []string{scaleDependencyEvidenceVersionV2,
		scaleDependencyEvidenceVersionV3, scaleDependencyEvidenceVersionV4}) ||
		objectMap(t, scaleProperties["boundary"], "sample-v3 scale boundary")["const"] != "dependency_e2e" {
		t.Fatal("sample-v3 does not keep historical Scale evidence-v2 separate from current evidence-v3")
	}
	requiredScale := stringArray(t, scaleSchema["required"], "sample-v3 scale required")
	for _, name := range []string{"expected_existing_facts", "expected_union_facts", "existing_dependency_sha256", "union_dependency_sha256"} {
		if !containsString(requiredScale, name) {
			t.Fatalf("sample-v3 Scale evidence does not require %s", name)
		}
	}
	artifactAlternatives, ok := objectMap(t, v3Properties["artifact_verification"], "sample-v3 artifact verification")["anyOf"].([]any)
	if !ok || len(artifactAlternatives) == 0 {
		t.Fatal("sample-v3 artifact verification has no current alternative")
	}
	artifactSchema := objectMap(t, artifactAlternatives[0], "sample-v3 artifact verification current alternative")
	artifactProperties := objectMap(t, artifactSchema["properties"], "sample-v3 artifact verification properties")
	if objectMap(t, artifactProperties["version"], "sample-v3 artifact version")["const"] != artifactEvidenceVersionV2 {
		t.Fatal("sample-v3 does not select Artifact evidence-v2")
	}

	current := validTestSample()
	current.SchemaVersion = FinalizedSampleSchemaVersion
	current.TaskGateAcceptanceV3 = &FinalizationV3{}
	if err := sampleV3SchemaValidator(t)(sampleJSONInstance(t, current)); err != nil {
		t.Fatalf("sample-v3 JSON Schema rejected a current PASS: %v", err)
	}
	if err := current.Validate(); err != nil {
		t.Fatalf("Go validation rejected a current sample-v3 PASS: %v", err)
	}
	withoutAcceptance := current
	withoutAcceptance.TaskGateAcceptanceV3 = nil
	if err := sampleV3SchemaValidator(t)(sampleJSONInstance(t, withoutAcceptance)); err == nil {
		t.Fatal("sample-v3 JSON Schema accepted a sample without finalizer acceptance")
	}
	if err := withoutAcceptance.Validate(); err == nil {
		t.Fatal("Go validation accepted sample-v3 without finalizer acceptance")
	}
	if err := sampleSchemaValidator(t)(sampleJSONInstance(t, current)); err == nil {
		t.Fatal("sample-v1 JSON Schema silently reinterpreted sample-v3")
	}
	current.Status = "fail"
	current.ErrorCode = "post_acceptance_evidence_invariant_failed"
	if err := sampleV3SchemaValidator(t)(sampleJSONInstance(t, current)); err != nil {
		t.Fatalf("sample-v3 JSON Schema rejected a retained post-acceptance FAIL: %v", err)
	}
	if err := current.Validate(); err != nil {
		t.Fatalf("Go validation rejected a retained post-acceptance FAIL: %v", err)
	}
	current.Status = "invalid"
	if err := sampleV3SchemaValidator(t)(sampleJSONInstance(t, current)); err == nil {
		t.Fatal("sample-v3 JSON Schema accepted INVALID")
	}
	if err := current.Validate(); err == nil {
		t.Fatal("Go validation accepted INVALID on the finalized sample-v3 wire")
	}
}

func TestOutcomeCandidateVerificationCannotLeakIntoLegacyOrNonScaleWires(t *testing.T) {
	expectation := outcomeCandidateTestExpectation(t,
		outcomeCandidateTestDigest(1), outcomeCandidateTestDigest(2), outcomeCandidateTestDigest(3),
		outcomeCandidateTestDigest(4), outcomeCandidateTestDigest(5))
	verification := &OutcomeCandidateVerificationV1{
		Version: OutcomeCandidateVerificationV1Version, Expected: expectation, Observed: expectation,
	}

	legacy := validTestSample()
	legacy.TaskGateAcceptanceV3 = &FinalizationV3{OutcomeCandidateVerification: verification}
	encodedLegacy, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var decodedLegacy Sample
	if err := StrictJSON(encodedLegacy, &decodedLegacy); err == nil {
		t.Fatal("sample-v1 strict reader silently acquired Outcome candidate verification")
	}
	if err := legacy.Validate(); err == nil {
		t.Fatal("sample-v1 Go validation silently acquired Outcome candidate verification")
	}

	nonScale := validTestSample()
	nonScale.SchemaVersion = FinalizedSampleSchemaVersion
	nonScale.TaskGateAcceptanceV3 = &FinalizationV3{OutcomeCandidateVerification: verification}
	nonScaleInstance := sampleJSONInstance(t, nonScale)
	if err := sampleV3SchemaValidator(t)(nonScaleInstance); err == nil {
		t.Fatal("sample-v3 schema accepted Outcome candidate verification on a non-Scale experiment")
	}
	encodedNonScale, err := json.Marshal(nonScaleInstance)
	if err != nil {
		t.Fatal(err)
	}
	var decodedNonScale Sample
	if err := StrictJSON(encodedNonScale, &decodedNonScale); err == nil {
		t.Fatal("sample-v3 strict reader accepted Outcome candidate verification on a non-Scale experiment")
	}
	if err := nonScale.Validate(); err == nil {
		t.Fatal("sample-v3 Go validation accepted Outcome candidate verification on a non-Scale experiment")
	}

	nullVerification := sampleJSONInstance(t, validTestSample())
	nullVerification["taskgate_acceptance_v3"] = map[string]any{"outcome_candidate_verification": nil}
	encodedNull, err := json.Marshal(nullVerification)
	if err != nil {
		t.Fatal(err)
	}
	var decodedNull Sample
	if err := StrictJSON(encodedNull, &decodedNull); err == nil {
		t.Fatal("sample-v1 strict reader accepted explicit-null Outcome candidate verification")
	}
}

func TestSampleV3ScaleRequiresDecision18AndOutcomeCandidateFields(t *testing.T) {
	digest := strings.Repeat("a", 64)
	sample := validTestSample()
	sample.SchemaVersion = FinalizedSampleSchemaVersion
	outcome := outcomeCandidateTestExpectation(t,
		outcomeCandidateTestDigest(1), outcomeCandidateTestDigest(2), outcomeCandidateTestDigest(3),
		outcomeCandidateTestDigest(4), outcomeCandidateTestDigest(5))
	sample.TaskGateAcceptanceV3 = &FinalizationV3{OutcomeCandidateVerification: &OutcomeCandidateVerificationV1{
		Version: OutcomeCandidateVerificationV1Version, Expected: outcome, Observed: outcome,
	}}
	sample.ExperimentID = "scale"
	sample.WorkloadID = "dependency-e2e"
	sample.Scale = "10k-overlap-0"
	sample.Mode = "novel"
	sample.ScaleVerification = &ScaleVerificationEvidence{
		Version: scaleDependencyEvidenceVersionV4, Boundary: "dependency_e2e",
		BindingFileSHA256: digest, BindingSHA256: digest, DatasetSHA256: digest,
		DatasetProbeSHA256: strings.Repeat("b", 64), CatalogSHA256: digest, QuerySHA256: digest,
		ExpectedRows: 1, ExpectedColumns: 1, ExpectedResultSHA256: digest,
		ExpectedCandidateFacts: 10_000, ObservedCandidateFacts: 10_000,
		ExpectedExistingFacts: 10_000, ExpectedUnionFacts: 20_000,
		ExpectedOutcomeMemberCardinality: 5, ObservedOutcomeMemberCardinality: 5,
		ExpectedOutcomeCandidateSetSHA256: outcome.OrdinarySetSHA256,
		ObservedOutcomeCandidateSetSHA256: outcome.OrdinarySetSHA256,
		ExistingDependencySHA256:          digest, CandidateDependencySHA256: digest, UnionDependencySHA256: digest,
		HistoryDependencyLink:    sampleSchemaScaleLink(DependencyScaleExistingSummaryRole, 10_000, digest),
		CandidateDependencyLink:  sampleSchemaScaleLink(DependencyScaleCandidateSummaryRole, 10_000, digest),
		RootBeforeDependencyLink: sampleSchemaScaleLink(DependencyScaleExistingSummaryRole, 10_000, digest),
		RootAfterDependencyLink:  sampleSchemaScaleLink(DependencyScaleUnionSummaryRole, 20_000, digest),
		ObserverWindow:           &ObserverWindowV2{},
	}
	instance := sampleJSONInstance(t, sample)
	validate := sampleV3SchemaValidator(t)
	if err := validate(instance); err != nil {
		t.Fatalf("sample-v3 schema rejected complete Decision-18 Scale evidence: %v", err)
	}
	if err := sample.Validate(); err != nil {
		t.Fatalf("sample-v3 Go reader rejected complete Decision-18 Scale evidence: %v", err)
	}

	for _, field := range []string{
		"expected_existing_facts", "expected_union_facts", "existing_dependency_sha256", "union_dependency_sha256",
		"expected_outcome_member_cardinality", "observed_outcome_member_cardinality",
		"expected_outcome_candidate_set_sha256", "observed_outcome_candidate_set_sha256",
		"candidate_dependency_link", "root_before_dependency_link", "root_after_dependency_link",
	} {
		t.Run("missing "+field, func(t *testing.T) {
			mutated := sampleJSONInstance(t, sample)
			delete(objectMap(t, mutated["scale_verification"], "scale verification"), field)
			if err := validate(mutated); err == nil {
				t.Fatalf("sample-v3 schema accepted Scale evidence without %s", field)
			}
		})
	}
	withoutFinalizerMembers := sampleJSONInstance(t, sample)
	delete(objectMap(t, withoutFinalizerMembers["taskgate_acceptance_v3"], "Scale acceptance"),
		"outcome_candidate_verification")
	if err := validate(withoutFinalizerMembers); err == nil {
		t.Fatal("sample-v3 schema accepted Scale evidence-v3 without finalizer member verification")
	}
	withoutFinalizerMembersSample := sample
	withoutFinalizerMembersSample.TaskGateAcceptanceV3 = &FinalizationV3{}
	if err := withoutFinalizerMembersSample.Validate(); err == nil {
		t.Fatal("sample-v3 Go reader accepted Scale evidence-v3 without finalizer member verification")
	}
	malformedFinalizerMembers := sampleJSONInstance(t, sample)
	verification := objectMap(t,
		objectMap(t, malformedFinalizerMembers["taskgate_acceptance_v3"], "Scale acceptance")["outcome_candidate_verification"],
		"Outcome member verification")
	delete(objectMap(t, verification["expected"], "expected Outcome members"), "members")
	if err := validate(malformedFinalizerMembers); err == nil {
		t.Fatal("sample-v3 schema accepted an incomplete finalizer Outcome member set")
	}
	historicalV2 := sampleJSONInstance(t, sample)
	historicalScale := objectMap(t, historicalV2["scale_verification"], "historical scale verification")
	historicalScale["version"] = scaleDependencyEvidenceVersionV2
	for _, field := range []string{
		"expected_outcome_member_cardinality", "observed_outcome_member_cardinality",
		"expected_outcome_candidate_set_sha256", "observed_outcome_candidate_set_sha256",
		"history_dependency_link", "candidate_dependency_link",
		"root_before_dependency_link", "root_after_dependency_link",
	} {
		delete(historicalScale, field)
	}
	delete(objectMap(t, historicalV2["taskgate_acceptance_v3"], "historical acceptance"),
		"outcome_candidate_verification")
	if err := validate(historicalV2); err != nil {
		t.Fatalf("sample-v3 schema rejected historical Scale evidence-v2: %v", err)
	}
	historicalSample := sample
	historicalSample.TaskGateAcceptanceV3 = &FinalizationV3{}
	historicalEvidence := *sample.ScaleVerification
	historicalEvidence.Version = scaleDependencyEvidenceVersionV2
	historicalEvidence.ExpectedOutcomeMemberCardinality = 0
	historicalEvidence.ObservedOutcomeMemberCardinality = 0
	historicalEvidence.ExpectedOutcomeCandidateSetSHA256 = ""
	historicalEvidence.ObservedOutcomeCandidateSetSHA256 = ""
	historicalEvidence.HistoryDependencyLink = nil
	historicalEvidence.CandidateDependencyLink = nil
	historicalEvidence.RootBeforeDependencyLink = nil
	historicalEvidence.RootAfterDependencyLink = nil
	historicalSample.ScaleVerification = &historicalEvidence
	if err := historicalSample.Validate(); err != nil {
		t.Fatalf("sample-v3 Go reader rejected historical Scale evidence-v2: %v", err)
	}
	v2WithFinalizerMembers := sampleJSONInstance(t, historicalSample)
	currentAcceptance := objectMap(t, sampleJSONInstance(t, sample)["taskgate_acceptance_v3"], "current acceptance")
	objectMap(t, v2WithFinalizerMembers["taskgate_acceptance_v3"], "historical acceptance")["outcome_candidate_verification"] =
		currentAcceptance["outcome_candidate_verification"]
	if err := validate(v2WithFinalizerMembers); err == nil {
		t.Fatal("sample-v3 schema silently added Outcome member verification to historical evidence-v2")
	}
	encodedV2WithFinalizerMembers, err := json.Marshal(v2WithFinalizerMembers)
	if err != nil {
		t.Fatal(err)
	}
	var decodedV2WithFinalizerMembers Sample
	if err := StrictJSON(encodedV2WithFinalizerMembers, &decodedV2WithFinalizerMembers); err == nil {
		t.Fatal("sample-v3 strict reader silently added Outcome member verification to historical evidence-v2")
	}
	historicalV3 := sampleJSONInstance(t, sample)
	historicalV3Scale := objectMap(t, historicalV3["scale_verification"], "historical v3 scale verification")
	historicalV3Scale["version"] = scaleDependencyEvidenceVersionV3
	for _, field := range []string{"history_dependency_link", "candidate_dependency_link",
		"root_before_dependency_link", "root_after_dependency_link"} {
		delete(historicalV3Scale, field)
	}
	if err := validate(historicalV3); err != nil {
		t.Fatalf("sample-v3 schema rejected frozen Scale evidence-v3 without v4 links: %v", err)
	}
	historicalV3Sample := sample
	historicalV3Evidence := *sample.ScaleVerification
	historicalV3Evidence.Version = scaleDependencyEvidenceVersionV3
	historicalV3Evidence.HistoryDependencyLink = nil
	historicalV3Evidence.CandidateDependencyLink = nil
	historicalV3Evidence.RootBeforeDependencyLink = nil
	historicalV3Evidence.RootAfterDependencyLink = nil
	historicalV3Sample.ScaleVerification = &historicalV3Evidence
	if err := historicalV3Sample.Validate(); err != nil {
		t.Fatalf("Go reader rejected frozen Scale evidence-v3 without v4 links: %v", err)
	}
	historicalSample.TaskGateAcceptanceV3 = sample.TaskGateAcceptanceV3
	if err := historicalSample.Validate(); err == nil {
		t.Fatal("sample-v3 Go validation silently added Outcome member verification to historical evidence-v2")
	}
	historicalSample.TaskGateAcceptanceV3 = &FinalizationV3{}
	for field, explicitValue := range map[string]any{
		"expected_outcome_member_cardinality":   float64(0),
		"observed_outcome_member_cardinality":   float64(0),
		"expected_outcome_candidate_set_sha256": "",
		"observed_outcome_candidate_set_sha256": "",
	} {
		t.Run("evidence-v2 rejects "+field, func(t *testing.T) {
			mutated := sampleJSONInstance(t, historicalSample)
			objectMap(t, mutated["scale_verification"], "historical scale verification")[field] = explicitValue
			if err := validate(mutated); err == nil {
				t.Fatalf("sample-v3 schema accepted evidence-v2 with explicit evidence-v3 member %s", field)
			}
			encoded, err := json.Marshal(mutated)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Sample
			if err := StrictJSON(encoded, &decoded); err == nil {
				t.Fatalf("sample-v3 strict reader accepted evidence-v2 with explicit evidence-v3 member %s", field)
			}
		})
	}

	legacy := sampleJSONInstance(t, sample)
	legacyScale := objectMap(t, legacy["scale_verification"], "legacy scale verification")
	legacyScale["version"] = scaleEvidenceVersion
	legacyScale["history_dependency_sha256"] = digest
	if err := validate(legacy); err == nil {
		t.Fatal("sample-v3 schema silently reinterpreted legacy Scale history/evidence-v1")
	}
	legacySample := sample
	legacyEvidence := *sample.ScaleVerification
	legacyEvidence.Version = scaleEvidenceVersion
	legacyEvidence.HistoryDependencySHA256 = digest
	legacySample.ScaleVerification = &legacyEvidence
	if err := legacySample.Validate(); err == nil {
		t.Fatal("sample-v3 Go reader silently reinterpreted legacy Scale history/evidence-v1")
	}
	legacySample = sample
	legacySample.SchemaVersion = SampleSchemaVersion
	if err := legacySample.Validate(); err == nil {
		t.Fatal("sample-v1 Go reader accepted sample-v3 Decision-18 Scale members")
	}
}

func sampleSchemaScaleLink(role DependencyScaleSummaryRole, cardinality int64,
	digest string) *ScaleDependencySetVerificationV1 {
	return &ScaleDependencySetVerificationV1{
		Version: ScaleDependencySetVerificationV1Version, Role: role, Match: true,
		ExpectedCardinality: cardinality, ExpectedSemanticSetSHA256: digest,
		ObservedCardinality: cardinality, ObservedSemanticSetSHA256: digest,
		ProductionSetSHA256: digest, ProductionDictionarySHA256: digest,
		ObservedOrdinalSetSHA256: digest,
	}
}

func TestSampleV1V2RejectDecision18ScaleMembersByWirePresence(t *testing.T) {
	tests := []struct {
		name     string
		instance func(*testing.T) map[string]any
		validate func(*testing.T) func(any) error
	}{
		{name: "sample-v1", instance: func(t *testing.T) map[string]any {
			return sampleJSONInstance(t, validTestSample())
		}, validate: sampleSchemaValidator},
		{name: "sample-v2", instance: validSampleV2JSON, validate: sampleV2SchemaValidator},
	}
	for _, test := range tests {
		for field, explicitZero := range map[string]any{
			"expected_existing_facts":               float64(0),
			"expected_union_facts":                  float64(0),
			"existing_dependency_sha256":            "",
			"union_dependency_sha256":               "",
			"expected_outcome_member_cardinality":   float64(0),
			"observed_outcome_member_cardinality":   float64(0),
			"expected_outcome_candidate_set_sha256": "",
			"observed_outcome_candidate_set_sha256": "",
		} {
			t.Run(test.name+"/"+field, func(t *testing.T) {
				instance := test.instance(t)
				instance["scale_verification"] = map[string]any{field: explicitZero}
				if err := test.validate(t)(instance); err == nil {
					t.Fatalf("%s JSON Schema accepted explicitly present %s", test.name, field)
				}
				encoded, err := json.Marshal(instance)
				if err != nil {
					t.Fatal(err)
				}
				var sample Sample
				if err := StrictJSON(encoded, &sample); err == nil {
					t.Fatalf("%s strict reader accepted explicitly present %s", test.name, field)
				}
			})
		}
	}
}

func TestSampleV3SchemaMatchesCurrentGoWire(t *testing.T) {
	schema := readSampleSchemaObject(t, "sample-v3.schema.json")
	properties := objectMap(t, schema["properties"], "sample-v3 properties")
	definitions := objectMap(t, schema["$defs"], "sample-v3 definitions")

	wantSample, _ := jsonFields(reflect.TypeOf(Sample{}))
	wantSample = withoutStrings(wantSample, "taskgate_rejection_v1")
	if got := sortedKeys(properties); !reflect.DeepEqual(got, wantSample) {
		t.Fatalf("sample-v3 properties = %v, current Go Sample wire = %v", got, wantSample)
	}

	for propertyName, evidenceType := range map[string]reflect.Type{
		"scale_verification":    reflect.TypeOf(ScaleVerificationEvidence{}),
		"artifact_verification": reflect.TypeOf(ArtifactVerificationEvidence{}),
	} {
		t.Run(propertyName, func(t *testing.T) {
			branches, ok := objectMap(t, properties[propertyName], propertyName)["anyOf"].([]any)
			if !ok || len(branches) != 2 {
				t.Fatal("sample-v3 evidence envelope must have current and retained-failure branches")
			}
			strict := resolveDefinition(t, objectMap(t, branches[0], propertyName+" current"), definitions)
			partial := resolveDefinition(t, objectMap(t, branches[1], propertyName+" partial"), definitions)
			wantFields, _ := jsonFields(evidenceType)
			if got := sortedKeys(objectMap(t, strict["properties"], propertyName+" properties")); !reflect.DeepEqual(got, wantFields) {
				t.Fatalf("sample-v3 current properties = %v, Go evidence wire = %v", got, wantFields)
			}
			partialNames := stringArray(t,
				objectMap(t, partial["propertyNames"], propertyName+" partial propertyNames")["enum"],
				propertyName+" partial property names")
			sort.Strings(partialNames)
			if !reflect.DeepEqual(partialNames, wantFields) {
				t.Fatalf("sample-v3 retained-failure properties = %v, Go evidence wire = %v", partialNames, wantFields)
			}
		})
	}

	acceptanceProperties := objectMap(t,
		objectMap(t, properties["taskgate_acceptance_v3"], "taskgate acceptance")["properties"],
		"taskgate acceptance properties")
	verificationSchema := resolveDefinition(t,
		objectMap(t, acceptanceProperties["outcome_candidate_verification"], "Outcome verification"), definitions)
	wantVerification, wantVerificationRequired := jsonFields(reflect.TypeOf(OutcomeCandidateVerificationV1{}))
	if got := sortedKeys(objectMap(t, verificationSchema["properties"], "Outcome verification properties")); !reflect.DeepEqual(got, wantVerification) {
		t.Fatalf("Outcome verification schema properties = %v, Go wire = %v", got, wantVerification)
	}
	gotVerificationRequired := stringArray(t, verificationSchema["required"], "Outcome verification required")
	sort.Strings(gotVerificationRequired)
	if !reflect.DeepEqual(gotVerificationRequired, wantVerificationRequired) {
		t.Fatalf("Outcome verification required = %v, Go wire = %v",
			gotVerificationRequired, wantVerificationRequired)
	}
	expectedSchema := resolveDefinition(t,
		objectMap(t, objectMap(t, verificationSchema["properties"], "Outcome verification properties")["expected"],
			"expected Outcome members"), definitions)
	wantExpectation, wantExpectationRequired := jsonFields(reflect.TypeOf(OutcomeCandidateExpectationV1{}))
	if got := sortedKeys(objectMap(t, expectedSchema["properties"], "Outcome expectation properties")); !reflect.DeepEqual(got, wantExpectation) {
		t.Fatalf("Outcome expectation schema properties = %v, Go wire = %v", got, wantExpectation)
	}
	gotExpectationRequired := stringArray(t, expectedSchema["required"], "Outcome expectation required")
	sort.Strings(gotExpectationRequired)
	if !reflect.DeepEqual(gotExpectationRequired, wantExpectationRequired) {
		t.Fatalf("Outcome expectation required = %v, Go wire = %v",
			gotExpectationRequired, wantExpectationRequired)
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

func sampleV3SchemaValidator(t *testing.T) func(any) error {
	t.Helper()
	schemaObject := readSampleSchemaObject(t, "sample-v3.schema.json")
	schemaBytes, err := json.Marshal(schemaObject)
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatal(err)
	}
	schema.ID = "https://taskgate.local/schema/sample-v3"
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Validate
}

func readSampleV2SchemaObject(t *testing.T) map[string]any {
	return readSampleSchemaObject(t, "sample-v2.schema.json")
}

func readSampleSchemaObject(t *testing.T, name string) map[string]any {
	t.Helper()
	path := filepath.Join("..", "..", "final-v5-wsl2", "schema", name)
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
