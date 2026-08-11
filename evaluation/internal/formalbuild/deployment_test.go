package formalbuild

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func manifestContext() Context {
	return Context{
		Source: SourceState{
			Commit: testCommit, Branch: "main", CleanTree: true, Published: true,
		},
		ContextSHA256:        testContextDigest,
		SourceManifestSHA256: testManifest,
	}
}

func manifestBindings() BuildBindings {
	return BuildBindings{
		DatasetBindingSHA256:  strings.Repeat("a", 64),
		ProfileRegistrySHA256: strings.Repeat("b", 64),
	}
}

func writeManifestFixture(t *testing.T, mutate func(*BuildManifest, *[]byte)) (string, string) {
	t.Helper()
	manifest, err := NewBuildManifest(manifestContext(), newFakeEngine().image,
		"taskgate-final-v5-gateway:"+testCommit, manifestBindings())
	if err != nil {
		t.Fatalf("NewBuildManifest: %v", err)
	}
	override, err := manifest.ComposeOverride()
	if err != nil {
		t.Fatalf("ComposeOverride: %v", err)
	}
	if mutate != nil {
		mutate(&manifest, &override)
	}
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "build-manifest.json")
	overridePath := filepath.Join(directory, "compose.formal-gateway.yaml")
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overridePath, override, 0o600); err != nil {
		t.Fatal(err)
	}
	return manifestPath, overridePath
}

func TestBuildDeploymentManifestUsesExactImageIDAndVerifies(t *testing.T) {
	engine := newFakeEngine()
	manifest, err := NewBuildManifest(manifestContext(), engine.image,
		"taskgate-final-v5-gateway:"+testCommit, manifestBindings())
	if err != nil {
		t.Fatalf("NewBuildManifest: %v", err)
	}
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "build-manifest.json")
	overridePath := filepath.Join(directory, "compose.formal-gateway.yaml")
	if err := WriteBuildDeployment(manifestPath, overridePath, manifest); err != nil {
		t.Fatalf("WriteBuildDeployment: %v", err)
	}
	for _, path := range []string{manifestPath, overridePath} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%s, want regular 0600", path, info.Mode())
		}
	}
	override, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(override), `image: "`+testImageID+`"`) ||
		strings.Contains(string(override), manifest.ImageTag) {
		t.Fatalf("Compose override does not select only the immutable image ID:\n%s", override)
	}
	verified, err := VerifyBuildDeployment(context.Background(), engine, manifestPath, overridePath,
		expectedSource(), manifestBindings())
	if err != nil {
		t.Fatalf("VerifyBuildDeployment: %v", err)
	}
	if verified.ImageID != engine.image.ID || verified.DatasetBindingSHA256 != manifestBindings().DatasetBindingSHA256 ||
		verified.ProfileRegistrySHA256 != manifestBindings().ProfileRegistrySHA256 {
		t.Fatalf("verified manifest lost a provenance join: %+v", verified)
	}
}

func TestBuildDeploymentVerificationFailsClosed(t *testing.T) {
	for name, fixture := range map[string]func(*testing.T) (string, string, *fakeEngine, BuildBindings){
		"missing manifest": func(t *testing.T) (string, string, *fakeEngine, BuildBindings) {
			manifest, override := writeManifestFixture(t, nil)
			return manifest + ".missing", override, newFakeEngine(), manifestBindings()
		},
		"manifest source mismatch": func(t *testing.T) (string, string, *fakeEngine, BuildBindings) {
			manifest, override := writeManifestFixture(t, func(value *BuildManifest, _ *[]byte) {
				value.SourceManifestSHA256 = strings.Repeat("c", 64)
			})
			return manifest, override, newFakeEngine(), manifestBindings()
		},
		"Dataset binding mismatch": func(t *testing.T) (string, string, *fakeEngine, BuildBindings) {
			manifest, override := writeManifestFixture(t, nil)
			bindings := manifestBindings()
			bindings.DatasetBindingSHA256 = strings.Repeat("d", 64)
			return manifest, override, newFakeEngine(), bindings
		},
		"Compose override mismatch": func(t *testing.T) (string, string, *fakeEngine, BuildBindings) {
			manifest, override := writeManifestFixture(t, func(_ *BuildManifest, payload *[]byte) {
				*payload = []byte("services:\n  gateway:\n    image: ordinary:latest\n")
			})
			return manifest, override, newFakeEngine(), manifestBindings()
		},
		"manifest image is unavailable": func(t *testing.T) (string, string, *fakeEngine, BuildBindings) {
			manifest, override := writeManifestFixture(t, func(value *BuildManifest, payload *[]byte) {
				value.ImageID = testOtherID
				updated, err := value.ComposeOverride()
				if err != nil {
					t.Fatal(err)
				}
				*payload = updated
			})
			return manifest, override, newFakeEngine(), manifestBindings()
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest, override, engine, bindings := fixture(t)
			if _, err := VerifyBuildDeployment(context.Background(), engine, manifest, override,
				expectedSource(), bindings); err == nil {
				t.Fatal("invalid formal Gateway deployment wiring was accepted")
			}
		})
	}
}

func TestBuildDeploymentOutputsAreCreateExclusive(t *testing.T) {
	manifest, err := NewBuildManifest(manifestContext(), newFakeEngine().image,
		"taskgate-final-v5-gateway:"+testCommit, manifestBindings())
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "build-manifest.json")
	overridePath := filepath.Join(directory, "compose.formal-gateway.yaml")
	const sentinel = "existing output must survive\n"
	if err := os.WriteFile(overridePath, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteBuildDeployment(manifestPath, overridePath, manifest); err == nil {
		t.Fatal("an existing Compose override was overwritten")
	}
	if _, err := os.Lstat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("partial build manifest was retained: %v", err)
	}
	got, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinel {
		t.Fatalf("existing Compose override changed to %q", got)
	}
}

func TestLoadBuildManifestRejectsUnknownFieldsAndUnsafeMode(t *testing.T) {
	manifestPath, _ := writeManifestFixture(t, nil)
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	payload = []byte(strings.Replace(string(payload), "\n}", ",\n  \"unreviewed\": true\n}", 1))
	if err := os.WriteFile(manifestPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBuildManifest(manifestPath); err == nil {
		t.Fatal("unknown build-manifest member was accepted")
	}
	if err := os.Chmod(manifestPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBuildManifest(manifestPath); err == nil {
		t.Fatal("unsafe build-manifest mode was accepted")
	}
}

func TestLoadBuildManifestRejectsDuplicateIdentityMembers(t *testing.T) {
	for name, member := range map[string]string{
		"image ID":        "image_id",
		"Dataset binding": "dataset_binding_sha256",
	} {
		t.Run(name, func(t *testing.T) {
			manifestPath, _ := writeManifestFixture(t, nil)
			payload, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			needle := `"` + member + `": "`
			start := strings.Index(string(payload), needle)
			if start < 0 {
				t.Fatalf("fixture omits %s", member)
			}
			end := strings.Index(string(payload[start:]), "\n")
			if end < 0 {
				t.Fatalf("fixture %s member has no line ending", member)
			}
			line := payload[start : start+end+1]
			payload = append(append(append([]byte(nil), payload[:start]...), line...), payload[start:]...)
			if err := os.WriteFile(manifestPath, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadBuildManifest(manifestPath); err == nil || !strings.Contains(err.Error(), "duplicate JSON object member") {
				t.Fatalf("duplicate %s error = %v", member, err)
			}
		})
	}
}
