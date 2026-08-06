package physicalquery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// PreparedOperationBindingV1Version identifies the durable preparation binding.
const PreparedOperationBindingV1Version = "taskgate-prepared-operation-binding-v1"

// The domain separators. Each digest covers a different thing, and separating
// them means a value that is legitimate in one position cannot be replayed into
// another.
const (
	preparedOperationDomain  = "TASKGATE-PREPARED-OPERATION-BINDING-V1"
	preparedTargetDomain     = "TASKGATE-PREPARED-TARGET-V1"
	compilerIdentityDomain   = "TASKGATE-PREPARATION-COMPILER-IDENTITY-V1"
	fieldListDomain          = "TASKGATE-PREPARATION-FIELD-LIST-V1"
	sidecarGrantsDomain      = "TASKGATE-PREPARATION-SIDECAR-GRANTS-V1"
	viewRevisionDomain       = "TASKGATE-PREPARATION-VIEW-REGISTRY-REVISION-V1"
	policyGrantDomain        = "TASKGATE-PREPARATION-POLICY-GRANT-V1"
	ordinalProgramDomain     = "TASKGATE-PREPARATION-ORDINAL-PROGRAM-V1"
	sourcePublicationsDomain = "TASKGATE-PREPARATION-SOURCE-PUBLICATIONS-V1"
	predicateFootprintDomain = "TASKGATE-PREPARATION-PREDICATE-FOOTPRINT-V1"
)

// TargetRole names one of the two statements an operation prepares.
type TargetRole string

const (
	RoleVisible   TargetRole = "visible"
	RoleCompanion TargetRole = "companion"
)

// CompilerIdentityV1 is the typed identity of the code that produced the
// statements.
//
// It is typed rather than a free string because a string let a caller write
// anything merely non-empty, and this value is what separates "the finalizer
// reproduced the statement" from "the finalizer reproduced a statement some
// other compiler would have produced". Both members of each pair are carried:
// the version is what a human bumps deliberately, and the digest is what moves
// whether or not anybody remembered to.
type CompilerIdentityV1 struct {
	QueryPlanVersion      string `json:"queryplan_version"`
	QueryPlanSHA256       string `json:"queryplan_sha256"`
	PolicyRendererVersion string `json:"policy_renderer_version"`
	PolicyRendererSHA256  string `json:"policy_renderer_sha256"`
	// SHA256 is computed by Seal and recomputed by Validate. It is never
	// supplied by a caller.
	SHA256 string `json:"sha256"`
}

// LocalCompilerIdentity is the sealed identity of the compiler and renderer this
// binary contains.
//
// Both the Gateway and the finalizer call it. Two processes built from one
// commit therefore agree by construction, and two built from different commits
// disagree in the binding rather than silently producing different SQL.
func LocalCompilerIdentity() (CompilerIdentityV1, error) {
	compiler, err := queryplan.CompilerSHA256()
	if err != nil {
		return CompilerIdentityV1{}, fmt.Errorf("query compiler identity: %w", err)
	}
	renderer, err := sqlpolicy.RendererSHA256()
	if err != nil {
		return CompilerIdentityV1{}, fmt.Errorf("policy renderer identity: %w", err)
	}
	return CompilerIdentityV1{
		QueryPlanVersion: queryplan.CompilerVersion, QueryPlanSHA256: compiler,
		PolicyRendererVersion: sqlpolicy.RendererVersion, PolicyRendererSHA256: renderer,
	}.Seal()
}

// Seal computes the identity's own digest over its members.
func (identity CompilerIdentityV1) Seal() (CompilerIdentityV1, error) {
	identity.SHA256 = ""
	digest, err := domainDigest(compilerIdentityDomain, identity)
	if err != nil {
		return CompilerIdentityV1{}, err
	}
	identity.SHA256 = digest
	return identity, nil
}

// Validate recomputes the digest and rejects one that was supplied rather than
// sealed.
func (identity CompilerIdentityV1) Validate() error {
	for name, value := range map[string]string{
		"queryplan version":       identity.QueryPlanVersion,
		"policy renderer version": identity.PolicyRendererVersion,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("compiler identity carries no %s", name)
		}
	}
	for name, value := range map[string]string{
		"queryplan digest":       identity.QueryPlanSHA256,
		"policy renderer digest": identity.PolicyRendererSHA256,
		"identity digest":        identity.SHA256,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("compiler identity %s is not a lowercase SHA-256", name)
		}
	}
	sealed, err := identity.Seal()
	if err != nil {
		return err
	}
	if sealed.SHA256 != identity.SHA256 {
		return fmt.Errorf("compiler identity digest is %s, its members seal to %s",
			shortDigest(identity.SHA256), shortDigest(sealed.SHA256))
	}
	return nil
}

// PreparedOperationBindingV1 is the durable half: what may enter a V9 receipt, a
// Sample or retained evidence.
//
// It carries no statement, no column name and no physical relation name -- only
// a version, flags, counts and cryptographic identities. Projections enter as
// digests and counts rather than as names, because a column name is still a
// disclosure about the query and durable evidence has no need of one.
//
// Every digest is computed by Seal and recomputed by Validate. Under the
// previous shape a caller could write any 64 hex characters and pass a format
// check, which would have made the whole comparison decorative.
type PreparedOperationBindingV1 struct {
	Version string `json:"version"`

	HasCompanion     bool `json:"has_companion"`
	Grouped          bool `json:"grouped"`
	ExpandedEvidence bool `json:"expanded_evidence"`

	VisibleFieldCount    int `json:"visible_field_count"`
	FactFieldCount       int `json:"fact_field_count"`
	ProvenanceFieldCount int `json:"provenance_field_count"`

	VisibleFieldsSHA256    string `json:"visible_fields_sha256"`
	FactFieldsSHA256       string `json:"fact_fields_sha256"`
	ProvenanceFieldsSHA256 string `json:"provenance_fields_sha256,omitempty"`

	// The inputs this preparation was derived from, each sealed over
	// canonicalized values so two semantically equal inputs in different orders
	// reach one identity, and any change to a grant, a scope, a Publication or a
	// sidecar moves it.
	PreparationInputsSHA256  string `json:"preparation_inputs_sha256"`
	GrantSHA256              string `json:"grant_sha256"`
	CatalogSHA256            string `json:"catalog_sha256"`
	SnapshotBindingSetSHA256 string `json:"snapshot_binding_set_sha256,omitempty"`
	PlanSHA256               string `json:"plan_sha256"`
	CompilerIdentitySHA256   string `json:"compiler_identity_sha256"`

	// What preparation produced, beyond the statements themselves.
	//
	// PolicyGrantSHA256 is the authorization the operation will actually be
	// authorized against. It is not GrantSHA256: that one identifies the task
	// authorization preparation READ, while this one identifies the sqlpolicy
	// grant preparation BUILT from it, sidecars folded in. A statement is admitted
	// against the latter, so evidence that named only the former would leave the
	// widening between them undescribed.
	PolicyGrantSHA256          string `json:"policy_grant_sha256"`
	OrdinalProgramSHA256       string `json:"ordinal_program_sha256,omitempty"`
	DictionarySetSHA256        string `json:"dictionary_set_sha256,omitempty"`
	SidecarGrantsSHA256        string `json:"sidecar_grants_sha256,omitempty"`
	SourcePublicationsSHA256   string `json:"source_publications_sha256,omitempty"`
	ViewBindingSHA256          string `json:"view_binding_sha256,omitempty"`
	ViewRegistryRevisionSHA256 string `json:"view_registry_revision_sha256,omitempty"`
	PredicateFootprintSHA256   string `json:"predicate_footprint_sha256,omitempty"`
	EstimatedBaseFacts         uint64 `json:"estimated_base_facts,omitempty"`

	// The two target bindings. Each covers its statement's digest, the inputs it
	// was prepared from and the compiler that produced it, so a statement cannot
	// be presented as the other role's or as another operation's.
	VisibleTargetSHA256 string `json:"visible_target_sha256"`
	// CompanionTargetSHA256 is empty exactly when HasCompanion is false.
	CompanionTargetSHA256 string `json:"companion_target_sha256,omitempty"`

	// SHA256 seals every member above.
	SHA256 string `json:"sha256"`
}

// Seal computes the binding's own digest over every other member.
func (binding PreparedOperationBindingV1) Seal() (PreparedOperationBindingV1, error) {
	binding.Version = PreparedOperationBindingV1Version
	binding.SHA256 = ""
	digest, err := domainDigest(preparedOperationDomain, binding)
	if err != nil {
		return PreparedOperationBindingV1{}, err
	}
	binding.SHA256 = digest
	return binding, nil
}

// Validate rejects a binding that is internally incoherent, and one whose digest
// was supplied rather than derived from its members.
func (binding PreparedOperationBindingV1) Validate() error {
	if binding.Version != PreparedOperationBindingV1Version {
		return fmt.Errorf("prepared operation binding version %q is unsupported", binding.Version)
	}
	for name, digest := range map[string]string{
		"visible fields":     binding.VisibleFieldsSHA256,
		"fact fields":        binding.FactFieldsSHA256,
		"preparation inputs": binding.PreparationInputsSHA256,
		"grant":              binding.GrantSHA256,
		"catalog":            binding.CatalogSHA256,
		"plan":               binding.PlanSHA256,
		"compiler identity":  binding.CompilerIdentitySHA256,
		"policy grant":       binding.PolicyGrantSHA256,
		"visible target":     binding.VisibleTargetSHA256,
		"prepared operation": binding.SHA256,
	} {
		if !validSHA256(digest) {
			return fmt.Errorf("prepared operation binding %s digest is not a lowercase SHA-256", name)
		}
	}
	for name, digest := range map[string]string{
		"provenance fields":      binding.ProvenanceFieldsSHA256,
		"snapshot binding set":   binding.SnapshotBindingSetSHA256,
		"ordinal program":        binding.OrdinalProgramSHA256,
		"dictionary set":         binding.DictionarySetSHA256,
		"sidecar grants":         binding.SidecarGrantsSHA256,
		"source publications":    binding.SourcePublicationsSHA256,
		"view binding":           binding.ViewBindingSHA256,
		"view registry revision": binding.ViewRegistryRevisionSHA256,
		"predicate footprint":    binding.PredicateFootprintSHA256,
		"companion target":       binding.CompanionTargetSHA256,
	} {
		if digest != "" && !validSHA256(digest) {
			return fmt.Errorf("prepared operation binding %s digest is not a lowercase SHA-256", name)
		}
	}
	// Presence-coupled in both directions, so a companion cannot be half present.
	if binding.HasCompanion == (binding.CompanionTargetSHA256 == "") {
		return errors.New("prepared operation binding's companion target and its presence flag disagree")
	}
	if binding.VisibleFieldCount <= 0 {
		return fmt.Errorf("prepared operation binding projects %d visible fields", binding.VisibleFieldCount)
	}
	if (binding.ProvenanceFieldCount == 0) != (binding.ProvenanceFieldsSHA256 == "") {
		return errors.New("prepared operation binding's provenance field count and digest disagree")
	}
	// Expanded evidence is a property of a companion-bearing operation. Without
	// one there is no evidence row budget to expand.
	if binding.ExpandedEvidence && !binding.HasCompanion {
		return errors.New("prepared operation binding claims expanded evidence but prepares no companion statement")
	}
	sealed, err := binding.Seal()
	if err != nil {
		return err
	}
	if sealed.SHA256 != binding.SHA256 {
		return fmt.Errorf("prepared operation binding digest is %s, its members seal to %s",
			shortDigest(binding.SHA256), shortDigest(sealed.SHA256))
	}
	return nil
}

// TargetSHA256 is one prepared target's binding.
func (binding PreparedOperationBindingV1) TargetSHA256(role TargetRole) (string, error) {
	switch role {
	case RoleVisible:
		return binding.VisibleTargetSHA256, nil
	case RoleCompanion:
		if !binding.HasCompanion {
			return "", errors.New("this operation prepares no companion statement")
		}
		return binding.CompanionTargetSHA256, nil
	default:
		return "", fmt.Errorf("%q is not a prepared target role", role)
	}
}

// RequireSame compares two independent preparations of what should be one
// operation and names every member they disagree on.
//
// This is what the finalizer rests on: it prepares from its own frozen inputs
// and requires the result to equal what the Gateway signed. Both sides are
// bindings, so no statement can enter the comparison or its failure message.
func (binding PreparedOperationBindingV1) RequireSame(other PreparedOperationBindingV1) error {
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("this preparation: %w", err)
	}
	if err := other.Validate(); err != nil {
		return fmt.Errorf("the other preparation: %w", err)
	}
	if binding.SHA256 == other.SHA256 {
		return nil
	}
	// The sealed digests differ, so at least one member does. Naming them is
	// what makes a rejection say what was got wrong rather than merely that
	// something was.
	var differences []string
	for name, pair := range map[string][2]any{
		"has companion":          {binding.HasCompanion, other.HasCompanion},
		"grouped":                {binding.Grouped, other.Grouped},
		"expanded evidence":      {binding.ExpandedEvidence, other.ExpandedEvidence},
		"visible field count":    {binding.VisibleFieldCount, other.VisibleFieldCount},
		"fact field count":       {binding.FactFieldCount, other.FactFieldCount},
		"provenance field count": {binding.ProvenanceFieldCount, other.ProvenanceFieldCount},
		"visible fields":         {binding.VisibleFieldsSHA256, other.VisibleFieldsSHA256},
		"fact fields":            {binding.FactFieldsSHA256, other.FactFieldsSHA256},
		"provenance fields":      {binding.ProvenanceFieldsSHA256, other.ProvenanceFieldsSHA256},
		"preparation inputs":     {binding.PreparationInputsSHA256, other.PreparationInputsSHA256},
		"grant":                  {binding.GrantSHA256, other.GrantSHA256},
		"catalog":                {binding.CatalogSHA256, other.CatalogSHA256},
		"snapshot binding set":   {binding.SnapshotBindingSetSHA256, other.SnapshotBindingSetSHA256},
		"plan":                   {binding.PlanSHA256, other.PlanSHA256},
		"compiler identity":      {binding.CompilerIdentitySHA256, other.CompilerIdentitySHA256},
		"policy grant":           {binding.PolicyGrantSHA256, other.PolicyGrantSHA256},
		"ordinal program":        {binding.OrdinalProgramSHA256, other.OrdinalProgramSHA256},
		"dictionary set":         {binding.DictionarySetSHA256, other.DictionarySetSHA256},
		"sidecar grants":         {binding.SidecarGrantsSHA256, other.SidecarGrantsSHA256},
		"source publications":    {binding.SourcePublicationsSHA256, other.SourcePublicationsSHA256},
		"view binding":           {binding.ViewBindingSHA256, other.ViewBindingSHA256},
		"view registry revision": {binding.ViewRegistryRevisionSHA256, other.ViewRegistryRevisionSHA256},
		"predicate footprint":    {binding.PredicateFootprintSHA256, other.PredicateFootprintSHA256},
		"estimated base facts":   {binding.EstimatedBaseFacts, other.EstimatedBaseFacts},
		"visible target":         {binding.VisibleTargetSHA256, other.VisibleTargetSHA256},
		"companion target":       {binding.CompanionTargetSHA256, other.CompanionTargetSHA256},
	} {
		if pair[0] != pair[1] {
			differences = append(differences, name)
		}
	}
	sort.Strings(differences)
	if len(differences) == 0 {
		// The sealed digests differ but no named member does, which means the
		// binding has a member this comparison does not cover. That is a defect
		// here, and saying so beats reporting agreement.
		return fmt.Errorf("two preparations seal to %s and %s but disagree on no named member; "+
			"PreparedOperationBindingV1 has a member RequireSame does not compare",
			shortDigest(binding.SHA256), shortDigest(other.SHA256))
	}
	return fmt.Errorf("two independent preparations of one operation disagree on %v", differences)
}

// OperationDraft is what a derivation produces, before it is sealed.
//
// It exists so that everything load-bearing enters a PreparedOperation through
// one function, in one value. A caller assembles a draft; NewPreparedOperation
// derives every digest from it and returns the sealed object. No digest is ever
// supplied, so there is no member a caller could assert rather than earn.
type OperationDraft struct {
	// VisibleSQL is the statement whose rows the agent sees. CompanionSQL is the
	// provenance statement, empty when the operation prepares none; there is no
	// separate presence flag, because a flag and a statement can disagree.
	VisibleSQL   string
	CompanionSQL string

	VisibleFields    []string
	FactFields       []string
	ProvenanceFields []string

	Grouped          bool
	ExpandedEvidence bool

	// PolicyGrant is what Derive authorizes against. It is produced by
	// preparation rather than by the caller, so the Gateway and the finalizer
	// authorize identically.
	PolicyGrant sqlpolicy.Grant
	// SidecarGrants are the ordinal sidecar product grants folded into
	// PolicyGrant, retained separately so their identity can be bound.
	SidecarGrants []sqlpolicy.ProductGrant
	// OrdinalProgram is the compiled program, absent when the profile has none.
	OrdinalProgram *queryplan.OrdinalProgram
	// DictionarySet is the Catalog-wide ordinal dictionary universe, absent when
	// the profile compiles no ordinal program.
	DictionarySet *ordinal.DictionarySetManifest
	// SourcePublications maps each ordinal source alias to the publication it
	// resolved to, so the Gateway can reattach its own verified SnapshotIndex
	// handles without this package ever holding one.
	SourcePublications map[string]string
	// EstimatedBaseFacts is the prepared estimate, zero when no ordinal program
	// was compiled.
	EstimatedBaseFacts uint64
	// PredicateFootprint is the V5 footprint, absent under other profiles.
	PredicateFootprint *queryplan.PredicateFootprint
}

// PreparedOperation is the in-memory result of preparing one operation.
//
// It holds the statements, the projections, the compiled ordinal program, the
// policy grant to authorize with and the verified sidecar bindings. None of it
// may be serialized. What travels is the binding.
//
// Every member is unexported and every accessor copies. That is the point: a
// coherent binding proves only that the binding is coherent, and the question
// the Gateway's evidence actually rests on is whether the object about to be
// executed is the one the binding describes. Under the previous shape a caller
// could prepare correctly, then rewrite the SQL, the policy grant or the ordinal
// program on the returned value and execute something the signed binding never
// covered. Closing construction is what makes that unrepresentable rather than
// merely discouraged, and Validate recomputing every member from the runtime
// state is what catches an edit made from inside this package.
type PreparedOperation struct {
	visibleSQL   string
	companionSQL string
	hasCompanion bool

	visibleFields    []string
	factFields       []string
	provenanceFields []string

	grouped          bool
	expandedEvidence bool

	policyGrant        sqlpolicy.Grant
	sidecarGrants      []sqlpolicy.ProductGrant
	ordinalProgram     *queryplan.OrdinalProgram
	dictionarySet      *ordinal.DictionarySetManifest
	sourcePublications map[string]string
	estimatedBaseFacts uint64
	predicateFootprint *queryplan.PredicateFootprint

	binding PreparedOperationBindingV1
}

// NewPreparedOperation is the only way to obtain a PreparedOperation.
//
// It derives every digest in the binding from the draft and the inputs, seals
// the binding, and then validates the result the same way any later holder
// would. An operation that cannot pass its own Validate is never returned, so
// "unvalidated prepared operation" is not a state that exists.
func NewPreparedOperation(inputs PreparationInputs, compiler CompilerIdentityV1,
	draft OperationDraft) (PreparedOperation, error) {
	if err := compiler.Validate(); err != nil {
		return PreparedOperation{}, err
	}
	inputsDigest, err := inputs.SHA256()
	if err != nil {
		return PreparedOperation{}, err
	}
	grantDigest, err := inputs.Grant.SHA256()
	if err != nil {
		return PreparedOperation{}, err
	}
	planDigest, err := inputs.PlanSHA256()
	if err != nil {
		return PreparedOperation{}, err
	}
	snapshotDigest, err := inputs.SnapshotBindingSetSHA256()
	if err != nil {
		return PreparedOperation{}, err
	}
	viewRevision, err := textDigest(viewRevisionDomain, inputs.Grant.ViewRegistryRevision)
	if err != nil {
		return PreparedOperation{}, err
	}

	// The draft is copied in, so a caller that keeps its own reference and
	// mutates it afterwards cannot reach what was sealed.
	prepared := PreparedOperation{
		visibleSQL:   draft.VisibleSQL,
		companionSQL: draft.CompanionSQL,
		hasCompanion: strings.TrimSpace(draft.CompanionSQL) != "",

		visibleFields:    cloneStrings(draft.VisibleFields),
		factFields:       cloneStrings(draft.FactFields),
		provenanceFields: cloneStrings(draft.ProvenanceFields),

		grouped:          draft.Grouped,
		expandedEvidence: draft.ExpandedEvidence,

		policyGrant:        clonePolicyGrant(draft.PolicyGrant),
		sidecarGrants:      cloneProductGrants(draft.SidecarGrants),
		sourcePublications: cloneStringMap(draft.SourcePublications),
		estimatedBaseFacts: draft.EstimatedBaseFacts,
	}
	if prepared.ordinalProgram, err = cloneOrdinalProgram(draft.OrdinalProgram); err != nil {
		return PreparedOperation{}, err
	}
	if prepared.dictionarySet, err = cloneDictionarySet(draft.DictionarySet); err != nil {
		return PreparedOperation{}, err
	}
	if prepared.predicateFootprint, err = clonePredicateFootprint(draft.PredicateFootprint); err != nil {
		return PreparedOperation{}, err
	}

	runtime, err := prepared.runtimeDigests(inputsDigest, compiler.SHA256)
	if err != nil {
		return PreparedOperation{}, err
	}
	binding, err := PreparedOperationBindingV1{
		HasCompanion: prepared.hasCompanion, Grouped: prepared.grouped,
		ExpandedEvidence: prepared.expandedEvidence,

		VisibleFieldCount:    len(prepared.visibleFields),
		FactFieldCount:       len(prepared.factFields),
		ProvenanceFieldCount: len(prepared.provenanceFields),

		VisibleFieldsSHA256:    runtime.visibleFields,
		FactFieldsSHA256:       runtime.factFields,
		ProvenanceFieldsSHA256: runtime.provenanceFields,

		PreparationInputsSHA256:  inputsDigest,
		GrantSHA256:              grantDigest,
		CatalogSHA256:            inputs.Catalog.Digest,
		SnapshotBindingSetSHA256: snapshotDigest,
		PlanSHA256:               planDigest,
		CompilerIdentitySHA256:   compiler.SHA256,

		PolicyGrantSHA256:        runtime.policyGrant,
		OrdinalProgramSHA256:     runtime.ordinalProgram,
		DictionarySetSHA256:      runtime.dictionarySet,
		SidecarGrantsSHA256:      runtime.sidecarGrants,
		SourcePublicationsSHA256: runtime.sourcePublications,
		PredicateFootprintSHA256: runtime.predicateFootprint,
		EstimatedBaseFacts:       prepared.estimatedBaseFacts,

		ViewBindingSHA256:          inputs.Grant.ViewBindingDigest,
		ViewRegistryRevisionSHA256: viewRevision,

		VisibleTargetSHA256:   runtime.visibleTarget,
		CompanionTargetSHA256: runtime.companionTarget,
	}.Seal()
	if err != nil {
		return PreparedOperation{}, err
	}
	prepared.binding = binding
	if err := prepared.Validate(); err != nil {
		return PreparedOperation{}, err
	}
	return prepared, nil
}

// The read-only surface.
//
// Each accessor that returns a map, a slice or a pointer returns a copy. Handing
// back the sealed state itself would reintroduce exactly the mutation closing
// construction removed, one indirection further out.

// Binding is the durable half, sealed. Every member is a scalar, so the value is
// safe to return directly.
func (prepared PreparedOperation) Binding() PreparedOperationBindingV1 { return prepared.binding }

// HasCompanion reports whether a provenance statement was prepared.
func (prepared PreparedOperation) HasCompanion() bool { return prepared.hasCompanion }

// Grouped and ExpandedEvidence are the exposure shape.
func (prepared PreparedOperation) Grouped() bool          { return prepared.grouped }
func (prepared PreparedOperation) ExpandedEvidence() bool { return prepared.expandedEvidence }

// ExecutableStatements is the only way to reach the SQL.
//
// It validates first, so a caller cannot execute an operation whose runtime
// state and binding have come apart -- including one this package's own code
// mutated after sealing. companion is empty exactly when HasCompanion is false.
func (prepared PreparedOperation) ExecutableStatements() (visible, companion string, err error) {
	if err := prepared.Validate(); err != nil {
		return "", "", err
	}
	return prepared.visibleSQL, prepared.companionSQL, nil
}

// The projections, in order. Order is part of the identity, so these are the
// prepared order and not a sorted one.
func (prepared PreparedOperation) VisibleFields() []string {
	return cloneStrings(prepared.visibleFields)
}
func (prepared PreparedOperation) FactFields() []string { return cloneStrings(prepared.factFields) }
func (prepared PreparedOperation) ProvenanceFields() []string {
	return cloneStrings(prepared.provenanceFields)
}

// PolicyGrant is the authorization Derive admits the statements against.
func (prepared PreparedOperation) PolicyGrant() sqlpolicy.Grant {
	return clonePolicyGrant(prepared.policyGrant)
}

// SidecarGrants are the ordinal sidecar grants folded into PolicyGrant.
func (prepared PreparedOperation) SidecarGrants() []sqlpolicy.ProductGrant {
	return cloneProductGrants(prepared.sidecarGrants)
}

// SourcePublications maps each ordinal source alias to its publication name.
func (prepared PreparedOperation) SourcePublications() map[string]string {
	return cloneStringMap(prepared.sourcePublications)
}

// EstimatedBaseFacts is the prepared working-set estimate.
func (prepared PreparedOperation) EstimatedBaseFacts() uint64 { return prepared.estimatedBaseFacts }

// OrdinalProgram returns a copy of the compiled program, nil when the profile
// compiled none.
func (prepared PreparedOperation) OrdinalProgram() (*queryplan.OrdinalProgram, error) {
	return cloneOrdinalProgram(prepared.ordinalProgram)
}

// DictionarySet returns a copy of the Catalog-wide dictionary universe, nil when
// the profile resolved none.
func (prepared PreparedOperation) DictionarySet() (*ordinal.DictionarySetManifest, error) {
	return cloneDictionarySet(prepared.dictionarySet)
}

// PredicateFootprint returns a copy of the V5 footprint, nil under profiles that
// prepare none.
func (prepared PreparedOperation) PredicateFootprint() (*queryplan.PredicateFootprint, error) {
	return clonePredicateFootprint(prepared.predicateFootprint)
}

// errPreparedOperationIsNotSerializable is what a caller gets for trying.
var errPreparedOperationIsNotSerializable = errors.New(
	"a PreparedOperation holds executable SQL and must never be serialized; " +
		"its Binding is what enters a receipt, a Sample or retained evidence")

// MarshalJSON refuses.
//
// The previous shape relied on `json:"-"` tags plus a helper that scanned the
// encoded form for SQL keywords. Both are conventions: a member added without
// the tag serializes, and a scan only finds tokens somebody thought to list.
// Refusing outright is the only version of this rule a future edit cannot
// weaken by omission.
func (PreparedOperation) MarshalJSON() ([]byte, error) {
	return nil, errPreparedOperationIsNotSerializable
}

// runtimeDigestSet is every digest recomputed from the runtime members.
//
// It is one struct rather than a dozen returns so that sealing and validating
// call the same code. If they did not, the two could drift and Validate would be
// checking a construction nothing produces.
type runtimeDigestSet struct {
	visibleFields      string
	factFields         string
	provenanceFields   string
	policyGrant        string
	sidecarGrants      string
	ordinalProgram     string
	dictionarySet      string
	sourcePublications string
	predicateFootprint string
	visibleTarget      string
	companionTarget    string
}

// runtimeDigests derives every digest from the runtime members alone.
//
// The two arguments are the only material that is not a runtime member: a target
// binding covers the inputs and the compiler as well as the bytes, and both are
// carried in the binding itself, so Validate can pass the binding's own copies
// back in and still be checking the statements rather than trusting them.
func (prepared PreparedOperation) runtimeDigests(inputsSHA256, compilerSHA256 string) (runtimeDigestSet, error) {
	var set runtimeDigestSet
	var err error
	if set.visibleFields, err = fieldListSHA256(prepared.visibleFields); err != nil {
		return runtimeDigestSet{}, err
	}
	if set.factFields, err = fieldListSHA256(prepared.factFields); err != nil {
		return runtimeDigestSet{}, err
	}
	if set.provenanceFields, err = fieldListSHA256(prepared.provenanceFields); err != nil {
		return runtimeDigestSet{}, err
	}
	if set.policyGrant, err = policyGrantSHA256(prepared.policyGrant); err != nil {
		return runtimeDigestSet{}, err
	}
	if set.sidecarGrants, err = sidecarGrantsSHA256(prepared.sidecarGrants); err != nil {
		return runtimeDigestSet{}, err
	}
	if set.ordinalProgram, err = ordinalProgramSHA256(prepared.ordinalProgram); err != nil {
		return runtimeDigestSet{}, err
	}
	// The dictionary set uses the manifest's own digest rather than a digest
	// local to this package. That value is what the Control Store keys ordinal
	// observations on, so computing a second one here would let the binding agree
	// with itself while naming a universe no ledger could be found under.
	if prepared.dictionarySet != nil {
		if set.dictionarySet, err = prepared.dictionarySet.Digest(); err != nil {
			return runtimeDigestSet{}, fmt.Errorf("dictionary set digest: %w", err)
		}
	}
	if set.sourcePublications, err = sourcePublicationsSHA256(prepared.sourcePublications); err != nil {
		return runtimeDigestSet{}, err
	}
	if set.predicateFootprint, err = predicateFootprintSHA256(prepared.predicateFootprint); err != nil {
		return runtimeDigestSet{}, err
	}
	if set.visibleTarget, err = preparedTargetSHA256(
		RoleVisible, prepared.visibleSQL, inputsSHA256, compilerSHA256); err != nil {
		return runtimeDigestSet{}, err
	}
	if set.companionTarget, err = preparedTargetSHA256(
		RoleCompanion, prepared.companionSQL, inputsSHA256, compilerSHA256); err != nil {
		return runtimeDigestSet{}, err
	}
	return set, nil
}

// Validate rejects a prepared operation that cannot be executed or compared, and
// one whose two halves describe different operations.
//
// It recomputes every digest in the binding that a runtime member determines,
// from that member. Checking only that the binding seals to its own digest would
// establish that the binding is self-consistent and nothing about the object the
// Gateway is holding; this is what makes the two the same claim.
func (prepared PreparedOperation) Validate() error {
	if strings.TrimSpace(prepared.visibleSQL) == "" {
		return errors.New("prepared operation carries no visible statement")
	}
	if prepared.hasCompanion == (strings.TrimSpace(prepared.companionSQL) == "") {
		return errors.New("prepared operation's companion statement and its presence flag disagree")
	}
	if len(prepared.visibleFields) == 0 {
		return errors.New("prepared operation projects no visible field")
	}
	binding := prepared.binding
	if err := binding.Validate(); err != nil {
		return err
	}
	// The binding must describe THIS operation. Without this the halves could
	// drift and every later comparison would be against the wrong one.
	if binding.HasCompanion != prepared.hasCompanion {
		return errors.New("prepared operation and its binding disagree about the companion statement")
	}
	if binding.Grouped != prepared.grouped || binding.ExpandedEvidence != prepared.expandedEvidence {
		return errors.New("prepared operation and its binding disagree about the exposure shape")
	}
	if binding.VisibleFieldCount != len(prepared.visibleFields) ||
		binding.FactFieldCount != len(prepared.factFields) ||
		binding.ProvenanceFieldCount != len(prepared.provenanceFields) {
		return errors.New("prepared operation and its binding disagree about the projected fields")
	}
	if binding.EstimatedBaseFacts != prepared.estimatedBaseFacts {
		return errors.New("prepared operation and its binding disagree about the estimated base facts")
	}
	// The target digests are recomputed against the binding's own inputs and
	// compiler identities. Those two are not runtime members, so this proves the
	// statements are the ones the binding names -- not that the binding names the
	// right inputs, which is what RequireInputs is for.
	runtime, err := prepared.runtimeDigests(binding.PreparationInputsSHA256, binding.CompilerIdentitySHA256)
	if err != nil {
		return err
	}
	for _, member := range []struct {
		name          string
		derived, held string
	}{
		{"visible statement", runtime.visibleTarget, binding.VisibleTargetSHA256},
		{"companion statement", runtime.companionTarget, binding.CompanionTargetSHA256},
		{"visible fields", runtime.visibleFields, binding.VisibleFieldsSHA256},
		{"fact fields", runtime.factFields, binding.FactFieldsSHA256},
		{"provenance fields", runtime.provenanceFields, binding.ProvenanceFieldsSHA256},
		{"policy grant", runtime.policyGrant, binding.PolicyGrantSHA256},
		{"sidecar grants", runtime.sidecarGrants, binding.SidecarGrantsSHA256},
		{"ordinal program", runtime.ordinalProgram, binding.OrdinalProgramSHA256},
		{"dictionary set", runtime.dictionarySet, binding.DictionarySetSHA256},
		{"source publications", runtime.sourcePublications, binding.SourcePublicationsSHA256},
		{"predicate footprint", runtime.predicateFootprint, binding.PredicateFootprintSHA256},
	} {
		if member.derived != member.held {
			return fmt.Errorf(
				"prepared operation's %s digests to %s, its binding carries %s; "+
					"the object about to execute is not the one the binding describes",
				member.name, shortDigest(member.derived), shortDigest(member.held))
		}
	}
	return nil
}

// RequireInputs proves the operation was prepared from these inputs by this
// compiler, rather than merely from some inputs it also carries the digests of.
//
// Validate closes the runtime-to-binding gap; this closes the binding-to-inputs
// one. A caller that holds the source material -- the Gateway before it
// executes, the finalizer before it compares -- calls both.
func (prepared PreparedOperation) RequireInputs(inputs PreparationInputs, compiler CompilerIdentityV1) error {
	if err := prepared.Validate(); err != nil {
		return err
	}
	if err := compiler.Validate(); err != nil {
		return err
	}
	inputsDigest, err := inputs.SHA256()
	if err != nil {
		return err
	}
	grantDigest, err := inputs.Grant.SHA256()
	if err != nil {
		return err
	}
	planDigest, err := inputs.PlanSHA256()
	if err != nil {
		return err
	}
	snapshotDigest, err := inputs.SnapshotBindingSetSHA256()
	if err != nil {
		return err
	}
	viewRevision, err := textDigest(viewRevisionDomain, inputs.Grant.ViewRegistryRevision)
	if err != nil {
		return err
	}
	binding := prepared.binding
	for _, member := range []struct {
		name         string
		wanted, held string
	}{
		{"preparation inputs", inputsDigest, binding.PreparationInputsSHA256},
		{"grant", grantDigest, binding.GrantSHA256},
		{"catalog", inputs.Catalog.Digest, binding.CatalogSHA256},
		{"plan", planDigest, binding.PlanSHA256},
		{"snapshot binding set", snapshotDigest, binding.SnapshotBindingSetSHA256},
		{"compiler identity", compiler.SHA256, binding.CompilerIdentitySHA256},
		{"view binding", inputs.Grant.ViewBindingDigest, binding.ViewBindingSHA256},
		{"view registry revision", viewRevision, binding.ViewRegistryRevisionSHA256},
	} {
		if member.wanted != member.held {
			return fmt.Errorf("prepared operation was not prepared from these inputs: "+
				"the %s digest is %s, these inputs give %s",
				member.name, shortDigest(member.held), shortDigest(member.wanted))
		}
	}
	return nil
}

// preparedTargetSHA256 binds one statement to the operation it belongs to.
//
// The statement enters as a digest, so the binding stays SQL-free while still
// moving whenever the executed bytes do. The role is a member because the same
// statement in the other position is a different target, and the inputs and
// compiler digests are members because the same bytes prepared from other
// material, or by another compiler, are not the same target either.
func preparedTargetSHA256(role TargetRole, sql, inputsSHA256, compilerSHA256 string) (string, error) {
	if sql == "" {
		return "", nil
	}
	return domainDigest(preparedTargetDomain, struct {
		Version   string     `json:"version"`
		Role      TargetRole `json:"role"`
		SQLSHA256 string     `json:"sql_sha256"`
		Inputs    string     `json:"preparation_inputs_sha256"`
		Compiler  string     `json:"compiler_identity_sha256"`
	}{PreparedOperationBindingV1Version, role, ExactDigest(sql), inputsSHA256, compilerSHA256})
}

// fieldListSHA256 digests an ordered projection.
//
// Order is part of the identity: two statements projecting the same columns in
// different orders return different results, so they must not share a digest.
func fieldListSHA256(fields []string) (string, error) {
	if len(fields) == 0 {
		return "", nil
	}
	return domainDigest(fieldListDomain, fields)
}

// sidecarGrantsSHA256 digests the ordinal sidecar grants in canonical order.
func sidecarGrantsSHA256(grants []sqlpolicy.ProductGrant) (string, error) {
	if len(grants) == 0 {
		return "", nil
	}
	canonical := make([]sqlpolicy.ProductGrant, len(grants))
	copy(canonical, grants)
	for index := range canonical {
		canonical[index].ApprovedColumns = canonicalStrings(canonical[index].ApprovedColumns)
		canonical[index].AllowedOperators = canonicalStrings(canonical[index].AllowedOperators)
	}
	sort.Slice(canonical, func(left, right int) bool {
		return canonical[left].LogicalName < canonical[right].LogicalName
	})
	return domainDigest(sidecarGrantsDomain, canonical)
}

// policyGrantSHA256 digests the authorization the statements are admitted
// against, in canonical form.
//
// Column, function, aggregate and operator lists and the mandatory scope are all
// sets, so they are sorted and deduplicated: two callers that assembled the same
// authorization in different orders must reach one identity, or the digest would
// report a difference in how a grant was built rather than in what it permits.
func policyGrantSHA256(grant sqlpolicy.Grant) (string, error) {
	return domainDigest(policyGrantDomain, canonicalProductGrants(grant.Products))
}

// ordinalProgramSHA256 digests the compiled program exactly as compiled.
//
// Nothing is reordered here, unlike a grant: a program's source order, witness
// rules and provenance order determine which base cells the evidence statement
// returns, so two orderings are two programs.
func ordinalProgramSHA256(program *queryplan.OrdinalProgram) (string, error) {
	if program == nil {
		return "", nil
	}
	return domainDigest(ordinalProgramDomain, program)
}

// sourcePublicationsSHA256 digests the alias-to-publication mapping.
//
// It is flattened to a sorted slice because a Go map has no order, and the
// mapping is what lets the Gateway reattach its own verified snapshot handles --
// so a change to it silently redirects a source at a different immutable
// artifact unless the identity moves.
func sourcePublicationsSHA256(publications map[string]string) (string, error) {
	if len(publications) == 0 {
		return "", nil
	}
	type sourcePublication struct {
		SourceAlias string `json:"source_alias"`
		Publication string `json:"publication"`
	}
	flattened := make([]sourcePublication, 0, len(publications))
	for alias, publication := range publications {
		flattened = append(flattened, sourcePublication{SourceAlias: alias, Publication: publication})
	}
	sort.Slice(flattened, func(left, right int) bool {
		return flattened[left].SourceAlias < flattened[right].SourceAlias
	})
	return domainDigest(sourcePublicationsDomain, flattened)
}

// predicateFootprintSHA256 digests the V5 footprint as prepared.
//
// The footprint already carries its own context and atom-set digests. Those are
// covered here rather than used in place of this one: they identify what the
// footprint claims, and this identifies the whole value, including the counts
// the ledger charges against.
func predicateFootprintSHA256(footprint *queryplan.PredicateFootprint) (string, error) {
	if footprint == nil {
		return "", nil
	}
	return domainDigest(predicateFootprintDomain, footprint)
}

// canonicalProductGrants is the one canonical form both grant digests use.
func canonicalProductGrants(grants []sqlpolicy.ProductGrant) []sqlpolicy.ProductGrant {
	if len(grants) == 0 {
		return nil
	}
	canonical := make([]sqlpolicy.ProductGrant, 0, len(grants))
	for _, grant := range grants {
		grant.ApprovedColumns = canonicalStrings(grant.ApprovedColumns)
		grant.AllowedFunctions = canonicalStrings(grant.AllowedFunctions)
		grant.AllowedAggregates = canonicalStrings(grant.AllowedAggregates)
		grant.AllowedOperators = canonicalStrings(grant.AllowedOperators)
		scope := make([]sqlpolicy.ScopePredicate, len(grant.MandatoryScope))
		copy(scope, grant.MandatoryScope)
		for index := range scope {
			scope[index].Values = canonicalStrings(scope[index].Values)
		}
		sort.SliceStable(scope, func(left, right int) bool {
			if scope[left].Column != scope[right].Column {
				return scope[left].Column < scope[right].Column
			}
			return scope[left].Operator < scope[right].Operator
		})
		grant.MandatoryScope = scope
		canonical = append(canonical, grant)
	}
	sort.SliceStable(canonical, func(left, right int) bool {
		return canonical[left].LogicalName < canonical[right].LogicalName
	})
	return canonical
}

// ------------------------------------------------------------------- copying

// The accessors and the constructor both copy. A shallow struct copy is not
// enough for any of these types: they carry slices and maps, so the copy would
// share backing storage with the sealed state and a caller could reach through
// it to change what a signed binding covers.

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	clone := make([]string, len(values))
	copy(clone, values)
	return clone
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func cloneProductGrant(grant sqlpolicy.ProductGrant) sqlpolicy.ProductGrant {
	grant.ApprovedColumns = cloneStrings(grant.ApprovedColumns)
	grant.AllowedFunctions = cloneStrings(grant.AllowedFunctions)
	grant.AllowedAggregates = cloneStrings(grant.AllowedAggregates)
	grant.AllowedOperators = cloneStrings(grant.AllowedOperators)
	if len(grant.MandatoryScope) > 0 {
		scope := make([]sqlpolicy.ScopePredicate, len(grant.MandatoryScope))
		for index, predicate := range grant.MandatoryScope {
			predicate.Values = cloneStrings(predicate.Values)
			scope[index] = predicate
		}
		grant.MandatoryScope = scope
	} else {
		grant.MandatoryScope = nil
	}
	return grant
}

func cloneProductGrants(grants []sqlpolicy.ProductGrant) []sqlpolicy.ProductGrant {
	if len(grants) == 0 {
		return nil
	}
	clone := make([]sqlpolicy.ProductGrant, len(grants))
	for index, grant := range grants {
		clone[index] = cloneProductGrant(grant)
	}
	return clone
}

func clonePolicyGrant(grant sqlpolicy.Grant) sqlpolicy.Grant {
	return sqlpolicy.Grant{Products: cloneProductGrants(grant.Products)}
}

func cloneDictionarySet(set *ordinal.DictionarySetManifest) (*ordinal.DictionarySetManifest, error) {
	if set == nil {
		return nil, nil
	}
	clone := *set
	if len(set.Members) > 0 {
		clone.Members = make([]ordinal.DictionarySetMember, len(set.Members))
		copy(clone.Members, set.Members)
	} else {
		clone.Members = nil
	}
	// A manifest that cannot digest cannot be bound, and returning it would defer
	// the failure to whoever tried.
	if _, err := clone.Digest(); err != nil {
		return nil, fmt.Errorf("dictionary set: %w", err)
	}
	return &clone, nil
}

// cloneOrdinalProgram and clonePredicateFootprint round-trip through the
// canonical encoder rather than copying field by field.
//
// Both types are large, deeply nested and still growing, and a hand-written copy
// would silently start sharing storage the first time a member was added. The
// round-trip cannot: whatever the encoder covers is what the digests cover too,
// so a member either survives both or neither, and jsonCloneIsFaithful is the
// test that says so.
func cloneOrdinalProgram(program *queryplan.OrdinalProgram) (*queryplan.OrdinalProgram, error) {
	if program == nil {
		return nil, nil
	}
	var clone queryplan.OrdinalProgram
	if err := jsonClone(program, &clone); err != nil {
		return nil, fmt.Errorf("copy ordinal program: %w", err)
	}
	return &clone, nil
}

func clonePredicateFootprint(footprint *queryplan.PredicateFootprint) (*queryplan.PredicateFootprint, error) {
	if footprint == nil {
		return nil, nil
	}
	var clone queryplan.PredicateFootprint
	if err := jsonClone(footprint, &clone); err != nil {
		return nil, fmt.Errorf("copy predicate footprint: %w", err)
	}
	return &clone, nil
}

func jsonClone(from, into any) error {
	encoded, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, into)
}

// domainDigest is the one hashing construction this package uses.
func domainDigest(domain string, value any) (string, error) {
	canonical, err := approval.CanonicalJSON(value)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s: %w", domain, err)
	}
	hash := sha256.New()
	hash.Write([]byte(domain + "\x00"))
	hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// textDigest hashes a free-text identity so it can be bound without being
// carried. A registry revision is an operational string, not evidence.
func textDigest(domain, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	return domainDigest(domain, value)
}

func canonicalStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'f':
		default:
			return false
		}
	}
	return true
}

func shortDigest(digest string) string {
	if len(digest) < 12 {
		return digest
	}
	return digest[:12]
}
