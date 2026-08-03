package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
)

type ObserverSnapshot struct {
	SchemaVersion          int    `json:"schema_version"`
	MemoryScope            string `json:"memory_scope"`
	Phase                  string `json:"phase"`
	RuntimeIdentitySHA256  string `json:"runtime_identity_sha256"`
	GatewayMemoryPeakBytes int64  `json:"gateway_memory_peak_bytes"`
	GatewayCPUUsec         int64  `json:"gateway_cpu_usec"`
	GatewayNetworkRXBytes  int64  `json:"gateway_network_rx_bytes"`
	GatewayNetworkTXBytes  int64  `json:"gateway_network_tx_bytes"`
	BusinessSQLQueries     int64  `json:"business_sql_queries"`
	ControlWALBytes        int64  `json:"control_wal_bytes"`
	BusinessWALBytes       int64  `json:"business_wal_bytes"`
	OOMEvents              int64  `json:"oom_events"`
	ContainerRestarts      int64  `json:"container_restarts"`
}

type ObserverDelta struct {
	GatewayMemoryPeakBytes int64
	GatewayCPUUsecDelta    int64
	GatewayNetworkRXDelta  int64
	GatewayNetworkTXDelta  int64
	BusinessSQLDelta       int64
	ControlWALBytesDelta   int64
	BusinessWALBytesDelta  int64
	OOMDelta               int64
	ContainerRestartDelta  int64
}

func RunObserver(ctx context.Context, argv []string, environment []string) (ObserverSnapshot, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return ObserverSnapshot{}, errors.New("observer exact argv is required")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = environment
	value, err := command.Output()
	if err != nil {
		return ObserverSnapshot{}, err
	}
	var snapshot ObserverSnapshot
	if err := StrictJSON(value, &snapshot); err != nil {
		return snapshot, err
	}
	if err := validateObserverSnapshot(snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func DifferenceObserver(before, after ObserverSnapshot) (ObserverDelta, error) {
	if err := validateObserverSnapshot(before); err != nil {
		return ObserverDelta{}, err
	}
	if err := validateObserverSnapshot(after); err != nil {
		return ObserverDelta{}, err
	}
	if before.RuntimeIdentitySHA256 != after.RuntimeIdentitySHA256 {
		return ObserverDelta{}, errors.New("observer runtime identity changed")
	}
	if before.Phase != "before" || after.Phase != "after" {
		return ObserverDelta{}, errors.New("observer phase transition is invalid")
	}
	if after.GatewayMemoryPeakBytes < before.GatewayMemoryPeakBytes {
		return ObserverDelta{}, errors.New("observer memory peak regressed")
	}
	if after.GatewayCPUUsec < before.GatewayCPUUsec || after.GatewayNetworkRXBytes < before.GatewayNetworkRXBytes || after.GatewayNetworkTXBytes < before.GatewayNetworkTXBytes || after.BusinessSQLQueries < before.BusinessSQLQueries || after.ControlWALBytes < before.ControlWALBytes || after.BusinessWALBytes < before.BusinessWALBytes || after.OOMEvents < before.OOMEvents || after.ContainerRestarts < before.ContainerRestarts {
		return ObserverDelta{}, errors.New("observer counter regressed")
	}
	return ObserverDelta{GatewayMemoryPeakBytes: after.GatewayMemoryPeakBytes, GatewayCPUUsecDelta: after.GatewayCPUUsec - before.GatewayCPUUsec, GatewayNetworkRXDelta: after.GatewayNetworkRXBytes - before.GatewayNetworkRXBytes, GatewayNetworkTXDelta: after.GatewayNetworkTXBytes - before.GatewayNetworkTXBytes, BusinessSQLDelta: after.BusinessSQLQueries - before.BusinessSQLQueries, ControlWALBytesDelta: after.ControlWALBytes - before.ControlWALBytes, BusinessWALBytesDelta: after.BusinessWALBytes - before.BusinessWALBytes, OOMDelta: after.OOMEvents - before.OOMEvents, ContainerRestartDelta: after.ContainerRestarts - before.ContainerRestarts}, nil
}

func validateObserverSnapshot(snapshot ObserverSnapshot) error {
	if snapshot.SchemaVersion != 1 || snapshot.MemoryScope != "cgroup_v2_memory_peak_including_mmap" ||
		(snapshot.Phase != "before" && snapshot.Phase != "after") || !validSHA256(snapshot.RuntimeIdentitySHA256) {
		return errors.New("observer scope/schema/runtime identity mismatch")
	}
	if snapshot.GatewayMemoryPeakBytes <= 0 || snapshot.GatewayCPUUsec < 0 || snapshot.GatewayNetworkRXBytes < 0 ||
		snapshot.GatewayNetworkTXBytes < 0 || snapshot.BusinessSQLQueries < 0 || snapshot.ControlWALBytes <= 0 ||
		snapshot.BusinessWALBytes <= 0 || snapshot.OOMEvents < 0 || snapshot.ContainerRestarts < 0 {
		return errors.New("observer snapshot contains an invalid counter")
	}
	return nil
}

func observerJSON(snapshot ObserverSnapshot) ([]byte, error) { return json.Marshal(snapshot) }
