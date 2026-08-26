package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

func TestArtifactStoredResultAppliesAliasesAndReordersSemanticIdentities(t *testing.T) {
	stored := storedQueryResult{
		Columns: []dataconnector.Column{
			{Name: "canonical_month", DataTypeOID: 25},
			{Name: "canonical_amount", DataTypeOID: 20},
			{Name: "canonical_approved", DataTypeOID: 16},
		},
		Rows:            [][]any{{"2026-07", int64(42), true}},
		DisplayColumns:  []string{"approved", "period", "spend"},
		ResultOrder:     []int{2, 0, 1},
		SemanticColumns: []string{"column:month", "column:amount", "column:approved"},
	}

	got, err := artifactStoredResult(stored)
	if err != nil {
		t.Fatalf("artifactStoredResult: %v", err)
	}
	wantColumns := []dataconnector.Column{
		{Name: "approved", DataTypeOID: 16},
		{Name: "period", DataTypeOID: 25},
		{Name: "spend", DataTypeOID: 20},
	}
	wantRows := [][]any{{true, "2026-07", int64(42)}}
	wantSemantic := []string{"column:approved", "column:month", "column:amount"}
	if !reflect.DeepEqual(got.Columns, wantColumns) {
		t.Fatalf("artifact columns = %#v, want %#v", got.Columns, wantColumns)
	}
	if !reflect.DeepEqual(got.Rows, wantRows) {
		t.Fatalf("artifact rows = %#v, want %#v", got.Rows, wantRows)
	}
	if !reflect.DeepEqual(got.SemanticColumns, wantSemantic) {
		t.Fatalf("artifact semantic identities = %#v, want %#v", got.SemanticColumns, wantSemantic)
	}
	if got.DisplayColumns != nil || got.ResultOrder != nil {
		t.Fatalf("artifact retained presentation metadata: aliases=%#v order=%#v", got.DisplayColumns, got.ResultOrder)
	}

	// The conversion receives a value copy and must not rewrite replay metadata
	// that remains attached to the connector result.
	if !reflect.DeepEqual(stored.ResultOrder, []int{2, 0, 1}) ||
		!reflect.DeepEqual(stored.SemanticColumns, []string{"column:month", "column:amount", "column:approved"}) {
		t.Fatalf("artifact conversion mutated its input: %#v", stored)
	}
}

func TestResultDeliveryCapabilityExpiresAndDownloadsDoNotMutateState(t *testing.T) {
	harness := newGatewayHarness(t)
	// PostgreSQL NUMERIC is lossless at the connector boundary. Do not use the
	// harness's legacy float fixture here: artifact encoding intentionally
	// rejects binary floats for an exact NUMERIC column.
	harness.connector.result.Rows = [][]any{{"sensitive-row", json.Number("123.45")}}
	backend := newGatewayArtifactMemoryBackend()
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatalf("create artifact cipher: %v", err)
	}
	tempDir := t.TempDir()
	if err := os.Chmod(tempDir, 0o700); err != nil {
		t.Fatalf("restrict temp directory: %v", err)
	}
	manager, err := resultartifact.NewManager(backend, cipher, tempDir)
	if err != nil {
		t.Fatalf("create artifact manager: %v", err)
	}
	harness.service.resultArtifacts = manager
	harness.service.resultTTL = time.Hour
	harness.service.deliveryBaseURL = "https://downloads.taskgate.test"
	harness.service.deliverySigningKey = []byte("gateway-artifact-delivery-test-key")
	harness.service.deliveryTTL = 2 * time.Minute

	const taskID = "task-artifact-delivery"
	harness.createActiveSummaryTask(t, taskID)
	queryResult := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": taskID, "request_id": "artifact-delivery-1", "sql": testSummarySQL,
	})
	resultID, ok := queryResult["result_id"].(string)
	if !ok || resultID == "" {
		t.Fatalf("query result_id = %#v", queryResult["result_id"])
	}
	queryID, ok := queryResult["query_id"].(string)
	if !ok || queryID == "" {
		t.Fatalf("query query_id = %#v", queryResult["query_id"])
	}
	if _, present := queryResult["rows"]; present {
		t.Fatalf("artifact-backed query exposed rows to the Agent: %#v", queryResult["rows"])
	}
	replayed := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": taskID, "request_id": "artifact-delivery-1", "sql": testSummarySQL,
	})
	if replayed["result_id"] != resultID || replayed["status"] != control.QueryCompleted ||
		replayed["artifact_status"] != control.ResultArtifactAvailable {
		t.Fatalf("artifact idempotent replay = %#v", replayed)
	}
	if _, present := replayed["rows"]; present {
		t.Fatalf("artifact replay exposed rows: %#v", replayed["rows"])
	}
	metadata := mustCallGatewayTool(t, harness.service, harness.alice, "get_query_result", map[string]any{
		"task_id": taskID, "query_id": queryID,
	})
	if metadata["result_id"] != resultID || metadata["status"] != control.QueryCompleted ||
		metadata["artifact_status"] != control.ResultArtifactAvailable {
		t.Fatalf("artifact metadata response = %#v", metadata)
	}
	if _, present := metadata["rows"]; present {
		t.Fatalf("artifact metadata lookup exposed rows: %#v", metadata["rows"])
	}

	before := captureGatewayDeliveryState(t, harness, taskID, queryID, resultID)
	if before.artifact.Status != control.ResultArtifactAvailable || before.artifact.ConsumedAt == nil {
		t.Fatalf("AVAILABLE transition did not establish logical consumption: %+v", before.artifact)
	}

	delivery := mustCallGatewayTool(t, harness.service, harness.alice, "deliver_result", map[string]any{
		"result_id": resultID, "format": "parquet",
	})
	downloadURL, ok := delivery["download_url"].(string)
	if !ok || downloadURL == "" {
		t.Fatalf("delivery URL = %#v", delivery["download_url"])
	}
	parsed, err := url.Parse(downloadURL)
	if err != nil {
		t.Fatalf("parse delivery URL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "downloads.taskgate.test" ||
		parsed.Path != "/api/v1/results/"+resultID+"/download" {
		t.Fatalf("delivery URL target = %s", parsed.String())
	}
	expires, err := strconv.ParseInt(parsed.Query().Get("expires"), 10, 64)
	if err != nil {
		t.Fatalf("parse delivery expiry: %v", err)
	}
	wantExpiry := harness.clock.value.Add(harness.service.deliveryTTL)
	if expires != wantExpiry.Unix() {
		t.Fatalf("delivery expiry = %d, want %d", expires, wantExpiry.Unix())
	}
	if token := parsed.Query().Get("token"); token == "" ||
		token != harness.service.deliverySignature(resultID, taskID, expires) {
		t.Fatalf("delivery token is not bound to result, task, and expiry: %q", token)
	}

	router := chi.NewRouter()
	router.Handle("/api/v1/results/{result_id}/download", harness.service.ResultDownloadHandler())
	download := func(rawURL string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, rawURL, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	for range cap(harness.service.downloadSlots) {
		harness.service.downloadSlots <- struct{}{}
	}
	saturated := download(downloadURL)
	for range cap(harness.service.downloadSlots) {
		<-harness.service.downloadSlots
	}
	if saturated.Code != http.StatusServiceUnavailable || saturated.Header().Get("Retry-After") != "5" {
		t.Fatalf("saturated download = %d Retry-After=%q", saturated.Code, saturated.Header().Get("Retry-After"))
	}

	for attempt := 1; attempt <= 2; attempt++ {
		response := download(downloadURL)
		if response.Code != http.StatusOK {
			t.Fatalf("download %d status = %d, body=%q", attempt, response.Code, response.Body.String())
		}
		body := response.Body.Bytes()
		if !bytes.HasPrefix(body, []byte("PAR1")) || !bytes.HasSuffix(body, []byte("PAR1")) {
			t.Fatalf("download %d is not plaintext Parquet framing", attempt)
		}
		if contentType := response.Header().Get("Content-Type"); contentType != "application/vnd.apache.parquet" {
			t.Fatalf("download %d content type = %q", attempt, contentType)
		}
		if contentLength := response.Header().Get("Content-Length"); contentLength != strconv.FormatInt(before.artifact.ParquetSize, 10) {
			t.Fatalf("download %d content length = %q", attempt, contentLength)
		}
		digestBytes, _ := hex.DecodeString(before.artifact.ParquetSHA256)
		wantDigest := "sha-256=:" + base64.StdEncoding.EncodeToString(digestBytes) + ":"
		if contentDigest := response.Header().Get("Content-Digest"); contentDigest != wantDigest {
			t.Fatalf("download %d content digest = %q, want %q", attempt, contentDigest, wantDigest)
		}
		if disposition := response.Header().Get("Content-Disposition"); disposition != fmt.Sprintf("attachment; filename=%q", resultID+".parquet") {
			t.Fatalf("download %d content disposition = %q", attempt, disposition)
		}
	}

	// Changing any signed capability field must fail before object access.
	tampered := *parsed
	tamperedQuery := tampered.Query()
	tamperedQuery.Set("expires", strconv.FormatInt(expires+1, 10))
	tampered.RawQuery = tamperedQuery.Encode()
	if response := download(tampered.String()); response.Code != http.StatusUnauthorized {
		t.Fatalf("tampered capability status = %d, want %d", response.Code, http.StatusUnauthorized)
	}

	// Expiry is exclusive: the capability is invalid at its exact expiry even
	// though the canonical artifact itself still has almost an hour of TTL.
	harness.clock.value = time.Unix(expires, 0).UTC()
	if response := download(downloadURL); response.Code != http.StatusUnauthorized {
		t.Fatalf("expired capability status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got := backend.getCount(); got != 2 {
		t.Fatalf("object reads = %d, want only the two authorized downloads", got)
	}

	after := captureGatewayDeliveryState(t, harness, taskID, queryID, resultID)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("delivery generation/download mutated durable state\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestArtifactRecoveryGatesReadinessUntilFullPassCompletes(t *testing.T) {
	harness := newGatewayHarness(t)
	backend := newGatewayArtifactMemoryBackend()
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x29}, 32))
	if err != nil {
		t.Fatalf("create artifact cipher: %v", err)
	}
	tempDir := t.TempDir()
	if err := os.Chmod(tempDir, 0o700); err != nil {
		t.Fatalf("restrict temp directory: %v", err)
	}
	manager, err := resultartifact.NewManager(backend, cipher, tempDir)
	if err != nil {
		t.Fatalf("create artifact manager: %v", err)
	}
	harness.service.resultArtifacts = manager
	harness.service.artifactRecoveryRunning.Store(true)
	if err := harness.service.ReadyError(); err == nil || !strings.Contains(err.Error(), "recovery is in progress") {
		t.Fatalf("readiness while recovery is pending = %v", err)
	}
	completed, err := harness.service.ReconcilePendingArtifacts(t.Context())
	if err != nil || completed != 0 {
		t.Fatalf("empty artifact reconciliation = %d, %v", completed, err)
	}
	if err := harness.service.ReadyError(); err != nil {
		t.Fatalf("readiness after full reconciliation = %v", err)
	}
}

func TestArtifactPromotionFailurePreservesSettlementAndRecoversWithoutReexecution(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.connector.result.Rows = [][]any{{"sensitive-row", json.Number("123.45")}}
	backend := newGatewayArtifactMemoryBackend()
	backend.copyFailures = 1
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	if err := os.Chmod(tempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := resultartifact.NewManager(backend, cipher, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	harness.service.resultArtifacts = manager
	const taskID = "task-artifact-promotion-recovery"
	const requestID = "artifact-promotion-recovery-1"
	harness.createActiveSummaryTask(t, taskID)
	if _, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": taskID, "request_id": requestID, "sql": testSummarySQL,
	}); err == nil {
		t.Fatal("promotion failure returned an available result")
	}
	record, err := harness.store.GetQueryByRequestID(t.Context(), taskID, requestID)
	if err != nil || record.Status != control.QueryCompleted {
		t.Fatalf("durable query after promotion failure = %+v, %v", record, err)
	}
	artifact, err := harness.store.GetResultArtifactByQuery(t.Context(), record.ID)
	if err != nil || artifact.Status != control.ResultArtifactPending || artifact.ConsumedAt != nil {
		t.Fatalf("artifact after promotion failure = %+v, %v", artifact, err)
	}
	budgetBefore, err := harness.store.GetBudget(t.Context(), taskID)
	if err != nil || budgetBefore.Usage.UsedQueries != 1 {
		t.Fatalf("settled budget after promotion failure = %+v, %v", budgetBefore, err)
	}
	receiptBefore, err := harness.store.GetQueryReceipt(t.Context(), record.ID)
	if err != nil {
		t.Fatalf("settlement receipt after promotion failure: %v", err)
	}
	consumedBefore, err := harness.store.ListAuditEvents(t.Context(), control.AuditFilter{
		QueryID: record.ID, EventType: "QUERY_RESULT_CONSUMED",
	})
	if err != nil || len(consumedBefore) != 0 {
		t.Fatalf("consumption audit before availability = %+v, %v", consumedBefore, err)
	}
	if err := harness.service.ReadyError(); err == nil {
		t.Fatal("readiness stayed open with a failed PENDING promotion")
	}
	connectorCalls := len(harness.connector.requests)
	completed, err := harness.service.ReconcilePendingArtifacts(t.Context())
	if err != nil || completed != 1 {
		t.Fatalf("artifact recovery = %d, %v", completed, err)
	}
	artifact, err = harness.store.GetResultArtifactByQuery(t.Context(), record.ID)
	if err != nil || artifact.Status != control.ResultArtifactAvailable || artifact.ConsumedAt == nil {
		t.Fatalf("artifact after recovery = %+v, %v", artifact, err)
	}
	budgetAfter, _ := harness.store.GetBudget(t.Context(), taskID)
	receiptAfter, _ := harness.store.GetQueryReceipt(t.Context(), record.ID)
	if !reflect.DeepEqual(budgetAfter, budgetBefore) {
		t.Fatalf("recovery changed the settled budget\n%+v ->\n%+v", budgetBefore, budgetAfter)
	}
	// The settlement evidence is compared member by member rather than as a whole
	// struct, because one member of it is not settlement evidence.
	//
	// QueryReceipt.Artifact is the LIVE result-object row, and promoting a PENDING
	// object to AVAILABLE is precisely what the recovery under test does -- so a
	// whole-struct comparison asserts that recovery did not do its job. What must
	// not move is what settlement sealed: the query record, the terminal audit
	// event, the signed receipt document, and the registration audit the signature
	// covers.
	for name, pair := range map[string][2]any{
		"query record":       {receiptBefore.Query, receiptAfter.Query},
		"terminal audit":     {receiptBefore.Audit, receiptAfter.Audit},
		"signed receipt":     {receiptBefore.Receipt, receiptAfter.Receipt},
		"registration audit": {receiptBefore.ArtifactRegistrationAudit, receiptAfter.ArtifactRegistrationAudit},
		"exposure":           {receiptBefore.Exposure, receiptAfter.Exposure},
		"execution binding":  {receiptBefore.ExecutionBinding, receiptAfter.ExecutionBinding},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Fatalf("recovery changed the settled %s\n%+v ->\n%+v", name, pair[0], pair[1])
		}
	}
	// And the object itself moved in exactly one way: from PENDING to consumed.
	if receiptBefore.Artifact.Status != control.ResultArtifactPending ||
		receiptAfter.Artifact.Status != control.ResultArtifactAvailable {
		t.Fatalf("the recovered artifact went %s -> %s, want PENDING -> AVAILABLE",
			receiptBefore.Artifact.Status, receiptAfter.Artifact.Status)
	}
	before, after := *receiptBefore.Artifact, *receiptAfter.Artifact
	before.Status, after.Status = "", ""
	before.ConsumedAt, after.ConsumedAt = nil, nil
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("recovery changed the artifact beyond its promotion\n%+v ->\n%+v", before, after)
	}
	if len(harness.connector.requests) != connectorCalls {
		t.Fatalf("recovery re-executed Business PostgreSQL: calls %d -> %d", connectorCalls, len(harness.connector.requests))
	}
	consumedAfter, err := harness.store.ListAuditEvents(t.Context(), control.AuditFilter{
		QueryID: record.ID, EventType: "QUERY_RESULT_CONSUMED",
	})
	if err != nil || len(consumedAfter) != 1 {
		t.Fatalf("consumption audit after availability = %+v, %v", consumedAfter, err)
	}
	if err := harness.service.ReadyError(); err != nil {
		t.Fatalf("readiness after recovery: %v", err)
	}
}

func TestCanonicalCopySurvivesAvailableTransactionFailureAndRecoversExactlyOnce(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.installCatalogV4SnapshotRegistry(t, "expense-summary-v1")
	const taskID = "task-copy-before-available"
	const requestID = "copy-before-available-1"
	// The task is created before the Connector fixture, because the fixture is
	// built from the program production prepares for this task's own grant.
	harness.createExposureV5SummaryTask(t, taskID, control.ExposureLimits{
		ReleaseFacts: 20, InfluenceFacts: 20, OutcomeFacts: 20,
	})
	plan := queryplan.QueryPlan{Product: "expense_summary", Columns: []string{"month", "total_amount"}}
	bound := prepareOrdinalForTest(t, harness, taskID, plan)
	row := map[string]any{
		"month": "2026-01", "department": "销售部", "expense_type": "机票",
		"total_amount": json.Number("1680.00"),
	}
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}},
		Rows:    [][]any{{row["month"], row["total_amount"]}}, RowCount: 1, DatabaseTime: 2 * time.Millisecond,
	}
	provenanceColumns, positions := ordinalProvenanceColumns(bound.Program)
	provenanceRow := make([]any, len(provenanceColumns))
	for _, source := range bound.Program.Sources {
		entityKey := ordinalFixtureEntityKey(t, source, row)
		handle, present := bound.Indexes[source.SourceAlias].LookupRowHandle(entityKey)
		if !present {
			t.Fatalf("snapshot index misses entity %q", entityKey)
		}
		provenanceRow[positions[source.HandleAlias]] = uint64(handle)
		for _, field := range source.EvidenceFields {
			provenanceRow[positions[field.ProvenanceAlias]] = row[field.Column]
		}
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: provenanceColumns, Rows: [][]any{provenanceRow}, RowCount: 1, DatabaseTime: time.Millisecond,
	}
	backend := newGatewayArtifactMemoryBackend()
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x32}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	if err := os.Chmod(tempDir, 0o700); err != nil {
		t.Fatalf("restrict temp directory: %v", err)
	}
	manager, err := resultartifact.NewManager(backend, cipher, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	harness.service.resultArtifacts = manager
	harness.service.deliverySigningKey = []byte("copy-before-available-test")
	availabilityBlocked := true
	harness.service.markArtifactAvailable = func(ctx context.Context, resultID, etag, actor string) (control.ResultArtifact, error) {
		if availabilityBlocked {
			return control.ResultArtifact{}, errors.New("injected AVAILABLE transaction failure")
		}
		return harness.store.MarkResultArtifactAvailable(ctx, resultID, etag, actor)
	}

	arguments := map[string]any{
		"task_id": taskID, "request_id": requestID,
		"plan": map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}},
	}
	_, executeErr := callGatewayTool(harness.service, harness.alice, "execute_plan", arguments)
	if executeErr == nil {
		t.Fatal("AVAILABLE transaction failure returned an available result")
	}
	record, err := harness.store.GetQueryByRequestID(t.Context(), taskID, requestID)
	if err != nil || record.Status != control.QueryCompleted {
		t.Fatalf("durable query after AVAILABLE failure = %+v, lookup=%v, execute=%v", record, err, executeErr)
	}
	artifact, err := harness.store.GetResultArtifactByQuery(t.Context(), record.ID)
	if err != nil || artifact.Status != control.ResultArtifactPending || artifact.ConsumedAt != nil {
		t.Fatalf("logical artifact after AVAILABLE failure = %+v, %v", artifact, err)
	}
	if _, err := backend.Stat(t.Context(), artifact.ObjectKey); err != nil {
		t.Fatalf("canonical object was not created before AVAILABLE failure: %v", err)
	}
	_, copiesAfterFailure := backend.operationCounts()
	if copiesAfterFailure != 1 {
		t.Fatalf("canonical copy calls before recovery = %d, want 1", copiesAfterFailure)
	}
	budgetBefore, err := harness.store.GetBudget(t.Context(), taskID)
	if err != nil || budgetBefore.Usage.UsedQueries != 1 {
		t.Fatalf("budget after AVAILABLE failure = %+v, %v", budgetBefore, err)
	}
	chargeBefore, err := harness.store.GetExposureCharge(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	receiptBefore, err := harness.store.GetQueryReceipt(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receiptBefore.Receipt == nil {
		t.Fatal("no receipt was persisted before artifact recovery")
	}
	var signedBefore queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(receiptBefore.Receipt.ReceiptJSON, &signedBefore); err != nil {
		t.Fatal(err)
	}
	// V10, not V8. This operation is exposure-V5 and it completed, so it has a
	// persisted QueryExecutionBindingV2; a receipt that dropped it and emitted V8
	// would be the silent downgrade the version selection refuses. The artifact
	// intent is still required here because this delivery registered a result
	// object -- V10 states its mode rather than requiring one, and an artifact
	// delivery that omitted the intent would be describing a different delivery.
	if signedBefore.Version != queryreceipt.Version || signedBefore.Exposure == nil ||
		signedBefore.Exposure.ProfileVersion != exposure.ProfileV5 || signedBefore.ArtifactIntent == nil ||
		signedBefore.ExecutionBindingV2 == nil || signedBefore.ExposureLedgerBefore == nil {
		t.Fatalf("crash-window receipt is not explicit V5 + V10 evidence: %+v", signedBefore)
	}
	intentBefore := *signedBefore.ArtifactIntent
	connectorCalls := len(harness.connector.requests)

	for tool, args := range map[string]map[string]any{
		"preview_result":   {"result_id": artifact.ResultID},
		"deliver_result":   {"result_id": artifact.ResultID, "format": "parquet"},
		"get_query_result": {"task_id": taskID, "query_id": record.ID},
	} {
		if _, err := callGatewayTool(harness.service, harness.alice, tool, args); err == nil {
			t.Fatalf("%s exposed a PENDING canonical object", tool)
		}
	}
	pendingAuditReceipt := mustCallGatewayTool(t, harness.service, harness.carol, "get_audit_receipt", map[string]any{
		"receipt_id": record.ID,
	})
	if _, exists := pendingAuditReceipt["availability_event_inclusion"]; exists {
		t.Fatalf("PENDING artifact exposed an availability event proof: %#v", pendingAuditReceipt)
	}
	router := chi.NewRouter()
	router.Handle("/api/v1/results/{result_id}/download", harness.service.ResultDownloadHandler())
	expires := harness.clock.value.Add(time.Minute).Unix()
	token := harness.service.deliverySignature(artifact.ResultID, taskID, expires)
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf(
		"/api/v1/results/%s/download?task_id=%s&expires=%d&token=%s", artifact.ResultID, taskID, expires, token), nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("download status for PENDING canonical object = %d, want 404", response.Code)
	}
	getsBeforeRecovery, copiesBeforeRecovery := backend.operationCounts()

	availabilityBlocked = false
	completed, err := harness.service.ReconcilePendingArtifacts(t.Context())
	if err != nil || completed != 1 {
		t.Fatalf("recover existing canonical object = %d, %v", completed, err)
	}
	getsAfterRecovery, copiesAfterRecovery := backend.operationCounts()
	if copiesAfterRecovery != copiesBeforeRecovery || getsAfterRecovery != getsBeforeRecovery+1 {
		t.Fatalf("recovery operations: gets %d -> %d, copies %d -> %d; want one exact-digest read and no recopy",
			getsBeforeRecovery, getsAfterRecovery, copiesBeforeRecovery, copiesAfterRecovery)
	}
	after, err := harness.store.GetResultArtifactByQuery(t.Context(), record.ID)
	if err != nil || after.Status != control.ResultArtifactAvailable || after.ConsumedAt == nil {
		t.Fatalf("artifact after recovery = %+v, %v", after, err)
	}
	budgetAfter, _ := harness.store.GetBudget(t.Context(), taskID)
	chargeAfter, _ := harness.store.GetExposureCharge(t.Context(), record.ID)
	receiptAfter, _ := harness.store.GetQueryReceipt(t.Context(), record.ID)
	if !reflect.DeepEqual(budgetAfter, budgetBefore) || !reflect.DeepEqual(chargeAfter, chargeBefore) ||
		receiptAfter.Receipt == nil || !reflect.DeepEqual(receiptAfter.Receipt, receiptBefore.Receipt) {
		t.Fatalf("recovery changed settlement evidence")
	}
	var signedAfter queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(receiptAfter.Receipt.ReceiptJSON, &signedAfter); err != nil {
		t.Fatal(err)
	}
	if signedAfter.ArtifactIntent == nil || !reflect.DeepEqual(*signedAfter.ArtifactIntent, intentBefore) {
		t.Fatalf("recovery changed V8 ArtifactIntent: before=%+v after=%+v", intentBefore, signedAfter.ArtifactIntent)
	}
	if len(harness.connector.requests) != connectorCalls {
		t.Fatalf("recovery re-executed Business PostgreSQL: %d -> %d", connectorCalls, len(harness.connector.requests))
	}
	completed, err = harness.service.ReconcilePendingArtifacts(t.Context())
	if err != nil || completed != 0 {
		t.Fatalf("idempotent recovery pass = %d, %v; want no pending work", completed, err)
	}
	settlements, err := harness.store.ListAuditEvents(t.Context(), control.AuditFilter{
		QueryID: record.ID, EventType: "QUERY_V5_EXPOSURE_SETTLED", Limit: 10,
	})
	if err != nil || len(settlements) != 1 {
		t.Fatalf("exposure settlement events = %+v, %v", settlements, err)
	}
	consumed, err := harness.store.ListAuditEvents(t.Context(), control.AuditFilter{
		QueryID: record.ID, EventType: "QUERY_RESULT_CONSUMED", Limit: 10,
	})
	if err != nil || len(consumed) != 1 {
		t.Fatalf("consumption events after recovery = %+v, %v", consumed, err)
	}
	auditReceipt := mustCallGatewayTool(t, harness.service, harness.carol, "get_audit_receipt", map[string]any{
		"receipt_id": record.ID,
	})
	availability, ok := auditReceipt["availability_event_inclusion"].(map[string]any)
	if !ok || availability["terminal_event"] == nil || availability["checkpoint"] == nil {
		t.Fatalf("availability event inclusion helper = %#v", auditReceipt["availability_event_inclusion"])
	}
	// Exercise the same post-restart semantic-replay budget boundary as the
	// Compose acceptance path: the tenth and final allowed query must still
	// return its committed result even though settlement archives the task.
	for queryIndex := 2; queryIndex <= 10; queryIndex++ {
		replayArguments := map[string]any{
			"task_id": taskID, "request_id": fmt.Sprintf("copy-before-available-%d", queryIndex),
			"plan": map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}},
		}
		if _, err := callGatewayTool(harness.service, harness.alice, "execute_plan", replayArguments); err != nil {
			replayRecord, recordErr := harness.store.GetQueryByRequestID(t.Context(), taskID,
				replayArguments["request_id"].(string))
			replayArtifact, artifactErr := harness.store.GetResultArtifactByQuery(t.Context(), replayRecord.ID)
			replayTask, taskErr := harness.store.GetTask(t.Context(), taskID)
			t.Fatalf("final allowed semantic replay %d: %v; record=%+v/%v artifact=%+v/%v task=%+v/%v",
				queryIndex, err, replayRecord, recordErr, replayArtifact, artifactErr, replayTask, taskErr)
		}
	}
	if len(harness.connector.requests) != connectorCalls {
		t.Fatalf("semantic replay budget boundary re-executed Business PostgreSQL: %d -> %d",
			connectorCalls, len(harness.connector.requests))
	}
	archived, err := harness.store.GetTask(t.Context(), taskID)
	if err != nil || archived.State != control.TaskArchived || archived.TerminalReason != control.TerminalBudgetExhausted {
		t.Fatalf("task after final allowed query = %+v, %v", archived, err)
	}
}

func TestFailedSettlementRetryStopsAtDurableCompletedQuery(t *testing.T) {
	harness := newGatewayHarness(t)
	const taskID = "task-completed-settlement-race"
	harness.createActiveSummaryTask(t, taskID)
	result := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": taskID, "request_id": "completed-settlement-race-1", "sql": testSummarySQL,
	})
	queryID, ok := result["query_id"].(string)
	if !ok || queryID == "" {
		t.Fatalf("query id = %#v", result["query_id"])
	}

	harness.service.failQueryBudget(t.Context(), control.BudgetSettlement{
		QueryID: queryID, ErrorCode: resultFinalizationFailed,
	})
	if pending := harness.service.pendingSettles.Load(); pending != 0 {
		t.Fatalf("completed query scheduled %d failed-settlement retries", pending)
	}
	durable, err := harness.store.GetQuery(t.Context(), queryID)
	if err != nil || durable.Status != control.QueryCompleted {
		t.Fatalf("durable query after failed-settlement race = %+v, %v", durable, err)
	}
	if err := harness.service.ReadyError(); err != nil {
		t.Fatalf("completed-query race poisoned readiness: %v", err)
	}
}

type gatewayDeliveryState struct {
	artifact control.ResultArtifact
	query    control.QueryRecord
	budget   control.BudgetSnapshot
	receipt  control.QueryReceipt
}

func captureGatewayDeliveryState(t *testing.T, harness *gatewayHarness, taskID, queryID, resultID string) gatewayDeliveryState {
	t.Helper()
	artifact, err := harness.store.GetResultArtifact(t.Context(), resultID)
	if err != nil {
		t.Fatalf("get result artifact: %v", err)
	}
	query, err := harness.store.GetQuery(t.Context(), queryID)
	if err != nil {
		t.Fatalf("get query: %v", err)
	}
	budget, err := harness.store.GetBudget(t.Context(), taskID)
	if err != nil {
		t.Fatalf("get budget: %v", err)
	}
	receipt, err := harness.store.GetQueryReceipt(t.Context(), queryID)
	if err != nil {
		t.Fatalf("get query receipt: %v", err)
	}
	return gatewayDeliveryState{artifact: artifact, query: query, budget: budget, receipt: receipt}
}

type gatewayArtifactMemoryObject struct {
	body []byte
	info resultartifact.ObjectInfo
}

type gatewayArtifactMemoryBackend struct {
	mu           sync.Mutex
	objects      map[string]gatewayArtifactMemoryObject
	gets         int
	copies       int
	copyFailures int
}

func newGatewayArtifactMemoryBackend() *gatewayArtifactMemoryBackend {
	return &gatewayArtifactMemoryBackend{objects: make(map[string]gatewayArtifactMemoryObject)}
}

func (backend *gatewayArtifactMemoryBackend) Put(_ context.Context, key string, reader io.Reader, size int64,
	options resultartifact.PutOptions) (resultartifact.ObjectInfo, error) {
	body, err := io.ReadAll(reader)
	if err != nil {
		return resultartifact.ObjectInfo{}, err
	}
	if int64(len(body)) != size {
		return resultartifact.ObjectInfo{}, fmt.Errorf("put size = %d, want %d", len(body), size)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	info := resultartifact.ObjectInfo{
		Key: key, Size: size, ETag: fmt.Sprintf("etag-%d", len(body)), Metadata: cloneGatewayArtifactMetadata(options.Metadata),
		LastModified: time.Now().UTC(),
	}
	backend.objects[key] = gatewayArtifactMemoryObject{body: append([]byte(nil), body...), info: info}
	return cloneGatewayArtifactInfo(info), nil
}

func (backend *gatewayArtifactMemoryBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	object, ok := backend.objects[key]
	if !ok {
		return nil, resultartifact.ErrObjectNotFound
	}
	backend.gets++
	return io.NopCloser(bytes.NewReader(append([]byte(nil), object.body...))), nil
}

func (backend *gatewayArtifactMemoryBackend) Stat(_ context.Context, key string) (resultartifact.ObjectInfo, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	object, ok := backend.objects[key]
	if !ok {
		return resultartifact.ObjectInfo{}, resultartifact.ErrObjectNotFound
	}
	return cloneGatewayArtifactInfo(object.info), nil
}

func (backend *gatewayArtifactMemoryBackend) Copy(_ context.Context, source, destination, expectedSHA256 string) (resultartifact.ObjectInfo, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.copies++
	if backend.copyFailures > 0 {
		backend.copyFailures--
		return resultartifact.ObjectInfo{}, errors.New("injected artifact promotion failure")
	}
	object, ok := backend.objects[source]
	if !ok {
		return resultartifact.ObjectInfo{}, resultartifact.ErrObjectNotFound
	}
	if _, exists := backend.objects[destination]; exists {
		return resultartifact.ObjectInfo{}, resultartifact.ErrObjectAlreadyExists
	}
	objectDigest := sha256.Sum256(object.body)
	if hex.EncodeToString(objectDigest[:]) != expectedSHA256 {
		return resultartifact.ObjectInfo{}, fmt.Errorf("staging result object digest differs from committed evidence")
	}
	object.body = append([]byte(nil), object.body...)
	object.info = cloneGatewayArtifactInfo(object.info)
	object.info.Key = destination
	backend.objects[destination] = object
	return cloneGatewayArtifactInfo(object.info), nil
}

func (backend *gatewayArtifactMemoryBackend) operationCounts() (gets, copies int) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.gets, backend.copies
}

func (backend *gatewayArtifactMemoryBackend) List(_ context.Context, prefix, startAfter string, limit int) ([]resultartifact.ObjectInfo, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	keys := make([]string, 0, len(backend.objects))
	for key := range backend.objects {
		if strings.HasPrefix(key, prefix) && key > startAfter {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	result := make([]resultartifact.ObjectInfo, len(keys))
	for index, key := range keys {
		result[index] = cloneGatewayArtifactInfo(backend.objects[key].info)
	}
	return result, nil
}

func (backend *gatewayArtifactMemoryBackend) Delete(_ context.Context, key string) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	delete(backend.objects, key)
	return nil
}

func (backend *gatewayArtifactMemoryBackend) Ready(context.Context) error { return nil }

func (backend *gatewayArtifactMemoryBackend) getCount() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.gets
}

func cloneGatewayArtifactInfo(info resultartifact.ObjectInfo) resultartifact.ObjectInfo {
	info.Metadata = cloneGatewayArtifactMetadata(info.Metadata)
	return info
}

func cloneGatewayArtifactMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}
