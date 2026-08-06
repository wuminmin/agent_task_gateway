package physicalquery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// PreparedOperationBindingV1Version identifies the durable preparation binding.
const PreparedOperationBindingV1Version = "taskgate-prepared-operation-binding-v1"

// The domain separators. Each digest covers a different thing, and separating
// them means a value that is legitimate in one position cannot be replayed into
// another.
const (
	preparedOperationDomain = "TASKGATE-PREPARED-OPERATION-BINDING-V1"
	preparedTargetDomain    = "TASKGATE-PREPARED-TARGET-V1"
	compilerIdentityDomain  = "TASKGATE-PREPARATION-COMPILER-IDENTITY-V1"
	fieldListDomain         = "TASKGATE-PREPARATION-FIELD-LIST-V1"
	sidecarGrantsDomain     = "TASKGATE-PREPARATION-SIDECAR-GRANTS-V1"
	viewRevisionDomain      = "TASKGATE-PREPARATION-VIEW-REGISTRY-REVISION-V1"
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
	OrdinalProgramSHA256       string `json:"ordinal_program_sha256,omitempty"`
	DictionarySetSHA256        string `json:"dictionary_set_sha256,omitempty"`
	SidecarGrantsSHA256        string `json:"sidecar_grants_sha256,omitempty"`
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
		"ordinal program":        {binding.OrdinalProgramSHA256, other.OrdinalProgramSHA256},
		"dictionary set":         {binding.DictionarySetSHA256, other.DictionarySetSHA256},
		"sidecar grants":         {binding.SidecarGrantsSHA256, other.SidecarGrantsSHA256},
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

// PreparedOperation is the in-memory result of preparing one operation.
//
// It holds the statements, the projections, the compiled ordinal program, the
// policy grant to authorize with and the verified sidecar bindings. None of it
// may be serialized. What travels is Binding.
type PreparedOperation struct {
	VisibleSQL   string
	CompanionSQL string
	HasCompanion bool

	VisibleFields    []string
	FactFields       []string
	ProvenanceFields []string

	Grouped          bool
	ExpandedEvidence bool

	// PolicyGrant is what Derive authorizes against. It is produced here rather
	// than by the caller, so the Gateway and the finalizer authorize identically.
	PolicyGrant sqlpolicy.Grant
	// SidecarGrants are the ordinal sidecar product grants folded into
	// PolicyGrant, retained separately so their identity can be bound.
	SidecarGrants []sqlpolicy.ProductGrant
	// OrdinalProgram is the compiled program, absent when the profile has none.
	OrdinalProgram *queryplan.OrdinalProgram
	// SourcePublications maps each ordinal source alias to the publication it
	// resolved to, so the Gateway can reattach its own verified SnapshotIndex
	// handles without this package ever holding one.
	SourcePublications map[string]string
	// EstimatedBaseFacts is the prepared estimate, zero when no ordinal program
	// was compiled.
	EstimatedBaseFacts uint64
	// PredicateFootprint is the V5 footprint, absent under other profiles.
	PredicateFootprint *queryplan.PredicateFootprint

	// Binding is the durable half, sealed.
	Binding PreparedOperationBindingV1
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

// Validate rejects a prepared operation that cannot be executed or compared, and
// one whose two halves describe different operations.
func (prepared PreparedOperation) Validate() error {
	if strings.TrimSpace(prepared.VisibleSQL) == "" {
		return errors.New("prepared operation carries no visible statement")
	}
	if prepared.HasCompanion == (strings.TrimSpace(prepared.CompanionSQL) == "") {
		return errors.New("prepared operation's companion statement and its presence flag disagree")
	}
	if len(prepared.VisibleFields) == 0 {
		return errors.New("prepared operation projects no visible field")
	}
	if err := prepared.Binding.Validate(); err != nil {
		return err
	}
	// The binding must describe THIS operation. Without this the halves could
	// drift and every later comparison would be against the wrong one.
	if prepared.Binding.HasCompanion != prepared.HasCompanion {
		return errors.New("prepared operation and its binding disagree about the companion statement")
	}
	if prepared.Binding.Grouped != prepared.Grouped ||
		prepared.Binding.ExpandedEvidence != prepared.ExpandedEvidence {
		return errors.New("prepared operation and its binding disagree about the exposure shape")
	}
	if prepared.Binding.VisibleFieldCount != len(prepared.VisibleFields) ||
		prepared.Binding.FactFieldCount != len(prepared.FactFields) ||
		prepared.Binding.ProvenanceFieldCount != len(prepared.ProvenanceFields) {
		return errors.New("prepared operation and its binding disagree about the projected fields")
	}
	for _, pair := range []struct {
		fields []string
		digest string
	}{
		{prepared.VisibleFields, prepared.Binding.VisibleFieldsSHA256},
		{prepared.FactFields, prepared.Binding.FactFieldsSHA256},
		{prepared.ProvenanceFields, prepared.Binding.ProvenanceFieldsSHA256},
	} {
		derived, err := fieldListSHA256(pair.fields)
		if err != nil {
			return err
		}
		if derived != pair.digest {
			return errors.New("prepared operation's projected fields do not digest to what its binding carries")
		}
	}
	sidecars, err := sidecarGrantsSHA256(prepared.SidecarGrants)
	if err != nil {
		return err
	}
	if sidecars != prepared.Binding.SidecarGrantsSHA256 {
		return errors.New("prepared operation's sidecar grants do not digest to what its binding carries")
	}
	if prepared.Binding.EstimatedBaseFacts != prepared.EstimatedBaseFacts {
		return errors.New("prepared operation and its binding disagree about the estimated base facts")
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
