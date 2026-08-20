package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/parquet-go/parquet-go"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/releasedartifact"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

const (
	pilotDirectSQL   = "SELECT receipt_no, department FROM reporting.expense_detail WHERE department = '销售部' ORDER BY receipt_no ASC LIMIT 3"
	pilotTaskGateSQL = "SELECT receipt_no, department FROM expense_detail WHERE department = '销售部' ORDER BY receipt_no ASC LIMIT 3"
	// pilotRewriteSQL is the Pilot's normalized_rewrite_replay text: the same
	// statement with different padding and spacing and no other change.
	pilotRewriteSQL = "  SELECT receipt_no, department FROM expense_detail WHERE department='销售部' ORDER BY receipt_no ASC LIMIT 3  "
)

type realAdapter struct {
	gatewayBase  string
	oaBase       string
	timeout      time.Duration
	http         *http.Client
	alice        *mcpClient
	carol        *mcpClient
	aliceOA      *http.Client
	bobOA        *http.Client
	business     *pgxpool.Pool
	observer     *pgxpool.Pool
	control      *pgxpool.Pool
	objectStore  *minio.Client
	objectBucket string
	verifier     *queryreceipt.Verifier
	keyBundle    queryreceipt.PublicKeyBundleV1
	pairs        map[string]*pairState
}

type pairState struct {
	taskID                 string
	novelRequestID         string
	novelQueryID           string
	novelResultID          string
	novelObservationSHA256 string
	novelGrantSHA256       string
	historyDependencyLink  *experiment.ScaleDependencySetVerificationV1
}

type queryResponse struct {
	ResultID         string                          `json:"result_id"`
	QueryID          string                          `json:"query_id"`
	TaskID           string                          `json:"task_id"`
	ArtifactStatus   string                          `json:"artifact_status"`
	RowCount         int64                           `json:"row_count"`
	ColumnCount      int                             `json:"column_count"`
	PipelineMS       map[string]float64              `json:"pipeline_ms"`
	DiagnosticMS     map[string]float64              `json:"diagnostic_ms"`
	PlanDigest       string                          `json:"plan_digest"`
	SemanticReplay   bool                            `json:"semantic_replay"`
	IdempotentReplay bool                            `json:"idempotent_replay"`
	OutcomeRadix     control.OutcomeRadixTelemetryV5 `json:"outcome_radix"`
	Receipt          queryreceipt.QueryReceiptV1     `json:"receipt"`
	Exposure         struct {
		QueryID                   string `json:"query_id"`
		RootTaskID                string `json:"root_task_id"`
		ProfileVersion            string `json:"profile_version"`
		ActualReleaseFacts        int64  `json:"actual_release_facts"`
		ActualInfluenceFacts      int64  `json:"actual_influence_facts"`
		ActualOutcomeFacts        int64  `json:"actual_outcome_facts"`
		ChargedReleaseFacts       int64  `json:"charged_release_facts"`
		ChargedInfluenceFacts     int64  `json:"charged_influence_facts"`
		ChargedOutcomeFacts       int64  `json:"charged_outcome_facts"`
		ActualPredicateAtomCount  int64  `json:"actual_predicate_atom_count"`
		ChargedPredicateAtomCount int64  `json:"charged_predicate_atom_count"`
		CompositeOutcomeSHA256    string `json:"composite_outcome_sha256"`
		PredicateContextSHA256    string `json:"predicate_context_sha256"`
		PredicateSetSHA256        string `json:"predicate_set_sha256"`
		ObservationSHA256         string `json:"observation_sha256"`
		DictionarySetDigest       string `json:"dictionary_set_digest"`
		ReleaseSetSHA256          string `json:"release_set_sha256"`
		InfluenceSetSHA256        string `json:"influence_set_sha256"`
		OutcomeSetSHA256          string `json:"outcome_set_sha256"`
		RootEpoch                 int64  `json:"root_epoch"`
	} `json:"exposure"`
}

type rawTerminalIdentity struct {
	taskID                string
	rootTaskID            string
	queryID               string
	requestID             string
	resultID              string
	receiptID             string
	queryStatus           string
	reservationStatus     string
	artifactStatus        string
	requestDigest         string
	grantDigest           string
	resultSHA256          string
	observationSHA256     string
	receiptSHA256         string
	receiptIdentitySHA256 string
	intentSHA256          string
	objectKey             string
	objectSHA256          string
	parquetSHA256         string
	objectSize            int64
	parquetSize           int64
	rootEpoch             int64
	settlementAudits      int64
	terminalAudits        int64
	registrationAudits    int64
	availabilityAudits    int64
	terminalSequence      int64
	registrationSequence  int64
	availabilitySequence  int64
	terminalHash          string
	registrationHash      string
	availabilityHash      string
	receipt               queryreceipt.QueryReceiptV1
}

type rawCrossBinding struct {
	taskID, rootTaskID, queryID, grantDigest string
	cacheKeySHA256, observationSHA256        string
	sourceQueryID, rootFirstQueryID          string
	sqlFingerprint, catalogDigest            string
	schemaDigest, datasourceID               string
	semanticReplayAudits, settlementAudits   int64
}

func newRealAdapter(ctx context.Context) (*realAdapter, error) {
	required := func(name string) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("%s is required", name)
		}
		return value, nil
	}
	aliceToken, err := required("TASKBOUND_ALICE_TOKEN")
	if err != nil {
		return nil, err
	}
	carolToken, err := required("TASKBOUND_CAROL_TOKEN")
	if err != nil {
		return nil, err
	}
	alicePassword, err := required("OA_ALICE_PASSWORD")
	if err != nil {
		return nil, err
	}
	bobPassword, err := required("OA_BOB_PASSWORD")
	if err != nil {
		return nil, err
	}
	businessDSN, err := required("TASKGATE_FINAL_V5_BUSINESS_DSN")
	if err != nil {
		return nil, err
	}
	controlDSN, err := required("TASKGATE_FINAL_V5_CONTROL_DSN")
	if err != nil {
		return nil, err
	}
	observerDSN, err := required("TASKGATE_FINAL_V5_BUSINESS_OBSERVER_DSN")
	if err != nil {
		return nil, err
	}
	objectEndpoint, err := required("TASKGATE_FINAL_V5_OBJECT_STORE_URL")
	if err != nil {
		return nil, err
	}
	objectAccessKey, err := required("TASKGATE_FINAL_V5_OBJECT_STORE_ACCESS_KEY")
	if err != nil {
		return nil, err
	}
	objectSecretKey, err := required("TASKGATE_FINAL_V5_OBJECT_STORE_SECRET_KEY")
	if err != nil {
		return nil, err
	}
	objectBucket, err := required("TASKGATE_FINAL_V5_OBJECT_STORE_BUCKET")
	if err != nil {
		return nil, err
	}
	gatewayBase := strings.TrimRight(envOr("TASKGATE_FINAL_V5_GATEWAY_URL", "http://127.0.0.1:8082"), "/")
	oaBase := strings.TrimRight(envOr("TASKGATE_FINAL_V5_OA_URL", "http://127.0.0.1:8092"), "/")
	timeout := 30 * time.Second
	client := &http.Client{Timeout: timeout}
	business, err := pgxpool.New(ctx, businessDSN)
	if err != nil {
		return nil, err
	}
	observer, err := pgxpool.New(ctx, observerDSN)
	if err != nil {
		business.Close()
		return nil, err
	}
	control, err := pgxpool.New(ctx, controlDSN)
	if err != nil {
		business.Close()
		observer.Close()
		return nil, err
	}
	closeOnError := func(err error) (*realAdapter, error) {
		business.Close()
		observer.Close()
		control.Close()
		return nil, err
	}
	if err := business.Ping(ctx); err != nil {
		return closeOnError(err)
	}
	if err := control.Ping(ctx); err != nil {
		return closeOnError(err)
	}
	if err := observer.Ping(ctx); err != nil {
		return closeOnError(err)
	}
	parsedObjectEndpoint, err := url.Parse(objectEndpoint)
	if err != nil || parsedObjectEndpoint.Host == "" || (parsedObjectEndpoint.Scheme != "http" && parsedObjectEndpoint.Scheme != "https") {
		return closeOnError(errors.New("invalid object-store URL"))
	}
	objectStore, err := minio.New(parsedObjectEndpoint.Host, &minio.Options{Creds: credentials.NewStaticV4(objectAccessKey, objectSecretKey, ""), Secure: parsedObjectEndpoint.Scheme == "https", BucketLookup: minio.BucketLookupPath})
	if err != nil {
		return closeOnError(err)
	}
	aliceOA, err := oaClient(oaBase, "alice", alicePassword, timeout)
	if err != nil {
		return closeOnError(err)
	}
	bobOA, err := oaClient(oaBase, "bob", bobPassword, timeout)
	if err != nil {
		return closeOnError(err)
	}
	var bundle queryreceipt.PublicKeyBundleV1
	keyBytes, err := httpGet(ctx, client, gatewayBase+"/.well-known/taskgate/query-receipt-keyring.json", 1<<20)
	if err != nil {
		return closeOnError(err)
	}
	if err := json.Unmarshal(keyBytes, &bundle); err != nil {
		return closeOnError(err)
	}
	verifier, err := bundle.Verifier()
	if err != nil {
		return closeOnError(err)
	}
	return &realAdapter{gatewayBase: gatewayBase, oaBase: oaBase, timeout: timeout, http: client,
		alice: &mcpClient{url: gatewayBase + "/mcp", token: aliceToken, http: client}, carol: &mcpClient{url: gatewayBase + "/mcp", token: carolToken, http: client},
		aliceOA: aliceOA, bobOA: bobOA, business: business, observer: observer, control: control, objectStore: objectStore, objectBucket: objectBucket, verifier: verifier, keyBundle: bundle, pairs: map[string]*pairState{}}, nil
}

func (adapter *realAdapter) Close() {
	adapter.business.Close()
	adapter.observer.Close()
	adapter.control.Close()
}

// baselinePlan is everything one Baseline cell executes with: the exact bytes
// of each arm, how its Task is provisioned, and which relations the Observer
// counts against. The non-publication S1/tiny Pilot and the frozen contract
// cells differ only in this value, so both run the same measured code.
type baselinePlan struct {
	directSQL   string
	taskGateSQL string
	rewriteSQL  string
	// planEntrypoint routes the governed arm to execute_plan with a QueryPlan
	// document instead of query_sql with SQL text. Baseline S5 is the only
	// workload whose contract names that entrypoint.
	planEntrypoint  bool
	expectedRows    int64
	expectedColumns int
	// resultSchema is the contract's typed schema. When present both arms
	// reduce through it, so the digests being compared describe the same
	// logical rows rather than two Go representations of them.
	resultSchema []finalv5oracle.ResultColumn
	// semanticView identifies the merged statement shape emitted when the
	// driving Product is expanded from a Catalog view_contract.
	semanticView bool
	// visibleRelation and companionRelation are the two names the Observer
	// filters on, carried here only so a failed call-count assertion can show
	// which statements it was counting.
	visibleRelation   string
	companionRelation string
	provision         func(context.Context, experiment.AdapterOperation) (string, error)
	snapshot          func(context.Context) (experiment.BusinessSQLSnapshot, error)
}

// pilotPlan is the retained non-publication S1/tiny path. Its expected shape is
// left zero because the Pilot asserts no contract row count.
func (adapter *realAdapter) pilotPlan() baselinePlan {
	return baselinePlan{
		directSQL:   pilotDirectSQL,
		taskGateSQL: pilotTaskGateSQL,
		rewriteSQL:  pilotRewriteSQL,
		provision:   adapter.provisionTask,
		snapshot:    adapter.businessSQLSnapshot,
	}
}

func (adapter *realAdapter) contractPlan(cell baselineExecutionCell) baselinePlan {
	return baselinePlan{
		directSQL:         cell.DirectSQL,
		taskGateSQL:       cell.BDGSQL,
		rewriteSQL:        cell.RewriteSQL,
		planEntrypoint:    cell.PlanEntrypoint,
		expectedRows:      cell.Contract.ExpectedRows,
		expectedColumns:   cell.Contract.ExpectedColumns,
		resultSchema:      cell.ResultSchema,
		semanticView:      cell.SemanticView,
		visibleRelation:   cell.Task.VisibleRelation,
		companionRelation: cell.Task.CompanionRelation,
		provision: func(ctx context.Context, operation experiment.AdapterOperation) (string, error) {
			return adapter.provisionBoundTask(ctx, operation, cell.Task)
		},
		snapshot: func(ctx context.Context) (experiment.BusinessSQLSnapshot, error) {
			return adapter.businessSQLSnapshotFor(ctx, cell.Task)
		},
	}
}

func (adapter *realAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if operation.ExperimentID != "baseline" {
		return invalidSample(operation, "unsupported_source_controlled_baseline_cell")
	}
	plan := adapter.pilotPlan()
	if operation.WorkloadID != "S1" || operation.Scale != "tiny" {
		cell, err := resolveBaselineExecutionCell(operation)
		if err != nil {
			return invalidSample(operation, "unsupported_source_controlled_baseline_cell")
		}
		plan = adapter.contractPlan(cell)
	}
	stateKey := operation.PairID + "\x00" + operation.RootGroupID
	if adapter.pairs == nil {
		adapter.pairs = map[string]*pairState{}
	}
	state := adapter.pairs[stateKey]
	if state == nil {
		state = &pairState{}
		adapter.pairs[stateKey] = state
	}
	var sample experiment.Sample
	var err error
	switch operation.Mode {
	case "direct":
		sample, err = adapter.direct(ctx, operation, plan)
	case "pending_recovery":
		// Pending recovery is a Pilot-only diagnostic mode; no frozen contract
		// cell declares it, so it never reaches a contract plan.
		sample, err = adapter.pendingRecovery(ctx, operation, state)
	default:
		sample, err = adapter.taskgate(ctx, operation, state, plan)
	}
	if err != nil {
		// The sample carries only the fail-closed code, because a failure
		// reason is not evidence and must not reach the evidence channel. It
		// does belong on the diagnostic channel: without this a targeted run
		// can observe that a cell failed and never why, which is exactly the
		// position S5/SF10 left this repository in on 2026-08-16.
		writeAdapterFailureDiagnostic("baseline", operation, err)
		return invalidSample(operation, "real_measurement_failed")
	}
	return sample
}

func (adapter *realAdapter) direct(ctx context.Context, operation experiment.AdapterOperation, plan baselinePlan) (experiment.Sample, error) {
	started := time.Now()
	tx, err := adapter.business.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return experiment.Sample{}, err
	}
	rows, err := tx.Query(ctx, plan.directSQL)
	if err != nil {
		_ = tx.Rollback(ctx)
		return experiment.Sample{}, err
	}
	var values [][]any
	columnCount := len(rows.FieldDescriptions())
	for rows.Next() {
		row, rowErr := rows.Values()
		if rowErr != nil {
			rows.Close()
			_ = tx.Rollback(ctx)
			return experiment.Sample{}, rowErr
		}
		values = append(values, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		_ = tx.Rollback(ctx)
		return experiment.Sample{}, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return experiment.Sample{}, err
	}
	elapsed := durationMS(time.Since(started))
	digest, err := baselineResultDigest(plan.resultSchema, values)
	if err != nil {
		return experiment.Sample{}, err
	}
	// A contract cell states its result shape, so a Direct arm that drained a
	// different one is a failed sample rather than a quietly recorded number.
	// The Pilot leaves the expectation zero and is not checked.
	if plan.expectedRows > 0 && (int64(len(values)) != plan.expectedRows || columnCount != plan.expectedColumns) {
		return experiment.Sample{}, fmt.Errorf("direct arm drained %dx%d, contract expects %dx%d",
			len(values), columnCount, plan.expectedRows, plan.expectedColumns)
	}
	sample := baseSample(operation, "postgresql")
	sample.ClientAvailableMS, sample.ClientFullDrainMS = elapsed, elapsed
	sample.PipelineMS = zeroPipeline()
	sample.PipelineMS["execute_and_derive"], sample.PipelineMS["server_total"] = elapsed, elapsed
	sample.RowCount, sample.ColumnCount, sample.ResultSHA256 = int64(len(values)), columnCount, digest
	sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256, sample.QueryPlanSHA256 = sha(plan.directSQL), sha(plan.taskGateSQL), sha(plan.directSQL)
	sample.Status = "pass"
	return sample, nil
}

func (adapter *realAdapter) taskgate(ctx context.Context, operation experiment.AdapterOperation, state *pairState, plan baselinePlan) (experiment.Sample, error) {
	if state.taskID == "" {
		taskID, err := plan.provision(ctx, operation)
		if err != nil {
			return experiment.Sample{}, err
		}
		state.taskID = taskID
	}
	requestID := "final-v5-" + sha(operation.PairID)[:20] + "-novel"
	sqlText := plan.taskGateSQL
	switch operation.Mode {
	case "novel":
		state.novelRequestID = requestID
	case "semantic_replay":
		requestID = "final-v5-" + sha(operation.SampleID)[:20] + "-semantic"
	case "normalized_rewrite_replay":
		requestID = "final-v5-" + sha(operation.SampleID)[:20] + "-normalized"
		sqlText = plan.rewriteSQL
	case "idempotent_replay":
		if state.novelRequestID == "" {
			return experiment.Sample{}, errors.New("idempotent replay lacks novel anchor")
		}
		requestID = state.novelRequestID
	default:
		return experiment.Sample{}, errors.New("unsupported baseline mode")
	}
	beforeRoot, err := adapter.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil {
		return experiment.Sample{}, err
	}
	var idempotentBefore *experiment.IdempotentControlSnapshot
	if operation.Mode == "idempotent_replay" {
		snapshot, snapshotErr := adapter.idempotentControlSnapshot(ctx, operation, state.taskID, requestID)
		if snapshotErr != nil {
			return experiment.Sample{}, snapshotErr
		}
		idempotentBefore = &snapshot
	}
	businessBefore, err := plan.snapshot(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	if idempotentBefore != nil {
		idempotentBefore.Business = businessBefore
	}
	started := time.Now()
	response, err := adapter.callGovernedArm(ctx, plan, state.taskID, requestID, sqlText)
	if err != nil {
		return experiment.Sample{}, err
	}
	availableMS := durationMS(time.Since(started))
	businessAfter, err := plan.snapshot(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	afterRoot, err := adapter.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil {
		return experiment.Sample{}, err
	}
	sample, parquetBytes, err := adapter.completeTaskgateSampleWithParquet(ctx, operation, state,
		beforeRoot, afterRoot, started, availableMS, sqlText, response)
	if err != nil {
		return experiment.Sample{}, err
	}
	// Reduce the released artifact through the contract schema so the governed
	// digest is comparable with the Direct arm's. Without this the pair differs
	// for every rich type even when both arms read identical rows.
	if len(plan.resultSchema) != 0 {
		observed, normalizeErr := finalv5contracts.NormalizeBDG(plan.resultSchema,
			finalv5contracts.ParquetInput(bytes.NewReader(parquetBytes), int64(len(parquetBytes))))
		if normalizeErr != nil {
			return experiment.Sample{}, normalizeErr
		}
		sample.ResultSHA256 = observed.Summary.CanonicalResultSHA256
	}
	if businessAfter.VisibleCalls < businessBefore.VisibleCalls || businessAfter.CompanionCalls < businessBefore.CompanionCalls {
		return experiment.Sample{}, errors.New("Business SQL observer counters regressed")
	}
	sample.BusinessSQLDelta = businessAfter.VisibleCalls - businessBefore.VisibleCalls + businessAfter.CompanionCalls - businessBefore.CompanionCalls
	sample.ReplayVerification = &experiment.ReplayVerificationEvidence{
		BusinessBefore: businessBefore,
		BusinessAfter:  businessAfter,
		RootBefore:     beforeRoot,
		RootAfter:      afterRoot,
	}
	if operation.Mode == "novel" {
		state.novelQueryID = response.QueryID
		state.novelResultID = response.ResultID
		state.novelObservationSHA256 = response.Exposure.ObservationSHA256
		state.novelGrantSHA256 = response.Receipt.GrantDigest
	}
	if operation.Mode == "semantic_replay" || operation.Mode == "normalized_rewrite_replay" {
		sample.ReplayVerification.SourceObservationSHA256 = state.novelObservationSHA256
		sample.ReplayVerification.ReplayObservationSHA256 = response.Exposure.ObservationSHA256
		if operation.Mode == "semantic_replay" {
			cross, crossErr := adapter.crossBindingVerification(ctx, operation, state, plan)
			if crossErr != nil {
				return experiment.Sample{}, crossErr
			}
			sample.CrossBindingVerification = &cross
		}
	}
	if operation.Mode == "idempotent_replay" {
		if idempotentBefore == nil {
			return experiment.Sample{}, errors.New("idempotent before snapshot is absent")
		}
		idempotentAfter, snapshotErr := adapter.idempotentControlSnapshot(ctx, operation, state.taskID, requestID)
		if snapshotErr != nil {
			return experiment.Sample{}, snapshotErr
		}
		idempotentAfter.Business = businessAfter
		sample.IdempotentVerification = &experiment.IdempotentVerificationEvidence{
			Before:   *idempotentBefore,
			After:    idempotentAfter,
			Returned: responseTerminalIdentityEvidence(operation, response, sample.BaselineVerification.VerifierManifest),
		}
	}
	return sample, nil
}

// callGovernedArm sends the governed arm through the entrypoint its contract
// names. A plan cell submits the rendered QueryPlan document; every other cell
// submits SQL text. The plan is decoded here rather than forwarded as a string
// because execute_plan takes a structured plan, and a cell that sent JSON as
// SQL would fail for a reason that reads like a different fault.
func (adapter *realAdapter) callGovernedArm(ctx context.Context, plan baselinePlan,
	taskID, requestID, payload string) (queryResponse, error) {
	var response queryResponse
	if !plan.planEntrypoint {
		err := adapter.alice.call(ctx, "query_sql",
			map[string]any{"task_id": taskID, "request_id": requestID, "sql": payload}, &response)
		return response, err
	}
	var document any
	if err := json.Unmarshal([]byte(payload), &document); err != nil {
		return response, fmt.Errorf("rendered QueryPlan is not a JSON document: %w", err)
	}
	err := adapter.alice.call(ctx, "execute_plan",
		map[string]any{"task_id": taskID, "request_id": requestID, "plan": document}, &response)
	return response, err
}

func (adapter *realAdapter) completeTaskgateSample(ctx context.Context, operation experiment.AdapterOperation, state *pairState,
	before, after experiment.RootLedgerSnapshot, started time.Time, availableMS float64, sqlText string, response queryResponse) (experiment.Sample, error) {
	sample, _, err := adapter.completeTaskgateSampleWithParquet(ctx, operation, state, before, after, started, availableMS, sqlText, response)
	return sample, err
}

// completeTaskgateSampleWithParquet additionally returns the verified released
// Parquet bytes. The Artifact experiment re-reduces exactly those bytes through
// the independent oracle, so it must not download or re-derive them separately.
func (adapter *realAdapter) completeTaskgateSampleWithParquet(ctx context.Context, operation experiment.AdapterOperation, state *pairState,
	before, after experiment.RootLedgerSnapshot, started time.Time, availableMS float64, sqlText string, response queryResponse) (experiment.Sample, []byte, error) {
	if response.TaskID != state.taskID || response.ArtifactStatus != "AVAILABLE" || response.ResultID == "" || response.QueryID == "" {
		return experiment.Sample{}, nil, errors.New("query response omitted AVAILABLE identity")
	}
	if err := requireVerifiedReceipt(adapter.verifier, response.Receipt); err != nil {
		return experiment.Sample{}, nil, fmt.Errorf("query response omitted a verified receipt: %w", err)
	}
	if response.Receipt.QueryID != response.QueryID || response.Receipt.TaskID != state.taskID {
		return experiment.Sample{}, nil, errors.New("the receipt names another query or task")
	}
	auditEvidence, err := adapter.loadAuditEvidence(ctx, response)
	if err != nil {
		return experiment.Sample{}, nil, err
	}
	var delivery struct {
		DownloadURL    string `json:"download_url"`
		ArtifactSHA256 string `json:"artifact_sha256"`
	}
	if err := adapter.alice.call(ctx, "deliver_result", map[string]any{"result_id": response.ResultID, "format": "parquet"}, &delivery); err != nil {
		return experiment.Sample{}, nil, err
	}
	if delivery.DownloadURL == "" || delivery.ArtifactSHA256 != response.Receipt.ArtifactIntent.ParquetSHA256 {
		return experiment.Sample{}, nil, errors.New("delivery metadata disagrees with receipt")
	}
	parquetBytes, err := httpGet(ctx, adapter.http, delivery.DownloadURL, 1<<30)
	if err != nil {
		return experiment.Sample{}, nil, err
	}
	if shaBytes(parquetBytes) != delivery.ArtifactSHA256 {
		return experiment.Sample{}, nil, errors.New("downloaded Parquet digest mismatch")
	}
	expectedBinding, canonicalEvidence, canonicalObject, err := adapter.openCanonicalObject(ctx, response)
	if err != nil {
		return experiment.Sample{}, nil, err
	}
	defer canonicalObject.Close()
	canonicalEvidence.ReleasedParquet = parquetBytes
	availability := auditEvidence.Availability
	transcript, err := releasedartifact.VerifyReleasedArtifactWithTranscript(adapter.verifier, releasedartifact.SettlementEvidence{
		Receipt: response.Receipt, ExpectedBinding: expectedBinding, ReceiptInclusion: auditEvidence.Audit,
		TerminalInclusion: auditEvidence.Terminal, RegistrationInclusion: auditEvidence.Registration,
		AvailabilityInclusion: &availability,
	}, canonicalEvidence)
	if err != nil || !transcript.Passed {
		if err == nil {
			err = errors.New("released-artifact verifier returned no passing transcript")
		}
		return experiment.Sample{}, nil, err
	}
	if err := validateResponseAgainstVerifiedReceipt(response); err != nil {
		return experiment.Sample{}, nil, err
	}
	intent := response.Receipt.ArtifactIntent
	signedExposure := response.Receipt.Exposure
	rows, err := parseParquet(parquetBytes, intent.ResultID, intent.RowCount)
	if err != nil {
		return experiment.Sample{}, nil, err
	}
	resultDigest, err := experiment.CanonicalResultHash(rows)
	if err != nil {
		return experiment.Sample{}, nil, err
	}
	sample := baseSample(operation, "taskgate")
	sample.ClientAvailableMS, sample.ClientFullDrainMS = availableMS, durationMS(time.Since(started))
	sample.PipelineMS = response.PipelineMS
	sample.DiagnosticMS = response.DiagnosticMS
	sample.RowCount, sample.ColumnCount, sample.ResultSHA256 = intent.RowCount, int(intent.ColumnCount), resultDigest
	sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256 = sha(sqlText), sha(sqlText)
	sample.QueryPlanSHA256 = response.PlanDigest
	sample.ActualReleaseFacts, sample.ChargedReleaseFacts = signedExposure.ActualReleaseFacts, signedExposure.ChargedReleaseFacts
	sample.ActualDependencyFacts, sample.ChargedDependencyFacts = signedExposure.ActualInfluenceFacts, signedExposure.ChargedInfluenceFacts
	sample.ActualOutcomeFacts, sample.ChargedOutcomeFacts = signedExposure.ActualOutcomeFacts, signedExposure.ChargedOutcomeFacts
	sample.ReleaseSetSHA256, sample.DependencySetSHA256, sample.OutcomeSetSHA256 = signedExposure.ReleaseSetSHA256, signedExposure.InfluenceSetSHA256, signedExposure.OutcomeSetSHA256
	sample.PredicateAtomCount, sample.CompositeCount = signedExposure.ActualPredicateAtomCount, signedExposure.ActualCompositeCount
	sample.SemanticReplay, sample.IdempotentReplay = response.SemanticReplay, response.IdempotentReplay
	if operation.Mode == "semantic_replay" || operation.Mode == "normalized_rewrite_replay" {
		if !response.SemanticReplay {
			return experiment.Sample{}, nil, errors.New("semantic replay marker missing")
		}
	} else if operation.Mode == "idempotent_replay" || operation.Mode == "pending_recovery" {
		if !response.IdempotentReplay {
			return experiment.Sample{}, nil, errors.New("idempotent replay marker missing")
		}
	}
	sample.RootEpochBefore, sample.RootEpochAfter = before.Epoch, after.Epoch
	sample.RootSetSHA256Before, sample.RootSetSHA256After = rootSetDigest(before), rootSetDigest(after)
	// A delegated task has its own response TaskID but the V8 exposure receipt
	// is bound to the shared authorization root. Always derive the published
	// root identity from the independently verified artifact binding.
	sample.RootTaskIDHash = saltedTaskHash(operation, expectedBinding.RootTaskID)
	sample.ParquetBytes, sample.EncryptedObjectBytes = intent.ParquetSize, intent.ObjectSize
	sample.ArtifactSHA256, sample.ObjectSHA256 = intent.ParquetSHA256, intent.ObjectSHA256
	sample.ReceiptVersion, sample.ReceiptSHA256, sample.ArtifactIntentSHA256 = response.Receipt.Version, receiptDigest(response.Receipt), intent.IntentSHA256
	availabilityBytes, _ := json.Marshal(auditEvidence.Availability)
	sample.AvailabilityAuditSHA256, sample.ReceiptVerified, sample.ArtifactAvailable = shaBytes(availabilityBytes), true, true
	manifest := &experiment.RedactedVerifierManifest{
		VerifierVersion: "taskgate-final-v5-composite-verifier-v1",
		QueryIDHash:     saltedIdentityHash(operation, "query", response.QueryID),
		ResultIDHash:    saltedIdentityHash(operation, "result", response.ResultID),
		RootTaskIDHash:  saltedTaskHash(operation, expectedBinding.RootTaskID),
		ReceiptSHA256:   receiptDigest(response.Receipt), ObservationSHA256: expectedBinding.ObservationSHA256,
		ReleaseSetSHA256: expectedBinding.ReleaseSetSHA256, DependencySetSHA256: expectedBinding.InfluenceSetSHA256,
		OutcomeSetSHA256: expectedBinding.OutcomeSetSHA256, ArtifactIntentSHA256: intent.IntentSHA256,
		ObjectKeySHA256:           intent.ObjectKeySHA256,
		CanonicalCiphertextSHA256: transcript.CiphertextSHA256, CanonicalCiphertextSize: transcript.CiphertextSize,
		ReleasedParquetSHA256: transcript.ReleasedParquetSHA256, ReleasedParquetSize: transcript.ReleasedParquetSize,
		SchemaSHA256:              transcript.ReleasedSchemaSHA256,
		TerminalAuditSequence:     transcript.TerminalAuditSequence,
		RegistrationAuditSequence: transcript.RegistrationAuditSequence,
		AvailabilityAuditSequence: transcript.AvailabilityAuditSequence,
		VerificationResult:        "pass",
	}
	sample.BaselineVerification = &experiment.BaselineVerificationEvidence{
		Receipt: response.Receipt, KeyBundle: adapter.keyBundle, AuditProof: auditEvidence.Audit,
		TerminalProof: auditEvidence.Terminal, RegistrationProof: auditEvidence.Registration,
		AvailabilityProof: auditEvidence.Availability, ArtifactStatus: canonicalEvidence.Status,
		DownloadedParquetSHA256: shaBytes(parquetBytes), ParsedResultSHA256: resultDigest,
		VerifierManifest: manifest,
	}
	sample.Status = "pass"
	return sample, parquetBytes, nil
}

type recoverySnapshot struct {
	queryRecords              int64
	usedQueries               int64
	settlements               int64
	artifactStatus            string
	receiptSHA256             string
	intentSHA256              string
	root                      experiment.RootLedgerSnapshot
	exposure                  queryreceipt.ExposureEvidenceV1
	object                    experiment.CanonicalObjectSnapshot
	settlementAuditSequences  []int64
	terminalAudits            int64
	registrationAudits        int64
	availabilityAudits        int64
	terminalAuditSequence     int64
	registrationAuditSequence int64
	availabilityAuditSequence int64
}

func (adapter *realAdapter) pendingRecovery(ctx context.Context, operation experiment.AdapterOperation, state *pairState) (experiment.Sample, error) {
	if state.taskID == "" {
		taskID, err := adapter.provisionTask(ctx, operation)
		if err != nil {
			return experiment.Sample{}, err
		}
		state.taskID = taskID
	}
	beforeRoot, err := adapter.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil {
		return experiment.Sample{}, err
	}
	before, err := adapter.recoverySnapshot(ctx, state.taskID, "")
	if err != nil {
		return experiment.Sample{}, err
	}
	businessBefore, err := adapter.businessSQLSnapshot(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	if err := adapter.installAvailabilityBlocker(ctx); err != nil {
		return experiment.Sample{}, err
	}
	blockerInstalled := true
	defer func() {
		if blockerInstalled {
			_ = adapter.removeAvailabilityBlocker(context.Background())
		}
	}()
	requestID := "final-v5-" + sha(operation.SampleID)[:20] + "-recovery"
	started := time.Now()
	var ignored queryResponse
	if callErr := adapter.alice.call(ctx, "query_sql", map[string]any{"task_id": state.taskID, "request_id": requestID, "sql": pilotTaskGateSQL}, &ignored); callErr == nil {
		return experiment.Sample{}, errors.New("availability blocker did not force a PENDING response")
	}
	businessAtFailure, err := adapter.businessSQLSnapshot(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	atFailure, err := adapter.recoverySnapshot(ctx, state.taskID, requestID)
	if err != nil {
		return experiment.Sample{}, err
	}
	if atFailure.artifactStatus != "PENDING" || !atFailure.object.Exists {
		return experiment.Sample{}, errors.New("forced failure did not leave a durable PENDING artifact")
	}
	if err := adapter.removeAvailabilityBlocker(ctx); err != nil {
		return experiment.Sample{}, err
	}
	blockerInstalled = false
	var response queryResponse
	if err := adapter.alice.call(ctx, "query_sql", map[string]any{"task_id": state.taskID, "request_id": requestID, "sql": pilotTaskGateSQL}, &response); err != nil {
		return experiment.Sample{}, err
	}
	availableMS := durationMS(time.Since(started))
	businessAfter, err := adapter.businessSQLSnapshot(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	afterRoot, err := adapter.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil {
		return experiment.Sample{}, err
	}
	sample, err := adapter.completeTaskgateSample(ctx, operation, state, beforeRoot, afterRoot, started, availableMS, pilotTaskGateSQL, response)
	if err != nil {
		return experiment.Sample{}, err
	}
	after, err := adapter.recoverySnapshot(ctx, state.taskID, requestID)
	if err != nil {
		return experiment.Sample{}, err
	}
	sample.BusinessSQLDelta = businessAtFailure.VisibleCalls - businessBefore.VisibleCalls +
		businessAtFailure.CompanionCalls - businessBefore.CompanionCalls
	sample.RecoveryVerification = &experiment.RecoveryVerificationEvidence{
		FailureObserved: true, CanonicalObjectObserved: true,
		ArtifactStatusBefore: atFailure.artifactStatus, ArtifactStatusAfter: after.artifactStatus,
		BusinessCallsBefore:    businessBefore.VisibleCalls + businessBefore.CompanionCalls,
		BusinessCallsAtFailure: businessAtFailure.VisibleCalls + businessAtFailure.CompanionCalls,
		BusinessCallsAfter:     businessAfter.VisibleCalls + businessAfter.CompanionCalls,
		QueryRecordsBefore:     before.queryRecords, QueryRecordsAtFailure: atFailure.queryRecords, QueryRecordsAfter: after.queryRecords,
		SettlementsAtFailure: atFailure.settlements, SettlementsAfter: after.settlements,
		UsedQueriesBefore: before.usedQueries, UsedQueriesAtFailure: atFailure.usedQueries, UsedQueriesAfter: after.usedQueries,
		ReceiptSHA256AtFailure: atFailure.receiptSHA256, ReceiptSHA256After: after.receiptSHA256,
		IntentSHA256AtFailure: atFailure.intentSHA256, IntentSHA256After: after.intentSHA256,
		BusinessBeforeSnapshot: businessBefore, BusinessAtFailureSnapshot: businessAtFailure, BusinessAfterSnapshot: businessAfter,
		RootAtFailure: atFailure.root, RootAfter: after.root,
		ExposureAtFailure: recoveryExposureEvidence(operation, atFailure.exposure),
		ExposureAfter:     recoveryExposureEvidence(operation, after.exposure),
		ObjectAtFailure:   atFailure.object, ObjectAfter: after.object,
		SettlementAuditSequencesAtFailure: atFailure.settlementAuditSequences,
		SettlementAuditSequencesAfter:     after.settlementAuditSequences,
		AvailabilityAuditsAtFailure:       atFailure.availabilityAudits, AvailabilityAuditsAfter: after.availabilityAudits,
		TerminalAuditsAtFailure: atFailure.terminalAudits, TerminalAuditsAfter: after.terminalAudits,
		RegistrationAuditsAtFailure: atFailure.registrationAudits, RegistrationAuditsAfter: after.registrationAudits,
		TerminalAuditSequenceAtFailure: atFailure.terminalAuditSequence, TerminalAuditSequenceAfter: after.terminalAuditSequence,
		RegistrationAuditSequenceAtFailure: atFailure.registrationAuditSequence, RegistrationAuditSequenceAfter: after.registrationAuditSequence,
		AvailabilityAuditSequenceAtFailure: atFailure.availabilityAuditSequence, AvailabilityAuditSequenceAfter: after.availabilityAuditSequence,
	}
	return sample, nil
}

func recoveryExposureEvidence(operation experiment.AdapterOperation,
	exposure queryreceipt.ExposureEvidenceV1) experiment.RecoveryExposureSnapshot {
	return experiment.RecoveryExposureSnapshot{
		RootTaskIDHash: saltedTaskHash(operation, exposure.RootTaskID), ProfileVersion: exposure.ProfileVersion,
		ActualReleaseFacts: exposure.ActualReleaseFacts, ActualInfluenceFacts: exposure.ActualInfluenceFacts,
		ActualOutcomeFacts: exposure.ActualOutcomeFacts, ChargedReleaseFacts: exposure.ChargedReleaseFacts,
		ChargedInfluenceFacts: exposure.ChargedInfluenceFacts, ChargedOutcomeFacts: exposure.ChargedOutcomeFacts,
		ObservationSHA256: exposure.ObservationSHA256, DictionarySetSHA256: exposure.DictionarySetSHA256,
		ReleaseSetSHA256: exposure.ReleaseSetSHA256, InfluenceSetSHA256: exposure.InfluenceSetSHA256,
		OutcomeSetSHA256: exposure.OutcomeSetSHA256, RootEpoch: exposure.RootEpoch,
		PredicateProfileVersion: exposure.PredicateProfileVersion,
		PredicateContextSHA256:  exposure.PredicateContextSHA256, PredicateSetSHA256: exposure.PredicateSetSHA256,
		ActualPredicateAtomCount:  exposure.ActualPredicateAtomCount,
		ChargedPredicateAtomCount: exposure.ChargedPredicateAtomCount,
		CompositeOutcomeSHA256:    exposure.CompositeOutcomeSHA256,
		ActualCompositeCount:      exposure.ActualCompositeCount, ChargedCompositeCount: exposure.ChargedCompositeCount,
	}
}

func (adapter *realAdapter) installAvailabilityBlocker(ctx context.Context) error {
	statements := []string{`
CREATE OR REPLACE FUNCTION final_v5_pilot_block_available() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'PENDING' AND NEW.status = 'AVAILABLE' THEN
    RAISE EXCEPTION 'final V5 Pilot forced AVAILABLE failure';
  END IF;
  RETURN NEW;
END;
$$;`,
		`DROP TRIGGER IF EXISTS zz_final_v5_pilot_block_available ON result_artifacts`,
		`CREATE TRIGGER zz_final_v5_pilot_block_available
BEFORE UPDATE ON result_artifacts
FOR EACH ROW EXECUTE FUNCTION final_v5_pilot_block_available()`,
	}
	for _, statement := range statements {
		if _, err := adapter.control.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (adapter *realAdapter) removeAvailabilityBlocker(ctx context.Context) error {
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS zz_final_v5_pilot_block_available ON result_artifacts`,
		`DROP FUNCTION IF EXISTS final_v5_pilot_block_available()`,
	} {
		if _, err := adapter.control.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (adapter *realAdapter) recoverySnapshot(ctx context.Context, taskID, requestID string) (recoverySnapshot, error) {
	var snapshot recoverySnapshot
	tx, err := adapter.control.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback(context.Background())
	if err := tx.QueryRow(ctx, `
SELECT (SELECT count(*) FROM query_records WHERE task_id=t.id),b.used_queries
FROM tasks t JOIN budget_ledger b ON b.task_id=t.id WHERE t.id=$1`, taskID).
		Scan(&snapshot.queryRecords, &snapshot.usedQueries); err != nil {
		return snapshot, err
	}
	if requestID == "" {
		if err := tx.Commit(ctx); err != nil {
			return snapshot, err
		}
		return snapshot, nil
	}
	snapshot.root, err = loadRootLedgerSnapshot(ctx, tx, taskID)
	if err != nil {
		return snapshot, err
	}
	var queryID, rootTaskID, queryStatus, reservationStatus string
	var reservationRootEpoch int64
	var observationSHA256, observationDictionarySHA256 string
	var observationReleaseSHA256, observationDependencySHA256, observationOutcomeSHA256 string
	var observationPredicateContextSHA256, observationPredicateSetSHA256, observationCompositeSHA256 string
	var observationReleaseFacts, observationDependencyFacts, observationOutcomeFacts, observationPredicateFacts int64
	var objectKey, objectSHA256, resultID string
	var objectSize int64
	var receiptJSON []byte
	var persistedReceiptSHA256 string
	err = tx.QueryRow(ctx, `
SELECT q.id,t.root_task_id,q.status,r.status,r.root_epoch,r.observation_sha256,
       o.dictionary_set_digest,o.release_set_sha256,o.influence_set_sha256,o.outcome_set_sha256,
       o.predicate_context_sha256,o.predicate_set_sha256,o.composite_outcome_sha256,
       o.actual_release_facts,o.actual_influence_facts,o.actual_outcome_facts,o.predicate_atom_count,
       a.status,a.object_key,a.object_sha256,a.object_size,a.result_id,
       qr.receipt_json,qr.receipt_sha256,
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_V5_EXPOSURE_SETTLED'),
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type IN
         ('QUERY_COMPLETED','QUERY_BUDGET_RELEASED','QUERY_FAILED','QUERY_INDETERMINATE','QUERY_INTERRUPTED')),
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_OBJECT_REGISTERED'),
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_CONSUMED'),
       COALESCE((SELECT max(sequence) FROM audit_events e WHERE e.query_id=q.id AND e.event_type IN
         ('QUERY_COMPLETED','QUERY_BUDGET_RELEASED','QUERY_FAILED','QUERY_INDETERMINATE','QUERY_INTERRUPTED')),0),
       COALESCE((SELECT max(sequence) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_OBJECT_REGISTERED'),0),
       COALESCE((SELECT max(sequence) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_CONSUMED'),0)
FROM query_records q
JOIN result_artifacts a ON a.query_id=q.id
JOIN tasks t ON t.id=q.task_id
JOIN v5_query_exposure_reservations r ON r.query_id=q.id
JOIN v5_observations o ON o.observation_sha256=r.observation_sha256
JOIN query_receipts qr ON qr.query_id=q.id
WHERE q.task_id=$1 AND q.request_id=$2`, taskID, requestID).
		Scan(&queryID, &rootTaskID, &queryStatus, &reservationStatus, &reservationRootEpoch, &observationSHA256,
			&observationDictionarySHA256, &observationReleaseSHA256, &observationDependencySHA256, &observationOutcomeSHA256,
			&observationPredicateContextSHA256, &observationPredicateSetSHA256, &observationCompositeSHA256,
			&observationReleaseFacts, &observationDependencyFacts, &observationOutcomeFacts, &observationPredicateFacts,
			&snapshot.artifactStatus, &objectKey, &objectSHA256, &objectSize, &resultID,
			&receiptJSON, &persistedReceiptSHA256, &snapshot.settlements,
			&snapshot.terminalAudits, &snapshot.registrationAudits, &snapshot.availabilityAudits,
			&snapshot.terminalAuditSequence, &snapshot.registrationAuditSequence, &snapshot.availabilityAuditSequence)
	if err != nil {
		return snapshot, err
	}
	rows, err := tx.Query(ctx, `SELECT sequence FROM audit_events
WHERE query_id=$1 AND event_type='QUERY_V5_EXPOSURE_SETTLED' ORDER BY sequence`, queryID)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		var sequence int64
		if err := rows.Scan(&sequence); err != nil {
			rows.Close()
			return snapshot, err
		}
		snapshot.settlementAuditSequences = append(snapshot.settlementAuditSequences, sequence)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return snapshot, err
	}
	rows.Close()
	if shaBytes(receiptJSON) != persistedReceiptSHA256 || !validDigest(persistedReceiptSHA256) {
		return snapshot, errors.New("PENDING recovery receipt bytes differ from their Control digest")
	}
	var receipt queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(receiptJSON, &receipt); err != nil {
		return snapshot, errors.New("PENDING recovery receipt does not decode")
	}
	if err := requireVerifiedReceipt(adapter.verifier, receipt); err != nil {
		return snapshot, fmt.Errorf("PENDING recovery receipt: %w", err)
	}
	intent, exposure := receipt.ArtifactIntent, receipt.Exposure
	if receipt.TaskID != taskID || receipt.QueryID != queryID ||
		queryStatus != "COMPLETED" || reservationStatus != "SETTLED" ||
		exposure.RootTaskID != rootTaskID || exposure.RootEpoch != reservationRootEpoch ||
		exposure.ObservationSHA256 != observationSHA256 || exposure.DictionarySetSHA256 != observationDictionarySHA256 ||
		exposure.ReleaseSetSHA256 != observationReleaseSHA256 || exposure.InfluenceSetSHA256 != observationDependencySHA256 ||
		exposure.OutcomeSetSHA256 != observationOutcomeSHA256 || exposure.PredicateContextSHA256 != observationPredicateContextSHA256 ||
		exposure.PredicateSetSHA256 != observationPredicateSetSHA256 || exposure.CompositeOutcomeSHA256 != observationCompositeSHA256 ||
		exposure.ActualReleaseFacts != observationReleaseFacts || exposure.ActualInfluenceFacts != observationDependencyFacts ||
		exposure.ActualOutcomeFacts != observationOutcomeFacts || exposure.ActualPredicateAtomCount != observationPredicateFacts ||
		intent.ResultID != resultID || intent.ObjectKeySHA256 != sha(objectKey) || intent.ObjectSHA256 != objectSHA256 ||
		intent.ObjectSize != objectSize || snapshot.settlements != 1 || len(snapshot.settlementAuditSequences) != 1 ||
		snapshot.terminalAudits != 1 || snapshot.registrationAudits != 1 || snapshot.availabilityAudits < 0 || snapshot.availabilityAudits > 1 ||
		snapshot.terminalAuditSequence != receipt.AuditSequence || snapshot.registrationAuditSequence != intent.RegistrationAuditSequence ||
		(snapshot.availabilityAudits == 0 && snapshot.availabilityAuditSequence != 0) ||
		(snapshot.availabilityAudits == 1 && snapshot.availabilityAuditSequence <= snapshot.registrationAuditSequence) {
		return snapshot, errors.New("PENDING recovery Control projection differs from signed evidence")
	}
	snapshot.receiptSHA256 = receiptDigest(receipt)
	snapshot.intentSHA256 = intent.IntentSHA256
	snapshot.exposure = *exposure
	if err := tx.Commit(ctx); err != nil {
		return snapshot, err
	}
	snapshot.object, err = adapter.canonicalObjectSnapshot(ctx, objectKey, objectSize, objectSHA256, intent.IntentSHA256)
	if err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (adapter *realAdapter) businessSQLSnapshot(ctx context.Context) (experiment.BusinessSQLSnapshot, error) {
	const query = `WITH statements AS (
  SELECT s.calls::bigint AS calls,replace(lower(s.query),'"','') AS normalized_query
  FROM pg_stat_statements s
  WHERE s.dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
    AND s.userid=(SELECT oid FROM pg_roles WHERE rolname='gateway_reader')
)
SELECT COALESCE(sum(calls) FILTER (
         WHERE (position('reporting.expense_detail' in normalized_query)>0
                OR position('reporting.final_v5_attack_expense_detail' in normalized_query)>0
                OR position('reporting.final_v5_rls_unlimited_expense_detail' in normalized_query)>0
                OR position('reporting.final_v5_rls_bounded_expense_detail' in normalized_query)>0)
           AND position('taskgate_ordinal.expense_detail_v1' in normalized_query)=0),0)::bigint,
       COALESCE(sum(calls) FILTER (
         WHERE position('taskgate_ordinal.expense_detail_v1' in normalized_query)>0),0)::bigint,
       info.stats_reset,info.dealloc::bigint
FROM pg_stat_statements_info info
LEFT JOIN statements ON true
GROUP BY info.stats_reset,info.dealloc`
	var snapshot experiment.BusinessSQLSnapshot
	var reset time.Time
	if err := adapter.observer.QueryRow(ctx, query).Scan(
		&snapshot.VisibleCalls, &snapshot.CompanionCalls, &reset, &snapshot.Dealloc); err != nil {
		return snapshot, err
	}
	snapshot.StatsResetUnixMicro = reset.UTC().UnixMicro()
	return snapshot, nil
}

func (adapter *realAdapter) provisionTask(ctx context.Context, operation experiment.AdapterOperation) (string, error) {
	return adapter.provisionExpenseTask(ctx, "Final V5 real baseline pilot "+operation.PairID,
		[]string{"receipt_no", "department"})
}

func (adapter *realAdapter) waitTask(ctx context.Context, taskID, taskRole, expected string) error {
	started := time.Now()
	deadline := started.Add(adapter.timeout)
	lastState := "UNOBSERVED"
	var lastErr error
	polls := 0
	for time.Now().Before(deadline) {
		var status struct {
			State string `json:"state"`
		}
		polls++
		lastErr = adapter.alice.call(ctx, "get_task_status", map[string]string{"task_id": taskID}, &status)
		if lastErr == nil {
			lastState = status.State
		}
		if lastErr == nil && status.State == expected {
			recordTaskMigrationWait(ctx, taskID, taskRole, expected, lastState, "reached", time.Since(started), polls, nil)
			return nil
		}
		select {
		case <-ctx.Done():
			elapsed := time.Since(started)
			recordTaskMigrationWait(ctx, taskID, taskRole, expected, lastState, "context_cancelled", elapsed, polls, ctx.Err())
			return &taskMigrationWaitError{expected: expected, lastState: lastState, elapsed: elapsed,
				polls: polls, lastErr: ctx.Err(), status: "context_cancelled"}
		case <-time.After(100 * time.Millisecond):
		}
	}
	elapsed := time.Since(started)
	recordTaskMigrationWait(ctx, taskID, taskRole, expected, lastState, "timeout", elapsed, polls, lastErr)
	return &taskMigrationWaitError{expected: expected, lastState: lastState, elapsed: elapsed,
		polls: polls, lastErr: lastErr, status: "timed out"}
}

func (adapter *realAdapter) rootLedgerSnapshot(ctx context.Context, taskID string) (experiment.RootLedgerSnapshot, error) {
	var snapshot experiment.RootLedgerSnapshot
	tx, err := adapter.control.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback(context.Background())
	snapshot, err = loadRootLedgerSnapshot(ctx, tx, taskID)
	if err != nil {
		return snapshot, err
	}
	if err := tx.Commit(ctx); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func loadRootLedgerSnapshot(ctx context.Context, tx pgx.Tx, taskID string) (experiment.RootLedgerSnapshot, error) {
	var snapshot experiment.RootLedgerSnapshot
	var usedRelease, usedDependency, usedOutcome int64
	err := tx.QueryRow(ctx, `
SELECT h.epoch,COALESCE(h.dictionary_set_digest,''),
       COALESCE(h.release_set_sha256,''),h.used_release_facts,
       COALESCE(r.static_cardinality+r.dynamic_cardinality,0),
       COALESCE(h.influence_set_sha256,''),h.used_influence_facts,
       COALESCE(d.static_cardinality+d.dynamic_cardinality,0),
       COALESCE(h.outcome_set_sha256,''),h.used_outcome_facts,
       COALESCE(o.cardinality,0)
FROM tasks t
JOIN v5_exposure_root_heads h ON h.root_task_id=t.root_task_id
LEFT JOIN v4_bitmap_sets r ON r.set_sha256=h.release_set_sha256
LEFT JOIN v4_bitmap_sets d ON d.set_sha256=h.influence_set_sha256
LEFT JOIN v5_outcome_hash_sets o ON o.set_sha256=h.outcome_set_sha256
WHERE t.id=$1`, taskID).Scan(
		&snapshot.Epoch, &snapshot.DictionarySetSHA256,
		&snapshot.ReleaseSetSHA256, &usedRelease, &snapshot.ReleaseCardinality,
		&snapshot.DependencySetSHA256, &usedDependency, &snapshot.DependencyCardinality,
		&snapshot.OutcomeSetSHA256, &usedOutcome, &snapshot.OutcomeCardinality)
	if err != nil {
		return snapshot, err
	}
	if usedRelease != snapshot.ReleaseCardinality || usedDependency != snapshot.DependencyCardinality || usedOutcome != snapshot.OutcomeCardinality {
		return snapshot, errors.New("root head cardinality differs from its immutable component set")
	}
	rows, err := tx.Query(ctx, `
SELECT ro.observation_sha256
FROM tasks t
JOIN v5_root_observations ro ON ro.root_task_id=t.root_task_id
WHERE t.id=$1
ORDER BY ro.observation_sha256`, taskID)
	if err != nil {
		return snapshot, err
	}
	var observations []string
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			rows.Close()
			return snapshot, err
		}
		observations = append(observations, digest)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return snapshot, err
	}
	rows.Close()
	snapshot.RootObservationCount = int64(len(observations))
	snapshot.RootObservationSetSHA256 = observationSetDigest(observations)
	if err := validateRootLedgerSnapshot(snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func validateRootLedgerSnapshot(snapshot experiment.RootLedgerSnapshot) error {
	if snapshot.Epoch == 0 {
		if snapshot.DictionarySetSHA256 != "" || snapshot.ReleaseSetSHA256 != "" || snapshot.DependencySetSHA256 != "" || snapshot.OutcomeSetSHA256 != "" ||
			snapshot.ReleaseCardinality != 0 || snapshot.DependencyCardinality != 0 || snapshot.OutcomeCardinality != 0 || snapshot.RootObservationCount != 0 {
			return errors.New("fresh root head contains committed exposure")
		}
	} else if !validDigest(snapshot.DictionarySetSHA256) || !validDigest(snapshot.ReleaseSetSHA256) ||
		!validDigest(snapshot.DependencySetSHA256) || !validDigest(snapshot.OutcomeSetSHA256) {
		return errors.New("committed root head contains an invalid digest")
	}
	if snapshot.RootObservationCount < 0 || !validDigest(snapshot.RootObservationSetSHA256) {
		return errors.New("root observation set evidence is invalid")
	}
	return nil
}

func observationSetDigest(values []string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-ROOT-OBSERVATION-SET-V1\x00"))
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func rootSetDigest(snapshot experiment.RootLedgerSnapshot) string {
	return sha(strings.Join([]string{snapshot.ReleaseSetSHA256, snapshot.DependencySetSHA256, snapshot.OutcomeSetSHA256}, "\x00"))
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func (adapter *realAdapter) idempotentControlSnapshot(ctx context.Context, operation experiment.AdapterOperation,
	taskID, requestID string) (experiment.IdempotentControlSnapshot, error) {
	var snapshot experiment.IdempotentControlSnapshot
	tx, err := adapter.control.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback(context.Background())
	snapshot.Root, err = loadRootLedgerSnapshot(ctx, tx, taskID)
	if err != nil {
		return snapshot, err
	}
	err = tx.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM query_records q WHERE q.task_id=t.id),
  (SELECT count(*) FROM v5_query_exposure_reservations r WHERE r.task_id=t.id AND r.status='SETTLED'),
  (SELECT count(*) FROM v5_query_observations o JOIN query_records q ON q.id=o.query_id WHERE q.task_id=t.id),
  (SELECT count(*) FROM query_receipts r JOIN query_records q ON q.id=r.query_id WHERE q.task_id=t.id),
  (SELECT count(*) FROM result_artifacts a WHERE a.task_id=t.id),
  (SELECT count(*) FROM result_artifacts a WHERE a.task_id=t.id AND a.status='AVAILABLE'),
  (SELECT count(*) FROM audit_events e WHERE e.task_id=t.id AND e.event_type IN
    ('QUERY_COMPLETED','QUERY_BUDGET_RELEASED','QUERY_FAILED','QUERY_INDETERMINATE','QUERY_INTERRUPTED')),
  (SELECT count(*) FROM audit_events e WHERE e.task_id=t.id AND e.event_type='QUERY_RESULT_OBJECT_REGISTERED'),
  (SELECT count(*) FROM audit_events e WHERE e.task_id=t.id AND e.event_type='QUERY_RESULT_CONSUMED')
FROM tasks t WHERE t.id=$1`, taskID).Scan(
		&snapshot.QueryRecords, &snapshot.ExposureCharges, &snapshot.Observations,
		&snapshot.Receipts, &snapshot.Artifacts, &snapshot.AvailableArtifacts,
		&snapshot.TerminalAudits, &snapshot.RegistrationAudits, &snapshot.AvailabilityAudits)
	if err != nil {
		return snapshot, err
	}
	raw, err := adapter.loadTerminalIdentity(ctx, tx, taskID, requestID)
	if err != nil {
		return snapshot, err
	}
	if err := tx.Commit(ctx); err != nil {
		return snapshot, err
	}
	object, err := adapter.canonicalObjectSnapshot(ctx, raw.objectKey, raw.objectSize, raw.objectSHA256, raw.intentSHA256)
	if err != nil {
		return snapshot, err
	}
	snapshot.CanonicalObjects, err = adapter.canonicalObjectCount(ctx)
	if err != nil {
		return snapshot, err
	}
	snapshot.Target = adapter.terminalIdentityEvidence(operation, raw, object)
	return snapshot, nil
}

func (adapter *realAdapter) loadTerminalIdentity(ctx context.Context, tx pgx.Tx, taskID, requestID string) (rawTerminalIdentity, error) {
	var raw rawTerminalIdentity
	var receiptJSON []byte
	var persistedReceiptSHA256, queryObservationSHA256 string
	var persistedTerminalHash string
	var queryObservationEpoch, persistedTerminalSequence int64
	err := tx.QueryRow(ctx, `
SELECT q.id,q.task_id,t.root_task_id,q.request_id,q.status,q.request_digest,q.grant_digest,q.result_sha256,
       r.status,r.observation_sha256,r.root_epoch,qo.observation_sha256,qo.root_epoch,
       qr.receipt_sha256,qr.receipt_json,qr.terminal_audit_sequence,qr.terminal_audit_hash,
       a.result_id,a.object_key,a.status,a.object_sha256,a.parquet_sha256,a.object_size,a.parquet_size,
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_V5_EXPOSURE_SETTLED'),
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type IN
         ('QUERY_COMPLETED','QUERY_BUDGET_RELEASED','QUERY_FAILED','QUERY_INDETERMINATE','QUERY_INTERRUPTED')),
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_OBJECT_REGISTERED'),
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_CONSUMED'),
       COALESCE((SELECT max(sequence) FROM audit_events e WHERE e.query_id=q.id AND e.event_type IN
         ('QUERY_COMPLETED','QUERY_BUDGET_RELEASED','QUERY_FAILED','QUERY_INDETERMINATE','QUERY_INTERRUPTED')),0),
       COALESCE((SELECT max(sequence) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_OBJECT_REGISTERED'),0),
       COALESCE((SELECT max(sequence) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_CONSUMED'),0),
       COALESCE((SELECT max(current_hash) FROM audit_events e WHERE e.query_id=q.id AND e.event_type IN
         ('QUERY_COMPLETED','QUERY_BUDGET_RELEASED','QUERY_FAILED','QUERY_INDETERMINATE','QUERY_INTERRUPTED')),''),
       COALESCE((SELECT max(current_hash) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_OBJECT_REGISTERED'),''),
       COALESCE((SELECT max(current_hash) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_CONSUMED'),'')
FROM query_records q
JOIN tasks t ON t.id=q.task_id
JOIN v5_query_exposure_reservations r ON r.query_id=q.id
JOIN v5_query_observations qo ON qo.query_id=q.id
JOIN query_receipts qr ON qr.query_id=q.id
JOIN result_artifacts a ON a.query_id=q.id
WHERE q.task_id=$1 AND q.request_id=$2`, taskID, requestID).Scan(
		&raw.queryID, &raw.taskID, &raw.rootTaskID, &raw.requestID, &raw.queryStatus,
		&raw.requestDigest, &raw.grantDigest, &raw.resultSHA256,
		&raw.reservationStatus, &raw.observationSHA256, &raw.rootEpoch,
		&queryObservationSHA256, &queryObservationEpoch,
		&persistedReceiptSHA256, &receiptJSON, &persistedTerminalSequence, &persistedTerminalHash,
		&raw.resultID, &raw.objectKey, &raw.artifactStatus, &raw.objectSHA256, &raw.parquetSHA256,
		&raw.objectSize, &raw.parquetSize,
		&raw.settlementAudits, &raw.terminalAudits, &raw.registrationAudits, &raw.availabilityAudits,
		&raw.terminalSequence, &raw.registrationSequence, &raw.availabilitySequence,
		&raw.terminalHash, &raw.registrationHash, &raw.availabilityHash)
	if err != nil {
		return raw, err
	}
	if shaBytes(receiptJSON) != persistedReceiptSHA256 || !validDigest(persistedReceiptSHA256) {
		return raw, errors.New("persisted receipt bytes differ from their Control digest")
	}
	if err := json.Unmarshal(receiptJSON, &raw.receipt); err != nil {
		return raw, err
	}
	if err := requireVerifiedReceipt(adapter.verifier, raw.receipt); err != nil {
		return raw, fmt.Errorf("persisted receipt: %w", err)
	}
	intent, exposure := raw.receipt.ArtifactIntent, raw.receipt.Exposure
	if raw.queryStatus != "COMPLETED" || raw.reservationStatus != "SETTLED" || raw.artifactStatus != "AVAILABLE" ||
		raw.receipt.TaskID != raw.taskID || raw.receipt.QueryID != raw.queryID || raw.receipt.RequestID != raw.requestID ||
		intent.ResultID != raw.resultID || intent.ObjectKeySHA256 != sha(raw.objectKey) ||
		intent.ObjectSHA256 != raw.objectSHA256 || intent.ParquetSHA256 != raw.parquetSHA256 ||
		intent.ObjectSize != raw.objectSize || intent.ParquetSize != raw.parquetSize ||
		exposure.RootTaskID != raw.rootTaskID || exposure.ObservationSHA256 != raw.observationSHA256 ||
		queryObservationSHA256 != raw.observationSHA256 || queryObservationEpoch != raw.rootEpoch {
		return raw, errors.New("terminal query identity is internally inconsistent")
	}
	if raw.settlementAudits != 1 || raw.terminalAudits != 1 || raw.registrationAudits != 1 || raw.availabilityAudits != 1 ||
		persistedTerminalSequence != raw.terminalSequence || persistedTerminalHash != raw.terminalHash ||
		raw.terminalSequence != raw.receipt.AuditSequence || raw.terminalHash != raw.receipt.AuditHash ||
		raw.registrationSequence != intent.RegistrationAuditSequence || raw.registrationHash != intent.RegistrationAuditHash ||
		raw.availabilitySequence <= raw.registrationSequence {
		return raw, errors.New("terminal query audit identity is incomplete")
	}
	raw.receiptID = raw.receipt.ReceiptID
	raw.receiptSHA256 = persistedReceiptSHA256
	raw.receiptIdentitySHA256 = receiptDigest(raw.receipt)
	raw.intentSHA256 = intent.IntentSHA256
	return raw, nil
}

func (adapter *realAdapter) canonicalObjectSnapshot(ctx context.Context, objectKey string, expectedSize int64,
	expectedSHA256, intentSHA256 string) (experiment.CanonicalObjectSnapshot, error) {
	var snapshot experiment.CanonicalObjectSnapshot
	if objectKey == "" || expectedSize <= 0 || !validDigest(expectedSHA256) || !validDigest(intentSHA256) {
		return snapshot, errors.New("canonical object projection is incomplete")
	}
	stat, err := adapter.objectStore.StatObject(ctx, adapter.objectBucket, objectKey, minio.StatObjectOptions{})
	if err != nil || stat.Size != expectedSize {
		return snapshot, errors.New("canonical object Stat differs from Control")
	}
	object, err := adapter.objectStore.GetObject(ctx, adapter.objectBucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return snapshot, err
	}
	defer object.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(object, expectedSize+1))
	if err != nil || written != expectedSize {
		return snapshot, errors.New("canonical object stream size differs from Control")
	}
	actualSHA256 := hex.EncodeToString(hash.Sum(nil))
	if actualSHA256 != expectedSHA256 {
		return snapshot, errors.New("canonical object stream digest differs from Control")
	}
	return experiment.CanonicalObjectSnapshot{
		Exists: true, ObjectKeySHA256: sha(objectKey), CanonicalCiphertextSHA256: actualSHA256,
		CanonicalCiphertextSize: written, IntentSHA256: intentSHA256,
	}, nil
}

func (adapter *realAdapter) canonicalObjectCount(ctx context.Context) (int64, error) {
	var count int64
	for object := range adapter.objectStore.ListObjects(ctx, adapter.objectBucket, minio.ListObjectsOptions{Prefix: "results/", Recursive: true}) {
		if object.Err != nil {
			return 0, object.Err
		}
		count++
	}
	return count, nil
}

func (adapter *realAdapter) terminalIdentityEvidence(operation experiment.AdapterOperation, raw rawTerminalIdentity,
	object experiment.CanonicalObjectSnapshot) experiment.TerminalIdentitySnapshot {
	return experiment.TerminalIdentitySnapshot{
		Found: true, QueryIDHash: saltedIdentityHash(operation, "query", raw.queryID),
		ResultIDHash:  saltedIdentityHash(operation, "result", raw.resultID),
		ReceiptSHA256: raw.receiptIdentitySHA256, IntentSHA256: raw.intentSHA256,
		ObjectKeySHA256: object.ObjectKeySHA256, CommittedObjectSHA256: raw.objectSHA256,
		CanonicalCiphertextSHA256: object.CanonicalCiphertextSHA256,
		CanonicalCiphertextSize:   object.CanonicalCiphertextSize,
		ArtifactStatus:            raw.artifactStatus, ObservationSHA256: raw.observationSHA256,
	}
}

func responseTerminalIdentityEvidence(operation experiment.AdapterOperation, response queryResponse,
	manifest *experiment.RedactedVerifierManifest) experiment.TerminalIdentitySnapshot {
	if response.Receipt.ArtifactIntent == nil || manifest == nil {
		return experiment.TerminalIdentitySnapshot{}
	}
	intent := response.Receipt.ArtifactIntent
	return experiment.TerminalIdentitySnapshot{
		Found: true, QueryIDHash: saltedIdentityHash(operation, "query", response.QueryID),
		ResultIDHash:  saltedIdentityHash(operation, "result", response.ResultID),
		ReceiptSHA256: receiptDigest(response.Receipt), IntentSHA256: intent.IntentSHA256,
		ObjectKeySHA256: intent.ObjectKeySHA256, CommittedObjectSHA256: intent.ObjectSHA256,
		CanonicalCiphertextSHA256: manifest.CanonicalCiphertextSHA256,
		CanonicalCiphertextSize:   manifest.CanonicalCiphertextSize,
		ArtifactStatus:            response.ArtifactStatus, ObservationSHA256: response.Exposure.ObservationSHA256,
	}
}

func saltedIdentityHash(operation experiment.AdapterOperation, kind, value string) string {
	return sha("TASKGATE-FINAL-V5-PILOT-IDENTITY-V1\x00" + operation.CampaignID + "\x00" + operation.DeploymentID + "\x00" + kind + "\x00" + value)
}

func (adapter *realAdapter) crossBindingVerification(ctx context.Context, operation experiment.AdapterOperation,
	state *pairState, plan baselinePlan) (experiment.CrossBindingVerificationEvidence, error) {
	var evidence experiment.CrossBindingVerificationEvidence
	if state.taskID == "" || state.novelRequestID == "" || state.novelQueryID == "" {
		return evidence, errors.New("cross-binding check lacks its novel anchor")
	}
	first, err := adapter.crossBindingSnapshot(ctx, state.taskID, state.novelRequestID)
	if err != nil {
		return evidence, err
	}
	secondTaskID, err := plan.provision(ctx, operation)
	if err != nil {
		return evidence, err
	}
	beforeRoot, err := adapter.rootLedgerSnapshot(ctx, secondTaskID)
	if err != nil {
		return evidence, err
	}
	businessBefore, err := plan.snapshot(ctx)
	if err != nil {
		return evidence, err
	}
	requestID := "final-v5-" + sha(operation.SampleID)[:20] + "-cross-binding"
	started := time.Now()
	response, err := adapter.callGovernedArm(ctx, plan, secondTaskID, requestID, plan.taskGateSQL)
	if err != nil {
		return evidence, err
	}
	availableMS := durationMS(time.Since(started))
	businessAfter, err := plan.snapshot(ctx)
	if err != nil {
		return evidence, err
	}
	afterRoot, err := adapter.rootLedgerSnapshot(ctx, secondTaskID)
	if err != nil {
		return evidence, err
	}
	if response.SemanticReplay || response.IdempotentReplay {
		return evidence, errors.New("cross-binding query reused an authority-bound materialization")
	}
	secondState := &pairState{taskID: secondTaskID}
	crossOperation := operation
	crossOperation.Mode = "novel"
	verifiedSample, err := adapter.completeTaskgateSample(ctx, crossOperation, secondState, beforeRoot, afterRoot,
		started, availableMS, plan.taskGateSQL, response)
	if err != nil {
		return evidence, err
	}
	second, err := adapter.crossBindingSnapshot(ctx, secondTaskID, requestID)
	if err != nil {
		return evidence, err
	}
	visibleCallsDelta := businessAfter.VisibleCalls - businessBefore.VisibleCalls
	companionCallsDelta := businessAfter.CompanionCalls - businessBefore.CompanionCalls
	// Each condition is named. The combined check used to report one sentence
	// for a dozen distinct failures, which is the same problem as swallowing an
	// error: a run could see that cross-binding evidence was incomplete and
	// never learn which part of it was.
	for _, incomplete := range []struct {
		failed bool
		reason string
	}{
		{first.taskID == second.taskID, "the two tasks share an identity"},
		{first.rootTaskID == second.rootTaskID, "the two tasks share an exposure root"},
		{first.queryID == second.queryID, "the two queries share an identity"},
		{first.grantDigest == second.grantDigest, "the two tasks share a signed grant"},
		{first.cacheKeySHA256 == second.cacheKeySHA256, "the two queries share a semantic cache key"},
		{second.sourceQueryID != second.queryID, "the second query did not originate itself"},
		{second.rootFirstQueryID != second.queryID, "the second query is not its root's first"},
		{!plan.semanticView && visibleCallsDelta != 1,
			fmt.Sprintf("visible Business SQL calls moved by %d, want 1",
				visibleCallsDelta)},
		{!plan.semanticView && companionCallsDelta != 1,
			fmt.Sprintf("ordinal companion calls moved by %d, want 1",
				companionCallsDelta)},
		{plan.semanticView && visibleCallsDelta+companionCallsDelta != 1,
			fmt.Sprintf("semantic View Business SQL calls moved by visible=%d companion=%d, want combined 1",
				visibleCallsDelta, companionCallsDelta)},
	} {
		if incomplete.failed {
			// Print what the Observer actually saw. A call-count assertion that
			// fails without showing the statements behind it forces the reader
			// to guess which relation names the executed SQL carried.
			adapter.reportObservedStatements(ctx, plan)
			return evidence, fmt.Errorf("cross-binding negative evidence is incomplete: %s", incomplete.reason)
		}
	}
	if verifiedSample.BaselineVerification == nil || verifiedSample.BaselineVerification.VerifierManifest == nil {
		return evidence, errors.New("cross-binding released-artifact manifest is absent")
	}
	evidence = experiment.CrossBindingVerificationEvidence{
		FirstTaskIDHash: saltedTaskHash(operation, first.taskID), SecondTaskIDHash: saltedTaskHash(operation, second.taskID),
		FirstRootTaskIDHash: saltedTaskHash(operation, first.rootTaskID), SecondRootTaskIDHash: saltedTaskHash(operation, second.rootTaskID),
		FirstQueryIDHash: saltedIdentityHash(operation, "query", first.queryID), SecondQueryIDHash: saltedIdentityHash(operation, "query", second.queryID),
		FirstGrantSHA256: first.grantDigest, SecondGrantSHA256: second.grantDigest,
		FirstCacheKeySHA256: first.cacheKeySHA256, SecondCacheKeySHA256: second.cacheKeySHA256,
		FirstSQLFingerprintSHA256: first.sqlFingerprint, SecondSQLFingerprintSHA256: second.sqlFingerprint,
		FirstCatalogSHA256: first.catalogDigest, SecondCatalogSHA256: second.catalogDigest,
		FirstSchemaSHA256: first.schemaDigest, SecondSchemaSHA256: second.schemaDigest,
		FirstDatasourceIDHash:  saltedIdentityHash(operation, "datasource", first.datasourceID),
		SecondDatasourceIDHash: saltedIdentityHash(operation, "datasource", second.datasourceID),
		FirstObservationSHA256: first.observationSHA256, SecondObservationSHA256: second.observationSHA256,
		FirstObservationBindingSHA256:  observationBindingDigest(saltedTaskHash(operation, first.rootTaskID), first.observationSHA256),
		SecondObservationBindingSHA256: observationBindingDigest(saltedTaskHash(operation, second.rootTaskID), second.observationSHA256),
		FirstSourceQueryIDHash:         saltedIdentityHash(operation, "query", first.sourceQueryID),
		SecondSourceQueryIDHash:        saltedIdentityHash(operation, "query", second.sourceQueryID),
		SecondRootFirstQueryIDHash:     saltedIdentityHash(operation, "query", second.rootFirstQueryID),
		BusinessBefore:                 businessBefore, BusinessAfter: businessAfter,
		SemanticReplayAudits: second.semanticReplayAudits, SettlementAudits: second.settlementAudits,
		SemanticReplay: response.SemanticReplay, IdempotentReplay: response.IdempotentReplay,
		VerifierManifest: verifiedSample.BaselineVerification.VerifierManifest,
	}
	return evidence, nil
}

func (adapter *realAdapter) crossBindingSnapshot(ctx context.Context, taskID, requestID string) (rawCrossBinding, error) {
	var snapshot rawCrossBinding
	err := adapter.control.QueryRow(ctx, `
SELECT q.task_id,t.root_task_id,q.id,q.grant_digest,m.cache_key_sha256,r.observation_sha256,
       q.sql_fingerprint,q.catalog_digest,q.schema_digest,q.datasource_id,
       m.source_query_id,ro.first_query_id,
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_V5_SEMANTIC_REPLAY'),
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_V5_EXPOSURE_SETTLED')
FROM query_records q
JOIN tasks t ON t.id=q.task_id
JOIN v5_query_exposure_reservations r ON r.query_id=q.id AND r.status='SETTLED'
JOIN v5_committed_materializations m ON m.task_id=q.task_id AND m.source_query_id=q.id
JOIN v5_root_observations ro ON ro.root_task_id=t.root_task_id AND ro.observation_sha256=r.observation_sha256
WHERE q.task_id=$1 AND q.request_id=$2`, taskID, requestID).Scan(
		&snapshot.taskID, &snapshot.rootTaskID, &snapshot.queryID, &snapshot.grantDigest,
		&snapshot.cacheKeySHA256, &snapshot.observationSHA256,
		&snapshot.sqlFingerprint, &snapshot.catalogDigest, &snapshot.schemaDigest, &snapshot.datasourceID,
		&snapshot.sourceQueryID,
		&snapshot.rootFirstQueryID, &snapshot.semanticReplayAudits, &snapshot.settlementAudits)
	if err != nil {
		return snapshot, err
	}
	if !validDigest(snapshot.grantDigest) || !validDigest(snapshot.cacheKeySHA256) || !validDigest(snapshot.sqlFingerprint) ||
		!validDigest(snapshot.catalogDigest) || !validDigest(snapshot.schemaDigest) || strings.TrimSpace(snapshot.datasourceID) == "" ||
		!validDigest(snapshot.observationSHA256) || snapshot.sourceQueryID != snapshot.queryID ||
		snapshot.rootFirstQueryID != snapshot.queryID || snapshot.semanticReplayAudits != 0 || snapshot.settlementAudits != 1 {
		return snapshot, errors.New("novel cross-binding anchor is invalid")
	}
	return snapshot, nil
}

func observationBindingDigest(rootTaskIDHash, observationSHA256 string) string {
	return sha("TASKGATE-FINAL-V5-ROOT-OBSERVATION-BINDING-V1\x00" + rootTaskIDHash + "\x00" + observationSHA256)
}

func parseParquet(value []byte, resultID string, expectedRows int64) ([][]any, error) {
	reader := parquet.NewReader(bytes.NewReader(value))
	defer reader.Close()
	storedSchema, ok := reader.File().Lookup("taskgate.schema")
	if !ok {
		return nil, errors.New("Parquet omitted TaskGate schema")
	}
	var schema []resultartifact.ColumnSchema
	if err := json.Unmarshal([]byte(storedSchema), &schema); err != nil {
		return nil, err
	}
	if reader.NumRows() != expectedRows {
		return nil, errors.New("Parquet row count mismatch")
	}
	limit := expectedRows
	if limit < 1 {
		limit = 1
	}
	return resultartifact.ReadParquet(value, resultID, schema, 0, limit)
}

// validateResponseAgainstVerifiedReceipt prevents mutable response metadata
// from drifting away from the signed evidence that the campaign records. It
// is called only after VerifyReleasedArtifact has authenticated the receipt
// and compared it with the independent Control projection.
func validateResponseAgainstVerifiedReceipt(response queryResponse) error {
	intent := response.Receipt.ArtifactIntent
	exposure := response.Receipt.Exposure
	if intent == nil || exposure == nil {
		return errors.New("verified response omitted artifact intent or exposure evidence")
	}
	stringsToCompare := []struct {
		name, actual, expected string
	}{
		{"task ID", response.TaskID, response.Receipt.TaskID},
		{"query ID", response.QueryID, response.Receipt.QueryID},
		{"result ID", response.ResultID, intent.ResultID},
		{"exposure query ID", response.Exposure.QueryID, response.Receipt.QueryID},
		{"root task ID", response.Exposure.RootTaskID, exposure.RootTaskID},
		{"exposure profile", response.Exposure.ProfileVersion, exposure.ProfileVersion},
		{"observation digest", response.Exposure.ObservationSHA256, exposure.ObservationSHA256},
		{"dictionary-set digest", response.Exposure.DictionarySetDigest, exposure.DictionarySetSHA256},
		{"Result-set digest", response.Exposure.ReleaseSetSHA256, exposure.ReleaseSetSHA256},
		{"Dependency-set digest", response.Exposure.InfluenceSetSHA256, exposure.InfluenceSetSHA256},
		{"Outcome-set digest", response.Exposure.OutcomeSetSHA256, exposure.OutcomeSetSHA256},
		{"predicate-context digest", response.Exposure.PredicateContextSHA256, exposure.PredicateContextSHA256},
		{"predicate-set digest", response.Exposure.PredicateSetSHA256, exposure.PredicateSetSHA256},
		{"composite-outcome digest", response.Exposure.CompositeOutcomeSHA256, exposure.CompositeOutcomeSHA256},
	}
	for _, comparison := range stringsToCompare {
		if comparison.actual == "" || comparison.actual != comparison.expected {
			return fmt.Errorf("query response %s differs from verified receipt", comparison.name)
		}
	}
	countsToCompare := []struct {
		name             string
		actual, expected int64
	}{
		{"row count", response.RowCount, intent.RowCount},
		{"column count", int64(response.ColumnCount), intent.ColumnCount},
		{"actual Result count", response.Exposure.ActualReleaseFacts, exposure.ActualReleaseFacts},
		{"actual Dependency count", response.Exposure.ActualInfluenceFacts, exposure.ActualInfluenceFacts},
		{"actual Outcome count", response.Exposure.ActualOutcomeFacts, exposure.ActualOutcomeFacts},
		{"charged Result count", response.Exposure.ChargedReleaseFacts, exposure.ChargedReleaseFacts},
		{"charged Dependency count", response.Exposure.ChargedInfluenceFacts, exposure.ChargedInfluenceFacts},
		{"charged Outcome count", response.Exposure.ChargedOutcomeFacts, exposure.ChargedOutcomeFacts},
		{"actual predicate count", response.Exposure.ActualPredicateAtomCount, exposure.ActualPredicateAtomCount},
		{"charged predicate count", response.Exposure.ChargedPredicateAtomCount, exposure.ChargedPredicateAtomCount},
		{"root epoch", response.Exposure.RootEpoch, exposure.RootEpoch},
	}
	for _, comparison := range countsToCompare {
		if comparison.actual != comparison.expected {
			return fmt.Errorf("query response %s differs from verified receipt", comparison.name)
		}
	}
	return nil
}

func (adapter *realAdapter) openCanonicalObject(ctx context.Context, response queryResponse) (releasedartifact.ExpectedBinding, releasedartifact.CanonicalObjectEvidence, io.ReadCloser, error) {
	var binding releasedartifact.ExpectedBinding
	var object releasedartifact.CanonicalObjectEvidence
	var expiresAt pgtype.Timestamptz
	err := adapter.control.QueryRow(ctx, `
SELECT q.task_id,q.id,a.result_id,
       q.manifest_digest,q.grant_digest,q.catalog_digest,q.catalog_version,q.datasource_id,q.schema_digest,
       reservation.root_task_id,reservation.profile_version,head.predicate_profile_version,
       reservation.observation_sha256,observation.dictionary_set_digest,
       observation.release_set_sha256,observation.influence_set_sha256,observation.outcome_set_sha256,
       reservation.predicate_context_sha256,reservation.predicate_set_sha256,reservation.composite_outcome_sha256,
       reservation.actual_release_facts,reservation.actual_influence_facts,reservation.actual_outcome_facts,
       reservation.charged_release_facts,reservation.charged_influence_facts,reservation.charged_outcome_facts,
       reservation.actual_predicate_atom_count,reservation.charged_predicate_atom_count,reservation.root_epoch,
       a.result_id,a.query_id,a.task_id,a.key_id,a.format,a.encryption,a.staging_key,a.object_key,
       a.parquet_sha256,a.object_sha256,a.parquet_size,a.object_size,a.row_count,a.column_count,
       a.schema_json,a.result_metadata_json,a.acl_json,a.expires_at,a.status
FROM query_records q
JOIN v5_query_exposure_reservations reservation ON reservation.query_id=q.id AND reservation.status='SETTLED'
JOIN v5_observations observation ON observation.observation_sha256=reservation.observation_sha256
JOIN v5_exposure_root_heads head ON head.root_task_id=reservation.root_task_id
JOIN result_artifacts a ON a.query_id=q.id
WHERE q.id=$1 AND q.task_id=$2 AND a.result_id=$3`,
		response.QueryID, response.TaskID, response.ResultID).Scan(
		&binding.TaskID, &binding.QueryID, &binding.ResultID,
		&binding.ManifestDigest, &binding.GrantDigest, &binding.CatalogDigest, &binding.CatalogVersion,
		&binding.DatasourceID, &binding.SchemaDigest, &binding.RootTaskID, &binding.ProfileVersion,
		&binding.PredicateProfileVersion, &binding.ObservationSHA256, &binding.DictionarySetSHA256,
		&binding.ReleaseSetSHA256, &binding.InfluenceSetSHA256, &binding.OutcomeSetSHA256,
		&binding.PredicateContextSHA256, &binding.PredicateSetSHA256, &binding.CompositeOutcomeSHA256,
		&binding.ActualReleaseFacts, &binding.ActualInfluenceFacts, &binding.ActualOutcomeFacts,
		&binding.ChargedReleaseFacts, &binding.ChargedInfluenceFacts, &binding.ChargedOutcomeFacts,
		&binding.ActualPredicateAtomCount, &binding.ChargedPredicateAtomCount, &binding.RootEpoch,
		&object.ResultID, &object.QueryID, &object.TaskID, &object.KeyID, &object.Format, &object.Encryption,
		&object.StagingKey, &object.ObjectKey, &object.ParquetSHA256, &object.ObjectSHA256,
		&object.ParquetSize, &object.ObjectSize, &object.RowCount, &object.ColumnCount,
		&object.SchemaJSON, &object.ResultMetadataJSON, &object.ACLJSON, &expiresAt, &object.Status)
	if err != nil {
		return binding, object, nil, err
	}
	if expiresAt.Valid {
		value := expiresAt.Time.UTC()
		object.ExpiresAt = &value
	}
	canonical, err := adapter.objectStore.GetObject(ctx, adapter.objectBucket, object.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		return binding, object, nil, err
	}
	object.Ciphertext = canonical
	return binding, object, canonical, nil
}

func baseSample(operation experiment.AdapterOperation, system string) experiment.Sample {
	return experiment.Sample{SchemaVersion: 1, CampaignID: operation.CampaignID, DeploymentID: operation.DeploymentID, ExperimentID: operation.ExperimentID, CellID: operation.CellID, SampleID: operation.SampleID, Iteration: operation.Iteration, ProcessReplicate: operation.ProcessReplicate, Warmup: operation.Warmup, OrderPosition: operation.OrderPosition, RandomSeed: operation.RandomSeed, PairID: operation.PairID, PairedSystemOrder: operation.PairedSystemOrder, RootGroupID: operation.RootGroupID, System: system, Mode: operation.Mode, WorkloadID: operation.WorkloadID, Scale: operation.Scale, PipelineMS: zeroPipeline(), DiagnosticMS: map[string]float64{}, Status: "invalid", PublicationEligible: operation.CampaignClass == "publication", KernelOnly: operation.KernelOnly}
}

func invalidSample(operation experiment.AdapterOperation, code string) experiment.Sample {
	system := "taskgate"
	switch operation.Mode {
	case "direct", "rls":
		system = "postgresql"
	case "provsql":
		system = "provsql"
	}
	sample := baseSample(operation, system)
	sample.ErrorCode = code
	sample.Reason = "source-controlled adapter failed closed; inspect service logs outside the evidence channel"
	return sample
}
func zeroPipeline() map[string]float64 {
	return map[string]float64{"prepare": 0, "execute_and_derive": 0, "artifact_stage": 0, "control_settlement": 0, "artifact_publication": 0, "response_finalize": 0, "server_total": 0}
}
func durationMS(value time.Duration) float64 {
	return float64(value.Nanoseconds()) / float64(time.Millisecond)
}
func sha(value string) string { return shaBytes([]byte(value)) }
func shaBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
func receiptDigest(receipt queryreceipt.QueryReceiptV1) string {
	digest, err := queryreceipt.DocumentSHA256(receipt)
	if err != nil {
		// QueryReceiptV1 has a closed typed JSON shape, so encoding cannot fail in
		// normal operation. Returning the empty identity keeps every downstream
		// evidence gate fail-closed if that invariant ever changes.
		return ""
	}
	return digest
}
func saltedTaskHash(operation experiment.AdapterOperation, taskID string) string {
	return sha(operation.CampaignID + "\x00" + operation.DeploymentID + "\x00" + taskID)
}
func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
func pathTail(value string) string {
	if parsed, err := url.Parse(value); err == nil {
		value = parsed.Path
	}
	value = strings.TrimRight(value, "/")
	return value[strings.LastIndex(value, "/")+1:]
}

// Ensure the bounded download path cannot silently accept a response whose
// body exceeds its declared experiment ceiling.
func readExactlyBounded(reader io.Reader, maximum int64) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > maximum {
		return nil, errors.New("evidence body exceeds limit")
	}
	return value, nil
}

// baselineResultDigest reduces a drained Direct result to its canonical digest.
// A cell whose contract carries a typed schema reduces through the shared
// normalizer, which is the only reduction comparable with the released
// artifact's; a cell without one keeps the harness's structural hash, which its
// simpler column types already make comparable.
func baselineResultDigest(schema []finalv5oracle.ResultColumn, values [][]any) (string, error) {
	if len(schema) == 0 {
		return experiment.CanonicalResultHash(values)
	}
	observed, err := finalv5contracts.NormalizeDirect(schema, func(yield func([]any) error) error {
		for _, row := range values {
			if err := yield(row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return observed.Summary.CanonicalResultSHA256, nil
}

// reportObservedStatements dumps the Business statements the Observer's filter
// can see, to the diagnostic channel only. It exists because the visible count
// is "contains the visible relation and does not contain the companion", so a
// zero can mean the statement never ran, ran against different relation names,
// or ran while also naming the companion -- three very different faults that
// the count alone cannot distinguish.
func (adapter *realAdapter) reportObservedStatements(ctx context.Context, plan baselinePlan) {
	const query = `SELECT s.calls::bigint,
  position($1 in replace(lower(s.query),'"','')) > 0 AS names_visible,
  position($2 in replace(lower(s.query),'"','')) > 0 AS names_companion,
  left(replace(replace(lower(s.query),'"',''), E'\n', ' '), 180)
FROM pg_stat_statements s
WHERE s.dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
  AND s.userid=(SELECT oid FROM pg_roles WHERE rolname='gateway_reader')
  AND (position($1 in replace(lower(s.query),'"','')) > 0
       OR position($2 in replace(lower(s.query),'"','')) > 0)
ORDER BY s.calls DESC LIMIT 8`
	rows, err := adapter.observer.Query(ctx, query, plan.visibleRelation, plan.companionRelation)
	if err != nil {
		fmt.Fprintf(os.Stderr, "observed-statement diagnostic unavailable: %v\n", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var calls int64
		var visible, companion bool
		var text string
		if err := rows.Scan(&calls, &visible, &companion, &text); err != nil {
			return
		}
		fmt.Fprintf(os.Stderr, "  observed calls=%d visible=%v companion=%v :: %s\n",
			calls, visible, companion, text)
	}
}
