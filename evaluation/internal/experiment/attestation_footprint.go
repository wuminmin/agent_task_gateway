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
// Connector constants executed once per ExpectedSchema entry, so their
// multiplicity follows from the shared builder. The nested lookup PostgreSQL
// performs inside pg_get_viewdef is emitted by the server, is only observable
// because track='all' counts nested statements, and no amount of source reading
// establishes how many times it runs. It has to be measured.
//
// Stage N1 measured it and found one internal key at one call per ExpectedSchema
// entry per Attestation, identical across scope, relation kind and warmth. That
// record is retained as diagnosis but is NOT this contract: it never recorded the
// ExpectedSchema it qualified (its expected_schema_digest field was declared and
// left empty), it encoded the entry count in a free-text relation_kind label
// rather than an integer, and it bound no PostgreSQL image. A footprint whose
// central claim is "valid only for the ExpectedSchema it was qualified against"
// cannot be carried by a record that does not name that ExpectedSchema.
const AttestationFootprintVersion = "taskgate-final-v5-attestation-footprint-v1"

// attestationFootprintDomain domain-separates the footprint digest.
const attestationFootprintDomain = "TASKGATE-FINAL-V5-ATTESTATION-FOOTPRINT-V1"

// AttestationScope is the execution scope one Attestation runs in. The two are
// qualified separately and never merged: Connector.Attestation against the pool
// and the same Go function inside a transaction are the same call site, but
// calling one function is not evidence that PostgreSQL emits the same internal
// statements in both. Stage N1 measured them equal; equality is the measurement,
// not the model.
type AttestationScope string

const (
	// AttestationScopePreflight is Connector.Attestation against the pool,
	// outside any transaction.
	AttestationScopePreflight AttestationScope = "preflight"
	// AttestationScopeTransactional is the attestation performed inside a
	// Connector.Query or Connector.QueryPairStream transaction.
	AttestationScopeTransactional AttestationScope = "transactional"
)

// AttestationScopes is the closed set of scopes, in canonical order.
func AttestationScopes() []AttestationScope {
	return []AttestationScope{AttestationScopePreflight, AttestationScopeTransactional}
}

// AttestationInternalEntry is one PostgreSQL-internal structural key and the
// number of times a single Attestation emits it.
//
// The count is per Attestation, not per trial. A trial that constructs a
// Connector performs two Attestations -- dataconnector.New attests once at
// construction -- so a raw trial delta divided by anything other than the
// observed Attestation count is the wrong number.
type AttestationInternalEntry struct {
	// StrictASTSHA256 is the structural identity, the same digest space the
	// classifier keys on.
	StrictASTSHA256 string `json:"strict_ast_sha256"`
	// CallsPerAttestation is the measured multiplicity for one Attestation
	// against the qualified ExpectedSchema.
	CallsPerAttestation int64 `json:"calls_per_attestation"`
}

// AttestationScopeFootprint is the qualified internal footprint for one scope.
type AttestationScopeFootprint struct {
	Scope AttestationScope `json:"scope"`
	// Internal is the exact multiset of internal statements one Attestation
	// emits, in canonical digest order. An empty slice is meaningful and legal:
	// it asserts that this scope emits no internal statement at all.
	Internal []AttestationInternalEntry `json:"internal"`
}

// TotalCallsPerAttestation is the number of internal statements one Attestation
// in this scope emits, summed over every structural key.
func (footprint AttestationScopeFootprint) TotalCallsPerAttestation() int64 {
	var total int64
	for _, entry := range footprint.Internal {
		total += entry.CallsPerAttestation
	}
	return total
}

// AttestationFootprintV1 is a footprint qualified against exactly one
// ExpectedSchema, one measurement environment and one PostgreSQL image.
//
// Every one of those bindings is load-bearing rather than descriptive. The
// footprint scales with the ExpectedSchema entry count, so it is invalid for a
// different ExpectedSchema; it depends on nested-statement tracking, so it is
// invalid under a different environment; and it is a property of one server
// implementation, so it is invalid against a different image. Consumers bind
// through Require, which fails closed on any of the four.
type AttestationFootprintV1 struct {
	Version string `json:"version"`
	// ExpectedSchemaDigest is the catalogschema.Result digest of the ordered
	// entry list this footprint was measured against.
	ExpectedSchemaDigest string `json:"expected_schema_digest"`
	// ExpectedSchemaEntries is that same schema's entry count, recorded as an
	// integer so a consumer never has to parse it out of a label.
	ExpectedSchemaEntries int64                  `json:"expected_schema_entries"`
	Environment           MeasurementEnvironment `json:"measurement_environment"`
	// PostgreSQLImageID is the immutable image the qualification ran against.
	PostgreSQLImageID string `json:"postgresql_image_id"`
	// QualificationID names the retained diagnosis run that produced these
	// numbers, so the contract points at its own evidence.
	QualificationID string `json:"qualification_id"`
	// Scopes carries exactly one footprint per AttestationScope, in canonical
	// order. Both must be present: a footprint silent about a scope would let
	// that scope's statements go unaccounted.
	Scopes []AttestationScopeFootprint `json:"scopes"`
}

// Validate rejects a footprint that cannot be consumed deterministically.
func (footprint AttestationFootprintV1) Validate() error {
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
	if err := validateImageID(footprint.PostgreSQLImageID); err != nil {
		return fmt.Errorf("attestation footprint PostgreSQL image: %w", err)
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
			return fmt.Errorf("attestation footprint scope %s lists key %s twice", footprint.Scope, entry.StrictASTSHA256[:12])
		}
		seen[entry.StrictASTSHA256] = true
		// Zero is rejected rather than treated as "absent": a qualification that
		// measured zero calls for a key should not list the key at all, and a
		// listed key with a zero count is more likely a truncated measurement.
		if entry.CallsPerAttestation <= 0 {
			return fmt.Errorf("attestation footprint scope %s lists key %s with %d calls per attestation",
				footprint.Scope, entry.StrictASTSHA256[:12], entry.CallsPerAttestation)
		}
	}
	if !sort.SliceIsSorted(footprint.Internal, func(left, right int) bool {
		return footprint.Internal[left].StrictASTSHA256 < footprint.Internal[right].StrictASTSHA256
	}) {
		return fmt.Errorf("attestation footprint scope %s entries are not in canonical order", footprint.Scope)
	}
	return nil
}

// validateImageID accepts the immutable content-addressed image identity Docker
// reports. A tag is deliberately not accepted: tags are mutable, and the whole
// point of the binding is that the image cannot be swapped underneath a
// qualified measurement.
func validateImageID(imageID string) error {
	digest, found := strings.CutPrefix(imageID, "sha256:")
	if !found {
		return fmt.Errorf("%q is not an immutable sha256: image identity", imageID)
	}
	if !validSHA256(digest) {
		return fmt.Errorf("%q is not an immutable sha256: image identity", imageID)
	}
	return nil
}

// Scope returns the qualified footprint for one scope.
func (footprint AttestationFootprintV1) Scope(scope AttestationScope) (AttestationScopeFootprint, error) {
	for _, candidate := range footprint.Scopes {
		if candidate.Scope == scope {
			return candidate, nil
		}
	}
	return AttestationScopeFootprint{}, fmt.Errorf("attestation footprint qualifies no scope %q", scope)
}

// Require binds the footprint to the exact conditions a measurement ran under.
//
// This is the fail-closed gate the whole contract rests on. A footprint
// qualified for one ExpectedSchema says nothing about another, and generalizing
// it -- for instance by scaling the measured per-entry count to a different E --
// would reintroduce exactly the unjustified assumption Stage N1 retired. There is
// no interpolation here on purpose: a different ExpectedSchema requires a new
// qualification run.
func (footprint AttestationFootprintV1) Require(expectedSchemaDigest string, expectedSchemaEntries int64,
	environment MeasurementEnvironment, postgreSQLImageID string) error {
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
	if footprint.PostgreSQLImageID != postgreSQLImageID {
		return fmt.Errorf("attestation footprint was qualified against PostgreSQL image %s, this measurement runs on %s",
			shortImageID(footprint.PostgreSQLImageID), shortImageID(postgreSQLImageID))
	}
	return nil
}

// InternalCalls is the number of internal statements a run performs, given how
// many Attestations it makes in each scope.
//
// The two scopes are multiplied out separately rather than against one combined
// attestation count. They happen to carry equal footprints in the qualified
// deployment, but nothing in this computation depends on that.
func (footprint AttestationFootprintV1) InternalCalls(preflightAttestations, transactionalAttestations int64) (int64, error) {
	if preflightAttestations < 0 || transactionalAttestations < 0 {
		return 0, fmt.Errorf("attestation counts must be non-negative, got preflight=%d transactional=%d",
			preflightAttestations, transactionalAttestations)
	}
	preflight, err := footprint.Scope(AttestationScopePreflight)
	if err != nil {
		return 0, err
	}
	transactional, err := footprint.Scope(AttestationScopeTransactional)
	if err != nil {
		return 0, err
	}
	return preflightAttestations*preflight.TotalCallsPerAttestation() +
		transactionalAttestations*transactional.TotalCallsPerAttestation(), nil
}

// InternalKeys is the union of structural keys any scope emits, in canonical
// order. The classifier must define every one of them, or a qualified internal
// statement would land in the unexpected class.
func (footprint AttestationFootprintV1) InternalKeys() []string {
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
// control plan so that a plan cannot be replayed against evidence qualified by a
// different run.
func (footprint AttestationFootprintV1) SHA256() (string, error) {
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

// NewAttestationFootprintV1 assembles a footprint from measured per-scope
// entries, sorting them into canonical order and validating the result.
func NewAttestationFootprintV1(expectedSchemaDigest string, expectedSchemaEntries int64,
	environment MeasurementEnvironment, postgreSQLImageID, qualificationID string,
	measured map[AttestationScope][]AttestationInternalEntry) (AttestationFootprintV1, error) {
	scopes := make([]AttestationScopeFootprint, 0, len(AttestationScopes()))
	for _, scope := range AttestationScopes() {
		entries := append([]AttestationInternalEntry(nil), measured[scope]...)
		sort.Slice(entries, func(left, right int) bool {
			return entries[left].StrictASTSHA256 < entries[right].StrictASTSHA256
		})
		scopes = append(scopes, AttestationScopeFootprint{Scope: scope, Internal: entries})
	}
	footprint := AttestationFootprintV1{
		Version: AttestationFootprintVersion, ExpectedSchemaDigest: expectedSchemaDigest,
		ExpectedSchemaEntries: expectedSchemaEntries, Environment: environment,
		PostgreSQLImageID: postgreSQLImageID, QualificationID: qualificationID, Scopes: scopes,
	}
	if err := footprint.Validate(); err != nil {
		return AttestationFootprintV1{}, err
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
