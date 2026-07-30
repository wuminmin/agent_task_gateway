package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

const (
	ordinalScaleDefaultRows   = 345_000
	ordinalScaleDefaultGroups = 12
)

type ordinalScaleMemory struct {
	RSSBytes        uint64 `json:"rss_bytes,omitempty"`
	ProcessHWMBytes uint64 `json:"process_hwm_bytes,omitempty"`
	HeapAllocBytes  uint64 `json:"heap_alloc_bytes"`
	HeapSysBytes    uint64 `json:"heap_sys_bytes"`
	TotalAllocBytes uint64 `json:"total_alloc_bytes"`
}

type ordinalScaleReport struct {
	SchemaVersion                 string             `json:"schema_version"`
	Status                        string             `json:"status"`
	MeasurementBoundary           string             `json:"measurement_boundary"`
	Rows                          int                `json:"rows"`
	Groups                        int                `json:"groups"`
	ExpectedInfluence             uint64             `json:"expected_influence"`
	ActualInfluence               uint64             `json:"actual_influence"`
	ExpectedDerivedRelease        int                `json:"expected_derived_release"`
	ActualDerivedRelease          int                `json:"actual_derived_release"`
	StaticRelease                 uint64             `json:"static_release"`
	FixtureGenerationMilliseconds float64            `json:"fixture_generation_ms"`
	OfflineIndexBuildMilliseconds float64            `json:"offline_index_build_ms"`
	HandlePreparationMilliseconds float64            `json:"handle_preparation_ms"`
	OnlineDerivationMilliseconds  float64            `json:"online_bitmap_derivation_ms"`
	MemoryBeforeIndex             ordinalScaleMemory `json:"memory_before_index"`
	MemoryAfterIndex              ordinalScaleMemory `json:"memory_after_index"`
	MemoryBeforeDerivation        ordinalScaleMemory `json:"memory_before_derivation"`
	MemoryAfterDerivation         ordinalScaleMemory `json:"memory_after_derivation"`
	DerivationSampledPeakRSSBytes uint64             `json:"derivation_sampled_peak_rss_bytes,omitempty"`
	CgroupMemoryPeakBytes         uint64             `json:"cgroup_memory_peak_bytes,omitempty"`
	RSSSampling                   string             `json:"rss_sampling"`
	GoVersion                     string             `json:"go_version"`
	GOMAXPROCS                    int                `json:"gomaxprocs"`
	HotArtifactBytes              int64              `json:"hot_artifact_bytes,omitempty"`
	HotActivationMilliseconds     float64            `json:"hot_activation_ms,omitempty"`
}

// TestOrdinalDerivationMillionInfluenceEvaluation is deliberately opt-in. It
// is a deterministic, in-process engineering measurement of the derivation
// kernel, not a public execute_plan benchmark and not evidence that the V4 SLO
// has been met. The default fixture has 345,000 rows and exactly three
// Influence facts per row: base-row, group cell, and aggregate-input cell.
func TestOrdinalDerivationMillionInfluenceEvaluation(t *testing.T) {
	if os.Getenv("TASKGATE_RUN_ORDINAL_SCALE") != "1" {
		t.Skip("set TASKGATE_RUN_ORDINAL_SCALE=1 to run the 1,035,000-Influence evaluation")
	}
	rowCount := ordinalScaleEnvInt(t, "TASKGATE_ORDINAL_SCALE_ROWS", ordinalScaleDefaultRows)
	groupCount := ordinalScaleEnvInt(t, "TASKGATE_ORDINAL_SCALE_GROUPS", ordinalScaleDefaultGroups)
	if rowCount%groupCount != 0 {
		t.Fatalf("rows (%d) must be exactly divisible by groups (%d)", rowCount, groupCount)
	}
	rowsPerGroup := rowCount / groupCount
	report := ordinalScaleReport{
		SchemaVersion:       "taskgate-ordinal-derivation-evaluation-v1",
		Status:              "engineering_measurement_only",
		MeasurementBoundary: "in-process warm-HOT ordinal derivation; excludes Business SQL, connector I/O, settlement, replay, cgroup RSS, and queueing",
		Rows:                rowCount, Groups: groupCount, ExpectedInfluence: uint64(rowCount) * 3,
		ExpectedDerivedRelease: groupCount, RSSSampling: "/proc/self/status VmRSS sampled every 4096 provenance rows",
		GoVersion: runtime.Version(), GOMAXPROCS: runtime.GOMAXPROCS(0),
	}

	product := queryplan.Product{
		Name: "ordinal_scale", Columns: map[string]struct{}{"amount": {}, "group_key": {}},
		AllowedAggregates: map[string]struct{}{"sum": {}},
		ColumnTypes:       map[string]string{"amount": "numeric", "group_key": "text"},
		ColumnCollations:  map[string]string{"group_key": "C"},
		CollationVersions: map[string]string{"group_key": "builtin"},
		SourceNamespace:   "evaluation.ordinal-scale", Snapshot: "snapshot-v1", StableRole: "scale",
		StableEntityKey: []string{"amount"}, SnapshotPublication: "ordinal-scale-publication-v1",
		SidecarManifestDigest: strings.Repeat("a", 64),
	}
	compilation, err := queryplan.CompileOrdinal(queryplan.QueryPlan{
		Product: product.Name, GroupBy: []string{"group_key"},
		Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "amount", Alias: "total"}},
	}, product)
	if err != nil {
		t.Fatal(err)
	}
	program := compilation.OrdinalProgram
	if len(program.Sources) != 1 || len(program.Visible) != 1 || len(program.Groups) != 1 || len(program.Aggregates) != 1 {
		t.Fatalf("unexpected scale program shape: sources=%d visible=%d groups=%d aggregates=%d",
			len(program.Sources), len(program.Visible), len(program.Groups), len(program.Aggregates))
	}
	source := program.Sources[0]

	fields := make([]ordinal.SnapshotField, 0, len(source.EvidenceFields))
	for _, binding := range source.EvidenceFields {
		fields = append(fields, ordinal.SnapshotField{Name: binding.Column,
			CanonicalFieldID: binding.FieldID, SQLType: binding.SQLType})
	}
	var artifact ordinal.CompiledArtifact
	importPath := strings.TrimSpace(os.Getenv("TASKGATE_ORDINAL_SCALE_IMPORT_HOT"))
	if importPath != "" {
		report.Status = "engineering_measurement_online_process"
		report.MeasurementBoundary = "fresh-process warm-HOT ordinal derivation; excludes Business SQL, connector I/O, settlement, replay, and queueing"
		report.MemoryBeforeIndex = ordinalScaleMemorySample()
		activationStarted := time.Now()
		encoded, readErr := os.ReadFile(importPath)
		manifestBytes, manifestErr := os.ReadFile(importPath + ".manifest")
		if readErr != nil || manifestErr != nil {
			t.Fatalf("read exported HOT artifact: %v / %v", readErr, manifestErr)
		}
		report.HotArtifactBytes = int64(len(encoded))
		hot, parseErr := ordinal.ParseHotDictionary(encoded, strings.TrimSpace(string(manifestBytes)))
		if parseErr != nil {
			t.Fatalf("parse exported HOT artifact: %v", parseErr)
		}
		artifact.Hot = hot
		report.HotActivationMilliseconds = ordinalMilliseconds(time.Since(activationStarted))
		encoded = nil
		manifestBytes = nil
		runtime.GC()
		debug.FreeOSMemory()
		report.MemoryAfterIndex = ordinalScaleMemorySample()
	} else {
		fixtureStarted := time.Now()
		snapshotRows := make([]ordinal.SnapshotRow, rowCount)
		for rowIndex := 0; rowIndex < rowCount; rowIndex++ {
			amount := int64(rowIndex + 1)
			values := map[string]any{"amount": amount, "group_key": ordinalScaleGroup(rowIndex / rowsPerGroup)}
			entityKey, keyErr := ordinalScaleEntityKey(source, values)
			if keyErr != nil {
				t.Fatalf("fixture entity %d: %v", rowIndex, keyErr)
			}
			snapshotRows[rowIndex] = ordinal.SnapshotRow{EntityKey: entityKey, Values: values}
		}
		report.FixtureGenerationMilliseconds = ordinalMilliseconds(time.Since(fixtureStarted))
		report.MemoryBeforeIndex = ordinalScaleMemorySample()

		indexStarted := time.Now()
		artifact, err = ordinal.CompileSnapshotArtifact(ordinal.SnapshotSpec{
			SourceID: "ordinal-scale-source", SourceNamespace: source.SourceNamespace, Snapshot: source.Snapshot,
			SchemaDigest: strings.Repeat("1", 64), Fields: fields, Rows: snapshotRows,
		})
		if err != nil {
			t.Fatal(err)
		}
		report.OfflineIndexBuildMilliseconds = ordinalMilliseconds(time.Since(indexStarted))
		report.MemoryAfterIndex = ordinalScaleMemorySample()
		exportPath := strings.TrimSpace(os.Getenv("TASKGATE_ORDINAL_SCALE_EXPORT_HOT"))
		if exportPath != "" {
			encoded, encodeErr := artifact.Hot.MarshalBinary()
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			report.HotArtifactBytes = int64(len(encoded))
			if writeErr := ordinalScaleWriteExclusive(exportPath, encoded, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			if writeErr := ordinalScaleWriteExclusive(exportPath+".manifest", []byte(artifact.Hot.ManifestDigest()+"\n"), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			encoded = nil
		}

		// The cold dictionary and compiler input are offline artifacts. Drop
		// their references before measuring the online warm-HOT path.
		artifact.Cold = nil
		snapshotRows = nil
		runtime.GC()
		// Production compiles the index in a one-shot builder container and loads
		// HOT in a separate Gateway process. Return the builder-only pages here so
		// the online RSS sample models that boundary in the combined run.
		debug.FreeOSMemory()
	}
	for sourceIndex := range program.Sources {
		program.Sources[sourceIndex].SidecarBinding.ManifestDigest = artifact.Hot.ManifestDigest()
	}
	for bindingIndex := range program.SnapshotBundle {
		program.SnapshotBundle[bindingIndex].SidecarManifestDigest = artifact.Hot.ManifestDigest()
	}
	if err := program.ValidateBoundSidecars(); err != nil {
		t.Fatal(err)
	}

	handleStarted := time.Now()
	handles := make([]ordinal.RowHandle, rowCount)
	for rowIndex := 0; rowIndex < rowCount; rowIndex++ {
		amount := int64(rowIndex + 1)
		entityKey, keyErr := ordinalScaleEntityKey(source, map[string]any{"amount": amount})
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		handle, found := artifact.Hot.LookupRowHandle(entityKey)
		if !found {
			t.Fatalf("HOT index omits row %d", rowIndex)
		}
		handles[rowIndex] = handle
	}
	report.HandlePreparationMilliseconds = ordinalMilliseconds(time.Since(handleStarted))

	visibleRows := make([]map[string]any, groupCount)
	for groupIndex := 0; groupIndex < groupCount; groupIndex++ {
		first := int64(groupIndex*rowsPerGroup + 1)
		last := int64((groupIndex + 1) * rowsPerGroup)
		total := (first + last) * int64(rowsPerGroup) / 2
		visibleRows[groupIndex] = groupedVisibleValues(program, ordinalScaleGroup(groupIndex), total)
	}
	visible := ordinalVisibleResult(program, visibleRows)
	columns, positions := ordinalProvenanceColumns(program)
	values := make([]any, len(columns))
	indexes := map[string]ordinal.SnapshotIndex{source.SourceAlias: artifact.Hot}

	runtime.GC()
	report.MemoryBeforeDerivation = ordinalScaleMemorySample()
	sampledPeakRSS := report.MemoryBeforeDerivation.RSSBytes
	derivationStarted := time.Now()
	deriver, err := newOrdinalDeriver(program, indexes, visible, ordinalDerivationPlanDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := deriver.Begin(context.Background(), columns); err != nil {
		t.Fatal(err)
	}
	for rowIndex := 0; rowIndex < rowCount; rowIndex++ {
		amount := int64(rowIndex + 1)
		group := ordinalScaleGroup(rowIndex / rowsPerGroup)
		values[positions[source.HandleAlias]] = uint64(handles[rowIndex])
		for _, binding := range source.EvidenceFields {
			switch binding.Column {
			case "amount":
				values[positions[binding.ProvenanceAlias]] = amount
			case "group_key":
				values[positions[binding.ProvenanceAlias]] = group
			default:
				t.Fatalf("unexpected evidence column %q", binding.Column)
			}
		}
		if err := deriver.Row(context.Background(), values); err != nil {
			t.Fatalf("provenance row %d: %v", rowIndex, err)
		}
		if rowIndex&4095 == 0 {
			if rss := ordinalScaleCurrentRSS(); rss > sampledPeakRSS {
				sampledPeakRSS = rss
			}
		}
	}
	effect, err := deriver.Finish()
	if err != nil {
		t.Fatal(err)
	}
	report.OnlineDerivationMilliseconds = ordinalMilliseconds(time.Since(derivationStarted))
	report.MemoryAfterDerivation = ordinalScaleMemorySample()
	if report.MemoryAfterDerivation.RSSBytes > sampledPeakRSS {
		sampledPeakRSS = report.MemoryAfterDerivation.RSSBytes
	}
	report.DerivationSampledPeakRSSBytes = sampledPeakRSS
	report.CgroupMemoryPeakBytes = ordinalScaleCgroupPeak()
	report.ActualInfluence = effect.Influence.Cardinality()
	report.ActualDerivedRelease = len(effect.DerivedRelease)
	report.StaticRelease = effect.Release.Cardinality()
	if report.ActualInfluence != report.ExpectedInfluence ||
		report.ActualDerivedRelease != report.ExpectedDerivedRelease || report.StaticRelease != 0 {
		t.Fatalf("scale cardinality mismatch: influence=%d/%d derived release=%d/%d static release=%d",
			report.ActualInfluence, report.ExpectedInfluence, report.ActualDerivedRelease,
			report.ExpectedDerivedRelease, report.StaticRelease)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("TASKGATE_ORDINAL_DERIVATION_REPORT=%s", encoded)
}

func ordinalScaleEnvInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		t.Fatalf("%s must be a positive integer", name)
	}
	return parsed
}

func ordinalScaleGroup(index int) string { return fmt.Sprintf("g%08d", index) }

func ordinalScaleWriteExclusive(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func ordinalScaleEntityKey(source queryplan.OrdinalSource, values map[string]any) (string, error) {
	components := make([]string, 0, 1+3*len(source.EntityKey))
	components = append(components, source.SourceNamespace)
	for _, binding := range source.EntityKey {
		value, present := values[binding.Column]
		if !present {
			return "", fmt.Errorf("entity-key value %q is absent", binding.Column)
		}
		canonical, err := exposure.CanonicalSQLValue(binding.SQLType, value)
		if err != nil {
			return "", err
		}
		components = append(components, binding.Column, binding.SQLType, canonical)
	}
	return exposure.ComposeCanonicalKeyV2("base-entity", components...)
}

func ordinalMilliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func ordinalScaleMemorySample() ordinalScaleMemory {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	rss, hwm := ordinalScaleProcStatus()
	return ordinalScaleMemory{RSSBytes: rss, ProcessHWMBytes: hwm, HeapAllocBytes: memory.HeapAlloc,
		HeapSysBytes: memory.HeapSys, TotalAllocBytes: memory.TotalAlloc}
}

func ordinalScaleCurrentRSS() uint64 {
	rss, _ := ordinalScaleProcStatus()
	return rss
}

func ordinalScaleCgroupPeak() uint64 {
	encoded, err := os.ReadFile("/sys/fs/cgroup/memory.peak")
	if err != nil {
		return 0
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(encoded)), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func ordinalScaleProcStatus() (rss uint64, hwm uint64) {
	encoded, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(encoded), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil {
			continue
		}
		switch fields[0] {
		case "VmRSS:":
			rss = value * 1024
		case "VmHWM:":
			hwm = value * 1024
		}
	}
	return rss, hwm
}
