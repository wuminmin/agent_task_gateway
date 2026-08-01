package experiment

import (
	"encoding/json"
	"errors"
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
	run := func(name string, args ...string) string {
		command := exec.Command(name, args...)
		command.Dir = repo
		value, err := command.CombinedOutput()
		if err != nil {
			return "ERROR: " + err.Error()
		}
		return strings.TrimSpace(string(value))
	}
	commit := run("git", "rev-parse", "HEAD")
	statusText := run("git", "status", "--short")
	var status []string
	if statusText != "" {
		status = strings.Split(statusText, "\n")
	}
	manifest := EnvironmentManifest{SchemaVersion: 1, CampaignID: campaignID, DeploymentID: deploymentID, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), GitCommit: commit, GitStatus: status, PublicationEligible: eligible, Datasets: datasets}
	manifest.Host = map[string]any{"os_release": run("cat", "/etc/os-release"), "uname": run("uname", "-a"), "lscpu": run("lscpu"), "nproc": run("nproc"), "memory": run("free", "-b"), "meminfo": run("cat", "/proc/meminfo"), "vmstat": run("cat", "/proc/vmstat"), "goos": runtime.GOOS, "goarch": runtime.GOARCH}
	manifest.Software = map[string]any{"go": run("go", "version"), "postgresql": run("psql", "--version"), "docker_engine": run("docker", "version", "--format", "{{json .}}"), "docker_compose": run("docker", "compose", "version"), "docker_info": run("docker", "info", "--format", "{{json .}}"), "containerd": run("containerd", "--version"), "image_ids": run("docker", "image", "ls", "--no-trunc", "--digests", "--format", "{{.Repository}} {{.Tag}} {{.Digest}} {{.ID}}")}
	manifest.Storage = map[string]any{"df": run("df", "-T"), "lsblk": run("lsblk", "-f"), "cgroup_filesystem": run("stat", "-fc", "%T", "/sys/fs/cgroup")}
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
		!manifest.FreshDeployment || !validSHA256(manifest.EnvironmentSHA256) || !validSHA256(manifest.WindowsEnvironmentSHA256) || !validSHA256(manifest.VMStatBeforeSHA256) || !validSHA256(manifest.VMStatAfterSHA256) || manifest.StartedAt == "" || manifest.FinishedAt == "" ||
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
