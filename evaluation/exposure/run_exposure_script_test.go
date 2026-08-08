package exposureeval

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const certifiedExposurePostgreSQLImage = "postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55"

type exposureScriptFixture struct {
	root      string
	script    string
	dockerLog string
	probeFile string
	fakeBin   string
	sentinels map[string]string
}

func newExposureScriptFixture(t *testing.T) exposureScriptFixture {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{
		"db/init",
		"evaluation/exposure/raw",
		"evaluation/final-v5-wsl2/sql/datasets",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(directory)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile(filepath.Join("..", "run-exposure.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "evaluation", "run-exposure.sh")
	if err := os.WriteFile(script, source, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerLog := filepath.Join(root, "docker.calls")
	if err := os.WriteFile(dockerLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeDocker := `#!/bin/sh
set -eu
{
  first=1
  for argument in "$@"; do
    if [ "$first" -eq 0 ]; then printf '\t'; fi
    printf '%s' "$argument"
    first=0
  done
  printf '\n'
} >> "$FAKE_DOCKER_LOG"
command=$1
shift
case "$command" in
  info|build|rm)
    exit 0
    ;;
  network)
    exit 0
    ;;
  inspect)
    if [ "${FAKE_EXPOSURE_MODE:-}" = oracle_exit ]; then
      case "$*" in
        *State.ExitCode*) printf '17\n' ;;
        *) printf 'false\n' ;;
      esac
    else
      case "$*" in
        *State.ExitCode*) printf '0\n' ;;
        *) printf 'true\n' ;;
      esac
    fi
    ;;
  logs)
    printf 'PX7_FAKE_POSTGRES_LOG: benchmark dataset mount missing\n'
    ;;
  exec)
    case "$*" in
      *"SHOW server_version_num"*) printf '%s\n' "${FAKE_SERVER_VERSION:-160014}" ;;
    esac
    ;;
  run)
    case "$*" in
      *"--detach --name "*)
        printf 'fake-postgres-container\n'
        ;;
      *"--entrypoint pg_isready"*)
        if [ "${FAKE_EXPOSURE_MODE:-}" = oracle_exit ]; then
          exit 1
        fi
        count=0
        if [ -f "$FAKE_PROBE_FILE" ]; then count=$(cat "$FAKE_PROBE_FILE"); fi
        count=$((count + 1))
        printf '%s\n' "$count" > "$FAKE_PROBE_FILE"
        if [ "${FAKE_EXPOSURE_MODE:-}" = retry ] && [ "$count" -eq 1 ]; then
          exit 1
        fi
        ;;
      *"taskgate-exposure-evaluation-build:local go test"*)
        exit 0
        ;;
      *"taskgate-exposure-evaluation-build:local go version"*)
        printf 'go version go1.25.12 linux/amd64\n'
        ;;
      *"taskgate-exposure-evaluation:local"*)
        exit 73
        ;;
    esac
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakeBin, "docker"), []byte(fakeDocker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinels := map[string]string{
		"evaluation/exposure/results.json":                   "old results\n",
		"evaluation/exposure/rq3-integration.json":           "old integration\n",
		"evaluation/exposure/raw/rq3-postgres-go-test.jsonl": "old raw\n",
	}
	for relative, value := range sentinels {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(relative)), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return exposureScriptFixture{
		root:      root,
		script:    script,
		dockerLog: dockerLog,
		probeFile: filepath.Join(root, "probe.count"),
		fakeBin:   fakeBin,
		sentinels: sentinels,
	}
}

func (fixture exposureScriptFixture) run(t *testing.T, values ...string) ([]byte, error) {
	t.Helper()
	command := exec.Command("/bin/sh", fixture.script)
	command.Dir = fixture.root
	command.Env = exposureEnvironmentWithout("PATH", "TASKGATE_EXPOSURE_POSTGRES_IMAGE",
		"FAKE_EXPOSURE_MODE", "FAKE_SERVER_VERSION", "FAKE_DOCKER_LOG", "FAKE_PROBE_FILE")
	command.Env = append(command.Env,
		"PATH="+fixture.fakeBin+":/usr/bin:/bin",
		"FAKE_DOCKER_LOG="+fixture.dockerLog,
		"FAKE_PROBE_FILE="+fixture.probeFile,
	)
	command.Env = append(command.Env, values...)
	return command.CombinedOutput()
}

func (fixture exposureScriptFixture) calls(t *testing.T) []string {
	t.Helper()
	value, err := os.ReadFile(fixture.dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func TestRunExposureUsesCertifiedPostgreSQLRuntimeAndReadOnlyDatasets(t *testing.T) {
	t.Run("mutable override is rejected before Docker", func(t *testing.T) {
		fixture := newExposureScriptFixture(t)
		output, err := fixture.run(t, "TASKGATE_EXPOSURE_POSTGRES_IMAGE=postgres:16-bookworm")
		if err == nil {
			t.Fatal("mutable PostgreSQL override was accepted")
		}
		if !strings.Contains(string(output), "PostgreSQL image is not the certified 16.14 digest") {
			t.Fatalf("override failed for the wrong reason: %s", output)
		}
		if calls := fixture.calls(t); len(calls) != 0 {
			t.Fatalf("rejected override reached Docker: %v", calls)
		}
	})

	t.Run("certified runtime is fully bound", func(t *testing.T) {
		fixture := newExposureScriptFixture(t)
		_, err := fixture.run(t)
		if err == nil {
			t.Fatal("fake exposure runner did not stop the fixture")
		}
		calls := fixture.calls(t)
		startup := exposureCallWith(calls, "run\t--detach")
		arguments := strings.Split(startup, "\t")
		exposureRequirePair(t, arguments, "--volume", filepath.Join(fixture.root, "db", "init")+":/docker-entrypoint-initdb.d:ro")
		exposureRequirePair(t, arguments, "--volume", filepath.Join(fixture.root, "evaluation", "final-v5-wsl2", "sql", "datasets")+":/opt/taskgate/final-v5-sql:ro")
		exposureRequireToken(t, arguments, certifiedExposurePostgreSQLImage)
		version := exposureCallContaining(calls, "exec\t", "SHOW server_version_num")
		integration := exposureCallContaining(calls, "run\t", "taskgate-exposure-evaluation-build:local", "go\ttest")
		if exposureCallIndex(calls, version) >= exposureCallIndex(calls, integration) {
			t.Fatal("certified PostgreSQL version was not checked before integration")
		}
	})

	t.Run("wrong live version fails before integration", func(t *testing.T) {
		fixture := newExposureScriptFixture(t)
		output, err := fixture.run(t, "FAKE_SERVER_VERSION=160013")
		if err == nil {
			t.Fatal("wrong PostgreSQL patch version was accepted")
		}
		if !strings.Contains(string(output), "server_version_num=160013, want 160014") {
			t.Fatalf("wrong version failed for the wrong reason: %s", output)
		}
		for _, call := range fixture.calls(t) {
			if strings.Contains(call, "taskgate-exposure-evaluation-build:local\tgo\ttest") {
				t.Fatalf("wrong version reached integration: %s", call)
			}
		}
	})
}

func TestRunExposureWaitsForFinalPostgreSQLOverCampaignNetwork(t *testing.T) {
	fixture := newExposureScriptFixture(t)
	_, err := fixture.run(t, "FAKE_EXPOSURE_MODE=retry")
	if err == nil {
		t.Fatal("fake exposure runner did not stop the fixture")
	}
	calls := fixture.calls(t)
	startup := strings.Split(exposureCallWith(calls, "run\t--detach"), "\t")
	container := exposureValueAfter(t, startup, "--name")
	network := exposureValueAfter(t, startup, "--network")
	var probes []string
	var probeIndexes []int
	for index, call := range calls {
		if strings.Contains(call, "--entrypoint\tpg_isready") {
			probes = append(probes, call)
			probeIndexes = append(probeIndexes, index)
		}
		if strings.HasPrefix(call, "exec\t") && strings.Contains(call, "pg_isready") {
			t.Fatalf("readiness fell back to the entrypoint's Unix socket: %s", call)
		}
	}
	if len(probes) != 2 {
		t.Fatalf("network readiness probes = %d, want 2: %v", len(probes), calls)
	}
	for _, probe := range probes {
		arguments := strings.Split(probe, "\t")
		exposureRequirePair(t, arguments, "--network", network)
		exposureRequirePair(t, arguments, "--entrypoint", "pg_isready")
		exposureRequirePair(t, arguments, "--host", container)
		exposureRequirePair(t, arguments, "--port", "5432")
		exposureRequirePair(t, arguments, "--username", "postgres")
		exposureRequirePair(t, arguments, "--dbname", "travel_demo")
		exposureRequireToken(t, arguments, certifiedExposurePostgreSQLImage)
	}
	firstProbe := probeIndexes[0]
	inspect := exposureFirstIndexContaining(calls, "inspect\t", "State.Running")
	secondProbe := probeIndexes[1]
	integration := exposureFirstIndexContaining(calls, "run\t", "taskgate-exposure-evaluation-build:local", "go\ttest")
	if !(firstProbe < inspect && inspect < secondProbe && secondProbe < integration) {
		t.Fatalf("readiness order is not probe/inspect/retry/integration: %v", calls)
	}
}

func TestRunExposureDiagnosesOracleExitWithoutPublishingEvidence(t *testing.T) {
	fixture := newExposureScriptFixture(t)
	output, err := fixture.run(t, "FAKE_EXPOSURE_MODE=oracle_exit")
	if err == nil {
		t.Fatal("exited PostgreSQL oracle was accepted")
	}
	text := string(output)
	if !strings.Contains(text, "PostgreSQL oracle exited before final-server readiness (exit_code=17)") ||
		!strings.Contains(text, "PX7_FAKE_POSTGRES_LOG: benchmark dataset mount missing") {
		t.Fatalf("oracle exit lost its stable diagnostics: %s", output)
	}
	for relative, want := range fixture.sentinels {
		got, readErr := os.ReadFile(filepath.Join(fixture.root, filepath.FromSlash(relative)))
		if readErr != nil || string(got) != want {
			t.Fatalf("failed oracle changed %s: %q, %v", relative, got, readErr)
		}
	}
	calls := fixture.calls(t)
	probe := exposureFirstIndexContaining(calls, "run\t", "--entrypoint\tpg_isready")
	inspect := exposureFirstIndexContaining(calls, "inspect\t", "State.Running")
	logs := exposureFirstIndexContaining(calls, "logs\t")
	remove := exposureFirstIndexContaining(calls, "rm\t--force")
	networkRemove := exposureFirstIndexContaining(calls, "network\trm")
	if !(probe < inspect && inspect < logs && logs < remove && remove < networkRemove) {
		t.Fatalf("oracle diagnostics/cleanup order is wrong: %v", calls)
	}
	for _, call := range calls {
		if strings.Contains(call, "taskgate-exposure-evaluation-build:local\tgo\ttest") ||
			strings.HasPrefix(call, "run\t") && strings.Contains(call, "taskgate-exposure-evaluation:local") {
			t.Fatalf("exited oracle reached an evidence consumer: %s", call)
		}
	}
}

func exposureEnvironmentWithout(names ...string) []string {
	excluded := make(map[string]struct{}, len(names))
	for _, name := range names {
		excluded[name] = struct{}{}
	}
	var result []string
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if _, skip := excluded[name]; found && skip {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func exposureCallWith(calls []string, prefix string) string {
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return call
		}
	}
	return ""
}

func exposureCallContaining(calls []string, fragments ...string) string {
	for _, call := range calls {
		matched := true
		for _, fragment := range fragments {
			matched = matched && strings.Contains(call, fragment)
		}
		if matched {
			return call
		}
	}
	return ""
}

func exposureCallIndex(calls []string, target string) int {
	for index, call := range calls {
		if call == target {
			return index
		}
	}
	return -1
}

func exposureFirstIndexContaining(calls []string, fragments ...string) int {
	return exposureCallIndex(calls, exposureCallContaining(calls, fragments...))
}

func exposureRequireToken(t *testing.T, arguments []string, want string) {
	t.Helper()
	for _, argument := range arguments {
		if argument == want {
			return
		}
	}
	t.Fatalf("argv %v omits %q", arguments, want)
}

func exposureRequirePair(t *testing.T, arguments []string, key, want string) {
	t.Helper()
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == key && arguments[index+1] == want {
			return
		}
	}
	t.Fatalf("argv %v omits pair %q %q", arguments, key, want)
}

func exposureValueAfter(t *testing.T, arguments []string, key string) string {
	t.Helper()
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == key {
			return arguments[index+1]
		}
	}
	t.Fatalf("argv %v omits %s value", arguments, key)
	return ""
}
