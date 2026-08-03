package main

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

// Separating the two attestation domains removed the only implicit tie between
// a Product and the Publication behind it, so the loader now checks that
// binding explicitly. These cases pin what it does and does not claim to check.
//
// See docs/final_v5_c15_attestation_separation.md.

func compatibilityFixture() (*catalog.Catalog, catalog.SnapshotPublication) {
	publication := catalog.SnapshotPublication{Name: "expense-detail-v1", Source: "travel_demo",
		SourceNamespace: "travel.expense_receipt", Snapshot: "travel-demo-2026-v1",
		OrdinalSidecar: "taskgate_ordinal.expense_detail_v1", SidecarDigest: strings.Repeat("a", 64),
		DictionaryDigest: strings.Repeat("b", 64), ManifestDigest: strings.Repeat("c", 64)}
	logicalCatalog := &catalog.Catalog{SHA256: strings.Repeat("d", 64),
		Sources:              []catalog.Source{{Name: "travel_demo", DatasourceID: "travel-demo"}},
		SnapshotPublications: []catalog.SnapshotPublication{publication},
		Products: []catalog.Product{{Name: "expense_detail", Source: "travel_demo",
			ReportingView: "reporting.expense_detail", Snapshot: "travel-demo-2026-v1",
			SnapshotPublication: "expense-detail-v1", FactNamespace: "travel.expense_receipt",
			EntityKey: []string{"receipt_no"},
			Fields:    []catalog.Field{{Name: "receipt_no", Type: "text"}, {Name: "amount", Type: "numeric"}}}},
	}
	return logicalCatalog, publication
}

func TestProductPublicationCompatibilityAcceptsAConsistentBinding(t *testing.T) {
	logicalCatalog, publication := compatibilityFixture()
	if err := verifyProductPublicationCompatibility(logicalCatalog, publication); err != nil {
		t.Fatalf("consistent Product/Publication binding was rejected: %v", err)
	}
}

func TestProductPublicationCompatibilityFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*catalog.Product){
		"source differs": func(product *catalog.Product) { product.Source = "other_demo" },
		"snapshot differs": func(product *catalog.Product) {
			product.Snapshot = "travel-demo-2027-v1"
		},
		"fact namespace differs": func(product *catalog.Product) {
			product.FactNamespace = "travel.other_receipt"
		},
		"entity key missing": func(product *catalog.Product) { product.EntityKey = nil },
		"entity key is not a published field": func(product *catalog.Product) {
			product.EntityKey = []string{"internal_row_id"}
		},
		"entity key partially unpublished": func(product *catalog.Product) {
			product.EntityKey = []string{"receipt_no", "line_no"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			logicalCatalog, publication := compatibilityFixture()
			mutate(&logicalCatalog.Products[0])
			if err := verifyProductPublicationCompatibility(logicalCatalog, publication); err == nil {
				t.Fatalf("incompatible Product/Publication binding %q was accepted", name)
			}
		})
	}
}

// A Product that reads a different Publication is out of scope for this check;
// it is verified when its own Publication is loaded.
func TestProductPublicationCompatibilityIgnoresOtherPublications(t *testing.T) {
	logicalCatalog, publication := compatibilityFixture()
	logicalCatalog.Products = append(logicalCatalog.Products, catalog.Product{Name: "orders_detail",
		Source: "sales_demo", ReportingView: "reporting.orders_detail", Snapshot: "sales-2026-v1",
		SnapshotPublication: "orders-detail-v1", FactNamespace: "sales.orders", EntityKey: []string{"order_id"},
		Fields: []catalog.Field{{Name: "order_id", Type: "bigint"}}})
	if err := verifyProductPublicationCompatibility(logicalCatalog, publication); err != nil {
		t.Fatalf("an unrelated Product's binding was charged to %q: %v", publication.Name, err)
	}
}

// A Catalog may declare a Publication before the Product that will read it.
// Closure completeness is a property of profile generation, not of the loader,
// and asserting it here would reject legitimate fixtures.
func TestProductPublicationCompatibilityAllowsAnUnreadPublication(t *testing.T) {
	logicalCatalog, publication := compatibilityFixture()
	logicalCatalog.Products = nil
	if err := verifyProductPublicationCompatibility(logicalCatalog, publication); err != nil {
		t.Fatalf("a Publication with no reading Product was rejected: %v", err)
	}
}

// Field-level type and collation compatibility is deliberately not re-derived
// from the dictionary manifest, which carries no semantic field set. It is
// enforced by the Profile Reporting-Surface Attestation against the live
// database. This test records that boundary so a later change cannot quietly
// start guessing at it.
func TestProductPublicationCompatibilityDoesNotGuessFieldTypes(t *testing.T) {
	logicalCatalog, publication := compatibilityFixture()
	logicalCatalog.Products[0].Fields[1].Type = "double precision"
	if err := verifyProductPublicationCompatibility(logicalCatalog, publication); err != nil {
		t.Fatalf("the loader inferred a field type it cannot see in the manifest: %v", err)
	}
}
