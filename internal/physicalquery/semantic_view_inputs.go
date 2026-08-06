package physicalquery

import (
	"errors"
	"fmt"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

// The semantic View input domain separators.
const (
	semanticViewInputsDomain      = "TASKGATE-SEMANTIC-VIEW-PREPARATION-INPUTS-V1"
	semanticViewArtifactDomain    = "TASKGATE-SEMANTIC-VIEW-ARTIFACT-V1"
	semanticViewCompositionDomain = "TASKGATE-SEMANTIC-VIEW-COMPOSITION-V1"
	semanticViewOuterPlanDomain   = "TASKGATE-SEMANTIC-VIEW-OUTER-PLAN-V1"
)

// SemanticViewPreparationInputsV1 is everything one semantic View preparation
// depends on.
//
// It is a separate type from PreparationInputs, and PrepareSemanticView is a
// separate entry point, because the two arrive along genuinely different paths.
// An ordinary product preparation starts from a QueryPlan and resolves the
// product out of the Catalog. A View preparation starts from a ViewBinding the
// Control Store already verified, a compiled Artifact, a Composition and the
// agent's outer plan, and only then reaches terminal Products. Folding both into
// one Prepare with optional members would make most combinations of those
// members meaningless, and the type would stop saying which ones go together --
// half an ordinary query and half a View is not a thing that can be prepared.
//
// Every member is an immutable value both sides can obtain independently. The
// Gateway resolves the binding from its Control Store and verifies the registry
// signature before calling; the finalizer loads the same values from retained
// frozen evidence. Neither passes a store, a registry, a Service, a database
// handle or any evaluation type, because a preparation that could go and look
// something up would no longer be a function of its inputs, and the two sides
// could then look at different things.
type SemanticViewPreparationInputsV1 struct {
	// OuterPlan is the agent's own plan against the public View relation. Its
	// filters become the V5 predicate footprint's caller bindings.
	OuterPlan queryplan.QueryPlan

	Catalog CatalogView
	Grant   Grant

	// ViewBindingDigest is the resolved binding the Control Store verified, and
	// ExpectedRevisionDigest the registry revision it was pinned to. They are
	// carried as values: this package does not verify signatures, it binds the
	// verified result into the prepared identity so a preparation against a
	// different binding cannot be mistaken for this one.
	ViewBindingDigest      string
	ExpectedRevisionDigest string

	// Artifact is the compiled View, and Composition the outer plan already
	// composed against it. Composition is an input rather than something this
	// package recomputes because composing is the viewcompiler's job and both
	// sides run the same compiler; what preparation owns is proving the
	// composed plan stays inside the root's governance envelope.
	Artifact    viewcompiler.Artifact
	Composition viewcompiler.Composition

	// SnapshotBindings is keyed by publication name, as for PreparationInputs,
	// and is required whenever the profile compiles an ordinal program.
	SnapshotBindings map[string]SnapshotBinding
}

// Validate rejects inputs that cannot settle a semantic View preparation.
func (inputs SemanticViewPreparationInputsV1) Validate() error {
	if err := inputs.Grant.Validate(); err != nil {
		return err
	}
	if err := inputs.Catalog.Validate(); err != nil {
		return err
	}
	if !validSHA256(inputs.ViewBindingDigest) {
		return errors.New("semantic View binding digest is not a lowercase SHA-256")
	}
	if !validSHA256(inputs.ExpectedRevisionDigest) {
		return errors.New("semantic View expected revision digest is not a lowercase SHA-256")
	}
	// The grant must be for this View. The Gateway and the finalizer resolve the
	// binding by different routes, so a grant pinned to another View is exactly
	// the disagreement this comparison exists to catch.
	if inputs.Grant.ViewBindingDigest != "" && inputs.Grant.ViewBindingDigest != inputs.ViewBindingDigest {
		return fmt.Errorf("preparation grant is bound to view %s but the inputs carry %s",
			shortDigest(inputs.Grant.ViewBindingDigest), shortDigest(inputs.ViewBindingDigest))
	}
	if inputs.Grant.ViewRegistryRevision != "" && inputs.Grant.ViewRegistryRevision != inputs.ExpectedRevisionDigest {
		return errors.New("preparation grant names a different view registry revision than the inputs")
	}
	if strings.TrimSpace(inputs.Artifact.Root.Name) == "" {
		return errors.New("semantic View artifact names no root relation")
	}
	if strings.TrimSpace(inputs.OuterPlan.Product) == "" {
		return errors.New("semantic View outer plan names no root product")
	}
	if inputs.OuterPlan.From != nil {
		// The outer plan is the agent's query against the single public View
		// relation. A relational outer plan would mean the View was grafted into
		// a wider graph, which is a different operation and is refused upstream.
		return errors.New("semantic View outer plan is relational, not a single View product")
	}
	if len(inputs.Artifact.Outputs) == 0 {
		return errors.New("semantic View artifact publishes no output")
	}
	if len(inputs.Artifact.BaseProducts) == 0 {
		return errors.New("semantic View artifact closes over no base product")
	}
	if !validSHA256(inputs.Artifact.CanonicalPlanDigest) {
		return errors.New("semantic View artifact canonical plan digest is not a lowercase SHA-256")
	}
	if len(inputs.Composition.VisibleFields) == 0 {
		return errors.New("semantic View composition exposes no visible field")
	}
	// The composed plan's projection and its declared visible fields must line
	// up one for one, or the visible result would not describe the statement.
	if width := len(inputs.Composition.Plan.Columns) + len(inputs.Composition.Plan.Aggregates); width != len(inputs.Composition.VisibleFields) {
		return fmt.Errorf("semantic View composition projects %d value(s) but declares %d visible field(s)",
			width, len(inputs.Composition.VisibleFields))
	}
	if err := inputs.validateSnapshotBindings(); err != nil {
		return err
	}
	return nil
}

// validateSnapshotBindings holds the semantic View path to the same
// dictionary-universe rule as PreparationInputs: the universe is Catalog-wide,
// so a partial resolution is rejected rather than silently narrowing it.
func (inputs SemanticViewPreparationInputsV1) validateSnapshotBindings() error {
	if !inputs.Grant.UsesOrdinalProgram() {
		if len(inputs.SnapshotBindings) != 0 {
			return fmt.Errorf("exposure profile %q compiles no ordinal program but %d snapshot binding(s) were supplied",
				inputs.Grant.ExposureProfile, len(inputs.SnapshotBindings))
		}
		return nil
	}
	for _, publication := range inputs.Catalog.SnapshotPublications {
		binding, found := inputs.SnapshotBindings[publication.Name]
		if !found {
			return fmt.Errorf("exposure profile %q binds the Catalog-wide dictionary universe, "+
				"but publication %q has no resolved snapshot binding",
				inputs.Grant.ExposureProfile, publication.Name)
		}
		if err := binding.RequireCatalog(publication); err != nil {
			return err
		}
	}
	for name := range inputs.SnapshotBindings {
		if _, found := inputs.Catalog.LookupSnapshotPublication(name); !found {
			return fmt.Errorf("snapshot binding %q names a publication this Catalog does not declare", name)
		}
	}
	return nil
}

// RootProduct resolves the public View relation this preparation is rooted at.
//
// The root is the outer plan's product -- the single public relation the agent
// queried -- and not Artifact.Root, which names the compiled physical relation.
// It must be a Catalog product carrying a ViewContract; a relation that is not
// one cannot be the root of a View preparation, and saying so here keeps every
// later step from having to re-check it.
func (inputs SemanticViewPreparationInputsV1) RootProduct() (catalog.Product, error) {
	name := inputs.OuterPlan.Product
	root, found := inputs.Catalog.LookupProduct(name)
	if !found {
		return catalog.Product{}, fmt.Errorf("semantic View root %q is not a product this Catalog declares", name)
	}
	if root.ViewContract == nil {
		return catalog.Product{}, fmt.Errorf("semantic View root %q carries no view contract", name)
	}
	return root, nil
}

// ArtifactSHA256 is the compiled Artifact's identity as prepared against.
//
// The whole Artifact is digested, not its BindingDigest member. An Artifact
// carries digests it computed for itself, and trusting those would make this
// identity a claim the Artifact makes about itself rather than a measurement of
// what preparation actually read. Everything load-bearing -- the plan, the
// outputs, the base-product closure, the dependency closure -- is covered
// because all of it is in the value.
func (inputs SemanticViewPreparationInputsV1) ArtifactSHA256() (string, error) {
	return domainDigest(semanticViewArtifactDomain, inputs.Artifact)
}

// CompositionSHA256 is the composed plan's identity.
func (inputs SemanticViewPreparationInputsV1) CompositionSHA256() (string, error) {
	return domainDigest(semanticViewCompositionDomain, inputs.Composition)
}

// OuterPlanSHA256 is the agent's own plan as supplied.
func (inputs SemanticViewPreparationInputsV1) OuterPlanSHA256() (string, error) {
	return domainDigest(semanticViewOuterPlanDomain, inputs.OuterPlan)
}

// SnapshotBindingSetSHA256 is the canonical digest of the resolved universe,
// empty when the profile resolves none.
func (inputs SemanticViewPreparationInputsV1) SnapshotBindingSetSHA256() (string, error) {
	return PreparationInputs{SnapshotBindings: inputs.SnapshotBindings}.SnapshotBindingSetSHA256()
}

// SHA256 is the identity of the whole semantic View input set.
//
// Like PreparationInputs.SHA256 it is over component digests, so each part keeps
// a separately checkable identity. Note what is signed here that a ViewBinding
// digest alone would not be: the Artifact and Composition values themselves, the
// agent's outer plan, and the Catalog's content including its scope policies.
// Whether the binding digest happens to cover any of those is a question about
// the Control Store's construction, and preparation should not have to answer it
// to know what it prepared against.
func (inputs SemanticViewPreparationInputsV1) SHA256() (string, error) {
	if err := inputs.Validate(); err != nil {
		return "", err
	}
	grant, err := inputs.Grant.SHA256()
	if err != nil {
		return "", err
	}
	outer, err := inputs.OuterPlanSHA256()
	if err != nil {
		return "", err
	}
	artifact, err := inputs.ArtifactSHA256()
	if err != nil {
		return "", err
	}
	composition, err := inputs.CompositionSHA256()
	if err != nil {
		return "", err
	}
	content, err := inputs.Catalog.ContentSHA256()
	if err != nil {
		return "", err
	}
	snapshots, err := inputs.SnapshotBindingSetSHA256()
	if err != nil {
		return "", err
	}
	return domainDigest(semanticViewInputsDomain, struct {
		Version                string `json:"version"`
		OuterPlanSHA256        string `json:"outer_plan_sha256"`
		GrantSHA256            string `json:"grant_sha256"`
		CatalogSHA256          string `json:"catalog_sha256"`
		CatalogVersion         string `json:"catalog_version"`
		CatalogContentSHA256   string `json:"catalog_content_sha256"`
		ViewBindingDigest      string `json:"view_binding_digest"`
		ExpectedRevisionDigest string `json:"expected_revision_digest"`
		ArtifactSHA256         string `json:"view_artifact_sha256"`
		CompositionSHA256      string `json:"view_composition_sha256"`
		SnapshotsSHA           string `json:"snapshot_binding_set_sha256,omitempty"`
	}{PreparedOperationBindingV1Version, outer, grant, inputs.Catalog.Digest,
		inputs.Catalog.Version, content, inputs.ViewBindingDigest, inputs.ExpectedRevisionDigest,
		artifact, composition, snapshots})
}
