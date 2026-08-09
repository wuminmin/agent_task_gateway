package legacyv14

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

// DecodeObserverSnapshot strictly decodes one archived schema-v1 snapshot.
// It is intentionally bytes-only: the legacy package cannot launch the live
// observer, and a v2/v1.5 document is rejected rather than treated as v1.
func DecodeObserverSnapshot(value []byte) (ObserverSnapshot, error) {
	var snapshot ObserverSnapshot
	if err := strictJSON(value, &snapshot); err != nil {
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

func strictJSON(value []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}
