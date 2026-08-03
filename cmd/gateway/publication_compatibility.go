package main

import (
	"fmt"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

// verifyProductPublicationCompatibility checks that every Product actually
// belongs to the Publication it reads through.
//
// Before C15 the loader required the bundle's build-time schema attestation to
// equal the active profile's reporting-surface attestation. That equality was
// wrong -- a shared Publication cannot match every profile's surface -- but it
// was the only thing incidentally tying a Product to the bundle behind it.
// Separating the attestation domains therefore has to make that link explicit.
//
// Scope note. ordinal.DictionaryManifest carries no semantic field set and no
// entity-key field list: it holds versions, source identity, snapshot, the four
// artifact digests and segment manifests. Field-level name, type, collation and
// collation-version compatibility is therefore deliberately not re-derived here.
// It is already enforced, and enforced more strongly, by the Profile
// Reporting-Surface Attestation: dataconnector.attestSchemaDigest compares every
// Product's declared fields against the live view's columns, types, collations
// and collation versions, and fails closed with DATA_CONNECTOR_SCHEMA_DRIFT.
// That check runs against the database the queries actually read, not against a
// build-time constant, so it is the equivalent check the C15 decision requires
// to be recorded rather than duplicated by guesswork.
//
// What is verified here is the binding the surface attestation cannot see: that
// a Product is attached to a Publication describing the same immutable snapshot
// and the same semantic namespace.
func verifyProductPublicationCompatibility(logicalCatalog *catalog.Catalog,
	publication catalog.SnapshotPublication) error {
	for _, product := range logicalCatalog.Products {
		if product.SnapshotPublication != publication.Name {
			continue
		}
		if product.Source != publication.Source {
			return fmt.Errorf("product %q reads source %q but publication %q is bound to %q",
				product.Name, product.Source, publication.Name, publication.Source)
		}
		if product.Snapshot != publication.Snapshot {
			return fmt.Errorf("product %q snapshot %q differs from publication %q snapshot %q",
				product.Name, product.Snapshot, publication.Name, publication.Snapshot)
		}
		if product.FactNamespace != "" && publication.SourceNamespace != "" &&
			product.FactNamespace != publication.SourceNamespace {
			return fmt.Errorf("product %q fact namespace %q differs from publication %q namespace %q",
				product.Name, product.FactNamespace, publication.Name, publication.SourceNamespace)
		}
		if len(product.EntityKey) == 0 {
			return fmt.Errorf("product %q binds publication %q with no entity key",
				product.Name, publication.Name)
		}
		// An entity key must be drawn from the Product's own published fields,
		// otherwise the stable row identity the Publication indexes cannot be
		// reconstructed from what the Product exposes.
		published := make(map[string]bool, len(product.Fields))
		for _, field := range product.Fields {
			published[field.Name] = true
		}
		for _, key := range product.EntityKey {
			if !published[key] {
				return fmt.Errorf("product %q entity key field %q is not one of its published fields",
					product.Name, key)
			}
		}
	}
	// Whether every activated Publication is reachable from some Product is a
	// profile-closure property, enforced when the profile Catalog is generated;
	// a Catalog may legitimately declare a Publication ahead of its Product.
	return nil
}
