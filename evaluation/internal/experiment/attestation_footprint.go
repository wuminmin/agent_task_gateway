package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/approval"
)

// AttestationFootprintVersion identifies the qualified PostgreSQL-internal
// Attestation footprint.
//
// The footprint exists because one class in the v3 closed world is not derivable
// from TaskGate source at all. The column and view-definition attestations are
// Connector constants executed once per ExpectedSchema entry, so the shared
// builder settles their multiplicity. The statements PostgreSQL issues inside
// pg_get_viewdef are emitted by the server, are only observable because
// track='all' counts nested statements, and no reading of TaskGate source
// establishes how many there are or what shape they take. They have to be
// measured.
//
// Version 2 supersedes the v1 shape, which was never qualified and produced no
// evidence. v1 carried two scopes and reduced the measurement to a single
// integer. Both were wrong:
//
//   - two scopes cannot distinguish the Connector's construction attestation
//     from an explicit pool attestation, nor Connector.Query's transaction from
//     Connector.QueryPairStream's. The Artifact paired path must not consume a
//     footprint measured only through Query.
//   - a single integer cannot detect a same-total substitution of one internal
//     statement by another. The qualified multiset of structural keys is carried
//     end to end instead, and every key is validated separately.
const AttestationFootprintVersion = "taskgate-final-v5-attestation-footprint-v2"

// attestationFootprintDomain domain-separates the footprint digest.
const attestationFootprintDomain = "TASKGATE-FINAL-V5-ATTESTATION-FOOTPRINT-V2"

// AttestationScope is one execution scope an Attestation runs in.
//
// The four are qualified separately and never merged. They reach the same Go
// code, but calling one function is not evidence that PostgreSQL emits the same
// internal statements: transaction state, snapshot isolation and plan caching
// are properties of the server, not of the caller.
type AttestationScope string

const (
	// AttestationScopeConstructorOrColdPool is the attestation dataconnector.New
	// performs while opening the pool. On a fresh PostgreSQL deployment it is
	// also the first relevant access, which is the only sense in which any
	// measurement here is "cold" -- resetting pg_stat_statements does not make a
	// server cold, and nothing in this contract claims it does.
	AttestationScopeConstructorOrColdPool AttestationScope = "constructor_or_cold_pool"
	// AttestationScopeExplicitPreflightPool is Connector.Attestation against the
	// pool, outside any transaction. This is the scope the control plan's
	// preflight dimension consumes.
	AttestationScopeExplicitPreflightPool AttestationScope = "explicit_preflight_pool"
	// AttestationScopeSingleQueryTransaction is the attestation inside a
	// Connector.Query transaction.
	AttestationScopeSingleQueryTransaction AttestationScope = "single_query_transaction"
	// AttestationScopePairedQueryTransaction is the attestation inside a
	// Connector.QueryPairStream transaction, which is the path every Artifact
	// paired-novel operation takes.
	AttestationScopePairedQueryTransaction AttestationScope = "paired_query_transaction"
)

// AttestationScopes is the closed set of scopes, in canonical order.
func AttestationScopes() []AttestationScope {
	return []AttestationScope{
		AttestationScopeConstructorOrColdPool,
		AttestationScopeExplicitPreflightPool,
		AttestationScopeSingleQueryTransaction,
		AttestationScopePairedQueryTransaction,
	}
}

// PostgreSQLRuntimeIdentity is the complete identity of the server a footprint
// was qualified against.
//
// Every field is here because one of them alone is insufficient. A tag is
// mutable. A RepoDigest binds the registry content but not what is running. A
// local image ID is deployment-local and means nothing to a reviewer elsewhere.
// A running container's image ID proves what actually served the measurement but
// not where it came from. Platform matters because the same digest can be a
// multi-architecture index.
type PostgreSQLRuntimeIdentity struct {
	// ImageReference is the immutable source reference the deployment names,
	// for example postgres@sha256:...
	ImageReference string `json:"image_reference"`
	// RepoDigest is the registry content digest.
	RepoDigest string `json:"repo_digest"`
	// LocalImageID is the local content-addressed image ID.
	LocalImageID string `json:"local_image_id"`
	// ContainerImageID is the image ID the running container was created from.
	ContainerImageID string `json:"container_image_id"`
	// Platform is the resolved os/architecture.
	Platform string `json:"platform"`
}

// Validate rejects an identity that leaves the server ambiguous.
func (identity PostgreSQLRuntimeIdentity) Validate() error {
	if err := requireImmutableReference(identity.ImageReference); err != nil {
		return fmt.Errorf("image_reference: %w", err)
	}
	if err := requireImmutableReference(identity.RepoDigest); err != nil {
		return fmt.Errorf("repo_digest: %w", err)
	}
	if err := requireImageID(identity.LocalImageID); err != nil {
		return fmt.Errorf("local_image_id: %w", err)
	}
	if err := requireImageID(identity.ContainerImageID); err != nil {
		return fmt.Errorf("container_image_id: %w", err)
	}
	// The running container must have been created from the image the reference
	// resolved to. A mismatch means the deployment is not running what it names.
	if identity.ContainerImageID != identity.LocalImageID {
		return fmt.Errorf("the running container was created from %s but the named image resolves to %s",
			shortImageID(identity.ContainerImageID), shortImageID(identity.LocalImageID))
	}
	if strings.TrimSpace(identity.Platform) == "" {
		return errors.New("platform is empty; the same digest can be a multi-architecture index")
	}
	return nil
}

// requireImmutableReference accepts only a digest-pinned reference. A tag is
// rejected: the whole point of the binding is that the image cannot be swapped
// underneath a qualified measurement.
func requireImmutableReference(reference string) error {
	_, digest, found := strings.Cut(reference, "@sha256:")
	if !found || !validSHA256(digest) {
		return fmt.Errorf("%q is not digest-pinned", reference)
	}
	return nil
}

func requireImageID(imageID string) error {
	digest, found := strings.CutPrefix(imageID, "sha256:")
	if !found || !validSHA256(digest) {
		return fmt.Errorf("%q is not an immutable sha256: image identity", imageID)
	}
	return nil
}

// AttestationInternalEntry is one PostgreSQL-internal structural key and the
// number of times a single Attestation in one scope emits it.
type AttestationInternalEntry struct {
	// StrictASTSHA256 is the structural identity, the same digest space the
	// classifier keys on.
	StrictASTSHA256 string `json:"strict_ast_sha256"`
	// CallsPerAttestation is the measured multiplicity for one Attestation in
	// this scope, against the qualified ExpectedSchema.
	CallsPerAttestation int64 `json:"calls_per_attestation"`
}

// AttestationScopeFootprint is the qualified internal footprint for one scope.
type AttestationScopeFootprint struct {
	Scope AttestationScope `json:"scope"`
	// Internal is the exact multiset of internal statements one Attestation in
	// this scope emits, in canonical digest order. An empty slice is meaningful
	// and legal: it asserts that this scope emits no internal statement at all.
	Internal []AttestationInternalEntry `json:"internal"`
}

// TotalCallsPerAttestation is the number of internal statements one Attestation
// in this scope emits, summed over every structural key.
//
// It is a reporting convenience only. Nothing in acceptance may compare totals:
// a same-total substitution of one key by another is exactly the mutation the
// per-key multiset exists to catch.
func (footprint AttestationScopeFootprint) TotalCallsPerAttestation() int64 {
	var total int64
	for _, entry := range footprint.Internal {
		total += entry.CallsPerAttestation
	}
	return total
}

func (footprint AttestationScopeFootprint) callsByKey() map[string]int64 {
	byKey := make(map[string]int64, len(footprint.Internal))
	for _, entry := range footprint.Internal {
		byKey[entry.StrictASTSHA256] = entry.CallsPerAttestation
	}
	return byKey
}

// InternalExpectation is one expected PostgreSQL-internal structural key and the
// exact number of calls a whole measured operation should produce.
type InternalExpectation struct {
	StrictASTSHA256 string `json:"strict_ast_sha256"`
	// RequiredTopLevel is always false. It is carried explicitly so the
	// expectation is a complete classifier key rather than a bare digest: the
	// same shape observed at top level is a different statement.
	RequiredTopLevel bool  `json:"required_toplevel"`
	Calls            int64 `json:"calls"`
}

// AttestationFootprintV2 is a footprint qualified against exactly one
// ExpectedSchema, one measurement environment and one PostgreSQL runtime
// identity, across all four Attestation scopes.
//
// Every binding is load-bearing rather than descriptive. The footprint scales
// with the ExpectedSchema entry count, so it is invalid for a different
// ExpectedSchema; it depends on nested-statement tracking, so it is invalid
// under a different environment; and it is a property of one server build, so it
// is invalid against a different image. Consumers bind through Require, which
// fails closed on all of them.
type AttestationFootprintV2 struct {
	Version string `json:"version"`
	// ExpectedSchemaDigest is the catalogschema.Result digest of the ordered
	// entry list this footprint was measured against. It must come from
	// catalogschema.Build over the activated Profile Catalog, not from a schema
	// reconstructed by reading live PostgreSQL catalogs: a live read that omits
	// collation identity produces a different digest for the same relation.
	ExpectedSchemaDigest string `json:"expected_schema_digest"`
	// ExpectedSchemaEntries is that same schema's entry count.
	ExpectedSchemaEntries int64                  `json:"expected_schema_entries"`
	Environment           MeasurementEnvironment `json:"measurement_environment"`
	// PostgreSQL is the complete runtime identity of the qualifying server.
	PostgreSQL PostgreSQLRuntimeIdentity `json:"postgresql_runtime_identity"`
	// QualificationID names the retained run that produced these numbers, so the
	// contract points at its own evidence.
	QualificationID string `json:"qualification_id"`
	// Scopes carries exactly one footprint per AttestationScope, in canonical
	// order. All four must be present: a footprint silent about a scope would
	// let that scope's statements go unaccounted.
	Scopes []AttestationScopeFootprint `json:"scopes"`
}

// Validate rejects a footprint that cannot be consumed deterministically.
func (footprint AttestationFootprintV2) Validate() error {
	if footprint.Version != AttestationFootprintVersion {
		return fmt.Errorf("attestation footprint version %q is unsupported", footprint.Version)
	}
	if !validSHA256(footprint.ExpectedSchemaDigest) {
		return errors.New("attestation footprint carries no ExpectedSchema digest")
	}
	if footprint.ExpectedSchemaEntries <= 0 {
		return fmt.Errorf("attestation footprint claims %d ExpectedSchema entries", footprint.ExpectedSchemaEntries)
	}
	if err := footprint.Environment.Validate(); err != nil {
		return err
	}
	if err := footprint.PostgreSQL.Validate(); err != nil {
		return fmt.Errorf("attestation footprint PostgreSQL identity: %w", err)
	}
	if strings.TrimSpace(footprint.QualificationID) == "" {
		return errors.New("attestation footprint names no qualification run")
	}
	required := AttestationScopes()
	if len(footprint.Scopes) != len(required) {
		return fmt.Errorf("attestation footprint qualifies %d scopes, the closed set has %d",
			len(footprint.Scopes), len(required))
	}
	for index, scope := range required {
		if footprint.Scopes[index].Scope != scope {
			return fmt.Errorf("attestation footprint scope %d is %q, canonical order requires %q",
				index, footprint.Scopes[index].Scope, scope)
		}
		if err := footprint.Scopes[index].validate(); err != nil {
			return err
		}
	}
	return nil
}

func (footprint AttestationScopeFootprint) validate() error {
	seen := map[string]bool{}
	for _, entry := range footprint.Internal {
		if !validSHA256(entry.StrictASTSHA256) {
			return fmt.Errorf("attestation footprint scope %s has an internal entry with no strict AST SHA-256", footprint.Scope)
		}
		if seen[entry.StrictASTSHA256] {
			return fmt.Errorf("attestation footprint scope %s lists key %s twice", footprint.Scope, shortDigest(entry.StrictASTSHA256))
		}
		seen[entry.StrictASTSHA256] = true
		// Zero is rejected rather than treated as "absent": a qualification that
		// measured zero calls for a key should not list the key at all, and a
		// listed key with a zero count is more likely a truncated measurement.
		if entry.CallsPerAttestation <= 0 {
			return fmt.Errorf("attestation footprint scope %s lists key %s with %d calls per attestation",
				footprint.Scope, shortDigest(entry.StrictASTSHA256), entry.CallsPerAttestation)
		}
	}
	if !sort.SliceIsSorted(footprint.Internal, func(left, right int) bool {
		return footprint.Internal[left].StrictASTSHA256 < footprint.Internal[right].StrictASTSHA256
	}) {
		return fmt.Errorf("attestation footprint scope %s entries are not in canonical order", footprint.Scope)
	}
	return nil
}

// Scope returns the qualified footprint for one scope.
func (footprint AttestationFootprintV2) Scope(scope AttestationScope) (AttestationScopeFootprint, error) {
	for _, candidate := range footprint.Scopes {
		if candidate.Scope == scope {
			return candidate, nil
		}
	}
	return AttestationScopeFootprint{}, fmt.Errorf("attestation footprint qualifies no scope %q", scope)
}

// ConstructorMatchesExplicitPreflight reports whether the two pool-scope
// observations agree exactly, key by key.
//
// They are kept separate rather than merged. This is retained qualification
// evidence: if a later revision wants one portable preflight scope, this is the
// equality it must require before merging them.
func (footprint AttestationFootprintV2) ConstructorMatchesExplicitPreflight() bool {
	constructor, err := footprint.Scope(AttestationScopeConstructorOrColdPool)
	if err != nil {
		return false
	}
	explicit, err := footprint.Scope(AttestationScopeExplicitPreflightPool)
	if err != nil {
		return false
	}
	return sameFootprint(constructor, explicit)
}

func sameFootprint(left, right AttestationScopeFootprint) bool {
	if len(left.Internal) != len(right.Internal) {
		return false
	}
	for index := range left.Internal {
		if left.Internal[index] != right.Internal[index] {
			return false
		}
	}
	return true
}

// Require binds the footprint to the exact conditions a measurement ran under.
//
// This is the fail-closed gate the whole contract rests on. A footprint
// qualified for one ExpectedSchema says nothing about another, and generalizing
// it -- for instance by scaling the measured per-entry count to a different E --
// would reintroduce exactly the unjustified assumption Stage N1 retired. There is
// no interpolation here on purpose: a different ExpectedSchema requires a new
// qualification run.
func (footprint AttestationFootprintV2) Require(expectedSchemaDigest string, expectedSchemaEntries int64,
	environment MeasurementEnvironment, postgreSQL PostgreSQLRuntimeIdentity) error {
	if err := footprint.Validate(); err != nil {
		return err
	}
	if footprint.ExpectedSchemaDigest != expectedSchemaDigest {
		return fmt.Errorf("attestation footprint was qualified for ExpectedSchema %s, this measurement runs against %s",
			shortDigest(footprint.ExpectedSchemaDigest), shortDigest(expectedSchemaDigest))
	}
	// The digest already implies the count, so a disagreement is not a second
	// binding failing -- it means the two were derived by different code.
	if footprint.ExpectedSchemaEntries != expectedSchemaEntries {
		return fmt.Errorf("attestation footprint carries ExpectedSchema digest %s with %d entries but the measurement reports %d; "+
			"the digest and the count were not derived from one builder",
			shortDigest(footprint.ExpectedSchemaDigest), footprint.ExpectedSchemaEntries, expectedSchemaEntries)
	}
	if footprint.Environment != environment {
		return fmt.Errorf("attestation footprint was qualified under PostgreSQL %d track=%s/track_utility=%s/track_planning=%s, "+
			"this measurement runs under %d track=%s/track_utility=%s/track_planning=%s",
			footprint.Environment.PostgreSQLVersionNum, footprint.Environment.Track,
			footprint.Environment.TrackUtility, footprint.Environment.TrackPlanning,
			environment.PostgreSQLVersionNum, environment.Track,
			environment.TrackUtility, environment.TrackPlanning)
	}
	if footprint.PostgreSQL != postgreSQL {
		return fmt.Errorf("attestation footprint was qualified against PostgreSQL image %s on %s, "+
			"this measurement runs on %s on %s",
			shortImageID(footprint.PostgreSQL.ContainerImageID), footprint.PostgreSQL.Platform,
			shortImageID(postgreSQL.ContainerImageID), postgreSQL.Platform)
	}
	return nil
}

// InternalExpectation is the exact multiset of PostgreSQL-internal statements a
// measured operation should produce, given how many Attestations it makes in
// each consumed scope.
//
// The scopes are multiplied out separately and summed key by key. A single
// aggregate would let one internal statement be replaced by another with no
// change in total, which is precisely the substitution this must catch.
//
// The constructor scope is deliberately not a parameter: it happens once when
// the Gateway opens its pool, which is outside every per-operation measurement
// window. It stays qualified as evidence.
func (footprint AttestationFootprintV2) InternalExpectation(
	preflightAttestations, singleQueryTransactions, pairedQueryTransactions int64) ([]InternalExpectation, error) {
	if err := footprint.Validate(); err != nil {
		return nil, err
	}
	if preflightAttestations < 0 || singleQueryTransactions < 0 || pairedQueryTransactions < 0 {
		return nil, fmt.Errorf("attestation counts must be non-negative, got preflight=%d single=%d paired=%d",
			preflightAttestations, singleQueryTransactions, pairedQueryTransactions)
	}
	totals := map[string]int64{}
	for _, term := range []struct {
		scope      AttestationScope
		multiplier int64
	}{
		{AttestationScopeExplicitPreflightPool, preflightAttestations},
		{AttestationScopeSingleQueryTransaction, singleQueryTransactions},
		{AttestationScopePairedQueryTransaction, pairedQueryTransactions},
	} {
		if term.multiplier == 0 {
			continue
		}
		scope, err := footprint.Scope(term.scope)
		if err != nil {
			return nil, err
		}
		for key, calls := range scope.callsByKey() {
			totals[key] += term.multiplier * calls
		}
	}
	expectation := make([]InternalExpectation, 0, len(totals))
	for key, calls := range totals {
		expectation = append(expectation, InternalExpectation{
			StrictASTSHA256: key, RequiredTopLevel: false, Calls: calls,
		})
	}
	sort.Slice(expectation, func(left, right int) bool {
		return expectation[left].StrictASTSHA256 < expectation[right].StrictASTSHA256
	})
	return expectation, nil
}

// InternalKeys is the union of structural keys any scope emits, in canonical
// order. The classifier must define every one of them, or a qualified internal
// statement would land in the unexpected class.
func (footprint AttestationFootprintV2) InternalKeys() []string {
	seen := map[string]bool{}
	var keys []string
	for _, scope := range footprint.Scopes {
		for _, entry := range scope.Internal {
			if !seen[entry.StrictASTSHA256] {
				seen[entry.StrictASTSHA256] = true
				keys = append(keys, entry.StrictASTSHA256)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// SHA256 is the footprint's canonical domain-separated digest. It enters the
// control plan so a plan cannot be replayed against evidence qualified by a
// different run, and two independent qualification runs are compared by it.
func (footprint AttestationFootprintV2) SHA256() (string, error) {
	if err := footprint.Validate(); err != nil {
		return "", err
	}
	canonical, err := approval.CanonicalJSON(footprint)
	if err != nil {
		return "", fmt.Errorf("canonicalize attestation footprint: %w", err)
	}
	hash := sha256.New()
	hash.Write([]byte(attestationFootprintDomain + "\x00"))
	hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// PortableSHA256 is the digest of everything a second, independent qualification
// run must reproduce exactly.
//
// It deliberately excludes QualificationID, which differs between runs by
// construction, and the deployment-local image IDs, which differ between
// machines. Two isolated qualifications of the same commit against the same
// source image must agree on this digest; SHA256 will not match and is not
// expected to.
func (footprint AttestationFootprintV2) PortableSHA256() (string, error) {
	if err := footprint.Validate(); err != nil {
		return "", err
	}
	portable := footprint
	portable.QualificationID = ""
	portable.PostgreSQL = PostgreSQLRuntimeIdentity{
		ImageReference: footprint.PostgreSQL.ImageReference,
		RepoDigest:     footprint.PostgreSQL.RepoDigest,
		Platform:       footprint.PostgreSQL.Platform,
	}
	canonical, err := approval.CanonicalJSON(portable)
	if err != nil {
		return "", fmt.Errorf("canonicalize portable attestation footprint: %w", err)
	}
	hash := sha256.New()
	hash.Write([]byte(attestationFootprintDomain + "-PORTABLE\x00"))
	hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// NewAttestationFootprintV2 assembles a footprint from measured per-scope
// entries, sorting them into canonical order and validating the result.
func NewAttestationFootprintV2(expectedSchemaDigest string, expectedSchemaEntries int64,
	environment MeasurementEnvironment, postgreSQL PostgreSQLRuntimeIdentity, qualificationID string,
	measured map[AttestationScope][]AttestationInternalEntry) (AttestationFootprintV2, error) {
	scopes := make([]AttestationScopeFootprint, 0, len(AttestationScopes()))
	for _, scope := range AttestationScopes() {
		entries := append([]AttestationInternalEntry(nil), measured[scope]...)
		sort.Slice(entries, func(left, right int) bool {
			return entries[left].StrictASTSHA256 < entries[right].StrictASTSHA256
		})
		scopes = append(scopes, AttestationScopeFootprint{Scope: scope, Internal: entries})
	}
	footprint := AttestationFootprintV2{
		Version: AttestationFootprintVersion, ExpectedSchemaDigest: expectedSchemaDigest,
		ExpectedSchemaEntries: expectedSchemaEntries, Environment: environment,
		PostgreSQL: postgreSQL, QualificationID: qualificationID, Scopes: scopes,
	}
	if err := footprint.Validate(); err != nil {
		return AttestationFootprintV2{}, err
	}
	return footprint, nil
}

func shortDigest(digest string) string {
	if len(digest) < 12 {
		return digest
	}
	return digest[:12]
}

func shortImageID(imageID string) string {
	digest, found := strings.CutPrefix(imageID, "sha256:")
	if !found {
		return imageID
	}
	return "sha256:" + shortDigest(digest)
}
