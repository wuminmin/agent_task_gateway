package exposureeval

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const certifiedExposurePostgreSQLImage = "postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55"

type exposureScriptFixture struct {
	root              string
	script            string
	dockerLog         string
	probeFile         string
	report            string
	integrationLog    string
	missingPackageLog string
	skippedTestLog    string
	fakeBin           string
	sentinels         map[string]string
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
	recorderSource, err := os.ReadFile("record_integration.py")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "evaluation", "exposure", "record_integration.py"), recorderSource, 0o700); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(root, "exposure-report.json")
	reportDocument := `{
  "schema_version": 7,
  "rq1_ground_truth": {"cases":24,"passed":24},
  "rq2_rewrite_invariance": {
    "generated_attempts":1024,
    "unique_normalized_pairs":1024,
    "executed_unique_pairs":1024,
    "duplicate_attempts":0,
    "mismatches":0,
    "postgres_major":16,
    "postgres_version":"16.14 (Debian 16.14-1.pgdg120+1)"
  },
  "rq2_exposure_invariance": {"status":"complete","mismatches":0},
  "rq3_anti_arbitrage": {
    "deterministic_cases":5,
    "deterministic_passed":5,
    "postgres_integration_manifest": [
      {"id":"concurrent_settlement","package":"taskbound.local/agent-data-gateway/internal/control","test":"TestConcurrentTaskFamilySettlementCannotOverspend"},
      {"id":"distinct_zero_result_predicates","package":"taskbound.local/agent-data-gateway/internal/gateway","test":"TestExposureV3ChargesDistinctZeroResultPredicates"},
      {"id":"online_relational_gateway_settlement","package":"taskbound.local/agent-data-gateway/internal/gateway","test":"TestRelationalGatewayEndToEndAgainstPostgreSQL"},
      {"id":"online_relational_postgres","package":"taskbound.local/agent-data-gateway/internal/gateway","test":"TestRelationalOnlinePathAgainstPostgreSQL"},
      {"id":"task_family_delegation","package":"taskbound.local/agent-data-gateway/internal/control","test":"TestDelegatedTasksShareRootAccountingState"}
    ]
  },
  "rq4_scaling": {"status":"complete","curves":[{},{},{}]}
}
`
	if err := os.WriteFile(report, []byte(reportDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	integrationLog := filepath.Join(root, "integration.jsonl")
	integrationDocument := strings.Join([]string{
		`{"Time":"2026-08-09T00:00:00Z","Action":"pass","Package":"taskbound.local/agent-data-gateway/internal/control","Test":"TestConcurrentTaskFamilySettlementCannotOverspend","Elapsed":0.01}`,
		`{"Time":"2026-08-09T00:00:01Z","Action":"pass","Package":"taskbound.local/agent-data-gateway/internal/control","Test":"TestDelegatedTasksShareRootAccountingState","Elapsed":0.01}`,
		`{"Time":"2026-08-09T00:00:02Z","Action":"pass","Package":"taskbound.local/agent-data-gateway/internal/control","Elapsed":0.02}`,
		`{"Time":"2026-08-09T00:00:03Z","Action":"pass","Package":"taskbound.local/agent-data-gateway/internal/gateway","Test":"TestExposureV3ChargesDistinctZeroResultPredicates","Elapsed":0.01}`,
		`{"Time":"2026-08-09T00:00:04Z","Action":"pass","Package":"taskbound.local/agent-data-gateway/internal/gateway","Test":"TestRelationalGatewayEndToEndAgainstPostgreSQL","Elapsed":0.01}`,
		`{"Time":"2026-08-09T00:00:05Z","Action":"pass","Package":"taskbound.local/agent-data-gateway/internal/gateway","Test":"TestRelationalOnlinePathAgainstPostgreSQL","Elapsed":0.01}`,
		`{"Time":"2026-08-09T00:00:06Z","Action":"pass","Package":"taskbound.local/agent-data-gateway/internal/gateway","Elapsed":0.03}`,
		"",
	}, "\n")
	if err := os.WriteFile(integrationLog, []byte(integrationDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	missingPackageLog := filepath.Join(root, "integration-missing-package-pass.jsonl")
	missingPackageDocument := strings.Replace(integrationDocument,
		`{"Time":"2026-08-09T00:00:06Z","Action":"pass","Package":"taskbound.local/agent-data-gateway/internal/gateway","Elapsed":0.03}`+"\n", "", 1)
	if err := os.WriteFile(missingPackageLog, []byte(missingPackageDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	skippedTestLog := filepath.Join(root, "integration-skipped-test.jsonl")
	skippedTestDocument := strings.Replace(integrationDocument,
		`"Action":"pass","Package":"taskbound.local/agent-data-gateway/internal/control","Test":"TestConcurrentTaskFamilySettlementCannotOverspend"`,
		`"Action":"skip","Package":"taskbound.local/agent-data-gateway/internal/control","Test":"TestConcurrentTaskFamilySettlementCannotOverspend"`, 1)
	if err := os.WriteFile(skippedTestLog, []byte(skippedTestDocument), 0o600); err != nil {
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
        case "${FAKE_EXPOSURE_MODE:-}" in
          record_malformed) cat "$FAKE_INTEGRATION_LOG"; printf 'not-json\n' ;;
          record_missing_package) cat "$FAKE_MISSING_PACKAGE_LOG" ;;
          record_skipped_test) cat "$FAKE_SKIPPED_TEST_LOG" ;;
          record_extra_package) cat "$FAKE_INTEGRATION_LOG"; printf '{"Action":"pass","Package":"taskbound.local/agent-data-gateway/internal/unrelated"}\n' ;;
          record_*) cat "$FAKE_INTEGRATION_LOG" ;;
        esac
        if [ "${FAKE_EXPOSURE_MODE:-}" = record_incomplete ]; then exit 1; fi
        ;;
      *"taskgate-exposure-evaluation-build:local go version"*)
        printf 'go version go1.25.12 linux/amd64\n'
        ;;
      *"taskgate-exposure-evaluation:local"*)
        case "${FAKE_EXPOSURE_MODE:-}" in
          record_incomplete_report) sed 's/"passed":24/"passed":20/' "$FAKE_EXPOSURE_REPORT" ;;
          record_*) cat "$FAKE_EXPOSURE_REPORT" ;;
          *) exit 73 ;;
        esac
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
		root:              root,
		script:            script,
		dockerLog:         dockerLog,
		probeFile:         filepath.Join(root, "probe.count"),
		report:            report,
		integrationLog:    integrationLog,
		missingPackageLog: missingPackageLog,
		skippedTestLog:    skippedTestLog,
		fakeBin:           fakeBin,
		sentinels:         sentinels,
	}
}

func (fixture exposureScriptFixture) run(t *testing.T, values ...string) ([]byte, error) {
	t.Helper()
	command := exec.Command("/bin/sh", fixture.script)
	command.Dir = fixture.root
	command.Env = exposureEnvironmentWithout("PATH", "TASKGATE_EXPOSURE_POSTGRES_IMAGE",
		"TASKGATE_EXPOSURE_EVAL_IMAGE", "TASKGATE_EXPOSURE_BUILD_IMAGE",
		"FAKE_EXPOSURE_MODE", "FAKE_SERVER_VERSION", "FAKE_DOCKER_LOG", "FAKE_PROBE_FILE",
		"FAKE_EXPOSURE_REPORT", "FAKE_INTEGRATION_LOG", "FAKE_MISSING_PACKAGE_LOG", "FAKE_SKIPPED_TEST_LOG")
	command.Env = append(command.Env,
		"PATH="+fixture.fakeBin+":/usr/bin:/bin",
		"FAKE_DOCKER_LOG="+fixture.dockerLog,
		"FAKE_PROBE_FILE="+fixture.probeFile,
		"FAKE_EXPOSURE_REPORT="+fixture.report,
		"FAKE_INTEGRATION_LOG="+fixture.integrationLog,
		"FAKE_MISSING_PACKAGE_LOG="+fixture.missingPackageLog,
		"FAKE_SKIPPED_TEST_LOG="+fixture.skippedTestLog,
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

func TestRunExposurePublishesOnlyCompleteIntegrationEvidence(t *testing.T) {
	t.Run("complete evidence closes both digest levels", func(t *testing.T) {
		fixture := newExposureScriptFixture(t)
		output, err := fixture.run(t, "FAKE_EXPOSURE_MODE=record_complete")
		if err != nil {
			t.Fatalf("complete synthetic evidence failed: %v (%s)", err, output)
		}
		rawPath := filepath.Join(fixture.root, "evaluation", "exposure", "raw", "rq3-postgres-go-test.jsonl")
		artifactPath := filepath.Join(fixture.root, "evaluation", "exposure", "rq3-integration.json")
		resultsPath := filepath.Join(fixture.root, "evaluation", "exposure", "results.json")
		var artifact struct {
			Status          string `json:"status"`
			Command         string `json:"command"`
			CommandExitCode int    `json:"command_exit_code"`
			RaceEnabled     bool   `json:"race_enabled"`
			RawLog          string `json:"raw_log"`
			RawLogSHA256    string `json:"raw_log_sha256"`
			Executed        int    `json:"executed"`
			Passed          int    `json:"passed"`
			Failed          int    `json:"failed"`
			Tests           []struct {
				ID      string `json:"id"`
				Package string `json:"package"`
				Test    string `json:"test"`
				Status  string `json:"status"`
			} `json:"tests"`
		}
		exposureReadJSON(t, artifactPath, &artifact)
		if artifact.Status != "complete" || artifact.CommandExitCode != 0 || !artifact.RaceEnabled ||
			artifact.Executed != 5 || artifact.Passed != 5 || artifact.Failed != 0 {
			t.Fatalf("published artifact is not complete 5/5/0 race evidence: %+v", artifact)
		}
		integrationCall := exposureCallContaining(fixture.calls(t), "taskgate-exposure-evaluation-build:local", "go\ttest")
		callArguments := strings.Split(integrationCall, "\t")
		imageIndex := -1
		for index, argument := range callArguments {
			if argument == "taskgate-exposure-evaluation-build:local" {
				imageIndex = index
				break
			}
		}
		if imageIndex < 0 || artifact.Command != strings.Join(callArguments[imageIndex+1:], " ") {
			t.Fatalf("published command does not match the integration invocation: artifact=%q call=%q", artifact.Command, integrationCall)
		}
		if artifact.RawLog != "evaluation/exposure/raw/rq3-postgres-go-test.jsonl" ||
			artifact.RawLogSHA256 != exposureFileSHA256(t, rawPath) {
			t.Fatalf("published raw binding is not closed: %+v", artifact)
		}
		publishedRaw, err := os.ReadFile(rawPath)
		if err != nil {
			t.Fatal(err)
		}
		integrationRaw, err := os.ReadFile(fixture.integrationLog)
		if err != nil {
			t.Fatal(err)
		}
		if string(publishedRaw) != string(integrationRaw) {
			t.Fatal("published raw log is not the integration process output")
		}
		wantTests := map[string]string{
			"concurrent_settlement":                "taskbound.local/agent-data-gateway/internal/control\x00TestConcurrentTaskFamilySettlementCannotOverspend",
			"distinct_zero_result_predicates":      "taskbound.local/agent-data-gateway/internal/gateway\x00TestExposureV3ChargesDistinctZeroResultPredicates",
			"online_relational_gateway_settlement": "taskbound.local/agent-data-gateway/internal/gateway\x00TestRelationalGatewayEndToEndAgainstPostgreSQL",
			"online_relational_postgres":           "taskbound.local/agent-data-gateway/internal/gateway\x00TestRelationalOnlinePathAgainstPostgreSQL",
			"task_family_delegation":               "taskbound.local/agent-data-gateway/internal/control\x00TestDelegatedTasksShareRootAccountingState",
		}
		if len(artifact.Tests) != len(wantTests) {
			t.Fatalf("published artifact has %d named tests, want %d", len(artifact.Tests), len(wantTests))
		}
		for _, test := range artifact.Tests {
			want, ok := wantTests[test.ID]
			if !ok || test.Status != "pass" || test.Package+"\x00"+test.Test != want {
				t.Fatalf("published artifact carries the wrong named test: %+v", test)
			}
			delete(wantTests, test.ID)
		}
		if len(wantTests) != 0 {
			t.Fatalf("published artifact omitted named tests: %v", wantTests)
		}
		var results struct {
			RQ3 struct {
				Integration struct {
					Status         string `json:"status"`
					Artifact       string `json:"artifact"`
					ArtifactSHA256 string `json:"artifact_sha256"`
					Executed       int    `json:"executed"`
					Passed         int    `json:"passed"`
					Failed         int    `json:"failed"`
				} `json:"postgres_integration"`
			} `json:"rq3_anti_arbitrage"`
		}
		exposureReadJSON(t, resultsPath, &results)
		integration := results.RQ3.Integration
		if integration.Status != "complete" || integration.Executed != 5 || integration.Passed != 5 || integration.Failed != 0 ||
			integration.Artifact != "evaluation/exposure/rq3-integration.json" ||
			integration.ArtifactSHA256 != exposureFileSHA256(t, artifactPath) {
			t.Fatalf("published result binding is not closed: %+v", integration)
		}
		exposureRequireNoStagingDirectory(t, fixture.root)
	})

	for _, test := range []struct {
		name   string
		mode   string
		reason string
	}{
		{name: "nonzero integration", mode: "record_incomplete", reason: "canonical evidence was not changed"},
		{name: "malformed JSONL", mode: "record_malformed", reason: "invalid go-test JSON"},
		{name: "missing package pass", mode: "record_missing_package", reason: "canonical evidence was not changed"},
		{name: "skipped named test", mode: "record_skipped_test", reason: "canonical evidence was not changed"},
		{name: "extra package pass", mode: "record_extra_package", reason: "canonical evidence was not changed"},
		{name: "incomplete evaluation report", mode: "record_incomplete_report", reason: "staged exposure report is incomplete: RQ1=24/24"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExposureScriptFixture(t)
			output, err := fixture.run(t, "FAKE_EXPOSURE_MODE="+test.mode)
			if err == nil {
				t.Fatalf("%s evidence was published", test.name)
			}
			if !strings.Contains(string(output), test.reason) {
				t.Fatalf("%s failed for the wrong reason: %s", test.name, output)
			}
			exposureRequireSentinels(t, fixture)
			exposureRequireNoStagingDirectory(t, fixture.root)
		})
	}

	t.Run("symlink destination", func(t *testing.T) {
		fixture := newExposureScriptFixture(t)
		rawPath := filepath.Join(fixture.root, "evaluation", "exposure", "raw", "rq3-postgres-go-test.jsonl")
		targetPath := filepath.Join(fixture.root, "old-raw-target")
		if err := os.WriteFile(targetPath, []byte("old raw\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(rawPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(targetPath, rawPath); err != nil {
			t.Fatal(err)
		}
		output, err := fixture.run(t, "FAKE_EXPOSURE_MODE=record_complete")
		if err == nil {
			t.Fatal("symlink evidence destination was accepted")
		}
		if !strings.Contains(string(output), "evidence destination must be a regular file or absent") {
			t.Fatalf("symlink destination failed for the wrong reason: %s", output)
		}
		exposureRequireSentinels(t, fixture)
		exposureRequireNoStagingDirectory(t, fixture.root)
	})
}

func exposureReadJSON(t *testing.T, path string, target any) {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(value, target); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func exposureFileSHA256(t *testing.T, path string) string {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(value))
}

func exposureRequireSentinels(t *testing.T, fixture exposureScriptFixture) {
	t.Helper()
	for relative, want := range fixture.sentinels {
		got, err := os.ReadFile(filepath.Join(fixture.root, filepath.FromSlash(relative)))
		if err != nil || string(got) != want {
			t.Fatalf("failed evidence changed %s: %q, %v", relative, got, err)
		}
	}
}

func exposureRequireNoStagingDirectory(t *testing.T, root string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "evaluation", "exposure", ".record-integration.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("RQ3 evidence staging survived: %v", matches)
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
