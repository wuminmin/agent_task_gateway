package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5dataset"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

const artifactVerificationVersion = "taskgate-final-v5-artifact-verification-v2"

// artifactAdapter executes the six frozen result-heavy cells. Everything it
// runs -- the workload matrix, the rendered query text, the Product and
// Publication it binds, the approved projection, and the expected result -- is
// derived from the verified Contract Index through the Contract-to-Runtime
// Bridge. The Adapter deliberately keeps no second workload table of its own.
type artifactAdapter struct {
	real    *realAdapter
	runtime *finalv5contracts.Runtime
	live    finalv5contracts.LiveDeployment
	// finalizer is the acceptance authority for every cell this adapter runs.
	//
	// It is received already opened. The Adapter cannot assemble one, cannot name
	// the sources it reads, and cannot supply any of the material it judges by;
	// what it can do is pre-register a classification before an operation runs and
	// submit evidence about that operation afterwards.
	finalizer *experiment.RuntimeFinalizerV3
}

// newArtifactAdapter binds the frozen contracts to the running deployment.
// A missing contract, a drifted contract digest, or an absent live Catalog
// leaves the adapter unconstructed and therefore fail-closed.
func newArtifactAdapter(ctx context.Context) (sourceControlledAdapter, error) {
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		return nil, fmt.Errorf("contract bridge: %w", err)
	}
	live, err := liveDeploymentBinding(ctx, runtime)
	if err != nil {
		return nil, err
	}
	// Opened here rather than per cell, and refused rather than deferred. A
	// deployment that cannot produce a finalizer cannot accept a sample either, so
	// discovering that after running six measured cells would waste the run and
	// tempt a later reader into treating the unaccepted samples as data.
	finalizer, err := experiment.OpenDeploymentFinalizerV3(ctx)
	if err != nil {
		return nil, fmt.Errorf("open the v3 acceptance authority: %w", err)
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	real.timeout = 30 * time.Minute
	real.http.Timeout = real.timeout
	return &artifactAdapter{real: real, runtime: runtime, live: live, finalizer: finalizer}, nil
}

// liveDeploymentBinding reads the Catalog and all five typed Dataset Products
// the Gateway is actually running. The Adapter never assumes the reviewed
// candidate is installed: both live identities are independently acquired and
// the Catalog digest is later cross-checked against every measured Receipt.
func liveDeploymentBinding(ctx context.Context,
	runtime *finalv5contracts.Runtime) (finalv5contracts.LiveDeployment, error) {
	if ctx == nil {
		return finalv5contracts.LiveDeployment{}, errors.New("live deployment binding requires a context")
	}
	if runtime == nil {
		return finalv5contracts.LiveDeployment{}, errors.New("verified Contract Runtime is required")
	}
	path := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_CATALOG"))
	if path == "" {
		return finalv5contracts.LiveDeployment{}, errors.New("TASKGATE_FINAL_V5_CATALOG is required")
	}
	digest, err := experiment.FileSHA256(path)
	if err != nil {
		return finalv5contracts.LiveDeployment{}, fmt.Errorf("live catalog: %w", err)
	}
	dsn := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN"))
	if dsn == "" {
		return finalv5contracts.LiveDeployment{}, errors.New("TASKGATE_FINAL_V5_BUSINESS_DSN is required")
	}
	referenceDatasetSHA, err := runtime.DatasetIdentitySHA256()
	if err != nil {
		return finalv5contracts.LiveDeployment{}, fmt.Errorf("reviewed typed benchmark Dataset identity: %w", err)
	}
	agreement, err := finalv5dataset.VerifyBenchmarkPostgreSQL(ctx, dsn)
	if err != nil {
		return finalv5contracts.LiveDeployment{}, fmt.Errorf("full live typed benchmark Dataset: %w", err)
	}
	if agreement.Reference.SHA256 != referenceDatasetSHA {
		return finalv5contracts.LiveDeployment{}, errors.New(
			"live Dataset verifier used a different reviewed benchmark formula")
	}
	return finalv5contracts.LiveDeployment{
		CatalogPath: path, CatalogSHA256: digest, DatasetSHA256: agreement.Observed.SHA256,
	}, nil
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
		return retainTaskGateRejection(retainedArtifactFailure(operation, sample, artifactFailureCode(err)), err)
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
	// The classification this window will be judged by, committed before there
	// are any observations to choose it against. The finalizer computes it: a
	// manifest the measured party chose is a standard the measured party set.
	selector := artifactContractSelector(cell.Identity)
	// The request identity is fixed before the observer window opens. The
	// finalizer's one-use ticket binds this exact task/request attempt to the
	// random window it issues, so another sample of the same stable contract cell
	// cannot donate either half of its measurement.
	requestID := "final-v5-artifact-" + sha(operation.SampleID)[:24]
	registered, err := adapter.finalizer.OpenObserverWindowV3(ctx, selector,
		experiment.ObserverAttemptV3{TaskID: state.taskID, RequestID: requestID})
	if err != nil {
		return experiment.Sample{}, err
	}
	observerBefore, err := captureBoundObserverV2(ctx, experiment.ObserverInvocationV3{
		Phase: "before", ObserverWindowID: registered.ObserverWindowID,
		ClassifierManifestSHA256: registered.ClassifierManifestSHA256,
	})
	if err != nil {
		return experiment.Sample{}, err
	}
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
	observerAfter, err := captureBoundObserverV2(ctx, experiment.ObserverInvocationV3{
		Phase: "after", ObserverWindowID: registered.ObserverWindowID,
		ClassifierManifestSHA256: registered.ClassifierManifestSHA256,
	})
	if err != nil {
		return sample, err
	}
	window := experiment.ObserverWindowV2{Before: observerBefore, After: observerAfter}
	sample.ArtifactVerification, err = adapter.artifactEvidence(sample, binding, query, comparison,
		businessBefore, businessAfter, beforeRoot, afterRoot, window)
	if err != nil {
		return sample, err
	}
	// The resource counters come from the window's own snapshots. They used to be
	// filled by the v1.4 accounting pass; the window carries the same readings and
	// refuses the same disturbances -- a restart or an OOM inside it is not a
	// smaller delta, it is two different processes.
	resource, err := window.ResourceDelta()
	if err != nil {
		return sample, err
	}
	sample.GatewayMemoryPeakBytes = resource.GatewayMemoryPeakBytes
	sample.GatewayCPUUsecDelta = resource.GatewayCPUUsecDelta
	sample.GatewayNetworkRXDelta = resource.GatewayNetworkRXDelta
	sample.GatewayNetworkTXDelta = resource.GatewayNetworkTXDelta
	sample.ControlWALBytesDelta = resource.ControlWALBytesDelta
	sample.BusinessWALBytesDelta = resource.BusinessWALBytesDelta
	// Profile campaigns use the process-scoped alias resolved independently
	// from the registry. Legacy publication/targeted launchers do not supply the
	// alias yet, so retain their stronger Result-heavy resolver unchanged.
	if adapterSampleProfileBinder != nil && adapterSampleProfileBinder.Active() {
		if err := adapterSampleProfileBinder.BindSample(&sample); err != nil {
			return sample, err
		}
	} else {
		sample.ProfileBinding, err = artifactProfileBindingFor(sample.BaselineVerification.Receipt.CatalogDigest)
		if err != nil {
			return sample, err
		}
	}
	visibleDelta := businessAfter.VisibleCalls - businessBefore.VisibleCalls
	companionDelta := businessAfter.CompanionCalls - businessBefore.CompanionCalls
	sample.BusinessSQLDelta = visibleDelta + companionDelta
	if visibleDelta != 1 || companionDelta != 1 {
		return sample, errors.New("artifact query did not execute exactly one visible and companion Business statement")
	}
	// Acceptance. Everything above this line is measurement; this is where the
	// sample stops being the Adapter's opinion of itself.
	//
	// The Adapter submits the receipt, what it observed and the case number. The
	// finalizer fetches the frozen contract, the activated Catalog, the retained
	// qualification, the runtime identity and the Control Store's account of the
	// request, reproduces the execution for itself and compares. Nothing the
	// Adapter passes here feeds a derivation -- see the header of
	// experiment/runtime_finalizer_v3.go for why the request can carry nothing
	// else.
	carried, err := carriedArtifactEvidence(registered, window, sample.BaselineVerification.Receipt)
	if err != nil {
		return sample, err
	}
	finalized, err := adapter.finalizer.FinalizeTaskGateObservationV3(ctx, experiment.FinalizationRequestV3{
		Receipt: sample.BaselineVerification.Receipt, Carried: carried, ContractSelector: selector,
		ObserverWindowTicket: registered.ObserverWindowTicket,
		// ReturnedReceiptJSON is deliberately absent. It is read only where the
		// Control Store reports an exact request-ID replay, and this path issues a
		// fresh request id per sample; a replay reported here would therefore be a
		// finalization failure, which is the correct outcome for an artifact cell
		// that did not execute.
	})
	if err != nil {
		return sample, err
	}
	sample.TaskGateAcceptanceV3 = &finalized
	// Dataset identity and the deployment probe have independent meanings only on
	// the explicit sample-v3 wire. Historical sample-v1 bytes retain their former
	// evidence-v1 equality rule, and finalizer refusals remain rejection-only v2.
	sample.SchemaVersion = experiment.FinalizedSampleSchemaVersion
	return sample, nil
}

// artifactContractSelector narrows the frozen contract search to one cell.
//
// It is a hint and carries no authority: finalization prepares every candidate
// the selector admits and keeps the one whose preparation the Gateway signed, so
// naming the wrong cell can only cause a rejection. Pre-registration is the one
// place it has to name exactly one, because there is no receipt yet to identify
// the operation by -- and a wrong name there produces a classification the
// finalization then rebuilds differently and refuses.
func artifactContractSelector(cell finalv5contracts.CellIdentity) experiment.FrozenContractSelectorV3 {
	return experiment.FrozenContractSelectorV3{
		ExperimentID: cell.ExperimentID, WorkloadID: cell.WorkloadID,
		Scale: cell.Scale, Mode: cell.Mode,
	}
}

// carriedArtifactEvidence assembles what the Adapter submits for one operation.
//
// Every member is either an observation the Adapter made or a value it is
// transcribing, and none of it is trusted material:
//
//   - the window is what the independent observer produced, sealed under the
//     classification committed before it opened;
//   - the statement identities are read off the Gateway's own signed execution
//     binding, and the finalizer compares them against statements it reproduces
//     for itself -- so transcribing them wrongly is a rejection;
//   - the operation, plan and classifier identities are the pre-registration,
//     carried back unchanged. The Adapter cannot derive them, because a
//     classifier manifest is built from the rendered target statements and an
//     Adapter holding those would hold the material its claim is checked
//     against. What carrying them establishes is that the operation the Gateway
//     signed is the one whose classification was pre-registered.
func carriedArtifactEvidence(registered experiment.PreRegisteredObservationV3,
	window experiment.ObserverWindowV2,
	receipt queryreceipt.QueryReceiptV1) (experiment.CarriedEvidenceV3, error) {
	signed := receipt.ExecutionBindingV2
	if signed == nil {
		return experiment.CarriedEvidenceV3{},
			errors.New("the receipt describes no execution, so there is nothing to submit about one")
	}
	if signed.Companion == nil {
		return experiment.CarriedEvidenceV3{},
			errors.New("a governed artifact query settles a provenance companion, and the receipt signs none")
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

// signedTargetStatement transcribes one signed target record as the statement
// identity the finalizer compares against its own reproduction.
//
// The prepared target binding is not part of it: physicalquery.StatementIdentity
// describes a statement, not its place in a compiled operation, and it is carried
// beside the identity so a statement cannot be presented as another target's.
func signedTargetStatement(record querybinding.TargetRecordV1) *physicalquery.StatementIdentity {
	return &physicalquery.StatementIdentity{
		ExactSHA256: record.ExactSQLSHA256, StrictASTSHA256: record.StrictASTSHA256,
		RowLimit: record.RowLimit, Fingerprint: record.PolicyFingerprint,
	}
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
	window experiment.ObserverWindowV2) (*experiment.ArtifactVerificationEvidence, error) {
	if sample.BaselineVerification == nil || sample.BaselineVerification.Receipt.CatalogDigest != binding.CatalogSHA256 {
		return nil, errors.New("the Gateway signed a different Catalog digest than the live Catalog this Adapter bound")
	}
	if sample.RowCount != comparison.BDGRows || sample.ColumnCount != comparison.BDGColumns {
		return nil, errors.New("released artifact intent shape differs from the drained Parquet result")
	}
	if sample.ParquetBytes <= 0 || sample.EncryptedObjectBytes <= 0 {
		return nil, errors.New("released artifact byte observations are absent")
	}
	bindingSHA256, err := binding.SHA256()
	if err != nil {
		return nil, err
	}
	return &experiment.ArtifactVerificationEvidence{
		Version: artifactVerificationVersion, BindingSHA256: bindingSHA256,
		DatasetSHA256: binding.DatasetSHA256, CatalogSHA256: binding.CatalogSHA256,
		DatasetProbeSHA256: binding.DatasetProbeSHA256, QuerySHA256: query.BDG.SQLSHA256,
		ExpectedRows: comparison.ExpectedRows, ExpectedColumns: comparison.ExpectedColumns,
		ExpectedResultSHA256: comparison.ExpectedResultSHA256,
		ObservedRows:         comparison.BDGRows, ObservedColumns: comparison.BDGColumns,
		ObservedResultSHA256: comparison.BDGResultSHA256,
		BusinessBefore:       businessBefore, BusinessAfter: businessAfter,
		RootBefore: beforeRoot, RootAfter: afterRoot, ObserverWindow: window,
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
