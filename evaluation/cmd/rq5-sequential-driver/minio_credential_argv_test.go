package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type composeCommandDocument struct {
	Services map[string]struct {
		Command []string `yaml:"command"`
	} `yaml:"services"`
}

func TestMinIOInitCredentialsUseStdinInsteadOfMCArguments(t *testing.T) {
	tests := []struct {
		name        string
		composePath string
		aliasLine   string
	}{
		{
			name:        "formal-compose",
			composePath: filepath.Join("..", "..", "..", "compose.yaml"),
			aliasLine:   "mc alias set taskgate http://result-object-store:9000",
		},
		{
			name:        "rq5-final-route",
			composePath: filepath.Join("..", "..", "daily-publication-online", "compose.yaml"),
			aliasLine:   "mc alias set rq5 http://result-object-store:9000",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := minioInitCommand(t, test.composePath)
			assertLiteralContainerExpansion(t, script, "MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD")
			pipeline := credentialPipeline(t, script, test.aliasLine)
			runCredentialPipeline(t, pipeline, map[string]string{
				"MINIO_ROOT_USER":     "test-root-identifier",
				"MINIO_ROOT_PASSWORD": "test-root-secret-value",
			}, strings.Fields(test.aliasLine)[1:], "test-root-identifier\ntest-root-secret-value\n")
		})
	}

	formalScript := minioInitCommand(t, filepath.Join("..", "..", "..", "compose.yaml"))
	assertLiteralContainerExpansion(t, formalScript,
		"GATEWAY_OBJECT_STORE_ACCESS_KEY", "GATEWAY_OBJECT_STORE_SECRET_KEY")
	userAddPipeline := credentialPipeline(t, formalScript, "mc admin user add taskgate")
	runCredentialPipeline(t, userAddPipeline, map[string]string{
		"GATEWAY_OBJECT_STORE_ACCESS_KEY": "test-gateway-identifier",
		"GATEWAY_OBJECT_STORE_SECRET_KEY": "test-gateway-secret-value",
	}, []string{"admin", "user", "add", "taskgate"},
		"test-gateway-identifier\ntest-gateway-secret-value\n")

	// The pinned mc release has no stdin or environment flag for policy attach:
	// MinIO's username is the access-key identifier. Keep that identifier-only
	// exception while ensuring the gateway secret never reaches this argv sink.
	attachLine := `mc admin policy attach taskgate taskgate-result-artifacts --user "$${GATEWAY_OBJECT_STORE_ACCESS_KEY}"`
	if !strings.Contains(formalScript, attachLine) {
		t.Fatal("formal MinIO init no longer attaches the least-privilege policy to the independent gateway user")
	}
	if strings.Contains(lineContaining(formalScript, "mc admin policy attach"), "GATEWAY_OBJECT_STORE_SECRET_KEY") {
		t.Fatal("gateway secret reached the policy-attach command")
	}
}

func minioInitCommand(t *testing.T, composePath string) string {
	t.Helper()
	contents, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	var document composeCommandDocument
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	service, ok := document.Services["result-object-store-init"]
	if !ok || len(service.Command) != 1 {
		t.Fatal("Compose must define exactly one result-object-store-init command script")
	}
	return service.Command[0]
}

func assertLiteralContainerExpansion(t *testing.T, script string, variables ...string) {
	t.Helper()
	for _, variable := range variables {
		literal := "$${" + variable + "}"
		if !strings.Contains(script, literal) {
			t.Fatalf("MinIO init omits literal container expansion for %s", variable)
		}
		withoutLiteral := strings.ReplaceAll(script, literal, "")
		if strings.Contains(withoutLiteral, "${"+variable+"}") {
			t.Fatalf("MinIO init lets Compose expand %s into the shell command argv", variable)
		}
	}
}

func credentialPipeline(t *testing.T, script, commandLine string) string {
	t.Helper()
	lines := strings.Split(script, "\n")
	for index, line := range lines {
		if strings.TrimSpace(line) != commandLine {
			continue
		}
		if index == 0 || !strings.HasPrefix(strings.TrimSpace(lines[index-1]), "printf '%s\\n%s\\n'") {
			t.Fatalf("%s is not fed by the required two-line stdin pipeline", commandLine)
		}
		return strings.TrimSpace(lines[index-1]) + "\n" + commandLine
	}
	t.Fatalf("MinIO init omits command %s", commandLine)
	return ""
}

func runCredentialPipeline(t *testing.T, pipeline string, environment map[string]string,
	wantArguments []string, wantInput string,
) {
	t.Helper()
	// Compose converts $$ to a single literal $ before passing the command to
	// /bin/sh. Apply that documented interpolation step before this shell test.
	pipeline = strings.ReplaceAll(pipeline, "$${", "${")
	directory := t.TempDir()
	argvPath := filepath.Join(directory, "argv")
	stdinPath := filepath.Join(directory, "stdin")
	fakeMC := filepath.Join(directory, "mc")
	if err := os.WriteFile(fakeMC, []byte(`#!/bin/sh
set -eu
printf '%s\n' "$@" > "$MC_TEST_ARGV"
dd of="$MC_TEST_STDIN" status=none
`), 0o700); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("/bin/sh", "-ec", pipeline)
	command.Env = []string{
		"PATH=" + directory + ":/usr/bin:/bin",
		"MC_TEST_ARGV=" + argvPath,
		"MC_TEST_STDIN=" + stdinPath,
	}
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("credential stdin pipeline failed: %v (output length %d)", err, len(output))
	}

	argvBytes, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Fields(string(argvBytes))
	if strings.Join(arguments, "\x00") != strings.Join(wantArguments, "\x00") {
		t.Fatal("mc received unexpected arguments; credential values must not be positional")
	}
	stdinBytes, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdinBytes) != wantInput {
		t.Fatal("mc did not receive exactly the expected two credential lines on stdin")
	}
}

func lineContaining(script, marker string) string {
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	return ""
}
