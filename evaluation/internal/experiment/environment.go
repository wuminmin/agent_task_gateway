package experiment

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type EnvironmentManifest struct {
	SchemaVersion       int            `json:"schema_version"`
	CampaignID          string         `json:"campaign_id"`
	DeploymentID        string         `json:"deployment_id"`
	CapturedAt          string         `json:"captured_at"`
	GitCommit           string         `json:"git_commit"`
	GitStatus           []string       `json:"git_status"`
	PublicationEligible bool           `json:"publication_eligible"`
	Host                map[string]any `json:"host"`
	Software            map[string]any `json:"software"`
	Storage             map[string]any `json:"storage"`
	Datasets            map[string]any `json:"datasets"`
}

func RecordEnvironment(repo, campaignID, deploymentID string, eligible bool, datasets map[string]any) (EnvironmentManifest, error) {
	if repo == "" || campaignID == "" || deploymentID == "" {
		return EnvironmentManifest{}, errors.New("repo, campaign, and deployment are required")
	}
	run := func(name string, args ...string) (string, error) {
		command := exec.Command(name, args...)
		command.Dir = repo
		value, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("record environment command %s: %w", name, err)
		}
		return strings.TrimSpace(string(value)), nil
	}
	commit, err := run("git", "rev-parse", "HEAD")
	if err != nil {
		return EnvironmentManifest{}, err
	}
	statusText, err := run("git", "status", "--short")
	if err != nil {
		return EnvironmentManifest{}, err
	}
	var status []string
	if statusText != "" {
		status = strings.Split(statusText, "\n")
	}
	manifest := EnvironmentManifest{SchemaVersion: 1, CampaignID: campaignID, DeploymentID: deploymentID, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), GitCommit: commit, GitStatus: status, PublicationEligible: eligible, Datasets: datasets}
	capture := func(target map[string]any, key, name string, args ...string) error {
		value, captureErr := run(name, args...)
		if captureErr != nil {
			return captureErr
		}
		if value == "" {
			return fmt.Errorf("record environment command %s returned empty output", name)
		}
		target[key] = value
		return nil
	}
	manifest.Host = map[string]any{"goos": runtime.GOOS, "goarch": runtime.GOARCH}
	for _, command := range []struct {
		key, name string
		args      []string
	}{
		{"os_release", "cat", []string{"/etc/os-release"}}, {"uname", "uname", []string{"-a"}},
		{"lscpu", "lscpu", nil}, {"nproc", "nproc", nil}, {"memory", "free", []string{"-b"}},
		{"meminfo", "cat", []string{"/proc/meminfo"}}, {"vmstat", "cat", []string{"/proc/vmstat"}},
	} {
		if err := capture(manifest.Host, command.key, command.name, command.args...); err != nil {
			return EnvironmentManifest{}, err
		}
	}
	manifest.Software = map[string]any{}
	for _, command := range []struct {
		key, name string
		args      []string
	}{
		{"go", "go", []string{"version"}}, {"postgresql", "psql", []string{"--version"}},
		{"docker_engine", "docker", []string{"version", "--format", "{{json .}}"}}, {"docker_compose", "docker", []string{"compose", "version"}},
		{"docker_info", "docker", []string{"info", "--format", "{{json .}}"}}, {"containerd", "containerd", []string{"--version"}},
		{"image_ids", "docker", []string{"image", "ls", "--no-trunc", "--digests", "--format", "{{.Repository}} {{.Tag}} {{.Digest}} {{.ID}}"}},
	} {
		if err := capture(manifest.Software, command.key, command.name, command.args...); err != nil {
			return EnvironmentManifest{}, err
		}
	}
	manifest.Storage = map[string]any{}
	for _, command := range []struct {
		key, name string
		args      []string
	}{
		{"df", "df", []string{"-T"}}, {"lsblk", "lsblk", []string{"-f"}}, {"cgroup_filesystem", "stat", []string{"-fc", "%T", "/sys/fs/cgroup"}},
	} {
		if err := capture(manifest.Storage, command.key, command.name, command.args...); err != nil {
			return EnvironmentManifest{}, err
		}
	}
	return manifest, nil
}

func WriteEnvironment(path string, manifest EnvironmentManifest) error {
	value, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeExclusive(path, append(value, '\n'))
}

func WriteDeployment(path string, manifest DeploymentManifest) error {
	if manifest.SchemaVersion != 1 || manifest.CampaignID == "" || manifest.DeploymentID == "" ||
		!manifest.FreshDeployment || !validSHA256(manifest.FreshDeploymentProofSHA256) || !validSHA256(manifest.EnvironmentSHA256) || !validSHA256(manifest.WindowsEnvironmentSHA256) || !validSHA256(manifest.VMStatBeforeSHA256) || !validSHA256(manifest.VMStatAfterSHA256) || manifest.StartedAt == "" || manifest.FinishedAt == "" ||
		manifest.SwapInDelta < 0 || manifest.SwapOutDelta < 0 || manifest.UnexpectedContainerRestarts < 0 {
		return errors.New("invalid deployment manifest")
	}
	value, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeExclusive(path, append(value, '\n'))
}

func ReadDatasetBindings(path string) (map[string]any, error) {
	if path == "" {
		return map[string]any{}, nil
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := StrictJSON(value, &decoded); err != nil {
		return nil, err
	}
	redacted, ok := RedactSecrets(decoded).(map[string]any)
	if !ok {
		return nil, errors.New("dataset binding is not an object")
	}
	return redacted, nil
}
