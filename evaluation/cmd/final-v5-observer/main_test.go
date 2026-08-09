package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/formalbuild"
	"taskbound.local/agent-data-gateway/evaluation/internal/legacyv14"
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
	healthcheck   *experiment.GatewayHealthcheck
	healthErr     error
	// The Docker inspection surface the observer resolves its Gateway and
	// PostgreSQL identities through. It is part of the fake deployment rather
	// than stubbed out, because "the observer inspects these for itself" is the
	// property v2 exists to establish.
	images     map[string]formalbuild.ImageInspect
	containers map[string]formalbuild.ContainerInspect
	imageFiles map[string]string
	binary     string
}

func (f *fakeEngine) ImageInspect(_ context.Context, reference string) (formalbuild.ImageInspect, error) {
	image, present := f.images[reference]
	if !present {
		return formalbuild.ImageInspect{}, fmt.Errorf("no such image %s", reference)
	}
	return image, nil
}

func (f *fakeEngine) ContainerInspect(_ context.Context, containerID string) (formalbuild.ContainerInspect, error) {
	container, present := f.containers[containerID]
	if !present {
		return formalbuild.ContainerInspect{}, fmt.Errorf("no such container %s", containerID)
	}
	return container, nil
}

func (f *fakeEngine) ResolveService(ctx context.Context, project, service string) (string, error) {
	identity, err := f.resolveService(ctx, project, service)
	if err != nil {
		return "", err
	}
	return identity.id, nil
}

// Exec answers the formal-provenance reads the identity resolver performs and
// otherwise falls through to the recorded container output.
func (f *fakeEngine) Exec(ctx context.Context, containerID string, command []string) ([]byte, error) {
	script := command[len(command)-1]
	switch {
	case strings.Contains(script, "sha256sum /usr/local/bin/app"):
		return []byte(f.binary + "\n"), nil
	case strings.Contains(script, "TASKGATE_FILE"):
		var rendered strings.Builder
		for _, path := range []string{
			"/usr/local/share/taskgate/source-commit",
			"/usr/local/share/taskgate/build-context-sha256",
			"/usr/local/share/taskgate/source-manifest-sha256",
			"/usr/local/share/taskgate/build-target",
			"/usr/local/share/taskgate/gateway-binary-sha256",
		} {
			fmt.Fprintf(&rendered, "TASKGATE_FILE %s\n%s\n", path, f.imageFiles[path])
		}
		return []byte(rendered.String()), nil
	}
	return f.exec(ctx, containerID, command)
}

func (f *fakeEngine) resolveGatewayHealthcheck(context.Context, serviceIdentity) (experiment.GatewayHealthcheck, error) {
	if f.healthErr != nil {
		return experiment.GatewayHealthcheck{}, f.healthErr
	}
	if f.healthcheck != nil {
		return *f.healthcheck, nil
	}
	// A complete fake deployment carries the approved formal probe.
	return experiment.FormalGatewayHealthcheck(), nil
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

// Integration gate 1: the observer emits a strict ObserverSnapshotV2, with the
// census read as one atomic statement and every identity resolved by the
// observer itself.
func TestCollectEmitsStrictObserverSnapshotV2(t *testing.T) {
	fake := completeFakeEngine()
	got, err := collectFake(fake, testProject, "before")
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("the emitted snapshot does not validate: %v", err)
	}

	invocation := fakeInvocation("before")
	for name, pair := range map[string][2]string{
		"version":            {got.Version, experiment.ObserverSnapshotV2Version},
		"phase":              {got.Phase, "before"},
		"observer window":    {got.ObserverWindowID, invocation.observerWindowID},
		"classifier":         {got.ClassifierManifestSHA256, invocation.classifierManifestSHA256},
		"operation binding":  {got.OperationBindingSHA256, invocation.operationBindingSHA256},
		"role":               {got.Role, "gateway_reader"},
		"gateway container":  {got.Runtime.GatewayContainerID, "gateway-id"},
		"gateway commit":     {got.Runtime.Gateway.SubmissionCommit, fakeCommit},
		"gateway image":      {got.Runtime.Gateway.LocalImageID, fakeGatewayImageID},
		"gateway binary":     {got.Runtime.Gateway.BinarySHA256, fakeBinaryDigest},
		"postgres reference": {got.Runtime.PostgreSQL.ImageReference, "postgres@" + fakePostgresDigest},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s is %q, want %q", name, pair[0], pair[1])
		}
	}
	if got.SchemaVersion != 2 {
		t.Errorf("schema_version is %d, want 2", got.SchemaVersion)
	}
	if got.Runtime.Gateway.HealthcheckSHA256 != experiment.FormalGatewayHealthcheck().SHA256() {
		t.Error("the snapshot does not bind the approved formal healthcheck")
	}

	// The role total and the structural rows come from one row set.
	var sum int64
	for _, row := range got.Structural {
		sum += row.Calls
	}
	if got.Total != 3 || sum != got.Total {
		t.Fatalf("total=%d rows sum=%d; the reading was not atomic", got.Total, sum)
	}
	if got.Resource.ControlWALBytes != 68157440 || got.Resource.BusinessWALBytes == 0 {
		t.Errorf("WAL evidence is missing: %+v", got.Resource)
	}
	if got.Resource.GatewayCPUUsec != 987654 || got.Resource.GatewayMemoryPeakBytes != 268435456 ||
		got.Resource.GatewayNetworkRXBytes != 404 || got.Resource.GatewayNetworkTXBytes != 606 ||
		got.Resource.OOMEvents != 2 || got.Resource.ContainerRestarts != 3 ||
		got.Resource.GatewayRestartCount != 2 || got.Resource.PostmasterStartTime == "" {
		t.Errorf("resource evidence is incomplete: %+v", got.Resource)
	}

	// The authoritative census statement is the one that was run, not the old
	// two-field counter query.
	if command := fake.execCommands["business-id"]; !reflect.DeepEqual(command,
		[]string{"sh", "-c", businessCensusShell}) {
		t.Fatalf("the observer did not read the authoritative census: %#v", command)
	}
	if command := fake.execCommands["control-id"]; !reflect.DeepEqual(command,
		[]string{"sh", "-c", controlSnapshotShell}) {
		t.Fatalf("unexpected Control command: %#v", command)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"version", "schema_version", "phase", "observer_window_id", "classifier_manifest_sha256",
		"observer_source_sha256", "role", "total_calls", "structural_rows",
		"measurement_environment", "stats_reset", "dealloc", "runtime_identity", "resource_evidence",
	} {
		if _, present := fields[field]; !present {
			t.Errorf("the v2 document omits %q", field)
		}
	}
	// The v1 document's fields must not survive into v2.
	for _, gone := range []string{"business_sql_queries", "runtime_identity_sha256", "memory_scope"} {
		if _, present := fields[gone]; present {
			t.Errorf("the v2 document still carries the v1 field %q", gone)
		}
	}
}

// A v1.5 invocation must never yield a schema-1 document, and the run path must
// emit the v2 one.
func TestRunEmitsSchemaTwoAndNeverSchemaOne(t *testing.T) {
	var out strings.Builder
	snapshot, err := collectFake(completeFakeEngine(), testProject, "after")
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(snapshot); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Version       string `json:"version"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != 2 || decoded.Version != experiment.ObserverSnapshotV2Version {
		t.Fatalf("the observer emitted schema %d version %q", decoded.SchemaVersion, decoded.Version)
	}
	// The archived reader is strict, so a current document cannot be accepted
	// after silently discarding all of its v2-only identity members.
	if _, err := legacyv14.DecodeObserverSnapshot([]byte(out.String())); err == nil {
		t.Fatal("the v2 document decoded as a valid legacy snapshot")
	}
}

// The Gateway and PostgreSQL identities must come from the observer's own
// inspection. A deployment whose image does not match the verified source, or
// whose PostgreSQL is not digest-pinned, cannot produce a snapshot at all.
func TestCollectRefusesAMismatchedGatewayOrPostgreSQL(t *testing.T) {
	for name, corrupt := range map[string]func(*fakeEngine){
		"gateway image built from another commit": func(fake *fakeEngine) {
			image := fake.images[fakeGatewayImageID]
			labels := copyLabels(image.Config.Labels)
			labels[formalbuild.LabelRevision] = "fedcba9876543210fedcba9876543210fedcba98"
			image.Config.Labels = labels
			fake.images[fakeGatewayImageID] = image
		},
		"gateway image built from another context": func(fake *fakeEngine) {
			image := fake.images[fakeGatewayImageID]
			labels := copyLabels(image.Config.Labels)
			labels[formalbuild.LabelBuildContext] = strings.Repeat("e", 64)
			image.Config.Labels = labels
			fake.images[fakeGatewayImageID] = image
		},
		"gateway image is not a formal build": func(fake *fakeEngine) {
			image := fake.images[fakeGatewayImageID]
			labels := copyLabels(image.Config.Labels)
			delete(labels, formalbuild.LabelFormalBuild)
			image.Config.Labels = labels
			fake.images[fakeGatewayImageID] = image
		},
		"gateway binary was replaced": func(fake *fakeEngine) {
			fake.binary = strings.Repeat("c", 64)
		},
		"gateway container runs another image": func(fake *fakeEngine) {
			container := fake.containers["gateway-id"]
			container.Image = fakePostgresImage
			fake.containers["gateway-id"] = container
		},
		"postgresql is named by a mutable tag": func(fake *fakeEngine) {
			container := fake.containers["business-id"]
			container.Config.Image = "postgres:16-bookworm"
			fake.containers["business-id"] = container
		},
		"postgresql carries no registry digest": func(fake *fakeEngine) {
			image := fake.images[fakePostgresImage]
			image.RepoDigests = nil
			fake.images[fakePostgresImage] = image
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := completeFakeEngine()
			corrupt(fake)
			if _, err := collectFake(fake, testProject, "before"); err == nil {
				t.Fatal("a snapshot was emitted for a deployment the observer must refuse")
			}
		})
	}
}

func copyLabels(labels map[string]string) map[string]string {
	copied := make(map[string]string, len(labels))
	for key, value := range labels {
		copied[key] = value
	}
	return copied
}

// The observer's identity must track its own sources: two snapshots of one
// window are only comparable if the same observer produced both.
func TestObserverSourceIdentityIsStableAndCoversEveryPackageSource(t *testing.T) {
	first, err := observerSourceSHA256()
	if err != nil {
		t.Fatalf("observerSourceSHA256: %v", err)
	}
	second, err := observerSourceSHA256()
	if err != nil {
		t.Fatalf("observerSourceSHA256: %v", err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("the observer source identity is unstable or malformed: %q %q", first, second)
	}

	// A new non-test source file must not escape the inventory. The embed
	// directive names files explicitly to keep test edits out of a production
	// snapshot's identity, and this is what stops that from silently under-
	// covering the observer.
	embedded, err := observerSourceNames()
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, name := range embedded {
		present[name] = true
	}
	entries, err := readPackageSources()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range entries {
		if !present[name] {
			t.Errorf("%s is a source of the observer but is absent from its embedded inventory; "+
				"add it to the go:embed directive in observer_source.go", name)
		}
		delete(present, name)
	}
	for name := range present {
		t.Errorf("the observer embeds %q, which is not one of its non-test sources", name)
	}
}

func TestCollectFailsClosedWhenRestartChangesDuringSnapshot(t *testing.T) {
	fake := completeFakeEngine()
	fake.restarts = []projectRestartSnapshot{
		restartSnapshot(map[string]int64{"gateway-id": 2, "business-id": 1, "control-id": 0}),
		restartSnapshot(map[string]int64{"gateway-id": 3, "business-id": 1, "control-id": 0}),
	}
	_, err := collectFake(fake, testProject, "before")
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
	_, err := collectFake(fake, testProject, "before")
	if err == nil || !strings.Contains(err.Error(), "restarted or was replaced") {
		t.Fatalf("expected same-total container replacement to fail closed, got %v", err)
	}
}

func TestCollectFailsClosedOnMissingCPUCounter(t *testing.T) {
	fake := completeFakeEngine()
	fake.resources.cpuUsec = 0
	_, err := collectFake(fake, testProject, "before")
	if err == nil || !strings.Contains(err.Error(), "omitted a real Gateway CPU/network counter") {
		t.Fatalf("expected missing real CPU counter to fail closed, got %v", err)
	}
}

// The v2 census carries the role total and the structural rows in one
// materialized row set. A reading whose total disagrees with its rows, or which
// omits state the window depends on, describes no single instant and must never
// become a snapshot.
func TestCollectFailsClosedOnANonAtomicOrIncompleteCensus(t *testing.T) {
	for name, response := range map[string]string{
		"total disagrees with the rows": canonicalCensus(
			"T|5|||", censusLine("SELECT 1", true, 2, "111")),
		"no state row": "E|160014|all|on|off\nT|2|||\n" +
			censusLine("SELECT 1", true, 2, "111") + "\n",
		"no total row": "E|160014|all|on|off\n" +
			"S|2026-08-04 10:41:46.431868+00|0|2026-08-04 10:40:00+00|73400320\n" +
			censusLine("SELECT 1", true, 2, "111") + "\n",
		"malformed row": "E|160014|all|on|off\nnonsense\n",
	} {
		t.Run(name, func(t *testing.T) {
			fake := completeFakeEngine()
			fake.outputs["business-id"] = []byte(response)
			if _, err := collectFake(fake, testProject, "before"); err == nil {
				t.Fatal("a snapshot was built from a census that cannot be authoritative")
			}
		})
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
			if _, err := collectFake(fake, testProject, "before"); err == nil {
				t.Fatal("missing real counter was accepted")
			}
		})
	}
}

func TestCollectRejectsNonFormalProjectAndDuplicateServices(t *testing.T) {
	if _, err := collectFake(completeFakeEngine(), "other-project", "before"); err == nil {
		t.Fatal("non-formal Compose project was accepted")
	}
	fake := completeFakeEngine()
	fake.services[testProject+"/"+businessService] = fake.services[testProject+"/"+gatewayService]
	if _, err := collectFake(fake, testProject, "before"); err == nil || !strings.Contains(err.Error(), "duplicate") {
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
	before, err := runtimeIdentitySHA256(testProject, fake.restarts[0], experiment.FormalGatewayHealthcheck(), gateway, business, control)
	if err != nil {
		t.Fatal(err)
	}
	afterSnapshot := restartSnapshot(map[string]int64{
		"gateway-new": 2, "business-id": 1, "control-id": 0,
	})
	gateway.id, gateway.pid = "gateway-new", 201
	after, err := runtimeIdentitySHA256(testProject, afterSnapshot, experiment.FormalGatewayHealthcheck(), gateway, business, control)
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

// The formal source the fake deployment's Gateway image was built from.
const (
	fakeCommit         = "0123456789abcdef0123456789abcdef01234567"
	fakeContextDigest  = "5555555555555555555555555555555555555555555555555555555555555555"
	fakeManifestDigest = "4444444444444444444444444444444444444444444444444444444444444444"
	fakeGatewayImageID = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	fakePostgresImage  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	fakeBinaryDigest   = "3333333333333333333333333333333333333333333333333333333333333333"
	fakePostgresDigest = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
)

func fakeExpectedSource() formalbuild.ExpectedSource {
	return formalbuild.ExpectedSource{
		Commit: fakeCommit, ContextSHA256: fakeContextDigest,
		SourceManifestSHA256: fakeManifestDigest, CleanTree: true,
	}
}

func fakeInvocation(phase string) observerInvocation {
	return observerInvocation{
		phase:                    phase,
		observerWindowID:         strings.Repeat("a1", 32),
		classifierManifestSHA256: strings.Repeat("b2", 32),
		operationBindingSHA256:   strings.Repeat("c3", 32),
	}
}

// collectFake runs the v2 collection against a fake deployment.
func collectFake(fake *fakeEngine, project, phase string) (experiment.ObserverSnapshotV2, error) {
	source, err := observerSourceSHA256()
	if err != nil {
		return experiment.ObserverSnapshotV2{}, err
	}
	return collectV2(context.Background(), fake, project, fakeInvocation(phase), source, fakeExpectedSource())
}

// fakeBusinessCensus is one atomic Business reading: an environment row, a state
// row, the role total and the structural rows that sum to it.
func fakeBusinessCensus() []byte {
	return []byte(canonicalCensus(
		"T|3|||",
		censusLine("SELECT 1", true, 2, "111"),
		censusLine("SELECT 2", false, 1, "222"),
	))
}

func completeFakeEngine() *fakeEngine {
	gatewayImage := formalbuild.ImageInspect{ID: fakeGatewayImageID, Architecture: "amd64", OS: "linux"}
	gatewayImage.Config.Labels = map[string]string{
		formalbuild.LabelFormalBuild:      "v1",
		formalbuild.LabelRevision:         fakeCommit,
		formalbuild.LabelBuildContext:     fakeContextDigest,
		formalbuild.LabelSourceManifest:   fakeManifestDigest,
		formalbuild.LabelBuildTarget:      formalbuild.GatewayBuildTarget,
		formalbuild.LabelBuilderBaseImage: "golang@sha256:" + strings.Repeat("7", 64),
		formalbuild.LabelRuntimeBaseImage: "debian@sha256:" + strings.Repeat("8", 64),
	}
	postgresImage := formalbuild.ImageInspect{
		ID: fakePostgresImage, Architecture: "amd64", OS: "linux",
		RepoDigests: []string{"postgres@" + fakePostgresDigest},
	}
	gatewayContainer := formalbuild.ContainerInspect{ID: "gateway-id", Image: fakeGatewayImageID}
	gatewayContainer.State.Running = true
	businessContainer := formalbuild.ContainerInspect{ID: "business-id", Image: fakePostgresImage}
	businessContainer.State.Running = true
	businessContainer.Config.Image = "postgres@" + fakePostgresDigest

	return &fakeEngine{
		images: map[string]formalbuild.ImageInspect{
			fakeGatewayImageID: gatewayImage,
			fakePostgresImage:  postgresImage,
		},
		containers: map[string]formalbuild.ContainerInspect{
			"gateway-id":  gatewayContainer,
			"business-id": businessContainer,
		},
		imageFiles: map[string]string{
			"/usr/local/share/taskgate/source-commit":          fakeCommit,
			"/usr/local/share/taskgate/build-context-sha256":   fakeContextDigest,
			"/usr/local/share/taskgate/source-manifest-sha256": fakeManifestDigest,
			"/usr/local/share/taskgate/build-target":           formalbuild.GatewayBuildTarget,
			"/usr/local/share/taskgate/gateway-binary-sha256":  fakeBinaryDigest,
		},
		binary: fakeBinaryDigest,
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
			"business-id": fakeBusinessCensus(),
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

// The formal window gate. A deployment whose Gateway still probes /health/ready
// on a timer performs a full Business PostgreSQL Attestation on every interval,
// so the observer must refuse to produce a snapshot at all rather than emit one
// carrying wall-clock-dependent statements.
func TestCollectRejectsAReadinessProbingGateway(t *testing.T) {
	readiness := experiment.GatewayHealthcheck{
		Test:     []string{"CMD", "curl", "--fail", "--silent", "http://127.0.0.1:8082/health/ready"},
		Interval: "3s", Timeout: "3s", Retries: 30,
	}
	fake := completeFakeEngine()
	fake.healthcheck = &readiness
	if _, err := collectFake(fake, testProject, "before"); err == nil {
		t.Fatal("a snapshot was produced under the readiness probe")
	}
}

func TestCollectRejectsHealthcheckScheduleDrift(t *testing.T) {
	for name, probe := range map[string]experiment.GatewayHealthcheck{
		"a different interval": {Test: experiment.FormalGatewayHealthcheck().Test,
			Interval: "30s", Timeout: "3s", Retries: 30},
		"a different timeout": {Test: experiment.FormalGatewayHealthcheck().Test,
			Interval: "3s", Timeout: "10s", Retries: 30},
		"a different retry count": {Test: experiment.FormalGatewayHealthcheck().Test,
			Interval: "3s", Timeout: "3s", Retries: 3},
		"no probe at all": {},
	} {
		t.Run(name, func(t *testing.T) {
			fake := completeFakeEngine()
			probe := probe
			fake.healthcheck = &probe
			if _, err := collectFake(fake, testProject, "before"); err == nil {
				t.Fatal("a snapshot was produced under a drifted healthcheck")
			}
		})
	}
}

// The healthcheck is part of runtime identity, so a snapshot taken under the
// readiness probe can never compare equal to one taken under the approved
// liveness probe even if every container and PID matched.
func TestRuntimeIdentitySeparatesHealthcheckModes(t *testing.T) {
	fake := completeFakeEngine()
	gateway := fake.services[testProject+"/"+gatewayService]
	business := fake.services[testProject+"/"+businessService]
	control := fake.services[testProject+"/"+controlService]
	approved, err := runtimeIdentitySHA256(testProject, fake.restarts[0],
		experiment.FormalGatewayHealthcheck(), gateway, business, control)
	if err != nil {
		t.Fatal(err)
	}
	readiness, err := runtimeIdentitySHA256(testProject, fake.restarts[0],
		experiment.GatewayHealthcheck{
			Test:     []string{"CMD", "curl", "--fail", "--silent", "http://127.0.0.1:8082/health/ready"},
			Interval: "3s", Timeout: "3s", Retries: 30,
		}, gateway, business, control)
	if err != nil {
		t.Fatal(err)
	}
	if approved == readiness {
		t.Fatal("runtime identity does not distinguish the readiness probe from the liveness probe")
	}
}

// readPackageSources lists the observer's non-test Go sources on disk.
func readPackageSources() ([]string, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Test 11: a change to the observer's source inventory must change the identity
// it stamps on its snapshots. Without this the before/after pair could not
// detect that two different observers produced the two halves of one window.
func TestObserverSourceInventoryChangesAlterTheIdentity(t *testing.T) {
	base := []inventoryEntry{
		{name: "main.go", content: []byte("package main\n")},
		{name: "snapshot_v2.go", content: []byte("package main // census\n")},
	}
	baseDigest := inventoryDigest(base)

	for name, mutated := range map[string][]inventoryEntry{
		"a file's content changed": {
			{name: "main.go", content: []byte("package main // edited\n")},
			{name: "snapshot_v2.go", content: []byte("package main // census\n")},
		},
		"a file was renamed": {
			{name: "observer.go", content: []byte("package main\n")},
			{name: "snapshot_v2.go", content: []byte("package main // census\n")},
		},
		"a file was added": append(append([]inventoryEntry(nil), base...),
			inventoryEntry{name: "extra.go", content: []byte("package main\n")}),
		"a file was removed": base[:1],
		"two files' contents were swapped": {
			{name: "main.go", content: []byte("package main // census\n")},
			{name: "snapshot_v2.go", content: []byte("package main\n")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if inventoryDigest(mutated) == baseDigest {
				t.Fatal("the observer source identity did not change with its inventory")
			}
		})
	}

	// The same inventory must always digest the same, or two snapshots of one
	// undisturbed window could never compare equal.
	if inventoryDigest(base) != baseDigest {
		t.Fatal("the observer source identity is not deterministic")
	}
}

// Test 12: in an undisturbed deployment the before and after snapshots must
// carry byte-identical runtime identities. Anything else would make the window
// comparison reject a measurement that was in fact clean.
func TestRuntimeIdentitiesAreByteIdenticalAcrossAnUndisturbedWindow(t *testing.T) {
	before, err := collectFake(completeFakeEngine(), testProject, "before")
	if err != nil {
		t.Fatal(err)
	}
	after, err := collectFake(completeFakeEngine(), testProject, "after")
	if err != nil {
		t.Fatal(err)
	}
	if before.Runtime != after.Runtime {
		t.Fatalf("the runtime identity differs across an undisturbed window:\n before %+v\n after  %+v",
			before.Runtime, after.Runtime)
	}
	beforeJSON, err := json.Marshal(before.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, err := json.Marshal(after.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeJSON, afterJSON) {
		t.Fatalf("the serialized runtime identity differs across an undisturbed window:\n%s\n%s",
			beforeJSON, afterJSON)
	}
	if before.ObserverSourceSHA256 != after.ObserverSourceSHA256 {
		t.Fatal("the two halves of one window report different observer source identities")
	}

	// The window itself must accept the pair. A disturbed deployment is caught
	// by the invariant comparison; an undisturbed one must survive it.
	if before.Runtime.ProjectTopologySHA256 != after.Runtime.ProjectTopologySHA256 {
		t.Fatal("the project topology identity moved across an undisturbed window")
	}
}
