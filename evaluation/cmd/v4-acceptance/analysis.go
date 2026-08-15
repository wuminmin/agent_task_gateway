package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxPointReleaseFacts   = int64(12)
	maxPointInfluenceFacts = int64(1_035_000)
)

func summarizeSamples(samples []sample) []summary {
	type key struct{ phase, shape string }
	grouped := make(map[key][]sample)
	for _, one := range samples {
		if one.Status != "measured" || (one.Phase != "direct_sql" && one.Phase != "novel" && one.Phase != "semantic_replay") {
			continue
		}
		grouped[key{phase: one.Phase}] = append(grouped[key{phase: one.Phase}], one)
		grouped[key{phase: one.Phase, shape: one.Shape}] = append(grouped[key{phase: one.Phase, shape: one.Shape}], one)
	}
	keys := make([]key, 0, len(grouped))
	for one := range grouped {
		keys = append(keys, one)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].phase != keys[j].phase {
			return keys[i].phase < keys[j].phase
		}
		return keys[i].shape < keys[j].shape
	})
	result := make([]summary, 0, len(keys))
	for _, one := range keys {
		values := grouped[one]
		latencies := make([]float64, 0, len(values))
		database := make([]float64, 0, len(values))
		components := make(map[string][]float64)
		for _, value := range values {
			latencies = append(latencies, value.ClientLatencyMS)
			if value.DatabaseMS >= 0 {
				database = append(database, value.DatabaseMS)
			}
			for name, component := range value.ComponentMS {
				components[name] = append(components[name], component)
			}
		}
		item := summary{Phase: one.phase, Shape: one.shape, Samples: len(values), ClientLatencyMS: summarize(latencies)}
		if len(database) > 0 {
			value := summarize(database)
			item.DatabaseMS = &value
		}
		if len(components) > 0 {
			item.ComponentMS = make(map[string]distribution, len(components))
			for name, component := range components {
				item.ComponentMS[name] = summarize(component)
			}
		}
		result = append(result, item)
	}
	return result
}

func summarize(values []float64) distribution {
	if len(values) == 0 {
		return distribution{}
	}
	copy := append([]float64(nil), values...)
	sort.Float64s(copy)
	var total float64
	for _, value := range copy {
		total += value
	}
	return distribution{Count: len(copy), Min: copy[0], P50: percentile(copy, 0.50), P95: percentile(copy, 0.95),
		P99: percentile(copy, 0.99), Max: copy[len(copy)-1], Mean: total / float64(len(copy))}
}

func percentile(sorted []float64, quantile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := quantile * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func buildCoverage(cfg config, samples []sample) coverage {
	result := coverage{Overlaps: make(map[string]coverageItem), Shapes: make(map[string]coverageItem)}
	for _, target := range []float64{0, 50, 90, 100} {
		key := formatPercent(target)
		item := coverageItem{Status: "unmeasured", Reason: "no successful novel sample for this target"}
		for _, one := range samples {
			if one.Phase != "novel" || one.Status != "measured" || math.Abs(one.TargetOverlapPercent-target) > 1e-9 ||
				one.ObservedOverlapPercent == nil {
				continue
			}
			item.Samples++
			item.Values = append(item.Values, *one.ObservedOverlapPercent)
		}
		if item.Samples > 0 {
			item.Status, item.Reason = "measured", ""
			for _, value := range item.Values {
				if math.Abs(value-target) > cfg.OverlapTolerancePoint {
					item.Status = "failed"
					item.Reason = fmt.Sprintf("observed overlap differs by more than %.3f percentage points", cfg.OverlapTolerancePoint)
					break
				}
			}
		}
		result.Overlaps[key] = item
	}
	for _, shape := range []string{"scan", "join_group", "union", "page"} {
		item := coverageItem{Status: "unmeasured", Reason: "no successful novel and replay pair for this shape"}
		novel := make(map[string]struct{})
		replay := make(map[string]struct{})
		for _, one := range samples {
			if one.Shape != shape || one.Status != "measured" {
				continue
			}
			identity := one.CaseID + ":" + fmt.Sprint(one.Trial)
			if one.Phase == "novel" {
				novel[identity] = struct{}{}
			}
			if one.Phase == "semantic_replay" && one.SemanticReplay {
				replay[identity] = struct{}{}
			}
		}
		for identity := range novel {
			if _, ok := replay[identity]; ok {
				item.Samples++
			}
		}
		if item.Samples > 0 {
			item.Status, item.Reason = "measured", ""
		}
		result.Shapes[shape] = item
	}
	return result
}

func evaluateGates(cfg config, result report) []gate {
	var gates []gate
	provenanceOK := len(result.Provenance.ConfigSHA256) == 64 && len(result.Provenance.SourceSHA256) == 64
	gates = append(gates, gate{ID: "evidence_provenance", Requirement: "configuration and implementation source are SHA-256 bound",
		Status: passFail(provenanceOK), Evidence: result.Provenance})
	environmentStatus := "unmeasured"
	if result.Environment.Status == "measured" {
		environmentStatus = "pass"
	} else if result.Environment.Status == "failed" {
		environmentStatus = "fail"
	}
	gates = append(gates, gate{ID: "fixed_environment_manifest", Requirement: "digest-bound host/software/database/dataset environment manifest",
		Status: environmentStatus, Evidence: result.Environment, Reason: result.Environment.Reason})
	failedSamples := 0
	for _, one := range result.Samples {
		if one.Status == "failed" {
			failedSamples++
		}
	}
	gates = append(gates, gate{ID: "execution_integrity", Requirement: "every configured operation completes and satisfies V4 response invariants",
		Status: passFail(failedSamples == 0 && len(result.Samples) > 0), Evidence: map[string]int{"failed_samples": failedSamples, "total_samples": len(result.Samples)}})
	if cfg.Observer != nil && cfg.Observer.Required {
		missing := 0
		for _, one := range result.Samples {
			if one.Status == "measured" && one.Observer.Status != "measured" {
				missing++
			}
		}
		gates = append(gates, gate{ID: "required_observer", Requirement: "the required observer measures every successful operation",
			Status: passFail(missing == 0 && len(result.Samples) > 0), Evidence: map[string]int{"missing_samples": missing}})
	}
	for _, target := range []float64{0, 50, 90, 100} {
		item := result.Coverage.Overlaps[formatPercent(target)]
		gates = append(gates, gate{ID: "overlap_" + formatPercent(target),
			Requirement: fmt.Sprintf("exact %.0f%% overlap within %.3f percentage points", target, cfg.OverlapTolerancePoint),
			Status:      normalizeGateStatus(item.Status), Evidence: item.Values, Reason: item.Reason})
	}
	for _, shape := range []string{"scan", "join_group", "union", "page"} {
		item := result.Coverage.Shapes[shape]
		gates = append(gates, gate{ID: "shape_" + shape, Requirement: "successful novel and semantic replay coverage",
			Status: normalizeGateStatus(item.Status), Evidence: map[string]any{"pairs": item.Samples}, Reason: item.Reason})
	}

	novel, novelOK := maxPointLatency(result.Samples, "novel")
	if !novelOK {
		gates = append(gates, gate{ID: "novel_latency", Requirement: "12-Release/1,035,000-Influence novel P50 <= 3000 ms and P95 <= 4000 ms",
			Status: "unmeasured", Reason: "no successful exact maximum-point novel samples"})
	} else {
		status := passFail(novel.P50 <= 3000 && novel.P95 <= 4000)
		gates = append(gates, gate{ID: "novel_latency", Requirement: "12-Release/1,035,000-Influence novel P50 <= 3000 ms and P95 <= 4000 ms",
			Status: status, Evidence: novel})
	}
	replay, replayOK := maxPointLatency(result.Samples, "semantic_replay")
	if !replayOK {
		gates = append(gates, gate{ID: "semantic_replay_latency", Requirement: "maximum-point semantic replay P50 <= 100 ms and P95 <= 150 ms",
			Status: "unmeasured", Reason: "no successful exact maximum-point semantic replay samples"})
	} else {
		status := passFail(replay.P50 <= 100 && replay.P95 <= 150)
		gates = append(gates, gate{ID: "semantic_replay_latency", Requirement: "maximum-point semantic replay P50 <= 100 ms and P95 <= 150 ms",
			Status: status, Evidence: replay})
	}

	componentStatus, componentEvidence := replayComponentEvidence(result.Samples)
	gates = append(gates, gate{ID: "semantic_replay_gateway_sql_components",
		Requirement: "replay reports database_ms=1 and no Business/provenance PostgreSQL component",
		Status:      componentStatus, Evidence: componentEvidence})
	externalStatus, externalEvidence, externalReason := externalReplaySQLEvidence(cfg.Observer, result.Samples)
	gates = append(gates, gate{ID: "semantic_replay_no_business_sql",
		Requirement: "external Business SQL query counter delta is zero for every replay",
		Status:      externalStatus, Evidence: externalEvidence, Reason: externalReason})

	peakStatus, peakEvidence, peakReason := gatewayPeakEvidence(cfg.Observer, result.Samples)
	gates = append(gates, gate{ID: "gateway_cgroup_peak_memory", Requirement: "cgroup peak memory including mmap pages <= 512 MiB",
		Status: peakStatus, Evidence: peakEvidence, Reason: peakReason})
	networkStatus, networkEvidence, networkReason := networkEvidence(cfg.Observer, result.Samples)
	gates = append(gates, gate{ID: "network_measurement", Requirement: "gateway workload RX and TX bytes are externally measured",
		Status: networkStatus, Evidence: networkEvidence, Reason: networkReason})
	walStatus, walEvidence := walEvidence(result.Samples)
	gates = append(gates, gate{ID: "wal_measurement", Requirement: "Business and Control WAL deltas are measured for every successful operation",
		Status: walStatus, Evidence: walEvidence})

	gates = append(gates, commandGates("index_build_time", "index build <= 600000 ms", result.IndexBuild,
		func(run commandRun) (bool, bool) { return run.WallMS <= 600000, true })...)
	builderRSS := gate{ID: "index_builder_rss", Requirement: "builder RSS <= 4 GiB"}
	if cfg.IndexBuild == nil || result.IndexBuild.Status == "unmeasured" {
		builderRSS.Status, builderRSS.Reason = "unmeasured", "index build was not configured"
	} else if !cfg.IndexBuild.SingleProcess {
		builderRSS.Status, builderRSS.Reason = "unmeasured", "root-process RSS cannot bound a command that may spawn child processes"
	} else {
		var peaks []int64
		valid := true
		for _, run := range result.IndexBuild.Runs {
			if run.Status != "measured" || run.RootPeakRSSBytes == nil {
				valid = false
				continue
			}
			peaks = append(peaks, *run.RootPeakRSSBytes)
			if *run.RootPeakRSSBytes > 4<<30 {
				builderRSS.Status = "fail"
			}
		}
		builderRSS.Evidence = map[string]any{"root_process_peak_rss_bytes": peaks, "scope": "single root process /proc VmHWM or VmRSS"}
		if builderRSS.Status == "" && valid && len(peaks) > 0 {
			builderRSS.Status = "pass"
		} else if builderRSS.Status == "" {
			builderRSS.Status, builderRSS.Reason = "unmeasured", "peak RSS was not observed for every run"
		}
	}
	gates = append(gates, builderRSS)
	gates = append(gates, artifactGate("artifact_total", "total snapshot artifact <= 2 GiB", result.Artifacts.TotalBytes, 2<<30, result.Artifacts.Reason))
	gates = append(gates, artifactGate("artifact_hot", "Gateway hot artifact <= 1024 MiB", result.Artifacts.HotBytes, 1024<<20, result.Artifacts.Reason))
	verificationGate := gate{ID: "activation_strict_verification",
		Requirement: "warm activation receipt is produced by a successful full HOT/COLD/sidecar verification phase"}
	if cfg.ActivationVerification == nil || result.ActivationVerification.Status == "unmeasured" {
		verificationGate.Status, verificationGate.Reason = "unmeasured", "strict activation verification was not configured"
	} else {
		verificationGate.Status = "pass"
		var walls []float64
		var outputs []string
		for _, run := range result.ActivationVerification.Runs {
			walls = append(walls, run.WallMS)
			outputs = append(outputs, run.OutputSHA256)
			if run.Status != "measured" || !validSourceDigest(run.OutputSHA256) {
				verificationGate.Status = "fail"
			}
		}
		if !validSourceDigest(result.Provenance.ActivationVerificationReceiptSHA256) ||
			len(result.ActivationVerification.Runs) == 0 {
			verificationGate.Status = "fail"
		}
		verificationGate.Evidence = map[string]any{"wall_ms": walls, "command_output_sha256": outputs,
			"receipt_sha256": result.Provenance.ActivationVerificationReceiptSHA256}
		if result.ActivationVerification.Status != "measured" {
			verificationGate.Status = "fail"
			verificationGate.Reason = result.ActivationVerification.Reason
		}
	}
	gates = append(gates, verificationGate)
	activationGate := gate{ID: "activation_time", Requirement: "warm verified index activation <= 2000 ms"}
	if cfg.Activation == nil || result.Activation.Status == "unmeasured" {
		activationGate.Status, activationGate.Reason = "unmeasured", "activation command was not configured"
	} else if !cfg.Activation.WarmVerified {
		activationGate.Status, activationGate.Reason = "unmeasured", "command was not declared to activate a warm verified index"
	} else if verificationGate.Status != "pass" {
		activationGate.Status, activationGate.Reason = "fail", "strict publication verification did not produce the receipt bound to activation"
	} else {
		var walls []float64
		activationGate.Status = "pass"
		for _, run := range result.Activation.Runs {
			walls = append(walls, run.WallMS)
			if run.Status != "measured" || run.WallMS > 2000 {
				activationGate.Status = "fail"
			}
		}
		activationGate.Evidence = map[string]any{"wall_ms": walls}
	}
	gates = append(gates, activationGate)

	storageStatus := "pass"
	if result.Storage.Status != "measured" {
		storageStatus = "unmeasured"
	}
	gates = append(gates, gate{ID: "storage_measurement", Requirement: "Control V4 and 1/10/100-root amortized storage are measured",
		Status: storageStatus, Evidence: result.Storage, Reason: result.Storage.Reason})

	bitmapEvidence := componentDistributions(result.Summaries, "novel", "bitmap_derivation")
	bitmapStatus := "pass"
	bitmapReason := ""
	if len(bitmapEvidence) == 0 {
		bitmapStatus = "unmeasured"
		bitmapReason = "no successful novel sample reported the full VisibleResult/Begin/Row/Finish consumer timer"
	}
	gates = append(gates, gate{ID: "bitmap_derivation_end_to_end", Requirement: "full streaming bitmap derivation is independently timed",
		Status: bitmapStatus, Evidence: bitmapEvidence, Reason: bitmapReason})
	streamEvidence := map[string]any{
		"provenance_postgresql_ms": componentDistributions(result.Summaries, "novel", "provenance_postgresql"),
		"bitmap_derivation_ms":     bitmapEvidence,
		"ordinal_stream_ms":        componentDistributions(result.Summaries, "novel", "ordinal_stream"),
	}
	streamStatus := "pass"
	streamReason := ""
	if len(componentDistributions(result.Summaries, "novel", "ordinal_stream")) == 0 || len(bitmapEvidence) == 0 ||
		len(componentDistributions(result.Summaries, "novel", "provenance_postgresql")) == 0 {
		streamStatus = "unmeasured"
		streamReason = "no successful novel sample reported both companion stream wall time and isolated consumer time"
	}
	gates = append(gates, gate{ID: "ordinal_stream_end_to_end", Requirement: "ordinal companion SQL and stream consumer wall time are independently timed",
		Status: streamStatus, Evidence: streamEvidence, Reason: streamReason})
	settle := componentDistributions(result.Summaries, "novel", "settle_persist")
	settleStatus := "pass"
	if len(settle) == 0 {
		settleStatus = "unmeasured"
	}
	gates = append(gates, gate{ID: "settlement_measurement", Requirement: "atomic settlement duration is reported",
		Status: settleStatus, Evidence: settle})

	gates = append(gates, smallQueryGate(cfg.SmallQueryBaseline, cfg.SmallQueryCandidate))
	return gates
}

func normalizeGateStatus(value string) string {
	switch value {
	case "measured":
		return "pass"
	case "failed":
		return "fail"
	default:
		return "unmeasured"
	}
}

func passFail(value bool) string {
	if value {
		return "pass"
	}
	return "fail"
}

func aggregateSummary(summaries []summary, phase string) (distribution, bool) {
	for _, item := range summaries {
		if item.Phase == phase && item.Shape == "" {
			return item.ClientLatencyMS, item.Samples > 0
		}
	}
	return distribution{}, false
}

func replayComponentEvidence(samples []sample) (string, any) {
	count := 0
	violations := 0
	for _, one := range samples {
		if one.Phase != "semantic_replay" || one.Status != "measured" {
			continue
		}
		count++
		_, business := one.ComponentMS["business_postgresql"]
		_, provenance := one.ComponentMS["provenance_postgresql"]
		if one.DatabaseMS != 1 || business || provenance {
			violations++
		}
	}
	if count == 0 {
		return "unmeasured", map[string]int{"samples": 0}
	}
	return passFail(violations == 0), map[string]int{"samples": count, "violations": violations}
}

func externalReplaySQLEvidence(observer *observerConfig, samples []sample) (string, any, string) {
	if observer == nil || observer.BusinessSQLCounter == "" {
		return "unmeasured", nil, "observer business_sql_counter was not configured"
	}
	count, violations, missing := 0, 0, 0
	var deltas []int64
	for _, one := range samples {
		if one.Phase != "semantic_replay" || one.Status != "measured" {
			continue
		}
		count++
		value, ok := one.Observer.Delta[observer.BusinessSQLCounter]
		if one.Observer.Status != "measured" || !ok {
			missing++
			continue
		}
		deltas = append(deltas, value)
		if value != 0 {
			violations++
		}
	}
	evidence := map[string]any{"samples": count, "deltas": deltas, "violations": violations, "missing": missing}
	if count == 0 || missing > 0 {
		return "unmeasured", evidence, "external SQL counter was absent for at least one replay"
	}
	return passFail(violations == 0), evidence, ""
}

func gatewayPeakEvidence(observer *observerConfig, samples []sample) (string, any, string) {
	if observer == nil || observer.GatewayPeakMemoryMetric == "" || observer.RequiredMemoryScope == "" {
		return "unmeasured", nil, "observer peak-memory metric and required scope were not configured"
	}
	var peaks []int64
	missing := 0
	for _, one := range samples {
		if one.Status != "measured" || one.Phase == "direct_sql" || strings.HasPrefix(one.Phase, "setup_") {
			continue
		}
		value, ok := one.Observer.After[observer.GatewayPeakMemoryMetric]
		if one.Observer.Status != "measured" || one.Observer.MemoryScope != observer.RequiredMemoryScope || !ok {
			missing++
			continue
		}
		peaks = append(peaks, value)
	}
	if len(peaks) == 0 || missing > 0 {
		return "unmeasured", map[string]any{"peaks": peaks, "missing": missing}, "cgroup peak was absent or had the wrong scope"
	}
	maxPeak := peaks[0]
	for _, value := range peaks[1:] {
		if value > maxPeak {
			maxPeak = value
		}
	}
	return passFail(maxPeak <= 512<<20), map[string]any{"max_bytes": maxPeak, "scope": observer.RequiredMemoryScope}, ""
}

func networkEvidence(observer *observerConfig, samples []sample) (string, any, string) {
	if observer == nil || observer.NetworkRXCounter == "" || observer.NetworkTXCounter == "" {
		return "unmeasured", nil, "observer RX/TX counters were not configured"
	}
	var rx, tx int64
	count, missing := 0, 0
	for _, one := range samples {
		if one.Status != "measured" {
			continue
		}
		rxValue, rxOK := one.Observer.Delta[observer.NetworkRXCounter]
		txValue, txOK := one.Observer.Delta[observer.NetworkTXCounter]
		if one.Observer.Status != "measured" || !rxOK || !txOK {
			missing++
			continue
		}
		count++
		rx += rxValue
		tx += txValue
	}
	if count == 0 || missing > 0 {
		return "unmeasured", map[string]any{"samples": count, "missing": missing}, "network counters were absent for at least one sample"
	}
	return "pass", map[string]any{"samples": count, "rx_bytes": rx, "tx_bytes": tx}, ""
}

func walEvidence(samples []sample) (string, any) {
	count, missing := 0, 0
	var business, control int64
	for _, one := range samples {
		if one.Status != "measured" {
			continue
		}
		if one.WAL.Status != "measured" || one.WAL.BusinessBytes == nil || one.WAL.ControlBytes == nil {
			missing++
			continue
		}
		count++
		business += *one.WAL.BusinessBytes
		control += *one.WAL.ControlBytes
	}
	status := "pass"
	if count == 0 || missing > 0 {
		status = "unmeasured"
	}
	return status, map[string]any{"samples": count, "missing": missing, "business_bytes": business, "control_bytes": control}
}

func commandGates(id, requirement string, phase phaseMeasurement, predicate func(commandRun) (bool, bool)) []gate {
	result := gate{ID: id, Requirement: requirement}
	if phase.Status == "unmeasured" {
		result.Status, result.Reason = "unmeasured", phase.Reason
		return []gate{result}
	}
	result.Status = "pass"
	var walls []float64
	for _, run := range phase.Runs {
		walls = append(walls, run.WallMS)
		ok, measured := predicate(run)
		if !measured {
			result.Status = "unmeasured"
		} else if run.Status != "measured" || !ok {
			result.Status = "fail"
		}
	}
	result.Evidence = map[string]any{"wall_ms": walls}
	return []gate{result}
}

func artifactGate(id, requirement string, value *int64, limit int64, reason string) gate {
	if value == nil {
		return gate{ID: id, Requirement: requirement, Status: "unmeasured", Reason: reason}
	}
	return gate{ID: id, Requirement: requirement, Status: passFail(*value <= limit), Evidence: map[string]int64{"bytes": *value, "limit_bytes": limit}}
}

func componentDistributions(summaries []summary, phase, component string) map[string]distribution {
	result := make(map[string]distribution)
	for _, item := range summaries {
		if item.Phase != phase {
			continue
		}
		if value, ok := item.ComponentMS[component]; ok && value.Count > 0 && value.P50 > 0 &&
			!math.IsNaN(value.P50) && !math.IsInf(value.P50, 0) {
			shape := item.Shape
			if shape == "" {
				shape = "all"
			}
			result[shape] = value
		}
	}
	return result
}

func maxPointLatency(samples []sample, phase string) (distribution, bool) {
	values := make([]float64, 0)
	for _, one := range samples {
		if one.Phase != phase || one.Status != "measured" || one.Exposure == nil ||
			one.Exposure.ActualReleaseFacts != maxPointReleaseFacts ||
			one.Exposure.ActualInfluenceFacts != maxPointInfluenceFacts ||
			one.Exposure.ActualOutcomeFacts != 1 {
			continue
		}
		values = append(values, one.ClientLatencyMS)
	}
	if len(values) == 0 {
		return distribution{}, false
	}
	return summarize(values), true
}

func smallQueryGate(baseline, candidate *smallQueryBaseline) gate {
	gateResult := gate{ID: "small_query_regression", Requirement: "small-query latency and throughput degradation <= 10%"}
	if baseline == nil || candidate == nil {
		gateResult.Status = "unmeasured"
		switch {
		case baseline == nil && candidate == nil:
			gateResult.Reason = "digest-bound baseline and candidate artifacts were not configured"
		case baseline == nil:
			gateResult.Reason = "digest-bound baseline artifact was not configured"
		default:
			gateResult.Reason = "digest-bound candidate artifact was not configured"
		}
		return gateResult
	}
	baselineDigest, err := validateSmallQueryEvidence("baseline", *baseline)
	if err != nil {
		gateResult.Status, gateResult.Reason = "fail", err.Error()
		return gateResult
	}
	candidateDigest, err := validateSmallQueryEvidence("candidate", *candidate)
	if err != nil {
		gateResult.Status, gateResult.Reason = "fail", err.Error()
		return gateResult
	}
	latencyDegradation := (candidate.P50MS/baseline.P50MS - 1) * 100
	throughputDegradation := (1 - candidate.ThroughputQPS/baseline.ThroughputQPS) * 100
	const threshold = 10.0
	const floatingPointTolerance = 1e-9
	gateResult.Status = passFail(latencyDegradation <= threshold+floatingPointTolerance &&
		throughputDegradation <= threshold+floatingPointTolerance)
	gateResult.Evidence = map[string]any{
		"baseline_artifact_sha256":       baselineDigest,
		"candidate_artifact_sha256":      candidateDigest,
		"baseline_p50_ms":                baseline.P50MS,
		"candidate_p50_ms":               candidate.P50MS,
		"latency_degradation_percent":    latencyDegradation,
		"baseline_throughput_qps":        baseline.ThroughputQPS,
		"candidate_throughput_qps":       candidate.ThroughputQPS,
		"throughput_degradation_percent": throughputDegradation,
		"limit_percent":                  threshold,
	}
	return gateResult
}

func validateSmallQueryEvidence(label string, evidence smallQueryBaseline) (string, error) {
	if strings.TrimSpace(evidence.ArtifactPath) == "" {
		return "", fmt.Errorf("%s artifact path is empty", label)
	}
	if len(evidence.ArtifactSHA256) != sha256.Size*2 ||
		evidence.ArtifactSHA256 != strings.ToLower(evidence.ArtifactSHA256) {
		return "", fmt.Errorf("%s artifact SHA-256 is invalid", label)
	}
	if decoded, err := hex.DecodeString(evidence.ArtifactSHA256); err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("%s artifact SHA-256 is invalid", label)
	}
	if evidence.P50MS <= 0 || math.IsNaN(evidence.P50MS) || math.IsInf(evidence.P50MS, 0) {
		return "", fmt.Errorf("%s p50_ms must be a positive finite number", label)
	}
	if evidence.ThroughputQPS <= 0 || math.IsNaN(evidence.ThroughputQPS) || math.IsInf(evidence.ThroughputQPS, 0) {
		return "", fmt.Errorf("%s throughput_qps must be a positive finite number", label)
	}
	raw, err := os.ReadFile(evidence.ArtifactPath)
	if err != nil {
		return "", fmt.Errorf("read %s artifact: %w", label, err)
	}
	digest := sha256.Sum256(raw)
	actual := hex.EncodeToString(digest[:])
	if actual != evidence.ArtifactSHA256 {
		return actual, fmt.Errorf("%s artifact SHA-256 mismatch", label)
	}
	return actual, nil
}

func acceptanceStatus(gates []gate) string {
	unmeasured := false
	for _, one := range gates {
		if one.Status == "fail" {
			return "fail"
		}
		if one.Status == "unmeasured" {
			unmeasured = true
		}
	}
	if unmeasured {
		return "incomplete"
	}
	return "pass"
}

func formatPercent(value float64) string {
	if math.Abs(value-math.Round(value)) < 1e-9 {
		return fmt.Sprintf("%.0f", value)
	}
	return strings.ReplaceAll(fmt.Sprintf("%.3f", value), ".", "_")
}

func measureStorage(ctx context.Context, control *pgxpool.Pool, artifacts artifactMeasurement) storageMeasurement {
	rows, err := control.Query(ctx, `SELECT c.relname, pg_total_relation_size(c.oid)::bigint
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
	WHERE n.nspname='public' AND c.relkind='r' AND left(c.relname,3)='v4_'
ORDER BY c.relname`)
	if err != nil {
		return storageMeasurement{Status: "unmeasured", Reason: err.Error()}
	}
	defer rows.Close()
	result := storageMeasurement{Status: "measured", Semantics: "fixed artifacts are amortized by projected roots; runtime bytes/root is the page-granular measured runtime-table total divided by measured nonzero-epoch roots"}
	for rows.Next() {
		var relation relationSize
		if err := rows.Scan(&relation.Name, &relation.Bytes); err != nil {
			return storageMeasurement{Status: "unmeasured", Reason: err.Error()}
		}
		relation.Class = storageClass(relation.Name)
		result.Relations = append(result.Relations, relation)
		if relation.Class == "fixed_dictionary" {
			result.FixedControlBytes += relation.Bytes
		} else {
			result.RuntimeControlBytes += relation.Bytes
		}
	}
	if err := rows.Err(); err != nil {
		return storageMeasurement{Status: "unmeasured", Reason: err.Error()}
	}
	if len(result.Relations) == 0 {
		return storageMeasurement{Status: "unmeasured", Reason: "no V4 Control PostgreSQL relations were found"}
	}
	if err := control.QueryRow(ctx, `SELECT count(*) FROM v4_exposure_root_heads WHERE epoch > 0`).Scan(&result.MeasuredRoots); err != nil {
		return storageMeasurement{Status: "unmeasured", Reason: err.Error()}
	}
	if artifacts.TotalBytes != nil {
		result.ArtifactBytes = *artifacts.TotalBytes
	}
	if result.MeasuredRoots == 0 {
		result.Status = "partial"
		result.Reason = "no settled roots exist, so runtime bytes/root cannot be estimated"
		return result
	}
	runtimePerRoot := float64(result.RuntimeControlBytes) / float64(result.MeasuredRoots)
	fixed := float64(result.FixedControlBytes + result.ArtifactBytes)
	for _, roots := range []int{1, 10, 100} {
		fixedPerRoot := fixed / float64(roots)
		result.Amortized = append(result.Amortized, amortizedStorage{Roots: roots, FixedBytesPerRoot: fixedPerRoot,
			RuntimeBytesPerRoot: runtimePerRoot, EstimatedBytesPerRoot: fixedPerRoot + runtimePerRoot})
	}
	if artifacts.TotalBytes == nil {
		result.Status = "partial"
		result.Reason = "snapshot artifact bytes were unavailable; amortization contains Control PostgreSQL bytes only"
	}
	return result
}

func storageClass(name string) string {
	for _, prefix := range []string{"v4_cutover", "v4_dictionary", "v4_snapshot_publication"} {
		if strings.HasPrefix(name, prefix) {
			return "fixed_dictionary"
		}
	}
	return "runtime_ledger_cache_audit"
}

func measureEnvironment(reference *artifactReference) evidenceArtifact {
	if reference == nil {
		return evidenceArtifact{Status: "unmeasured", Reason: "environment manifest was not configured"}
	}
	raw, err := os.ReadFile(reference.Path)
	if err != nil {
		return evidenceArtifact{Status: "failed", Reason: err.Error()}
	}
	digest := sha256.Sum256(raw)
	actual := hex.EncodeToString(digest[:])
	if actual != reference.SHA256 {
		return evidenceArtifact{Status: "failed", SHA256: actual, Reason: "environment manifest digest mismatch"}
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return evidenceArtifact{Status: "failed", SHA256: actual, Reason: "environment manifest is not valid JSON: " + err.Error()}
	}
	for _, key := range []string{"host", "software", "database", "datasets"} {
		value, ok := manifest[key]
		if !ok {
			return evidenceArtifact{Status: "failed", SHA256: actual, Reason: "environment manifest omitted " + key}
		}
		if object, ok := value.(map[string]any); !ok || len(object) == 0 {
			return evidenceArtifact{Status: "failed", SHA256: actual, Reason: "environment manifest has an empty or invalid " + key + " object"}
		}
	}
	return evidenceArtifact{Status: "measured", SHA256: actual}
}
