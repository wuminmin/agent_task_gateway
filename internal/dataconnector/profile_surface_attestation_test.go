package dataconnector

import (
	"testing"
)

// The Profile Reporting-Surface Attestation is the digest a Catalog pins in
// Source.SchemaDigest. After C15 it is the *only* thing that value means: it
// attests that the reporting views the active profile's Products declare match
// the live Business PostgreSQL schema. It is no longer compared with any
// Publication bundle. These cases prove the separation did not weaken it.
//
// See docs/final_v5_c15_attestation_separation.md.

// profileSurface returns the reporting surface of a two-Product profile.
func profileSurface() []ViewSchema {
	return []ViewSchema{
		{Schema: "reporting", View: "expense_detail", Definition: "SELECT receipt_no, amount FROM travel.expense_receipt",
			Columns: []SchemaColumn{
				{Name: "receipt_no", PostgreSQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true},
				{Name: "amount", PostgreSQLType: "numeric", CollationDeterministic: true},
			}},
		{Schema: "reporting", View: "expense_summary", Definition: "SELECT month, total_amount FROM travel.expense_receipt",
			Columns: []SchemaColumn{
				{Name: "month", PostgreSQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true},
				{Name: "total_amount", PostgreSQLType: "numeric", CollationDeterministic: true},
			}},
	}
}

func surfaceDigest(t *testing.T, views []ViewSchema) string {
	t.Helper()
	digest, err := SchemaDigest(views)
	if err != nil {
		t.Fatalf("SchemaDigest: %v", err)
	}
	return digest
}

func TestProfileSurfaceAttestationFailsClosedOnSurfaceDrift(t *testing.T) {
	pinned := surfaceDigest(t, profileSurface())

	// The pre-C15 deployment attested every reporting view in the full Catalog.
	// A profile that declares a strict subset must not accept that older value:
	// it would mean the profile never proved its own surface.
	fullCatalogSurface := append(profileSurface(), ViewSchema{Schema: "reporting", View: "orders_detail",
		Definition: "SELECT order_id FROM sales.orders",
		Columns:    []SchemaColumn{{Name: "order_id", PostgreSQLType: "bigint", CollationDeterministic: true}}})
	if got := surfaceDigest(t, fullCatalogSurface); got == pinned {
		t.Fatal("the old full-Catalog attestation equalled the profile surface attestation")
	}

	for name, mutate := range map[string]func([]ViewSchema) []ViewSchema{
		"column added": func(views []ViewSchema) []ViewSchema {
			views[0].Columns = append(views[0].Columns,
				SchemaColumn{Name: "currency", PostgreSQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true})
			return views
		},
		"column removed": func(views []ViewSchema) []ViewSchema {
			views[0].Columns = views[0].Columns[:1]
			return views
		},
		"field type changed": func(views []ViewSchema) []ViewSchema {
			views[0].Columns[1].PostgreSQLType = "double precision"
			return views
		},
		"view definition changed": func(views []ViewSchema) []ViewSchema {
			views[0].Definition = "SELECT receipt_no, amount FROM travel.expense_receipt WHERE amount > 0"
			return views
		},
		"reporting view removed from the profile": func(views []ViewSchema) []ViewSchema {
			return views[:1]
		},
		"reporting view replaced in the profile": func(views []ViewSchema) []ViewSchema {
			views[1].View = "expense_summary_v2"
			return views
		},
	} {
		t.Run(name, func(t *testing.T) {
			drifted := surfaceDigest(t, mutate(profileSurface()))
			if drifted == pinned {
				t.Fatalf("surface drift %q produced the pinned attestation", name)
			}
			// A drifted surface must be refused at the point the connector
			// compares it, with the schema-drift code the Gateway maps to
			// DATA_CONNECTOR_SCHEMA_DRIFT, not merely produce a different digest.
			connector := Connector{expectedSchemaDigest: pinned}
			if err := connector.compareAttestation(Attestation{SchemaDigest: drifted}); !IsCode(err, CodeSchemaDrift) {
				t.Fatalf("compareAttestation(%q) = %v, want %s", name, err, CodeSchemaDrift)
			}
		})
	}

	// The unmodified surface must still be accepted, otherwise the cases above
	// would pass for the wrong reason.
	connector := Connector{expectedSchemaDigest: pinned}
	if err := connector.compareAttestation(Attestation{SchemaDigest: surfaceDigest(t, profileSurface())}); err != nil {
		t.Fatalf("the pinned profile surface was rejected: %v", err)
	}
}

// Collation, collation version and collation determinism are not folded into
// SchemaDigest. They are attested one layer earlier: attestSchemaDigest
// compares every live column against the Catalog-declared expectation with
// sameSchemaColumns and refuses to produce a digest at all when they differ.
// That is a stronger check than a digest comparison, because it names the
// drifted column, and it means an unchanged digest is never evidence that
// collation is unchecked. The layering predates C15 and the separation does not
// alter it; this test pins it so the surface attestation cannot be read as
// collation-blind.
func TestProfileSurfaceAttestationRejectsCollationDriftBeforeDigesting(t *testing.T) {
	expected := profileSurface()[0].Columns
	for name, mutate := range map[string]func(*SchemaColumn){
		"collation changed":            func(column *SchemaColumn) { column.Collation = "en_US.utf8" },
		"collation version changed":    func(column *SchemaColumn) { column.CollationVersion = "2.41" },
		"collation determinism lost":   func(column *SchemaColumn) { column.CollationDeterministic = false },
		"column renamed":               func(column *SchemaColumn) { column.Name = "receipt_number" },
		"column type changed":          func(column *SchemaColumn) { column.PostgreSQLType = "varchar" },
		"collation dropped altogether": func(column *SchemaColumn) { column.Collation = "" },
	} {
		t.Run(name, func(t *testing.T) {
			actual := append([]SchemaColumn(nil), expected...)
			mutate(&actual[0])
			if sameSchemaColumns(expected, actual) {
				t.Fatalf("live surface drift %q was accepted against the Catalog expectation", name)
			}
		})
	}
	if !sameSchemaColumns(expected, append([]SchemaColumn(nil), expected...)) {
		t.Fatal("the declared reporting surface was rejected against itself")
	}
}

// Two profiles that declare different reporting surfaces must attest to
// different digests. This is the property that makes comparing a shared
// Publication bundle's build-time attestation with Source.SchemaDigest wrong,
// and it is why C15 separated them.
func TestDistinctProfileSurfacesAttestToDistinctDigests(t *testing.T) {
	full := surfaceDigest(t, profileSurface())
	detailOnly := surfaceDigest(t, profileSurface()[:1])
	if full == detailOnly {
		t.Fatal("two different profile reporting surfaces share one attestation")
	}
	// Both are legitimate attestations of the same underlying database, so a
	// single embedded build-time constant cannot equal both.
	if full == "" || detailOnly == "" {
		t.Fatal("profile surface attestation was empty")
	}
}
