package experiment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// formalWindowProjectEnv names the Compose project of a running formal
// deployment. When it is set this test must PASS or FAIL -- never skip -- because
// it is a freeze gate: a skipped run is not evidence that the measurement window
// is clean.
const formalWindowProjectEnv = "TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT"

func formalWindowProject(t *testing.T) string {
	t.Helper()
	project := strings.TrimSpace(os.Getenv(formalWindowProjectEnv))
	if project == "" {
		t.Skipf("%s is not set; this gate is only meaningful against a running formal deployment",
			formalWindowProjectEnv)
	}
	return project
}

func dockerOutput(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func gatewayReaderCalls(t *testing.T, project string) int64 {
	t.Helper()
	const query = `SELECT COALESCE(sum(calls),0) FROM pg_stat_statements s
WHERE s.dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
  AND s.userid=(SELECT oid FROM pg_roles WHERE rolname='gateway_reader')`
	raw := dockerOutput(t, "exec", project+"-business-postgres-1",
		"psql", "-U", "postgres", "-d", "travel_demo", "-tAc", query)
	calls, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		t.Fatalf("parse gateway_reader calls %q: %v", raw, err)
	}
	return calls
}

// The running Gateway must carry exactly the approved periodic probe, and the
// observer must be able to reconstruct it from Docker's own report.
func TestFormalDeploymentRunsTheApprovedHealthcheckLive(t *testing.T) {
	project := formalWindowProject(t)
	raw := dockerOutput(t, "inspect", project+"-gateway-1", "--format", "{{json .Config.Healthcheck}}")
	var probe struct {
		Test              []string `json:"Test"`
		Interval, Timeout int64
		Retries           int64
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		t.Fatalf("decode healthcheck: %v", err)
	}
	seconds := func(nanoseconds int64) string {
		if nanoseconds <= 0 || nanoseconds%int64(time.Second) != 0 {
			return fmt.Sprintf("%dns", nanoseconds)
		}
		return fmt.Sprintf("%ds", nanoseconds/int64(time.Second))
	}
	resolved := GatewayHealthcheck{Test: probe.Test, Interval: seconds(probe.Interval),
		Timeout: seconds(probe.Timeout), Retries: probe.Retries}
	if err := resolved.Validate(); err != nil {
		t.Fatalf("the running formal deployment does not carry the approved probe: %v", err)
	}
}

// The core Stage M-B gate. Periodic liveness probes must continue for longer
// than several intervals and add no Business statement at all.
func TestPeriodicLivenessProbesAddNoBusinessStatements(t *testing.T) {
	project := formalWindowProject(t)

	healthLogEntries := func() int {
		raw := dockerOutput(t, "inspect", project+"-gateway-1",
			"--format", "{{len .State.Health.Log}}")
		count, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			t.Fatalf("parse health log length %q: %v", raw, err)
		}
		return count
	}

	before := gatewayReaderCalls(t, project)
	probesBefore := healthLogEntries()
	// Longer than six seconds, so at the approved three-second interval at
	// least two further probes must run inside the window.
	time.Sleep(10 * time.Second)
	after := gatewayReaderCalls(t, project)
	probesAfter := healthLogEntries()

	// Docker keeps a bounded health log, so a saturated log is evidence the
	// probe is running even when the entry count stops growing.
	if probesAfter == probesBefore && probesAfter < 5 {
		t.Fatalf("no further healthcheck ran during the window (%d entries); "+
			"a zero Business delta would prove nothing", probesAfter)
	}
	if status := dockerOutput(t, "inspect", project+"-gateway-1",
		"--format", "{{.State.Health.Status}}"); status != "healthy" {
		t.Fatalf("the Gateway is %s after the window; health monitoring must remain active", status)
	}
	if delta := after - before; delta != 0 {
		t.Fatalf("periodic healthchecks added %d gateway_reader statements during the measurement window; "+
			"the formal override is not in effect", delta)
	}
}

// Readiness is still genuinely proven -- it simply happens outside the window.
// An explicit call must perform the complete Attestation, so the harness's
// before/after readiness checks remain meaningful evidence rather than a
// formality.
func TestExplicitReadinessOutsideTheWindowStillAttests(t *testing.T) {
	project := formalWindowProject(t)
	endpoint := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_FORMAL_WINDOW_GATEWAY"))
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8082"
	}

	before := gatewayReaderCalls(t, project)
	request, err := http.NewRequest(http.MethodGet, endpoint+"/health/ready", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("explicit readiness call: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("explicit readiness returned HTTP %d, want 204", response.StatusCode)
	}
	after := gatewayReaderCalls(t, project)

	// The Attestation reads the datasource identity and, per ExpectedSchema
	// entry, the view's shape and definition. The exact composition depends on
	// the entry count and on whether the reporting relation is a plain or
	// materialized view, so this asserts the property that matters: an explicit
	// readiness check really does reach Business PostgreSQL.
	if delta := after - before; delta <= 0 {
		t.Fatalf("an explicit readiness call issued %d Business statements; "+
			"it is no longer performing the Attestation and proves nothing", delta)
	}
}
