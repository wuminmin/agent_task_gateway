package gateway

import (
	"fmt"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// This is the Gateway's half of the preparation split, and the only half it
// keeps.
//
// Preparation itself is shared code: the Gateway calls it before executing and
// the finalizer calls it afterwards from independently obtained frozen material,
// and the two reproduce one another only because neither supplies anything the
// other cannot obtain for itself. What cannot be shared is getting the material
// in the first place -- reading the loaded Catalog, resolving a Publication
// through a registry this process has verified, holding a live SnapshotIndex.
// That is what this file does, and where it stops.
//
// It lived in the differential harness until now, which was correct while
// Prepare was reachable only from tests: production had its own derivation and
// the harness compared the two. Promoting it is what makes the comparison the
// harness performs a comparison against the inputs production actually builds,
// rather than against a second construction of them that could drift.

// preparationInputs maps verified external state into the immutable inputs
// Prepare reads.
//
// Nothing Gateway-specific crosses. A SnapshotIndex is a live handle onto a
// compiled artifact and stays here; what crosses is the descriptor of what that
// handle resolved to, which the finalizer can obtain from retained Publication
// evidence without holding the artifact at all.
func (s *Service) preparationInputs(grant control.TaskGrant,
	plan queryplan.QueryPlan) (physicalquery.PreparationInputs, error) {
	if s.catalog == nil {
		return physicalquery.PreparationInputs{}, fmt.Errorf("this service has no loaded Catalog to prepare against")
	}
	// The one supported constructor, not a hand-assembled view: every member has
	// to come from the same loaded Catalog, and building it field by field is
	// exactly the pairing CatalogViewFromCatalog exists to prevent.
	view, err := physicalquery.CatalogViewFromCatalog(*s.catalog)
	if err != nil {
		return physicalquery.PreparationInputs{}, fmt.Errorf("build catalog view: %w", err)
	}
	inputs := physicalquery.PreparationInputs{
		Plan: plan,
		Grant: physicalquery.Grant{
			ApprovedProducts: grant.ApprovedProducts,
			ApprovedColumns:  grant.ApprovedColumns,
			MandatoryScope:   grant.MandatoryScope,
			ExposureProfile:  grant.Exposure.ProfileVersion,
			PredicateLimits:  predicateLimitsForGrant(grant.Exposure),
		},
		Catalog: view,
	}
	if inputs.Grant.UsesOrdinalProgram() {
		bindings, bindingsErr := s.snapshotBindings()
		if bindingsErr != nil {
			return physicalquery.PreparationInputs{}, bindingsErr
		}
		inputs.SnapshotBindings = bindings
	}
	return inputs, nil
}

// snapshotBindings reads the verified registry into descriptors.
//
// The whole Catalog-wide universe is resolved, not merely the Publications this
// plan touches. That is the dictionary-universe rule the CatalogView records:
// an ordinal identity is only stable if every Publication the Catalog declares
// resolved to one manifest, so a preparation that bound a subset would be
// identified by a universe that depended on which query happened to run.
func (s *Service) snapshotBindings() (map[string]physicalquery.SnapshotBinding, error) {
	if s.snapshotRegistry == nil {
		return nil, fmt.Errorf("this exposure profile compiles an ordinal program but no snapshot registry is loaded")
	}
	bindings := make(map[string]physicalquery.SnapshotBinding, len(s.catalog.SnapshotPublications))
	for _, publication := range s.catalog.SnapshotPublications {
		index, err := s.snapshotRegistry.Resolve(ordinal.PublicationKey{
			CatalogDigest: s.catalog.SHA256, PublicationName: publication.Name,
		})
		if err != nil {
			return nil, fmt.Errorf("resolve snapshot publication %s: %w", publication.Name, err)
		}
		manifest := index.Manifest()
		bindings[publication.Name] = physicalquery.SnapshotBinding{
			PublicationName:  publication.Name,
			DictionaryDigest: index.DictionaryDigest(),
			ManifestDigest:   index.ManifestDigest(),
			SidecarDigest:    manifest.SidecarDigest,
			SourceNamespace:  manifest.SourceNamespace,
			Snapshot:         manifest.Snapshot,
			OrdinalSidecar:   publication.OrdinalSidecar,
			RowCount:         index.RowCount(),
		}
	}
	return bindings, nil
}
