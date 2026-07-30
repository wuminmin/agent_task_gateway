package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const exposureBudgetErrorCode = "EXPOSURE_BUDGET_EXHAUSTED"

type concurrencyCampaign struct {
	cfg           concurrencyConfig
	control       *pgxpool.Pool
	mcp           *mcpClient
	contenderMCPs []*mcpClient
}

func runConcurrencyCampaign(ctx context.Context, cfg concurrencyConfig, configRaw []byte) (result concurrencyReport, returnErr error) {
	monotonicStart := time.Now()
	started := monotonicStart.UTC()
	result = concurrencyReport{
		SchemaVersion: concurrencyReportSchema,
		Status:        "running",
		Acceptance:    "fail",
		StartedAt:     started,
		Provenance: reportProvenance{
			ConfigSHA256: sha256Hex(configRaw),
			SourceSHA256: sourceDigest(),
		},
		MetricNotes: map[string]string{
			"client_latency_ms":  "descriptive MCP request wall time; this supplemental axis defines no latency SLO",
			"root_lock_waiters":  "same-user Control PostgreSQL lock waiters transitively downstream of the acceptance-owned backend holding the target v4_exposure_root_heads row; that transaction holds no other conflicting lock",
			"inference_boundary": "the transitive lock chain establishes only that requests queued behind the held root row; it does not reveal which root epoch a contender read and does not prove a CAS conflict or retry",
			"gateway_replicas":   "contenders are distributed round-robin over two identical Gateway processes (eight requests per process at width 16), sharing one Catalog, Control PostgreSQL, root ledger, and production binary",
			"failure_audit":      "a complete QUERY_FAILED audit/receipt is required within lock_wait_timeout_ms after B+1 returns; success settlement/result audits are forbidden",
		},
	}
	defer func() {
		result.FinishedAt = started.Add(time.Since(monotonicStart))
	}()
	levels := make([]int, 0, len(cfg.Cases))
	dimensions := make([]string, 0, len(cfg.Cases))
	for _, one := range cfg.Cases {
		levels = append(levels, one.Concurrency)
		dimensions = append(dimensions, one.BoundaryDimension)
	}
	sort.Ints(levels)
	sort.Strings(dimensions)
	contenderURLs := normalizedGatewayURLs(cfg.Gateway.ContenderURLs)
	result.Configuration = reportConfig{
		GatewayURL: strings.TrimRight(cfg.Gateway.URL, "/"), ContenderGatewayURLs: contenderURLs,
		ContenderGatewayCount: len(contenderURLs), PerGatewayControlPool: 10, RequestTimeoutMS: cfg.RequestTimeoutMS,
		LockWaitTimeoutMS: cfg.LockWaitTimeoutMS, CaseCount: len(cfg.Cases),
		ConcurrencyLevels: uniqueInts(levels), BoundaryDimensions: uniqueStrings(dimensions),
	}
	if !validDigest(result.Provenance.SourceSHA256) {
		result.Status = "failed_preflight"
		result.Errors = append(result.Errors, "cannot determine complete source digest")
		result.Gates = evaluateConcurrencyGates(cfg, result.Cells)
		return result, errors.New(result.Errors[0])
	}
	token := strings.TrimSpace(os.Getenv(cfg.Gateway.TokenEnv))
	controlDSN := strings.TrimSpace(os.Getenv(cfg.ControlDSNEnv))
	if token == "" || controlDSN == "" {
		result.Status = "failed_preflight"
		result.Errors = append(result.Errors, "required Gateway token or Control PostgreSQL DSN environment variable is empty")
		result.Gates = evaluateConcurrencyGates(cfg, result.Cells)
		return result, errors.New(result.Errors[0])
	}
	control, err := pgxpool.New(ctx, controlDSN)
	if err != nil {
		result.Status = "failed_preflight"
		result.Errors = append(result.Errors, "open Control PostgreSQL: "+err.Error())
		result.Gates = evaluateConcurrencyGates(cfg, result.Cells)
		return result, err
	}
	defer control.Close()
	if err := control.Ping(ctx); err != nil {
		result.Status = "failed_preflight"
		result.Errors = append(result.Errors, "ping Control PostgreSQL: "+err.Error())
		result.Gates = evaluateConcurrencyGates(cfg, result.Cells)
		return result, err
	}
	newMCP := func(gatewayURL string) *mcpClient {
		return &mcpClient{url: strings.TrimRight(gatewayURL, "/") + "/mcp", token: token,
			http: &http.Client{Timeout: time.Duration(cfg.RequestTimeoutMS) * time.Millisecond}}
	}
	campaign := concurrencyCampaign{cfg: cfg, control: control, mcp: newMCP(cfg.Gateway.URL)}
	for _, gatewayURL := range contenderURLs {
		campaign.contenderMCPs = append(campaign.contenderMCPs, newMCP(gatewayURL))
	}
	for _, one := range cfg.Cases {
		cell := campaign.runCell(ctx, one)
		result.Cells = append(result.Cells, cell)
		if cell.Status != "measured" {
			result.Errors = append(result.Errors, fmt.Sprintf("case %s: %s", one.ID, cell.Error))
		}
	}
	result.Gates = evaluateConcurrencyGates(cfg, result.Cells)
	result.Acceptance = gateAcceptance(result.Gates)
	if len(result.Errors) == 0 && result.Acceptance == "pass" {
		result.Status = "complete_measured_campaign"
		return result, nil
	}
	result.Status = "complete_with_execution_errors"
	return result, errors.New("one or more concurrency cells or gates failed")
}

func (campaign *concurrencyCampaign) runCell(ctx context.Context, contract concurrencyCase) (cell concurrencyCell) {
	cell = concurrencyCell{CaseID: contract.ID, Concurrency: contract.Concurrency,
		BoundaryDimension: contract.BoundaryDimension, RootTaskSHA256: sha256Hex([]byte(contract.RootTaskID)),
		Status: "failed"}
	operationTasks := append([]string{contract.PrefixTaskID, contract.OverflowTaskID}, contract.ContenderTaskIDs...)
	for _, taskID := range operationTasks {
		cell.FamilyTaskSHA256 = append(cell.FamilyTaskSHA256, sha256Hex([]byte(taskID)))
	}
	sort.Strings(cell.FamilyTaskSHA256)
	fail := func(format string, args ...any) concurrencyCell {
		cell.Error = fmt.Sprintf(format, args...)
		return cell
	}

	shared, err := sharedRootFamily(ctx, campaign.control, contract.RootTaskID, operationTasks)
	if err != nil {
		return fail("verify root family: %v", err)
	}
	cell.Checks.SharedRootFamily = shared
	if !shared {
		return fail("configured operation tasks do not all share root %s", cell.RootTaskSHA256)
	}
	initial, err := readRootHead(ctx, campaign.control, contract.RootTaskID)
	if err != nil {
		return fail("read initial root head: %v", err)
	}
	cell.Initial = initial
	cell.Checks.FreshRoot = initial.Epoch == 0 && initial.Used.zero() &&
		initial.Limits == contract.AtBudget && initial.ReleaseSetSHA256 == "" &&
		initial.InfluenceSetSHA256 == "" && initial.OutcomeSetSHA256 == ""
	if !cell.Checks.FreshRoot {
		return fail("root is not fresh or its Catalog budget differs from at_budget")
	}

	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	prefix, err := campaign.executePlan(ctx, contract.PrefixTaskID,
		requestID(contract.ID, "prefix", 0, runID), contract.PrefixPlan)
	cell.Prefix = prefix
	if err != nil {
		return fail("B-1 prefix failed: %v", err)
	}
	before, err := readRootHead(ctx, campaign.control, contract.RootTaskID)
	if err != nil {
		return fail("read B-1 root head: %v", err)
	}
	cell.BeforeBoundary = before
	cell.Checks.BMinusOneCommitted = prefix.Status == "measured" && prefix.Charged == contract.BeforeUsed &&
		before.Used == contract.BeforeUsed && before.Limits == contract.AtBudget &&
		before.Epoch == initial.Epoch+1 && prefix.RootEpoch == before.Epoch &&
		before.Used.dimension(contract.BoundaryDimension)+1 == before.Limits.dimension(contract.BoundaryDimension)
	if !cell.Checks.BMinusOneCommitted {
		return fail("prefix did not atomically establish the configured B-1 state")
	}

	contention, responses, err := campaign.runContenders(ctx, contract, runID)
	cell.Contention = contention
	if err != nil {
		return fail("contention batch: %v", err)
	}
	at, err := readRootHead(ctx, campaign.control, contract.RootTaskID)
	if err != nil {
		return fail("read B root head: %v", err)
	}
	cell.AtBoundary = at
	delta := contract.AtBudget.subtract(contract.BeforeUsed)
	cell.Checks.BCommitted = at.Used == contract.AtBudget && at.Limits == contract.AtBudget &&
		at.Epoch == before.Epoch+1 && contention.TotalCharged == delta &&
		contention.SuccessfulRequests == contract.Concurrency && contention.FailedRequests == 0
	cell.Checks.ThreeDimensionalAtomic = cell.Checks.BCommitted && contention.ChargedWinners == 1 &&
		contention.ZeroNoveltySettlements == contract.Concurrency-1 && sameIdentity(responses) &&
		allEpoch(responses, at.Epoch)
	cell.Checks.RootLockQueueObserved = contention.RootLockWaitersObserved >= contract.Concurrency
	if !cell.Checks.BCommitted || !cell.Checks.ThreeDimensionalAtomic || !cell.Checks.RootLockQueueObserved {
		return fail("contention did not establish one exact three-dimensional transition with a fully observed root-lock queue")
	}

	contentBefore, err := readContentCounts(ctx, campaign.control)
	if err != nil {
		return fail("read pre-overflow content counts: %v", err)
	}
	overflowRequestID := requestID(contract.ID, "overflow", 0, runID)
	overflow, err := campaign.executeOverflow(ctx, contract.OverflowTaskID, overflowRequestID,
		contract.OverflowPlan, contentBefore)
	cell.Overflow = overflow
	if err != nil {
		return fail("B+1 inspection failed: %v", err)
	}
	after, err := readRootHead(ctx, campaign.control, contract.RootTaskID)
	if err != nil {
		return fail("read post-overflow root head: %v", err)
	}
	cell.AfterRejectedOverflow = after
	cell.Checks.OverflowRejected = overflow.Status == "rejected" &&
		overflow.ObservedErrorCode == exposureBudgetErrorCode && after == at
	cell.Checks.FailureLeftNoPartialCommit = overflow.QueryStatus == "FAILED" &&
		overflow.ExposureReservationStatus == "RELEASED" && overflow.QueryResultSHA256 == "" &&
		overflow.EncryptedResults == 0 && overflow.EncryptedResultChunks == 0 &&
		overflow.Materializations == 0 && overflow.QueryObservations == 0 &&
		overflow.RootObservations == 0 && overflow.TerminalSuccessAudits == 0 &&
		overflow.TerminalFailureAudits == 1 && overflow.Receipts == 1 &&
		overflow.ContentBefore == overflow.ContentAfter && after == at
	if !cell.Checks.OverflowRejected || !cell.Checks.FailureLeftNoPartialCommit {
		return fail("B+1 was not fail-closed without partial success state")
	}
	cell.Status = "measured"
	cell.Error = ""
	return cell
}

func (campaign *concurrencyCampaign) executePlan(ctx context.Context, taskID, requestID string,
	plan json.RawMessage) (prefixEvidence, error) {
	return campaign.executePlanWith(ctx, campaign.mcp, taskID, requestID, plan)
}

func (campaign *concurrencyCampaign) executePlanWith(ctx context.Context, client *mcpClient,
	taskID, requestID string, plan json.RawMessage) (prefixEvidence, error) {
	result := prefixEvidence{Status: "failed"}
	var decoded any
	if err := decodePlan(plan, &decoded); err != nil {
		return result, err
	}
	started := time.Now()
	var response executeResponse
	err := client.call(ctx, "execute_plan", map[string]any{
		"task_id": taskID, "request_id": requestID, "plan": decoded,
	}, &response)
	result.LatencyMS = durationMS(time.Since(started))
	if err != nil {
		return result, err
	}
	if err := validateResponse(response); err != nil {
		return result, err
	}
	digest, err := resultDigest(response.Rows)
	if err != nil {
		return result, err
	}
	result.Status = "measured"
	result.ObservationSHA = response.Exposure.ObservationSHA256
	result.Actual = response.Exposure.actual()
	result.Charged = response.Exposure.charged()
	result.RootEpoch = response.Exposure.RootEpoch
	result.ResultSHA256 = digest
	return result, nil
}

type contenderResult struct {
	response prefixEvidence
	err      error
}

func (campaign *concurrencyCampaign) runContenders(ctx context.Context, contract concurrencyCase,
	runID string) (contentionEvidence, []prefixEvidence, error) {
	evidence := contentionEvidence{Status: "failed"}
	blocker, err := campaign.control.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return evidence, nil, err
	}
	blockerOpen := true
	defer func() {
		if blockerOpen {
			_ = blocker.Rollback(context.Background())
		}
	}()
	var blockerPID int32
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		return evidence, nil, err
	}
	var lockedRoot string
	if err := blocker.QueryRow(ctx, `SELECT root_task_id FROM v4_exposure_root_heads
WHERE root_task_id=$1 FOR UPDATE`, contract.RootTaskID).Scan(&lockedRoot); err != nil {
		return evidence, nil, err
	}
	start := make(chan struct{})
	results := make(chan contenderResult, contract.Concurrency)
	var wait sync.WaitGroup
	for index, taskID := range contract.ContenderTaskIDs {
		client := campaign.contenderMCPs[index%len(campaign.contenderMCPs)]
		wait.Add(1)
		go func(index int, taskID string, client *mcpClient) {
			defer wait.Done()
			<-start
			response, callErr := campaign.executePlanWith(ctx, client, taskID,
				requestID(contract.ID, "contender", index+1, runID), contract.ContenderPlan)
			results <- contenderResult{response: response, err: callErr}
		}(index, taskID, client)
	}
	close(start)
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(campaign.cfg.LockWaitTimeoutMS)*time.Millisecond)
	waiters, waitErr := waitForRootLockWaiters(waitCtx, campaign.control, blockerPID, contract.Concurrency)
	cancel()
	evidence.RootLockWaitersObserved = waiters
	if commitErr := blocker.Commit(ctx); commitErr != nil && waitErr == nil {
		waitErr = commitErr
	}
	blockerOpen = false
	wait.Wait()
	close(results)
	responses := make([]prefixEvidence, 0, contract.Concurrency)
	for one := range results {
		if one.err != nil {
			evidence.FailedRequests++
			continue
		}
		evidence.SuccessfulRequests++
		responses = append(responses, one.response)
		evidence.TotalCharged = evidence.TotalCharged.add(one.response.Charged)
		evidence.RootEpochs = append(evidence.RootEpochs, one.response.RootEpoch)
		evidence.ObservationSHA256 = append(evidence.ObservationSHA256, one.response.ObservationSHA)
		evidence.ResultSHA256 = append(evidence.ResultSHA256, one.response.ResultSHA256)
		evidence.ClientLatencyMS = append(evidence.ClientLatencyMS, one.response.LatencyMS)
		if one.response.Charged.zero() {
			evidence.ZeroNoveltySettlements++
		} else {
			evidence.ChargedWinners++
		}
	}
	sort.Slice(evidence.ClientLatencyMS, func(i, j int) bool { return evidence.ClientLatencyMS[i] < evidence.ClientLatencyMS[j] })
	sort.Slice(evidence.RootEpochs, func(i, j int) bool { return evidence.RootEpochs[i] < evidence.RootEpochs[j] })
	sort.Strings(evidence.ObservationSHA256)
	sort.Strings(evidence.ResultSHA256)
	if waitErr != nil {
		return evidence, responses, waitErr
	}
	if evidence.FailedRequests != 0 {
		return evidence, responses, fmt.Errorf("%d contender requests failed", evidence.FailedRequests)
	}
	evidence.Status = "measured"
	return evidence, responses, nil
}

func waitForRootLockWaiters(ctx context.Context, control *pgxpool.Pool, blockerPID int32, required int) (int, error) {
	maxObserved := 0
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		err := control.QueryRow(ctx, `WITH RECURSIVE downstream(pid) AS (
 SELECT pid FROM pg_stat_activity
 WHERE datname=current_database() AND usename=current_user AND state='active'
   AND wait_event_type='Lock' AND $1 = ANY(pg_blocking_pids(pid))
 UNION
 SELECT activity.pid FROM pg_stat_activity activity
 JOIN downstream blocker ON blocker.pid = ANY(pg_blocking_pids(activity.pid))
 WHERE activity.datname=current_database() AND activity.usename=current_user
   AND activity.state='active' AND activity.wait_event_type='Lock'
)
SELECT count(*) FROM downstream`, blockerPID).Scan(&waiting)
		if err != nil {
			return maxObserved, err
		}
		if waiting > maxObserved {
			maxObserved = waiting
		}
		if waiting >= required {
			return maxObserved, nil
		}
		select {
		case <-ctx.Done():
			return maxObserved, fmt.Errorf("observed %d/%d root-lock waiters: %w", maxObserved, required, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (campaign *concurrencyCampaign) executeOverflow(ctx context.Context, taskID, requestID string,
	plan json.RawMessage, before contentCounts) (overflowEvidence, error) {
	result := overflowEvidence{Status: "failed", ExpectedErrorCode: exposureBudgetErrorCode, ContentBefore: before}
	var decoded any
	if err := decodePlan(plan, &decoded); err != nil {
		return result, err
	}
	started := time.Now()
	var ignored executeResponse
	err := campaign.mcp.call(ctx, "execute_plan", map[string]any{
		"task_id": taskID, "request_id": requestID, "plan": decoded,
	}, &ignored)
	result.LatencyMS = durationMS(time.Since(started))
	var toolErr *toolCallError
	if !errors.As(err, &toolErr) {
		if err == nil {
			return result, errors.New("B+1 request unexpectedly released a result")
		}
		return result, fmt.Errorf("B+1 returned non-tool error: %w", err)
	}
	result.ObservedErrorCode = toolErr.Code
	var queryID string
	readTerminal := func() error {
		return campaign.control.QueryRow(ctx, `SELECT id,status,result_sha256 FROM query_records
WHERE task_id=$1 AND request_id=$2`, taskID, requestID).Scan(&queryID, &result.QueryStatus, &result.QueryResultSHA256)
	}
	err = readTerminal()
	if err != nil {
		return result, fmt.Errorf("read failed query: %w", err)
	}
	deadline := time.Now().Add(time.Duration(campaign.cfg.LockWaitTimeoutMS) * time.Millisecond)
	for result.QueryStatus == "RESERVED" && time.Now().Before(deadline) {
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
		if err := readTerminal(); err != nil {
			return result, fmt.Errorf("poll failed query terminal state: %w", err)
		}
	}
	err = campaign.control.QueryRow(ctx, `SELECT
 (SELECT status FROM v4_query_exposure_reservations WHERE query_id=$1),
 (SELECT count(*) FROM encrypted_query_results WHERE query_id=$1),
 (SELECT count(*) FROM encrypted_query_result_chunks WHERE query_id=$1),
 (SELECT count(*) FROM v4_committed_materializations WHERE source_query_id=$1),
 (SELECT count(*) FROM v4_query_observations WHERE query_id=$1),
 (SELECT count(*) FROM v4_root_observations WHERE first_query_id=$1),
 (SELECT count(*) FROM audit_events WHERE query_id=$1 AND event_type IN
   ('QUERY_COMPLETED','QUERY_RESULT_STORED','QUERY_ORDINAL_EXPOSURE_SETTLED')),
 (SELECT count(*) FROM audit_events WHERE query_id=$1 AND event_type='QUERY_FAILED'),
 (SELECT count(*) FROM query_receipts WHERE query_id=$1)`, queryID).Scan(
		&result.ExposureReservationStatus, &result.EncryptedResults, &result.EncryptedResultChunks,
		&result.Materializations, &result.QueryObservations,
		&result.RootObservations, &result.TerminalSuccessAudits, &result.TerminalFailureAudits,
		&result.Receipts)
	if err != nil {
		return result, fmt.Errorf("inspect failed query atomicity: %w", err)
	}
	result.ContentAfter, err = readContentCounts(ctx, campaign.control)
	if err != nil {
		return result, err
	}
	if toolErr.Code == exposureBudgetErrorCode {
		result.Status = "rejected"
	}
	return result, nil
}

func sharedRootFamily(ctx context.Context, control *pgxpool.Pool, rootTaskID string, taskIDs []string) (bool, error) {
	rows, err := control.Query(ctx, `SELECT id,root_task_id FROM tasks WHERE id=ANY($1::text[]) ORDER BY id`, taskIDs)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	seen := make(map[string]struct{}, len(taskIDs))
	for rows.Next() {
		var taskID, rootID string
		if err := rows.Scan(&taskID, &rootID); err != nil {
			return false, err
		}
		if rootID != rootTaskID {
			return false, nil
		}
		seen[taskID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return len(seen) == len(taskIDs), nil
}

func readRootHead(ctx context.Context, control *pgxpool.Pool, rootTaskID string) (rootHeadEvidence, error) {
	var result rootHeadEvidence
	err := control.QueryRow(ctx, `SELECT epoch,max_release_facts,max_influence_facts,max_outcome_facts,
 used_release_facts,used_influence_facts,used_outcome_facts,COALESCE(release_set_sha256,''),
 COALESCE(influence_set_sha256,''),COALESCE(outcome_set_sha256,'')
FROM v4_exposure_root_heads WHERE root_task_id=$1`, rootTaskID).Scan(&result.Epoch,
		&result.Limits.Release, &result.Limits.Influence, &result.Limits.Outcome,
		&result.Used.Release, &result.Used.Influence, &result.Used.Outcome,
		&result.ReleaseSetSHA256, &result.InfluenceSetSHA256, &result.OutcomeSetSHA256)
	return result, err
}

func readContentCounts(ctx context.Context, control *pgxpool.Pool) (contentCounts, error) {
	var result contentCounts
	err := control.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM v4_bitmap_containers),
 (SELECT count(*) FROM v4_bitmap_sets),
 (SELECT count(*) FROM v4_dynamic_facts),
 (SELECT count(*) FROM v4_observations)`).Scan(&result.Containers, &result.Sets,
		&result.DynamicFacts, &result.Observations)
	return result, err
}

func validateResponse(response executeResponse) error {
	if response.SemanticReplay {
		return errors.New("concurrency operation unexpectedly used semantic replay")
	}
	value := response.Exposure
	if value.ProfileVersion != "taskgate-exposure-v4" || value.RootEpoch <= 0 {
		return errors.New("response omitted a committed V4 exposure epoch")
	}
	for name, digest := range map[string]string{"observation": value.ObservationSHA256,
		"dictionary_set": value.DictionarySetDigest, "release_set": value.ReleaseSetSHA256,
		"influence_set": value.InfluenceSetSHA256, "outcome_set": value.OutcomeSetSHA256} {
		if !validDigest(digest) {
			return fmt.Errorf("response has invalid %s digest", name)
		}
	}
	actual, charged := value.actual(), value.charged()
	if actual.Release < 0 || actual.Influence < 0 || actual.Outcome != 1 ||
		charged.Release < 0 || charged.Influence < 0 || charged.Outcome < 0 ||
		charged.Release > actual.Release || charged.Influence > actual.Influence || charged.Outcome > actual.Outcome {
		return fmt.Errorf("invalid exposure cardinalities: actual=%+v charged=%+v", actual, charged)
	}
	return nil
}

func sameIdentity(values []prefixEvidence) bool {
	if len(values) == 0 {
		return false
	}
	first := values[0]
	for _, value := range values[1:] {
		if value.ObservationSHA != first.ObservationSHA || value.Actual != first.Actual ||
			value.ResultSHA256 != first.ResultSHA256 {
			return false
		}
	}
	return true
}

func allEpoch(values []prefixEvidence, epoch int64) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if value.RootEpoch != epoch {
			return false
		}
	}
	return true
}

func decodePlan(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func requestID(caseID, phase string, index int, runID string) string {
	base := safeID(caseID)
	if len(base) > 48 {
		base = base[:48]
	}
	return fmt.Sprintf("v4cas-%s-%s-%02d-%s", base, phase, index, runID)
}

func safeID(value string) string {
	var result strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' {
			result.WriteRune(char)
		} else {
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-")
}

func resultDigest(rows [][]any) (string, error) {
	canonicalRows := make([]string, 0, len(rows))
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			return "", err
		}
		canonicalRows = append(canonicalRows, string(raw))
	}
	sort.Strings(canonicalRows)
	raw, err := json.Marshal(canonicalRows)
	if err != nil {
		return "", err
	}
	return sha256Hex(raw), nil
}

func durationMS(value time.Duration) float64 {
	result := float64(value.Nanoseconds()) / float64(time.Millisecond)
	if math.IsNaN(result) || math.IsInf(result, 0) || result < 0 {
		return 0
	}
	return result
}

func uniqueInts(values []int) []int {
	result := make([]int, 0, len(values))
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func normalizedGatewayURLs(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, strings.TrimRight(strings.TrimSpace(value), "/"))
	}
	return result
}
