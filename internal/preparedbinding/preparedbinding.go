// Package preparedbinding holds the durable, SQL-free identity of one prepared
// governed operation.
//
// # Why it is its own package
//
// The type used to live in physicalquery, beside the preparation that produces
// it. That was the natural home while only the preparer and the Gateway read it.
// It stopped being the right home when the Query Execution Binding began to
// carry the whole sealed document rather than its digest: querybinding would
// have had to import physicalquery, and queryreceipt imports querybinding, so
// the receipt would have acquired sqlpolicy, queryplan and viewcompiler in its
// link closure.
//
// That closure is not a matter of taste. A Query Receipt is a description of
// what happened, handed to holders -- a finalizer, an auditor, an evaluation --
// that must not be able to authorize anything. Linking an authorizer into the
// package that defines the description makes "the receipt cannot authorize" a
// claim about what the code happens to call rather than about what it can reach.
//
// Nothing here can authorize, compile or render. Every member is a version, a
// flag, a count or a digest; the package's whole dependency is on a canonical
// JSON encoder. physicalquery aliases these names, so every existing reference
// to physicalquery.PreparedOperationBindingV1 continues to name this type.
package preparedbinding

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/approval"
)

// PreparedOperationBindingV1Version identifies the durable preparation binding.
const PreparedOperationBindingV1Version = "taskgate-prepared-operation-binding-v1"

// The domain separators. Each digest covers a different thing, and separating
// them means a value that is legitimate in one position cannot be replayed into
// another.
const (
	preparedOperationDomain = "TASKGATE-PREPARED-OPERATION-BINDING-V1"
	compilerIdentityDomain  = "TASKGATE-PREPARATION-COMPILER-IDENTITY-V1"
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

// Seal computes the identity's own digest over its members.
func (identity CompilerIdentityV1) Seal() (CompilerIdentityV1, error) {
	identity.SHA256 = ""
	digest, err := DomainDigest(compilerIdentityDomain, identity)
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
		if !ValidSHA256(value) {
			return fmt.Errorf("compiler identity %s is not a lowercase SHA-256", name)
		}
	}
	sealed, err := identity.Seal()
	if err != nil {
		return err
	}
	if sealed.SHA256 != identity.SHA256 {
		return fmt.Errorf("compiler identity digest is %s, its members seal to %s",
			ShortDigest(identity.SHA256), ShortDigest(sealed.SHA256))
	}
	return nil
}

// PreparedOperationBindingV1 is the durable half: what may enter a receipt, a
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

	HasCompanion bool `json:"has_companion"`
	// Grouped and ExpandedEvidence are two independent properties of the compiled
	// shape, and neither alone is the flag the row-limit derivation branches on.
	// UsesExpandedEvidence is that flag; see its comment for why they are stored
	// apart and combined in one place.
	Grouped          bool `json:"grouped"`
	ExpandedEvidence bool `json:"expanded_evidence"`

	VisibleFieldCount    int `json:"visible_field_count"`
	FactFieldCount       int `json:"fact_field_count"`
	ProvenanceFieldCount int `json:"provenance_field_count"`

	// Each digest is present exactly when its count is non-zero. An operation
	// with no exposure accounting projects no fact field at all, so requiring a
	// fact digest of every operation would make the unaccounted shape
	// unrepresentable rather than merely unaccounted.
	VisibleFieldsSHA256    string `json:"visible_fields_sha256"`
	FactFieldsSHA256       string `json:"fact_fields_sha256,omitempty"`
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
	PolicyGrantSHA256 string `json:"policy_grant_sha256"`
	// NormalFormSHA256 is the profile's canonical plan identity: the V2 normal
	// form under V2/V3/V4, the V4 normal form under V5, the algebra normal form
	// for a relational plan, and empty under a profile that computes none.
	//
	// It is not PlanSHA256. That one digests the QueryPlan as submitted, so two
	// plans that differ only in how they were written reach different values;
	// this one is what the exposure ledger and the Query Execution Binding
	// identify a query by, and two plans with one canonical identity share it.
	NormalFormSHA256           string `json:"normal_form_sha256,omitempty"`
	OrdinalProgramSHA256       string `json:"ordinal_program_sha256,omitempty"`
	DictionarySetSHA256        string `json:"dictionary_set_sha256,omitempty"`
	SidecarGrantsSHA256        string `json:"sidecar_grants_sha256,omitempty"`
	SourcePublicationsSHA256   string `json:"source_publications_sha256,omitempty"`
	ViewBindingSHA256          string `json:"view_binding_sha256,omitempty"`
	ViewRegistryRevisionSHA256 string `json:"view_registry_revision_sha256,omitempty"`

	// The semantic View identities, present exactly on a View preparation.
	//
	// These are separate members rather than something read out of
	// ViewBindingSHA256 on the argument that a binding digest "covers" them. That
	// argument is about how the Control Store happens to construct a ViewBinding,
	// which is a different package's invariant and could change without this one
	// noticing; a name containing the word "binding" is not a coverage proof. Each
	// is measured here from the value preparation actually read, so a change to a
	// compiled Artifact, a composed plan, the terminal Product closure or the
	// governance envelope moves the sealed binding whatever the Control Store did.
	ViewArtifactSHA256           string `json:"view_artifact_sha256,omitempty"`
	ViewCompositionSHA256        string `json:"view_composition_sha256,omitempty"`
	TerminalProductClosureSHA256 string `json:"terminal_product_closure_sha256,omitempty"`
	GovernanceEnvelopeSHA256     string `json:"governance_envelope_sha256,omitempty"`

	PredicateFootprintSHA256 string `json:"predicate_footprint_sha256,omitempty"`
	EstimatedBaseFacts       uint64 `json:"estimated_base_facts,omitempty"`

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
	digest, err := DomainDigest(preparedOperationDomain, binding)
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
		"preparation inputs": binding.PreparationInputsSHA256,
		"grant":              binding.GrantSHA256,
		"catalog":            binding.CatalogSHA256,
		"plan":               binding.PlanSHA256,
		"compiler identity":  binding.CompilerIdentitySHA256,
		"policy grant":       binding.PolicyGrantSHA256,
		"visible target":     binding.VisibleTargetSHA256,
		"prepared operation": binding.SHA256,
	} {
		if !ValidSHA256(digest) {
			return fmt.Errorf("prepared operation binding %s digest is not a lowercase SHA-256", name)
		}
	}
	for name, digest := range map[string]string{
		"fact fields":              binding.FactFieldsSHA256,
		"provenance fields":        binding.ProvenanceFieldsSHA256,
		"snapshot binding set":     binding.SnapshotBindingSetSHA256,
		"normal form":              binding.NormalFormSHA256,
		"ordinal program":          binding.OrdinalProgramSHA256,
		"dictionary set":           binding.DictionarySetSHA256,
		"sidecar grants":           binding.SidecarGrantsSHA256,
		"source publications":      binding.SourcePublicationsSHA256,
		"view binding":             binding.ViewBindingSHA256,
		"view registry revision":   binding.ViewRegistryRevisionSHA256,
		"view artifact":            binding.ViewArtifactSHA256,
		"view composition":         binding.ViewCompositionSHA256,
		"terminal product closure": binding.TerminalProductClosureSHA256,
		"governance envelope":      binding.GovernanceEnvelopeSHA256,
		"predicate footprint":      binding.PredicateFootprintSHA256,
		"companion target":         binding.CompanionTargetSHA256,
	} {
		if digest != "" && !ValidSHA256(digest) {
			return fmt.Errorf("prepared operation binding %s digest is not a lowercase SHA-256", name)
		}
	}
	// The four semantic View identities travel together. A binding carrying some
	// of them describes a View preparation that measured only part of what it
	// prepared against, which is worse than one that measured none: it would look
	// like a complete View binding to anything checking for presence.
	viewIdentities := map[string]string{
		"view artifact":            binding.ViewArtifactSHA256,
		"view composition":         binding.ViewCompositionSHA256,
		"terminal product closure": binding.TerminalProductClosureSHA256,
		"governance envelope":      binding.GovernanceEnvelopeSHA256,
	}
	present := 0
	for _, digest := range viewIdentities {
		if digest != "" {
			present++
		}
	}
	if present != 0 && present != len(viewIdentities) {
		var missing []string
		for name, digest := range viewIdentities {
			if digest == "" {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		return fmt.Errorf("prepared operation binding is a semantic View preparation but carries no %s digest",
			strings.Join(missing, ", no "))
	}
	// A View preparation is bound to a View. Measuring the artifact and the
	// governance envelope while naming no binding would leave the whole identity
	// resting on values the Control Store never verified.
	if present != 0 && binding.ViewBindingSHA256 == "" {
		return errors.New("prepared operation binding carries semantic View identities but names no view binding")
	}
	// Presence-coupled in both directions, so a companion cannot be half present.
	if binding.HasCompanion == (binding.CompanionTargetSHA256 == "") {
		return errors.New("prepared operation binding's companion target and its presence flag disagree")
	}
	if binding.VisibleFieldCount <= 0 {
		return fmt.Errorf("prepared operation binding projects %d visible fields", binding.VisibleFieldCount)
	}
	for _, pair := range []struct {
		name   string
		count  int
		digest string
	}{
		{"fact", binding.FactFieldCount, binding.FactFieldsSHA256},
		{"provenance", binding.ProvenanceFieldCount, binding.ProvenanceFieldsSHA256},
	} {
		if (pair.count == 0) != (pair.digest == "") {
			return fmt.Errorf("prepared operation binding's %s field count and digest disagree", pair.name)
		}
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
			ShortDigest(binding.SHA256), ShortDigest(sealed.SHA256))
	}
	return nil
}

// UsesExpandedEvidence is the flag the row-limit derivation branches on: whether
// the companion is authorized for one row more than it is expected to yield, so
// a truncated evidence result is distinguishable from a complete one.
//
// It is a disjunction rather than a member because two different compilations
// need the wider companion. An aggregating or grouping plan does -- the visible
// rows are groups and the evidence rows are the facts underneath them -- and so
// does a relational compilation that declares it outright. Storing the two
// causes separately keeps the binding a description of what was compiled;
// combining them here, once, keeps everything that reads the flag reading the
// same function.
//
// Everything that derives, reproduces or checks a companion limit must ask this
// rather than read ExpandedEvidence. The Gateway computed the disjunction while
// QueryExecutionBindingV2 read the member, which meant a grouped operation
// derived its limits one way and described them another; it went unnoticed
// because the only profile that signed a binding also set the member.
func (binding PreparedOperationBindingV1) UsesExpandedEvidence() bool {
	return binding.Grouped || binding.ExpandedEvidence
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
	differences := binding.differences(other)
	if len(differences) == 0 {
		// The sealed digests differ but no named member does, which means the
		// binding has a member this comparison does not cover. That is a defect
		// here, and saying so beats reporting agreement.
		return fmt.Errorf("two preparations seal to %s and %s but disagree on no named member; "+
			"PreparedOperationBindingV1 has a member RequireSame does not compare",
			ShortDigest(binding.SHA256), ShortDigest(other.SHA256))
	}
	return &PreparedOperationMismatchError{members: differences}
}

// PreparedOperationMember is one closed member of PreparedOperationBindingV1.
//
// It is numeric rather than a string alias so arbitrary text cannot become a
// member name. Durable encoders still reject values whose Code is empty. Code
// is the stable wire spelling; String retains the operator-facing spelling.
type PreparedOperationMember uint8

const (
	PreparedMemberHasCompanion PreparedOperationMember = iota + 1
	PreparedMemberGrouped
	PreparedMemberExpandedEvidence
	PreparedMemberVisibleFieldCount
	PreparedMemberFactFieldCount
	PreparedMemberProvenanceFieldCount
	PreparedMemberVisibleFields
	PreparedMemberFactFields
	PreparedMemberProvenanceFields
	PreparedMemberPreparationInputs
	PreparedMemberGrant
	PreparedMemberCatalog
	PreparedMemberSnapshotBindingSet
	PreparedMemberPlan
	PreparedMemberCompilerIdentity
	PreparedMemberPolicyGrant
	PreparedMemberNormalForm
	PreparedMemberOrdinalProgram
	PreparedMemberDictionarySet
	PreparedMemberSidecarGrants
	PreparedMemberSourcePublications
	PreparedMemberViewBinding
	PreparedMemberViewRegistryRevision
	PreparedMemberViewArtifact
	PreparedMemberViewComposition
	PreparedMemberTerminalProductClosure
	PreparedMemberGovernanceEnvelope
	PreparedMemberPredicateFootprint
	PreparedMemberEstimatedBaseFacts
	PreparedMemberVisibleTarget
	PreparedMemberCompanionTarget
	preparedOperationMemberCount
)

var preparedOperationMembers = [...]PreparedOperationMember{
	PreparedMemberHasCompanion,
	PreparedMemberGrouped,
	PreparedMemberExpandedEvidence,
	PreparedMemberVisibleFieldCount,
	PreparedMemberFactFieldCount,
	PreparedMemberProvenanceFieldCount,
	PreparedMemberVisibleFields,
	PreparedMemberFactFields,
	PreparedMemberProvenanceFields,
	PreparedMemberPreparationInputs,
	PreparedMemberGrant,
	PreparedMemberCatalog,
	PreparedMemberSnapshotBindingSet,
	PreparedMemberPlan,
	PreparedMemberCompilerIdentity,
	PreparedMemberPolicyGrant,
	PreparedMemberNormalForm,
	PreparedMemberOrdinalProgram,
	PreparedMemberDictionarySet,
	PreparedMemberSidecarGrants,
	PreparedMemberSourcePublications,
	PreparedMemberViewBinding,
	PreparedMemberViewRegistryRevision,
	PreparedMemberViewArtifact,
	PreparedMemberViewComposition,
	PreparedMemberTerminalProductClosure,
	PreparedMemberGovernanceEnvelope,
	PreparedMemberPredicateFootprint,
	PreparedMemberEstimatedBaseFacts,
	PreparedMemberVisibleTarget,
	PreparedMemberCompanionTarget,
}

const preparedOperationMemberCardinality = int(preparedOperationMemberCount) - 1

// Both directions are intentional: adding an enum without adding it to the
// closed list, or adding a list entry without an enum, fails compilation.
var _ [preparedOperationMemberCardinality - len(preparedOperationMembers)]struct{}
var _ [len(preparedOperationMembers) - preparedOperationMemberCardinality]struct{}

// PreparedOperationMembers returns the complete closed member enumeration.
func PreparedOperationMembers() []PreparedOperationMember {
	return append([]PreparedOperationMember(nil), preparedOperationMembers[:]...)
}

// Code is the stable lowercase wire spelling used by rejection evidence.
func (member PreparedOperationMember) Code() string {
	switch member {
	case PreparedMemberHasCompanion:
		return "has_companion"
	case PreparedMemberGrouped:
		return "grouped"
	case PreparedMemberExpandedEvidence:
		return "expanded_evidence"
	case PreparedMemberVisibleFieldCount:
		return "visible_field_count"
	case PreparedMemberFactFieldCount:
		return "fact_field_count"
	case PreparedMemberProvenanceFieldCount:
		return "provenance_field_count"
	case PreparedMemberVisibleFields:
		return "visible_fields"
	case PreparedMemberFactFields:
		return "fact_fields"
	case PreparedMemberProvenanceFields:
		return "provenance_fields"
	case PreparedMemberPreparationInputs:
		return "preparation_inputs"
	case PreparedMemberGrant:
		return "grant"
	case PreparedMemberCatalog:
		return "catalog"
	case PreparedMemberSnapshotBindingSet:
		return "snapshot_binding_set"
	case PreparedMemberPlan:
		return "plan"
	case PreparedMemberCompilerIdentity:
		return "compiler_identity"
	case PreparedMemberPolicyGrant:
		return "policy_grant"
	case PreparedMemberNormalForm:
		return "normal_form"
	case PreparedMemberOrdinalProgram:
		return "ordinal_program"
	case PreparedMemberDictionarySet:
		return "dictionary_set"
	case PreparedMemberSidecarGrants:
		return "sidecar_grants"
	case PreparedMemberSourcePublications:
		return "source_publications"
	case PreparedMemberViewBinding:
		return "view_binding"
	case PreparedMemberViewRegistryRevision:
		return "view_registry_revision"
	case PreparedMemberViewArtifact:
		return "view_artifact"
	case PreparedMemberViewComposition:
		return "view_composition"
	case PreparedMemberTerminalProductClosure:
		return "terminal_product_closure"
	case PreparedMemberGovernanceEnvelope:
		return "governance_envelope"
	case PreparedMemberPredicateFootprint:
		return "predicate_footprint"
	case PreparedMemberEstimatedBaseFacts:
		return "estimated_base_facts"
	case PreparedMemberVisibleTarget:
		return "visible_target"
	case PreparedMemberCompanionTarget:
		return "companion_target"
	default:
		return ""
	}
}

func (member PreparedOperationMember) String() string {
	return strings.ReplaceAll(member.Code(), "_", " ")
}

// PreparedOperationMismatchError is the typed form of a whole-binding
// mismatch. Members is the one list RequireSame itself computes; finalizer
// evidence therefore never parses or restates an error string to learn what
// differed.
type PreparedOperationMismatchError struct {
	members []PreparedOperationMember
}

func (mismatch *PreparedOperationMismatchError) Error() string {
	if mismatch == nil {
		return "two independent preparations disagree"
	}
	return fmt.Sprintf("two independent preparations of one operation disagree on %v", mismatch.members)
}

// Members returns every differing member in stable order.
func (mismatch *PreparedOperationMismatchError) Members() []PreparedOperationMember {
	if mismatch == nil {
		return nil
	}
	return append([]PreparedOperationMember(nil), mismatch.members...)
}

// differences names every member two bindings disagree on, in a stable order.
//
// It is separate from RequireSame so the Query Execution Binding can report the
// same member names when the finalizer's preparation disagrees with the signed
// one, rather than growing a second, drifting copy of this list.
func (binding PreparedOperationBindingV1) differences(other PreparedOperationBindingV1) []PreparedOperationMember {
	var differences []PreparedOperationMember
	for member, pair := range map[PreparedOperationMember][2]any{
		PreparedMemberHasCompanion:           {binding.HasCompanion, other.HasCompanion},
		PreparedMemberGrouped:                {binding.Grouped, other.Grouped},
		PreparedMemberExpandedEvidence:       {binding.ExpandedEvidence, other.ExpandedEvidence},
		PreparedMemberVisibleFieldCount:      {binding.VisibleFieldCount, other.VisibleFieldCount},
		PreparedMemberFactFieldCount:         {binding.FactFieldCount, other.FactFieldCount},
		PreparedMemberProvenanceFieldCount:   {binding.ProvenanceFieldCount, other.ProvenanceFieldCount},
		PreparedMemberVisibleFields:          {binding.VisibleFieldsSHA256, other.VisibleFieldsSHA256},
		PreparedMemberFactFields:             {binding.FactFieldsSHA256, other.FactFieldsSHA256},
		PreparedMemberProvenanceFields:       {binding.ProvenanceFieldsSHA256, other.ProvenanceFieldsSHA256},
		PreparedMemberPreparationInputs:      {binding.PreparationInputsSHA256, other.PreparationInputsSHA256},
		PreparedMemberGrant:                  {binding.GrantSHA256, other.GrantSHA256},
		PreparedMemberCatalog:                {binding.CatalogSHA256, other.CatalogSHA256},
		PreparedMemberSnapshotBindingSet:     {binding.SnapshotBindingSetSHA256, other.SnapshotBindingSetSHA256},
		PreparedMemberPlan:                   {binding.PlanSHA256, other.PlanSHA256},
		PreparedMemberCompilerIdentity:       {binding.CompilerIdentitySHA256, other.CompilerIdentitySHA256},
		PreparedMemberPolicyGrant:            {binding.PolicyGrantSHA256, other.PolicyGrantSHA256},
		PreparedMemberNormalForm:             {binding.NormalFormSHA256, other.NormalFormSHA256},
		PreparedMemberOrdinalProgram:         {binding.OrdinalProgramSHA256, other.OrdinalProgramSHA256},
		PreparedMemberDictionarySet:          {binding.DictionarySetSHA256, other.DictionarySetSHA256},
		PreparedMemberSidecarGrants:          {binding.SidecarGrantsSHA256, other.SidecarGrantsSHA256},
		PreparedMemberSourcePublications:     {binding.SourcePublicationsSHA256, other.SourcePublicationsSHA256},
		PreparedMemberViewBinding:            {binding.ViewBindingSHA256, other.ViewBindingSHA256},
		PreparedMemberViewRegistryRevision:   {binding.ViewRegistryRevisionSHA256, other.ViewRegistryRevisionSHA256},
		PreparedMemberViewArtifact:           {binding.ViewArtifactSHA256, other.ViewArtifactSHA256},
		PreparedMemberViewComposition:        {binding.ViewCompositionSHA256, other.ViewCompositionSHA256},
		PreparedMemberTerminalProductClosure: {binding.TerminalProductClosureSHA256, other.TerminalProductClosureSHA256},
		PreparedMemberGovernanceEnvelope:     {binding.GovernanceEnvelopeSHA256, other.GovernanceEnvelopeSHA256},
		PreparedMemberPredicateFootprint:     {binding.PredicateFootprintSHA256, other.PredicateFootprintSHA256},
		PreparedMemberEstimatedBaseFacts:     {binding.EstimatedBaseFacts, other.EstimatedBaseFacts},
		PreparedMemberVisibleTarget:          {binding.VisibleTargetSHA256, other.VisibleTargetSHA256},
		PreparedMemberCompanionTarget:        {binding.CompanionTargetSHA256, other.CompanionTargetSHA256},
	} {
		if pair[0] != pair[1] {
			differences = append(differences, member)
		}
	}
	sort.Slice(differences, func(left, right int) bool {
		return differences[left].Code() < differences[right].Code()
	})
	return differences
}

// DomainDigest is the one hashing construction this binding and its preparer
// use. physicalquery delegates to it rather than keeping a second copy, because
// two constructions that were meant to be identical are exactly the thing that
// drifts.
func DomainDigest(domain string, value any) (string, error) {
	canonical, err := approval.CanonicalJSON(value)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s: %w", domain, err)
	}
	hash := sha256.New()
	hash.Write([]byte(domain + "\x00"))
	hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ValidSHA256 reports whether a value is a lowercase hex SHA-256.
func ValidSHA256(value string) bool {
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

// ShortDigest truncates a digest for a failure message.
func ShortDigest(digest string) string {
	if len(digest) < 12 {
		return digest
	}
	return digest[:12]
}
