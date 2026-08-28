package experiment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The builder writes module_proxy (d87fafe) and the finalizer decodes the
// manifest strictly; the two must agree on every field or a complete
// publication campaign is refused at its last step.
func TestPublicationFormalBuildManifestMirrorsTheBuilderFields(t *testing.T) {
	manifest := `{
  "schema_version": 1,
  "submission_commit": "649eadb42ae6c3084942eb6f5101ac8c536b754c",
  "clean_tree_at_build": true,
  "build_context_sha256": "` + strings.Repeat("1", 64) + `",
  "source_manifest_sha256": "` + strings.Repeat("2", 64) + `",
  "image_id": "sha256:` + strings.Repeat("3", 64) + `",
  "image_tag": "taskgate-final-v5-gateway:649eadb42ae6c3084942eb6f5101ac8c536b754c",
  "platform": "linux/amd64",
  "build_target": "gateway",
  "builder_base_image": "golang@sha256:` + strings.Repeat("5", 64) + `",
  "runtime_base_image": "debian@sha256:` + strings.Repeat("6", 64) + `",
  "dataset_binding_sha256": "` + strings.Repeat("7", 64) + `",
  "profile_registry_sha256": "` + strings.Repeat("8", 64) + `",
  "module_proxy": "http://172.25.95.157:3000,https://proxy.golang.org,direct"
}
`
	path := filepath.Join(t.TempDir(), "formal-gateway-build.json")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	var decoded publicationFormalBuildManifest
	if err := readStrictFile(path, &decoded); err != nil {
		t.Fatalf("a manifest carrying module_proxy must decode strictly: %v", err)
	}
	if decoded.ModuleProxy == "" || decoded.BuildTarget != "gateway" || !decoded.CleanTreeAtBuild {
		t.Fatalf("decoded manifest lost fields: %+v", decoded)
	}
	// An unknown field must still be refused: the mirror is meant to be exact.
	unknown := strings.Replace(manifest, `"module_proxy"`, `"module_proxy_v2"`, 1)
	if err := os.WriteFile(path, []byte(unknown), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readStrictFile(path, &decoded); err == nil {
		t.Fatal("an unmirrored builder field was accepted")
	}
}
