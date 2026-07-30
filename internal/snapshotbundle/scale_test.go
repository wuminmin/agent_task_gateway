package snapshotbundle

import (
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const snapshotBundleScaleRows = 345_000

type snapshotBundleScaleReport struct {
	SchemaVersion string  `json:"schema_version"`
	Status        string  `json:"status"`
	Rows          int     `json:"rows"`
	Facts         int     `json:"facts"`
	FixtureMS     float64 `json:"fixture_ms"`
	CompileMS     float64 `json:"compile_verify_ms"`
	PeakRSSBytes  uint64  `json:"peak_rss_bytes"`
	HotBytes      int64   `json:"hot_bytes"`
	ColdBytes     int64   `json:"cold_bytes"`
	SidecarBytes  int64   `json:"sidecar_bytes"`
	ManifestBytes int64   `json:"bundle_manifest_bytes"`
	TotalBytes    int64   `json:"total_bytes"`
	GoVersion     string  `json:"go_version"`
}

// TestSnapshotPublicationScaleEvaluation exercises the complete production
// compiler, serialization, re-parse verification, and sidecar verification
// path. It is opt-in because the canonical cold dictionary is intentionally an
// offline, multi-gigabyte-RSS workload at the million-fact point.
func TestSnapshotPublicationScaleEvaluation(t *testing.T) {
	if os.Getenv("TASKGATE_RUN_SNAPSHOT_SCALE") != "1" {
		t.Skip("set TASKGATE_RUN_SNAPSHOT_SCALE=1 to run the complete publication build evaluation")
	}
	rows := snapshotBundleScaleRows
	if raw := strings.TrimSpace(os.Getenv("TASKGATE_SNAPSHOT_SCALE_ROWS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("TASKGATE_SNAPSHOT_SCALE_ROWS must be positive")
		}
		rows = parsed
	}
	report := snapshotBundleScaleReport{SchemaVersion: "taskgate-snapshot-publication-evaluation-v1",
		Status: "engineering_measurement_only", Rows: rows, Facts: rows * 3, GoVersion: runtime.Version()}

	fixtureStarted := time.Now()
	input := CompilerInput{Version: CompilerInputVersion, PublicationName: "ordinal_scale_v1",
		CatalogSource: "ordinal_scale", OrdinalSidecar: "taskgate_ordinal.ordinal_scale_v1",
		EntityKeyFields: []string{"amount"}, Snapshot: SnapshotInput{
			SourceID: "ordinal_scale", SourceNamespace: "evaluation.ordinal-scale", Snapshot: "snapshot-v1",
			SchemaDigest: strings.Repeat("1", 64), Fields: []SnapshotField{
				{Name: "amount", SQLType: "numeric"},
				{Name: "group_key", SQLType: "text", Collation: "C", CollationVersion: "builtin"},
			}, Rows: make([]SnapshotRow, rows),
		}}
	for index := 0; index < rows; index++ {
		input.Snapshot.Rows[index] = SnapshotRow{Values: map[string]any{
			"amount": int64(index + 1), "group_key": "g" + strconv.Itoa(index%12),
		}}
	}
	report.FixtureMS = float64(time.Since(fixtureStarted)) / float64(time.Millisecond)
	runtime.GC()

	compileStarted := time.Now()
	bundle, err := Compile(input)
	report.CompileMS = float64(time.Since(compileStarted)) / float64(time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := bundle.ManifestJSON()
	if err != nil {
		t.Fatal(err)
	}
	report.HotBytes = int64(len(bundle.Hot))
	report.ColdBytes = int64(len(bundle.Cold))
	report.SidecarBytes = int64(len(bundle.Sidecar))
	report.ManifestBytes = int64(len(manifest))
	report.TotalBytes = report.HotBytes + report.ColdBytes + report.SidecarBytes + report.ManifestBytes
	_, report.PeakRSSBytes = snapshotScaleProcMemory()

	if bundle.Manifest.RowCount != uint64(rows) || report.HotBytes > 160<<20 || report.TotalBytes > 2<<30 ||
		report.PeakRSSBytes > 4<<30 || report.CompileMS > float64((10*time.Minute)/time.Millisecond) {
		t.Fatalf("snapshot publication scale gate failed: %+v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("TASKGATE_SNAPSHOT_PUBLICATION_REPORT=%s", encoded)
}

func snapshotScaleProcMemory() (rss uint64, hwm uint64) {
	content, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
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
