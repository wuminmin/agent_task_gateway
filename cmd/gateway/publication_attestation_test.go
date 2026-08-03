package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

// C15 separated the two schema attestations: the loader no longer requires the
// Publication bundle's build-time attestation to equal the active profile's
// reporting-surface attestation. These cases prove that the Publication-build
// Schema Attestation is nevertheless still fully bound, and that the checks the
// loader kept are the ones that make tampering fail closed.
//
// See docs/final_v5_c15_attestation_separation.md.

const loaderVariantSchemaDigest = "2222222222222222222222222222222222222222222222222222222222222222"

// compileLoaderVariant compiles the same snapshot under a different build-time
// schema attestation. Every artifact and the Publication identity change with
// it, which is exactly what makes the attestation binding observable.
func compileLoaderVariant(t *testing.T, schemaDigest string) snapshotbundle.CompiledBundle {
	t.Helper()
	bundle, err := snapshotbundle.Compile(loaderCompilerInput(schemaDigest))
	if err != nil {
		t.Fatalf("Compile variant: %v", err)
	}
	return bundle
}

// writeRawBundleManifest replaces the published bundle manifest without the
// compiler's own validation, so deliberately inconsistent manifests reach the
// loader instead of being rejected by the test helper.
func writeRawBundleManifest(t *testing.T, base string, manifest snapshotbundle.BundleManifest) {
	t.Helper()
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, manifest.PublicationName, manifest.PublicationName+".bundle.json")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// publicationDirectoryDigest is a stable digest of the immutable publication
// directory: every file name bound to its content hash.
func publicationDirectoryDigest(t *testing.T, base, publication string) string {
	t.Helper()
	directory := filepath.Join(base, publication)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		payload, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%s %x", entry.Name(), sha256.Sum256(payload)))
	}
	sort.Strings(lines)
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(digest[:])
}

func TestPublicationBundleAttestationFailsClosed(t *testing.T) {
	// 1. The bundle's embedded schema attestation is edited while the Catalog
	//    keeps the original Publication identity. The build attestation is
	//    folded into DictionaryManifest.Digest, so the bundle no longer
	//    recomputes to its own manifest_digest.
	t.Run("bundle schema attestation edited, Catalog ManifestDigest unchanged", func(t *testing.T) {
		base, logicalCatalog, bundle := writeLoaderFixture(t)
		tampered := bundle.Manifest
		tampered.DictionaryManifest.SchemaDigest = loaderVariantSchemaDigest
		writeRawBundleManifest(t, base, tampered)
		store := &recordingSnapshotPublicationStore{}
		_, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog, store)
		if err == nil {
			t.Fatal("edited bundle schema attestation was accepted")
		}
		t.Logf("refused: %v", err)
		if store.cutovers != 0 || store.calls != 0 {
			t.Fatalf("edited bundle attestation reached activation: %#v", store)
		}
	})

	// 2. The forger repairs the bundle so it is internally consistent with the
	//    new attestation. The Catalog still pins the original ManifestDigest, so
	//    the edit surfaces as the Publication identity violation it is. This is
	//    the case that proves removing the Source.SchemaDigest comparison did not
	//    unbind the build attestation.
	t.Run("bundle re-digested to a new attestation, Catalog pins the old identity", func(t *testing.T) {
		base, logicalCatalog, bundle := writeLoaderFixture(t)
		repaired := bundle.Manifest
		repaired.DictionaryManifest.SchemaDigest = loaderVariantSchemaDigest
		recomputed, err := repaired.DictionaryManifest.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if recomputed == bundle.Manifest.ManifestDigest {
			t.Fatal("the build attestation is not folded into the Publication identity")
		}
		repaired.ManifestDigest = recomputed
		writeRawBundleManifest(t, base, repaired)
		store := &recordingSnapshotPublicationStore{}
		_, err = loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog, store)
		if err == nil {
			t.Fatal("re-digested bundle was accepted against the Catalog-pinned identity")
		}
		t.Logf("refused: %v", err)
		if store.cutovers != 0 || store.calls != 0 {
			t.Fatalf("re-digested bundle reached activation: %#v", store)
		}
	})

	// 3. The HOT artifact carries its own copy of the dictionary manifest. It is
	//    swapped for one compiled under a different attestation, and the bundle
	//    descriptor is repaired so the swap survives the file-digest check.
	t.Run("HOT dictionary manifest attestation replaced", func(t *testing.T) {
		base, logicalCatalog, bundle := writeLoaderFixture(t)
		variant := compileLoaderVariant(t, loaderVariantSchemaDigest)
		if variant.Manifest.DictionaryManifest.SchemaDigest == bundle.Manifest.DictionaryManifest.SchemaDigest {
			t.Fatal("variant fixture reused the original attestation")
		}
		hotPath := filepath.Join(base, bundle.Manifest.PublicationName, bundle.Manifest.Hot.Name)
		if err := os.WriteFile(hotPath, variant.Hot, 0o600); err != nil {
			t.Fatal(err)
		}
		repaired := bundle.Manifest
		repaired.Hot = variant.Manifest.Hot
		writeRawBundleManifest(t, base, repaired)
		store := &recordingSnapshotPublicationStore{}
		_, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog, store)
		if err == nil {
			t.Fatal("HOT artifact with a foreign build attestation was accepted")
		}
		t.Logf("refused: %v", err)
		if store.cutovers != 0 || store.calls != 0 {
			t.Fatalf("foreign HOT attestation reached activation: %#v", store)
		}
	})

	// 4. Bundle manifest and HOT manifest disagree, and the Catalog has been
	//    updated to follow the bundle, so every self-consistency check inside
	//    the bundle passes. In practice this is refused one layer earlier than
	//    the bundle/HOT DeepEqual: a HOT artifact is sealed against its own
	//    manifest digest, so it cannot be parsed under a foreign identity at
	//    all. The DeepEqual in matchHotIndexToCatalog is defence in depth behind
	//    that seal, and both are exercised here.
	t.Run("bundle and HOT dictionary manifests disagree", func(t *testing.T) {
		base, logicalCatalog, bundle := writeLoaderFixture(t)
		variant := compileLoaderVariant(t, loaderVariantSchemaDigest)
		forged := bundle.Manifest
		forged.DictionaryManifest = variant.Manifest.DictionaryManifest
		forged.ManifestDigest = variant.Manifest.ManifestDigest
		writeRawBundleManifest(t, base, forged)
		publication := &logicalCatalog.SnapshotPublications[0]
		publication.ManifestDigest = variant.Manifest.ManifestDigest
		publication.DictionaryDigest = variant.Manifest.DictionaryManifest.DictionaryDigest
		publication.SidecarDigest = variant.Manifest.DictionaryManifest.SidecarDigest
		store := &recordingSnapshotPublicationStore{}
		_, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog, store)
		if err == nil {
			t.Fatal("disagreeing bundle and HOT manifests were accepted")
		}
		t.Logf("refused: %v", err)
		if store.cutovers != 0 || store.calls != 0 {
			t.Fatalf("manifest disagreement reached activation: %#v", store)
		}
	})

	// 5. Datasource identity is still enforced in both the bundle and the HOT
	//    manifest; only the schema attestation comparison was removed.
	t.Run("bundle SourceID differs from the Catalog datasource", func(t *testing.T) {
		base, logicalCatalog, _ := writeLoaderFixture(t)
		logicalCatalog.Sources[0].DatasourceID = "some-other-datasource"
		store := &recordingSnapshotPublicationStore{}
		_, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog, store)
		if err == nil {
			t.Fatal("bundle bound to a foreign datasource was accepted")
		}
		t.Logf("refused: %v", err)
		if store.cutovers != 0 || store.calls != 0 {
			t.Fatalf("foreign datasource reached activation: %#v", store)
		}
	})

	// 6. Every remaining Catalog-to-bundle binding still fails closed.
	for name, mutate := range map[string]func(*catalog.SnapshotPublication){
		"source namespace": func(p *catalog.SnapshotPublication) { p.SourceNamespace = "travel.other_namespace" },
		"snapshot":         func(p *catalog.SnapshotPublication) { p.Snapshot = "travel-demo-2027-v1" },
		"sidecar digest":   func(p *catalog.SnapshotPublication) { p.SidecarDigest = strings.Repeat("c", 64) },
		"dictionary digest": func(p *catalog.SnapshotPublication) {
			p.DictionaryDigest = strings.Repeat("d", 64)
		},
		"manifest digest": func(p *catalog.SnapshotPublication) { p.ManifestDigest = strings.Repeat("f", 64) },
		"ordinal sidecar": func(p *catalog.SnapshotPublication) { p.OrdinalSidecar = "taskgate_ordinal.other_v1" },
		"catalog source":  func(p *catalog.SnapshotPublication) { p.Source = "other_demo" },
	} {
		t.Run("Catalog binding differs: "+name, func(t *testing.T) {
			base, logicalCatalog, _ := writeLoaderFixture(t)
			mutate(&logicalCatalog.SnapshotPublications[0])
			store := &recordingSnapshotPublicationStore{}
			_, err := loadSnapshotArtifactDirectory(context.Background(), base, logicalCatalog, store)
			if err == nil {
				t.Fatalf("Catalog/bundle %s mismatch was accepted", name)
			}
			t.Logf("refused: %v", err)
			if store.cutovers != 0 || store.calls != 0 {
				t.Fatalf("%s mismatch reached activation: %#v", name, store)
			}
		})
	}
}

// 7. The positive case C15 exists for: one immutable Publication is loaded by
// several different Profile Catalogs. The bundle bytes, the bundle's build-time
// attestation and the Publication identity are byte-identical across all of
// them, while each profile carries its own Catalog digest, its own Product
// closure and its own reporting-surface attestation. Before the separation this
// was impossible, because at most one profile's surface digest could equal the
// value frozen into the shared bundle.
func TestOnePublicationBundleLoadsUnderEveryExpenseFamilyProfile(t *testing.T) {
	base, template, bundle := writeLoaderFixture(t)
	publication := template.SnapshotPublications[0]
	directoryDigest := publicationDirectoryDigest(t, base, publication.Name)

	// The five expense-family profiles, each with its own Catalog digest, its
	// own reporting-surface attestation and its own Product projection.
	profiles := []struct {
		alias        string
		catalogSHA   string
		surface      string
		productName  string
		productField string
	}{
		{"expense-detail", strings.Repeat("a", 64), strings.Repeat("a1", 32), "expense_detail", "amount"},
		{"attack-expense-detail", strings.Repeat("b", 64), strings.Repeat("b2", 32), "expense_detail", "amount"},
		{"concurrency-expense-detail", strings.Repeat("c", 64), strings.Repeat("c3", 32), "expense_detail", "amount"},
		{"rls-unlimited", strings.Repeat("d", 64), strings.Repeat("d4", 32), "expense_detail_rls", "amount"},
		{"rls-bounded", strings.Repeat("e", 64), strings.Repeat("e5", 32), "expense_detail_rls", "amount"},
	}

	seenCatalogSHA := map[string]bool{}
	seenSurface := map[string]bool{}
	for _, profile := range profiles {
		t.Run(profile.alias, func(t *testing.T) {
			profileCatalog := &catalog.Catalog{
				SHA256: profile.catalogSHA,
				Sources: []catalog.Source{{Name: publication.Source, DatasourceID: template.Sources[0].DatasourceID,
					SchemaDigest: profile.surface}},
				SnapshotPublications: []catalog.SnapshotPublication{publication},
				Products: []catalog.Product{{Name: profile.productName, Source: publication.Source,
					ReportingView: "reporting." + profile.productName, Snapshot: publication.Snapshot,
					SnapshotPublication: publication.Name, FactNamespace: publication.SourceNamespace,
					EntityKey: []string{"receipt_no"},
					Fields: []catalog.Field{{Name: "receipt_no", Type: "text"},
						{Name: profile.productField, Type: "numeric"}}}},
			}
			store := &recordingSnapshotPublicationStore{}
			registry, err := loadSnapshotArtifactDirectory(context.Background(), base, profileCatalog, store)
			if err != nil {
				t.Fatalf("profile %s could not load the shared Publication: %v", profile.alias, err)
			}
			if registry == nil || store.calls != 1 || store.publication != publication.ManifestDigest {
				t.Fatalf("profile %s activation = %#v", profile.alias, store)
			}
			// The Publication is identical for every profile: same bytes on
			// disk, same build-time attestation, same identity.
			if got := publicationDirectoryDigest(t, base, publication.Name); got != directoryDigest {
				t.Fatalf("profile %s changed the Publication directory: %s", profile.alias, got)
			}
			if bundle.Manifest.ManifestDigest != publication.ManifestDigest {
				t.Fatalf("profile %s observed a different Publication identity", profile.alias)
			}
			// The profile's own attestation domain is distinct and unrelated.
			if profileCatalog.Sources[0].SchemaDigest == bundle.Manifest.DictionaryManifest.SchemaDigest {
				t.Fatalf("profile %s surface attestation equalled the build attestation", profile.alias)
			}
			seenCatalogSHA[profileCatalog.SHA256] = true
			seenSurface[profileCatalog.Sources[0].SchemaDigest] = true
		})
	}
	if len(seenCatalogSHA) != len(profiles) || len(seenSurface) != len(profiles) {
		t.Fatalf("profiles did not carry distinct Catalog digests and surface attestations: %d/%d",
			len(seenCatalogSHA), len(seenSurface))
	}
}
