//go:build taskgate_hostonly

// These cases require host resources the product Compose stack has no reason to
// carry: a Docker socket, the retained qualification artifacts, or a live
// benchmark Dataset. They exercise the evaluation harness rather than the
// product, and the formal campaign exercises the same material at runtime, so
// they sit behind taskgate_hostonly instead of failing the acceptance run.

package experiment

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactTargetedBuildManifestPreservesALargeSourceListingOffArgv(t *testing.T) {
	block := artifactTargetedBuildSealedFunction(t, artifactTargetedLauncherBody(t))
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "sealed-binary")
	manifestPath := filepath.Join(directory, "sealed-binary.build.json")
	listingPath := filepath.Join(directory, "source-listing.txt")
	const commit = "0123456789abcdef0123456789abcdef01234567"
	const buildCommand = "go build -buildvcs=false -trimpath -o sealed-binary ./synthetic-target"
	const goVersion = "go version go-test-only linux/amd64"

	shell := "set -euo pipefail\n" + block + `
go() {
  if [[ "$1" == "version" ]]; then
    printf '%s\n' '` + goVersion + `'
    return 0
  fi
  if [[ "$1" != "build" ]]; then
    return 64
  fi
  shift
  local output=""
  while (( $# > 0 )); do
    if [[ "$1" == "-o" ]]; then
      output="$2"
      shift 2
      continue
    fi
    shift
  done
  [[ -n "$output" ]]
  printf '%s' 'SEALED-BINARY-CONTENT' > "$output"
}

binary="$1"
manifest="$2"
listing="$3"
commit="$4"
build_command="$5"
export LC_ALL=C
source_listing="$(awk 'BEGIN {
  for (i = 0; i < 4096; i++) {
    printf "%064d  tracked/path/%06d.go\n", 0, i
  }
}')"
printf '%s' "$source_listing" > "$listing"
source_sha="$(printf '%s' "$source_listing" | sha256sum | awk '{print $1}')"
build_sealed ./synthetic-target "$binary" "$manifest" "$build_command" >/dev/null
`
	command := exec.Command("bash", "-c", shell, "large-listing-build-manifest-test",
		binaryPath, manifestPath, listingPath, commit, buildCommand)
	argvBytes := 0
	for index, argument := range command.Args {
		argvBytes += len(argument)
		if len(argument) >= 256*1024 {
			t.Fatalf("test process argv[%d] carries a large source listing (%d bytes)", index, len(argument))
		}
	}
	if argvBytes >= 64*1024 {
		t.Fatalf("test process argv carries %d bytes; the large listing must be generated inside bash", argvBytes)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("execute exact build_sealed helper with a large in-shell listing: %v\n%s", err, output)
	}

	listing, err := os.ReadFile(listingPath)
	if err != nil {
		t.Fatalf("read generated source listing: %v", err)
	}
	if len(listing) < 256*1024 {
		t.Fatalf("source listing is only %d bytes; want at least 256 KiB", len(listing))
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read build manifest: %v", err)
	}
	var manifest struct {
		SchemaVersion    int    `json:"schema_version"`
		SubmissionCommit string `json:"submission_commit"`
		BinarySHA256     string `json:"binary_sha256"`
		SourceSHA256     string `json:"source_sha256"`
		GoVersion        string `json:"go_version"`
		BuildCommand     string `json:"build_command"`
		SourceFiles      string `json:"source_files"`
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("strict-decode build manifest: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("build manifest has a trailing JSON value: %v", err)
	}
	if manifest.SchemaVersion != 1 || manifest.SubmissionCommit != commit ||
		manifest.BuildCommand != buildCommand || manifest.GoVersion != goVersion {
		t.Fatalf("build manifest fixed identity = %+v", manifest)
	}
	if !bytes.Equal([]byte(manifest.SourceFiles), listing) {
		t.Fatal("build manifest source_files does not preserve the in-shell listing byte for byte")
	}
	if manifest.SourceSHA256 != artifactTargetedScriptSHA256(listing) {
		t.Fatalf("source SHA-256 = %q, want digest of exact source_files", manifest.SourceSHA256)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read fake sealed binary: %v", err)
	}
	if manifest.BinarySHA256 != artifactTargetedScriptSHA256(binary) {
		t.Fatalf("binary SHA-256 = %q, want digest of exact binary", manifest.BinarySHA256)
	}
}

func TestArtifactTargetedFormalWindowGateRequiresThreeExactPasses(t *testing.T) {
	body := artifactTargetedLauncherBody(t)
	block := launcherShellBlock(t, body, "FORMAL_WINDOW_GATE_ADJUDICATION")
	tests := []string{
		"TestFormalDeploymentRunsTheApprovedHealthcheckLive",
		"TestPeriodicLivenessProbesAddNoBusinessStatements",
		"TestExplicitReadinessOutsideTheWindowStillAttests",
	}
	expected, err := json.Marshal(tests)
	if err != nil {
		t.Fatal(err)
	}
	event := func(name, action string) string {
		payload, err := json.Marshal(map[string]string{"Action": action, "Test": name})
		if err != nil {
			t.Fatal(err)
		}
		return string(payload)
	}
	packageEvent := func(action string) string {
		payload, err := json.Marshal(map[string]string{"Action": action})
		if err != nil {
			t.Fatal(err)
		}
		return string(payload)
	}
	passes := []string{event(tests[0], "pass"), event(tests[1], "pass"), event(tests[2], "pass")}

	for _, test := range []struct {
		name    string
		events  []string
		wantErr bool
	}{
		{name: "three exact passes", events: passes},
		{name: "skip", events: []string{passes[0], event(tests[1], "skip"), passes[2]}, wantErr: true},
		{name: "failure", events: []string{passes[0], event(tests[1], "fail"), passes[2]}, wantErr: true},
		{name: "missing", events: passes[:2], wantErr: true},
		{name: "duplicate terminal", events: append(append([]string{}, passes...), passes[2]), wantErr: true},
		{name: "unexpected terminal", events: []string{passes[0], passes[1], event("TestUnexpected", "pass")}, wantErr: true},
		{name: "package skip", events: append(append([]string{}, passes...), packageEvent("skip")), wantErr: true},
		{name: "package failure", events: append(append([]string{}, passes...), packageEvent("fail")), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := filepath.Join(t.TempDir(), "formal-window.jsonl")
			if err := os.WriteFile(report, []byte(strings.Join(test.events, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := runLauncherShellBlock(block,
				`require_formal_window_gate_passes "$1" "$2"`, report, string(expected))
			if test.wantErr && err == nil {
				t.Fatalf("gate adjudication unexpectedly succeeded: %s", output)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("gate adjudication failed: %v\n%s", err, output)
			}
		})
	}
}

func TestArtifactTargetedRejectionReportDisambiguatesFinalizerOutcomes(t *testing.T) {
	body := artifactTargetedLauncherBody(t)
	block := launcherShellBlock(t, body, "ARTIFACT_REJECTION_REPORT")
	samples := filepath.Join(t.TempDir(), "samples.jsonl")
	rows := strings.Join([]string{
		`{"sample_id":"rejected","scale":"100x4","status":"fail","error_code":"artifact_measurement_failed","taskgate_rejection_v1":{"version":"taskgate-rejection-v1"}}`,
		`{"sample_id":"postcheck","scale":"100x4","status":"fail","error_code":"artifact_evidence_invariant_failed","taskgate_acceptance_v3":{"version":"taskgate-finalization-v3"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(samples, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := runLauncherShellBlock(block, `
set -euo pipefail
report_retained_artifact_rejections "$1"
`, samples)
	if err != nil {
		t.Fatalf("rejection report failed: %v\n%s", err, output)
	}
	for _, required := range []string{
		`"sample_id":"rejected"`,
		`"taskgate_acceptance_v3_present":false`,
		`"taskgate_rejection_v1":{"version":"taskgate-rejection-v1"}`,
		`"sample_id":"postcheck"`,
		`"taskgate_acceptance_v3_present":true`,
		`"taskgate_rejection_v1":null`,
	} {
		if !bytes.Contains(output, []byte(required)) {
			t.Errorf("machine rejection report %q omits %q", output, required)
		}
	}
}

func TestArtifactTargetedScaleSelectionIsAValidatedFrozenSubset(t *testing.T) {
	body := artifactTargetedLauncherBody(t)
	block := launcherShellBlock(t, body, "ARTIFACT_SCALE_SELECTION")

	defaultOutput, err := runLauncherShellBlock(block, `artifact_default_scales`)
	if err != nil {
		t.Fatalf("default scale selection: %v\n%s", err, defaultOutput)
	}
	const allScales = "100x4,10k-x4,100k-x4,100x16,10k-x16,100k-x16"
	if got := strings.TrimSpace(string(defaultOutput)); got != allScales {
		t.Fatalf("default scale selection = %q, want %q", got, allScales)
	}

	for _, test := range []struct {
		name      string
		selection string
		want      []string
	}{
		{name: "single canary cell", selection: "100x4", want: []string{"100x4"}},
		{name: "two cells retain frozen order", selection: "100k-x16,100x4", want: []string{"100x4", "100k-x16"}},
		{name: "all cells", selection: allScales, want: strings.Split(allScales, ",")},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := runLauncherShellBlock(block, `resolve_artifact_scales "$1"`, test.selection)
			if err != nil {
				t.Fatalf("resolve %q: %v\n%s", test.selection, err, output)
			}
			var got []string
			if err := json.Unmarshal(output, &got); err != nil {
				t.Fatalf("decode scale selection %q: %v\n%s", test.selection, err, output)
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("resolve %q = %v, want %v", test.selection, got, test.want)
			}
		})
	}

	for _, selection := range []string{
		"",
		"unknown",
		"100x4,100x4",
		"100x4,",
		",100x4",
		"100x4, 10k-x4",
	} {
		t.Run("reject "+selection, func(t *testing.T) {
			if output, err := runLauncherShellBlock(block, `resolve_artifact_scales "$1"`, selection); err == nil {
				t.Fatalf("resolve %q unexpectedly succeeded: %s", selection, output)
			}
		})
	}
}
