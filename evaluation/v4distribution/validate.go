package v4distribution

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"strings"

	"taskbound.local/agent-data-gateway/internal/ordinal"
)

// ValidateReport fails closed on a malformed matrix and independently
// regenerates every deterministic bitmap metric and digest. Latency and memory
// observations cannot be regenerated, so their raw samples and internal
// summaries are checked for coherence instead.
func ValidateReport(report Report) error {
	return validateReport(report, true)
}

// ReadAndValidate reads a bounded regular JSON file, rejects unknown fields
// and trailing JSON, then performs the independent deterministic validation.
func ReadAndValidate(path string) (Report, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Report{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 64<<20 {
		return Report{}, errors.New("distribution report is not a bounded regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return Report{}, fmt.Errorf("distribution report canonical JSON: %w", err)
	}
	var report Report
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return Report{}, fmt.Errorf("decode distribution report: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Report{}, errors.New("distribution report contains trailing JSON")
	}
	if err := ValidateReport(report); err != nil {
		return Report{}, err
	}
	return report, nil
}

func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	var walk func(json.Token) error
	walk = func(token json.Token) error {
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("duplicate object key %q", key)
				}
				seen[key] = struct{}{}
				value, err := decoder.Token()
				if err != nil {
					return err
				}
				if err := walk(value); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return errors.New("object is not closed canonically")
			}
		case '[':
			for decoder.More() {
				value, err := decoder.Token()
				if err != nil {
					return err
				}
				if err := walk(value); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return errors.New("array is not closed canonically")
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		return nil
	}
	if err := walk(first); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func validateReport(report Report, recompute bool) error {
	if report.SchemaVersion != SchemaVersion || report.GeneratorVersion != GeneratorVersion ||
		report.Status != "complete_measured_kernel" || report.Scope != kernelScope {
		return errors.New("distribution report schema, generator, or status is invalid")
	}
	if err := report.Configuration.Validate(); err != nil {
		return fmt.Errorf("distribution report configuration: %w", err)
	}
	if report.StartedAt.IsZero() || report.FinishedAt.IsZero() || report.FinishedAt.Before(report.StartedAt) {
		return errors.New("distribution report timestamps are invalid")
	}
	if report.Runtime.GoVersion == "" || report.Runtime.GOOS == "" || report.Runtime.GOARCH == "" || report.Runtime.CPUs <= 0 {
		return errors.New("distribution report runtime identity is incomplete")
	}
	if len(report.MetricSemantics) != len(metricSemantics()) || !reflect.DeepEqual(report.MetricSemantics, metricSemantics()) {
		return errors.New("distribution report metric semantics changed")
	}
	eligible, reason := evidenceEligibility(report.Configuration)
	if report.AcceptanceEligible != eligible || report.EligibilityReason != reason {
		return errors.New("distribution report acceptance eligibility is incoherent")
	}
	expectedCells := len(distributionNames) * len(overlapTargets)
	if len(report.Cells) != expectedCells {
		return fmt.Errorf("distribution report has %d cells, want %d", len(report.Cells), expectedCells)
	}
	effectByDistribution := make(map[string]BitmapMetrics, len(distributionNames))
	observationByDistribution := make(map[string]string, len(distributionNames))
	physicalSignatures := make(map[string]string, len(distributionNames))
	effectDigests := make(map[string]string, len(distributionNames))
	for index := range report.Cells {
		distribution := distributionNames[index/len(overlapTargets)]
		target := overlapTargets[index%len(overlapTargets)]
		cell := report.Cells[index]
		if cell.Distribution != distribution || cell.TargetOverlapPercent != target {
			return fmt.Errorf("cell %d is not in canonical distribution/overlap order", index)
		}
		if err := validateCell(report.Configuration, cell); err != nil {
			return fmt.Errorf("cell %s/%d%%: %w", distribution, target, err)
		}
		if prior, found := effectByDistribution[distribution]; found {
			if !reflect.DeepEqual(prior, cell.Effect) || observationByDistribution[distribution] != cell.ObservationSHA256 {
				return fmt.Errorf("effect or observation changed across %s overlap cells", distribution)
			}
		} else {
			effectByDistribution[distribution] = cell.Effect
			observationByDistribution[distribution] = cell.ObservationSHA256
			physicalSignatures[fmt.Sprintf("%d/%d/%d/%d", cell.Effect.ContainerCount,
				cell.Effect.PortableBytes, cell.Effect.MinimumOrdinal, cell.Effect.MaximumOrdinal)] = distribution
			effectDigests[cell.Effect.Digest] = distribution
		}
		if recompute {
			expected, err := recomputeCell(report.Configuration, distribution, target)
			if err != nil {
				return fmt.Errorf("independent deterministic recomputation: %w", err)
			}
			if !sameDeterministicCell(cell, expected) {
				return errors.New("reported bitmap metrics or deterministic digest differ from independent recomputation")
			}
		}
	}
	if len(effectDigests) != len(distributionNames) || len(physicalSignatures) != len(distributionNames) {
		return errors.New("dense, clustered, and random-sparse effects are not physically distinct")
	}
	if report.MatrixSHA256 != matrixDigest(report.Cells) || !validDigest(report.MatrixSHA256) {
		return errors.New("distribution matrix digest mismatch")
	}
	return nil
}

func validateCell(config Config, cell Cell) error {
	overlapCardinality := config.Cardinality * uint64(cell.TargetOverlapPercent) / 100
	if cell.Effect.Cardinality != config.Cardinality || cell.LedgerBefore.Cardinality != overlapCardinality ||
		cell.NovelDelta.Cardinality != config.Cardinality-overlapCardinality ||
		cell.LedgerAfter.Cardinality != config.Cardinality || cell.ReplayDelta.Cardinality != 0 {
		return errors.New("effect, overlap, novelty, union, or replay cardinality is incorrect")
	}
	expectedOverlap := 100 * float64(overlapCardinality) / float64(config.Cardinality)
	if cell.ObservedOverlapPercent != expectedOverlap || expectedOverlap != float64(cell.TargetOverlapPercent) {
		return errors.New("observed overlap is not exact")
	}
	for label, metric := range map[string]BitmapMetrics{
		"effect": cell.Effect, "ledger_before": cell.LedgerBefore, "novel_delta": cell.NovelDelta,
		"ledger_after": cell.LedgerAfter, "replay_delta": cell.ReplayDelta,
	} {
		if err := validateBitmapMetrics(metric); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	if !reflect.DeepEqual(cell.Effect, cell.LedgerAfter) {
		return errors.New("ledger after OR must exactly equal the effect because prior is a subset")
	}
	if cell.TargetOverlapPercent == 0 && (cell.LedgerBefore.HasOrdinals || cell.LedgerBefore.ContainerCount != 0 || cell.LedgerBefore.PortableBytes != 0) {
		return errors.New("zero-overlap prior is not empty")
	}
	if cell.TargetOverlapPercent == 100 && !reflect.DeepEqual(cell.LedgerBefore, cell.Effect) {
		return errors.New("full-overlap prior does not equal effect")
	}
	if cell.ReplayDelta.HasOrdinals || cell.ReplayDelta.ContainerCount != 0 || cell.ReplayDelta.PortableBytes != 0 {
		return errors.New("semantic replay delta is not structurally empty")
	}
	if !validDigest(cell.ObservationSHA256) || cell.ReplayObservationSHA256 != cell.ObservationSHA256 || !cell.ReplayMatched ||
		cell.ObservationSHA256 != observationDigest(cell.Effect.Digest, cell.Effect.Cardinality) {
		return errors.New("committed observation digest replay is incoherent")
	}
	if cell.DeterministicCellSHA256 != deterministicCellDigest(cell) || !validDigest(cell.DeterministicCellSHA256) {
		return errors.New("deterministic cell digest mismatch")
	}
	if cell.ReplayLookupsPerRun != config.ReplayLookupsPerRun || !positiveFinite(cell.ConstructionAndEncodeMS) {
		return errors.New("kernel construction or replay batch evidence is invalid")
	}
	if err := validateLatency(cell.NovelBitmapLatency, config.Runs); err != nil {
		return fmt.Errorf("ANDNOT+OR latency: %w", err)
	}
	if err := validateLatency(cell.ReplayDigestLookup, config.Runs); err != nil {
		return fmt.Errorf("replay digest lookup latency: %w", err)
	}
	if cell.Memory.PeakHeapAllocBytes < cell.Memory.StartHeapAllocBytes ||
		cell.Memory.PeakHeapAllocBytes < cell.Memory.EndHeapAllocBytes ||
		cell.Memory.PeakHeapInuseBytes == 0 || cell.Memory.PeakHeapSysBytes == 0 ||
		cell.Memory.TotalAllocDelta == 0 ||
		cell.Memory.PeakDeltaBytes != cell.Memory.PeakHeapAllocBytes-cell.Memory.StartHeapAllocBytes {
		return errors.New("heap peak/workset evidence is incoherent")
	}
	if cell.Memory.PeakHeapAllocBytes > config.MaxPeakHeapBytes {
		return fmt.Errorf("kernel peak heap %d exceeds configured limit %d", cell.Memory.PeakHeapAllocBytes, config.MaxPeakHeapBytes)
	}
	return nil
}

func validateBitmapMetrics(metric BitmapMetrics) error {
	if !validDigest(metric.Digest) || metric.ContainerCount < 0 || !metric.PortableRoundTripVerified ||
		metric.RoundTripDigest != metric.Digest {
		return errors.New("digest or container count is invalid")
	}
	if metric.Cardinality == 0 {
		if metric.HasOrdinals || metric.ContainerCount != 0 || metric.PortableBytes != 0 ||
			metric.MinimumOrdinal != 0 || metric.MaximumOrdinal != 0 {
			return errors.New("empty bitmap has nonempty structural metrics")
		}
		return nil
	}
	if !metric.HasOrdinals || metric.ContainerCount == 0 || metric.PortableBytes == 0 || metric.MinimumOrdinal > metric.MaximumOrdinal {
		return errors.New("nonempty bitmap has incomplete structural metrics")
	}
	return nil
}

func validateLatency(evidence LatencyEvidence, runs int) error {
	if len(evidence.SamplesMS) != runs {
		return fmt.Errorf("sample count=%d, want %d", len(evidence.SamplesMS), runs)
	}
	for _, value := range evidence.SamplesMS {
		if !positiveFinite(value) {
			return errors.New("sample is not positive and finite")
		}
	}
	expected := summarize(evidence.SamplesMS)
	if !sameDistribution(expected, evidence.Summary) {
		return errors.New("type-7 summary differs from raw samples")
	}
	return nil
}

func sameDistribution(left, right Distribution) bool {
	return left.Count == right.Count && nearlyEqual(left.Min, right.Min) && nearlyEqual(left.P50, right.P50) &&
		nearlyEqual(left.P95, right.P95) && nearlyEqual(left.P99, right.P99) &&
		nearlyEqual(left.Max, right.Max) && nearlyEqual(left.Mean, right.Mean)
}

func nearlyEqual(left, right float64) bool {
	if left == right {
		return true
	}
	scale := math.Max(1, math.Max(math.Abs(left), math.Abs(right)))
	return math.Abs(left-right) <= 1e-12*scale
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func recomputeCell(config Config, distribution string, target int) (Cell, error) {
	overlapCardinality := config.Cardinality * uint64(target) / 100
	effectBuilder := ordinal.NewBuilder()
	priorBuilder := ordinal.NewBuilder()
	for index := uint64(0); index < config.Cardinality; index++ {
		ref := ordinal.FactRef{DictionaryDigest: kernelDictionary, SegmentID: "influence",
			Ordinal: generatedOrdinal(config, distribution, index)}
		if err := effectBuilder.Add(ref); err != nil {
			return Cell{}, err
		}
		if index < overlapCardinality {
			if err := priorBuilder.Add(ref); err != nil {
				return Cell{}, err
			}
		}
	}
	effect, err := effectBuilder.Freeze()
	if err != nil {
		return Cell{}, err
	}
	prior, err := priorBuilder.Freeze()
	if err != nil {
		return Cell{}, err
	}
	novel := effect.Difference(prior)
	updated := prior.Union(effect)
	replay := effect.Difference(updated)
	effectMetric, err := bitmapMetrics(effect)
	if err != nil {
		return Cell{}, err
	}
	priorMetric, err := bitmapMetrics(prior)
	if err != nil {
		return Cell{}, err
	}
	novelMetric, err := bitmapMetrics(novel)
	if err != nil {
		return Cell{}, err
	}
	updatedMetric, err := bitmapMetrics(updated)
	if err != nil {
		return Cell{}, err
	}
	replayMetric, err := bitmapMetrics(replay)
	if err != nil {
		return Cell{}, err
	}
	observation := observationDigest(effectMetric.Digest, effectMetric.Cardinality)
	cell := Cell{Distribution: distribution, TargetOverlapPercent: target,
		ObservedOverlapPercent: 100 * float64(prior.Cardinality()) / float64(effect.Cardinality()),
		Effect:                 effectMetric, LedgerBefore: priorMetric, NovelDelta: novelMetric,
		LedgerAfter: updatedMetric, ReplayDelta: replayMetric,
		ObservationSHA256: observation, ReplayObservationSHA256: observation, ReplayMatched: true}
	cell.DeterministicCellSHA256 = deterministicCellDigest(cell)
	return cell, nil
}

func sameDeterministicCell(measured, expected Cell) bool {
	return measured.Distribution == expected.Distribution &&
		measured.TargetOverlapPercent == expected.TargetOverlapPercent &&
		measured.ObservedOverlapPercent == expected.ObservedOverlapPercent &&
		reflect.DeepEqual(measured.Effect, expected.Effect) &&
		reflect.DeepEqual(measured.LedgerBefore, expected.LedgerBefore) &&
		reflect.DeepEqual(measured.NovelDelta, expected.NovelDelta) &&
		reflect.DeepEqual(measured.LedgerAfter, expected.LedgerAfter) &&
		reflect.DeepEqual(measured.ReplayDelta, expected.ReplayDelta) &&
		measured.ObservationSHA256 == expected.ObservationSHA256 &&
		measured.ReplayObservationSHA256 == expected.ReplayObservationSHA256 &&
		measured.ReplayMatched == expected.ReplayMatched &&
		measured.DeterministicCellSHA256 == expected.DeterministicCellSHA256
}
