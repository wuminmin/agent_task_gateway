package finalv5publication

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
)

// Set TASKGATE_E1_VALIDATION_FIXTURE to a freshly generated E1 output when
// running this test as an acceptance gate. Without it the always-on pure
// negative tests still run; this test returns without claiming live coverage.
func TestValidatePublicationOutputMutationMatrix(t *testing.T) {
	source := strings.TrimSpace(os.Getenv("TASKGATE_E1_VALIDATION_FIXTURE"))
	if source == "" {
		t.Skip("TASKGATE_E1_VALIDATION_FIXTURE is required for the generated-output mutation matrix")
	}
	root := repositoryRoot(t)
	if _, err := ValidatePublicationOutput(root, source, nil); err != nil {
		t.Fatalf("live-generated validation fixture is invalid: %v", err)
	}
	tests := []struct {
		name          string
		mutate        func(*testing.T, string)
		errorContains string
	}{
		{name: "placeholder", mutate: func(t *testing.T, directory string) {
			path := filepath.Join(directory, ProvenanceOutputName)
			value := readFile(t, path)
			var report ProvenanceReport
			if err := strictJSON(value, &report); err != nil {
				t.Fatal(err)
			}
			needle := []byte(`"sha256":"` + report.BindingInputIdentity.SHA256 + `"`)
			value = bytes.Replace(value, needle, []byte(`"sha256":"`+strings.Repeat("0", 64)+`"`), 1)
			writeFile(t, path, value)
		}},
		{name: "missing item", mutate: func(t *testing.T, directory string) {
			if err := os.Remove(filepath.Join(directory, BindingOutputName)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "byte drift", mutate: func(t *testing.T, directory string) {
			path := filepath.Join(directory, BindingOutputName)
			writeFile(t, path, append(readFile(t, path), '\n'))
		}},
		{name: "wrong mode", mutate: func(t *testing.T, directory string) {
			if err := os.Chmod(filepath.Join(directory, CatalogOutputName), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unknown field", mutate: func(t *testing.T, directory string) {
			path := filepath.Join(directory, ProvenanceOutputName)
			writeFile(t, path, insertBeforeFinalObjectEnd(t, readFile(t, path), `,"unknown_field":true`))
		}},
		{name: "credential leakage", mutate: func(t *testing.T, directory string) {
			path := filepath.Join(directory, ProvenanceOutputName)
			writeFile(t, path, insertBeforeFinalObjectEnd(t, readFile(t, path),
				`,"note":"postgres://reader:CredentialLeak_8cf1@example.test/travel_demo"`))
		}},
		{name: "coherent binding oracle digest drift", mutate: func(t *testing.T, directory string) {
			mutateCoherentBindingOracleDigests(t, directory)
		}},
		{name: "coherent binding Outcome member-set drift", mutate: func(t *testing.T, directory string) {
			mutateCoherentBindingOutcomeCandidate(t, directory)
		}, errorContains: "publication binding bytes differ from the exact source-controlled 12/6/105 model"},
		{name: "coherent Dataset probe source path drift", mutate: func(t *testing.T, directory string) {
			mutateCoherentProbeSource(t, root, directory)
		}},
		{name: "coherent Catalog attestation session drift", mutate: func(t *testing.T, directory string) {
			mutateCoherentCatalogSession(t, root, directory)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := copyPublicationOutput(t, source)
			test.mutate(t, directory)
			_, err := ValidatePublicationOutput(root, directory, []string{"CredentialLeak_8cf1"})
			if err == nil {
				t.Fatal("mutated publication output was accepted")
			}
			if test.errorContains != "" && !strings.Contains(err.Error(), test.errorContains) {
				t.Fatalf("mutation was rejected before independent regeneration: %v", err)
			}
		})
	}
}

type mutablePublicationBinding struct {
	DatasetSHA256      string                 `json:"dataset_sha256"`
	DatasetProbeSHA256 string                 `json:"dataset_probe_sha256"`
	CatalogSHA256      string                 `json:"catalog_sha256"`
	Adapter            finalv5binding.Section `json:"final_v5_adapter_v2"`
}

func mutateCoherentBindingOracleDigests(t *testing.T, directory string) {
	t.Helper()
	path := filepath.Join(directory, BindingOutputName)
	var document mutablePublicationBinding
	if err := strictJSON(readFile(t, path), &document); err != nil {
		t.Fatal(err)
	}
	scale := document.Adapter.Scale.DependencyE2E["10k-overlap-0"]
	scale.Candidate.DependencySetSHA256 = sha256Hex([]byte("coherent Scale set drift"))
	document.Adapter.Scale.DependencyE2E["10k-overlap-0"] = scale
	artifact := document.Adapter.Artifact.ResultHeavy["100x4"]
	artifact.Query.ExpectedResultSHA256 = sha256Hex([]byte("coherent Artifact result drift"))
	document.Adapter.Artifact.ResultHeavy["100x4"] = artifact
	for key, query := range document.Adapter.ProvSQL.TaskGate {
		query.DependencySetSHA256 = sha256Hex([]byte("coherent ProvSQL dependency drift"))
		document.Adapter.ProvSQL.TaskGate[key] = query
		break
	}
	value, err := canonicalJSON(document)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, value)
	rewriteReportForBinding(t, directory, sha256Hex(value), int64(len(value)))
}

func mutateCoherentBindingOutcomeCandidate(t *testing.T, directory string) {
	t.Helper()
	const scaleName = "10k-overlap-0"
	path := filepath.Join(directory, BindingOutputName)
	var document mutablePublicationBinding
	if err := strictJSON(readFile(t, path), &document); err != nil {
		t.Fatal(err)
	}
	cell, present := document.Adapter.Scale.DependencyE2E[scaleName]
	if !present {
		t.Fatalf("Scale cell %s is absent", scaleName)
	}
	members := append([]string(nil), cell.OutcomeCandidate.Members...)
	if len(members) != 5 {
		t.Fatalf("Scale cell %s has %d Outcome members", scaleName, len(members))
	}
	members[0] = sha256Hex([]byte("coherent but independently wrong Outcome member"))
	sort.Strings(members)
	summary, err := finalv5oracle.SummarizeSemanticSet("candidate", func(yield func(string) error) error {
		for _, member := range members {
			if err := yield(member); err != nil {
				return err
			}
		}
		return nil
	}, finalv5oracle.StreamSetOptions{MaxInMemoryMembers: len(members)})
	if err != nil {
		t.Fatal(err)
	}
	cell.OutcomeCandidate = finalv5binding.BoundOutcomeCandidateExpectation{
		Cardinality: summary.Cardinality, Members: members, OrdinarySetSHA256: summary.SetSHA256,
	}
	if err := finalv5binding.ValidateBoundOutcomeCandidate(cell.OutcomeCandidate); err != nil {
		t.Fatalf("coherently mutated Outcome candidate is internally invalid: %v", err)
	}
	document.Adapter.Scale.DependencyE2E[scaleName] = cell
	value, err := canonicalJSON(document)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, value)
	rewriteReportForOutcomeCandidate(t, directory, scaleName, cell.OutcomeCandidate,
		sha256Hex(value), int64(len(value)))
}

func rewriteReportForOutcomeCandidate(t *testing.T, directory, scaleName string,
	expected finalv5binding.BoundOutcomeCandidateExpectation, bindingSHA string, bindingBytes int64) {
	t.Helper()
	path := filepath.Join(directory, ProvenanceOutputName)
	var report ProvenanceReport
	if err := strictJSON(readFile(t, path), &report); err != nil {
		t.Fatal(err)
	}
	for index := range report.Outputs {
		if report.Outputs[index].Name == BindingOutputName {
			report.Outputs[index].SHA256 = bindingSHA
			report.Outputs[index].Bytes = bindingBytes
		}
	}
	found := false
	for cellIndex := range report.Inputs.Cells {
		cell := &report.Inputs.Cells[cellIndex]
		if cell.Workload != "scale/dependency-e2e" || cell.Cell != scaleName {
			continue
		}
		found = true
		updated := 0
		for identityIndex := range cell.Identities {
			identity := &cell.Identities[identityIndex]
			switch identity.Name {
			case "outcome_candidate_ordinary_set":
				identity.SHA256 = expected.OrdinarySetSHA256
				updated++
			case "outcome_candidate_member_01", "outcome_candidate_member_02",
				"outcome_candidate_member_03", "outcome_candidate_member_04",
				"outcome_candidate_member_05":
				memberIndex := int(identity.Name[len(identity.Name)-1] - '1')
				identity.SHA256 = expected.Members[memberIndex]
				updated++
			}
		}
		if updated != 6 {
			t.Fatalf("Scale provenance input %s retained %d Outcome identities; expected six", scaleName, updated)
		}
	}
	if !found {
		t.Fatalf("Scale provenance input %s is absent", scaleName)
	}
	report.Inputs.CellInputSetSHA256 = digestStructured(
		"TASKGATE-FINAL-V5-PUBLICATION-CELL-INPUT-SET-V1\x00", report.Inputs.Cells)
	identity, err := buildBindingInputIdentity(bindingSHA, report.BindingInputIdentity.CatalogSHA256,
		report.BindingInputIdentity.DatasetSHA256, report.BindingInputIdentity.DatasetProbeSHA256,
		report.LiveObservation.ObservationSHA256, report.SetAlgebra.SHA256, report.Inputs.CellInputSetSHA256)
	if err != nil {
		t.Fatal(err)
	}
	report.BindingInputIdentity = identity
	writeCanonicalReport(t, path, report)
}

func rewriteReportForBinding(t *testing.T, directory, bindingSHA string, bindingBytes int64) {
	t.Helper()
	path := filepath.Join(directory, ProvenanceOutputName)
	var report ProvenanceReport
	if err := strictJSON(readFile(t, path), &report); err != nil {
		t.Fatal(err)
	}
	for index := range report.Outputs {
		if report.Outputs[index].Name == BindingOutputName {
			report.Outputs[index].SHA256 = bindingSHA
			report.Outputs[index].Bytes = bindingBytes
		}
	}
	identity, err := buildBindingInputIdentity(bindingSHA, report.BindingInputIdentity.CatalogSHA256,
		report.BindingInputIdentity.DatasetSHA256, report.BindingInputIdentity.DatasetProbeSHA256,
		report.LiveObservation.ObservationSHA256, report.SetAlgebra.SHA256, report.Inputs.CellInputSetSHA256)
	if err != nil {
		t.Fatal(err)
	}
	report.BindingInputIdentity = identity
	writeCanonicalReport(t, path, report)
}

func mutateCoherentProbeSource(t *testing.T, root, directory string) {
	t.Helper()
	materials, err := loadGenerationMaterials(root)
	if err != nil {
		t.Fatal(err)
	}
	sourceSHA, err := materials.runtime.ContractSHA256(scaleCandidateDirectPath)
	if err != nil {
		t.Fatal(err)
	}
	rewriteLiveBoundReport(t, directory, materials, func(live *LiveObservation) {
		live.DatasetProbe.SourcePath = scaleCandidateDirectPath
		live.DatasetProbe.SourceSHA256 = sourceSHA
	})
}

func mutateCoherentCatalogSession(t *testing.T, root, directory string) {
	t.Helper()
	materials, err := loadGenerationMaterials(root)
	if err != nil {
		t.Fatal(err)
	}
	rewriteLiveBoundReport(t, directory, materials, func(live *LiveObservation) {
		live.Database += "_coherent_drift"
	})
}

func rewriteLiveBoundReport(t *testing.T, directory string, materials generationMaterials,
	mutate func(*LiveObservation)) {
	t.Helper()
	path := filepath.Join(directory, ProvenanceOutputName)
	var report ProvenanceReport
	if err := strictJSON(readFile(t, path), &report); err != nil {
		t.Fatal(err)
	}
	mutate(&report.LiveObservation)
	liveSHA, err := liveObservationDigest(report.LiveObservation)
	if err != nil {
		t.Fatal(err)
	}
	report.LiveObservation.ObservationSHA256 = liveSHA
	report.SetAlgebra, err = buildSetAlgebra(materials.scaleManifests, liveSHA)
	if err != nil {
		t.Fatal(err)
	}
	report.BindingInputIdentity, err = buildBindingInputIdentity(report.BindingInputIdentity.BindingSHA256,
		report.BindingInputIdentity.CatalogSHA256, report.BindingInputIdentity.DatasetSHA256,
		report.BindingInputIdentity.DatasetProbeSHA256, liveSHA, report.SetAlgebra.SHA256,
		report.Inputs.CellInputSetSHA256)
	if err != nil {
		t.Fatal(err)
	}
	writeCanonicalReport(t, path, report)
}

func writeCanonicalReport(t *testing.T, path string, report ProvenanceReport) {
	t.Helper()
	value, err := canonicalJSON(report)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, value)
}

func copyPublicationOutput(t *testing.T, source string) string {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "candidate")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range publicationOutputNames {
		writeFile(t, filepath.Join(destination, name), readFile(t, filepath.Join(source, name)))
	}
	return destination
}
