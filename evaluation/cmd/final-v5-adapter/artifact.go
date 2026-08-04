package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const artifactVerificationVersion = "taskgate-final-v5-artifact-verification-v1"

// artifactAdapter executes the six frozen result-heavy cells. Everything it
// runs -- the workload matrix, the rendered query text, the Product and
// Publication it binds, the approved projection, and the expected result -- is
// derived from the verified Contract Index through the Contract-to-Runtime
// Bridge. The Adapter deliberately keeps no second workload table of its own.
type artifactAdapter struct {
	real    *realAdapter
	runtime *finalv5contracts.Runtime
	live    finalv5contracts.LiveDeployment
}

// newArtifactAdapter binds the frozen contracts to the running deployment.
// A missing contract, a drifted contract digest, or an absent live Catalog
// leaves the adapter unconstructed and therefore fail-closed.
func newArtifactAdapter(ctx context.Context) (sourceControlledAdapter, error) {
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		return nil, fmt.Errorf("contract bridge: %w", err)
	}
	live, err := liveDeploymentBinding()
	if err != nil {
		return nil, err
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	real.timeout = 30 * time.Minute
	real.http.Timeout = real.timeout
	return &artifactAdapter{real: real, runtime: runtime, live: live}, nil
}

// liveDeploymentBinding reads the Catalog the Gateway is actually running. The
// Adapter never assumes the reviewed candidate is installed: the digest is
// recomputed here and cross-checked against the digest the Gateway signed into
// the Receipt of every measured query.
func liveDeploymentBinding() (finalv5contracts.LiveDeployment, error) {
	path := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_CATALOG"))
	if path == "" {
		return finalv5contracts.LiveDeployment{}, errors.New("TASKGATE_FINAL_V5_CATALOG is required")
	}
	digest, err := experiment.FileSHA256(path)
	if err != nil {
		return finalv5contracts.LiveDeployment{}, fmt.Errorf("live catalog: %w", err)
	}
	return finalv5contracts.LiveDeployment{CatalogPath: path, CatalogSHA256: digest}, nil
}

func (adapter *artifactAdapter) Close() { adapter.real.Close() }

func (adapter *artifactAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if operation.ExperimentID != finalv5contracts.ArtifactExperimentID {
		return invalidSample(operation, "experiment_identity_mismatch")
	}
	cell, err := adapter.runtime.ArtifactCell(operation.Scale, operation.Mode)
	if err != nil || cell.Identity.WorkloadID != operation.WorkloadID {
		return invalidSample(operation, "unsupported_frozen_artifact_cell")
	}
	sample, err := adapter.executeResultHeavy(ctx, operation, cell)
	if err != nil {
		// The campaign record stays redacted: it carries only the taxonomy code.
		// A retained failure still has to be diagnosable by the operator running
		// the deployment, so the underlying error goes to this process's stderr,
		// which never enters a sample, an evidence file or an agent context.
		fmt.Fprintf(os.Stderr, "artifact cell %s: %v\n", cell.Identity, err)
		return retainedArtifactFailure(operation, sample, artifactFailureCode(err))
	}
	return validateArtifactPass(sample)
}

// artifactFailureCode keeps a retained failure attributable without leaking
// query text, identities, or credentials into the campaign record.
func artifactFailureCode(err error) string {
	switch {
	case err == nil:
		return ""
	case strings.Contains(err.Error(), "oracle"):
		return "artifact_oracle_mismatch"
	case strings.Contains(err.Error(), "Catalog") || strings.Contains(err.Error(), "Publication") ||
		strings.Contains(err.Error(), "Product"):
		return "artifact_binding_invalid"
	default:
		return "artifact_measurement_failed"
	}
}

func validateArtifactPass(sample experiment.Sample) experiment.Sample {
	if sample.Status == "pass" {
		if err := experiment.ValidateArtifactEvidence(sample); err != nil {
			sample.Status = "fail"
			sample.ErrorCode = "artifact_evidence_invariant_failed"
			sample.Reason = "the retained real artifact sample failed its independent evidence invariant"
		}
	}
	return sample
}

func retainedArtifactFailure(operation experiment.AdapterOperation, sample experiment.Sample, code string) experiment.Sample {
	if sample.SchemaVersion == 0 {
		sample = failedSample(operation, code)
	} else {
		sample.Status = "fail"
		sample.ErrorCode = code
		sample.Reason = "a real artifact backend operation was attempted and failed; safely collected evidence is retained"
	}
	return sample
}

// executeResultHeavy runs one complete frozen cell: a Direct PostgreSQL
// complete drain, an OA-approved public BDG query through the whole result
// exposure settlement, Parquet, AES-GCM staging, PENDING, canonical object
// promotion and AVAILABLE, the composite released-artifact verification, and
// finally the independent Artifact Oracle over both drained results.
func (adapter *artifactAdapter) executeResultHeavy(ctx context.Context, operation experiment.AdapterOperation,
	cell finalv5contracts.ArtifactCell) (experiment.Sample, error) {
	query, err := adapter.runtime.QueryContract(cell)
	if err != nil {
		return experiment.Sample{}, err
	}
	probeDigest, err := adapter.verifyDatasetProbe(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	live := adapter.live
	live.DatasetProbeSHA256 = probeDigest
	binding, err := adapter.runtime.BindDeployment(cell, live)
	if err != nil {
		return experiment.Sample{}, err
	}
	// The Direct arm is the Baseline S6 equivalence control. It is drained
	// completely before the measured BDG call so that a partial drain on either
	// side cannot be mistaken for agreement.
	direct, err := adapter.drainDirect(ctx, query)
	if err != nil {
		return experiment.Sample{}, err
	}
	state := &pairState{}
	state.taskID, err = adapter.provisionArtifactTask(ctx, operation, binding)
	if err != nil {
		return experiment.Sample{}, err
	}
	beforeRoot, err := adapter.real.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil || beforeRoot.Epoch != 0 {
		if err == nil {
			err = errors.New("artifact task root is not fresh")
		}
		return experiment.Sample{}, err
	}
	businessBefore, err := adapter.businessSnapshot(ctx, binding)
	if err != nil {
		return experiment.Sample{}, err
	}
	// Taken next to the targeted snapshot so the census and the visible/companion
	// counters describe the same instant of pg_stat_statements.
	censusBefore, err := adapter.statementCensus(ctx, binding)
	if err != nil {
		return experiment.Sample{}, err
	}
	observerBefore, err := captureBoundObserver(ctx, "before")
	if err != nil {
		return experiment.Sample{}, err
	}
	requestID := "final-v5-artifact-" + sha(operation.SampleID)[:24]
	started := time.Now()
	var response queryResponse
	if err := adapter.real.alice.call(ctx, finalv5contracts.PublicBDGTool, map[string]any{
		"task_id": state.taskID, "request_id": requestID, "sql": query.BDG.SQL,
	}, &response); err != nil {
		return experiment.Sample{}, err
	}
	availableMS := durationMS(time.Since(started))
	partial := observedTaskgateQueryPrefix(operation, state.taskID, query.BDG.SQL, started, availableMS,
		response, beforeRoot, beforeRoot)
	businessAfter, err := adapter.businessSnapshot(ctx, binding)
	if err != nil {
		return partial, err
	}
	censusAfter, err := adapter.statementCensus(ctx, binding)
	if err != nil {
		return partial, err
	}
	afterRoot, err := adapter.real.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil {
		return partial, err
	}
	partial = observedTaskgateQueryPrefix(operation, state.taskID, query.BDG.SQL, started, availableMS,
		response, beforeRoot, afterRoot)
	sample, parquetBytes, err := adapter.real.completeTaskgateSampleWithParquet(ctx, operation, state,
		beforeRoot, afterRoot, started, availableMS, query.BDG.SQL, response)
	if err != nil {
		return partial, err
	}
	bdg, err := finalv5contracts.NormalizeBDG(query.Schema,
		finalv5contracts.ParquetInput(bytes.NewReader(parquetBytes), int64(len(parquetBytes))))
	if err != nil {
		return sample, err
	}
	comparison, err := adapter.runtime.CompareResults(cell, direct, bdg)
	if err != nil {
		return sample, err
	}
	// The contract names the canonical logical-result digest as the Artifact
	// result identity, so the sample and its parsed-Parquet evidence carry the
	// independently reducible digest rather than the JSON-shaped baseline hash.
	sample.ResultSHA256 = comparison.BDGResultSHA256
	sample.BaselineVerification.ParsedResultSHA256 = comparison.BDGResultSHA256
	sample.ArtifactVerification, err = adapter.artifactEvidence(sample, binding, query, comparison,
		businessBefore, businessAfter, beforeRoot, afterRoot, observerBefore)
	if err != nil {
		return sample, err
	}
	observerAfter, err := captureBoundObserver(ctx, "after")
	if err != nil {
		return sample, err
	}
	sample.ArtifactVerification.ObserverAfter = observerAfter
	// Bind the sample to the activated Result-heavy profile. The Catalog digest
	// comes from the Receipt the Gateway signed, so this is an observation of
	// what actually served the query, not a declaration about it.
	sample.ProfileBinding, err = artifactProfileBindingFor(sample.BaselineVerification.Receipt.CatalogDigest)
	if err != nil {
		return sample, err
	}
	visibleDelta := businessAfter.VisibleCalls - businessBefore.VisibleCalls
	companionDelta := businessAfter.CompanionCalls - businessBefore.CompanionCalls
	sample.BusinessSQLDelta = visibleDelta + companionDelta
	if visibleDelta != 1 || companionDelta != 1 {
		return sample, errors.New("artifact query did not execute exactly one visible and companion Business statement")
	}
	// A governed artifact query settles two transactions -- the visible statement
	// and its provenance companion -- and the reporting-view count comes from the
	// Catalog the Gateway signed, so the control multiplicity is derived from the
	// activated profile rather than fixed at 14.
	views, err := servedReportingViewCount(sample.BaselineVerification.Receipt.CatalogDigest)
	if err != nil {
		return sample, err
	}
	plan := experiment.NewGatewayControlPlan(2, views, visibleDelta, companionDelta)
	if err := applyObserverDelta(&sample, observerBefore, observerAfter, plan, censusBefore, censusAfter); err != nil {
		return sample, err
	}
	return sample, nil
}

// artifactEvidence binds the measured sample to the contract, the live Catalog
// and the independent oracle. The released Parquet's canonical logical-result
// digest becomes the sample result identity, because that is the identity the
// contract names; the Parquet and object binary digests stay separate runtime
// observations that the oracle never predeclares.
func (adapter *artifactAdapter) artifactEvidence(sample experiment.Sample, binding finalv5contracts.Binding,
	query finalv5contracts.QueryContract, comparison finalv5contracts.ResultComparison,
	businessBefore, businessAfter experiment.BusinessSQLSnapshot,
	beforeRoot, afterRoot experiment.RootLedgerSnapshot,
	observerBefore experiment.ObserverSnapshot) (*experiment.ArtifactVerificationEvidence, error) {
	if sample.BaselineVerification == nil || sample.BaselineVerification.Receipt.CatalogDigest != binding.CatalogSHA256 {
		return nil, errors.New("the Gateway signed a different Catalog digest than the live Catalog this Adapter bound")
	}
	if sample.RowCount != comparison.BDGRows || sample.ColumnCount != comparison.BDGColumns {
		return nil, errors.New("released artifact intent shape differs from the drained Parquet result")
	}
	if sample.ParquetBytes <= 0 || sample.EncryptedObjectBytes <= 0 {
		return nil, errors.New("released artifact byte observations are absent")
	}
	bindingRecord, err := json.Marshal(binding)
	if err != nil {
		return nil, err
	}
	return &experiment.ArtifactVerificationEvidence{
		Version: artifactVerificationVersion, BindingSHA256: shaBytes(bindingRecord),
		DatasetSHA256: binding.DatasetProbeSHA256, CatalogSHA256: binding.CatalogSHA256,
		DatasetProbeSHA256: binding.DatasetProbeSHA256, QuerySHA256: query.BDG.SQLSHA256,
		ExpectedRows: comparison.ExpectedRows, ExpectedColumns: comparison.ExpectedColumns,
		ExpectedResultSHA256: comparison.ExpectedResultSHA256,
		ObservedRows:         comparison.BDGRows, ObservedColumns: comparison.BDGColumns,
		ObservedResultSHA256: comparison.BDGResultSHA256,
		BusinessBefore:       businessBefore, BusinessAfter: businessAfter,
		RootBefore: beforeRoot, RootAfter: afterRoot, ObserverBefore: observerBefore,
	}, nil
}

// drainDirect executes the frozen Direct template against Business PostgreSQL
// and reduces every row through the shared normalizer. The transaction-local
// statement timeout comes from the same approved budget the governed arm uses,
// so the control arm is not given a longer allowance than the measured one.
func (adapter *artifactAdapter) drainDirect(ctx context.Context,
	query finalv5contracts.QueryContract) (finalv5contracts.ObservedResult, error) {
	return finalv5contracts.NormalizeDirect(query.Schema, func(yield func([]any) error) error {
		tx, err := adapter.real.business.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('statement_timeout', $1, true)`,
			directStatementTimeout); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, query.Direct.SQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		if len(rows.FieldDescriptions()) != query.Columns {
			return errors.New("Direct PostgreSQL result width differs from the contract projection")
		}
		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				return err
			}
			if err := yield(values); err != nil {
				return err
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}

// directStatementTimeout matches the reviewed benchmark profile's query
// timeout. gateway_reader carries a 5s session default that a complete 100k-row
// drain cannot meet, and silently truncating the control arm would fake
// agreement rather than measure it.
const directStatementTimeout = "1800000"

// verifyDatasetProbe records the live deployment fingerprint produced by the
// contract-indexed probe. It is a deployment observation, not a logical oracle.
func (adapter *artifactAdapter) verifyDatasetProbe(ctx context.Context) (string, error) {
	probe, err := adapter.runtime.DatasetProbeSQL()
	if err != nil {
		return "", err
	}
	tx, err := adapter.real.business.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('statement_timeout', $1, true)`,
		directStatementTimeout); err != nil {
		return "", err
	}
	rows, err := tx.Query(ctx, probe)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if len(rows.FieldDescriptions()) != 1 || !rows.Next() {
		return "", errors.New("dataset probe must return exactly one scalar row")
	}
	var fingerprint string
	if err := rows.Scan(&fingerprint); err != nil || rows.Next() {
		return "", errors.New("dataset probe must return exactly one scalar row")
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return sha(fingerprint), nil
}

// businessSnapshot counts the statements the Gateway reader issued against the
// exact relation the live Catalog binds, so a governed query cannot be credited
// to some other Product's traffic.
func (adapter *artifactAdapter) businessSnapshot(ctx context.Context,
	binding finalv5contracts.Binding) (experiment.BusinessSQLSnapshot, error) {
	return adapter.real.businessSQLSnapshotFor(ctx, boundTaskRequest{
		VisibleRelation:   binding.ReportingView,
		CompanionRelation: binding.OrdinalSidecar,
	})
}

// statementCensus decomposes every gateway_reader statement into the closed
// class set against the same bound relations the targeted counters use.
func (adapter *artifactAdapter) statementCensus(ctx context.Context,
	binding finalv5contracts.Binding) (experiment.GatewayStatementCensus, error) {
	return adapter.real.gatewayStatementCensus(ctx, binding.ReportingView, binding.OrdinalSidecar)
}

// provisionArtifactTask requests exactly the Product, ordered projection and
// complete scope domain the contract and the live Catalog agree on, then drives
// the real OA submit/approve cycle.
func (adapter *artifactAdapter) provisionArtifactTask(ctx context.Context, operation experiment.AdapterOperation,
	binding finalv5contracts.Binding) (string, error) {
	return adapter.real.provisionBoundTask(ctx, operation, boundTaskRequest{
		Objective:         "IEEE TKDE Final-V5 artifact " + binding.Cell.String(),
		DataProducts:      []string{binding.ProductID},
		Columns:           map[string][]string{binding.ProductID: binding.Columns},
		Scopes:            binding.Scopes,
		VisibleRelation:   binding.ReportingView,
		CompanionRelation: binding.OrdinalSidecar,
	})
}

// observedTaskgateQueryPrefix retains only redacted, already returned service
// telemetry before the composite receipt/artifact verifier finishes. It never
// marks the response verified or available and never stores raw identifiers.
func observedTaskgateQueryPrefix(operation experiment.AdapterOperation, taskID, sqlText string,
	started time.Time, availableMS float64, response queryResponse,
	before, after experiment.RootLedgerSnapshot) experiment.Sample {
	sample := baseSample(operation, "taskgate")
	sample.ClientAvailableMS, sample.ClientFullDrainMS = availableMS, durationMS(time.Since(started))
	for name, value := range response.PipelineMS {
		sample.PipelineMS[name] = value
	}
	sample.DiagnosticMS = make(map[string]float64, len(response.DiagnosticMS))
	for name, value := range response.DiagnosticMS {
		sample.DiagnosticMS[name] = value
	}
	if response.RowCount >= 0 {
		sample.RowCount = response.RowCount
	}
	if response.ColumnCount >= 0 {
		sample.ColumnCount = response.ColumnCount
	}
	sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256 = sha(sqlText), sha(sqlText)
	if validDigest(response.PlanDigest) {
		sample.QueryPlanSHA256 = response.PlanDigest
	}
	sample.ActualReleaseFacts, sample.ChargedReleaseFacts = response.Exposure.ActualReleaseFacts, response.Exposure.ChargedReleaseFacts
	sample.ActualDependencyFacts, sample.ChargedDependencyFacts = response.Exposure.ActualInfluenceFacts, response.Exposure.ChargedInfluenceFacts
	sample.ActualOutcomeFacts, sample.ChargedOutcomeFacts = response.Exposure.ActualOutcomeFacts, response.Exposure.ChargedOutcomeFacts
	if validDigest(response.Exposure.ReleaseSetSHA256) {
		sample.ReleaseSetSHA256 = response.Exposure.ReleaseSetSHA256
	}
	if validDigest(response.Exposure.InfluenceSetSHA256) {
		sample.DependencySetSHA256 = response.Exposure.InfluenceSetSHA256
	}
	if validDigest(response.Exposure.OutcomeSetSHA256) {
		sample.OutcomeSetSHA256 = response.Exposure.OutcomeSetSHA256
	}
	sample.RootEpochBefore, sample.RootEpochAfter = before.Epoch, after.Epoch
	sample.RootSetSHA256Before, sample.RootSetSHA256After = rootSetDigest(before), rootSetDigest(after)
	if taskID != "" {
		sample.RootTaskIDHash = saltedTaskHash(operation, taskID)
	}
	return sample
}
