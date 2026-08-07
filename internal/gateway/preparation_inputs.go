package gateway

import (
	"fmt"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
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
// It lived in the differential harness while Prepare was reachable only from
// tests, and was promoted so the harness compared against the inputs production
// actually builds rather than a second construction of them. Since T1d the
// Gateway has no derivation of its own left to compare against at all: these
// mappings are the whole of what it contributes to preparing a statement.

// preparationGrant maps the stored task authorization onto the values
// preparation reads.
//
// It is one function rather than a literal at each call site because the grant's
// digest is sealed into every prepared binding: two mappings that differed in a
// member would produce two identities for one authorization, and the check that
// the approval has not moved since preparation would then fail for a reason
// nobody changed.
func preparationGrant(grant control.TaskGrant) physicalquery.Grant {
	return physicalquery.Grant{
		ApprovedProducts: grant.ApprovedProducts,
		ApprovedColumns:  grant.ApprovedColumns,
		MandatoryScope:   grant.MandatoryScope,
		ExposureProfile:  grant.Exposure.ProfileVersion,
		PredicateLimits:  predicateLimitsForGrant(grant.Exposure),
	}
}

// viewPreparationGrant is preparationGrant pinned to one compiled semantic View.
func viewPreparationGrant(grant control.TaskGrant, bindingDigest, revisionDigest string) physicalquery.Grant {
	mapped := preparationGrant(grant)
	mapped.ViewBindingDigest = bindingDigest
	mapped.ViewRegistryRevision = revisionDigest
	return mapped
}

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
		Plan:    plan,
		Grant:   preparationGrant(grant),
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

// semanticViewPreparationInputs maps the resolved View material onto the pure
// input contract.
//
// The binding and its expected registry revision arrive already verified: the
// Control Store proved the task is bound to this View and the registry
// signature was checked before this is reached. What crosses is the verified
// result as values, never the store or the registry -- the finalizer loads the
// same values from retained frozen evidence.
//
// The grant is pinned to the View as well as carrying the task's own surface.
// SemanticViewPreparationInputsV1.Validate refuses a grant bound to a different
// View or revision, so stating it here is what turns "these happen to be the
// same" into a checked claim.
func (s *Service) semanticViewPreparationInputs(grant control.TaskGrant, artifact viewcompiler.Artifact,
	composition viewcompiler.Composition, binding *resolvedViewBinding,
	outer queryplan.QueryPlan) (physicalquery.SemanticViewPreparationInputsV1, error) {
	if s.catalog == nil {
		return physicalquery.SemanticViewPreparationInputsV1{},
			fmt.Errorf("this service has no loaded Catalog to prepare against")
	}
	if binding == nil {
		return physicalquery.SemanticViewPreparationInputsV1{},
			fmt.Errorf("this semantic View operation carries no resolved binding")
	}
	view, err := physicalquery.CatalogViewFromCatalog(*s.catalog)
	if err != nil {
		return physicalquery.SemanticViewPreparationInputsV1{}, fmt.Errorf("build catalog view: %w", err)
	}
	inputs := physicalquery.SemanticViewPreparationInputsV1{
		OuterPlan:              outer,
		Catalog:                view,
		Grant:                  viewPreparationGrant(grant, binding.Digest, binding.Expectation.ExpectedRevisionDigest),
		ViewBindingDigest:      binding.Digest,
		ExpectedRevisionDigest: binding.Expectation.ExpectedRevisionDigest,
		Artifact:               artifact,
		Composition:            composition,
	}
	if inputs.Grant.UsesOrdinalProgram() {
		bindings, bindingsErr := s.snapshotBindings()
		if bindingsErr != nil {
			return physicalquery.SemanticViewPreparationInputsV1{}, bindingsErr
		}
		inputs.SnapshotBindings = bindings
	}
	return inputs, nil
}

// resolveOrdinalIndexes reattaches this process's verified snapshot handles to
// the source aliases the preparation bound.
//
// The mapping is preparation's output, not this function's choice: a source
// alias resolves to whatever Publication the compiled program pinned, and the
// handle is looked up under that name. Each is re-checked against the Catalog
// on the way out, because the registry is live state and the Catalog is the
// evidence the preparation was sealed against -- a handle that no longer agrees
// with it is a conflict, not a policy question.
func (s *Service) resolveOrdinalIndexes(
	publications map[string]string) (map[string]ordinal.SnapshotIndex, error) {
	if s.snapshotRegistry == nil {
		return nil, fmt.Errorf("this operation compiled an ordinal program but no snapshot registry is loaded")
	}
	indexes := make(map[string]ordinal.SnapshotIndex, len(publications))
	for alias, name := range publications {
		publication, found := s.catalog.LookupSnapshotPublication(name)
		if !found {
			return nil, fmt.Errorf("preparation bound source %q to publication %q, "+
				"which this Catalog does not declare", alias, name)
		}
		index, err := s.snapshotRegistry.Resolve(ordinal.PublicationKey{
			CatalogDigest: s.catalog.SHA256, PublicationName: name,
		})
		if err != nil || index.DictionaryDigest() != publication.DictionaryDigest ||
			index.ManifestDigest() != publication.ManifestDigest ||
			index.Manifest().SidecarDigest != publication.SidecarDigest ||
			index.Manifest().SourceNamespace != publication.SourceNamespace ||
			index.Manifest().Snapshot != publication.Snapshot {
			return nil, fmt.Errorf("snapshot publication %q does not match the Catalog manifest", name)
		}
		indexes[alias] = index
	}
	return indexes, nil
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
