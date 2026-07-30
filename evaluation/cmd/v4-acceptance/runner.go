package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var embeddedSourceDigest string

var sourceDigestRoots = []string{
	"cmd/gateway",
	"cmd/oa-demo",
	"cmd/snapshot-index",
	"cmd/snapshot-sidecar-install",
	"db/init",
	"evaluation/cmd/v4-acceptance",
	"evaluation/cmd/v4-compose-observer",
	"evaluation/cmd/v4-offline",
	"evaluation/cmd/exposure-bench",
	"internal/gateway",
	"internal/control",
	"internal/catalog",
	"internal/ordinal",
	"internal/dataconnector",
	"internal/exposure",
	"internal/queryplan",
	"internal/queryreceipt",
	"internal/semanticcache",
	"internal/snapshotbundle",
	"internal/sqlpolicy",
}

var sourceDigestFiles = []string{
	".dockerignore",
	"Dockerfile",
	"compose.yaml",
	"config/snapshots/expense-detail-v1.json",
	"config/snapshots/expense-summary-v1.json",
	"db/control-init/10-control-role.sh",
	"go.mod",
	"go.sum",
	"evaluation/Dockerfile",
	"evaluation/exposure-performance/compose.yaml",
	"evaluation/run-exposure-performance.sh",
	"evaluation/v4-acceptance/README.md",
	"evaluation/v4-acceptance/compose.full.yaml",
	"evaluation/v4-acceptance/compose.observer.yaml",
	"evaluation/v4-acceptance/compose.scale-narrow.yaml",
	"evaluation/v4-acceptance/compose.small-regression.yaml",
	"evaluation/exposure-scale/05-scale-data.sql",
	"evaluation/v4-acceptance/full-matrix.template.json",
	"evaluation/v4-acceptance/observer/09-pg-stat-statements.sql",
	"evaluation/v4-acceptance/provision-full.sh",
	"evaluation/v4-acceptance/scale-fixture/06-freeze-scale-publication.sql",
	"evaluation/v4-acceptance/scale-fixture/10-scale-reader.sh",
	"evaluation/v4-acceptance/scale-fixture/catalog-full.yaml",
	"evaluation/v4-acceptance/scale-fixture/snapshots/scale-lineitem-v4-narrow-1.json",
	"evaluation/v4-acceptance/scale-fixture/snapshots/scale-orders-v4-narrow-1.json",
	"evaluation/v4-acceptance/small-regression/catalog.yaml",
}

type campaign struct {
	cfg      config
	report   report
	business *pgxpool.Pool
	control  *pgxpool.Pool
	mcp      *mcpClient
	observer *observerRunner
}

func runCampaign(ctx context.Context, cfg config, configRaw []byte, outputDir string) (report, error) {
	started := time.Now().UTC()
	result := report{
		SchemaVersion: reportSchema,
		Status:        "running",
		Acceptance:    "incomplete",
		StartedAt:     started,
		Samples:       make([]sample, 0),
		Summaries:     make([]summary, 0),
		Provenance: provenance{
			ConfigSHA256: sha256Hex(configRaw),
			SourceSHA256: sourceDigest(),
		},
		MetricSemantics: metricSemantics(),
	}
	requireFresh := true
	if cfg.RequireFreshRoot != nil {
		requireFresh = *cfg.RequireFreshRoot
	}
	var shapes []string
	var overlaps []float64
	for _, one := range cfg.Cases {
		shapes = append(shapes, one.Shape)
		overlaps = append(overlaps, one.TargetOverlapPercent)
	}
	sort.Float64s(overlaps)
	result.Configuration = reportConfig{
		GatewayURL:                   sanitizeURL(cfg.Gateway.URL),
		RequestTimeoutMS:             cfg.RequestTimeoutMS,
		StatementTimeoutMS:           cfg.StatementTimeoutMS,
		OverlapTolerancePoint:        cfg.OverlapTolerancePoint,
		RequireFreshRoot:             requireFresh,
		CaseCount:                    len(cfg.Cases),
		TrialCount:                   trialCount(cfg.Cases),
		ConfiguredShapes:             uniqueStrings(shapes),
		ConfiguredOverlapPercentages: uniqueFloats(overlaps),
	}
	result.Environment = measureEnvironment(cfg.EnvironmentManifest)
	if result.Environment.Status == "measured" {
		result.Provenance.EnvironmentSHA256 = result.Environment.SHA256
	}

	// Offline measurements are intentionally independent from service startup.
	result.IndexBuild = measureCommandPhase(ctx, "index_build", cfg.IndexBuild, filepath.Join(outputDir, "index-build"))
	receiptPath := filepath.Join(outputDir, "activation-verification-receipt.json")
	verificationMetric := replaceCommandMetricToken(cfg.ActivationVerification, "{{verification_receipt}}", receiptPath)
	result.ActivationVerification = measureCommandPhase(ctx, "activation_verification", verificationMetric,
		filepath.Join(outputDir, "activation-verification"))
	receiptSHA256 := ""
	if cfg.ActivationVerification != nil && result.ActivationVerification.Status == "measured" {
		receiptRaw, receiptErr := readBoundedEvidenceFile(receiptPath, 4<<20)
		if receiptErr != nil {
			result.ActivationVerification.Status = "failed"
			result.ActivationVerification.Reason = "strict verification did not produce a bounded receipt: " + receiptErr.Error()
		} else {
			receiptSHA256 = sha256Hex(receiptRaw)
			result.Provenance.ActivationVerificationReceiptSHA256 = receiptSHA256
		}
	}
	activationMetric := replaceCommandMetricToken(cfg.Activation, "{{verification_receipt}}", receiptPath)
	activationMetric = replaceCommandMetricToken(activationMetric, "{{verification_receipt_sha256}}", receiptSHA256)
	if cfg.Activation != nil && cfg.Activation.WarmVerified && result.ActivationVerification.Status != "measured" {
		result.Activation = phaseMeasurement{Status: "failed",
			Reason: "warm activation was not run because strict publication verification failed"}
	} else {
		result.Activation = measureCommandPhase(ctx, "activation", activationMetric, filepath.Join(outputDir, "activation"))
	}
	result.Artifacts = measureArtifacts(cfg.Artifacts.TotalPaths, cfg.Artifacts.HotPaths)
	if result.IndexBuild.Status == "measured" {
		for _, run := range result.IndexBuild.Runs {
			if run.ArtifactBytes != nil && (result.Artifacts.TotalBytes == nil || *run.ArtifactBytes > *result.Artifacts.TotalBytes) {
				value := *run.ArtifactBytes
				result.Artifacts.TotalBytes = &value
			}
			if run.HotArtifactBytes != nil && (result.Artifacts.HotBytes == nil || *run.HotArtifactBytes > *result.Artifacts.HotBytes) {
				value := *run.HotArtifactBytes
				result.Artifacts.HotBytes = &value
			}
		}
		if result.Artifacts.TotalBytes != nil && result.Artifacts.HotBytes != nil {
			result.Artifacts.Status, result.Artifacts.Reason = "measured", ""
		}
	}

	token := os.Getenv(cfg.Gateway.TokenEnv)
	businessDSN := os.Getenv(cfg.BusinessDSNEnv)
	controlDSN := os.Getenv(cfg.ControlDSNEnv)
	if token == "" || businessDSN == "" || controlDSN == "" {
		missing := make([]string, 0, 3)
		for name, value := range map[string]string{cfg.Gateway.TokenEnv: token, cfg.BusinessDSNEnv: businessDSN, cfg.ControlDSNEnv: controlDSN} {
			if value == "" {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		result.Status = "failed_preflight"
		result.Errors = append(result.Errors, "missing required environment variables: "+strings.Join(missing, ", "))
		result.Storage = storageMeasurement{Status: "unmeasured", Reason: "PostgreSQL DSNs were unavailable"}
		result.Coverage = buildCoverage(cfg, result.Samples)
		result.Gates = evaluateGates(cfg, result)
		result.Acceptance = acceptanceStatus(result.Gates)
		return result, errors.New(result.Errors[0])
	}

	business, err := pgxpool.New(ctx, businessDSN)
	if err != nil {
		result.Status = "failed_preflight"
		result.Errors = append(result.Errors, "open Business PostgreSQL: "+err.Error())
		result.Storage = storageMeasurement{Status: "unmeasured", Reason: "Business PostgreSQL was unavailable"}
		result.Coverage = buildCoverage(cfg, result.Samples)
		result.Gates = evaluateGates(cfg, result)
		result.Acceptance = acceptanceStatus(result.Gates)
		return result, err
	}
	defer business.Close()
	control, err := pgxpool.New(ctx, controlDSN)
	if err != nil {
		result.Status = "failed_preflight"
		result.Errors = append(result.Errors, "open Control PostgreSQL: "+err.Error())
		result.Storage = storageMeasurement{Status: "unmeasured", Reason: "Control PostgreSQL was unavailable"}
		result.Coverage = buildCoverage(cfg, result.Samples)
		result.Gates = evaluateGates(cfg, result)
		result.Acceptance = acceptanceStatus(result.Gates)
		return result, err
	}
	defer control.Close()
	if err := business.Ping(ctx); err != nil {
		return failedPreflight(result, cfg, "Business PostgreSQL ping: "+err.Error())
	}
	if err := control.Ping(ctx); err != nil {
		return failedPreflight(result, cfg, "Control PostgreSQL ping: "+err.Error())
	}

	observer, observerErr := newObserverRunner(cfg.Observer)
	if observerErr != nil {
		if cfg.Observer != nil && cfg.Observer.Required {
			return failedPreflight(result, cfg, "observer: "+observerErr.Error())
		}
		result.Warnings = append(result.Warnings, "optional observer unavailable: "+observerErr.Error())
	}
	if observer != nil {
		result.Provenance.ObserverExecutableSHA256 = observer.executableSHA256
	}
	c := &campaign{cfg: cfg, report: result, business: business, control: control, observer: observer,
		mcp: &mcpClient{url: strings.TrimRight(cfg.Gateway.URL, "/") + "/mcp", token: token,
			http: &http.Client{Timeout: time.Duration(cfg.RequestTimeoutMS) * time.Millisecond}}}
	c.runWorkloads(ctx, requireFresh)
	c.report.Storage = measureStorage(ctx, control, c.report.Artifacts)
	c.report.Summaries = summarizeSamples(c.report.Samples)
	c.report.Coverage = buildCoverage(cfg, c.report.Samples)
	c.report.Gates = evaluateGates(cfg, c.report)
	c.report.Acceptance = acceptanceStatus(c.report.Gates)
	if len(c.report.Errors) > 0 {
		c.report.Status = "complete_with_execution_errors"
	} else {
		c.report.Status = "complete_measured_campaign"
	}
	return c.report, nil
}

func replaceCommandMetricToken(metric *commandMetric, token, value string) *commandMetric {
	if metric == nil {
		return nil
	}
	result := *metric
	result.Argv = append([]string(nil), metric.Argv...)
	for index := range result.Argv {
		result.Argv[index] = strings.ReplaceAll(result.Argv[index], token, value)
	}
	return &result
}

func readBoundedEvidenceFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maximum {
		return nil, errors.New("evidence path is not a bounded regular file")
	}
	return os.ReadFile(path)
}

func failedPreflight(result report, cfg config, message string) (report, error) {
	result.Status = "failed_preflight"
	result.Errors = append(result.Errors, message)
	result.Storage = storageMeasurement{Status: "unmeasured", Reason: message}
	result.Coverage = buildCoverage(cfg, result.Samples)
	result.Gates = evaluateGates(cfg, result)
	result.Acceptance = acceptanceStatus(result.Gates)
	return result, errors.New(message)
}

func (c *campaign) runWorkloads(ctx context.Context, requireFresh bool) {
	for _, one := range c.cfg.Cases {
		dimension := one.OverlapDimension
		if dimension == "" {
			dimension = "influence"
		}
		for trialIndex, taskID := range one.TaskIDs {
			trial := trialIndex + 1
			if requireFresh {
				fresh, epoch, err := freshTaskRoot(ctx, c.control, taskID)
				if err != nil || !fresh {
					reason := "root freshness check failed"
					if err != nil {
						reason += ": " + err.Error()
					} else {
						reason += fmt.Sprintf(": epoch=%d", epoch)
					}
					c.recordFailure(one, dimension, trial, taskID, "preflight", reason)
					continue
				}
			}
			direct := c.measureDirect(ctx, one, dimension, trial, taskID)
			c.report.Samples = append(c.report.Samples, direct)
			if direct.Status != "measured" {
				continue
			}
			setupOK := true
			for setupIndex, plan := range one.SetupPlans {
				setup := c.measureGateway(ctx, one, dimension, trial, taskID,
					fmt.Sprintf("setup_%d", setupIndex+1), plan)
				c.report.Samples = append(c.report.Samples, setup)
				if setup.Status != "measured" || setup.SemanticReplay {
					if setup.SemanticReplay && setup.Error == "" {
						setup.Status = "failed"
						setup.Error = "setup operation unexpectedly used semantic replay"
						c.report.Samples[len(c.report.Samples)-1] = setup
						c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d setup_%d: %s", one.ID, trial, setupIndex+1, setup.Error))
					}
					setupOK = false
					break
				}
			}
			if !setupOK {
				continue
			}
			novel := c.measureGateway(ctx, one, dimension, trial, taskID, "novel", one.Plan)
			if direct.ResultSHA256 != "" && novel.ResultSHA256 != "" && direct.ResultSHA256 != novel.ResultSHA256 {
				novel.Status = "failed"
				novel.Error = "direct SQL and Gateway visible result digests differ"
				c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d novel: %s", one.ID, trial, novel.Error))
			}
			if novel.Exposure != nil {
				novel.ObservedOverlapPercent = overlapPercent(*novel.Exposure, dimension)
			}
			if novel.SemanticReplay {
				novel.Status = "failed"
				novel.Error = "measured novel operation unexpectedly used semantic replay"
				c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d novel: %s", one.ID, trial, novel.Error))
			}
			c.report.Samples = append(c.report.Samples, novel)
			if novel.Status != "measured" {
				continue
			}
			replay := c.measureGateway(ctx, one, dimension, trial, taskID, "semantic_replay", one.Plan)
			if !replay.SemanticReplay {
				replay.Status = "failed"
				replay.Error = "distinct-request replay did not report semantic_replay=true"
				c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d replay: %s", one.ID, trial, replay.Error))
			}
			if replay.Exposure != nil {
				replay.ObservedOverlapPercent = overlapPercent(*replay.Exposure, dimension)
				if novel.Exposure != nil && (!sameObservationIdentity(*novel.Exposure, *replay.Exposure) ||
					replay.Exposure.ChargedReleaseFacts != 0 || replay.Exposure.ChargedInfluenceFacts != 0 ||
					replay.Exposure.ChargedOutcomeFacts != 0 || replay.ResultSHA256 != novel.ResultSHA256) {
					replay.Status = "failed"
					replay.Error = "semantic replay changed result/observation identity or charged novelty"
					c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d replay: %s", one.ID, trial, replay.Error))
				}
			}
			c.report.Samples = append(c.report.Samples, replay)
		}
	}
}

func (c *campaign) recordFailure(one workloadCase, dimension string, trial int, taskID, phase, reason string) {
	c.report.Samples = append(c.report.Samples, sample{CaseID: one.ID, Shape: one.Shape,
		TargetOverlapPercent: one.TargetOverlapPercent, OverlapDimension: dimension, Trial: trial,
		TaskSHA256: hashTask(taskID), Phase: phase, Status: "failed", Error: reason,
		WAL:      walMeasurement{Status: "unmeasured", Reason: "operation did not run"},
		Observer: observerDelta{Status: "unmeasured", Reason: "operation did not run"}})
	c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d %s: %s", one.ID, trial, phase, reason))
}

func (c *campaign) measureDirect(ctx context.Context, one workloadCase, dimension string, trial int, taskID string) sample {
	result := c.baseSample(one, dimension, trial, taskID, "direct_sql")
	telemetry := c.beforeTelemetry(ctx, one.ID, "direct_sql", trial)
	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.StatementTimeoutMS)*time.Millisecond)
	defer cancel()
	started := time.Now()
	tx, err := c.business.BeginTx(queryCtx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err == nil {
		_, err = tx.Exec(queryCtx, `SELECT set_config('statement_timeout',$1,true)`, fmt.Sprintf("%dms", c.cfg.StatementTimeoutMS))
	}
	var rows pgx.Rows
	if err == nil {
		rows, err = tx.Query(queryCtx, one.DirectSQL, one.DirectArgs...)
	}
	var directRows [][]any
	if err == nil {
		for rows.Next() {
			values, valuesErr := rows.Values()
			if valuesErr != nil {
				err = valuesErr
				break
			}
			directRows = append(directRows, append([]any(nil), values...))
			result.RowCount++
		}
		if rows.Err() != nil {
			err = rows.Err()
		}
		rows.Close()
	}
	if tx != nil {
		if err == nil {
			err = tx.Commit(queryCtx)
		} else {
			_ = tx.Rollback(context.Background())
		}
	}
	result.ClientLatencyMS = durationMS(time.Since(started))
	result.DatabaseMS = result.ClientLatencyMS
	result.WAL, result.Observer = c.afterTelemetry(ctx, telemetry, one.ID, "direct_sql", trial)
	if err != nil {
		result.Status, result.Error = "failed", err.Error()
		c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d direct_sql: %v", one.ID, trial, err))
		return result
	}
	if one.Expected.RowCount != nil && result.RowCount != *one.Expected.RowCount {
		result.Status = "failed"
		result.Error = fmt.Sprintf("direct row_count=%d, want %d", result.RowCount, *one.Expected.RowCount)
		c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d: %s", one.ID, trial, result.Error))
	}
	if digest, digestErr := resultDigest(directRows); digestErr != nil {
		result.Status, result.Error = "failed", "direct result encoding: "+digestErr.Error()
		c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d: %s", one.ID, trial, result.Error))
	} else {
		result.ResultSHA256 = digest
	}
	return result
}

func (c *campaign) measureGateway(ctx context.Context, one workloadCase, dimension string, trial int,
	taskID, phase string, plan json.RawMessage) sample {
	result := c.baseSample(one, dimension, trial, taskID, phase)
	var decoded any
	if err := json.Unmarshal(plan, &decoded); err != nil {
		result.Status, result.Error = "failed", err.Error()
		return result
	}
	requestID := fmt.Sprintf("v4eval-%s-%d-%s-%d", safeID(one.ID), trial, safeID(phase), time.Now().UnixNano())
	telemetry := c.beforeTelemetry(ctx, one.ID, phase, trial)
	started := time.Now()
	var response executeResponse
	err := c.mcp.call(ctx, "execute_plan", map[string]any{"task_id": taskID, "request_id": requestID, "plan": decoded}, &response)
	result.ClientLatencyMS = durationMS(time.Since(started))
	result.WAL, result.Observer = c.afterTelemetry(ctx, telemetry, one.ID, phase, trial)
	if err != nil {
		result.Status, result.Error = "failed", err.Error()
		c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d %s: %v", one.ID, trial, phase, err))
		return result
	}
	result.DatabaseMS = response.DatabaseMS
	result.ComponentMS = response.ComponentMS
	result.RowCount = response.RowCount
	if digest, digestErr := resultDigest(response.Rows); digestErr != nil {
		result.Status, result.Error = "failed", "Gateway result encoding: "+digestErr.Error()
		c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d %s: %v", one.ID, trial, phase, digestErr))
	} else {
		result.ResultSHA256 = digest
	}
	result.SemanticReplay = response.SemanticReplay
	result.Exposure = &response.Exposure
	if err := validateV4Response(one, phase, response); err != nil {
		result.Status, result.Error = "failed", err.Error()
		c.report.Errors = append(c.report.Errors, fmt.Sprintf("%s trial %d %s: %v", one.ID, trial, phase, err))
	}
	return result
}

func (c *campaign) baseSample(one workloadCase, dimension string, trial int, taskID, phase string) sample {
	return sample{CaseID: one.ID, Shape: one.Shape, TargetOverlapPercent: one.TargetOverlapPercent,
		OverlapDimension: dimension, Trial: trial, TaskSHA256: hashTask(taskID), SmallQuery: one.SmallQuery,
		Phase: phase, Status: "measured",
		WAL:      walMeasurement{Status: "unmeasured", Reason: "WAL probe was unavailable"},
		Observer: observerDelta{Status: "unmeasured", Reason: "observer was not configured"}}
}

func validateV4Response(one workloadCase, phase string, response executeResponse) error {
	if response.Exposure.ProfileVersion != "taskgate-exposure-v4" {
		return fmt.Errorf("profile_version=%q, want taskgate-exposure-v4", response.Exposure.ProfileVersion)
	}
	for name, value := range map[string]string{
		"observation":    response.Exposure.ObservationSHA256,
		"dictionary_set": response.Exposure.DictionarySetDigest,
		"release_set":    response.Exposure.ReleaseSetSHA256,
		"influence_set":  response.Exposure.InfluenceSetSHA256,
		"outcome_set":    response.Exposure.OutcomeSetSHA256,
	} {
		if len(value) != 64 {
			return fmt.Errorf("V4 %s digest is absent or malformed", name)
		}
	}
	if response.Exposure.ActualOutcomeFacts != 1 {
		return fmt.Errorf("actual outcome facts=%d, want 1", response.Exposure.ActualOutcomeFacts)
	}
	if response.Exposure.RootEpoch <= 0 {
		return fmt.Errorf("V4 root epoch=%d, want a committed positive epoch", response.Exposure.RootEpoch)
	}
	for name, pair := range map[string]struct{ actual, charged int64 }{
		"release":   {response.Exposure.ActualReleaseFacts, response.Exposure.ChargedReleaseFacts},
		"influence": {response.Exposure.ActualInfluenceFacts, response.Exposure.ChargedInfluenceFacts},
		"outcome":   {response.Exposure.ActualOutcomeFacts, response.Exposure.ChargedOutcomeFacts},
	} {
		if pair.actual < 0 || pair.charged < 0 || pair.charged > pair.actual {
			return fmt.Errorf("invalid %s cardinalities: actual=%d charged=%d", name, pair.actual, pair.charged)
		}
	}
	if phase == "novel" {
		if one.Expected.RowCount != nil && response.RowCount != *one.Expected.RowCount {
			return fmt.Errorf("row_count=%d, want %d", response.RowCount, *one.Expected.RowCount)
		}
		for name, pair := range map[string]struct {
			actual   int64
			expected *int64
		}{
			"release":   {response.Exposure.ActualReleaseFacts, one.Expected.ReleaseFacts},
			"influence": {response.Exposure.ActualInfluenceFacts, one.Expected.InfluenceFacts},
			"outcome":   {response.Exposure.ActualOutcomeFacts, one.Expected.OutcomeFacts},
		} {
			if pair.expected != nil && pair.actual != *pair.expected {
				return fmt.Errorf("actual %s facts=%d, want %d", name, pair.actual, *pair.expected)
			}
		}
		if err := validateOrdinalTimingComponents(response.ComponentMS); err != nil {
			return err
		}
	}
	return nil
}

func sameObservationIdentity(left, right exposureResult) bool {
	return left.ProfileVersion == right.ProfileVersion &&
		left.ActualReleaseFacts == right.ActualReleaseFacts &&
		left.ActualInfluenceFacts == right.ActualInfluenceFacts &&
		left.ActualOutcomeFacts == right.ActualOutcomeFacts &&
		left.ObservationSHA256 == right.ObservationSHA256 &&
		left.DictionarySetDigest == right.DictionarySetDigest &&
		left.ReleaseSetSHA256 == right.ReleaseSetSHA256 &&
		left.InfluenceSetSHA256 == right.InfluenceSetSHA256 &&
		left.OutcomeSetSHA256 == right.OutcomeSetSHA256
}

func validateOrdinalTimingComponents(components map[string]float64) error {
	const toleranceMS = 0.000001 // one nanosecond after duration-to-ms conversion
	required := []string{
		"provenance_postgresql",
		"ordinal_stream",
		"ordinal_stream_consumer",
		"ordinal_visible_preparation",
		"ordinal_finish",
		"bitmap_derivation",
	}
	for _, name := range required {
		value, ok := components[name]
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("V4 component timer %q is absent or invalid", name)
		}
	}
	if components["ordinal_stream"] <= 0 || components["bitmap_derivation"] <= 0 {
		return errors.New("V4 stream and bitmap aggregate timers must be positive measurements")
	}
	streamLeaves := components["provenance_postgresql"] + components["ordinal_stream_consumer"]
	if math.Abs(components["ordinal_stream"]-streamLeaves) > toleranceMS {
		return fmt.Errorf("ordinal stream timer is incoherent: aggregate=%f leaves=%f",
			components["ordinal_stream"], streamLeaves)
	}
	bitmapLeaves := components["ordinal_visible_preparation"] + components["ordinal_stream_consumer"] +
		components["ordinal_finish"]
	if math.Abs(components["bitmap_derivation"]-bitmapLeaves) > toleranceMS {
		return fmt.Errorf("bitmap derivation timer is incoherent: aggregate=%f leaves=%f",
			components["bitmap_derivation"], bitmapLeaves)
	}
	return nil
}

func overlapPercent(value exposureResult, dimension string) *float64 {
	var actual, charged int64
	switch dimension {
	case "release":
		actual, charged = value.ActualReleaseFacts, value.ChargedReleaseFacts
	case "outcome":
		actual, charged = value.ActualOutcomeFacts, value.ChargedOutcomeFacts
	case "all":
		actual = value.ActualReleaseFacts + value.ActualInfluenceFacts + value.ActualOutcomeFacts
		charged = value.ChargedReleaseFacts + value.ChargedInfluenceFacts + value.ChargedOutcomeFacts
	default:
		actual, charged = value.ActualInfluenceFacts, value.ChargedInfluenceFacts
	}
	if actual <= 0 || charged < 0 || charged > actual {
		return nil
	}
	result := 100 * float64(actual-charged) / float64(actual)
	return &result
}

func freshTaskRoot(ctx context.Context, control *pgxpool.Pool, taskID string) (bool, int64, error) {
	var epoch int64
	err := control.QueryRow(ctx, `SELECT COALESCE(head.epoch,0)
FROM tasks task LEFT JOIN v4_exposure_root_heads head ON head.root_task_id=task.root_task_id
WHERE task.id=$1`, taskID).Scan(&epoch)
	if err != nil {
		return false, 0, err
	}
	return epoch == 0, epoch, nil
}

func durationMS(value time.Duration) float64 {
	return float64(value.Nanoseconds()) / float64(time.Millisecond)
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

func sanitizeURL(value string) string {
	if index := strings.Index(value, "?"); index >= 0 {
		value = value[:index]
	}
	return strings.TrimRight(value, "/")
}

func safeID(value string) string {
	var result strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			result.WriteRune(char)
		} else {
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-")
}

func uniqueFloats(values []float64) []float64 {
	result := make([]float64, 0, len(values))
	for _, value := range values {
		if len(result) == 0 || math.Abs(value-result[len(result)-1]) > 1e-9 {
			result = append(result, value)
		}
	}
	return result
}

func sourceDigest() string {
	if embeddedSourceDigest != "" {
		if validSourceDigest(embeddedSourceDigest) {
			return embeddedSourceDigest
		}
		return ""
	}
	repositoryRoot := findRepositoryRoot()
	if repositoryRoot == "" {
		return ""
	}
	digest, err := sourceDigestFromRoot(repositoryRoot)
	if err != nil {
		return ""
	}
	return digest
}

func sourceDigestFromRoot(repositoryRoot string) (string, error) {
	if repositoryRoot == "" {
		return "", errors.New("repository root is empty")
	}
	paths := make([]string, 0)
	for _, root := range sourceDigestRoots {
		fileCount := 0
		if err := filepath.WalkDir(filepath.Join(repositoryRoot, root), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && (strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".sql")) {
				paths = append(paths, path)
				fileCount++
			}
			return nil
		}); err != nil {
			return "", fmt.Errorf("walk source root %s: %w", root, err)
		}
		if fileCount == 0 {
			return "", fmt.Errorf("source root %s contains no Go or SQL files", root)
		}
	}
	for _, relative := range sourceDigestFiles {
		paths = append(paths, filepath.Join(repositoryRoot, relative))
	}
	sort.Strings(paths)
	var joined strings.Builder
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read source artifact %s: %w", path, err)
		}
		relative, err := filepath.Rel(repositoryRoot, path)
		if err != nil {
			return "", fmt.Errorf("make source path relative %s: %w", path, err)
		}
		joined.WriteString(filepath.ToSlash(relative))
		joined.WriteByte(0)
		joined.Write(raw)
		joined.WriteByte(0)
	}
	return sha256Hex([]byte(joined.String())), nil
}

func validSourceDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func findRepositoryRoot() string {
	working, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if info, err := os.Stat(filepath.Join(working, "go.mod")); err == nil && info.Mode().IsRegular() {
			return working
		}
		parent := filepath.Dir(working)
		if parent == working {
			return ""
		}
		working = parent
	}
}

func metricSemantics() map[string]string {
	return map[string]string{
		"direct_sql":                  "client wall time for the configured semantically corresponding read-only SQL",
		"novel":                       "distinct request whose semantic replay key was absent; overlap is exact charged-vs-actual V4 novelty in the configured dimension",
		"semantic_replay":             "same task and plan under a distinct request_id after a committed novel result",
		"provenance_postgresql":       "ordinal companion query-to-drain wall time minus synchronously measured Begin/Row consumer callbacks",
		"ordinal_stream":              "ordinal companion query-to-drain wall time including network, decoding, and synchronous consumer callbacks",
		"ordinal_stream_consumer":     "sum of synchronously measured Begin/Row callback durations contained in ordinal_stream",
		"ordinal_visible_preparation": "VisibleResult callback duration measured between the visible and provenance statements",
		"ordinal_finish":              "post-stream Finish and immutable Control-observation construction duration",
		"bitmap_derivation":           "sum of measured VisibleResult, Begin, Row, and Finish callback durations for exact bitmap derivation",
		"exposure_derivation":         "post-stream Finish duration retained for backward-compatible component analysis",
		"connector_overhead":          "connector wall time excluding visible/provenance query-to-drain timers and separately reported VisibleResult work",
		"settle_persist":              "Gateway-reported atomic V4 settlement, result/audit persistence, and root-head CAS store duration",
		"wal":                         "pg_wal_lsn_diff around the operation; relation-level attribution is not inferred",
		"observer":                    "delta of monotonic counters and/or cgroup peak values returned by the configured external observer",
		"storage":                     "PostgreSQL pg_total_relation_size plus exact filesystem byte counts; projected per-root values are labeled estimates",
	}
}
