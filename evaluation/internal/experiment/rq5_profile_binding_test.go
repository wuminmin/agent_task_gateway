package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRQ5DynamicCatalogFamilyProfileBindingAndForgedGenerator(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	generatorPath := filepath.Join(root, "evaluation", "daily-publication", "sql", "05-generate-daily-data.sh")
	configPath := filepath.Join(root, "evaluation", "daily-publication", "config.json")
	generatorSHA, err := FileSHA256(generatorPath)
	if err != nil {
		t.Fatal(err)
	}
	configSHA, err := FileSHA256(configPath)
	if err != nil {
		t.Fatal(err)
	}
	listing := generatorSHA + "  evaluation/daily-publication/sql/05-generate-daily-data.sh\n" +
		configSHA + "  evaluation/daily-publication/config.json\n"
	sourceDigest := sha256.Sum256([]byte(listing))
	manifest := map[string]any{"schema_version": 1,
		"submission_commit": strings.Repeat("a", 40), "binary_sha256": strings.Repeat("b", 64),
		"source_sha256": hex.EncodeToString(sourceDigest[:]), "go_version": "go test",
		"build_command": "go build test", "source_files": listing}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "rq5-build.json")
	if err := os.WriteFile(manifestPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestSHA, err := FileSHA256(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	request := RQ5ProfileBindingRequest{
		RegistryPath: filepath.Join(root, "config", "profiles", "registry.json"), ProfileAlias: "expense-detail",
		DatasetBindingSHA256: strings.Repeat("c", 64),
		CatalogFamilyPath:    filepath.Join(root, "config", "profiles", "rq5-daily-catalog-family-v1.json"),
		BuildManifestPath:    manifestPath, BuildManifestSHA256: manifestSHA,
		SubmissionCommit: strings.Repeat("a", 40), GeneratorSHA256: generatorSHA, ConfigSHA256: configSHA,
	}
	resolved, err := ResolveRQ5ProfileBinding(request)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Binding == nil || resolved.Binding.CatalogSHA256 != resolved.Family.FamilySHA256 ||
		resolved.Binding.ProfileID != "profile-0d88c4e9d8b7561b" || resolved.Binding.PublicationIdentity == "" {
		t.Fatalf("RQ5 binding = %+v, family = %+v", resolved.Binding, resolved.Family)
	}
	binder, err := NewSampleProfileBinder(resolved.Binding)
	if err != nil {
		t.Fatal(err)
	}
	binder.rq5 = &finalv5RQ5CatalogExpectation{FamilySHA256: resolved.Family.FamilySHA256,
		BuildManifestSHA256: resolved.Family.BuildManifestSHA256,
		GeneratorSHA256:     resolved.Family.GeneratorSHA256, ConfigSHA256: resolved.Family.ConfigSHA256}
	sample := validRQ5SampleForTest(1, "build_verify_activate")
	sample.RQ5Verification.BuildManifestSHA256 = resolved.Family.BuildManifestSHA256
	sample.RQ5Verification.GeneratorSHA256 = resolved.Family.GeneratorSHA256
	sample.RQ5Verification.ConfigSHA256 = resolved.Family.ConfigSHA256
	if err := binder.BindSample(&sample); err != nil || sample.ProfileBinding == nil ||
		!sample.ProfileBinding.Equal(*resolved.Binding) {
		t.Fatalf("RQ5 sample binding = %+v, err = %v", sample.ProfileBinding, err)
	}
	request.GeneratorSHA256 = strings.Repeat("d", 64)
	if _, err := ResolveRQ5ProfileBinding(request); err == nil {
		t.Fatal("a forged generator identity was accepted instead of the sealed manifest entry")
	}
}
