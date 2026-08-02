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

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/releasedartifact"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

const (
	pilotDirectSQL   = "SELECT receipt_no, department FROM reporting.expense_detail WHERE department = '销售部' ORDER BY receipt_no ASC LIMIT 3"
	pilotTaskGateSQL = "SELECT receipt_no, department FROM expense_detail WHERE department = '销售部' ORDER BY receipt_no ASC LIMIT 3"
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

type pairState struct{ taskID, novelRequestID string }

type queryResponse struct {
	ResultID         string                      `json:"result_id"`
	QueryID          string                      `json:"query_id"`
	TaskID           string                      `json:"task_id"`
	ArtifactStatus   string                      `json:"artifact_status"`
	RowCount         int64                       `json:"row_count"`
	ColumnCount      int                         `json:"column_count"`
	PipelineMS       map[string]float64          `json:"pipeline_ms"`
	DiagnosticMS     map[string]float64          `json:"diagnostic_ms"`
	PlanDigest       string                      `json:"plan_digest"`
	SemanticReplay   bool                        `json:"semantic_replay"`
	IdempotentReplay bool                        `json:"idempotent_replay"`
	Receipt          queryreceipt.QueryReceiptV1 `json:"receipt"`
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

type rootState struct {
	epoch  int64
	digest string
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

func (adapter *realAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if operation.ExperimentID != "baseline" || operation.WorkloadID != "S1" || operation.Scale != "tiny" {
		return invalidSample(operation, "unsupported_source_controlled_baseline_cell")
	}
	stateKey := operation.PairID + "\x00" + operation.RootGroupID
	state := adapter.pairs[stateKey]
	if state == nil {
		state = &pairState{}
		adapter.pairs[stateKey] = state
	}
	var sample experiment.Sample
	var err error
	if operation.Mode == "direct" {
		sample, err = adapter.direct(ctx, operation)
	} else if operation.Mode == "pending_recovery" {
		sample, err = adapter.pendingRecovery(ctx, operation, state)
	} else {
		sample, err = adapter.taskgate(ctx, operation, state)
	}
	if err != nil {
		return invalidSample(operation, "real_measurement_failed")
	}
	return sample
}

func (adapter *realAdapter) direct(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	started := time.Now()
	tx, err := adapter.business.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return experiment.Sample{}, err
	}
	rows, err := tx.Query(ctx, pilotDirectSQL)
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
	digest, err := experiment.CanonicalResultHash(values)
	if err != nil {
		return experiment.Sample{}, err
	}
	sample := baseSample(operation, "postgresql")
	sample.ClientAvailableMS, sample.ClientFullDrainMS = elapsed, elapsed
	sample.PipelineMS = zeroPipeline()
	sample.PipelineMS["execute_and_derive"], sample.PipelineMS["server_total"] = elapsed, elapsed
	sample.RowCount, sample.ColumnCount, sample.ResultSHA256 = int64(len(values)), columnCount, digest
	sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256, sample.QueryPlanSHA256 = sha(pilotDirectSQL), sha(pilotTaskGateSQL), sha(pilotDirectSQL)
	sample.Status = "pass"
	return sample, nil
}

func (adapter *realAdapter) taskgate(ctx context.Context, operation experiment.AdapterOperation, state *pairState) (experiment.Sample, error) {
	if state.taskID == "" {
		taskID, err := adapter.provisionTask(ctx, operation)
		if err != nil {
			return experiment.Sample{}, err
		}
		state.taskID = taskID
	}
	before, err := adapter.rootState(ctx, state.taskID)
	if err != nil {
		return experiment.Sample{}, err
	}
	requestID := "final-v5-" + sha(operation.PairID)[:20] + "-novel"
	sqlText := pilotTaskGateSQL
	switch operation.Mode {
	case "novel":
		state.novelRequestID = requestID
	case "semantic_replay":
		requestID = "final-v5-" + sha(operation.SampleID)[:20] + "-semantic"
	case "normalized_rewrite_replay":
		requestID = "final-v5-" + sha(operation.SampleID)[:20] + "-normalized"
		sqlText = "  SELECT receipt_no, department FROM expense_detail WHERE department='销售部' ORDER BY receipt_no ASC LIMIT 3  "
	case "idempotent_replay":
		if state.novelRequestID == "" {
			return experiment.Sample{}, errors.New("idempotent replay lacks novel anchor")
		}
		requestID = state.novelRequestID
	default:
		return experiment.Sample{}, errors.New("unsupported baseline mode")
	}
	started := time.Now()
	var response queryResponse
	if err := adapter.alice.call(ctx, "query_sql", map[string]any{"task_id": state.taskID, "request_id": requestID, "sql": sqlText}, &response); err != nil {
		return experiment.Sample{}, err
	}
	return adapter.completeTaskgateSample(ctx, operation, state, before, started, durationMS(time.Since(started)), sqlText, response)
}

func (adapter *realAdapter) completeTaskgateSample(ctx context.Context, operation experiment.AdapterOperation, state *pairState,
	before rootState, started time.Time, availableMS float64, sqlText string, response queryResponse) (experiment.Sample, error) {
	if response.TaskID != state.taskID || response.ArtifactStatus != "AVAILABLE" || response.ResultID == "" || response.QueryID == "" {
		return experiment.Sample{}, errors.New("query response omitted AVAILABLE identity")
	}
	if response.Receipt.Version != queryreceipt.VersionV8 || response.Receipt.ArtifactIntent == nil || response.Receipt.QueryID != response.QueryID || response.Receipt.TaskID != state.taskID {
		return experiment.Sample{}, errors.New("query response omitted matching V8 receipt")
	}
	auditEvidence, err := adapter.loadAuditEvidence(ctx, response)
	if err != nil {
		return experiment.Sample{}, err
	}
	var delivery struct {
		DownloadURL    string `json:"download_url"`
		ArtifactSHA256 string `json:"artifact_sha256"`
	}
	if err := adapter.alice.call(ctx, "deliver_result", map[string]any{"result_id": response.ResultID, "format": "parquet"}, &delivery); err != nil {
		return experiment.Sample{}, err
	}
	if delivery.DownloadURL == "" || delivery.ArtifactSHA256 != response.Receipt.ArtifactIntent.ParquetSHA256 {
		return experiment.Sample{}, errors.New("delivery metadata disagrees with receipt")
	}
	parquetBytes, err := httpGet(ctx, adapter.http, delivery.DownloadURL, 1<<30)
	if err != nil {
		return experiment.Sample{}, err
	}
	if shaBytes(parquetBytes) != delivery.ArtifactSHA256 {
		return experiment.Sample{}, errors.New("downloaded Parquet digest mismatch")
	}
	expectedBinding, canonicalEvidence, canonicalObject, err := adapter.openCanonicalObject(ctx, response)
	if err != nil {
		return experiment.Sample{}, err
	}
	defer canonicalObject.Close()
	canonicalEvidence.ReleasedParquet = parquetBytes
	availability := auditEvidence.Availability
	if err := releasedartifact.VerifyReleasedArtifact(adapter.verifier, releasedartifact.SettlementEvidence{
		Receipt: response.Receipt, ExpectedBinding: expectedBinding, ReceiptInclusion: auditEvidence.Audit,
		TerminalInclusion: auditEvidence.Terminal, RegistrationInclusion: auditEvidence.Registration,
		AvailabilityInclusion: &availability,
	}, canonicalEvidence); err != nil {
		return experiment.Sample{}, err
	}
	if err := validateResponseAgainstVerifiedReceipt(response); err != nil {
		return experiment.Sample{}, err
	}
	intent := response.Receipt.ArtifactIntent
	signedExposure := response.Receipt.Exposure
	rows, err := parseParquet(parquetBytes, intent.ResultID, intent.RowCount)
	if err != nil {
		return experiment.Sample{}, err
	}
	resultDigest, err := experiment.CanonicalResultHash(rows)
	if err != nil {
		return experiment.Sample{}, err
	}
	after, err := adapter.rootState(ctx, state.taskID)
	if err != nil {
		return experiment.Sample{}, err
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
			return experiment.Sample{}, errors.New("semantic replay marker missing")
		}
		sample.BusinessSQLDelta = 0
	} else if operation.Mode == "idempotent_replay" || operation.Mode == "pending_recovery" {
		if !response.IdempotentReplay {
			return experiment.Sample{}, errors.New("idempotent replay marker missing")
		}
		if operation.Mode == "idempotent_replay" {
			sample.BusinessSQLDelta = 0
		} else {
			sample.BusinessSQLDelta = 1
		}
	} else {
		sample.BusinessSQLDelta = 1
	}
	sample.RootEpochBefore, sample.RootEpochAfter = before.epoch, after.epoch
	sample.RootSetSHA256Before, sample.RootSetSHA256After = before.digest, after.digest
	sample.RootTaskIDHash = saltedTaskHash(operation, state.taskID)
	sample.ParquetBytes, sample.EncryptedObjectBytes = intent.ParquetSize, intent.ObjectSize
	sample.ArtifactSHA256, sample.ObjectSHA256 = intent.ParquetSHA256, intent.ObjectSHA256
	sample.ReceiptVersion, sample.ReceiptSHA256, sample.ArtifactIntentSHA256 = response.Receipt.Version, receiptDigest(response.Receipt), intent.IntentSHA256
	availabilityBytes, _ := json.Marshal(auditEvidence.Availability)
	sample.AvailabilityAuditSHA256, sample.ReceiptVerified, sample.ArtifactAvailable = shaBytes(availabilityBytes), true, true
	sample.BaselineVerification = &experiment.BaselineVerificationEvidence{Receipt: response.Receipt, KeyBundle: adapter.keyBundle, AuditProof: auditEvidence.Audit, TerminalProof: auditEvidence.Terminal, RegistrationProof: auditEvidence.Registration, AvailabilityProof: auditEvidence.Availability, ArtifactStatus: canonicalEvidence.Status, DownloadedParquetSHA256: shaBytes(parquetBytes), ParsedResultSHA256: resultDigest}
	sample.Status = "pass"
	return sample, nil
}

type recoverySnapshot struct {
	queryRecords   int64
	usedQueries    int64
	settlements    int64
	artifactStatus string
	objectKey      string
	receiptSHA256  string
	intentSHA256   string
}

func (adapter *realAdapter) pendingRecovery(ctx context.Context, operation experiment.AdapterOperation, state *pairState) (experiment.Sample, error) {
	if state.taskID == "" {
		taskID, err := adapter.provisionTask(ctx, operation)
		if err != nil {
			return experiment.Sample{}, err
		}
		state.taskID = taskID
	}
	beforeRoot, err := adapter.rootState(ctx, state.taskID)
	if err != nil {
		return experiment.Sample{}, err
	}
	before, err := adapter.recoverySnapshot(ctx, state.taskID, "")
	if err != nil {
		return experiment.Sample{}, err
	}
	businessBefore, err := adapter.businessCallCount(ctx)
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
	atFailure, err := adapter.recoverySnapshot(ctx, state.taskID, requestID)
	if err != nil {
		return experiment.Sample{}, err
	}
	businessAtFailure, err := adapter.businessCallCount(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	if atFailure.artifactStatus != "PENDING" || atFailure.objectKey == "" {
		return experiment.Sample{}, errors.New("forced failure did not leave a durable PENDING artifact")
	}
	stat, err := adapter.objectStore.StatObject(ctx, adapter.objectBucket, atFailure.objectKey, minio.StatObjectOptions{})
	if err != nil || stat.Size <= 0 {
		return experiment.Sample{}, errors.New("canonical object was not present before PENDING recovery")
	}
	if err := adapter.removeAvailabilityBlocker(ctx); err != nil {
		return experiment.Sample{}, err
	}
	blockerInstalled = false
	var response queryResponse
	if err := adapter.alice.call(ctx, "query_sql", map[string]any{"task_id": state.taskID, "request_id": requestID, "sql": pilotTaskGateSQL}, &response); err != nil {
		return experiment.Sample{}, err
	}
	sample, err := adapter.completeTaskgateSample(ctx, operation, state, beforeRoot, started, durationMS(time.Since(started)), pilotTaskGateSQL, response)
	if err != nil {
		return experiment.Sample{}, err
	}
	after, err := adapter.recoverySnapshot(ctx, state.taskID, requestID)
	if err != nil {
		return experiment.Sample{}, err
	}
	businessAfter, err := adapter.businessCallCount(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	sample.RecoveryVerification = &experiment.RecoveryVerificationEvidence{
		FailureObserved: true, CanonicalObjectObserved: true,
		ArtifactStatusBefore: atFailure.artifactStatus, ArtifactStatusAfter: after.artifactStatus,
		BusinessCallsBefore: businessBefore, BusinessCallsAtFailure: businessAtFailure, BusinessCallsAfter: businessAfter,
		QueryRecordsBefore: before.queryRecords, QueryRecordsAtFailure: atFailure.queryRecords, QueryRecordsAfter: after.queryRecords,
		SettlementsAtFailure: atFailure.settlements, SettlementsAfter: after.settlements,
		UsedQueriesBefore: before.usedQueries, UsedQueriesAtFailure: atFailure.usedQueries, UsedQueriesAfter: after.usedQueries,
		ReceiptSHA256AtFailure: atFailure.receiptSHA256, ReceiptSHA256After: after.receiptSHA256,
		IntentSHA256AtFailure: atFailure.intentSHA256, IntentSHA256After: after.intentSHA256,
	}
	return sample, nil
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
	if err := adapter.control.QueryRow(ctx, `SELECT count(*) FROM query_records WHERE task_id=$1`, taskID).Scan(&snapshot.queryRecords); err != nil {
		return snapshot, err
	}
	if err := adapter.control.QueryRow(ctx, `SELECT used_queries FROM budget_ledger WHERE task_id=$1`, taskID).Scan(&snapshot.usedQueries); err != nil {
		return snapshot, err
	}
	if requestID == "" {
		return snapshot, nil
	}
	var receiptJSON []byte
	if err := adapter.control.QueryRow(ctx, `
SELECT a.status,a.object_key,r.receipt_json,
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_V5_EXPOSURE_SETTLED')
FROM query_records q
JOIN result_artifacts a ON a.query_id=q.id
JOIN query_receipts r ON r.query_id=q.id
WHERE q.task_id=$1 AND q.request_id=$2`, taskID, requestID).
		Scan(&snapshot.artifactStatus, &snapshot.objectKey, &receiptJSON, &snapshot.settlements); err != nil {
		return snapshot, err
	}
	var receipt queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(receiptJSON, &receipt); err != nil || receipt.ArtifactIntent == nil {
		return snapshot, errors.New("PENDING recovery receipt omitted its V8 artifact intent")
	}
	// Control persists RFC 8785 canonical bytes while the public response is
	// decoded into a typed receipt and encoded by the experiment harness. Hash
	// the typed identity on both paths so key ordering cannot create a false
	// recovery mismatch. Every typed receipt field, including its signature,
	// remains part of this identity; Control separately enforces persistence
	// immutability.
	snapshot.receiptSHA256 = receiptDigest(receipt)
	snapshot.intentSHA256 = receipt.ArtifactIntent.IntentSHA256
	return snapshot, nil
}

func (adapter *realAdapter) businessCallCount(ctx context.Context) (int64, error) {
	const query = `SELECT COALESCE(sum(s.calls),0)::bigint
FROM pg_stat_statements s
WHERE s.dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
  AND s.userid=(SELECT oid FROM pg_roles WHERE rolname='gateway_reader')
  AND (position('reporting.expense_detail' in replace(lower(s.query),'"','')) > 0
       OR position('taskgate_ordinal.expense_detail_v1' in replace(lower(s.query),'"','')) > 0)`
	var calls int64
	if err := adapter.observer.QueryRow(ctx, query).Scan(&calls); err != nil {
		return 0, err
	}
	return calls, nil
}

func (adapter *realAdapter) provisionTask(ctx context.Context, operation experiment.AdapterOperation) (string, error) {
	var created struct {
		TaskID string `json:"task_id"`
		OAURL  string `json:"oa_url"`
	}
	arguments := map[string]any{"objective": "Final V5 real baseline pilot " + operation.PairID, "data_products": []string{"expense_detail"}, "columns": map[string][]string{"expense_detail": {"receipt_no", "department"}}, "scopes": map[string]any{"department": []string{"销售部"}}}
	if err := adapter.alice.call(ctx, "request_data_task", arguments, &created); err != nil {
		return "", err
	}
	if created.TaskID == "" || created.OAURL == "" {
		return "", errors.New("task request omitted identity")
	}
	draftID := pathTail(created.OAURL)
	if err := oaAction(ctx, adapter.aliceOA, adapter.oaBase, draftID, "submit", ""); err != nil {
		return "", err
	}
	if err := adapter.waitTask(ctx, created.TaskID, "AWAITING_APPROVAL"); err != nil {
		return "", err
	}
	if err := oaAction(ctx, adapter.bobOA, adapter.oaBase, draftID, "decision", "approved"); err != nil {
		return "", err
	}
	if err := adapter.waitTask(ctx, created.TaskID, "ACTIVE"); err != nil {
		return "", err
	}
	return created.TaskID, nil
}

func (adapter *realAdapter) waitTask(ctx context.Context, taskID, expected string) error {
	deadline := time.Now().Add(adapter.timeout)
	for time.Now().Before(deadline) {
		var status struct {
			State string `json:"state"`
		}
		if adapter.alice.call(ctx, "get_task_status", map[string]string{"task_id": taskID}, &status) == nil && status.State == expected {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("task state transition timed out")
}

func (adapter *realAdapter) rootState(ctx context.Context, taskID string) (rootState, error) {
	var epoch int64
	var release, influence, outcome string
	err := adapter.control.QueryRow(ctx, `SELECT epoch,COALESCE(release_set_sha256,''),COALESCE(influence_set_sha256,''),COALESCE(outcome_set_sha256,'') FROM v5_exposure_root_heads WHERE root_task_id=(SELECT root_task_id FROM tasks WHERE id=$1)`, taskID).Scan(&epoch, &release, &influence, &outcome)
	if err != nil {
		return rootState{}, err
	}
	return rootState{epoch: epoch, digest: sha(strings.Join([]string{release, influence, outcome}, "\x00"))}, nil
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
	return experiment.Sample{SchemaVersion: 1, CampaignID: operation.CampaignID, DeploymentID: operation.DeploymentID, ExperimentID: operation.ExperimentID, CellID: operation.CellID, SampleID: operation.SampleID, Iteration: operation.Iteration, ProcessReplicate: operation.ProcessReplicate, OrderPosition: operation.OrderPosition, RandomSeed: operation.RandomSeed, PairID: operation.PairID, PairedSystemOrder: operation.PairedSystemOrder, RootGroupID: operation.RootGroupID, System: system, Mode: operation.Mode, WorkloadID: operation.WorkloadID, Scale: operation.Scale, PipelineMS: zeroPipeline(), DiagnosticMS: map[string]float64{}, Status: "invalid", PublicationEligible: operation.CampaignClass == "publication"}
}

func invalidSample(operation experiment.AdapterOperation, code string) experiment.Sample {
	sample := baseSample(operation, "taskgate")
	if operation.Mode == "direct" {
		sample.System = "postgresql"
	}
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
	encoded, _ := json.Marshal(receipt)
	return shaBytes(encoded)
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
