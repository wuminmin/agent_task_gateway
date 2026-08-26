//go:build taskgate_hostonly

// These cases require host resources the product Compose stack has no reason to
// carry: a Docker socket, the retained qualification artifacts, or a live
// benchmark Dataset. They exercise the evaluation harness rather than the
// product, and the formal campaign exercises the same material at runtime, so
// they sit behind taskgate_hostonly instead of failing the acceptance run.

package experiment

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeHostPreflightReportsOwnersAndNeverMutatesDocker(t *testing.T) {
	toolDir := t.TempDir()
	dockerLog := filepath.Join(t.TempDir(), "docker.log")
	helperPath, err := filepath.Abs(composeHostPreflightPath)
	if err != nil {
		t.Fatal(err)
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	writeTool := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(toolDir, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTool("docker", `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_DOCKER_LOG"
case "$1" in
  compose)
    [[ " $* " == *" config --format json "* ]] || exit 91
    printf '%s\n' '{"services":{"final-v5-direct-postgres":{"ports":[{"host_ip":"127.0.0.1","published":"25534","target":5432,"protocol":"tcp"}]}}}'
    ;;
  ps)
    if [[ " $* " == *" --all "* ]]; then
      if [[ "${FAKE_PROJECT_RESIDUE:-}" == container ]]; then printf '%s\n' prior-container; fi
    elif [[ " $* " == *" publish=25534/tcp "* && "${FAKE_DOCKER_CONFLICT:-0}" == 1 ]]; then
      printf '%s\n' 'taskgate-dbtest-business-postgres-1|taskgate-dbtest|business-postgres|127.0.0.1:25534->5432/tcp'
    fi
    ;;
  volume)
    if [[ "${FAKE_PROJECT_RESIDUE:-}" == volume ]]; then printf '%s\n' prior-volume; fi
    ;;
  network)
    if [[ "${FAKE_PROJECT_RESIDUE:-}" == network ]]; then printf '%s\n' prior-network; fi
    ;;
  *) exit 92 ;;
esac
`)
	writeTool("ss", `#!/usr/bin/env bash
set -euo pipefail
if [[ "${FAKE_HOST_LISTENER:-0}" == 1 ]]; then
  printf '%s\n' 'LISTEN 0 128 127.0.0.1:25534 0.0.0.0:* users:(("host-owner",pid=42,fd=3))'
fi
`)
	writeTool("lsof", `#!/usr/bin/env bash
set -euo pipefail
if [[ "${FAKE_HOST_LISTENER:-0}" == 1 ]]; then
  printf '%s\n' 'COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME' 'host-owner 42 test 3u IPv4 1 0t0 TCP 127.0.0.1:25534 (LISTEN)'
fi
`)

	run := func(extraEnv ...string) ([]byte, error) {
		t.Helper()
		command := exec.Command("bash", helperPath, "taskgate-preflight-test", "compose.yaml")
		command.Dir = repoRoot
		command.Env = append([]string{
			"PATH=" + toolDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"FAKE_DOCKER_LOG=" + dockerLog,
		}, extraEnv...)
		return command.CombinedOutput()
	}

	output, err := run("FAKE_DOCKER_CONFLICT=1")
	if exitError, ok := err.(*exec.ExitError); !ok || exitError.ExitCode() != 2 {
		t.Fatalf("Docker conflict exit = %v, want 2\n%s", err, output)
	}
	for _, required := range []string{
		"127.0.0.1:25534/tcp", "final-v5-direct-postgres",
		"taskgate-dbtest-business-postgres-1", "compose project: taskgate-dbtest",
		"./scripts/db-test-env.sh down", "Preflight made no changes",
	} {
		if !strings.Contains(string(output), required) {
			t.Errorf("Docker conflict output omits %q:\n%s", required, output)
		}
	}

	for _, resource := range []string{"container", "volume", "network"} {
		output, err = run("FAKE_PROJECT_RESIDUE=" + resource)
		if exitError, ok := err.(*exec.ExitError); !ok || exitError.ExitCode() != 2 {
			t.Fatalf("%s residue exit = %v, want 2\n%s", resource, err, output)
		}
		if !strings.Contains(string(output), "earlier Compose project") ||
			!strings.Contains(string(output), "prior-"+resource) {
			t.Fatalf("%s residue is not actionable:\n%s", resource, output)
		}
	}

	output, err = run("FAKE_HOST_LISTENER=1")
	if exitError, ok := err.(*exec.ExitError); !ok || exitError.ExitCode() != 2 {
		t.Fatalf("host listener exit = %v, want 2\n%s", err, output)
	}
	if !strings.Contains(string(output), "host listener:") || !strings.Contains(string(output), "process owner: host-owner") {
		t.Fatalf("host listener owner is not reported:\n%s", output)
	}

	output, err = run()
	if err != nil {
		t.Fatalf("clean host preflight failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "1 host ports free; no Compose residue") {
		t.Fatalf("clean host preflight output = %q", output)
	}

	logBytes, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	log := " " + strings.Join(strings.Fields(string(logBytes)), " ") + " "
	for _, forbidden := range []string{" down ", " stop ", " rm ", " kill "} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("read-only preflight invoked a mutating Docker command %q:\n%s", forbidden, logBytes)
		}
	}
}
