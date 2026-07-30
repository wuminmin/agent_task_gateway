// Package v4distribution implements the deterministic bitmap-distribution
// kernel used by the TaskGate V4 evaluation. It deliberately measures only
// ordinal BitmapSet algebra and committed-observation digest lookup. It is not
// a Gateway, PostgreSQL, or end-to-end latency benchmark.
package v4distribution

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"taskbound.local/agent-data-gateway/internal/ordinal"
)

const (
	SchemaVersion    = 2
	GeneratorVersion = "taskgate-v4-bitmap-distribution-v2"

	defaultCardinality          = uint64(1_035_000)
	defaultRuns                 = 50
	minimumEvidenceRuns         = 50
	defaultClusterCount         = uint32(128)
	defaultRandomSeed           = uint32(0x6d2b79f5)
	defaultReplayLookupsPerRun  = 4096
	defaultMaximumPeakHeapBytes = uint64(512 << 20)
	maximumCardinality          = defaultCardinality

	observationDigestDomain = "TASKGATE-V4-DISTRIBUTION-OBSERVATION-V2\x00"
	kernelDigestDomain      = "TASKGATE-V4-DISTRIBUTION-CELL-V2\x00"
	kernelScope             = "ordinal BitmapSet kernel only; excludes Gateway, PostgreSQL, networking, encryption, CAS, and result persistence"
)

var (
	distributionNames = []string{"dense", "clustered", "random_sparse"}
	overlapTargets    = []int{0, 50, 90, 100}
	kernelDictionary  = "9ae9664e9aa327965754ef44e2fe680fc3bb11848bfc38c4c40d238ca7b4161a"
)

// Config fixes every input that can affect the generated ordinal sets. A
// cardinality divisible by 100 makes each requested percentage an exact set
// cardinality rather than a rounded approximation.
type Config struct {
	Cardinality         uint64 `json:"cardinality"`
	Runs                int    `json:"runs"`
	ClusterCount        uint32 `json:"cluster_count"`
	RandomSeed          uint32 `json:"random_seed"`
	ReplayLookupsPerRun int    `json:"replay_lookups_per_run"`
	MaxPeakHeapBytes    uint64 `json:"max_peak_heap_bytes"`
}

func DefaultConfig() Config {
	return Config{
		Cardinality:         defaultCardinality,
		Runs:                defaultRuns,
		ClusterCount:        defaultClusterCount,
		RandomSeed:          defaultRandomSeed,
		ReplayLookupsPerRun: defaultReplayLookupsPerRun,
		MaxPeakHeapBytes:    defaultMaximumPeakHeapBytes,
	}
}

func (c Config) Validate() error {
	if c.Cardinality == 0 || c.Cardinality > maximumCardinality || c.Cardinality%100 != 0 {
		return fmt.Errorf("cardinality must be a positive multiple of 100 no larger than %d", maximumCardinality)
	}
	if c.Runs < 1 || c.Runs > 100 {
		return errors.New("runs must be in [1,100]")
	}
	if c.ClusterCount < 2 || uint64(c.ClusterCount) > c.Cardinality || c.ClusterCount > 4096 {
		return errors.New("cluster_count must be in [2,min(cardinality,4096)]")
	}
	clusterWidth := (c.Cardinality + uint64(c.ClusterCount) - 1) / uint64(c.ClusterCount)
	clusterStride := uint64(math.MaxUint32) / (uint64(c.ClusterCount) + 1)
	if clusterWidth >= clusterStride {
		return errors.New("cardinality and cluster_count do not leave disjoint deterministic clusters")
	}
	if c.ReplayLookupsPerRun < 1 || c.ReplayLookupsPerRun > 1_000_000 {
		return errors.New("replay_lookups_per_run must be in [1,1000000]")
	}
	if c.MaxPeakHeapBytes == 0 {
		return errors.New("max_peak_heap_bytes must be positive")
	}
	return nil
}

type Report struct {
	SchemaVersion      int               `json:"schema_version"`
	Status             string            `json:"status"`
	GeneratorVersion   string            `json:"generator_version"`
	Scope              string            `json:"scope"`
	StartedAt          time.Time         `json:"started_at"`
	FinishedAt         time.Time         `json:"finished_at"`
	Configuration      Config            `json:"configuration"`
	Runtime            Runtime           `json:"runtime"`
	MetricSemantics    map[string]string `json:"metric_semantics"`
	Cells              []Cell            `json:"cells"`
	MatrixSHA256       string            `json:"matrix_sha256"`
	AcceptanceEligible bool              `json:"acceptance_eligible"`
	EligibilityReason  string            `json:"eligibility_reason,omitempty"`
}

type Runtime struct {
	GoVersion string `json:"go_version"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	CPUs      int    `json:"cpus"`
}

type Cell struct {
	Distribution            string          `json:"distribution"`
	TargetOverlapPercent    int             `json:"target_overlap_percent"`
	ObservedOverlapPercent  float64         `json:"observed_overlap_percent"`
	Effect                  BitmapMetrics   `json:"effect"`
	LedgerBefore            BitmapMetrics   `json:"ledger_before"`
	NovelDelta              BitmapMetrics   `json:"novel_delta"`
	LedgerAfter             BitmapMetrics   `json:"ledger_after"`
	ReplayDelta             BitmapMetrics   `json:"replay_delta"`
	ObservationSHA256       string          `json:"observation_sha256"`
	ReplayObservationSHA256 string          `json:"replay_observation_sha256"`
	ReplayMatched           bool            `json:"replay_matched"`
	NovelBitmapLatency      LatencyEvidence `json:"andnot_or_latency_ms"`
	ReplayDigestLookup      LatencyEvidence `json:"replay_digest_lookup_latency_ms"`
	ReplayLookupsPerRun     int             `json:"replay_lookups_per_run"`
	ConstructionAndEncodeMS float64         `json:"construction_and_encode_ms"`
	Memory                  MemoryMetrics   `json:"memory"`
	DeterministicCellSHA256 string          `json:"deterministic_cell_sha256"`
}

// BitmapMetrics measures the canonical portable high-16 representation. The
// byte count is exactly the sum of PortableContainer.Bitmap payload lengths;
// it excludes JSON/base64 and database tuple overhead.
type BitmapMetrics struct {
	Cardinality               uint64 `json:"cardinality"`
	ContainerCount            int    `json:"container_count"`
	PortableBytes             uint64 `json:"portable_bitmap_bytes"`
	Digest                    string `json:"digest"`
	PortableRoundTripVerified bool   `json:"portable_round_trip_verified"`
	RoundTripDigest           string `json:"round_trip_digest"`
	HasOrdinals               bool   `json:"has_ordinals"`
	MinimumOrdinal            uint32 `json:"minimum_ordinal,omitempty"`
	MaximumOrdinal            uint32 `json:"maximum_ordinal,omitempty"`
}

type LatencyEvidence struct {
	SamplesMS []float64    `json:"samples_ms"`
	Summary   Distribution `json:"summary"`
}

type Distribution struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
}

// MemoryMetrics are process-wide Go heap measurements while one cell is the
// only active kernel work. PeakDeltaBytes is a heap workset proxy, not RSS or
// cgroup memory, and the report labels it accordingly.
type MemoryMetrics struct {
	StartHeapAllocBytes uint64 `json:"start_heap_alloc_bytes"`
	EndHeapAllocBytes   uint64 `json:"end_heap_alloc_bytes"`
	PeakHeapAllocBytes  uint64 `json:"peak_heap_alloc_bytes"`
	PeakHeapInuseBytes  uint64 `json:"peak_heap_inuse_bytes"`
	PeakHeapSysBytes    uint64 `json:"peak_heap_sys_bytes"`
	PeakDeltaBytes      uint64 `json:"peak_heap_delta_bytes"`
	TotalAllocDelta     uint64 `json:"total_alloc_delta_bytes"`
}

func Run(config Config) (Report, error) {
	if err := config.Validate(); err != nil {
		return Report{}, err
	}
	// Keep the serialized wall-clock timestamp, but derive the finish from Go's
	// monotonic clock.  UTC conversion strips the monotonic component, and a
	// host clock correction during a run must not produce an impossible report
	// with finished_at before started_at.
	started := time.Now()
	report := Report{
		SchemaVersion:    SchemaVersion,
		Status:           "running",
		GeneratorVersion: GeneratorVersion,
		Scope:            kernelScope,
		StartedAt:        started.UTC(),
		Configuration:    config,
		Runtime: Runtime{GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
			CPUs: runtime.NumCPU()},
		MetricSemantics: metricSemantics(),
		Cells:           make([]Cell, 0, len(distributionNames)*len(overlapTargets)),
	}
	for _, name := range distributionNames {
		for _, target := range overlapTargets {
			cell, err := runCell(config, name, target)
			if err != nil {
				return Report{}, fmt.Errorf("%s/%d%%: %w", name, target, err)
			}
			report.Cells = append(report.Cells, cell)
		}
	}
	report.MatrixSHA256 = matrixDigest(report.Cells)
	report.AcceptanceEligible, report.EligibilityReason = evidenceEligibility(config)
	report.FinishedAt = report.StartedAt.Add(time.Since(started))
	report.Status = "complete_measured_kernel"
	if err := validateReport(report, false); err != nil {
		return Report{}, fmt.Errorf("self-validate distribution report: %w", err)
	}
	return report, nil
}

func evidenceEligibility(config Config) (bool, string) {
	if config.Cardinality != defaultCardinality {
		return false, "acceptance evidence requires exactly 1,035,000 effect facts per cell"
	}
	if config.Runs < minimumEvidenceRuns {
		return false, fmt.Sprintf("acceptance evidence requires at least %d raw timing runs per cell", minimumEvidenceRuns)
	}
	if config.ClusterCount != defaultClusterCount || config.RandomSeed != defaultRandomSeed ||
		config.ReplayLookupsPerRun != defaultReplayLookupsPerRun ||
		config.MaxPeakHeapBytes != defaultMaximumPeakHeapBytes {
		return false, "acceptance evidence requires the pinned generator, replay batch, and 512 MiB heap ceiling"
	}
	return true, ""
}

func metricSemantics() map[string]string {
	return map[string]string{
		"andnot_or_latency_ms":            "wall time for immutable effect ANDNOT ledger-before, exact popcount, and ledger-before OR effect; excludes construction and portable encoding",
		"replay_digest_lookup_latency_ms": "per-lookup wall time in an in-memory committed-observation map, measured in fixed-size batches; no bitmap scan is performed",
		"portable_bitmap_bytes":           "sum of canonical Roaring portable bytes over independently addressable high-16 containers; excludes JSON, base64, and database tuple overhead",
		"portable_round_trip_verified":    "PortableContainers serialization was parsed with ParsePortableContainers and checked for exact set equality and digest identity at full cell cardinality",
		"peak_heap_delta_bytes":           "maximum sampled Go HeapAlloc minus cell-start HeapAlloc; a kernel heap-workset proxy, not process RSS or cgroup memory",
		"total_alloc_delta_bytes":         "process-wide Go TotalAlloc increase during the isolated sequential cell",
		"construction_and_encode_ms":      "effect/prior construction plus canonical portable metrics and digests, outside the ANDNOT+OR latency samples",
	}
}

type replayRecord struct {
	observation string
	ledger      string
}

func runCell(config Config, name string, target int) (Cell, error) {
	runtime.GC()
	sampler := startHeapSampler()
	defer sampler.stop()
	constructionStarted := time.Now()
	overlapCardinality := config.Cardinality * uint64(target) / 100
	effectBuilder := ordinal.NewBuilder()
	priorBuilder := ordinal.NewBuilder()
	for index := uint64(0); index < config.Cardinality; index++ {
		ref := ordinal.FactRef{DictionaryDigest: kernelDictionary, SegmentID: "influence", Ordinal: generatedOrdinal(config, name, index)}
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
	effectMetric, err := bitmapMetrics(effect)
	if err != nil {
		return Cell{}, err
	}
	priorMetric, err := bitmapMetrics(prior)
	if err != nil {
		return Cell{}, err
	}
	observation := observationDigest(effectMetric.Digest, effectMetric.Cardinality)
	constructionElapsed := elapsedMS(time.Since(constructionStarted))

	novelSamples := make([]float64, 0, config.Runs)
	for run := 0; run < config.Runs; run++ {
		started := time.Now()
		delta := effect.Difference(prior)
		novelty := delta.Cardinality()
		updated := prior.Union(effect)
		updatedCardinality := updated.Cardinality()
		novelSamples = append(novelSamples, elapsedMS(time.Since(started)))
		if novelty != config.Cardinality-overlapCardinality || updatedCardinality != config.Cardinality {
			return Cell{}, errors.New("ANDNOT/OR cardinality invariant failed")
		}
		runtime.KeepAlive(delta)
		runtime.KeepAlive(updated)
	}
	novelDelta := effect.Difference(prior)
	updated := prior.Union(effect)
	replayDelta := effect.Difference(updated)
	novelMetric, err := bitmapMetrics(novelDelta)
	if err != nil {
		return Cell{}, err
	}
	updatedMetric, err := bitmapMetrics(updated)
	if err != nil {
		return Cell{}, err
	}
	replayMetric, err := bitmapMetrics(replayDelta)
	if err != nil {
		return Cell{}, err
	}

	committed := map[string]replayRecord{observation: {observation: observation, ledger: updatedMetric.Digest}}
	replaySamples := make([]float64, 0, config.Runs)
	replayObservation := ""
	for run := 0; run < config.Runs; run++ {
		started := time.Now()
		for lookup := 0; lookup < config.ReplayLookupsPerRun; lookup++ {
			record, found := committed[observation]
			if !found || record.observation != observation || record.ledger != updatedMetric.Digest {
				return Cell{}, errors.New("committed observation digest lookup failed")
			}
			replayObservation = record.observation
		}
		perLookup := elapsedMS(time.Since(started)) / float64(config.ReplayLookupsPerRun)
		replaySamples = append(replaySamples, perLookup)
	}
	sampler.sample()
	memory := sampler.stop()
	cell := Cell{
		Distribution:            name,
		TargetOverlapPercent:    target,
		ObservedOverlapPercent:  100 * float64(prior.Cardinality()) / float64(effect.Cardinality()),
		Effect:                  effectMetric,
		LedgerBefore:            priorMetric,
		NovelDelta:              novelMetric,
		LedgerAfter:             updatedMetric,
		ReplayDelta:             replayMetric,
		ObservationSHA256:       observation,
		ReplayObservationSHA256: replayObservation,
		ReplayMatched:           replayObservation == observation,
		NovelBitmapLatency:      latencyEvidence(novelSamples),
		ReplayDigestLookup:      latencyEvidence(replaySamples),
		ReplayLookupsPerRun:     config.ReplayLookupsPerRun,
		ConstructionAndEncodeMS: constructionElapsed,
		Memory:                  memory,
	}
	cell.DeterministicCellSHA256 = deterministicCellDigest(cell)
	runtime.KeepAlive(effect)
	runtime.KeepAlive(prior)
	runtime.KeepAlive(novelDelta)
	runtime.KeepAlive(updated)
	return cell, nil
}

func generatedOrdinal(config Config, name string, index uint64) uint32 {
	switch name {
	case "dense":
		return uint32(index)
	case "clustered":
		clusters := uint64(config.ClusterCount)
		cluster := index % clusters
		within := index / clusters
		stride := uint64(math.MaxUint32) / (clusters + 1)
		return uint32(cluster*stride + within)
	case "random_sparse":
		// Multiplication by an odd number is a permutation modulo 2^32, so
		// this produces a deterministic sparse-looking set without collisions.
		return uint32(uint64(uint32(index))*uint64(0x9e3779b1) + uint64(config.RandomSeed))
	default:
		panic("validated distribution name is unknown")
	}
}

func bitmapMetrics(set ordinal.BitmapSet) (BitmapMetrics, error) {
	containers, err := set.PortableContainers()
	if err != nil {
		return BitmapMetrics{}, err
	}
	metric := BitmapMetrics{Cardinality: set.Cardinality(), ContainerCount: len(containers)}
	var containerCardinality uint64
	for _, one := range containers {
		if uint64(len(one.Bitmap)) > math.MaxUint64-metric.PortableBytes {
			return BitmapMetrics{}, errors.New("portable bitmap byte count overflow")
		}
		metric.PortableBytes += uint64(len(one.Bitmap))
		containerCardinality += one.Cardinality
	}
	if containerCardinality != metric.Cardinality {
		return BitmapMetrics{}, errors.New("portable container cardinality differs from set")
	}
	metric.Digest, err = set.Digest()
	if err != nil {
		return BitmapMetrics{}, err
	}
	roundTrip, err := ordinal.ParsePortableContainers(containers)
	if err != nil {
		return BitmapMetrics{}, fmt.Errorf("parse canonical portable containers: %w", err)
	}
	if !set.Equal(roundTrip) {
		return BitmapMetrics{}, errors.New("portable container round trip changed the exact set")
	}
	metric.RoundTripDigest, err = roundTrip.Digest()
	if err != nil {
		return BitmapMetrics{}, fmt.Errorf("digest portable container round trip: %w", err)
	}
	if metric.RoundTripDigest != metric.Digest {
		return BitmapMetrics{}, errors.New("portable container round trip changed the set digest")
	}
	metric.PortableRoundTripVerified = true
	bounds := set.SegmentBounds()
	if len(bounds) > 0 {
		metric.HasOrdinals = true
		metric.MinimumOrdinal = bounds[0].MinOrdinal
		metric.MaximumOrdinal = bounds[0].MaxOrdinal
		for _, bound := range bounds[1:] {
			if bound.MinOrdinal < metric.MinimumOrdinal {
				metric.MinimumOrdinal = bound.MinOrdinal
			}
			if bound.MaxOrdinal > metric.MaximumOrdinal {
				metric.MaximumOrdinal = bound.MaxOrdinal
			}
		}
	}
	return metric, nil
}

func observationDigest(effectDigest string, cardinality uint64) string {
	hash := sha256.New()
	hash.Write([]byte(observationDigestDomain))
	hash.Write([]byte(effectDigest))
	hash.Write([]byte{0})
	hash.Write([]byte(strconv.FormatUint(cardinality, 10)))
	return hex.EncodeToString(hash.Sum(nil))
}

func deterministicCellDigest(cell Cell) string {
	hash := sha256.New()
	hash.Write([]byte(kernelDigestDomain))
	fields := []string{
		GeneratorVersion,
		cell.Distribution,
		strconv.Itoa(cell.TargetOverlapPercent),
		strconv.FormatUint(cell.Effect.Cardinality, 10), cell.Effect.Digest,
		strconv.FormatBool(cell.Effect.PortableRoundTripVerified), cell.Effect.RoundTripDigest,
		strconv.FormatUint(cell.LedgerBefore.Cardinality, 10), cell.LedgerBefore.Digest,
		strconv.FormatBool(cell.LedgerBefore.PortableRoundTripVerified), cell.LedgerBefore.RoundTripDigest,
		strconv.FormatUint(cell.NovelDelta.Cardinality, 10), cell.NovelDelta.Digest,
		strconv.FormatBool(cell.NovelDelta.PortableRoundTripVerified), cell.NovelDelta.RoundTripDigest,
		strconv.FormatUint(cell.LedgerAfter.Cardinality, 10), cell.LedgerAfter.Digest,
		strconv.FormatBool(cell.LedgerAfter.PortableRoundTripVerified), cell.LedgerAfter.RoundTripDigest,
		strconv.FormatUint(cell.ReplayDelta.Cardinality, 10), cell.ReplayDelta.Digest,
		strconv.FormatBool(cell.ReplayDelta.PortableRoundTripVerified), cell.ReplayDelta.RoundTripDigest,
		cell.ObservationSHA256,
	}
	for _, field := range fields {
		hash.Write([]byte(field))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func matrixDigest(cells []Cell) string {
	hash := sha256.New()
	hash.Write([]byte(GeneratorVersion))
	hash.Write([]byte{0})
	for _, cell := range cells {
		hash.Write([]byte(cell.DeterministicCellSHA256))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func latencyEvidence(samples []float64) LatencyEvidence {
	copy := append([]float64(nil), samples...)
	return LatencyEvidence{SamplesMS: copy, Summary: summarize(copy)}
}

func summarize(values []float64) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}
	values = append([]float64(nil), values...)
	sort.Float64s(values)
	var total float64
	for _, value := range values {
		total += value
	}
	return Distribution{Count: len(values), Min: values[0], P50: percentile(values, 0.50),
		P95: percentile(values, 0.95), P99: percentile(values, 0.99), Max: values[len(values)-1],
		Mean: total / float64(len(values))}
}

func percentile(sorted []float64, quantile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := quantile * float64(len(sorted)-1)
	lower, upper := int(math.Floor(position)), int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func elapsedMS(value time.Duration) float64 {
	return float64(value.Nanoseconds()) / float64(time.Millisecond)
}

type heapSampler struct {
	stopSignal chan struct{}
	done       chan struct{}
	start      runtime.MemStats
	peakAlloc  atomic.Uint64
	peakInuse  atomic.Uint64
	peakSys    atomic.Uint64
	stopOnce   sync.Once
	metrics    MemoryMetrics
}

func startHeapSampler() *heapSampler {
	result := &heapSampler{stopSignal: make(chan struct{}), done: make(chan struct{})}
	runtime.ReadMemStats(&result.start)
	result.peakAlloc.Store(result.start.HeapAlloc)
	result.peakInuse.Store(result.start.HeapInuse)
	result.peakSys.Store(result.start.HeapSys)
	go func() {
		defer close(result.done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				result.sample()
			case <-result.stopSignal:
				return
			}
		}
	}()
	return result
}

func (s *heapSampler) sample() {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	storeMaximum(&s.peakAlloc, memory.HeapAlloc)
	storeMaximum(&s.peakInuse, memory.HeapInuse)
	storeMaximum(&s.peakSys, memory.HeapSys)
}

func (s *heapSampler) stop() MemoryMetrics {
	s.stopOnce.Do(func() {
		s.sample()
		close(s.stopSignal)
		<-s.done
		var end runtime.MemStats
		runtime.ReadMemStats(&end)
		storeMaximum(&s.peakAlloc, end.HeapAlloc)
		storeMaximum(&s.peakInuse, end.HeapInuse)
		storeMaximum(&s.peakSys, end.HeapSys)
		peak := s.peakAlloc.Load()
		delta := uint64(0)
		if peak > s.start.HeapAlloc {
			delta = peak - s.start.HeapAlloc
		}
		total := uint64(0)
		if end.TotalAlloc >= s.start.TotalAlloc {
			total = end.TotalAlloc - s.start.TotalAlloc
		}
		s.metrics = MemoryMetrics{StartHeapAllocBytes: s.start.HeapAlloc, EndHeapAllocBytes: end.HeapAlloc,
			PeakHeapAllocBytes: peak, PeakHeapInuseBytes: s.peakInuse.Load(), PeakHeapSysBytes: s.peakSys.Load(),
			PeakDeltaBytes: delta, TotalAllocDelta: total}
	})
	return s.metrics
}

func storeMaximum(target *atomic.Uint64, value uint64) {
	for prior := target.Load(); value > prior; prior = target.Load() {
		if target.CompareAndSwap(prior, value) {
			return
		}
	}
}
