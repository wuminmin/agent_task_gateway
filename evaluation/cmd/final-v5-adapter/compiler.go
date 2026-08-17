package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"taskbound.local/agent-data-gateway/evaluation/internal/compilerfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

const (
	compilerDSNEnv         = "TASKGATE_FINAL_V5_COMPILER_DSN"
	compilerDSNFallbackEnv = "TASKGATE_FINAL_V5_BUSINESS_DSN"
)

type compilerAdapter struct {
	oracle compilerResultOracle
	cache  map[string]compilerOracleResult
}

type compilerOracleResult struct {
	DirectResultSHA256 string
	NestedResultSHA256 string
	PhysicalSQLSHA256  string
	LogicalSQLSHA256   string
	Rows               int64
	Columns            int
}

type compilerResultOracle interface {
	DatasetSHA256() string
	Verify(context.Context, compilerfixture.Case, viewcompiler.Artifact) (compilerOracleResult, error)
	Close()
}

type postgresCompilerOracle struct {
	pool          *pgxpool.Pool
	datasetSHA256 string
}

// newCompilerAdapter is the production constructor used by the unified
// source-controlled adapter registry. It requires a live PostgreSQL 16 fixture;
// an absent DSN, missing table, or content drift fails construction so the main
// adapter emits an invalid sample instead of a self-asserted pass.
func newCompilerAdapter(ctx context.Context) (*compilerAdapter, error) {
	oracle, err := newPostgresCompilerOracle(ctx)
	if err != nil {
		return nil, err
	}
	adapter, err := newCompilerAdapterWithOracle(oracle)
	if err != nil {
		oracle.Close()
		return nil, err
	}
	return adapter, nil
}

func newCompilerAdapterWithOracle(oracle compilerResultOracle) (*compilerAdapter, error) {
	if oracle == nil {
		return nil, errors.New("compiler PostgreSQL oracle is required")
	}
	// Constructor-time exhaustion proves that every preregistered cell builds a
	// real viewcompiler.Compiler input and that both controls reach the exact
	// production rejection code before a capability can be registered.
	for _, cell := range compilerfixture.FrozenCells {
		one, err := compilerfixture.Build(cell.WorkloadID, cell.Scale, cell.Mode)
		if err != nil {
			return nil, err
		}
		compiler, err := one.NewCompiler()
		if err != nil {
			return nil, err
		}
		_, compileErr := compiler.Compile(one.MeasuredRoot)
		if cell.Mode == "compile" {
			if compileErr != nil {
				return nil, fmt.Errorf("compiler fixture %s/%s: %w", cell.WorkloadID, cell.Scale, compileErr)
			}
			continue
		}
		structured, ok := asCompilerError(compileErr)
		want := viewcompiler.CodeDepthLimit
		if cell.Scale == "sources-17" {
			want = viewcompiler.CodeSourceLimit
		}
		if !ok || structured.Code != want {
			return nil, fmt.Errorf("compiler control %s returned %v, want %s", cell.Scale, compileErr, want)
		}
	}
	return &compilerAdapter{oracle: oracle, cache: make(map[string]compilerOracleResult)}, nil
}

func (adapter *compilerAdapter) Close() {
	if adapter != nil && adapter.oracle != nil {
		adapter.oracle.Close()
	}
}

func (adapter *compilerAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if operation.ExperimentID != "compiler" || !compilerfixture.IsFrozenCell(operation.WorkloadID, operation.Scale, operation.Mode) {
		return invalidSample(operation, "unsupported_source_controlled_compiler_cell")
	}
	one, err := compilerfixture.Build(operation.WorkloadID, operation.Scale, operation.Mode)
	if err != nil {
		return invalidSample(operation, "compiler_fixture_invalid")
	}
	datasetSHA256, err := canonicalRowsSHA256(compilerfixture.DatasetRows())
	if err != nil || adapter == nil || adapter.oracle == nil || adapter.oracle.DatasetSHA256() != datasetSHA256 {
		return invalidSample(operation, "compiler_postgresql_fixture_invalid")
	}
	sample := baseSample(operation, "taskgate")
	sample.Counters = map[string]int64{"alloc_bytes": 0, "alloc_objects": 0}
	sample.CompilerVerification = &experiment.CompilerVerificationEvidence{
		FixtureVersion:   compilerfixture.Version,
		RegistrySHA256:   compilerfixture.RegistrySHA256(one.Registry),
		ProductsSHA256:   compilerfixture.ProductsSHA256(one.Products),
		FixtureSQLSHA256: compilerfixture.FixtureSQLSHA256,
		DatasetSHA256:    datasetSHA256,
		ExpectedDepth:    one.ExpectedDepth, ObservedDepth: one.ExpectedDepth,
		ExpectedSources: one.ExpectedSources, ObservedSources: one.ExpectedSources,
		Artifacts: []experiment.CompilerArtifactEvidence{},
	}
	compiler, err := one.NewCompiler()
	if err != nil {
		return compilerFailure(sample, "fail", "compiler_fixture_constructor_failed", err)
	}
	if operation.Mode == "structured_rejection" {
		return validateCompilerPass(adapter.executeCompilerControl(sample, compiler, one))
	}
	return validateCompilerPass(adapter.executeCompilerMeasurement(ctx, sample, compiler, one))
}

func validateCompilerPass(sample experiment.Sample) experiment.Sample {
	if sample.Status == "pass" {
		if err := experiment.ValidateCompilerEvidence(sample); err != nil {
			writeAdapterSampleFailureDiagnostic("compiler", sample, err)
			sample.Status = "fail"
			sample.ErrorCode = "compiler_evidence_invariant_failed"
			sample.Reason = "the retained real compiler sample failed its independent evidence invariant"
		}
	}
	return sample
}

func (adapter *compilerAdapter) executeCompilerMeasurement(ctx context.Context, sample experiment.Sample, compiler *viewcompiler.Compiler, one compilerfixture.Case) experiment.Sample {
	measured, metrics, measuredErr := compiler.CompileMeasured(one.MeasuredRoot)
	setCompilerTiming(&sample, metrics)
	if measuredErr != nil {
		if structured, ok := asCompilerError(measuredErr); ok {
			sample.CompilerVerification.StructuredErrorCode = string(structured.Code)
			sample.CompilerVerification.StructuredErrorRelationSHA256 = compilerRelationSHA256(structured.Relation)
		}
		return compilerFailure(sample, "fail", "compiler_supported_cell_rejected", measuredErr)
	}

	allocation, allocationMetrics, allocationErr := compiler.CompileAllocationMeasured(one.MeasuredRoot)
	if err := setCompilerAllocations(&sample, allocationMetrics); err != nil {
		return compilerFailure(sample, "invalid", "compiler_allocation_counter_overflow", err)
	}
	if allocationErr != nil {
		if structured, ok := asCompilerError(allocationErr); ok {
			sample.CompilerVerification.AllocationErrorCode = string(structured.Code)
		}
		return compilerFailure(sample, "fail", "compiler_allocation_run_failed", allocationErr)
	}

	repeat, repeatErr := compiler.Compile(one.MeasuredRoot)
	if repeatErr != nil {
		return compilerFailure(sample, "fail", "compiler_repeat_run_failed", repeatErr)
	}
	artifacts := map[string]viewcompiler.Artifact{"measured": measured, "repeat": repeat, "allocation": allocation}
	for name, root := range one.SemanticRoots {
		if name == "measured" {
			continue
		}
		compiled, err := compiler.Compile(root)
		if err != nil {
			return compilerFailure(sample, "fail", "compiler_semantic_variant_failed", fmt.Errorf("compile semantic variant %s: %w", name, err))
		}
		artifacts[name] = compiled
	}

	if compilerfixture.JSONSHA256(measured) != compilerfixture.JSONSHA256(repeat) ||
		compilerfixture.JSONSHA256(measured) != compilerfixture.JSONSHA256(allocation) {
		return compilerFailure(sample, "fail", "compiler_artifact_nondeterministic", errors.New("measured, repeat, and allocation artifacts differ"))
	}
	direct := artifacts["direct"]
	for name, artifact := range artifacts {
		if name == "repeat" || name == "allocation" {
			continue
		}
		if artifact.CanonicalPlanDigest != direct.CanonicalPlanDigest ||
			artifact.InterfaceDigest != direct.InterfaceDigest ||
			compilerfixture.JSONSHA256(artifact.Outputs) != compilerfixture.JSONSHA256(direct.Outputs) ||
			compilerfixture.JSONSHA256(artifact.BaseProducts) != compilerfixture.JSONSHA256(direct.BaseProducts) {
			return compilerFailure(sample, "fail", "compiler_semantic_variant_drift", fmt.Errorf("semantic variant %s differs from direct", name))
		}
	}

	names := make([]string, 0, len(artifacts))
	for name := range artifacts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		descriptor := compilerfixture.DescribeArtifact(artifacts[name], one.Registry)
		sample.CompilerVerification.Artifacts = append(sample.CompilerVerification.Artifacts, compilerArtifactEvidence(name, descriptor))
	}

	cacheKey := one.WorkloadID + "\x00" + one.Scale
	oracle, present := adapter.cache[cacheKey]
	if !present {
		var err error
		oracle, err = adapter.oracle.Verify(ctx, one, artifacts["nested"])
		if err != nil {
			return compilerFailure(sample, "invalid", "compiler_postgresql_oracle_failed", err)
		}
		adapter.cache[cacheKey] = oracle
	}
	expectedResult, err := canonicalRowsSHA256(one.ExpectedRows)
	if err != nil || oracle.DirectResultSHA256 != expectedResult || oracle.NestedResultSHA256 != expectedResult ||
		oracle.DirectResultSHA256 != oracle.NestedResultSHA256 || oracle.Rows != int64(len(one.ExpectedRows)) || oracle.Columns <= 0 {
		cause := err
		if cause == nil {
			cause = errors.New("PostgreSQL oracle result differs from the source-controlled expected rows")
		}
		return compilerFailure(sample, "fail", "compiler_postgresql_result_mismatch", cause)
	}

	descriptor := compilerfixture.DescribeArtifact(measured, one.Registry)
	sample.RowCount, sample.ColumnCount, sample.ResultSHA256 = oracle.Rows, oracle.Columns, oracle.NestedResultSHA256
	sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256 = oracle.PhysicalSQLSHA256, oracle.LogicalSQLSHA256
	sample.QueryPlanSHA256, sample.ArtifactSHA256 = descriptor.CanonicalPlanSHA256, descriptor.ArtifactSHA256
	sample.CompilerVerification.DirectResultSHA256 = oracle.DirectResultSHA256
	sample.CompilerVerification.NestedResultSHA256 = oracle.NestedResultSHA256
	sample.Status, sample.ErrorCode, sample.Reason = "pass", "", ""
	return sample
}

func (adapter *compilerAdapter) executeCompilerControl(sample experiment.Sample, compiler *viewcompiler.Compiler, one compilerfixture.Case) experiment.Sample {
	artifact, metrics, compileErr := compiler.CompileMeasured(one.MeasuredRoot)
	setCompilerTiming(&sample, metrics)
	allocationArtifact, allocationMetrics, allocationErr := compiler.CompileAllocationMeasured(one.MeasuredRoot)
	if err := setCompilerAllocations(&sample, allocationMetrics); err != nil {
		return compilerFailure(sample, "invalid", "compiler_allocation_counter_overflow", err)
	}
	if compileErr == nil || allocationErr == nil {
		if compileErr == nil {
			descriptor := compilerfixture.DescribeArtifact(artifact, one.Registry)
			sample.CompilerVerification.Artifacts = append(sample.CompilerVerification.Artifacts, compilerArtifactEvidence("unexpected", descriptor))
		}
		if allocationErr == nil {
			descriptor := compilerfixture.DescribeArtifact(allocationArtifact, one.Registry)
			sample.CompilerVerification.Artifacts = append(sample.CompilerVerification.Artifacts, compilerArtifactEvidence("unexpected_allocation", descriptor))
		}
		return compilerFailure(sample, "fail", "compiler_limit_control_unexpected_success", errors.New("compiler limit control unexpectedly succeeded"))
	}
	structured, compileOK := asCompilerError(compileErr)
	allocationStructured, allocationOK := asCompilerError(allocationErr)
	if compileOK {
		sample.CompilerVerification.StructuredErrorCode = string(structured.Code)
		sample.CompilerVerification.StructuredErrorRelationSHA256 = compilerRelationSHA256(structured.Relation)
		sample.ErrorCode = string(structured.Code)
	}
	if allocationOK {
		sample.CompilerVerification.AllocationErrorCode = string(allocationStructured.Code)
	}
	want := viewcompiler.CodeDepthLimit
	if one.Scale == "sources-17" {
		want = viewcompiler.CodeSourceLimit
	}
	if !compileOK || !allocationOK || structured.Code != want || allocationStructured.Code != want || structured.Relation != allocationStructured.Relation {
		return compilerFailure(sample, "fail", "compiler_limit_control_wrong_error", fmt.Errorf("compiler limit control returned compile=%v allocation=%v, want %s", compileErr, allocationErr, want))
	}
	sample.Rejected = true
	sample.RejectedNoResult = true
	sample.RejectedNoArtifact = true
	sample.RejectedNoSuccessfulAudit = true
	sample.Status, sample.Reason = "pass", ""
	return sample
}

func compilerFailure(sample experiment.Sample, status, code string, cause error) experiment.Sample {
	writeAdapterSampleFailureDiagnostic("compiler", sample, cause)
	sample.Status, sample.ErrorCode = status, code
	sample.Reason = "source-controlled compiler adapter failed closed; detailed diagnostics remain outside the evidence channel"
	return sample
}

func setCompilerTiming(sample *experiment.Sample, metrics viewcompiler.CompileMetrics) {
	total := durationMS(metrics.Total)
	sample.ClientAvailableMS, sample.ClientFullDrainMS, sample.GenerationBoundaryMS = total, total, total
	sample.PipelineMS = zeroPipeline()
	sample.PipelineMS["execute_and_derive"], sample.PipelineMS["server_total"] = total, total
	sample.DiagnosticMS = map[string]float64{
		"total": total, "recursive_expansion": durationMS(metrics.RecursiveExpansion),
		"parse_validation": durationMS(metrics.ParseValidation), "compile_semantic": durationMS(metrics.CompileSemantic),
		"plan_materialization": durationMS(metrics.PlanMaterialization), "digest_generation": durationMS(metrics.DigestGeneration),
	}
}

func setCompilerAllocations(sample *experiment.Sample, metrics viewcompiler.CompileMetrics) error {
	if metrics.AllocBytes > math.MaxInt64 || metrics.AllocObjects > math.MaxInt64 {
		return errors.New("compiler allocation counter exceeds int64")
	}
	if sample.Counters == nil {
		sample.Counters = map[string]int64{}
	}
	sample.Counters["alloc_bytes"] = int64(metrics.AllocBytes)
	sample.Counters["alloc_objects"] = int64(metrics.AllocObjects)
	return nil
}

func compilerArtifactEvidence(name string, value compilerfixture.ArtifactDescriptor) experiment.CompilerArtifactEvidence {
	return experiment.CompilerArtifactEvidence{
		Name: name, ArtifactSHA256: value.ArtifactSHA256, DefinitionSHA256: value.DefinitionSHA256,
		DependencySHA256: value.DependencySHA256, InterfaceSHA256: value.InterfaceSHA256,
		CanonicalPlanSHA256: value.CanonicalPlanSHA256, BindingSHA256: value.BindingSHA256,
		OutputsSHA256: value.OutputsSHA256, BaseProductsSHA256: value.BaseProductsSHA256,
		ReachableRelations: value.ReachableRelations, DependencyEdges: value.DependencyEdges,
		ExpandedSources: value.ExpandedSources, DefinitionBytes: value.DefinitionBytes,
		CanonicalPlanBytes: value.CanonicalPlanBytes,
	}
}

func asCompilerError(err error) (*viewcompiler.Error, bool) {
	var structured *viewcompiler.Error
	if !errors.As(err, &structured) || structured == nil {
		return nil, false
	}
	return structured, true
}

func compilerRelationSHA256(name viewcompiler.RelationName) string {
	return compilerfixture.SHA256String(compilerfixture.Version + "\x00relation\x00" + name.String())
}

func canonicalRowsSHA256(rows [][]any) (string, error) {
	return experiment.CanonicalResultHash(rows)
}

func newPostgresCompilerOracle(ctx context.Context) (*postgresCompilerOracle, error) {
	dsn := strings.TrimSpace(os.Getenv(compilerDSNEnv))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv(compilerDSNFallbackEnv))
	}
	if dsn == "" {
		return nil, fmt.Errorf("%s or %s is required", compilerDSNEnv, compilerDSNFallbackEnv)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	config.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*postgresCompilerOracle, error) {
		pool.Close()
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return closeOnError(err)
	}
	var versionNumber string
	if err := pool.QueryRow(ctx, "SHOW server_version_num").Scan(&versionNumber); err != nil {
		return closeOnError(err)
	}
	version, err := strconv.Atoi(versionNumber)
	if err != nil || version/10000 != compilerfixture.PostgreSQLMajor {
		return closeOnError(fmt.Errorf("compiler fixture PostgreSQL major is %q, want %d", versionNumber, compilerfixture.PostgreSQLMajor))
	}
	rows, _, err := queryCompilerRows(ctx, pool, compilerfixture.DatasetQuery())
	if err != nil {
		return closeOnError(err)
	}
	actualDataset, err := canonicalRowsSHA256(rows)
	if err != nil {
		return closeOnError(err)
	}
	expectedDataset, err := canonicalRowsSHA256(compilerfixture.DatasetRows())
	if err != nil || actualDataset != expectedDataset {
		return closeOnError(errors.New("live compiler fixture differs from its source-controlled dataset"))
	}
	return &postgresCompilerOracle{pool: pool, datasetSHA256: actualDataset}, nil
}

func (oracle *postgresCompilerOracle) DatasetSHA256() string { return oracle.datasetSHA256 }
func (oracle *postgresCompilerOracle) Close() {
	if oracle != nil && oracle.pool != nil {
		oracle.pool.Close()
	}
}

func (oracle *postgresCompilerOracle) Verify(ctx context.Context, one compilerfixture.Case, nested viewcompiler.Artifact) (compilerOracleResult, error) {
	compiled, err := queryplan.CompileRelational(nested.Plan, one.Products)
	if err != nil {
		return compilerOracleResult{}, err
	}
	tx, err := oracle.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return compilerOracleResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = "+compilerfixture.SchemaName+", pg_catalog"); err != nil {
		return compilerOracleResult{}, err
	}
	directRows, directColumns, err := queryCompilerTxRows(ctx, tx, one.DirectSQL)
	if err != nil {
		return compilerOracleResult{}, err
	}
	nestedRows, nestedColumns, err := queryCompilerTxRows(ctx, tx, compiled.VisibleSQL)
	if err != nil {
		return compilerOracleResult{}, err
	}
	if directColumns != nestedColumns || len(directRows) != len(nestedRows) {
		return compilerOracleResult{}, errors.New("direct and nested compiler oracle shapes differ")
	}
	directDigest, err := canonicalRowsSHA256(directRows)
	if err != nil {
		return compilerOracleResult{}, err
	}
	nestedDigest, err := canonicalRowsSHA256(nestedRows)
	if err != nil {
		return compilerOracleResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return compilerOracleResult{}, err
	}
	return compilerOracleResult{
		DirectResultSHA256: directDigest, NestedResultSHA256: nestedDigest,
		PhysicalSQLSHA256: compilerfixture.SHA256String(one.DirectSQL),
		LogicalSQLSHA256:  compilerfixture.SHA256String(compiled.VisibleSQL),
		Rows:              int64(len(nestedRows)), Columns: nestedColumns,
	}, nil
}

func queryCompilerRows(ctx context.Context, pool *pgxpool.Pool, sqlText string) ([][]any, int, error) {
	rows, err := pool.Query(ctx, sqlText)
	if err != nil {
		return nil, 0, err
	}
	return collectCompilerRows(rows)
}

func queryCompilerTxRows(ctx context.Context, tx pgx.Tx, sqlText string) ([][]any, int, error) {
	rows, err := tx.Query(ctx, sqlText)
	if err != nil {
		return nil, 0, err
	}
	return collectCompilerRows(rows)
}

func collectCompilerRows(rows pgx.Rows) ([][]any, int, error) {
	defer rows.Close()
	columns := len(rows.FieldDescriptions())
	var result [][]any
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, 0, err
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return result, columns, nil
}
