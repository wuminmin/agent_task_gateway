package gateway

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/ordinal"
)

// What this file tests changed at T1d, and it is worth saying why.
//
// It used to drive the Gateway's own bindOrdinalSidecars: the generated sidecar
// join, the merged least-privilege grant, the Catalog-wide dictionary universe
// and the working-set estimate. None of that is derived here any more --
// internal/physicalquery produces all of it from values the finalizer can obtain
// for itself, and the parity harness checks it against the Catalog and the
// policy engine rather than against a second implementation.
//
// What is left on this side is the one thing preparation cannot do: turning the
// alias-to-Publication mapping preparation recorded back into live SnapshotIndex
// handles from this process's verified registry. That is what these cases cover,
// including the two ways it must fail closed.

// sharedSidecarFixture is one compiled snapshot published under one Catalog
// entry, with a Service whose registry has it registered.
type sharedSidecarFixture struct {
	service     *Service
	publication catalog.SnapshotPublication
	artifact    ordinal.SnapshotIndex
}

func newSharedSidecarFixture(t *testing.T, catalogDigest string) sharedSidecarFixture {
	t.Helper()
	artifact, err := ordinal.CompileSnapshotArtifact(ordinal.SnapshotSpec{
		SourceID: "source", SourceNamespace: "semantic.shared", Snapshot: "snapshot-v1",
		SchemaDigest: strings.Repeat("1", 64),
		Fields: []ordinal.SnapshotField{
			{Name: "order_id", CanonicalFieldID: "orders.order_id", SQLType: "text"},
			{Name: "line_id", CanonicalFieldID: "lines.line_id", SQLType: "text"},
		},
		Rows: []ordinal.SnapshotRow{{EntityKey: "shared-1", Values: map[string]any{
			"order_id": "O-1", "line_id": "L-1",
		}}},
	})
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact: %v", err)
	}
	publication := catalog.SnapshotPublication{
		Name: "shared-v1", Source: "source", SourceNamespace: "semantic.shared", Snapshot: "snapshot-v1",
		OrdinalSidecar: "taskgate_ordinal.shared_v1", SidecarDigest: artifact.Hot.Manifest().SidecarDigest,
		DictionaryDigest: artifact.Hot.DictionaryDigest(), ManifestDigest: artifact.Hot.ManifestDigest(),
	}
	registry, err := ordinal.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.RegisterPublication(ordinal.PublicationKey{
		CatalogDigest: catalogDigest, PublicationName: publication.Name,
	}, publication.ManifestDigest, artifact.Hot); err != nil {
		t.Fatalf("RegisterPublication: %v", err)
	}
	return sharedSidecarFixture{
		service: &Service{
			catalog: &catalog.Catalog{SHA256: catalogDigest,
				SnapshotPublications: []catalog.SnapshotPublication{publication}},
			snapshotRegistry: registry,
		},
		publication: publication,
		artifact:    artifact.Hot,
	}
}

// Two source aliases bound to one Publication must reattach to one artifact.
//
// This is the Gateway half of the duplicate-binding rule the parity harness
// states about preparation. If the two aliases resolved to different handles --
// or if one of them silently resolved to none -- the ordinal deriver would look
// a row handle up in the wrong dictionary, and the exact cross-product union the
// root ledger depends on would stop holding.
func TestResolveOrdinalIndexesReattachesOneArtifactToEverySharedAlias(t *testing.T) {
	fixture := newSharedSidecarFixture(t, strings.Repeat("b", 64))
	indexes, err := fixture.service.resolveOrdinalIndexes(map[string]string{
		"orders": fixture.publication.Name, "lines": fixture.publication.Name,
	})
	if err != nil {
		t.Fatalf("resolveOrdinalIndexes: %v", err)
	}
	if len(indexes) != 2 || indexes["orders"] != fixture.artifact || indexes["lines"] != fixture.artifact {
		t.Fatalf("shared-publication indexes = %#v, want both aliases on one artifact", indexes)
	}
}

// A Publication preparation named but the Catalog does not declare is a refusal.
//
// Preparation is sealed against a Catalog; if this process's Catalog no longer
// carries the Publication, the handles it would reattach are not the ones the
// statements were prepared against.
func TestResolveOrdinalIndexesRefusesAnUndeclaredPublication(t *testing.T) {
	fixture := newSharedSidecarFixture(t, strings.Repeat("c", 64))
	if _, err := fixture.service.resolveOrdinalIndexes(map[string]string{
		"orders": "not-in-this-catalog",
	}); err == nil {
		t.Fatal("a publication this Catalog does not declare was resolved")
	}
}

// A registry whose artifact disagrees with the Catalog manifest is a refusal.
//
// The registry is live state and the Catalog is the evidence the preparation was
// sealed against. Accepting a handle that no longer matches would let a query
// prepared against one immutable artifact execute against another.
func TestResolveOrdinalIndexesRefusesACatalogRegistryMismatch(t *testing.T) {
	registry, err := ordinal.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	service := &Service{catalog: &catalog.Catalog{SHA256: strings.Repeat("a", 64),
		SnapshotPublications: []catalog.SnapshotPublication{{
			Name: "expense-v1", Source: "source", SourceNamespace: "semantic.expense",
			Snapshot: "snapshot-v1", OrdinalSidecar: "taskgate_ordinal.expense_v1",
			ManifestDigest: strings.Repeat("1", 64), DictionaryDigest: strings.Repeat("2", 64),
			SidecarDigest: strings.Repeat("3", 64),
		}}}, snapshotRegistry: registry}
	if _, err := service.resolveOrdinalIndexes(map[string]string{"expense": "expense-v1"}); err == nil {
		t.Fatal("a Catalog-declared publication with no registered index was accepted")
	}
}

// A Service with no registry cannot reattach anything, and must say so.
func TestResolveOrdinalIndexesRefusesWithoutARegistry(t *testing.T) {
	service := &Service{catalog: &catalog.Catalog{SHA256: strings.Repeat("a", 64)}}
	if _, err := service.resolveOrdinalIndexes(map[string]string{"expense": "expense-v1"}); err == nil {
		t.Fatal("an ordinal program was bound with no snapshot registry loaded")
	}
}
