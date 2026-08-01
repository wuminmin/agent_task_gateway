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
		t.Fatalf("canonical object creation did not establish consumption: %+v", before.artifact)
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
	if !reflect.DeepEqual(budgetAfter, budgetBefore) || !reflect.DeepEqual(receiptAfter, receiptBefore) {
		t.Fatalf("recovery changed settlement evidence\nbudget: %+v -> %+v\nreceipt: %+v -> %+v",
			budgetBefore, budgetAfter, receiptBefore, receiptAfter)
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
