package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const testProject = "taskgate-final-v5-deployment-02-0123456789abcdefabcd"

type fakeEngine struct {
	services      map[string]serviceIdentity
	resources     gatewayResourceSnapshot
	outputs       map[string][]byte
	restarts      []projectRestartSnapshot
	restartReads  int
	execCommands  map[string][]string
	resolveErrors map[string]error
}

func (f *fakeEngine) resolveService(_ context.Context, project, service string) (serviceIdentity, error) {
	if err := f.resolveErrors[service]; err != nil {
		return serviceIdentity{}, err
	}
	return f.services[project+"/"+service], nil
}

func (f *fakeEngine) readGatewayResources(context.Context, serviceIdentity, string) (gatewayResourceSnapshot, error) {
	return f.resources, nil
}

func (f *fakeEngine) exec(_ context.Context, container string, command []string) ([]byte, error) {
	if f.execCommands == nil {
		f.execCommands = make(map[string][]string)
	}
	f.execCommands[container] = append([]string(nil), command...)
	return f.outputs[container], nil
}

func (f *fakeEngine) restartSnapshot(context.Context, string) (projectRestartSnapshot, error) {
	index := f.restartReads
	f.restartReads++
	if index >= len(f.restarts) {
		index = len(f.restarts) - 1
	}
	return f.restarts[index], nil
}

func TestCollectEmitsStrictRealSnapshot(t *testing.T) {
	fake := completeFakeEngine()
	got, err := collect(context.Background(), fake, testProject, "before")
	if err != nil {
		t.Fatal(err)
	}
	runtimeIdentity, err := runtimeIdentitySHA256(testProject, fake.restarts[1],
		fake.services[testProject+"/"+gatewayService], fake.services[testProject+"/"+businessService],
		fake.services[testProject+"/"+controlService])
	if err != nil {
		t.Fatal(err)
	}
	want := experiment.ObserverSnapshot{
		SchemaVersion:          1,
		MemoryScope:            memoryScope,
		Phase:                  "before",
		RuntimeIdentitySHA256:  runtimeIdentity,
		GatewayMemoryPeakBytes: 268435456,
		GatewayCPUUsec:         987654,
		GatewayNetworkRXBytes:  404,
		GatewayNetworkTXBytes:  606,
		BusinessSQLQueries:     42,
		ControlWALBytes:        68157440,
		BusinessWALBytes:       73400320,
		OOMEvents:              2,
		ContainerRestarts:      3,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot differs\n got: %+v\nwant: %+v", got, want)
	}
	for container, wantScript := range map[string]string{
		"business-id": businessSnapshotShell,
		"control-id":  controlSnapshotShell,
	} {
		if command := fake.execCommands[container]; !reflect.DeepEqual(command, []string{"sh", "-c", wantScript}) {
			t.Fatalf("unexpected %s command: %#v", container, command)
		}
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := []string{
		"schema_version", "memory_scope", "phase", "runtime_identity_sha256", "gateway_memory_peak_bytes", "gateway_cpu_usec",
		"gateway_network_rx_bytes", "gateway_network_tx_bytes", "business_sql_queries",
		"control_wal_bytes", "business_wal_bytes", "oom_events", "container_restarts",
	}
	if len(fields) != len(wantFields) {
		t.Fatalf("flat observer JSON has %d fields, want %d: %s", len(fields), len(wantFields), raw)
	}
	for _, field := range wantFields {
		if _, present := fields[field]; !present {
			t.Fatalf("flat observer JSON omitted %q: %s", field, raw)
		}
	}
}

func TestCollectFailsClosedWhenRestartChangesDuringSnapshot(t *testing.T) {
	fake := completeFakeEngine()
	fake.restarts = []projectRestartSnapshot{
		restartSnapshot(map[string]int64{"gateway-id": 2, "business-id": 1, "control-id": 0}),
		restartSnapshot(map[string]int64{"gateway-id": 3, "business-id": 1, "control-id": 0}),
	}
	_, err := collect(context.Background(), fake, testProject, "before")
	if err == nil || !strings.Contains(err.Error(), "restarted or was replaced") {
		t.Fatalf("expected restart race to fail closed, got %v", err)
	}
}

func TestCollectFailsClosedWhenContainerIdentityChangesAtSameRestartTotal(t *testing.T) {
	fake := completeFakeEngine()
	fake.restarts = []projectRestartSnapshot{
		restartSnapshot(map[string]int64{"gateway-id": 2, "business-id": 1, "control-id": 0}),
		restartSnapshot(map[string]int64{"gateway-new": 2, "business-id": 1, "control-id": 0}),
	}
	_, err := collect(context.Background(), fake, testProject, "before")
	if err == nil || !strings.Contains(err.Error(), "restarted or was replaced") {
		t.Fatalf("expected same-total container replacement to fail closed, got %v", err)
	}
}

func TestCollectFailsClosedOnMissingCPUCounter(t *testing.T) {
	fake := completeFakeEngine()
	fake.resources.cpuUsec = 0
	_, err := collect(context.Background(), fake, testProject, "before")
	if err == nil || !strings.Contains(err.Error(), "omitted a real Gateway CPU/network counter") {
		t.Fatalf("expected missing real CPU counter to fail closed, got %v", err)
	}
}

func TestCollectFailsClosedOnMissingBusinessRoleValue(t *testing.T) {
	fake := completeFakeEngine()
	// PostgreSQL renders the CASE NULL as an empty first field. Treating that
	// as zero would turn a missing measured role into synthetic evidence.
	fake.outputs["business-id"] = []byte("|73400320\n")
	_, err := collect(context.Background(), fake, testProject, "before")
	if err == nil || !strings.Contains(err.Error(), "Business SQL calls") {
		t.Fatalf("expected missing Business counter to fail closed, got %v", err)
	}
}

func TestCollectFailsClosedOnZeroMemoryPeakOrWAL(t *testing.T) {
	for name, mutate := range map[string]func(*fakeEngine){
		"memory": func(fake *fakeEngine) {
			fake.resources.memoryPeak = []byte("0\n")
		},
		"business WAL": func(fake *fakeEngine) { fake.outputs["business-id"] = []byte("42|0\n") },
		"control WAL":  func(fake *fakeEngine) { fake.outputs["control-id"] = []byte("0\n") },
	} {
		t.Run(name, func(t *testing.T) {
			fake := completeFakeEngine()
			mutate(fake)
			if _, err := collect(context.Background(), fake, testProject, "before"); err == nil {
				t.Fatal("missing real counter was accepted")
			}
		})
	}
}

func TestCollectRejectsNonFormalProjectAndDuplicateServices(t *testing.T) {
	if _, err := collect(context.Background(), completeFakeEngine(), "other-project", "before"); err == nil {
		t.Fatal("non-formal Compose project was accepted")
	}
	fake := completeFakeEngine()
	fake.services[testProject+"/"+businessService] = fake.services[testProject+"/"+gatewayService]
	if _, err := collect(context.Background(), fake, testProject, "before"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate service container was accepted: %v", err)
	}
}

func TestDockerSocketAcceptsOnlyUnixEndpoint(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"default":  {},
		"absolute": {"DOCKER_HOST": "unix:///run/user/1000/docker.sock"},
	} {
		t.Run(name, func(t *testing.T) {
			path, err := dockerSocket(mapEnvironment(values))
			if err != nil || path == "" {
				t.Fatalf("valid Docker socket rejected: %q %v", path, err)
			}
		})
	}
	for _, endpoint := range []string{
		"tcp://127.0.0.1:2375", "ssh://host/run/docker.sock", "unix://relative.sock", "/run/docker.sock",
	} {
		if _, err := dockerSocket(mapEnvironment(map[string]string{"DOCKER_HOST": endpoint})); err == nil {
			t.Fatalf("unsafe/invalid Docker endpoint %q was accepted", endpoint)
		}
	}
}

func TestRunRejectsArgumentsAndNonFormalProjectBeforeDockerAccess(t *testing.T) {
	for _, args := range [][]string{{"observer"}, {"observer", "--phase", "during"}, {"observer", "--phase=before"}} {
		if err := run(args, mapEnvironment(nil), &strings.Builder{}); err == nil {
			t.Fatalf("invalid observer arguments were accepted: %#v", args)
		}
	}
	if err := run([]string{"observer", "--phase", "before"},
		mapEnvironment(map[string]string{"COMPOSE_PROJECT_NAME": "debug"}), &strings.Builder{}); err == nil {
		t.Fatal("non-formal project was accepted")
	}
}

func TestGatewayResourcePhaseOrderingExcludesMemoryProbeFromCounterDelta(t *testing.T) {
	var order []string
	readMemory := func() ([]byte, []byte, error) {
		order = append(order, "memory")
		return []byte("4096\n"), []byte("oom 0\n"), nil
	}
	readStats := func() (gatewayStatsSnapshot, error) {
		order = append(order, "stats")
		return gatewayStatsSnapshot{cpuUsec: 77, networkRX: 10, networkTX: 20}, nil
	}
	before, err := bracketGatewayResources("before", readMemory, readStats)
	if err != nil || !reflect.DeepEqual(order, []string{"memory", "stats"}) || before.cpuUsec != 77 {
		t.Fatalf("before bracket = %+v, order=%v, err=%v", before, order, err)
	}
	order = nil
	after, err := bracketGatewayResources("after", readMemory, readStats)
	if err != nil || !reflect.DeepEqual(order, []string{"stats", "memory"}) || after.networkTX != 20 {
		t.Fatalf("after bracket = %+v, order=%v, err=%v", after, order, err)
	}
	order = nil
	if _, err := bracketGatewayResources("during", readMemory, readStats); err == nil || len(order) != 0 {
		t.Fatalf("invalid phase invoked a resource reader: order=%v err=%v", order, err)
	}
}

func TestParseGatewayStatsUsesEngineCPUAndEveryNetwork(t *testing.T) {
	var response containerStatsResponse
	if err := json.Unmarshal([]byte(`{
		"id":"gateway-id",
		"cpu_stats":{"cpu_usage":{"total_usage":987654000}},
		"networks":{"eth0":{"rx_bytes":101,"tx_bytes":202},"eth1":{"rx_bytes":303,"tx_bytes":404}}
	}`), &response); err != nil {
		t.Fatal(err)
	}
	got, err := parseGatewayStats("gateway-id", response)
	if err != nil || got != (gatewayStatsSnapshot{cpuUsec: 987654, networkRX: 404, networkTX: 606}) {
		t.Fatalf("Gateway stats = %+v, %v", got, err)
	}
	if _, err := parseGatewayStats("other-id", response); err == nil {
		t.Fatal("stats from a different container were accepted")
	}
}

func TestParseGatewayMemoryProbeRequiresPrivateCgroupV2(t *testing.T) {
	raw := []byte("TASKGATE_CGROUP\n0::/\nTASKGATE_CONTROLLERS\ncpu io memory\n" +
		"TASKGATE_MEMORY_PEAK\n4096\nTASKGATE_MEMORY_EVENTS\noom 0\noom_kill 0\n")
	peak, events, err := parseGatewayMemoryProbe(raw)
	if err != nil || strings.TrimSpace(string(peak)) != "4096" || !strings.Contains(string(events), "oom 0") {
		t.Fatalf("memory probe = %q %q, %v", peak, events, err)
	}
	bad := strings.Replace(string(raw), "0::/", "0::/host-visible", 1)
	if _, _, err := parseGatewayMemoryProbe([]byte(bad)); err == nil {
		t.Fatal("non-private cgroup namespace was accepted")
	}
}

func TestRuntimeIdentityChangesWhenProjectContainerIsReplaced(t *testing.T) {
	fake := completeFakeEngine()
	gateway := fake.services[testProject+"/"+gatewayService]
	business := fake.services[testProject+"/"+businessService]
	control := fake.services[testProject+"/"+controlService]
	before, err := runtimeIdentitySHA256(testProject, fake.restarts[0], gateway, business, control)
	if err != nil {
		t.Fatal(err)
	}
	afterSnapshot := restartSnapshot(map[string]int64{
		"gateway-new": 2, "business-id": 1, "control-id": 0,
	})
	gateway.id, gateway.pid = "gateway-new", 201
	after, err := runtimeIdentitySHA256(testProject, afterSnapshot, gateway, business, control)
	if err != nil {
		t.Fatal(err)
	}
	if before == after || len(before) != 64 || len(after) != 64 {
		t.Fatalf("runtime identity did not bind replacement: before=%q after=%q", before, after)
	}
}

func TestFormalProjectTopologyRejectsMissingAndExtraServices(t *testing.T) {
	if err := validateFormalProjectSnapshot(restartSnapshot(nil)); err != nil {
		t.Fatalf("exact formal topology rejected: %v", err)
	}

	missing := restartSnapshot(nil)
	missingID := missing.services["oa-demo"]
	delete(missing.services, "oa-demo")
	delete(missing.counts, missingID)
	if err := validateFormalProjectSnapshot(missing); err == nil || !strings.Contains(err.Error(), "topology") {
		t.Fatalf("missing formal service accepted: %v", err)
	}

	extra := restartSnapshot(nil)
	extra.services["unexpected-sidecar"] = "unexpected-id"
	extra.counts["unexpected-id"] = 0
	if err := validateFormalProjectSnapshot(extra); err == nil || !strings.Contains(err.Error(), "topology") {
		t.Fatalf("extra formal service accepted: %v", err)
	}
}

func TestValidDockerAPIVersion(t *testing.T) {
	for _, value := range []string{"1.44", "1.52"} {
		if !validDockerAPIVersion(value) {
			t.Fatalf("valid API version %q was rejected", value)
		}
	}
	for _, value := range []string{"", "v1.44", "1", "1.latest"} {
		if validDockerAPIVersion(value) {
			t.Fatalf("invalid API version %q was accepted", value)
		}
	}
}

func TestDockerEngineResolvesLabelsAndSumsRealRestartCounts(t *testing.T) {
	serviceIDs := formalServiceIDs()
	summaries := make([]containerSummary, 0, len(serviceIDs))
	inspects := make(map[string]map[string]any, len(serviceIDs))
	gatewayPID := 0
	for index, service := range formalProjectServices {
		containerID := serviceIDs[service]
		restartCount := int64(0)
		if service == gatewayService {
			restartCount = 2
			gatewayPID = 101 + index
		}
		if service == businessService {
			restartCount = 5
		}
		summaries = append(summaries, containerSummary{ID: containerID})
		inspects[containerID] = map[string]any{
			"Id": containerID, "RestartCount": restartCount,
			"State": map[string]any{"Running": true, "Pid": 101 + index},
			"Config": map[string]any{"Labels": map[string]string{
				"com.docker.compose.project": testProject,
				"com.docker.compose.service": service,
			}},
		}
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/containers/json" {
			if request.URL.Query().Get("all") == "1" {
				_ = json.NewEncoder(writer).Encode(summaries)
				return
			}
			_ = json.NewEncoder(writer).Encode([]containerSummary{{ID: serviceIDs[gatewayService]}})
			return
		}
		containerID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/containers/"), "/json")
		if inspected, present := inspects[containerID]; present && request.URL.Path == "/containers/"+containerID+"/json" {
			_ = json.NewEncoder(writer).Encode(inspected)
			return
		}
		http.NotFound(writer, request)
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	docker := &dockerEngine{client: server.Client(), baseURL: server.URL}
	identity, err := docker.resolveService(context.Background(), testProject, gatewayService)
	if err != nil || identity.id != "gateway-id" || identity.pid != gatewayPID {
		t.Fatalf("resolve service = %+v, %v", identity, err)
	}
	restarts, err := docker.restartSnapshot(context.Background(), testProject)
	wantCounts := make(map[string]int64, len(serviceIDs))
	for _, containerID := range serviceIDs {
		wantCounts[containerID] = 0
	}
	wantCounts[serviceIDs[gatewayService]] = 2
	wantCounts[serviceIDs[businessService]] = 5
	if err != nil || restarts.total != 7 || !reflect.DeepEqual(restarts.counts, wantCounts) ||
		!reflect.DeepEqual(restarts.services, serviceIDs) {
		t.Fatalf("restart snapshot = %+v, %v", restarts, err)
	}
}

func TestDockerEngineRejectsProjectContainerWithoutServiceIdentity(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/containers/json":
			_, _ = writer.Write([]byte(`[{"Id":"unbound-id"}]`))
		case "/containers/unbound-id/json":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"Id": "unbound-id", "RestartCount": int64(0),
				"Config": map[string]any{"Labels": map[string]string{
					"com.docker.compose.project": testProject,
				}},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	docker := &dockerEngine{client: server.Client(), baseURL: server.URL}
	if _, err := docker.restartSnapshot(context.Background(), testProject); err == nil ||
		!strings.Contains(err.Error(), "invalid Compose/restart identity") {
		t.Fatalf("unbound project container was accepted: %v", err)
	}
}

func completeFakeEngine() *fakeEngine {
	return &fakeEngine{
		services: map[string]serviceIdentity{
			testProject + "/" + gatewayService: {
				project: testProject, service: gatewayService, id: "gateway-id", pid: 101,
			},
			testProject + "/" + businessService: {
				project: testProject, service: businessService, id: "business-id", pid: 102,
			},
			testProject + "/" + controlService: {
				project: testProject, service: controlService, id: "control-id", pid: 103,
			},
		},
		resources: gatewayResourceSnapshot{
			memoryPeak:   []byte("268435456\n"),
			memoryEvents: []byte("low 0\nhigh 0\nmax 7\noom 2\noom_kill 1\n"),
			cpuUsec:      987654,
			networkRX:    404,
			networkTX:    606,
		},
		outputs: map[string][]byte{
			"business-id": []byte("42|73400320\n"),
			"control-id":  []byte("68157440\n"),
		},
		restarts: []projectRestartSnapshot{
			restartSnapshot(map[string]int64{"gateway-id": 2, "business-id": 1, "control-id": 0}),
			restartSnapshot(map[string]int64{"gateway-id": 2, "business-id": 1, "control-id": 0}),
		},
		resolveErrors: make(map[string]error),
	}
}

func formalServiceIDs() map[string]string {
	result := make(map[string]string, len(formalProjectServices))
	for _, service := range formalProjectServices {
		result[service] = service + "-id"
	}
	result[gatewayService] = "gateway-id"
	result[businessService] = "business-id"
	result[controlService] = "control-id"
	return result
}

func restartSnapshot(counts map[string]int64) projectRestartSnapshot {
	services := formalServiceIDs()
	if _, replaced := counts["gateway-new"]; replaced {
		services[gatewayService] = "gateway-new"
	}
	copyCounts := make(map[string]int64, len(services))
	knownIDs := make(map[string]struct{}, len(services))
	for _, id := range services {
		copyCounts[id] = 0
		knownIDs[id] = struct{}{}
	}
	for id, count := range counts {
		if _, known := knownIDs[id]; !known {
			services["unexpected-"+id] = id
		}
		copyCounts[id] = count
	}
	var total int64
	for _, count := range copyCounts {
		total += count
	}
	return projectRestartSnapshot{counts: copyCounts, services: services, total: total}
}

func mapEnvironment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}
