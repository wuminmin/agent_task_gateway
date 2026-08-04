// Package catalogschema builds the Connector's ExpectedSchema from a validated
// Catalog.
//
// It exists so that exactly one construction is used by both the Gateway at
// startup and the evaluation when it derives an observer accounting plan. The
// multiplicity of the per-entry attestations -- the column attestation, the
// view-definition attestation and the nested lookup PostgreSQL performs inside
// pg_get_viewdef -- is a function of the number of ExpectedSchema entries, so a
// second, approximate reimplementation on the evaluation side would silently
// produce a different expected count.
//
// In particular the entry count is NOT the number of distinct reporting views.
// Build appends one entry per governed Product, so two Products naming the same
// reporting view yield two entries and two attestation passes. Counting distinct
// views, as an earlier evaluation-side derivation did, undercounts exactly those
// Catalogs.
package catalogschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

// expectedSchemaListDomain domain-separates the ordered entry-list digest.
const expectedSchemaListDomain = "TASKGATE-FINAL-V5-EXPECTED-SCHEMA-LIST-V1"

// Result is the complete ExpectedSchema derivation for one Catalog.
type Result struct {
	// Source is the single Catalog source every governed Product resolves to.
	Source catalog.Source
	// Entries is the ordered ExpectedSchema the Connector holds. Order is
	// Catalog Product order and is significant: the attestations run in it.
	Entries []dataconnector.ViewSchema
	// Count is len(Entries), the E of the observer accounting plan.
	Count int64
	// Digest is the canonical digest of the ordered entry list.
	Digest string
}

// Build derives the ExpectedSchema exactly as the Gateway does.
//
// Products carrying a ViewContract are excluded: Phase-B semantic Views are
// attested by a task-scoped transitive registry binding inside each query
// transaction, and keeping them out of the source-wide digest stops an
// unrelated View replacement from disabling every task and readiness probe.
func Build(logicalCatalog *catalog.Catalog) (Result, error) {
	if logicalCatalog == nil || len(logicalCatalog.Products) == 0 {
		return Result{}, errors.New("validated catalog contains no products")
	}
	sources := make(map[string]catalog.Source, len(logicalCatalog.Sources))
	for _, source := range logicalCatalog.Sources {
		sources[source.Name] = source
	}
	var selected catalog.Source
	entries := make([]dataconnector.ViewSchema, 0, len(logicalCatalog.Products))
	for _, product := range logicalCatalog.Products {
		source, ok := sources[product.Source]
		if !ok {
			return Result{}, errors.New("validated catalog contains a product with an invalid source")
		}
		if selected.Name == "" {
			selected = source
		} else if selected.Name != source.Name {
			return Result{}, errors.New("multiple catalog sources require connector routing")
		}
		if product.ViewContract != nil {
			continue
		}
		schema, view, ok := strings.Cut(product.ReportingView, ".")
		if !ok || schema == "" || view == "" {
			return Result{}, errors.New("validated catalog contains an invalid reporting view")
		}
		columns := make([]dataconnector.SchemaColumn, 0, len(product.Fields))
		for _, field := range product.Fields {
			columns = append(columns, dataconnector.SchemaColumn{Name: field.Name, PostgreSQLType: field.Type,
				Collation: field.Collation, CollationVersion: field.CollationVersion,
				CollationDeterministic: field.Collation != ""})
		}
		// One entry per governed Product, deliberately without deduplication:
		// the Connector attests each entry, so duplicates cost real statements.
		entries = append(entries, dataconnector.ViewSchema{Schema: schema, View: view, Columns: columns})
	}
	if selected.Name == "" {
		return Result{}, errors.New("validated catalog contains no source")
	}
	if len(entries) == 0 {
		return Result{}, errors.New("semantic View catalogs require at least one governed terminal product")
	}
	if selected.SchemaDigest == "" {
		return Result{}, errors.New("selected catalog source is missing schema_digest")
	}
	return Result{Source: selected, Entries: entries, Count: int64(len(entries)), Digest: Digest(entries)}, nil
}

// Digest is the canonical digest of an ordered ExpectedSchema entry list.
//
// It is exported so that anything qualifying a measurement against an
// ExpectedSchema -- notably the Attestation footprint probe, which assembles its
// entries from live relations rather than from a Catalog -- produces an identity
// in the same space as the one Build gives the Gateway. A second digest
// implementation would make a qualified footprint impossible to bind to the
// production schema it is supposed to be valid for.
//
// It is length-delimited at every level so that no regrouping of schema, view or
// column names can produce the same bytes.
func Digest(entries []dataconnector.ViewSchema) string {
	hash := sha256.New()
	hash.Write([]byte(expectedSchemaListDomain + "\x00"))
	fmt.Fprintf(hash, "%d\x00", len(entries))
	for _, entry := range entries {
		writeDelimited(hash, entry.Schema)
		writeDelimited(hash, entry.View)
		fmt.Fprintf(hash, "%d\x00", len(entry.Columns))
		for _, column := range entry.Columns {
			writeDelimited(hash, column.Name)
			writeDelimited(hash, column.PostgreSQLType)
			writeDelimited(hash, column.Collation)
			writeDelimited(hash, column.CollationVersion)
			if column.CollationDeterministic {
				writeDelimited(hash, "deterministic")
			} else {
				writeDelimited(hash, "nondeterministic")
			}
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeDelimited(hash interface{ Write([]byte) (int, error) }, value string) {
	fmt.Fprintf(hash, "%d\x00%s\x00", len(value), value)
}
