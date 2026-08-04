package formalbuild

import (
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
	"path/filepath"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// maxEngineResponse bounds an Engine reply. An inspect payload is kilobytes; a
// reply orders of magnitude larger means the endpoint is not what it claims.
const maxEngineResponse = 1 << 20

// ImageInspect is the part of a Docker image inspection the formal build binds.
type ImageInspect struct {
	ID           string   `json:"Id"`
	RepoDigests  []string `json:"RepoDigests"`
	Architecture string   `json:"Architecture"`
	OS           string   `json:"Os"`
	Config       struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// Platform is the resolved os/architecture.
//
// It is bound because the same image ID can be reached on more than one
// architecture through a multi-architecture index, so an ID alone does not say
// what actually ran.
func (image ImageInspect) Platform() string {
	return image.OS + "/" + image.Architecture
}

// Label reads one label, reporting absence separately from emptiness. A missing
// label and a label set to "" are different failures: the first says the image
// was not built by the formal path, the second says it was built with a missing
// build argument.
func (image ImageInspect) Label(name string) (string, bool) {
	value, present := image.Config.Labels[name]
	return value, present
}

// ContainerInspect is the part of a container inspection the formal build binds.
type ContainerInspect struct {
	ID    string `json:"Id"`
	Image string `json:"Image"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		// Healthcheck is the periodic probe Docker actually runs. It is part of
		// the runtime identity because the observer-v3 override replaces
		// /health/ready with /health/live: a probe that still reaches
		// /health/ready performs a full Business PostgreSQL Attestation on every
		// interval, so the measured window would absorb a wall-clock-dependent
		// number of statements no derived plan can predict.
		Healthcheck *struct {
			Test     []string `json:"Test"`
			Interval int64    `json:"Interval"`
			Timeout  int64    `json:"Timeout"`
			Retries  int64    `json:"Retries"`
		} `json:"Healthcheck"`
	} `json:"Config"`
}

// Healthcheck renders the running probe in the second-granularity form the
// Compose override declares, so the comparison is like with like.
func (container ContainerInspect) Healthcheck() (experiment.GatewayHealthcheck, error) {
	if container.Config.Healthcheck == nil {
		return experiment.GatewayHealthcheck{}, errors.New("the running Gateway declares no periodic healthcheck")
	}
	probe := container.Config.Healthcheck
	return experiment.GatewayHealthcheck{
		Test:     append([]string(nil), probe.Test...),
		Interval: durationSeconds(probe.Interval),
		Timeout:  durationSeconds(probe.Timeout),
		Retries:  probe.Retries,
	}, nil
}

// durationSeconds renders a nanosecond duration as whole seconds.
//
// Anything that is not a whole number of seconds cannot equal the approved
// definition and is reported verbatim, so the mismatch stays legible rather than
// being rounded into agreement.
func durationSeconds(nanoseconds int64) string {
	if nanoseconds <= 0 || nanoseconds%int64(time.Second) != 0 {
		return fmt.Sprintf("%dns", nanoseconds)
	}
	return fmt.Sprintf("%ds", nanoseconds/int64(time.Second))
}

// Engine is the Docker Engine capability the formal build needs.
//
// It is an interface so the verification logic can be exercised against
// recorded Engine replies. The checks are the load-bearing part; reaching a
// real daemon to test them would make them untestable without a daemon.
type Engine interface {
	ImageInspect(ctx context.Context, reference string) (ImageInspect, error)
	ContainerInspect(ctx context.Context, containerID string) (ContainerInspect, error)
	Exec(ctx context.Context, containerID string, command []string) ([]byte, error)
	// ResolveService finds the single running container of one Compose service.
	ResolveService(ctx context.Context, project, service string) (string, error)
}

// HTTPEngine talks to a Docker Engine over its Unix socket.
type HTTPEngine struct {
	client  *http.Client
	baseURL string
}

// DefaultDockerSocket is the Engine socket used when DOCKER_HOST is unset.
const DefaultDockerSocket = "/var/run/docker.sock"

// DockerSocket resolves the Engine socket from the environment.
//
// Only an absolute unix:// endpoint is accepted. A tcp:// endpoint would let the
// identity of the daemon that answered depend on the network, which is not
// something an image label can bind.
func DockerSocket(getenv func(string) string) (string, error) {
	raw := strings.TrimSpace(getenv("DOCKER_HOST"))
	if raw == "" {
		return DefaultDockerSocket, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "unix" || parsed.Host != "" || parsed.User != nil ||
		parsed.Opaque != "" || !filepath.IsAbs(parsed.Path) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("DOCKER_HOST must be an absolute unix:// socket")
	}
	return filepath.Clean(parsed.Path), nil
}

// NewHTTPEngine dials the Engine socket and negotiates its API version.
func NewHTTPEngine(ctx context.Context, socket string) (*HTTPEngine, error) {
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
	engine := &HTTPEngine{client: &http.Client{Transport: transport}, baseURL: "http://docker"}
	var version struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := engine.getJSON(ctx, "/version", &version); err != nil {
		return nil, fmt.Errorf("negotiate Docker Engine API: %w", err)
	}
	if !validAPIVersion(version.APIVersion) {
		return nil, errors.New("Docker Engine returned an invalid API version")
	}
	engine.baseURL = "http://docker/v" + version.APIVersion
	return engine, nil
}

func validAPIVersion(value string) bool {
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

// ImageInspect inspects one image by ID, digest reference or tag.
func (engine *HTTPEngine) ImageInspect(ctx context.Context, reference string) (ImageInspect, error) {
	var inspected ImageInspect
	if strings.TrimSpace(reference) == "" {
		return inspected, errors.New("cannot inspect an image with no reference")
	}
	if err := engine.getJSON(ctx, "/images/"+url.PathEscape(reference)+"/json", &inspected); err != nil {
		return ImageInspect{}, fmt.Errorf("inspect image %s: %w", reference, err)
	}
	if err := requireImageID(inspected.ID); err != nil {
		return ImageInspect{}, fmt.Errorf("image %s: %w", reference, err)
	}
	return inspected, nil
}

// ContainerInspect inspects one container by ID.
func (engine *HTTPEngine) ContainerInspect(ctx context.Context, containerID string) (ContainerInspect, error) {
	var inspected ContainerInspect
	if err := engine.getJSON(ctx, "/containers/"+url.PathEscape(containerID)+"/json", &inspected); err != nil {
		return ContainerInspect{}, fmt.Errorf("inspect container %s: %w", shortImageID(containerID), err)
	}
	if inspected.ID != containerID {
		return ContainerInspect{}, errors.New("Docker Engine returned a different container than the one inspected")
	}
	return inspected, nil
}

// Exec runs one command in a container and returns its combined output.
func (engine *HTTPEngine) Exec(ctx context.Context, containerID string, command []string) ([]byte, error) {
	request := struct {
		AttachStdout bool     `json:"AttachStdout"`
		AttachStderr bool     `json:"AttachStderr"`
		Tty          bool     `json:"Tty"`
		Cmd          []string `json:"Cmd"`
	}{AttachStdout: true, AttachStderr: true, Tty: true, Cmd: command}
	var created struct {
		ID string `json:"Id"`
	}
	body, status, err := engine.request(ctx, http.MethodPost,
		"/containers/"+url.PathEscape(containerID)+"/exec", request)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("Docker exec create returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return nil, errors.New("Docker Engine returned an empty exec id")
	}
	output, status, err := engine.request(ctx, http.MethodPost,
		"/exec/"+url.PathEscape(created.ID)+"/start", map[string]bool{"Detach": false, "Tty": true})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("Docker exec start returned HTTP %d: %s", status, strings.TrimSpace(string(output)))
	}
	var inspected struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	if err := engine.getJSON(ctx, "/exec/"+url.PathEscape(created.ID)+"/json", &inspected); err != nil {
		return nil, err
	}
	if inspected.Running {
		return nil, errors.New("Docker exec was still running after attached start completed")
	}
	if inspected.ExitCode != 0 {
		return nil, fmt.Errorf("container command exited %d: %s", inspected.ExitCode, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// ResolveService finds the one running container of a Compose service.
//
// Exactly one, never the first of several: a project with two containers for one
// service is ambiguous about which one the evidence describes.
func (engine *HTTPEngine) ResolveService(ctx context.Context, project, service string) (string, error) {
	filters, err := json.Marshal(map[string][]string{"label": {
		"com.docker.compose.project=" + project,
		"com.docker.compose.service=" + service,
	}})
	if err != nil {
		return "", err
	}
	var containers []struct {
		ID string `json:"Id"`
	}
	if err := engine.getJSON(ctx, "/containers/json?filters="+url.QueryEscape(string(filters)), &containers); err != nil {
		return "", err
	}
	if len(containers) != 1 || containers[0].ID == "" {
		return "", fmt.Errorf("found %d running containers for project=%q service=%q, want exactly one",
			len(containers), project, service)
	}
	return containers[0].ID, nil
}

func (engine *HTTPEngine) getJSON(ctx context.Context, path string, target any) error {
	body, status, err := engine.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Docker Engine returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("Docker Engine returned trailing JSON data")
	}
	return nil
}

func (engine *HTTPEngine) request(ctx context.Context, method, path string, value any) ([]byte, int, error) {
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, engine.baseURL+path, body)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := engine.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxEngineResponse+1))
	if err != nil {
		return nil, 0, err
	}
	if len(raw) > maxEngineResponse {
		return nil, 0, errors.New("Docker Engine response exceeded 1 MiB")
	}
	return raw, response.StatusCode, nil
}

// requireImageID accepts only a full sha256: content-addressed image ID.
func requireImageID(imageID string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(imageID, prefix) || !validSHA256(strings.TrimPrefix(imageID, prefix)) {
		return fmt.Errorf("%q is not a sha256: content-addressed image ID", imageID)
	}
	return nil
}

func shortImageID(imageID string) string {
	trimmed := strings.TrimPrefix(imageID, "sha256:")
	if len(trimmed) <= 12 {
		return trimmed
	}
	return trimmed[:12]
}
