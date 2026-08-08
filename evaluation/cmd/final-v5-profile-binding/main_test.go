package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

const (
	cliTestClosure = "a86cd4df5cad6e2685d1930caeccdbe3a865194fcef33faeca86b7c2c592bdbd"
	cliTestCatalog = "533837084c0df141a0fac6e74788a4c2b9eb84611c1f96d4c760806745b4f709"
	cliTestAlias   = "result-heavy"
)

func writeCLIRegistry(t *testing.T) string {
	t.Helper()
	status := finalv5profile.ProfileStatus{ClosureComplete: true, CatalogMaterializable: true,
		LiveRouteAvailable: true, ActivationSupported: true, ActivationSmokePassed: true}
	registry := finalv5profile.Registry{SchemaVersion: 1, RegistryVersion: finalv5profile.RegistryVersion,
		Profiles: []finalv5profile.Profile{{
			ID: finalv5profile.ProfileID(cliTestClosure), Alias: cliTestAlias,
			Closure: finalv5profile.Closure{SHA256: cliTestClosure,
				Publications: []string{"final-v5-result-heavy-v1"}},
			Status: status, TargetedRunEligible: status.TargetedRunEligible(), CatalogSHA256: cliTestCatalog,
		}}}
	payload, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cliArgs(registry, dataset, out string) []string {
	return []string{"--registry", registry, "--alias", cliTestAlias,
		"--dataset-binding-sha256", dataset, "--out", out}
}

func TestRunWritesTheSharedBindingCreateExclusive(t *testing.T) {
	registry := writeCLIRegistry(t)
	dataset := strings.Repeat("c", 64)
	out := filepath.Join(t.TempDir(), "profile-binding.json")
	if err := run(cliArgs(registry, dataset, out), io.Discard); err != nil {
		t.Fatalf("run CLI: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("output mode = %04o, want 0600", info.Mode().Perm())
	}
	payload, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := experiment.ResolveProfileBinding(registry, cliTestAlias, dataset)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.MarshalIndent(shared, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if !bytes.Equal(payload, want) {
		t.Fatalf("output = %s, want %s", payload, want)
	}
	var decoded experiment.ProfileBinding
	if err := experiment.StrictJSON(payload, &decoded); err != nil {
		t.Fatalf("output is not strict ProfileBinding JSON: %v", err)
	}
	if !decoded.Equal(*shared) {
		t.Fatalf("decoded output %+v differs from shared resolver %+v", decoded, *shared)
	}

	original := append([]byte(nil), payload...)
	if err := run(cliArgs(registry, strings.Repeat("d", 64), out), io.Discard); err == nil {
		t.Fatal("CLI overwrote an existing binding")
	}
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("failed create-exclusive write changed the existing binding")
	}
}

func TestRunRefusesSymlinkOutput(t *testing.T) {
	registry := writeCLIRegistry(t)
	directory := t.TempDir()
	target := filepath.Join(directory, "target.json")
	if err := os.WriteFile(target, []byte("unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "profile-binding.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := run(cliArgs(registry, strings.Repeat("c", 64), link), io.Discard); err == nil {
		t.Fatal("CLI followed an existing output symlink")
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "unchanged\n" {
		t.Fatal("refused symlink output changed its target")
	}
}

func TestRunRejectsBadArgumentsWithoutCreatingOutput(t *testing.T) {
	registry := writeCLIRegistry(t)
	dataset := strings.Repeat("c", 64)
	for name, buildArgs := range map[string]func(string) []string{
		"missing registry": func(out string) []string {
			return []string{"--alias", cliTestAlias, "--dataset-binding-sha256", dataset, "--out", out}
		},
		"missing alias": func(out string) []string {
			return []string{"--registry", registry, "--dataset-binding-sha256", dataset, "--out", out}
		},
		"missing dataset digest": func(out string) []string {
			return []string{"--registry", registry, "--alias", cliTestAlias, "--out", out}
		},
		"missing output": func(string) []string {
			return []string{"--registry", registry, "--alias", cliTestAlias, "--dataset-binding-sha256", dataset}
		},
		"malformed dataset digest": func(out string) []string {
			return cliArgs(registry, "short", out)
		},
		"positional argument": func(out string) []string {
			return append(cliArgs(registry, dataset, out), "extra")
		},
		"unknown flag": func(out string) []string {
			return append(cliArgs(registry, dataset, out), "--unexpected")
		},
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "profile-binding.json")
			if err := run(buildArgs(out), io.Discard); err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if _, err := os.Lstat(out); !os.IsNotExist(err) {
				t.Fatalf("%s left output behind: %v", name, err)
			}
		})
	}
}

func TestRunRejectsNonStrictRegistryWithoutCreatingOutput(t *testing.T) {
	registry := writeCLIRegistry(t)
	payload, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload[:len(payload)-1], []byte(`,"unexpected":true}`)...)
	if err := os.WriteFile(registry, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "profile-binding.json")
	if err := run(cliArgs(registry, strings.Repeat("c", 64), out), io.Discard); err == nil {
		t.Fatal("CLI accepted a registry with an unknown field")
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatalf("failed resolution left output behind: %v", err)
	}
}
