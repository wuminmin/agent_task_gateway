package finalv5binding

import (
	"bytes"
	"encoding/json"
	"path"
	"reflect"
	"strings"
	"testing"

	contractfs "taskbound.local/agent-data-gateway/evaluation/final-v5-wsl2"
	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

const observedBenchmarkProbeSHA256 = "0eb905408442997de37ac810683f18c758b614a716c50758312015aeb753d314"

func TestCompleteBindingRejectsPlaceholderConflationAndOpenClosures(t *testing.T) {
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	dataset, err := runtime.DatasetIdentitySHA256()
	if err != nil {
		t.Fatal(err)
	}
	catalogBytes, err := contractfs.FS.ReadFile("catalog/benchmark-contract-v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	parsedCatalog, err := catalog.Parse(catalogBytes)
	if err != nil {
		t.Fatal(err)
	}
	base := CompleteBindingInput{DatasetSHA256: dataset, DatasetProbeSHA256: observedBenchmarkProbeSHA256,
		Catalog: parsedCatalog, ArtifactRuntime: runtime,
		ScaleManifests:   make([]finalv5oracle.ExposureScaleManifestArtifact, 24),
		ProvSQLManifests: make([]finalv5oracle.ProvSQLManifestArtifact, 105)}
	tests := []struct {
		name   string
		mutate func(*CompleteBindingInput)
	}{
		{name: "placeholder Catalog", mutate: func(input *CompleteBindingInput) {
			changed := *input.Catalog
			changed.SHA256 = strings.Repeat("0", 64)
			input.Catalog = &changed
		}},
		{name: "obvious test probe digest", mutate: func(input *CompleteBindingInput) {
			input.DatasetProbeSHA256 = strings.Repeat("a", 64)
		}},
		{name: "Dataset/probe conflation", mutate: func(input *CompleteBindingInput) {
			input.DatasetProbeSHA256 = input.DatasetSHA256
		}},
		{name: "Catalog/probe conflation", mutate: func(input *CompleteBindingInput) {
			changed := *input.Catalog
			changed.SHA256 = input.DatasetProbeSHA256
			input.Catalog = &changed
		}},
		{name: "missing Scale cell", mutate: func(input *CompleteBindingInput) {
			input.ScaleManifests = input.ScaleManifests[:23]
		}},
		{name: "extra ProvSQL cell", mutate: func(input *CompleteBindingInput) {
			input.ProvSQLManifests = append(input.ProvSQLManifests, finalv5oracle.ProvSQLManifestArtifact{})
		}},
		{name: "missing Contract runtime", mutate: func(input *CompleteBindingInput) {
			input.ArtifactRuntime = nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			test.mutate(&input)
			if _, _, err := BuildCompleteBinding(input); err == nil {
				t.Fatal("invalid complete-binding input was accepted")
			}
		})
	}
}

func TestManifestArtifactEnvelopeRejectsByteDriftAndDuplicatePath(t *testing.T) {
	scale := trackedScaleManifestArtifacts(t)
	drifted := append([]finalv5oracle.ExposureScaleManifestArtifact(nil), scale...)
	drifted[0].SHA256 = strings.Repeat("a", 64)
	if _, err := verifyScaleManifestArtifacts(drifted, finalv5oracle.StreamSetOptions{}); err == nil {
		t.Fatal("Scale manifest byte drift was accepted")
	}
	duplicated := append([]finalv5oracle.ExposureScaleManifestArtifact(nil), scale...)
	duplicated[1].RelativePath = duplicated[0].RelativePath
	if _, err := verifyScaleManifestArtifacts(duplicated, finalv5oracle.StreamSetOptions{}); err == nil {
		t.Fatal("duplicate Scale manifest path was accepted")
	}

	provSQL := trackedProvSQLManifestArtifacts(t)
	provSQL[0].SHA256 = strings.Repeat("b", 64)
	if _, err := verifyProvSQLManifestArtifacts(provSQL, finalv5oracle.StreamSetOptions{}); err == nil {
		t.Fatal("ProvSQL manifest byte drift was accepted")
	}
}

func TestBuildProvSQLBindingKeepsTypedPublicationAndAdapterResultDomainsSeparate(t *testing.T) {
	manifests := trackedProvSQLManifestArtifacts(t)
	binding, err := buildProvSQLBinding(manifests)
	if err != nil {
		t.Fatal(err)
	}
	type identities struct{ typed, adapter string }
	byScale := make(map[string]identities, 3)
	for _, scale := range []string{"1k", "10k", "45k"} {
		rows, err := provsqlfixture.ExpectedResultRows(scale)
		if err != nil {
			t.Fatal(err)
		}
		typed, err := finalv5oracle.CanonicalResult(finalv5oracle.ProvSQLResultSchema(), rows)
		if err != nil {
			t.Fatal(err)
		}
		adapterSHA256, err := canonicalAdapterResultHash(rows)
		if err != nil {
			t.Fatal(err)
		}
		if typed.CanonicalResultSHA256 == adapterSHA256 {
			t.Fatalf("%s typed publication and Adapter result identities unexpectedly share one digest domain", scale)
		}
		byScale[scale] = identities{typed: typed.CanonicalResultSHA256, adapter: adapterSHA256}
	}
	for _, artifact := range manifests {
		wanted := byScale[artifact.Manifest.Scale]
		if artifact.Manifest.Expected.CanonicalResultSHA256 != wanted.typed {
			t.Fatalf("manifest %s does not retain the independently typed publication result identity",
				artifact.RelativePath)
		}
		tracked, present := binding.TaskGate[artifact.Manifest.BindingKey]
		if !present || tracked.ExpectedResultSHA256 != wanted.adapter {
			t.Fatalf("binding %s does not retain the Adapter result identity", artifact.Manifest.BindingKey)
		}
	}
	if len(binding.TaskGate) != 105 || len(manifests) != 105 {
		t.Fatalf("ProvSQL closure = %d binding cells/%d manifests; want 105/105", len(binding.TaskGate), len(manifests))
	}

	drifted := append([]finalv5oracle.ProvSQLManifestArtifact(nil), manifests...)
	drifted[0].Manifest.Expected.CanonicalResultSHA256 = byScale[drifted[0].Manifest.Scale].adapter
	if _, err := buildProvSQLBinding(drifted); err == nil {
		t.Fatal("binding generation accepted an Adapter digest in the typed publication result domain")
	}
	driftedBinding := *binding
	driftedBinding.TaskGate = make(map[string]BoundQueryExpectation, len(binding.TaskGate))
	for key, cell := range binding.TaskGate {
		driftedBinding.TaskGate[key] = cell
	}
	driftedCell := driftedBinding.TaskGate["1k/1"]
	driftedCell.ExpectedResultSHA256 = byScale["1k"].typed
	driftedBinding.TaskGate["1k/1"] = driftedCell
	if err := ValidateProvSQLBinding(&driftedBinding); err == nil {
		t.Fatal("binding validation accepted a typed publication digest in the Adapter result domain")
	}
}

func TestBuildScaleOutcomeCandidateExpectationRetainsStrictOracleMembers(t *testing.T) {
	catalogSHA256 := shaBytes([]byte("complete Scale Catalog"))
	options := finalv5oracle.StreamSetOptions{TempDir: t.TempDir(), MaxInMemoryMembers: 20_000}
	oracle, err := finalv5oracle.GenerateExposureScaleOutcomeCandidate(finalv5oracle.ExposureScaleOutcomeRequest{
		CatalogSHA256: catalogSHA256, CandidateFacts: finalv5oracle.DependencyScale10K, SetOptions: options,
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := buildScaleOutcomeCandidateExpectation(catalogSHA256,
		finalv5oracle.DependencyScale10K, options)
	if err != nil {
		t.Fatal(err)
	}
	if expected.Cardinality != 5 || len(expected.Members) != 5 ||
		expected.OrdinarySetSHA256 != oracle.CandidateSetSHA256 ||
		!reflect.DeepEqual(expected.Members, oracle.Members) {
		t.Fatalf("bound strict Outcome candidate = %+v; oracle cardinality/set/members = %d/%s/%v",
			expected, oracle.CandidateCardinality, oracle.CandidateSetSHA256, oracle.Members)
	}
	if err := ValidateBoundOutcomeCandidate(expected); err != nil {
		t.Fatalf("strict Outcome candidate failed member-level validation: %v", err)
	}
	incomplete := ScaleOutcomeCandidateExpectations{catalogSHA256: catalogSHA256,
		byCandidateFacts: map[int64]BoundOutcomeCandidateExpectation{
			finalv5oracle.DependencyScale10K: expected,
		}}
	if err := incomplete.validate(catalogSHA256); err == nil {
		t.Fatal("incomplete pre-run Scale Outcome expectation bundle was accepted")
	}
	if err := incomplete.validate(shaBytes([]byte("different complete Catalog"))); err == nil {
		t.Fatal("pre-run Scale Outcome expectation bundle was accepted for a different Catalog")
	}
}

func TestGenerateScaleOutcomeCandidateExpectationsClosesThreeFrozenSizes(t *testing.T) {
	catalogSHA256 := shaBytes([]byte("complete pre-run Scale Catalog"))
	expected, err := GenerateScaleOutcomeCandidateExpectations(catalogSHA256,
		finalv5oracle.StreamSetOptions{TempDir: t.TempDir(), MaxInMemoryMembers: 2_100_000})
	if err != nil {
		t.Fatal(err)
	}
	if err := expected.validate(catalogSHA256); err != nil {
		t.Fatal(err)
	}
	identityByFacts := make(map[int64]string, 3)
	factsByIdentity := make(map[string]int64, 3)
	for _, spec := range finalv5oracle.ExposureScaleDependencyCells() {
		outcome, err := expected.forCandidateFacts(spec.CandidateFacts)
		if err != nil {
			t.Fatal(err)
		}
		identity := outcome.OrdinarySetSHA256 + strings.Join(outcome.Members, "")
		if prior, present := identityByFacts[spec.CandidateFacts]; present && prior != identity {
			t.Fatalf("cached %d-Fact strict Outcome identity changed across overlap cells", spec.CandidateFacts)
		}
		if priorFacts, present := factsByIdentity[identity]; present && priorFacts != spec.CandidateFacts {
			t.Fatalf("strict Outcome identity was shared by %d and %d candidate Facts", priorFacts, spec.CandidateFacts)
		}
		identityByFacts[spec.CandidateFacts] = identity
		factsByIdentity[identity] = spec.CandidateFacts
	}
	if len(identityByFacts) != 3 || len(factsByIdentity) != 3 {
		t.Fatalf("pre-run strict Outcome bundle has %d sizes and %d identities; want 3/3",
			len(identityByFacts), len(factsByIdentity))
	}
}

func TestVerifiedRuntimeClosesScaleAndProvSQLPublicMatrices(t *testing.T) {
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	enableExtreme, err := validateContractRuntimeClosure(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if enableExtreme {
		t.Fatal("verified runtime enabled the Scale extreme grid removed in contract v1.11")
	}
}

func TestScaleExtremeFeatureFlagFollowsSourceControlledGridAndRoundTrips(t *testing.T) {
	exact := []finalv5contracts.CellIdentity{
		{ExperimentID: "scale-extreme", WorkloadID: "taskgate_scale_extreme", Scale: "10m", Mode: "kernel_storage_only"},
		{ExperimentID: "scale-extreme", WorkloadID: "taskgate_scale_extreme", Scale: "100m", Mode: "kernel_storage_only"},
	}
	for _, test := range []struct {
		name  string
		cells []finalv5contracts.CellIdentity
		want  bool
	}{
		{name: "exact extreme grid", cells: exact, want: true},
		{name: "no extreme grid", cells: nil, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			enabled, err := scaleExtremeFeatureEnabled(test.cells)
			if err != nil {
				t.Fatal(err)
			}
			binding := ScaleBinding{EnableExtreme: enabled}
			value, err := json.Marshal(binding)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(value, []byte(`"enable_extreme"`)) != test.want {
				t.Fatalf("encoded Scale binding = %s; enable_extreme presence want %t", value, test.want)
			}
			var decoded ScaleBinding
			if err := strictJSON(value, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.EnableExtreme != test.want {
				t.Fatalf("decoded enable_extreme = %t, want %t", decoded.EnableExtreme, test.want)
			}
		})
	}
	if _, err := scaleExtremeFeatureEnabled(exact[:1]); err == nil {
		t.Fatal("partial Scale extreme grid was accepted")
	}
}

func TestFixedTaskModelsAreClosedDetachedCopies(t *testing.T) {
	scale := FixedScaleTask()
	scale.Columns[exposureScaleProduct][0] = "changed"
	if FixedScaleTask().Columns[exposureScaleProduct][0] != "member_rank" {
		t.Fatal("Scale task model leaked a mutable shared slice")
	}
	x4, err := FixedArtifactTask(4)
	if err != nil {
		t.Fatal(err)
	}
	x16, err := FixedArtifactTask(16)
	if err != nil {
		t.Fatal(err)
	}
	if len(x4.Columns[resultHeavyProduct]) != 4 || len(x16.Columns[resultHeavyProduct]) != 16 {
		t.Fatal("Artifact task model differs from fixed x4/x16 projections")
	}
	if _, err := FixedArtifactTask(5); err == nil {
		t.Fatal("non-frozen Artifact projection was accepted")
	}
	provSQL := FixedProvSQLTask()
	if len(provSQL.DataProducts) != 3 || len(provSQL.Columns) != 3 {
		t.Fatal("ProvSQL task model is not the exact three-Product surface")
	}
}

func trackedScaleManifestArtifacts(t *testing.T) []finalv5oracle.ExposureScaleManifestArtifact {
	t.Helper()
	result := make([]finalv5oracle.ExposureScaleManifestArtifact, 0, 24)
	for _, cell := range finalv5oracle.ExposureScaleDependencyCells() {
		for _, mode := range []string{finalv5oracle.ExposureScaleModeNovel,
			finalv5oracle.ExposureScaleModeSemanticReplay} {
			relative, err := finalv5oracle.ExposureScaleDependencyManifestPath(cell.Scale, mode)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, readScaleManifestArtifact(t, relative))
		}
	}
	return result
}

func readScaleManifestArtifact(t *testing.T, relative string) finalv5oracle.ExposureScaleManifestArtifact {
	t.Helper()
	value, err := contractfs.FS.ReadFile(path.Join("oracle-manifests", relative))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := finalv5oracle.DecodeManifest(value)
	if err != nil {
		t.Fatal(err)
	}
	return finalv5oracle.ExposureScaleManifestArtifact{RelativePath: relative, SHA256: shaBytes(value), Manifest: manifest}
}

func trackedProvSQLManifestArtifacts(t *testing.T) []finalv5oracle.ProvSQLManifestArtifact {
	t.Helper()
	result := make([]finalv5oracle.ProvSQLManifestArtifact, 0, 105)
	for _, cell := range finalv5oracle.ProvSQLNonceJoinCells() {
		relative, err := finalv5oracle.ProvSQLNonceJoinManifestPath(cell.Scale, cell.Nonce)
		if err != nil {
			t.Fatal(err)
		}
		value, err := contractfs.FS.ReadFile(path.Join("oracle-manifests", relative))
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := finalv5oracle.DecodeManifest(value)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, finalv5oracle.ProvSQLManifestArtifact{RelativePath: relative,
			SHA256: shaBytes(value), Manifest: manifest})
	}
	return result
}
