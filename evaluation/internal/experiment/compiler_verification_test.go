package experiment

import (
	"sort"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/compilerfixture"
)

func TestCompilerVerificationAcceptsExactlyFrozenMatrix(t *testing.T) {
	if len(compilerfixture.FrozenCells) != 11 {
		t.Fatalf("compiler cells = %d, want 11", len(compilerfixture.FrozenCells))
	}
	for _, cell := range compilerfixture.FrozenCells {
		t.Run(cell.WorkloadID+"/"+cell.Scale+"/"+cell.Mode, func(t *testing.T) {
			sample := validCompilerVerificationSample(t, cell)
			if err := validateCompilerVerification(sample); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompilerVerificationRejectsSelfConsistentMutations(t *testing.T) {
	base := validCompilerVerificationSample(t, compilerfixture.Cell{WorkloadID: "join-sources", Scale: "16", Mode: "compile"})
	mutations := []struct {
		name   string
		mutate func(*Sample)
	}{
		{name: "registry binding", mutate: func(value *Sample) { value.CompilerVerification.RegistrySHA256 = strings.Repeat("f", 64) }},
		{name: "dataset binding", mutate: func(value *Sample) { value.CompilerVerification.DatasetSHA256 = strings.Repeat("f", 64) }},
		{name: "source scale", mutate: func(value *Sample) { value.CompilerVerification.ObservedSources-- }},
		{name: "direct result", mutate: func(value *Sample) { value.CompilerVerification.DirectResultSHA256 = strings.Repeat("f", 64) }},
		{name: "missing variant", mutate: func(value *Sample) { value.CompilerVerification.Artifacts = value.CompilerVerification.Artifacts[1:] }},
		{name: "artifact binding", mutate: func(value *Sample) { value.CompilerVerification.Artifacts[0].BindingSHA256 = strings.Repeat("f", 64) }},
		{name: "semantic plan", mutate: func(value *Sample) {
			value.CompilerVerification.Artifacts[0].CanonicalPlanSHA256 = strings.Repeat("f", 64)
		}},
		{name: "repeat artifact", mutate: func(value *Sample) {
			for index := range value.CompilerVerification.Artifacts {
				if value.CompilerVerification.Artifacts[index].Name == "repeat" {
					value.CompilerVerification.Artifacts[index].ArtifactSHA256 = strings.Repeat("f", 64)
				}
			}
		}},
		{name: "top level plan", mutate: func(value *Sample) { value.QueryPlanSHA256 = strings.Repeat("f", 64) }},
		{name: "physical SQL", mutate: func(value *Sample) { value.PhysicalSQLSHA256 = strings.Repeat("f", 64) }},
		{name: "allocation counter", mutate: func(value *Sample) { value.Counters["alloc_objects"] = 0 }},
		{name: "timing overlap", mutate: func(value *Sample) { value.DiagnosticMS["digest_generation"] = value.DiagnosticMS["total"] + 1 }},
		{name: "Gateway phase leakage", mutate: func(value *Sample) { value.PipelineMS["artifact_stage"] = 1; value.PipelineMS["server_total"]++ }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			value := cloneCompilerVerificationSample(base)
			mutation.mutate(&value)
			if err := validateCompilerVerification(value); err == nil {
				t.Fatal("mutated compiler evidence was accepted")
			}
		})
	}
}

func TestCompilerLimitControlsRequireProductionCodeAndNoOutput(t *testing.T) {
	base := validCompilerVerificationSample(t, compilerfixture.Cell{WorkloadID: "limit-controls", Scale: "depth-17", Mode: "structured_rejection"})
	mutations := []struct {
		name   string
		mutate func(*Sample)
	}{
		{name: "obsolete code", mutate: func(value *Sample) {
			value.ErrorCode = "VIEW_DEPTH_LIMIT"
			value.CompilerVerification.StructuredErrorCode = "VIEW_DEPTH_LIMIT"
			value.CompilerVerification.AllocationErrorCode = "VIEW_DEPTH_LIMIT"
		}},
		{name: "wrong relation", mutate: func(value *Sample) {
			value.CompilerVerification.StructuredErrorRelationSHA256 = strings.Repeat("f", 64)
		}},
		{name: "not rejected", mutate: func(value *Sample) { value.Rejected = false }},
		{name: "result leaked", mutate: func(value *Sample) { value.ResultSHA256 = strings.Repeat("f", 64) }},
		{name: "artifact leaked", mutate: func(value *Sample) { value.ArtifactSHA256 = strings.Repeat("f", 64) }},
		{name: "allocation error differs", mutate: func(value *Sample) { value.CompilerVerification.AllocationErrorCode = "VIEW_SOURCE_LIMIT_EXCEEDED" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			value := cloneCompilerVerificationSample(base)
			mutation.mutate(&value)
			if err := validateCompilerVerification(value); err == nil {
				t.Fatal("mutated limit-control evidence was accepted")
			}
		})
	}
}

func validCompilerVerificationSample(t *testing.T, cell compilerfixture.Cell) Sample {
	t.Helper()
	expected, err := expectedCompilerVerification(cell.WorkloadID, cell.Scale, cell.Mode)
	if err != nil {
		t.Fatal(err)
	}
	sample := Sample{
		ExperimentID: "compiler", WorkloadID: cell.WorkloadID, Scale: cell.Scale, Mode: cell.Mode,
		System: "taskgate", Status: "pass", Counters: map[string]int64{"alloc_bytes": 128, "alloc_objects": 2},
		PipelineMS:        map[string]float64{"prepare": 0, "execute_and_derive": 1, "artifact_stage": 0, "control_settlement": 0, "artifact_publication": 0, "response_finalize": 0, "server_total": 1},
		DiagnosticMS:      map[string]float64{"total": 1, "recursive_expansion": .1, "parse_validation": .1, "compile_semantic": .1, "plan_materialization": .1, "digest_generation": .1},
		ClientAvailableMS: 1, ClientFullDrainMS: 1,
		CompilerVerification: &CompilerVerificationEvidence{
			FixtureVersion:   compilerfixture.Version,
			RegistrySHA256:   compilerfixture.RegistrySHA256(expected.fixture.Registry),
			ProductsSHA256:   compilerfixture.ProductsSHA256(expected.fixture.Products),
			FixtureSQLSHA256: compilerfixture.FixtureSQLSHA256, DatasetSHA256: expected.datasetSHA256,
			ExpectedDepth: expected.fixture.ExpectedDepth, ObservedDepth: expected.fixture.ExpectedDepth,
			ExpectedSources: expected.fixture.ExpectedSources, ObservedSources: expected.fixture.ExpectedSources,
			Artifacts: []CompilerArtifactEvidence{},
		},
	}
	if cell.Mode == "structured_rejection" {
		sample.ErrorCode = expected.errorCode
		sample.Rejected, sample.RejectedNoResult, sample.RejectedNoArtifact, sample.RejectedNoSuccessfulAudit = true, true, true, true
		sample.CompilerVerification.StructuredErrorCode = expected.errorCode
		sample.CompilerVerification.AllocationErrorCode = expected.errorCode
		sample.CompilerVerification.StructuredErrorRelationSHA256 = expected.errorRelationSHA256
		return sample
	}
	names := make([]string, 0, len(expected.artifacts))
	for name := range expected.artifacts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sample.CompilerVerification.Artifacts = append(sample.CompilerVerification.Artifacts, expected.artifacts[name])
	}
	measured := expected.artifacts["measured"]
	sample.RowCount, sample.ColumnCount, sample.ResultSHA256 = int64(len(expected.fixture.ExpectedRows)), len(expected.fixture.ExpectedRows[0]), expected.resultSHA256
	sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256 = expected.physicalSQLSHA256, expected.logicalSQLSHA256
	sample.QueryPlanSHA256, sample.ArtifactSHA256 = measured.CanonicalPlanSHA256, measured.ArtifactSHA256
	sample.CompilerVerification.DirectResultSHA256 = expected.resultSHA256
	sample.CompilerVerification.NestedResultSHA256 = expected.resultSHA256
	return sample
}

func cloneCompilerVerificationSample(value Sample) Sample {
	cloned := value
	verification := *value.CompilerVerification
	verification.Artifacts = append([]CompilerArtifactEvidence(nil), value.CompilerVerification.Artifacts...)
	cloned.CompilerVerification = &verification
	cloned.Counters = map[string]int64{}
	for key, item := range value.Counters {
		cloned.Counters[key] = item
	}
	cloned.PipelineMS = map[string]float64{}
	for key, item := range value.PipelineMS {
		cloned.PipelineMS[key] = item
	}
	cloned.DiagnosticMS = map[string]float64{}
	for key, item := range value.DiagnosticMS {
		cloned.DiagnosticMS[key] = item
	}
	return cloned
}
