// v4-compose-observer is the out-of-process telemetry probe used by the V4
// acceptance driver. It talks to the Docker Engine over its Unix socket, so
// none of its PostgreSQL statements or filesystem reads run through Gateway.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	memoryScope     = "cgroup_v2_memory_peak_including_mmap"
	maxResponseSize = 1 << 20
)

type output struct {
	SchemaVersion int              `json:"schema_version"`
	MemoryScope   string           `json:"memory_scope"`
	Metrics       map[string]int64 `json:"metrics"`
}

type containerSummary struct {
	ID string `json:"Id"`
}

type containerInspect struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

type execCreateResponse struct {
	ID string `json:"Id"`
}

type execInspectResponse struct {
	Running  bool `json:"Running"`
	ExitCode int  `json:"ExitCode"`
}

type versionResponse struct {
	APIVersion string `json:"ApiVersion"`
}

type engine interface {
	composeProject(context.Context) (string, error)
	resolveService(context.Context, string, string) (string, error)
	exec(context.Context, string, []string) ([]byte, error)
	businessQueryCount(context.Context, func(string) string) (int64, error)
}

type dockerEngine struct {
	client  *http.Client
	baseURL string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "v4 compose observer:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := validateInvocation(os.Getenv); err != nil {
		return err
	}
	timeout, err := timeoutFromEnvironment(os.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	engine, err := newDockerEngine(envOr("V4_OBSERVER_DOCKER_SOCKET", "/var/run/docker.sock"))
	if err != nil {
		return err
	}
	if err := engine.negotiate(ctx); err != nil {
		return err
	}
	value, err := collect(ctx, engine, os.Getenv)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func validateInvocation(getenv func(string) string) error {
	for _, name := range []string{"V4_EVAL_CASE", "V4_EVAL_PHASE", "V4_EVAL_TRIAL"} {
		if strings.TrimSpace(getenv(name)) == "" {
			return fmt.Errorf("%s is required by the observer contract", name)
		}
	}
	switch getenv("V4_EVAL_POINT") {
	case "before", "after":
		return nil
	default:
		return errors.New("V4_EVAL_POINT must be before or after")
	}
}

func timeoutFromEnvironment(getenv func(string) string) (time.Duration, error) {
	raw := strings.TrimSpace(getenv("V4_OBSERVER_TIMEOUT_MS"))
	if raw == "" {
		return 4 * time.Second, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > 60000 {
		return 0, errors.New("V4_OBSERVER_TIMEOUT_MS must be in [1,60000]")
	}
	return time.Duration(value) * time.Millisecond, nil
}

func collect(ctx context.Context, docker engine, getenv func(string) string) (output, error) {
	project := strings.TrimSpace(getenv("V4_OBSERVER_COMPOSE_PROJECT"))
	var err error
	if project == "" {
		project, err = docker.composeProject(ctx)
		if err != nil {
			return output{}, fmt.Errorf("resolve Compose project: %w", err)
		}
	}
	gatewayService := envOrFrom(getenv, "V4_OBSERVER_GATEWAY_SERVICE", "gateway")
	gatewayID, err := docker.resolveService(ctx, project, gatewayService)
	if err != nil {
		return output{}, fmt.Errorf("resolve Gateway container: %w", err)
	}
	cgroup, controllers, peakRaw, networkRaw, err := gatewaySnapshot(ctx, docker, gatewayID)
	if err != nil {
		return output{}, err
	}
	if !privateUnifiedCgroup(cgroup) {
		return output{}, errors.New("Gateway is not in a private unified cgroup v2 namespace")
	}
	if !containsField(controllers, "memory") {
		return output{}, errors.New("Gateway cgroup v2 memory controller is unavailable")
	}
	peak, err := parseNonnegativeInt(peakRaw, "Gateway memory.peak")
	if err != nil {
		return output{}, err
	}
	rx, tx, err := parseNetworkCounters(networkRaw)
	if err != nil {
		return output{}, err
	}
	queries, err := docker.businessQueryCount(ctx, getenv)
	if err != nil {
		return output{}, err
	}
	return output{SchemaVersion: 1, MemoryScope: memoryScope, Metrics: map[string]int64{
		"gateway_memory_peak_bytes":  peak,
		"gateway_network_rx_bytes":   rx,
		"gateway_network_tx_bytes":   tx,
		"business_sql_queries_total": queries,
	}}, nil
}

func gatewaySnapshot(ctx context.Context, docker engine, containerID string) ([]byte, []byte, []byte, []byte, error) {
	const script = `set -eu
printf 'TASKGATE_CGROUP\n'
cat /proc/self/cgroup
printf 'TASKGATE_CONTROLLERS\n'
cat /sys/fs/cgroup/cgroup.controllers
printf 'TASKGATE_PEAK\n'
cat /sys/fs/cgroup/memory.peak
printf 'TASKGATE_NETWORK\n'
cat /proc/net/dev`
	raw, err := docker.exec(ctx, containerID, []string{"sh", "-c", script})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read Gateway cgroup and network counters: %w", err)
	}
	sections, err := parseSections(raw, []string{
		"TASKGATE_CGROUP", "TASKGATE_CONTROLLERS", "TASKGATE_PEAK", "TASKGATE_NETWORK",
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return sections[0], sections[1], sections[2], sections[3], nil
}

func parseSections(raw []byte, markers []string) ([][]byte, error) {
	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	result := make([][]byte, 0, len(markers))
	for index, marker := range markers {
		prefix := marker + "\n"
		start := strings.Index(normalized, prefix)
		if start < 0 || strings.Count(normalized, prefix) != 1 {
			return nil, fmt.Errorf("Gateway snapshot marker %q is absent or duplicated", marker)
		}
		start += len(prefix)
		end := len(normalized)
		if index+1 < len(markers) {
			next := strings.Index(normalized[start:], markers[index+1]+"\n")
			if next < 0 {
				return nil, fmt.Errorf("Gateway snapshot marker %q is out of order", markers[index+1])
			}
			end = start + next
		}
		result = append(result, []byte(normalized[start:end]))
	}
	return result, nil
}

func (d *dockerEngine) businessQueryCount(ctx context.Context, getenv func(string) string) (int64, error) {
	dsn := strings.TrimSpace(getenv("V4_OBSERVER_BUSINESS_DSN"))
	if strings.TrimSpace(dsn) == "" {
		return 0, errors.New("V4_OBSERVER_BUSINESS_DSN is required for an out-of-band counter")
	}
	targetRole := envOrFrom(getenv, "V4_OBSERVER_GATEWAY_DB_ROLE", "gateway_reader")
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("connect to Business PostgreSQL observer: %w", err)
	}
	defer connection.Close(context.Background())
	if connection.Config().User == targetRole {
		return 0, errors.New("observer and measured Business SQL role must be different")
	}
	const query = `SELECT COALESCE(sum(s.calls), 0)::bigint
FROM pg_stat_statements AS s
WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND s.userid = (SELECT oid FROM pg_roles WHERE rolname = $1)
  AND (
    position('reporting.scale_' in replace(lower(s.query), '"', '')) > 0 OR
    position('taskgate_ordinal.' in replace(lower(s.query), '"', '')) > 0
  )`
	var value int64
	if err := connection.QueryRow(ctx, query, targetRole).Scan(&value); err != nil {
		return 0, fmt.Errorf("read pg_stat_statements Business query counter: %w", err)
	}
	return value, nil
}

func privateUnifiedCgroup(raw []byte) bool {
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "0::/" {
			return true
		}
	}
	return false
}

func containsField(raw []byte, wanted string) bool {
	for _, field := range strings.Fields(string(raw)) {
		if field == wanted {
			return true
		}
	}
	return false
}

func parseNonnegativeInt(raw []byte, label string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s is not a nonnegative int64", label)
	}
	return value, nil
}

func parseNetworkCounters(raw []byte) (int64, int64, error) {
	var rxTotal, txTotal int64
	interfaces := 0
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if name == "" || name == "lo" {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) != 16 {
			return 0, 0, fmt.Errorf("Gateway network interface %q has %d fields, want 16", name, len(fields))
		}
		rx, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || rx < 0 {
			return 0, 0, fmt.Errorf("Gateway network interface %q has invalid RX bytes", name)
		}
		tx, err := strconv.ParseInt(fields[8], 10, 64)
		if err != nil || tx < 0 {
			return 0, 0, fmt.Errorf("Gateway network interface %q has invalid TX bytes", name)
		}
		if rx > int64(^uint64(0)>>1)-rxTotal || tx > int64(^uint64(0)>>1)-txTotal {
			return 0, 0, errors.New("Gateway network counter overflow")
		}
		rxTotal += rx
		txTotal += tx
		interfaces++
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	if interfaces == 0 {
		return 0, 0, errors.New("Gateway network namespace has no non-loopback interface")
	}
	return rxTotal, txTotal, nil
}

func newDockerEngine(socket string) (*dockerEngine, error) {
	info, err := os.Stat(socket)
	if err != nil {
		return nil, fmt.Errorf("Docker socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("Docker endpoint is not a Unix socket")
	}
	dialer := &net.Dialer{Timeout: time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socket)
	}}
	return &dockerEngine{client: &http.Client{Transport: transport}, baseURL: "http://docker"}, nil
}

func (d *dockerEngine) negotiate(ctx context.Context) error {
	var version versionResponse
	if err := d.getJSON(ctx, "/version", &version); err != nil {
		return fmt.Errorf("negotiate Docker Engine API: %w", err)
	}
	if !validDockerAPIVersion(version.APIVersion) {
		return errors.New("Docker Engine returned an invalid API version")
	}
	d.baseURL = "http://docker/v" + version.APIVersion
	return nil
}

func validDockerAPIVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, one := range part {
			if one < '0' || one > '9' {
				return false
			}
		}
	}
	return true
}

func (d *dockerEngine) composeProject(ctx context.Context) (string, error) {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "", errors.New("V4_OBSERVER_COMPOSE_PROJECT is required outside a Compose container")
	}
	var inspect containerInspect
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(hostname)+"/json", &inspect); err != nil {
		return "", errors.New("set V4_OBSERVER_COMPOSE_PROJECT when the observer is not running in Compose")
	}
	project := strings.TrimSpace(inspect.Config.Labels["com.docker.compose.project"])
	if project == "" {
		return "", errors.New("observer container has no com.docker.compose.project label")
	}
	return project, nil
}

func (d *dockerEngine) resolveService(ctx context.Context, project, service string) (string, error) {
	filters, err := json.Marshal(map[string][]string{"label": {
		"com.docker.compose.project=" + project,
		"com.docker.compose.service=" + service,
	}})
	if err != nil {
		return "", err
	}
	var containers []containerSummary
	if err := d.getJSON(ctx, "/containers/json?filters="+url.QueryEscape(string(filters)), &containers); err != nil {
		return "", err
	}
	if len(containers) != 1 || containers[0].ID == "" {
		return "", fmt.Errorf("found %d running containers for project=%q service=%q, want exactly one", len(containers), project, service)
	}
	return containers[0].ID, nil
}

func (d *dockerEngine) exec(ctx context.Context, containerID string, command []string) ([]byte, error) {
	request := struct {
		AttachStdout bool     `json:"AttachStdout"`
		AttachStderr bool     `json:"AttachStderr"`
		Tty          bool     `json:"Tty"`
		Cmd          []string `json:"Cmd"`
	}{AttachStdout: true, AttachStderr: true, Tty: true, Cmd: command}
	var created execCreateResponse
	if err := d.postJSON(ctx, "/containers/"+url.PathEscape(containerID)+"/exec", request, &created); err != nil {
		return nil, err
	}
	if created.ID == "" {
		return nil, errors.New("Docker Engine returned an empty exec id")
	}
	body, status, err := d.request(ctx, http.MethodPost, "/exec/"+url.PathEscape(created.ID)+"/start", map[string]bool{"Detach": false, "Tty": true})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("Docker exec start returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	var inspected execInspectResponse
	if err := d.getJSON(ctx, "/exec/"+url.PathEscape(created.ID)+"/json", &inspected); err != nil {
		return nil, err
	}
	if inspected.Running {
		return nil, errors.New("Docker exec was still running after attached start completed")
	}
	if inspected.ExitCode != 0 {
		return nil, fmt.Errorf("container command exited %d: %s", inspected.ExitCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (d *dockerEngine) getJSON(ctx context.Context, path string, target any) error {
	body, status, err := d.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Docker Engine returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(bytes.NewReader(body)).Decode(target)
}

func (d *dockerEngine) postJSON(ctx context.Context, path string, request, target any) error {
	body, status, err := d.request(ctx, http.MethodPost, path, request)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Docker Engine returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, target)
}

func (d *dockerEngine) request(ctx context.Context, method, path string, value any) ([]byte, int, error) {
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, d.baseURL+path, body)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize+1))
	if err != nil {
		return nil, 0, err
	}
	if len(raw) > maxResponseSize {
		return nil, 0, errors.New("Docker Engine response exceeded 1 MiB")
	}
	return raw, response.StatusCode, nil
}

func envOr(name, fallback string) string {
	return envOrFrom(os.Getenv, name, fallback)
}

func envOrFrom(getenv func(string) string, name, fallback string) string {
	if value := strings.TrimSpace(getenv(name)); value != "" {
		return value
	}
	return fallback
}
