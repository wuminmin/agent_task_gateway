package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type fakeEngine struct {
	project  string
	services map[string]string
	outputs  map[string][]byte
	commands [][]string
	queries  int64
}

func (f *fakeEngine) composeProject(context.Context) (string, error) { return f.project, nil }

func (f *fakeEngine) resolveService(_ context.Context, project, service string) (string, error) {
	return f.services[project+"/"+service], nil
}

func (f *fakeEngine) exec(_ context.Context, container string, command []string) ([]byte, error) {
	f.commands = append(f.commands, append([]string{container}, command...))
	key := container + "\x00" + strings.Join(command, "\x00")
	if strings.Contains(strings.Join(command, " "), "pg_stat_statements") {
		key = container + "\x00psql"
	}
	return f.outputs[key], nil
}

func (f *fakeEngine) businessQueryCount(context.Context, func(string) string) (int64, error) {
	return f.queries, nil
}

func TestCollectEmitsAcceptanceContractMetrics(t *testing.T) {
	network := `Inter-|   Receive                                                |  Transmit
 face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed
    lo: 100 1 0 0 0 0 0 0 100 1 0 0 0 0 0 0
  eth0: 101 1 0 0 0 0 0 0 202 2 0 0 0 0 0 0
  eth1: 303 3 0 0 0 0 0 0 404 4 0 0 0 0 0 0
`
	fake := &fakeEngine{project: "narrow", queries: 42, services: map[string]string{
		"narrow/gateway": "gateway-id",
	}, outputs: map[string][]byte{
		"gateway-id\x00sh\x00-c\x00" + gatewaySnapshotScript(): []byte("TASKGATE_CGROUP\n0::/\nTASKGATE_CONTROLLERS\ncpu io memory pids\nTASKGATE_MEMORY_CURRENT\n134217728\nTASKGATE_MEMORY_PEAK\n268435456\nTASKGATE_MEMORY_MAX\n1073741824\nTASKGATE_SWAP_CURRENT\n4096\nTASKGATE_SWAP_PEAK\n8192\nTASKGATE_SWAP_MAX\nmax\nTASKGATE_MEMORY_EVENTS\nlow 0\nhigh 0\nmax 7\noom 2\noom_kill 1\noom_group_kill 0\nTASKGATE_NETWORK\n" + network),
	}}
	got, err := collect(context.Background(), fake, mapEnvironment(map[string]string{"POSTGRES_DB": "scale"}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"gateway_memory_peak_bytes": 268435456, "gateway_memory_current_bytes": 134217728,
		"gateway_memory_max_bytes": 1073741824, "gateway_memory_swap_current_bytes": 4096,
		"gateway_memory_swap_peak_bytes": 8192, "gateway_memory_swap_max_bytes": unlimitedCgroupValue,
		"gateway_memory_events_max_total": 7, "gateway_memory_events_oom_total": 2,
		"gateway_memory_events_oom_kill_total": 1, "gateway_network_rx_bytes": 404,
		"gateway_network_tx_bytes": 606, "business_sql_queries_total": 42,
	}
	if got.SchemaVersion != 1 || got.MemoryScope != memoryScope || !reflect.DeepEqual(got.Metrics, want) {
		t.Fatalf("unexpected output: %#v", got)
	}
}

func TestParseNetworkCountersRejectsMissingContainerInterface(t *testing.T) {
	_, _, err := parseNetworkCounters([]byte("lo: 1 0 0 0 0 0 0 0 1 0 0 0 0 0 0 0\n"))
	if err == nil {
		t.Fatal("expected loopback-only network namespace to fail closed")
	}
}

func TestCollectRejectsHostCgroupNamespace(t *testing.T) {
	fake := &fakeEngine{project: "narrow", services: map[string]string{
		"narrow/gateway": "gateway-id",
	}, outputs: map[string][]byte{
		"gateway-id\x00sh\x00-c\x00" + gatewaySnapshotScript(): []byte("TASKGATE_CGROUP\n0::/system.slice/docker.scope\nTASKGATE_CONTROLLERS\nmemory\nTASKGATE_MEMORY_CURRENT\n1\nTASKGATE_MEMORY_PEAK\n1\nTASKGATE_MEMORY_MAX\n1\nTASKGATE_SWAP_CURRENT\n0\nTASKGATE_SWAP_PEAK\n0\nTASKGATE_SWAP_MAX\n0\nTASKGATE_MEMORY_EVENTS\nmax 0\noom 0\noom_kill 0\nTASKGATE_NETWORK\neth0: 1 0 0 0 0 0 0 0 1 0 0 0 0 0 0 0\n"),
	}}
	_, err := collect(context.Background(), fake, mapEnvironment(nil))
	if err == nil || !strings.Contains(err.Error(), "private unified cgroup") {
		t.Fatalf("expected cgroup scope failure, got %v", err)
	}
}

func TestValidateInvocation(t *testing.T) {
	valid := mapEnvironment(map[string]string{
		"V4_EVAL_CASE": "max", "V4_EVAL_PHASE": "novel", "V4_EVAL_TRIAL": "1", "V4_EVAL_POINT": "before",
	})
	if err := validateInvocation(valid); err != nil {
		t.Fatal(err)
	}
	invalid := mapEnvironment(map[string]string{
		"V4_EVAL_CASE": "max", "V4_EVAL_PHASE": "novel", "V4_EVAL_TRIAL": "1", "V4_EVAL_POINT": "during",
	})
	if err := validateInvocation(invalid); err == nil {
		t.Fatal("expected invalid point to fail")
	}
}

func TestValidDockerAPIVersion(t *testing.T) {
	for _, value := range []string{"1.44", "1.52"} {
		if !validDockerAPIVersion(value) {
			t.Fatalf("expected %q to be accepted", value)
		}
	}
	for _, value := range []string{"", "v1.44", "1", "1.latest"} {
		if validDockerAPIVersion(value) {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func gatewaySnapshotScript() string {
	return gatewaySnapshotShellScript
}

func mapEnvironment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}
