package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

const provSQLVerificationVersion = "taskgate-final-v5-provsql-verification-v2"

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type provSQLSystem struct {
	PostgreSQLVersion    string
	PostgreSQLVersionNum string
	StatementTimeoutMS   int64
	MaxParallelWorkers   int64
	ClientMinMessages    string
	LogMinMessages       string
	ProvSQLVersion       string
	ProvSQLCommit        string
	SharedPreload        bool
	AggTokenTextAsUUID   bool
	AggTokenOID          uint32
	UUIDOID              uint32
}

type provSQLMetrics struct {
	Gates         int64
	ArtifactBytes int64
}

type provSQLExecution struct {
	AvailableMS       float64
	FullDrainMS       float64
	Rows              int64
	Columns           int
	ResultSHA256      string
	TypedDrainFields  int64
	TypedDrainSHA256  string
	FieldOIDs         []uint32
	AggregateTokens   int64
	RowTokens         int64
	Before            provSQLMetrics
	After             provSQLMetrics
	RepresentationSHA string
	RootTypesVerified bool
	BaseTupleLink     *experiment.ProvSQLBaseTupleLinkV1
}

type provSQLAggregateRow struct {
	status int64
	roots  [provsqlfixture.CarrierColumns]string
}

type provSQLSequenceState struct {
	initialized         bool
	lastGates           int64
	lastBytes           int64
	seenNonces          map[int64]bool
	seenRepresentations map[string]bool
}

type provSQLAdapter struct {
	real          *realAdapter
	direct        *pgx.Conn
	provsql       *pgx.Conn
	binding       adapterDeploymentBinding
	directSystem  provSQLSystem
	provSQLSystem provSQLSystem
	datasetSHA256 string
	sequence      provSQLSequenceState

	// Only the TaskGate arm is governed by the v3 acceptance authority. Keep it
	// lazy so the raw PostgreSQL and native ProvSQL controls remain independent
	// of the Catalog, qualification, and Control-Store inputs they never use.
	finalizerOnce sync.Once
	finalizer     *experiment.RuntimeFinalizerV3
	finalizerErr  error
}

// newProvSQLAdapter wires three independent real systems: pinned direct
// PostgreSQL, pinned ProvSQL PostgreSQL, and TaskGate's public V8/Parquet path.
// Missing DSNs, fixtures, extensions, private task bindings, or live dataset
// attestations fail initialization; capability registration is source-static
// and does not turn such an invalid environment into a passing sample.
func newProvSQLAdapter(ctx context.Context) (sourceControlledAdapter, error) {
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	real.timeout = 30 * time.Minute
	real.http.Timeout = real.timeout
	closeReal := true
	defer func() {
		if closeReal {
			real.Close()
		}
	}()

	binding, err := loadAdapterDeploymentBinding()
	if err != nil || validateProvSQLDeploymentBinding(binding.Section.ProvSQL) != nil {
		return nil, errors.New("strict ProvSQL deployment binding is unavailable")
	}
	directDSN := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_DIRECT_DSN"))
	provSQLDSN := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_PROVSQL_DSN"))
	if directDSN == "" || provSQLDSN == "" || directDSN == provSQLDSN {
		return nil, errors.New("distinct direct and ProvSQL DSNs are required")
	}
	direct, err := pgx.Connect(ctx, directDSN)
	if err != nil {
		return nil, err
	}
	closeDirect := true
	defer func() {
		if closeDirect {
			_ = direct.Close(context.Background())
		}
	}()
	prov, err := pgx.Connect(ctx, provSQLDSN)
	if err != nil {
		return nil, err
	}
	closeProv := true
	defer func() {
		if closeProv {
			_ = prov.Close(context.Background())
		}
	}()
	if err := configureProvSQLSession(ctx, direct, false); err != nil {
		return nil, fmt.Errorf("configure direct PostgreSQL: %w", err)
	}
	if err := configureProvSQLSession(ctx, prov, true); err != nil {
		return nil, fmt.Errorf("configure ProvSQL PostgreSQL: %w", err)
	}
	directSystem, err := inspectProvSQLSystem(ctx, direct, false)
	if err != nil {
		return nil, err
	}
	provSystem, err := inspectProvSQLSystem(ctx, prov, true)
	if err != nil {
		return nil, err
	}
	if err := validateProvSQLSystems(directSystem, provSystem); err != nil {
		return nil, err
	}
	expectedDataset := binding.Section.ProvSQL.DatasetSHA256
	directDataset, err := fingerprintProvSQLDataset(ctx, direct, false)
	if err != nil || directDataset != expectedDataset {
		return nil, errors.New("direct PostgreSQL dataset differs from the frozen fixture")
	}
	provDataset, err := fingerprintProvSQLDataset(ctx, prov, true)
	if err != nil || provDataset != expectedDataset {
		return nil, errors.New("ProvSQL dataset differs from the frozen fixture")
	}
	businessDataset, err := fingerprintProvSQLDatasetRows(ctx, real.business, provsqlfixture.BusinessDatasetFingerprintSQL)
	if err != nil || businessDataset != expectedDataset {
		return nil, errors.New("TaskGate Business dataset differs from the frozen fixture")
	}
	closeReal, closeDirect, closeProv = false, false, false
	return &provSQLAdapter{
		real: real, direct: direct, provsql: prov, binding: binding,
		directSystem: directSystem, provSQLSystem: provSystem, datasetSHA256: expectedDataset,
		sequence: provSQLSequenceState{seenNonces: map[int64]bool{}, seenRepresentations: map[string]bool{}},
	}, nil
}

func (adapter *provSQLAdapter) Close() {
	if adapter.direct != nil {
		_ = adapter.direct.Close(context.Background())
	}
	if adapter.provsql != nil {
		_ = adapter.provsql.Close(context.Background())
	}
	if adapter.real != nil {
		adapter.real.Close()
	}
}

func (adapter *provSQLAdapter) provSQLFinalizer(ctx context.Context) (*experiment.RuntimeFinalizerV3, error) {
	adapter.finalizerOnce.Do(func() {
		adapter.finalizer, adapter.finalizerErr = experiment.OpenDeploymentFinalizerV3(ctx)
		if adapter.finalizerErr != nil {
			adapter.finalizerErr = fmt.Errorf("open the ProvSQL v3 acceptance authority: %w", adapter.finalizerErr)
		}
	})
	return adapter.finalizer, adapter.finalizerErr
}

func (adapter *provSQLAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if !validProvSQLCell(operation) {
		return invalidSample(operation, "unsupported_frozen_provsql_cell")
	}
	spec, _ := provsqlfixture.ParseScale(operation.Scale)
	nonce, err := provsqlfixture.Nonce(operation.Scale, operation.ProcessReplicate, operation.Iteration, operation.Warmup)
	if err != nil {
		return invalidSample(operation, "provsql_nonce_allocation_invalid")
	}
	expected, err := validateProvSQLCellBinding(adapter.binding.Section.ProvSQL, operation.Scale, nonce)
	if err != nil {
		return invalidSample(operation, "provsql_cell_binding_invalid")
	}
	var sample experiment.Sample
	switch operation.Mode {
	case "direct":
		execution, executeErr := executeProvSQLExternal(ctx, adapter.direct, spec, nonce, false, adapter.directSystem)
		if executeErr != nil {
			return adapter.retainedProvSQLFailure(operation, expected, spec, nonce, "direct_complete_typed_drain",
				adapter.directSystem, execution, "external_query_or_drain", "provsql_direct_measurement_failed", executeErr)
		}
		sample = externalProvSQLSample(operation, execution, "postgresql", provsqlfixture.PhysicalSQL)
		sample.ProvSQLVerification = adapter.provSQLVerification(operation, expected, spec, nonce,
			"direct_complete_typed_drain", adapter.directSystem, execution)
	case "provsql":
		execution, executeErr := executeProvSQLExternal(ctx, adapter.provsql, spec, nonce, true, adapter.provSQLSystem)
		if executeErr != nil {
			return adapter.retainedProvSQLFailure(operation, expected, spec, nonce, "provsql_complete_typed_drain",
				adapter.provSQLSystem, execution, "external_query_or_circuit_verification", "provsql_circuit_measurement_failed", executeErr)
		}
		if sequenceErr := adapter.validateAndAdvanceProvSQLSequence(nonce, execution); sequenceErr != nil {
			return adapter.retainedProvSQLFailure(operation, expected, spec, nonce, "provsql_complete_typed_drain",
				adapter.provSQLSystem, execution, "cross_operation_sequence", "provsql_circuit_measurement_failed", sequenceErr)
		}
		sample = externalProvSQLSample(operation, execution, "provsql", provsqlfixture.PhysicalSQL)
		sample.ProvSQLVerification = adapter.provSQLVerification(operation, expected, spec, nonce,
			"provsql_complete_typed_drain", adapter.provSQLSystem, execution)
	case "taskgate":
		var executeErr error
		sample, executeErr = adapter.executeProvSQLTaskGate(ctx, operation, expected)
		retainedSnapshots := sample.ProvSQLVerification
		if executeErr != nil {
			execution := provSQLExecution{Rows: sample.RowCount, Columns: sample.ColumnCount,
				ResultSHA256: sample.ResultSHA256, TypedDrainFields: sample.RowCount * int64(sample.ColumnCount),
				TypedDrainSHA256: sample.ResultSHA256}
			failed := adapter.retainedProvSQLFailure(operation, expected, spec, nonce, "taskgate_released_parquet_v8",
				provSQLSystem{}, execution, "taskgate_query_or_verifier", "provsql_taskgate_measurement_failed", executeErr)
			copyProvSQLPartialSample(&failed, sample)
			failed.ProvSQLVerification = adapter.provSQLVerification(operation, expected, spec, nonce,
				"taskgate_released_parquet_v8", provSQLSystem{}, execution)
			copyProvSQLTaskGateSnapshots(failed.ProvSQLVerification, retainedSnapshots)
			failed.ProvSQLVerification.FailureStage = "taskgate_query_or_verifier"
			return retainTaskGateRejection(failed, executeErr)
		}
		execution := provSQLExecution{Rows: sample.RowCount, Columns: sample.ColumnCount,
			ResultSHA256: sample.ResultSHA256, TypedDrainFields: sample.RowCount * int64(sample.ColumnCount),
			TypedDrainSHA256: sample.ResultSHA256}
		sample.ProvSQLVerification = adapter.provSQLVerification(operation, expected, spec, nonce,
			"taskgate_released_parquet_v8", provSQLSystem{}, execution)
		copyProvSQLTaskGateSnapshots(sample.ProvSQLVerification, retainedSnapshots)
	}
	validate := experiment.ValidateProvSQLEvidence
	if operation.Warmup {
		validate = experiment.ValidateProvSQLWarmupEvidence
	}
	if err := validate(sample); err != nil {
		return retainedProvSQLInvariantFailure(sample, err)
	}
	return sample
}

func validProvSQLCell(operation experiment.AdapterOperation) bool {
	if operation.ExperimentID != "provsql" || operation.WorkloadID != "nonce-join-group" ||
		(operation.Mode != "direct" && operation.Mode != "provsql" && operation.Mode != "taskgate") {
		return false
	}
	_, err := provsqlfixture.ParseScale(operation.Scale)
	return err == nil
}

func copyProvSQLTaskGateSnapshots(target, source *experiment.ProvSQLVerificationEvidence) {
	if target == nil || source == nil {
		return
	}
	target.BusinessBefore, target.BusinessAfter = source.BusinessBefore, source.BusinessAfter
	target.RootBefore, target.RootAfter = source.RootBefore, source.RootAfter
	target.ObserverWindow = source.ObserverWindow
	target.DependencyLink = source.DependencyLink
	if source.DependencyLink != nil {
		target.Version = source.Version
	}
}

func retainedProvSQLInvariantFailure(sample experiment.Sample, cause error) experiment.Sample {
	writeAdapterSampleFailureDiagnostic("provsql", sample, cause)
	sample.Status = "fail"
	sample.ErrorCode = "provsql_evidence_invariant_failed"
	sample.Reason = "the retained real ProvSQL sample failed its independent evidence invariant"
	if sample.ProvSQLVerification != nil {
		sample.ProvSQLVerification.FailureStage = "adapter_evidence_invariant"
	}
	return sample
}

func (adapter *provSQLAdapter) provSQLVerification(operation experiment.AdapterOperation, expected boundQueryExpectation,
	spec provsqlfixture.Scale, nonce int64, boundary string, system provSQLSystem, execution provSQLExecution) *experiment.ProvSQLVerificationEvidence {
	nonceBinding, _ := provsqlfixture.NonceBindingSHA256(operation.Scale, operation.ProcessReplicate, operation.Iteration, operation.Warmup)
	logical, _ := provsqlfixture.LogicalSQL(operation.Scale, nonce)
	physicalSHA := provsqlfixture.PhysicalSQLSHA256()
	if operation.Mode == "taskgate" {
		physicalSHA = sha(logical)
	}
	return &experiment.ProvSQLVerificationEvidence{
		Version: "taskgate-final-v5-provsql-verification-v1", Boundary: boundary,
		BindingFileSHA256: adapter.binding.FileSHA256, BindingSHA256: adapter.binding.SectionSHA256,
		FixtureVersion: provsqlfixture.Version, FixtureSQLSHA256: provsqlfixture.FixtureSQLSHA256(),
		EnableSQLSHA256: provsqlfixture.EnableSQLSHA256(), DatasetSHA256: adapter.datasetSHA256,
		DatasetProbeSQLSHA256: provsqlfixture.DatasetProbeSQLSHA256(), DatasetRows: provsqlfixture.DatasetRowCount,
		BusinessDatasetProbeSQLSHA256: provsqlfixture.BusinessDatasetProbeSQLSHA256(),
		ScaleLimit:                    spec.Limit, Nonce: nonce, Warmup: operation.Warmup, NonceBindingSHA256: nonceBinding,
		PhysicalSQLSHA256: physicalSHA, LogicalSQLSHA256: sha(logical),
		CacheConditionSHA256: sha(provsqlfixture.Version + "\x00warm-after-complete-typed-dataset-fingerprint"),
		ExecutionOrderSHA256: sha(strings.Join([]string{provsqlfixture.Version, "execution-order-v2", operation.PairID,
			operation.PairedSystemOrder, strconv.Itoa(operation.OrderPosition), operation.Mode, strconv.FormatInt(nonce, 10)}, "\x00")),
		ExpectedRows: expected.ExpectedRows, ExpectedColumns: expected.ExpectedColumns,
		ExpectedResultSHA256: expected.ExpectedResultSHA256, ObservedResultSHA256: execution.ResultSHA256,
		ExpectedDependencyFacts: expected.DependencyFacts, ExpectedDependencySHA256: expected.DependencySetSHA256,
		TypedDrainFields: execution.TypedDrainFields, TypedDrainSHA256: execution.TypedDrainSHA256,
		// A TaskGate Parquet drain has no PostgreSQL field OIDs. Retain that as
		// the schema-valid empty array, not JSON null (which would make an
		// otherwise accepted sample impossible to publish).
		FieldOIDs: append([]uint32{}, execution.FieldOIDs...), PostgreSQLVersion: system.PostgreSQLVersion,
		PostgreSQLVersionNum: system.PostgreSQLVersionNum, StatementTimeoutMS: system.StatementTimeoutMS,
		MaxParallelWorkers: system.MaxParallelWorkers, ClientMinMessages: system.ClientMinMessages,
		LogMinMessages: system.LogMinMessages, ProvSQLVersion: system.ProvSQLVersion,
		ProvSQLCommit: system.ProvSQLCommit, SharedPreload: system.SharedPreload,
		AggTokenTextAsUUID: system.AggTokenTextAsUUID, AggTokenOID: system.AggTokenOID, UUIDOID: system.UUIDOID,
		CarrierGateType:   map[bool]string{true: "agg"}[operation.Mode == "provsql"],
		RowGateType:       map[bool]string{true: "delta"}[operation.Mode == "provsql"],
		RootTypesVerified: execution.RootTypesVerified,
		AggregateTokens:   execution.AggregateTokens, RowTokens: execution.RowTokens,
		GatesBefore: execution.Before.Gates, GatesAfter: execution.After.Gates,
		ArtifactBytesBefore: execution.Before.ArtifactBytes, ArtifactBytesAfter: execution.After.ArtifactBytes,
		RepresentationSHA256: execution.RepresentationSHA,
		BaseTupleLink:        execution.BaseTupleLink,
	}
}

// expandProvSQLBaseTuples walks ProvSQL's circuit from the output roots to
// its input gates (provsql.get_children), maps every input gate to the base
// row whose provsql column carries it, builds the oracle's canonical base-row
// Fact for each such row, and compares the resulting set with the oracle's
// own enumeration for this scale and nonce. ProvSQL's circuit is the only
// witness of which base tuples contributed; the oracle package builds the
// identities but plays no part in the expansion.
func expandProvSQLBaseTuples(ctx context.Context, conn *pgx.Conn, roots []string, limit, nonce int64) (experiment.ProvSQLBaseTupleLinkV1, error) {
	started := time.Now()
	link := experiment.ProvSQLBaseTupleLinkV1{Version: experiment.ProvSQLBaseTupleLinkV1Version, Roots: int64(len(roots))}
	rows, err := conn.Query(ctx, `WITH RECURSIVE walk(gate) AS (
  SELECT DISTINCT r::uuid FROM unnest($1::text[]) AS r
  UNION
  SELECT child FROM walk, LATERAL unnest(provsql.get_children(walk.gate)) AS child
)
SELECT gate::text FROM walk WHERE provsql.get_gate_type(gate) = 'input'`, roots)
	if err != nil {
		return link, fmt.Errorf("expand ProvSQL circuit: %w", err)
	}
	var inputs []string
	for rows.Next() {
		var gate string
		if err := rows.Scan(&gate); err != nil {
			rows.Close()
			return link, err
		}
		inputs = append(inputs, gate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return link, err
	}
	link.InputGates = int64(len(inputs))
	if len(inputs) == 0 {
		return link, errors.New("ProvSQL circuit expansion reached no input gate")
	}
	type baseRow struct {
		product int
		keys    []any
	}
	var mapped []baseRow
	// ProvSQL's planner hook appends a provenance column to every SELECT over
	// a provenance-enabled relation; the mapping below reads the base rows'
	// key columns as plain SQL, so the hook is switched off for this
	// transaction only (the circuit itself is untouched).
	tx, err := conn.Begin(ctx)
	if err != nil {
		return link, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL provsql.active = off"); err != nil {
		return link, err
	}
	mappedRows, err := tx.Query(ctx, `SELECT 0, orderkey::bigint, NULL::integer FROM final_v5_provsql.orders WHERE provsql = ANY($1::uuid[])
UNION ALL SELECT 1, orderkey::bigint, linenumber::integer FROM final_v5_provsql.lineitem WHERE provsql = ANY($1::uuid[])
UNION ALL SELECT 2, nonce_id::bigint, NULL::integer FROM final_v5_provsql.nonce WHERE provsql = ANY($1::uuid[])`, inputs)
	if err != nil {
		return link, fmt.Errorf("map ProvSQL input gates to base rows: %w", err)
	}
	for mappedRows.Next() {
		var product int
		var first int64
		var second *int32
		if err := mappedRows.Scan(&product, &first, &second); err != nil {
			mappedRows.Close()
			return link, err
		}
		switch product {
		case 0:
			link.OrdersRows++
			mapped = append(mapped, baseRow{product: 0, keys: []any{first}})
		case 1:
			link.LineitemRows++
			if second == nil {
				mappedRows.Close()
				return link, errors.New("ProvSQL lineitem input gate lacks a line number")
			}
			mapped = append(mapped, baseRow{product: 1, keys: []any{first, int64(*second)}})
		default:
			link.NonceRows++
			mapped = append(mapped, baseRow{product: 2, keys: []any{first}})
		}
	}
	mappedRows.Close()
	if err := mappedRows.Err(); err != nil {
		return link, err
	}
	if err := tx.Commit(ctx); err != nil {
		return link, err
	}
	link.UnmappedInputGates = link.InputGates - int64(len(mapped))
	digests := make([]string, 0, len(mapped))
	for _, row := range mapped {
		fact, err := finalv5oracle.ProvSQLBaseRowFactFromKeys(row.product, row.keys)
		if err != nil {
			return link, err
		}
		digests = append(digests, fact.SHA256)
	}
	link.ProvSQLRowFacts = int64(len(digests))
	link.ProvSQLRowFactSetSHA256 = finalv5oracle.FactDigestSetSHA256(digests)
	link.OracleRowFacts, link.OracleRowFactSetSHA256, err = finalv5oracle.ProvSQLOracleBaseRowSet(limit, nonce)
	if err != nil {
		return link, err
	}
	link.Match = link.UnmappedInputGates == 0 && link.ProvSQLRowFacts == link.OracleRowFacts &&
		link.ProvSQLRowFactSetSHA256 == link.OracleRowFactSetSHA256
	link.ExpansionMS = durationMS(time.Since(started))
	if !link.Match {
		return link, fmt.Errorf("ProvSQL base tuples differ from the oracle's base-row facts: provsql=%d oracle=%d unmapped=%d",
			link.ProvSQLRowFacts, link.OracleRowFacts, link.UnmappedInputGates)
	}
	return link, nil
}

func (adapter *provSQLAdapter) retainedProvSQLFailure(operation experiment.AdapterOperation, expected boundQueryExpectation,
	spec provsqlfixture.Scale, nonce int64, boundary string, system provSQLSystem, execution provSQLExecution,
	stage, code string, cause error) experiment.Sample {
	writeAdapterFailureDiagnostic("provsql", operation, cause)
	sample := failedSample(operation, code)
	sample.ClientAvailableMS, sample.ClientFullDrainMS = execution.AvailableMS, execution.FullDrainMS
	if execution.FullDrainMS > 0 {
		sample.PipelineMS["execute_and_derive"] = execution.FullDrainMS
		sample.PipelineMS["server_total"] = execution.FullDrainMS
	}
	sample.RowCount = execution.Rows
	if execution.Columns > 0 {
		sample.ColumnCount = provsqlfixture.ExpectedColumns
	}
	if validDigest(execution.ResultSHA256) {
		sample.ResultSHA256 = execution.ResultSHA256
	}
	logical, _ := provsqlfixture.LogicalSQL(operation.Scale, nonce)
	if operation.Mode == "taskgate" {
		sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256 = sha(logical), sha(logical)
	} else {
		sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256 = provsqlfixture.PhysicalSQLSHA256(), sha(logical)
	}
	sample.ProvSQLVerification = adapter.provSQLVerification(operation, expected, spec, nonce, boundary, system, execution)
	sample.ProvSQLVerification.FailureStage = stage
	return sample
}

func copyProvSQLPartialSample(target *experiment.Sample, source experiment.Sample) {
	if source.SampleID == "" {
		return
	}
	status, code, reason := target.Status, target.ErrorCode, target.Reason
	*target = source
	target.Status, target.ErrorCode, target.Reason = status, code, reason
}

func externalProvSQLSample(operation experiment.AdapterOperation, execution provSQLExecution, system, physicalSQL string) experiment.Sample {
	sample := baseSample(operation, system)
	sample.ClientAvailableMS, sample.ClientFullDrainMS = execution.AvailableMS, execution.FullDrainMS
	sample.PipelineMS["execute_and_derive"] = execution.FullDrainMS
	sample.PipelineMS["server_total"] = execution.FullDrainMS
	sample.RowCount, sample.ColumnCount, sample.ResultSHA256 = execution.Rows, provsqlfixture.ExpectedColumns, execution.ResultSHA256
	sample.PhysicalSQLSHA256 = sha(physicalSQL)
	logical, _ := provsqlfixture.LogicalSQL(operation.Scale, mustProvSQLNonce(operation))
	sample.LogicalSQLSHA256, sample.QueryPlanSHA256 = sha(logical), sha(physicalSQL)
	sample.Status = "pass"
	return sample
}

func mustProvSQLNonce(operation experiment.AdapterOperation) int64 {
	nonce, _ := provsqlfixture.Nonce(operation.Scale, operation.ProcessReplicate, operation.Iteration, operation.Warmup)
	return nonce
}

func (adapter *provSQLAdapter) executeProvSQLTaskGate(ctx context.Context, operation experiment.AdapterOperation,
	expected boundQueryExpectation) (experiment.Sample, error) {
	partial := baseSample(operation, "taskgate")
	state := &pairState{}
	var err error
	state.taskID, err = adapter.real.provisionBoundTask(ctx, operation, adapter.binding.Section.ProvSQL.Task)
	if err != nil {
		return partial, err
	}
	// The one-use observer ticket binds the exact task/request attempt. The
	// private binding key narrows this public ProvSQL cell from its 35 frozen
	// nonce variants to exactly the statement this sample is about; it remains a
	// hint, and the finalizer independently loads and verifies the binding bytes.
	requestID := "final-v5-provsql-" + sha(operation.SampleID)[:24]
	finalizer, err := adapter.provSQLFinalizer(ctx)
	if err != nil {
		return partial, err
	}
	selector := provSQLContractSelector(operation, provSQLBindingKey(operation.Scale, mustProvSQLNonce(operation)))
	registered, err := finalizer.OpenObserverWindowV3(ctx, selector,
		experiment.ObserverAttemptV3{TaskID: state.taskID, RequestID: requestID})
	if err != nil {
		return partial, err
	}
	beforeRoot, err := adapter.real.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil || beforeRoot.Epoch != 0 {
		return partial, errors.New("ProvSQL TaskGate root is not fresh")
	}
	businessBefore, err := adapter.real.businessSQLSnapshotFor(ctx, adapter.binding.Section.ProvSQL.Task)
	if err != nil {
		return partial, err
	}
	observerBefore, err := captureBoundObserverV2(ctx, experiment.ObserverInvocationV3{
		Phase: "before", ObserverWindowID: registered.ObserverWindowID,
		ClassifierManifestSHA256: registered.ClassifierManifestSHA256,
	})
	if err != nil {
		return partial, err
	}
	window := experiment.ObserverWindowV2{Before: observerBefore}
	evidence := &experiment.ProvSQLVerificationEvidence{
		BusinessBefore: &businessBefore, RootBefore: &beforeRoot, ObserverWindow: &window,
	}
	partial.ProvSQLVerification = evidence
	started := time.Now()
	var response queryResponse
	if err := adapter.real.alice.call(ctx, "query_sql", map[string]any{
		"task_id": state.taskID, "request_id": requestID, "sql": expected.SQL,
	}, &response); err != nil {
		return partial, err
	}
	availableMS := durationMS(time.Since(started))
	partial = observedTaskgateQueryPrefix(operation, state.taskID, expected.SQL, started, availableMS,
		response, beforeRoot, beforeRoot)
	partial.ProvSQLVerification = evidence
	businessAfter, err := adapter.real.businessSQLSnapshotFor(ctx, adapter.binding.Section.ProvSQL.Task)
	if err != nil {
		return partial, err
	}
	evidence.BusinessAfter = &businessAfter
	afterRoot, err := adapter.real.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil {
		return partial, err
	}
	evidence.RootAfter = &afterRoot
	partial = observedTaskgateQueryPrefix(operation, state.taskID, expected.SQL, started, availableMS,
		response, beforeRoot, afterRoot)
	partial.ProvSQLVerification = evidence
	sample, err := adapter.real.completeTaskgateSample(ctx, operation, state, beforeRoot, afterRoot,
		started, availableMS, expected.SQL, response)
	if err != nil {
		return partial, err
	}
	sample.ProvSQLVerification = partial.ProvSQLVerification
	partial = sample
	observerAfter, err := captureBoundObserverV2(ctx, experiment.ObserverInvocationV3{
		Phase: "after", ObserverWindowID: registered.ObserverWindowID,
		ClassifierManifestSHA256: registered.ClassifierManifestSHA256,
	})
	if err != nil {
		return partial, err
	}
	window.After = observerAfter
	partial = sample
	resource, err := window.ResourceDelta()
	if err != nil {
		return partial, err
	}
	sample.GatewayMemoryPeakBytes = resource.GatewayMemoryPeakBytes
	sample.GatewayCPUUsecDelta = resource.GatewayCPUUsecDelta
	sample.GatewayNetworkRXDelta = resource.GatewayNetworkRXDelta
	sample.GatewayNetworkTXDelta = resource.GatewayNetworkTXDelta
	sample.ControlWALBytesDelta = resource.ControlWALBytesDelta
	sample.BusinessWALBytesDelta = resource.BusinessWALBytesDelta
	visibleDelta := businessAfter.VisibleCalls - businessBefore.VisibleCalls
	companionDelta := businessAfter.CompanionCalls - businessBefore.CompanionCalls
	sample.BusinessSQLDelta = visibleDelta + companionDelta
	partial = sample
	if visibleDelta != 1 || companionDelta != 1 ||
		expected.ExpectedVisibleCalls != 1 || expected.ExpectedCompanionCalls != 1 {
		return partial, errors.New("ProvSQL TaskGate Business statement counts differ from the private exact binding")
	}
	partial = sample
	if err := validateBoundScaleSampleResult(sample, expected); err != nil {
		return partial, err
	}
	dependencyLink, err := finalizer.VerifyProvSQLDependencySetV1(ctx,
		experiment.ProvSQLDependencySetVerificationRequestV1{
			ContractSelector: selector, ProductionSetSHA256: sample.DependencySetSHA256,
		})
	if err != nil {
		return partial, err
	}
	evidence.DependencyLink = &dependencyLink
	evidence.Version = provSQLVerificationVersion
	partial = sample
	generation := sample.PipelineMS["prepare"] + sample.PipelineMS["execute_and_derive"]
	if generation <= 0 || sample.ClientFullDrainMS <= 0 {
		return partial, errors.New("TaskGate timing boundaries are absent")
	}
	sample.GenerationBoundaryMS, sample.FullTaskGateMS = generation, sample.ClientFullDrainMS
	sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256 = sha(expected.SQL), sha(expected.SQL)
	partial = sample
	if sample.BaselineVerification == nil {
		return partial, errors.New("verified ProvSQL sample omitted its receipt evidence")
	}
	carried, err := carriedProvSQLEvidence(registered, window, sample.BaselineVerification.Receipt)
	if err != nil {
		return partial, err
	}
	finalized, err := finalizer.FinalizeTaskGateObservationV3(ctx, experiment.FinalizationRequestV3{
		Receipt: sample.BaselineVerification.Receipt, Carried: carried, ContractSelector: selector,
		ObserverWindowTicket: registered.ObserverWindowTicket,
	})
	if err != nil {
		return partial, err
	}
	sample.TaskGateAcceptanceV3 = &finalized
	return sample, nil
}

// provSQLContractSelector names one public ProvSQL cell plus its exact private
// nonce variant. Every coordinate is a narrowing hint; finalization accepts only
// the candidate whose independently reproduced preparation the Gateway signed.
func provSQLContractSelector(operation experiment.AdapterOperation, bindingKey string) experiment.FrozenContractSelectorV3 {
	return experiment.FrozenContractSelectorV3{
		ExperimentID: operation.ExperimentID, WorkloadID: operation.WorkloadID,
		Scale: operation.Scale, Mode: operation.Mode, BindingKey: bindingKey,
	}
}

// carriedProvSQLEvidence transcribes the two executed targets signed by the
// Gateway. ProvSQL's TaskGate control is always a paired novel execution: an
// absent companion, another path kind, or an authorized-but-unexecuted target
// is not evidence that the measured operation ran.
func carriedProvSQLEvidence(registered experiment.PreRegisteredObservationV3,
	window experiment.ObserverWindowV2,
	receipt queryreceipt.QueryReceiptV1) (experiment.CarriedEvidenceV3, error) {
	if registered.Operation.PathKind != experiment.PathPairedNovel ||
		registered.Plan.PathKind != experiment.PathPairedNovel {
		return experiment.CarriedEvidenceV3{}, errors.New("the ProvSQL registration is not a paired novel operation")
	}
	signed := receipt.ExecutionBindingV2
	if signed == nil {
		return experiment.CarriedEvidenceV3{}, errors.New("the ProvSQL receipt describes no prepared execution")
	}
	if signed.PathKind != querybinding.PathPairedNovel {
		return experiment.CarriedEvidenceV3{}, errors.New("the ProvSQL receipt did not sign a paired novel execution")
	}
	if signed.Companion == nil {
		return experiment.CarriedEvidenceV3{}, errors.New("the ProvSQL receipt signs no provenance companion")
	}
	if !signed.Visible.Executed || !signed.Companion.Executed {
		return experiment.CarriedEvidenceV3{}, errors.New("the ProvSQL receipt did not execute both signed targets")
	}
	return experiment.CarriedEvidenceV3{
		Arm:                                  experiment.ArmTaskGate,
		Operation:                            registered.Operation,
		Plan:                                 registered.Plan,
		ClassifierManifestSHA256:             registered.ClassifierManifestSHA256,
		ClassifierBindingSHA256:              registered.ClassifierBindingSHA256,
		Window:                               window,
		VisibleStatement:                     signedTargetStatement(signed.Visible),
		CompanionStatement:                   signedTargetStatement(*signed.Companion),
		VisiblePreparedTargetBindingSHA256:   signed.Visible.PreparedTargetBindingSHA256,
		CompanionPreparedTargetBindingSHA256: signed.Companion.PreparedTargetBindingSHA256,
	}, nil
}

func (adapter *provSQLAdapter) validateAndAdvanceProvSQLSequence(nonce int64, execution provSQLExecution) error {
	if adapter.sequence.seenNonces[nonce] || execution.RepresentationSHA == "" ||
		adapter.sequence.seenRepresentations[execution.RepresentationSHA] ||
		execution.After.Gates <= execution.Before.Gates || execution.Before.ArtifactBytes <= 0 ||
		execution.After.ArtifactBytes < execution.Before.ArtifactBytes {
		return errors.New("ProvSQL operation did not create a unique monotone circuit")
	}
	if adapter.sequence.initialized && (execution.Before.Gates < adapter.sequence.lastGates || execution.Before.ArtifactBytes < adapter.sequence.lastBytes) {
		return errors.New("ProvSQL circuit state regressed between operations")
	}
	adapter.sequence.seenNonces[nonce] = true
	adapter.sequence.seenRepresentations[execution.RepresentationSHA] = true
	adapter.sequence.initialized = true
	adapter.sequence.lastGates = execution.After.Gates
	adapter.sequence.lastBytes = execution.After.ArtifactBytes
	return nil
}

func configureProvSQLSession(ctx context.Context, conn *pgx.Conn, provenance bool) error {
	statements := []string{
		fmt.Sprintf("SET statement_timeout = %d", provsqlfixture.StatementTimeout),
		"SET max_parallel_workers_per_gather = 0",
		"SET client_min_messages = error",
		"SET log_min_messages = error",
	}
	if provenance {
		statements = append(statements, "SET provsql.aggtoken_text_as_uuid = on")
	}
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func inspectProvSQLSystem(ctx context.Context, conn *pgx.Conn, provenance bool) (provSQLSystem, error) {
	var result provSQLSystem
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version'),current_setting('server_version_num'),
(SELECT setting::bigint FROM pg_settings WHERE name='statement_timeout'),
(SELECT setting::bigint FROM pg_settings WHERE name='max_parallel_workers_per_gather'),
current_setting('client_min_messages'),current_setting('log_min_messages'),'uuid'::regtype::oid`).Scan(
		&result.PostgreSQLVersion, &result.PostgreSQLVersionNum, &result.StatementTimeoutMS,
		&result.MaxParallelWorkers, &result.ClientMinMessages, &result.LogMinMessages, &result.UUIDOID); err != nil {
		return result, fmt.Errorf("inspect PostgreSQL session: %w", err)
	}
	var extensionPresent bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='provsql')`).Scan(&extensionPresent); err != nil {
		return result, err
	}
	var preload string
	if err := conn.QueryRow(ctx, `SELECT current_setting('shared_preload_libraries')`).Scan(&preload); err != nil {
		return result, err
	}
	for _, item := range strings.Split(preload, ",") {
		result.SharedPreload = result.SharedPreload || strings.TrimSpace(item) == "provsql"
	}
	if !provenance {
		if extensionPresent || result.SharedPreload {
			return result, errors.New("direct PostgreSQL unexpectedly loads ProvSQL")
		}
		return result, nil
	}
	if !extensionPresent {
		return result, errors.New("ProvSQL extension is absent")
	}
	if err := conn.QueryRow(ctx, `SELECT extversion FROM pg_extension WHERE extname='provsql'`).Scan(&result.ProvSQLVersion); err != nil {
		return result, err
	}
	if err := conn.QueryRow(ctx, `SELECT btrim(pg_read_file('/usr/local/share/taskgate-evaluation/provsql-source-commit'), E' \t\n\r')`).Scan(&result.ProvSQLCommit); err != nil {
		return result, err
	}
	if err := conn.QueryRow(ctx, `SELECT current_setting('provsql.aggtoken_text_as_uuid')::boolean,'provsql.agg_token'::regtype::oid`).Scan(
		&result.AggTokenTextAsUUID, &result.AggTokenOID); err != nil {
		return result, err
	}
	return result, nil
}

func validateProvSQLSystems(direct, provenance provSQLSystem) error {
	common := func(system provSQLSystem) bool {
		return system.PostgreSQLVersion != "" && system.PostgreSQLVersionNum != "" &&
			system.StatementTimeoutMS == provsqlfixture.StatementTimeout && system.MaxParallelWorkers == 0 &&
			system.ClientMinMessages == "error" && system.LogMinMessages == "error" && system.UUIDOID != 0
	}
	if !common(direct) || !common(provenance) || direct.PostgreSQLVersion != provenance.PostgreSQLVersion ||
		direct.PostgreSQLVersionNum != provenance.PostgreSQLVersionNum || direct.UUIDOID != provenance.UUIDOID ||
		provenance.ProvSQLVersion != provsqlfixture.ProvSQLVersion || provenance.ProvSQLCommit != provsqlfixture.ProvSQLCommit ||
		!provenance.SharedPreload || !provenance.AggTokenTextAsUUID || provenance.AggTokenOID == 0 {
		return errors.New("direct/ProvSQL system attestation differs from the pinned protocol")
	}
	return nil
}

type provSQLRowsQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func fingerprintProvSQLDataset(ctx context.Context, conn *pgx.Conn, disableProvSQL bool) (string, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())
	if disableProvSQL {
		if _, err := tx.Exec(ctx, "SET LOCAL provsql.active = off"); err != nil {
			return "", err
		}
	}
	digest, err := fingerprintProvSQLDatasetRows(ctx, tx, provsqlfixture.DatasetFingerprintSQL)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return digest, nil
}

func fingerprintProvSQLDatasetRows(ctx context.Context, queryer provSQLRowsQueryer, sqlText string) (string, error) {
	rows, err := queryer.Query(ctx, sqlText)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	hasher := provsqlfixture.NewDatasetHasher()
	for rows.Next() {
		var kind, value string
		var key1, key2 int64
		if err := rows.Scan(&kind, &key1, &key2, &value); err != nil {
			return "", err
		}
		hasher.Add(kind, key1, key2, value)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	digest, count := hasher.Sum()
	if count != provsqlfixture.DatasetRowCount {
		return "", fmt.Errorf("dataset fingerprint rows = %d, want %d", count, provsqlfixture.DatasetRowCount)
	}
	return digest, nil
}

func executeProvSQLExternal(ctx context.Context, conn *pgx.Conn, spec provsqlfixture.Scale, nonce int64,
	provenance bool, system provSQLSystem) (provSQLExecution, error) {
	result := provSQLExecution{}
	var err error
	if provenance {
		result.Before, err = readProvSQLMetrics(ctx, conn)
		if err != nil {
			return result, err
		}
	}
	started := time.Now()
	rows, err := conn.Query(ctx, provsqlfixture.PhysicalSQL, pgx.QueryExecModeSimpleProtocol, spec.Limit, nonce)
	if err != nil {
		return result, err
	}
	fields := rows.FieldDescriptions()
	expectedColumns := 1 + provsqlfixture.CarrierColumns
	if provenance {
		expectedColumns++
	}
	if len(fields) != expectedColumns {
		rows.Close()
		return result, fmt.Errorf("external query columns = %d, want %d", len(fields), expectedColumns)
	}
	result.Columns = len(fields)
	result.FieldOIDs = make([]uint32, len(fields))
	for index := range fields {
		result.FieldOIDs[index] = fields[index].DataTypeOID
	}
	if err := validateProvSQLFieldDescriptions(fields, provenance, system); err != nil {
		rows.Close()
		return result, err
	}
	drain := sha256.New()
	_, _ = drain.Write([]byte("TASKGATE-FINAL-V5-PROVSQL-TYPED-DRAIN-V1\x00"))
	visible := make([][]any, 0, provsqlfixture.ExpectedRows)
	pending := make([]provSQLAggregateRow, 0, provsqlfixture.ExpectedRows)
	aggregateRoots := make([]string, 0, provsqlfixture.ExpectedRows*provsqlfixture.CarrierColumns)
	rowRoots := make([]string, 0, provsqlfixture.ExpectedRows)
	for rows.Next() {
		if result.Rows == 0 {
			result.AvailableMS = durationMS(time.Since(started))
		}
		values, valueErr := rows.Values()
		if valueErr != nil || len(values) != len(fields) {
			rows.Close()
			if valueErr == nil {
				valueErr = errors.New("typed drain omitted a field")
			}
			return result, valueErr
		}
		raw := rows.RawValues()
		if len(raw) != len(fields) {
			rows.Close()
			return result, errors.New("raw drain omitted a field")
		}
		for index := range values {
			writeTypedDrainField(drain, fields[index].DataTypeOID, raw[index], values[index] == nil)
			result.TypedDrainFields++
		}
		if provenance {
			pendingRow, parseErr := parseProvSQLAggregateJSON(string(raw[0]))
			if parseErr != nil {
				rows.Close()
				return result, parseErr
			}
			for index := 1; index <= provsqlfixture.CarrierColumns; index++ {
				root := string(raw[index])
				if !canonicalUUID.MatchString(root) {
					rows.Close()
					return result, errors.New("ProvSQL aggregate carrier is not a canonical UUID")
				}
				if root != pendingRow.roots[index-1] {
					rows.Close()
					return result, errors.New("ProvSQL row_json aggregate root differs from its typed carrier")
				}
				aggregateRoots = append(aggregateRoots, root)
			}
			pending = append(pending, pendingRow)
			rowRoot := string(raw[len(raw)-1])
			if !canonicalUUID.MatchString(rowRoot) {
				rows.Close()
				return result, errors.New("ProvSQL hidden row token is not a canonical UUID")
			}
			rowRoots = append(rowRoots, rowRoot)
		} else {
			visibleRow, parseErr := parseProvSQLVisibleJSON(string(raw[0]))
			if parseErr != nil {
				rows.Close()
				return result, parseErr
			}
			visible = append(visible, visibleRow)
		}
		result.Rows++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	result.TypedDrainSHA256 = hex.EncodeToString(drain.Sum(nil))
	if provenance {
		visible, err = resolveProvSQLVisibleRows(ctx, conn, pending)
		if err != nil {
			return result, err
		}
	}
	result.ResultSHA256, err = experiment.CanonicalResultHash(visible)
	if err != nil {
		result.FullDrainMS = durationMS(time.Since(started))
		return result, err
	}
	expectedRows, err := provsqlfixture.ExpectedResultRows(spec.Name)
	if err != nil {
		return result, err
	}
	expectedDigest, err := experiment.CanonicalResultHash(expectedRows)
	// The ProvSQL logical values are usable only after the real aggregate
	// roots have been resolved and the visible result has passed through its
	// oracle comparison. Include both in the client full-drain boundary;
	// root-type and mmap diagnostics below stay outside it for both arms.
	result.FullDrainMS = durationMS(time.Since(started))
	if err != nil || result.Rows != provsqlfixture.ExpectedRows || result.ResultSHA256 != expectedDigest {
		return result, errors.New("external visible result differs from the frozen four-column oracle")
	}
	if provenance {
		result.AggregateTokens, result.RowTokens = int64(len(aggregateRoots)), int64(len(rowRoots))
		result.RepresentationSHA = representationDigest(aggregateRoots, rowRoots)
		result.After, err = readProvSQLMetrics(ctx, conn)
		if err != nil {
			return result, err
		}
		if err := validateProvSQLRootTypes(ctx, conn, aggregateRoots, rowRoots); err != nil {
			return result, err
		}
		result.RootTypesVerified = true
		// Outside every timing boundary: expand the circuit to its input gates
		// and link the base tuples to the oracle's base-row Facts.
		link, linkErr := expandProvSQLBaseTuples(ctx, conn, append(append([]string(nil), aggregateRoots...), rowRoots...), spec.Limit, nonce)
		if linkErr != nil {
			return result, linkErr
		}
		result.BaseTupleLink = &link
	}
	if result.AvailableMS <= 0 || result.FullDrainMS <= 0 || result.AvailableMS > result.FullDrainMS {
		return result, errors.New("external complete-drain timing boundary is invalid")
	}
	return result, nil
}

func validateProvSQLFieldDescriptions(fields []pgconn.FieldDescription, provenance bool, system provSQLSystem) error {
	names := []string{"row_json", "price_provenance", "line_provenance", "count_provenance"}
	if provenance {
		names = append(names, "provsql")
	}
	for index := range names {
		if fields[index].Name != names[index] {
			return fmt.Errorf("external query column %d name = %q, want %q", index, fields[index].Name, names[index])
		}
	}
	if fields[0].DataTypeOID != 25 {
		return errors.New("canonical row_json is not PostgreSQL text")
	}
	if !provenance {
		want := []uint32{25, 1700, 20, 20}
		for index := range want {
			if fields[index].DataTypeOID != want[index] {
				return fmt.Errorf("direct column %d OID = %d, want %d", index, fields[index].DataTypeOID, want[index])
			}
		}
		return nil
	}
	for index := 1; index <= provsqlfixture.CarrierColumns; index++ {
		if fields[index].DataTypeOID != system.AggTokenOID {
			return fmt.Errorf("ProvSQL carrier %d OID = %d, want agg_token %d", index, fields[index].DataTypeOID, system.AggTokenOID)
		}
	}
	if fields[len(fields)-1].DataTypeOID != system.UUIDOID {
		return fmt.Errorf("ProvSQL hidden token OID = %d, want uuid %d", fields[len(fields)-1].DataTypeOID, system.UUIDOID)
	}
	return nil
}

func writeTypedDrainField(hash interface{ Write([]byte) (int, error) }, oid uint32, raw []byte, isNull bool) {
	var encoded [8]byte
	binary.BigEndian.PutUint32(encoded[:4], oid)
	if isNull {
		encoded[4] = 0
	} else {
		encoded[4] = 1
	}
	binary.BigEndian.PutUint64(encoded[:], uint64(len(raw)))
	// Keep OID and nullability in a fixed prefix separate from value length.
	var oidBytes [5]byte
	binary.BigEndian.PutUint32(oidBytes[:4], oid)
	if !isNull {
		oidBytes[4] = 1
	}
	_, _ = hash.Write(oidBytes[:])
	_, _ = hash.Write(encoded[:])
	_, _ = hash.Write(raw)
}

func parseProvSQLVisibleJSON(value string) ([]any, error) {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var raw []any
	if err := decoder.Decode(&raw); err != nil || len(raw) != provsqlfixture.ExpectedColumns {
		return nil, errors.New("canonical row_json is not the frozen four-field array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("canonical row_json contains trailing data")
	}
	integer := func(input any) (int64, error) {
		number, ok := input.(json.Number)
		if !ok {
			return 0, errors.New("canonical integer has the wrong JSON type")
		}
		return strconv.ParseInt(string(number), 10, 64)
	}
	status, err := integer(raw[0])
	if err != nil {
		return nil, err
	}
	price, ok := raw[1].(string)
	if !ok || !regexp.MustCompile(`^[0-9]+[.][0-9]{2}$`).MatchString(price) {
		return nil, errors.New("canonical price has the wrong type or scale")
	}
	lines, err := integer(raw[2])
	if err != nil {
		return nil, err
	}
	members, err := integer(raw[3])
	if err != nil {
		return nil, err
	}
	return []any{status, price, lines, members}, nil
}

// ProvSQL rewrites aggregate output cells to agg_token. With the pinned
// aggtoken_text_as_uuid session setting, both the three explicit carriers and
// their row_json positions expose circuit-root UUIDs. Keep those physical
// values in the typed drain, then recover the actual-world scalar values from
// ProvSQL's circuit API; never substitute the fixture oracle for an observed
// value.
func parseProvSQLAggregateJSON(value string) (provSQLAggregateRow, error) {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var raw []any
	if err := decoder.Decode(&raw); err != nil || len(raw) != provsqlfixture.ExpectedColumns {
		return provSQLAggregateRow{}, errors.New("ProvSQL row_json is not the frozen four-field root array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return provSQLAggregateRow{}, errors.New("ProvSQL row_json contains trailing data")
	}
	statusNumber, ok := raw[0].(json.Number)
	if !ok {
		return provSQLAggregateRow{}, errors.New("ProvSQL row_json status has the wrong JSON type")
	}
	status, err := strconv.ParseInt(string(statusNumber), 10, 64)
	if err != nil {
		return provSQLAggregateRow{}, errors.New("ProvSQL row_json status is not an integer")
	}
	result := provSQLAggregateRow{status: status}
	for index := range result.roots {
		root, ok := raw[index+1].(string)
		if !ok || !canonicalUUID.MatchString(root) {
			return provSQLAggregateRow{}, errors.New("ProvSQL row_json aggregate root is not a canonical UUID")
		}
		result.roots[index] = root
	}
	return result, nil
}

func resolveProvSQLVisibleRows(ctx context.Context, conn *pgx.Conn, pending []provSQLAggregateRow) ([][]any, error) {
	if len(pending) != int(provsqlfixture.ExpectedRows) {
		return nil, fmt.Errorf("ProvSQL aggregate rows = %d, want %d", len(pending), provsqlfixture.ExpectedRows)
	}
	roots := make([]string, 0, len(pending)*provsqlfixture.CarrierColumns)
	for _, row := range pending {
		roots = append(roots, row.roots[:]...)
	}
	rows, err := conn.Query(ctx, `SELECT u.root,
       provsql.get_gate_type(u.root::uuid)::text,
       provsql.agg_gate_value(u.root::uuid)::text
FROM unnest($1::text[]) WITH ORDINALITY AS u(root,ordinality)
ORDER BY u.ordinality`, roots)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]string, 0, len(roots))
	for rows.Next() {
		var root, gateType, scalar string
		if err := rows.Scan(&root, &gateType, &scalar); err != nil {
			return nil, err
		}
		index := len(values)
		if index >= len(roots) || root != roots[index] || gateType != "agg" || scalar == "" {
			return nil, errors.New("ProvSQL aggregate root did not resolve to its ordered real scalar")
		}
		values = append(values, scalar)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(values) != len(roots) {
		return nil, errors.New("ProvSQL aggregate scalar recovery omitted a carrier")
	}
	visible := make([][]any, len(pending))
	for index, row := range pending {
		price := values[index*provsqlfixture.CarrierColumns]
		if !regexp.MustCompile(`^[0-9]+[.][0-9]{2}$`).MatchString(price) {
			return nil, errors.New("ProvSQL recovered price has the wrong type or scale")
		}
		lines, err := strconv.ParseInt(values[index*provsqlfixture.CarrierColumns+1], 10, 64)
		if err != nil {
			return nil, errors.New("ProvSQL recovered line sum is not an integer")
		}
		members, err := strconv.ParseInt(values[index*provsqlfixture.CarrierColumns+2], 10, 64)
		if err != nil {
			return nil, errors.New("ProvSQL recovered member count is not an integer")
		}
		visible[index] = []any{row.status, price, lines, members}
	}
	return visible, nil
}

func readProvSQLMetrics(ctx context.Context, conn *pgx.Conn) (provSQLMetrics, error) {
	const query = `SELECT provsql.get_nb_gates(),
       COALESCE(sum((pg_stat_file('base/' || d.oid::text || '/' || f.name,true)).size),0)::bigint
FROM pg_database AS d
CROSS JOIN unnest(ARRAY['provsql_gates.mmap','provsql_wires.mmap','provsql_mapping.mmap',
  'provsql_extra.mmap','provsql_table_info.mmap']) AS f(name)
WHERE d.datname=current_database()
GROUP BY provsql.get_nb_gates()`
	var result provSQLMetrics
	if err := conn.QueryRow(ctx, query).Scan(&result.Gates, &result.ArtifactBytes); err != nil {
		return result, err
	}
	return result, nil
}

func validateProvSQLRootTypes(ctx context.Context, conn *pgx.Conn, aggregateRoots, rowRoots []string) error {
	for _, group := range []struct {
		roots []string
		want  string
	}{{aggregateRoots, "agg"}, {rowRoots, "delta"}} {
		for _, root := range group.roots {
			var actual string
			if err := conn.QueryRow(ctx, `SELECT provsql.get_gate_type($1::uuid)::text`, root).Scan(&actual); err != nil {
				return err
			}
			if actual != group.want {
				return fmt.Errorf("ProvSQL root gate type = %q, want %q", actual, group.want)
			}
		}
	}
	return nil
}

func representationDigest(aggregateRoots, rowRoots []string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-PROVSQL-REPRESENTATION-V1\x00"))
	for _, roots := range []struct {
		label string
		list  []string
	}{{"aggregate", aggregateRoots}, {"row", rowRoots}} {
		for _, root := range roots.list {
			_, _ = hash.Write([]byte(roots.label + "\x00" + root + "\x00"))
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}
