package experiment

import (
	"strings"
	"testing"
)

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
