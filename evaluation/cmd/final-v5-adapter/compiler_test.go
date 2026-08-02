package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/compilerfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

type fakeCompilerOracle struct {
	datasetSHA256 string
	err           error
	verifyCalls   int
	closed        bool
}

func (oracle *fakeCompilerOracle) DatasetSHA256() string { return oracle.datasetSHA256 }
func (oracle *fakeCompilerOracle) Close()                { oracle.closed = true }
func (oracle *fakeCompilerOracle) Verify(_ context.Context, one compilerfixture.Case, nested viewcompiler.Artifact) (compilerOracleResult, error) {
	oracle.verifyCalls++
	if oracle.err != nil {
		return compilerOracleResult{}, oracle.err
	}
	resultSHA256, err := experiment.CanonicalResultHash(one.ExpectedRows)
	if err != nil {
		return compilerOracleResult{}, err
	}
	compiled, err := queryplan.CompileRelational(nested.Plan, one.Products)
	if err != nil {
		return compilerOracleResult{}, err
	}
	return compilerOracleResult{
		DirectResultSHA256: resultSHA256, NestedResultSHA256: resultSHA256,
		PhysicalSQLSHA256: compilerfixture.SHA256String(one.DirectSQL),
		LogicalSQLSHA256:  compilerfixture.SHA256String(compiled.VisibleSQL),
		Rows:              int64(len(one.ExpectedRows)), Columns: len(one.ExpectedRows[0]),
	}, nil
}

func TestCompilerAdapterExhaustsFrozenElevenCells(t *testing.T) {
	datasetSHA256, err := experiment.CanonicalResultHash(compilerfixture.DatasetRows())
	if err != nil {
		t.Fatal(err)
	}
	oracle := &fakeCompilerOracle{datasetSHA256: datasetSHA256}
	adapter, err := newCompilerAdapterWithOracle(oracle)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	if len(compilerfixture.FrozenCells) != 11 {
		t.Fatalf("frozen cells = %d, want 11", len(compilerfixture.FrozenCells))
	}
	compileCells := 0
	for index, cell := range compilerfixture.FrozenCells {
		operation := compilerTestOperation(index+1, cell)
		sample := adapter.Execute(context.Background(), operation)
		if err := sample.Validate(); err != nil {
			t.Fatalf("%+v invalid sample: %v", cell, err)
		}
		if sample.Status != "pass" || sample.CompilerVerification == nil ||
			sample.CompilerVerification.RegistrySHA256 == "" || sample.CompilerVerification.DatasetSHA256 != datasetSHA256 {
			t.Fatalf("%+v did not pass with bound evidence: %+v", cell, sample)
		}
		if cell.Mode == "structured_rejection" {
			want := string(viewcompiler.CodeDepthLimit)
			if cell.Scale == "sources-17" {
				want = string(viewcompiler.CodeSourceLimit)
			}
			if !sample.Rejected || sample.ErrorCode != want || sample.CompilerVerification.StructuredErrorCode != want ||
				sample.CompilerVerification.AllocationErrorCode != want || len(sample.CompilerVerification.Artifacts) != 0 {
				t.Fatalf("%+v control evidence = %+v", cell, sample.CompilerVerification)
			}
			continue
		}
		compileCells++
		one, _ := compilerfixture.Build(cell.WorkloadID, cell.Scale, cell.Mode)
		if sample.Rejected || sample.ResultSHA256 == "" || sample.ArtifactSHA256 == "" ||
			len(sample.CompilerVerification.Artifacts) != len(one.ExpectedVariantNames()) ||
			sample.Counters["alloc_bytes"] <= 0 || sample.Counters["alloc_objects"] <= 0 {
			t.Fatalf("%+v measurement evidence is incomplete", cell)
		}
	}
	if oracle.verifyCalls != compileCells {
		t.Fatalf("PostgreSQL oracle calls = %d, want one per compile cell (%d)", oracle.verifyCalls, compileCells)
	}
}

func TestCompilerAdapterFailsClosedOnUnsupportedOrUnavailableOracle(t *testing.T) {
	datasetSHA256, err := experiment.CanonicalResultHash(compilerfixture.DatasetRows())
	if err != nil {
		t.Fatal(err)
	}
	cell := compilerfixture.FrozenCells[0]
	operation := compilerTestOperation(1, cell)

	wrongDataset, err := newCompilerAdapterWithOracle(&fakeCompilerOracle{datasetSHA256: compilerfixture.SHA256String("wrong")})
	if err != nil {
		t.Fatal(err)
	}
	if sample := wrongDataset.Execute(context.Background(), operation); sample.Status != "invalid" || sample.ErrorCode != "compiler_postgresql_fixture_invalid" {
		t.Fatalf("wrong live fixture was not invalid: %+v", sample)
	}

	failing, err := newCompilerAdapterWithOracle(&fakeCompilerOracle{datasetSHA256: datasetSHA256, err: errors.New("backend unavailable")})
	if err != nil {
		t.Fatal(err)
	}
	if sample := failing.Execute(context.Background(), operation); sample.Status != "invalid" || sample.ErrorCode != "compiler_postgresql_oracle_failed" || sample.CompilerVerification == nil {
		t.Fatalf("oracle failure was not retained as invalid with partial evidence: %+v", sample)
	}

	unsupported := operation
	unsupported.Scale = "17"
	unsupported.CellID = "view-depth/17/compile"
	if sample := failing.Execute(context.Background(), unsupported); sample.Status != "invalid" || sample.ErrorCode != "unsupported_source_controlled_compiler_cell" {
		t.Fatalf("unsupported cell was accepted: %+v", sample)
	}
}

func TestCompilerInvariantFailureRetainsCompletedEvidence(t *testing.T) {
	operation := experiment.AdapterOperation{SchemaVersion: 1, CampaignID: "campaign", DeploymentID: "deployment-01",
		ExperimentID: "compiler", CellID: "cell", SampleID: "sample", Iteration: 1, ProcessReplicate: 1,
		OrderPosition: 1, RandomSeed: 1, PairID: "pair", PairedSystemOrder: "compile", RootGroupID: "compile",
		WorkloadID: "join-sources", Scale: "2", Mode: "compile"}
	sample := baseSample(operation, "taskgate")
	sample.Status = "pass"
	sample.RowCount = 9
	sample.ResultSHA256 = strings.Repeat("a", 64)
	sample.Counters = map[string]int64{"retained_marker": 17}
	sample.CompilerVerification = &experiment.CompilerVerificationEvidence{FixtureVersion: "mutated"}
	failed := validateCompilerPass(sample)
	if failed.Status != "fail" || failed.ErrorCode != "compiler_evidence_invariant_failed" ||
		failed.RowCount != 9 || failed.ResultSHA256 != strings.Repeat("a", 64) ||
		failed.Counters["retained_marker"] != 17 || failed.CompilerVerification == nil {
		t.Fatalf("compiler invariant failure discarded completed evidence: %+v", failed)
	}
}

func TestLiveCompilerPostgreSQLFixture(t *testing.T) {
	if os.Getenv(compilerDSNEnv) == "" && os.Getenv(compilerDSNFallbackEnv) == "" {
		t.Skip("live compiler PostgreSQL DSN is not configured")
	}
	adapter, err := newCompilerAdapter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	for index, cell := range compilerfixture.FrozenCells {
		sample := adapter.Execute(context.Background(), compilerTestOperation(index+1, cell))
		if err := sample.Validate(); err != nil {
			t.Fatalf("live compiler sample %+v is invalid: %v", cell, err)
		}
		if sample.Status != "pass" {
			t.Fatalf("live compiler sample %+v = %+v", cell, sample)
		}
	}
}

func compilerTestOperation(index int, cell compilerfixture.Cell) experiment.AdapterOperation {
	return experiment.AdapterOperation{
		SchemaVersion: 1, CampaignClass: "publication", CampaignID: "compiler-test",
		DeploymentID: "deployment-01", ExperimentID: "compiler",
		CellID:   cell.WorkloadID + "/" + cell.Scale + "/" + cell.Mode,
		SampleID: "sample-" + string(rune('a'+index)), Iteration: 1, ProcessReplicate: 1,
		OrderPosition: index, RandomSeed: 20260801, PairID: "pair-compiler",
		PairedSystemOrder: cell.Mode, RootGroupID: cell.Mode,
		WorkloadID: cell.WorkloadID, Scale: cell.Scale, Mode: cell.Mode,
	}
}
