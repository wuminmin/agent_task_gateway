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
	if snapshot.SchemaVersion != 1 || snapshot.MemoryScope != "cgroup_v2_memory_peak_including_mmap" {
		return snapshot, errors.New("observer scope/schema mismatch")
	}
	return snapshot, nil
}

func DifferenceObserver(before, after ObserverSnapshot) (ObserverDelta, error) {
	if after.GatewayCPUUsec < before.GatewayCPUUsec || after.GatewayNetworkRXBytes < before.GatewayNetworkRXBytes || after.GatewayNetworkTXBytes < before.GatewayNetworkTXBytes || after.BusinessSQLQueries < before.BusinessSQLQueries || after.ControlWALBytes < before.ControlWALBytes || after.BusinessWALBytes < before.BusinessWALBytes || after.OOMEvents < before.OOMEvents || after.ContainerRestarts < before.ContainerRestarts {
		return ObserverDelta{}, errors.New("observer counter regressed")
	}
	return ObserverDelta{GatewayMemoryPeakBytes: after.GatewayMemoryPeakBytes, GatewayCPUUsecDelta: after.GatewayCPUUsec - before.GatewayCPUUsec, GatewayNetworkRXDelta: after.GatewayNetworkRXBytes - before.GatewayNetworkRXBytes, GatewayNetworkTXDelta: after.GatewayNetworkTXBytes - before.GatewayNetworkTXBytes, BusinessSQLDelta: after.BusinessSQLQueries - before.BusinessSQLQueries, ControlWALBytesDelta: after.ControlWALBytes - before.ControlWALBytes, BusinessWALBytesDelta: after.BusinessWALBytes - before.BusinessWALBytes, OOMDelta: after.OOMEvents - before.OOMEvents, ContainerRestartDelta: after.ContainerRestarts - before.ContainerRestarts}, nil
}

func observerJSON(snapshot ObserverSnapshot) ([]byte, error) { return json.Marshal(snapshot) }
