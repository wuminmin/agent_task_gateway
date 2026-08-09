package legacyv14

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLegacyDecoderAcceptsOnlyTheSchemaV1WireShape(t *testing.T) {
	legacy := validObserverSnapshotForTest()
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeObserverSnapshot(encoded); err != nil {
		t.Fatalf("the archived schema-v1 snapshot did not decode: %v", err)
	}

	// This is a valid v1 body plus a v2-only identity. DisallowUnknownFields is
	// therefore the check that rejects it; merely validating the legacy members
	// after a permissive decode would accept a v1.5 document wearing a v1 body.
	var hybrid map[string]any
	if err := json.Unmarshal(encoded, &hybrid); err != nil {
		t.Fatal(err)
	}
	hybrid["version"] = "taskgate-final-v5-observer-snapshot-v2"
	encoded, _ = json.Marshal(hybrid)
	if _, err := DecodeObserverSnapshot(encoded); err == nil {
		t.Fatal("the legacy decoder accepted a snapshot carrying a v2-only member")
	}

	// A representative current document is independently rejected by its wire
	// version/schema even before any current Sample can see it.
	current := []byte(`{"version":"taskgate-final-v5-observer-snapshot-v2","schema_version":2,"phase":"after"}`)
	if _, err := DecodeObserverSnapshot(current); err == nil {
		t.Fatal("the legacy decoder accepted a v2/v1.5 observer document")
	}
}

func TestDifferenceObserverRequiresStableRuntimeIdentityAndMemoryPeak(t *testing.T) {
	before := validObserverSnapshotForTest()
	after := before
	after.Phase = "after"
	after.GatewayMemoryPeakBytes++
	after.GatewayCPUUsec++
	after.GatewayNetworkRXBytes++
	after.GatewayNetworkTXBytes++
	after.BusinessSQLQueries++
	after.ControlWALBytes++
	after.BusinessWALBytes++
	if _, err := DifferenceObserver(before, after); err != nil {
		t.Fatalf("valid observer transition rejected: %v", err)
	}

	replaced := after
	replaced.RuntimeIdentitySHA256 = strings.Repeat("b", 64)
	if _, err := DifferenceObserver(before, replaced); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("container replacement identity was accepted: %v", err)
	}

	regressedPeak := after
	regressedPeak.GatewayMemoryPeakBytes = before.GatewayMemoryPeakBytes - 1
	if _, err := DifferenceObserver(before, regressedPeak); err == nil || !strings.Contains(err.Error(), "memory peak regressed") {
		t.Fatalf("cgroup memory peak regression was accepted: %v", err)
	}
}

func TestDifferenceObserverRejectsMissingIdentityAndInvalidCounters(t *testing.T) {
	before := validObserverSnapshotForTest()
	after := before
	after.Phase = "after"
	before.RuntimeIdentitySHA256 = ""
	if _, err := DifferenceObserver(before, after); err == nil {
		t.Fatal("missing runtime identity was accepted")
	}
	before = validObserverSnapshotForTest()
	before.ControlWALBytes = 0
	if _, err := DifferenceObserver(before, after); err == nil {
		t.Fatal("missing real WAL position was accepted")
	}
}

func validObserverSnapshotForTest() ObserverSnapshot {
	return ObserverSnapshot{
		SchemaVersion: 1, MemoryScope: "cgroup_v2_memory_peak_including_mmap",
		Phase: "before", RuntimeIdentitySHA256: strings.Repeat("a", 64), GatewayMemoryPeakBytes: 1024,
		GatewayCPUUsec: 100, GatewayNetworkRXBytes: 200, GatewayNetworkTXBytes: 300,
		BusinessSQLQueries: 4, ControlWALBytes: 500, BusinessWALBytes: 600,
	}
}
