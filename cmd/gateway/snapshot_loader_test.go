package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

type recordingSnapshotPublicationStore struct {
	calls       int
	publication string
	dictionary  string
	artifacts   int
	cutovers    int
	cutoverHash string
	cutoverV4   bool
	cutoverErr  error
	err         error
}

func (s *recordingSnapshotPublicationStore) EnforceExposureDeploymentMode(_ context.Context, digest string, v4 bool) error {
	s.cutovers++
	s.cutoverHash = digest
	s.cutoverV4 = v4
	return s.cutoverErr
}

func (s *recordingSnapshotPublicationStore) PutOrdinalSnapshotPublication(_ context.Context, publication string,
	index ordinal.SnapshotIndex, artifacts []control.OrdinalArtifactChunk) error {
	s.calls++
	s.publication = publication
	s.dictionary = index.DictionaryDigest()
	s.artifacts = len(artifacts)
	return s.err
}

func TestLoadSnapshotArtifactDirectoryRegistersVerifiedHotIndex(t *testing.T) {
	base, logicalCatalog, bundle := writeLoaderFixture(t)
	store := &recordingSnapshotPublicationStore{}
	registry, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog, store)
	if err != nil {
		t.Fatalf("loadSnapshotArtifactDirectory: %v", err)
	}
	publication := logicalCatalog.SnapshotPublications[0]
	resolved, err := registry.Resolve(ordinal.PublicationKey{CatalogDigest: logicalCatalog.SHA256, PublicationName: publication.Name})
	if err != nil || resolved.DictionaryDigest() != publication.DictionaryDigest {
		t.Fatalf("Resolve = %#v, %v", resolved, err)
	}
	if store.cutovers != 1 || store.cutoverHash != logicalCatalog.SHA256 || !store.cutoverV4 ||
		store.calls != 1 || store.publication != publication.ManifestDigest ||
		store.dictionary != bundle.Manifest.DictionaryManifest.DictionaryDigest || store.artifacts != 0 {
		t.Fatalf("Control registration = %#v", store)
	}
}

func TestLoadSnapshotArtifactDirectoryFailsClosed(t *testing.T) {
	t.Run("catalog digest mismatch", func(t *testing.T) {
		base, logicalCatalog, _ := writeLoaderFixture(t)
		logicalCatalog.SnapshotPublications[0].SidecarDigest = strings.Repeat("e", 64)
		store := &recordingSnapshotPublicationStore{}
		if _, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog, store); err == nil {
			t.Fatal("Catalog sidecar mismatch was accepted")
		}
		if store.calls != 0 {
			t.Fatal("mismatched artifact reached Control registration")
		}
	})

	t.Run("tampered HOT", func(t *testing.T) {
		base, logicalCatalog, bundle := writeLoaderFixture(t)
		path := filepath.Join(base, bundle.Manifest.PublicationName, bundle.Manifest.Hot.Name)
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		payload[len(payload)/2] ^= 0xff
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog,
			&recordingSnapshotPublicationStore{}); err == nil {
			t.Fatal("tampered HOT artifact was accepted")
		}
	})

	t.Run("tampered COLD with matching mutable descriptor", func(t *testing.T) {
		base, logicalCatalog, bundle := writeLoaderFixture(t)
		path := filepath.Join(base, bundle.Manifest.PublicationName, bundle.Manifest.Cold.Name)
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		payload[len(payload)/2] ^= 0xff
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		rewriteLoaderBundleDescriptor(t, base, &bundle, "cold", payload)
		store := &recordingSnapshotPublicationStore{}
		if _, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog, store); err == nil {
			t.Fatal("tampered COLD artifact with matching bundle descriptor was accepted")
		}
		if store.cutovers != 0 || store.calls != 0 {
			t.Fatalf("tampered COLD reached activation: %#v", store)
		}
	})

	t.Run("tampered sidecar with matching mutable descriptor", func(t *testing.T) {
		base, logicalCatalog, bundle := writeLoaderFixture(t)
		path := filepath.Join(base, bundle.Manifest.PublicationName, bundle.Manifest.Sidecar.Name)
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		tampered := bytes.Replace(payload, []byte("R-001"), []byte("R-009"), 1)
		if bytes.Equal(tampered, payload) {
			t.Fatal("fixture sidecar did not contain the expected key")
		}
		if err := os.WriteFile(path, tampered, 0o600); err != nil {
			t.Fatal(err)
		}
		rewriteLoaderBundleDescriptor(t, base, &bundle, "sidecar", tampered)
		store := &recordingSnapshotPublicationStore{}
		if _, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog, store); err == nil {
			t.Fatal("tampered sidecar with matching bundle descriptor was accepted")
		}
		if store.cutovers != 0 || store.calls != 0 {
			t.Fatalf("tampered sidecar reached activation: %#v", store)
		}
	})

	t.Run("undeclared directory", func(t *testing.T) {
		base, logicalCatalog, _ := writeLoaderFixture(t)
		if err := os.Mkdir(filepath.Join(base, "unknown-v1"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog,
			&recordingSnapshotPublicationStore{}); err == nil {
			t.Fatal("undeclared publication directory was accepted")
		}
	})

	t.Run("Control failure", func(t *testing.T) {
		base, logicalCatalog, _ := writeLoaderFixture(t)
		if registry, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog,
			&recordingSnapshotPublicationStore{err: errors.New("control unavailable")}); err == nil || registry != nil {
			t.Fatalf("Control failure returned registry=%#v err=%v", registry, err)
		}
	})

	t.Run("nonempty legacy ledger", func(t *testing.T) {
		base, logicalCatalog, _ := writeLoaderFixture(t)
		store := &recordingSnapshotPublicationStore{cutoverErr: errors.New("legacy ledger is not empty")}
		if registry, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog, store); err == nil || registry != nil {
			t.Fatalf("dirty cutover returned registry=%#v err=%v", registry, err)
		}
		if store.calls != 0 || store.cutovers != 1 {
			t.Fatalf("dirty cutover registration = %#v", store)
		}
	})
}

func rewriteLoaderBundleDescriptor(t *testing.T, base string, bundle *snapshotbundle.CompiledBundle,
	kind string, payload []byte) {
	t.Helper()
	digest := sha256.Sum256(payload)
	descriptor := snapshotbundle.FileDescriptor{SHA256: fmt.Sprintf("%x", digest[:]), Bytes: int64(len(payload))}
	switch kind {
	case "cold":
		descriptor.Name = bundle.Manifest.Cold.Name
		bundle.Manifest.Cold = descriptor
	case "sidecar":
		descriptor.Name = bundle.Manifest.Sidecar.Name
		bundle.Manifest.Sidecar = descriptor
	default:
		t.Fatalf("unknown descriptor kind %q", kind)
	}
	encoded, err := bundle.ManifestJSON()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, bundle.Manifest.PublicationName, bundle.Manifest.PublicationName+".bundle.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRegistryFromEnvIsOptional(t *testing.T) {
	t.Setenv("GATEWAY_SNAPSHOT_ARTIFACT_DIR", "")
	registry, err := snapshotRegistryFromEnv(context.Background(), &catalog.Catalog{}, &recordingSnapshotPublicationStore{})
	if err != nil || registry != nil {
		t.Fatalf("unconfigured registry = %#v, %v", registry, err)
	}
}

// loaderFixtureSchemaDigest is the build-time schema attestation frozen into
// the fixture Publication. After C15 it is deliberately unrelated to the
// profile reporting-surface attestation a Catalog pins in Source.SchemaDigest.
const loaderFixtureSchemaDigest = "1111111111111111111111111111111111111111111111111111111111111111"

func loaderCompilerInput(schemaDigest string) snapshotbundle.CompilerInput {
	return snapshotbundle.CompilerInput{
		Version: snapshotbundle.CompilerInputVersion, PublicationName: "expense-detail-v1", CatalogSource: "travel_demo",
		OrdinalSidecar: "taskgate_ordinal.expense_detail_v1", EntityKeyFields: []string{"receipt_no"},
		Snapshot: snapshotbundle.SnapshotInput{SourceID: "travel-demo", SourceNamespace: "travel.expense_receipt",
			Snapshot: "travel-demo-2026-v1", SchemaDigest: schemaDigest,
			Fields: []snapshotbundle.SnapshotField{
				{Name: "receipt_no", SQLType: "text", Collation: "C", CollationVersion: "builtin"},
				{Name: "amount", SQLType: "numeric"},
			},
			Rows: []snapshotbundle.SnapshotRow{
				{Values: map[string]any{"receipt_no": "R-001", "amount": json.Number("10.00")}},
				{Values: map[string]any{"receipt_no": "R-002", "amount": json.Number("20.00")}},
			}},
	}
}

func writeLoaderFixture(t *testing.T) (string, *catalog.Catalog, snapshotbundle.CompiledBundle) {
	t.Helper()
	const schemaDigest = loaderFixtureSchemaDigest
	input := loaderCompilerInput(schemaDigest)
	bundle, err := snapshotbundle.Compile(input)
	if err != nil {
		t.Fatalf("Compile fixture: %v", err)
	}
	base := t.TempDir()
	if _, err := bundle.Write(base); err != nil {
		t.Fatalf("Write fixture: %v", err)
	}
	manifest := bundle.Manifest.DictionaryManifest
	logicalCatalog := &catalog.Catalog{SHA256: strings.Repeat("a", 64),
		Sources: []catalog.Source{{Name: input.CatalogSource, DatasourceID: input.Snapshot.SourceID, SchemaDigest: schemaDigest}},
		SnapshotPublications: []catalog.SnapshotPublication{{Name: input.PublicationName, Source: input.CatalogSource,
			SourceNamespace: input.Snapshot.SourceNamespace, Snapshot: input.Snapshot.Snapshot,
			OrdinalSidecar: input.OrdinalSidecar, SidecarDigest: manifest.SidecarDigest,
			DictionaryDigest: manifest.DictionaryDigest, ManifestDigest: bundle.Manifest.ManifestDigest}},
	}
	return base, logicalCatalog, bundle
}
