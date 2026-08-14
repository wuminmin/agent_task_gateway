// Package exposure defines TaskGate's task-bound data exposure semantics.
package exposure

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

var ErrInvalid = errors.New("invalid exposure value")

const (
	RowExistenceField = "$row"
	ProfileV1         = "taskgate-exposure-v1"
	ProfileV2         = "taskgate-exposure-v2"
	ProfileV3         = "taskgate-exposure-v3"
	// ProfileV4 changes the physical representation of an observation, not the
	// semantic identity of its facts. Release and influence facts retain their
	// V2 payloads and outcome facts retain their V3 payloads so a V4 bitmap can
	// be decoded and compared with the existing exact FactSet model.
	ProfileV4 = "taskgate-exposure-v4"
	// ProfileV5 accounts a successful query as the exact set of canonical
	// caller-controlled predicate atoms plus one composite outcome.  V5 has a
	// new hash domain; V1--V4 payloads and hashes remain byte-for-byte stable.
	ProfileV5            = "taskgate-exposure-v5"
	factDomainV2         = "TASKGATE-FACT-V2\x00"
	factDomainV3         = "TASKGATE-FACT-V3\x00"
	factDomainV5         = "TASKGATE-FACT-V5\x00"
	predicateSetDomainV1 = "TASKGATE-PREDICATE-SET-V1\x00"
)

// FactKind identifies the semantic category of a V2 fact. The hash is only an
// index for this object; it is not the definition of the object.
type FactKind string

const (
	FactBaseRow          FactKind = "base-row"
	FactBaseCell         FactKind = "base-cell"
	FactDerived          FactKind = "derived"
	FactOutcome          FactKind = "outcome"
	FactPredicateAtom    FactKind = "predicate-atom"
	FactCompositeOutcome FactKind = "composite-outcome"
)

const PredicateFootprintVersion = "taskgate-predicate-footprint-v1"

// PredicateAtomFactV5 is the public construction payload for one tested
// caller-controlled literal predicate.  It deliberately contains no truth
// value: an atom records that a condition was tested, not whether it was true,
// false, or unknown for any individual row.
type PredicateAtomFactV5 struct {
	ProfileVersion           string
	AtomizerVersion          string
	PredicateContextSHA256   string
	SemanticProductID        string
	StableRole               string
	PublicFieldID            string
	ResolvedExpressionSHA256 string
	SQLType                  string
	CollationName            string
	CollationVersion         string
	Operator                 string
	CanonicalLiteral         string
}

// CompositeOutcomeFactV5 binds a complete normalized query and committed
// result observation to the exact predicate footprint settled with it.
type CompositeOutcomeFactV5 struct {
	ProfileVersion          string
	QueryNormalFormVersion  string
	QueryNormalFormSHA256   string
	ResultObservationSHA256 string
	VisibleRows             int64
	PredicateProfileVersion string
	PredicateContextSHA256  string
	PredicateSetSHA256      string
	PredicateAtomCount      int64
}

// SnapshotBinding is one member of an immutable derived-query snapshot
// bundle. Bindings are sorted by SourceNamespace before canonical encoding.
type SnapshotBinding struct {
	SourceNamespace string `json:"source_namespace"`
	Snapshot        string `json:"snapshot"`
}

// FactID retains the V1 fields for persisted-ledger compatibility and adds a
// disjoint tagged representation for taskgate-exposure-v2. A V2 fact never
// populates Product or ValueVersion, so the two profiles cannot be mixed by
// construction.
type FactID struct {
	// V1 identity. Keep field order stable: legacy hashes were JSON hashes.
	Product      string `json:"product,omitempty"`
	Snapshot     string `json:"snapshot,omitempty"`
	EntityKey    string `json:"entity_key,omitempty"`
	Field        string `json:"field,omitempty"`
	ValueVersion string `json:"value_version,omitempty"`

	// V2 semantic payload.
	Profile              string            `json:"profile,omitempty"`
	Kind                 FactKind          `json:"kind,omitempty"`
	SourceNamespace      string            `json:"source_namespace,omitempty"`
	SQLType              string            `json:"sql_type,omitempty"`
	CanonicalValue       string            `json:"canonical_value,omitempty"`
	SnapshotBundle       []SnapshotBinding `json:"snapshot_bundle,omitempty"`
	OutputRowKey         string            `json:"output_row_key,omitempty"`
	NormalizedExpression string            `json:"normalized_expression,omitempty"`
	WitnessCommitment    string            `json:"witness_commitment,omitempty"`

	// V3 query-outcome payload. Release and positive-output dependency facts
	// intentionally retain their V2 identity; only proposition-bearing outcome
	// facts use this representation.
	QueryNormalFormVersion string `json:"query_normal_form_version,omitempty"`
	QueryNormalFormSHA256  string `json:"query_normal_form_sha256,omitempty"`
	OutcomeSHA256          string `json:"outcome_sha256,omitempty"`
	OutcomeRows            int64  `json:"outcome_rows,omitempty"`

	// V5 predicate-atom payload.
	AtomizerVersion          string `json:"atomizer_version,omitempty"`
	PredicateContextSHA256   string `json:"predicate_context_sha256,omitempty"`
	SemanticProductID        string `json:"semantic_product_id,omitempty"`
	StableRole               string `json:"stable_role,omitempty"`
	PublicFieldID            string `json:"public_field_id,omitempty"`
	ResolvedExpressionSHA256 string `json:"resolved_expression_sha256,omitempty"`
	CollationName            string `json:"collation_name,omitempty"`
	CollationVersion         string `json:"collation_version,omitempty"`
	Operator                 string `json:"operator,omitempty"`
	CanonicalLiteral         string `json:"canonical_literal,omitempty"`

	// V5 composite-outcome payload.
	ResultObservationSHA256 string `json:"result_observation_sha256,omitempty"`
	VisibleRows             int64  `json:"visible_rows,omitempty"`
	PredicateProfileVersion string `json:"predicate_profile_version,omitempty"`
	PredicateSetSHA256      string `json:"predicate_set_sha256,omitempty"`
	PredicateAtomCount      int64  `json:"predicate_atom_count,omitempty"`
}

func (f FactID) IsV2() bool { return f.Profile == ProfileV2 }

func (f FactID) IsV3() bool { return f.Profile == ProfileV3 }

func (f FactID) IsV5() bool { return f.Profile == ProfileV5 }

func (f FactID) isVersioned() bool { return f.Profile != "" || f.Kind != "" }

func (f FactID) Validate() error {
	if f.isVersioned() {
		return f.validateVersioned()
	}
	for name, value := range map[string]string{
		"product": f.Product, "snapshot": f.Snapshot, "entity_key": f.EntityKey,
		"field": f.Field, "value_version": f.ValueVersion,
	} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("%w: %s is required without boundary whitespace", ErrInvalid, name)
		}
	}
	if !isSHA256(f.ValueVersion) {
		return fmt.Errorf("%w: value_version must be lowercase SHA-256", ErrInvalid)
	}
	return nil
}

func (f FactID) validateVersioned() error {
	if f.Product != "" || f.ValueVersion != "" {
		return fmt.Errorf("%w: V1 and versioned fact fields cannot be mixed", ErrInvalid)
	}
	switch f.Kind {
	case FactBaseRow:
		if f.Profile != ProfileV2 {
			return fmt.Errorf("%w: base-row fact requires profile %q", ErrInvalid, ProfileV2)
		}
		if invalidToken(f.SourceNamespace) || invalidToken(f.Snapshot) || invalidToken(f.EntityKey) {
			return fmt.Errorf("%w: base row namespace, snapshot, and entity key are required", ErrInvalid)
		}
		if f.Field != "" || f.SQLType != "" || f.CanonicalValue != "" || len(f.SnapshotBundle) != 0 ||
			f.OutputRowKey != "" || f.NormalizedExpression != "" || f.WitnessCommitment != "" || f.hasOutcomePayload() || f.hasV5OnlyPayload() {
			return fmt.Errorf("%w: base row carries cell or derived fields", ErrInvalid)
		}
	case FactBaseCell:
		if f.Profile != ProfileV2 {
			return fmt.Errorf("%w: base-cell fact requires profile %q", ErrInvalid, ProfileV2)
		}
		if invalidToken(f.SourceNamespace) || invalidToken(f.Snapshot) || invalidToken(f.EntityKey) ||
			invalidToken(f.Field) || invalidToken(f.SQLType) || f.CanonicalValue == "" {
			return fmt.Errorf("%w: base cell payload is incomplete", ErrInvalid)
		}
		canonicalType, err := CanonicalSQLTypeV2(f.SQLType)
		if err != nil || canonicalType != f.SQLType {
			return fmt.Errorf("%w: base cell SQL type is not canonical", ErrInvalid)
		}
		if len(f.SnapshotBundle) != 0 || f.OutputRowKey != "" || f.NormalizedExpression != "" || f.WitnessCommitment != "" || f.hasOutcomePayload() || f.hasV5OnlyPayload() {
			return fmt.Errorf("%w: base cell carries derived fields", ErrInvalid)
		}
	case FactDerived:
		if f.Profile != ProfileV2 {
			return fmt.Errorf("%w: derived fact requires profile %q", ErrInvalid, ProfileV2)
		}
		if f.SourceNamespace != "" || f.Snapshot != "" || f.EntityKey != "" || f.Field != "" ||
			invalidToken(f.OutputRowKey) || invalidToken(f.NormalizedExpression) || invalidToken(f.SQLType) ||
			f.CanonicalValue == "" || !isSHA256(f.WitnessCommitment) || len(f.SnapshotBundle) == 0 || f.hasOutcomePayload() || f.hasV5OnlyPayload() {
			return fmt.Errorf("%w: derived fact payload is incomplete or mixed", ErrInvalid)
		}
		canonicalType, err := CanonicalSQLTypeV2(f.SQLType)
		if err != nil || canonicalType != f.SQLType {
			return fmt.Errorf("%w: derived fact SQL type is not canonical", ErrInvalid)
		}
		seen := make(map[string]struct{}, len(f.SnapshotBundle))
		for _, binding := range f.SnapshotBundle {
			if invalidToken(binding.SourceNamespace) || invalidToken(binding.Snapshot) {
				return fmt.Errorf("%w: snapshot bundle binding is incomplete", ErrInvalid)
			}
			if _, duplicate := seen[binding.SourceNamespace]; duplicate {
				return fmt.Errorf("%w: duplicate snapshot namespace %q", ErrInvalid, binding.SourceNamespace)
			}
			seen[binding.SourceNamespace] = struct{}{}
		}
	case FactOutcome:
		if f.Profile != ProfileV3 {
			return fmt.Errorf("%w: outcome fact requires profile %q", ErrInvalid, ProfileV3)
		}
		if f.SourceNamespace != "" || f.Snapshot != "" || f.EntityKey != "" || f.Field != "" ||
			f.SQLType != "" || f.CanonicalValue != "" || len(f.SnapshotBundle) != 0 || f.OutputRowKey != "" ||
			f.NormalizedExpression != "" || f.WitnessCommitment != "" {
			return fmt.Errorf("%w: outcome fact carries base or derived fields", ErrInvalid)
		}
		if invalidToken(f.QueryNormalFormVersion) || !isSHA256(f.QueryNormalFormSHA256) ||
			!isSHA256(f.OutcomeSHA256) || f.OutcomeRows < 0 {
			return fmt.Errorf("%w: outcome fact payload is incomplete", ErrInvalid)
		}
		if f.hasV5Payload() {
			return fmt.Errorf("%w: V3 outcome fact carries V5 fields", ErrInvalid)
		}
	case FactPredicateAtom:
		if f.Profile != ProfileV5 {
			return fmt.Errorf("%w: predicate atom requires profile %q", ErrInvalid, ProfileV5)
		}
		if f.hasLegacyVersionedPayload() || f.hasOutcomePayload() || f.hasCompositePayload() {
			return fmt.Errorf("%w: predicate atom carries fields from another fact kind", ErrInvalid)
		}
		if f.AtomizerVersion != PredicateFootprintVersion || !isSHA256(f.PredicateContextSHA256) ||
			invalidToken(f.SemanticProductID) || invalidToken(f.StableRole) || invalidToken(f.PublicFieldID) ||
			invalidToken(f.SQLType) || invalidToken(f.Operator) || f.CanonicalLiteral == "" {
			return fmt.Errorf("%w: predicate atom payload is incomplete", ErrInvalid)
		}
		canonicalType, err := CanonicalSQLTypeV2(f.SQLType)
		if err != nil || canonicalType != f.SQLType {
			return fmt.Errorf("%w: predicate atom SQL type is not canonical", ErrInvalid)
		}
		if err := ValidateCanonicalSQLValueEncoding(f.SQLType, f.CanonicalLiteral); err != nil {
			return fmt.Errorf("%w: predicate atom literal is not canonical: %v", ErrInvalid, err)
		}
		if f.ResolvedExpressionSHA256 != "" && !isSHA256(f.ResolvedExpressionSHA256) {
			return fmt.Errorf("%w: resolved expression must be lowercase SHA-256", ErrInvalid)
		}
		if isCollatableTypeV5(f.SQLType) {
			if invalidToken(f.CollationName) || invalidToken(f.CollationVersion) {
				return fmt.Errorf("%w: collatable predicate atom requires a collation binding", ErrInvalid)
			}
		} else if f.CollationName != "" || f.CollationVersion != "" {
			return fmt.Errorf("%w: non-collatable predicate atom carries a collation", ErrInvalid)
		}
		switch f.Operator {
		case "EQ", "NE", "LT", "LE", "GT", "GE", "LIKE":
		default:
			return fmt.Errorf("%w: unsupported predicate atom operator %q", ErrInvalid, f.Operator)
		}
	case FactCompositeOutcome:
		if f.Profile != ProfileV5 {
			return fmt.Errorf("%w: composite outcome requires profile %q", ErrInvalid, ProfileV5)
		}
		if f.hasLegacyVersionedPayload() || f.OutcomeSHA256 != "" || f.OutcomeRows != 0 || f.hasAtomPayload() {
			return fmt.Errorf("%w: composite outcome carries fields from another fact kind", ErrInvalid)
		}
		if invalidToken(f.QueryNormalFormVersion) || !isSHA256(f.QueryNormalFormSHA256) ||
			!isSHA256(f.ResultObservationSHA256) || f.VisibleRows < 0 ||
			f.PredicateProfileVersion != PredicateFootprintVersion || !isSHA256(f.PredicateContextSHA256) ||
			!isSHA256(f.PredicateSetSHA256) || f.PredicateAtomCount < 0 {
			return fmt.Errorf("%w: composite outcome payload is incomplete", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown versioned fact kind %q", ErrInvalid, f.Kind)
	}
	return nil
}

func isCollatableTypeV5(sqlType string) bool {
	switch sqlType {
	case "text", "character", "character varying":
		return true
	default:
		return false
	}
}

func (f FactID) hasLegacyVersionedPayload() bool {
	return f.SourceNamespace != "" || f.Snapshot != "" || f.EntityKey != "" || f.Field != "" ||
		f.CanonicalValue != "" || len(f.SnapshotBundle) != 0 || f.OutputRowKey != "" ||
		f.NormalizedExpression != "" || f.WitnessCommitment != ""
}

func (f FactID) hasAtomPayload() bool {
	return f.AtomizerVersion != "" || f.SemanticProductID != "" || f.StableRole != "" ||
		f.PublicFieldID != "" || f.ResolvedExpressionSHA256 != "" || f.SQLType != "" ||
		f.CollationName != "" || f.CollationVersion != "" || f.Operator != "" || f.CanonicalLiteral != ""
}

func (f FactID) hasCompositePayload() bool {
	return f.ResultObservationSHA256 != "" || f.VisibleRows != 0 || f.PredicateProfileVersion != "" ||
		f.PredicateSetSHA256 != "" || f.PredicateAtomCount != 0
}

func (f FactID) hasV5Payload() bool {
	return f.PredicateContextSHA256 != "" || f.hasAtomPayload() || f.hasCompositePayload()
}

func (f FactID) hasV5OnlyPayload() bool {
	return f.AtomizerVersion != "" || f.PredicateContextSHA256 != "" || f.SemanticProductID != "" ||
		f.StableRole != "" || f.PublicFieldID != "" || f.ResolvedExpressionSHA256 != "" ||
		f.CollationName != "" || f.CollationVersion != "" || f.Operator != "" || f.CanonicalLiteral != "" ||
		f.ResultObservationSHA256 != "" || f.VisibleRows != 0 || f.PredicateProfileVersion != "" ||
		f.PredicateSetSHA256 != "" || f.PredicateAtomCount != 0
}

func (f FactID) hasOutcomePayload() bool {
	return f.QueryNormalFormVersion != "" || f.QueryNormalFormSHA256 != "" || f.OutcomeSHA256 != "" || f.OutcomeRows != 0
}

func invalidToken(value string) bool {
	return strings.TrimSpace(value) == "" || value != strings.TrimSpace(value)
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// CanonicalPayload returns the exact payload committed by Hash. Control PG
// persists these bytes and compares them on every hash conflict.
func (f FactID) CanonicalPayload() ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if !f.isVersioned() {
		// Preserve the V1 hash byte-for-byte. The added fields use omitempty.
		return json.Marshal(f)
	}
	var payload bytes.Buffer
	writeCanonicalString(&payload, string(f.Kind))
	writeCanonicalString(&payload, f.Profile)
	switch f.Kind {
	case FactBaseRow:
		writeCanonicalString(&payload, f.SourceNamespace)
		writeCanonicalString(&payload, f.Snapshot)
		writeCanonicalString(&payload, f.EntityKey)
	case FactBaseCell:
		writeCanonicalString(&payload, f.SourceNamespace)
		writeCanonicalString(&payload, f.Snapshot)
		writeCanonicalString(&payload, f.EntityKey)
		writeCanonicalString(&payload, f.Field)
		writeCanonicalString(&payload, f.SQLType)
		writeCanonicalString(&payload, f.CanonicalValue)
	case FactDerived:
		bindings := append([]SnapshotBinding(nil), f.SnapshotBundle...)
		sort.Slice(bindings, func(i, j int) bool {
			if bindings[i].SourceNamespace != bindings[j].SourceNamespace {
				return bindings[i].SourceNamespace < bindings[j].SourceNamespace
			}
			return bindings[i].Snapshot < bindings[j].Snapshot
		})
		writeCanonicalUint64(&payload, uint64(len(bindings)))
		for _, binding := range bindings {
			writeCanonicalString(&payload, binding.SourceNamespace)
			writeCanonicalString(&payload, binding.Snapshot)
		}
		writeCanonicalString(&payload, f.OutputRowKey)
		writeCanonicalString(&payload, f.NormalizedExpression)
		writeCanonicalString(&payload, f.SQLType)
		writeCanonicalString(&payload, f.CanonicalValue)
		writeCanonicalString(&payload, f.WitnessCommitment)
	case FactOutcome:
		writeCanonicalString(&payload, f.QueryNormalFormVersion)
		writeCanonicalString(&payload, f.QueryNormalFormSHA256)
		writeCanonicalString(&payload, f.OutcomeSHA256)
		writeCanonicalUint64(&payload, uint64(f.OutcomeRows))
	case FactPredicateAtom:
		writeCanonicalString(&payload, f.AtomizerVersion)
		writeCanonicalString(&payload, f.PredicateContextSHA256)
		writeCanonicalString(&payload, f.SemanticProductID)
		writeCanonicalString(&payload, f.StableRole)
		writeCanonicalString(&payload, f.PublicFieldID)
		writeCanonicalString(&payload, f.ResolvedExpressionSHA256)
		writeCanonicalString(&payload, f.SQLType)
		writeCanonicalString(&payload, f.CollationName)
		writeCanonicalString(&payload, f.CollationVersion)
		writeCanonicalString(&payload, f.Operator)
		writeCanonicalString(&payload, f.CanonicalLiteral)
	case FactCompositeOutcome:
		writeCanonicalString(&payload, f.QueryNormalFormVersion)
		writeCanonicalString(&payload, f.QueryNormalFormSHA256)
		writeCanonicalString(&payload, f.ResultObservationSHA256)
		writeCanonicalUint64(&payload, uint64(f.VisibleRows))
		writeCanonicalString(&payload, f.PredicateProfileVersion)
		writeCanonicalString(&payload, f.PredicateContextSHA256)
		writeCanonicalString(&payload, f.PredicateSetSHA256)
		writeCanonicalUint64(&payload, uint64(f.PredicateAtomCount))
	}
	return payload.Bytes(), nil
}

// HashBytes returns the binary PostgreSQL ledger index for the semantic payload.
func (f FactID) HashBytes() ([32]byte, error) {
	payload, err := f.CanonicalPayload()
	if err != nil {
		return [32]byte{}, err
	}
	if f.IsV2() {
		payload = append([]byte(factDomainV2), payload...)
	} else if f.IsV3() {
		payload = append([]byte(factDomainV3), payload...)
	} else if f.IsV5() {
		payload = append([]byte(factDomainV5), payload...)
	}
	return sha256.Sum256(payload), nil
}

// Hash returns the durable hexadecimal PostgreSQL ledger index for the semantic payload.
func (f FactID) Hash() (string, error) {
	digest, err := f.HashBytes()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest[:]), nil
}

func NewPredicateAtomFactV5(input PredicateAtomFactV5) (FactID, error) {
	profile := input.ProfileVersion
	if profile == "" {
		profile = ProfileV5
	}
	atomizer := input.AtomizerVersion
	if atomizer == "" {
		atomizer = PredicateFootprintVersion
	}
	fact := FactID{
		Profile: profile, Kind: FactPredicateAtom, AtomizerVersion: atomizer,
		PredicateContextSHA256: input.PredicateContextSHA256, SemanticProductID: input.SemanticProductID,
		StableRole: input.StableRole, PublicFieldID: input.PublicFieldID,
		ResolvedExpressionSHA256: input.ResolvedExpressionSHA256, SQLType: input.SQLType,
		CollationName: input.CollationName, CollationVersion: input.CollationVersion,
		Operator: input.Operator, CanonicalLiteral: input.CanonicalLiteral,
	}
	return fact, fact.Validate()
}

func NewCompositeOutcomeFactV5(input CompositeOutcomeFactV5) (FactID, error) {
	profile := input.ProfileVersion
	if profile == "" {
		profile = ProfileV5
	}
	predicateProfile := input.PredicateProfileVersion
	if predicateProfile == "" {
		predicateProfile = PredicateFootprintVersion
	}
	fact := FactID{
		Profile: profile, Kind: FactCompositeOutcome,
		QueryNormalFormVersion:  input.QueryNormalFormVersion,
		QueryNormalFormSHA256:   input.QueryNormalFormSHA256,
		ResultObservationSHA256: input.ResultObservationSHA256, VisibleRows: input.VisibleRows,
		PredicateProfileVersion: predicateProfile, PredicateContextSHA256: input.PredicateContextSHA256,
		PredicateSetSHA256: input.PredicateSetSHA256, PredicateAtomCount: input.PredicateAtomCount,
	}
	return fact, fact.Validate()
}

// PredicateSetHashV1 commits the sorted, duplicate-free full hashes of V5
// predicate atoms. Both QueryPlan construction and settlement validation use
// this exact encoding.
func PredicateSetHashV1(atoms []FactID) (string, error) {
	type member struct {
		hash    [sha256.Size]byte
		payload []byte
	}
	members := make(map[[sha256.Size]byte][]byte, len(atoms))
	for _, atom := range atoms {
		if atom.Profile != ProfileV5 || atom.Kind != FactPredicateAtom {
			return "", fmt.Errorf("%w: predicate set contains a non-atom fact", ErrInvalid)
		}
		hashText, err := atom.Hash()
		if err != nil {
			return "", err
		}
		hashBytes, _ := hex.DecodeString(hashText)
		var hash [sha256.Size]byte
		copy(hash[:], hashBytes)
		payload, err := atom.CanonicalPayload()
		if err != nil {
			return "", err
		}
		if existing, present := members[hash]; present && !bytes.Equal(existing, payload) {
			return "", fmt.Errorf("%w: predicate atom SHA-256 collision", ErrInvalid)
		}
		members[hash] = payload
	}
	ordered := make([]member, 0, len(members))
	for hash, payload := range members {
		ordered = append(ordered, member{hash: hash, payload: payload})
	}
	sort.Slice(ordered, func(i, j int) bool { return bytes.Compare(ordered[i].hash[:], ordered[j].hash[:]) < 0 })
	h := sha256.New()
	_, _ = h.Write([]byte(predicateSetDomainV1))
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(ordered)))
	_, _ = h.Write(count[:])
	for _, member := range ordered {
		_, _ = h.Write(member.hash[:])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func NewBaseRowFactV2(sourceNamespace, snapshot, entityKey string) (FactID, error) {
	fact := FactID{Profile: ProfileV2, Kind: FactBaseRow, SourceNamespace: sourceNamespace, Snapshot: snapshot, EntityKey: entityKey}
	return fact, fact.Validate()
}

func NewBaseCellFactV2(sourceNamespace, snapshot, entityKey, fieldID, sqlType string, value any) (FactID, error) {
	typeName, err := CanonicalSQLTypeV2(sqlType)
	if err != nil {
		return FactID{}, err
	}
	canonical, err := CanonicalSQLValue(typeName, value)
	if err != nil {
		return FactID{}, err
	}
	fact := FactID{Profile: ProfileV2, Kind: FactBaseCell, SourceNamespace: sourceNamespace, Snapshot: snapshot,
		EntityKey: entityKey, Field: fieldID, SQLType: typeName, CanonicalValue: canonical}
	return fact, fact.Validate()
}

func NewDerivedFactV2(snapshotBundle []SnapshotBinding, outputRowKey, normalizedExpression, sqlType string, value any, witnessCommitment string) (FactID, error) {
	typeName, err := CanonicalSQLTypeV2(sqlType)
	if err != nil {
		return FactID{}, err
	}
	canonical, err := CanonicalSQLValue(typeName, value)
	if err != nil {
		return FactID{}, err
	}
	fact := FactID{Profile: ProfileV2, Kind: FactDerived, SnapshotBundle: append([]SnapshotBinding(nil), snapshotBundle...),
		OutputRowKey: outputRowKey, NormalizedExpression: normalizedExpression, SQLType: typeName,
		CanonicalValue: canonical, WitnessCommitment: witnessCommitment}
	return fact, fact.Validate()
}

func NewOutcomeFactV3(normalFormVersion, normalFormSHA256, outcomeSHA256 string, outcomeRows int64) (FactID, error) {
	fact := FactID{
		Profile:                ProfileV3,
		Kind:                   FactOutcome,
		QueryNormalFormVersion: normalFormVersion,
		QueryNormalFormSHA256:  normalFormSHA256,
		OutcomeSHA256:          outcomeSHA256,
		OutcomeRows:            outcomeRows,
	}
	return fact, fact.Validate()
}

// CanonicalSQLValue encodes the supported PostgreSQL value domain without a
// float64 coercion. The SQL type is part of the fact payload, while this value
// encoding normalizes equivalent representations inside that type.
func CanonicalSQLValue(sqlType string, value any) (string, error) {
	typeName, err := CanonicalSQLTypeV2(sqlType)
	if err != nil {
		return "", err
	}
	if value == nil {
		return "null", nil
	}
	switch typeName {
	case "smallint", "integer", "bigint":
		integer, err := canonicalInteger(value)
		if err != nil {
			return "", err
		}
		if err := validateIntegerRange(typeName, integer); err != nil {
			return "", err
		}
		return "i:" + integer, nil
	case "numeric":
		number, err := canonicalNumeric(value)
		if err != nil {
			return "", err
		}
		if number == "null" {
			return "null", nil
		}
		return "n:" + number, nil
	case "real", "double precision":
		number, err := canonicalFloat(typeName, value)
		if err != nil {
			return "", err
		}
		return "f:" + number, nil
	case "boolean":
		boolean, ok := value.(bool)
		if !ok {
			return "", fmt.Errorf("%w: %T is not boolean", ErrInvalid, value)
		}
		return "b:" + strconv.FormatBool(boolean), nil
	case "bytea":
		binaryValue, ok := value.([]byte)
		if !ok {
			return "", fmt.Errorf("%w: %T is not bytea", ErrInvalid, value)
		}
		return "x:" + hex.EncodeToString(binaryValue), nil
	case "date":
		parsed, err := canonicalDate(value)
		if err != nil {
			return "", err
		}
		if parsed == "null" {
			return "null", nil
		}
		return "d:" + parsed, nil
	case "time without time zone":
		parsed, err := canonicalTimeOfDay(value)
		if err != nil {
			return "", err
		}
		if parsed == "null" {
			return "null", nil
		}
		return "tm:" + parsed, nil
	case "timestamp with time zone":
		parsed, err := canonicalTimestamp(value, true)
		if err != nil {
			return "", err
		}
		if parsed == "null" {
			return "null", nil
		}
		return "tz:" + parsed, nil
	case "timestamp without time zone":
		parsed, err := canonicalTimestamp(value, false)
		if err != nil {
			return "", err
		}
		if parsed == "null" {
			return "null", nil
		}
		return "ts:" + parsed, nil
	case "jsonb":
		encoded, err := canonicalJSONBValue(value)
		if err != nil {
			return "", err
		}
		return "j:" + string(encoded), nil
	case "uuid":
		canonical, err := canonicalUUID(value)
		if err != nil {
			return "", err
		}
		if canonical == "null" {
			return "null", nil
		}
		return "u:" + canonical, nil
	case "text", "character", "character varying":
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("%w: %T is not valid for SQL type %q", ErrInvalid, value, typeName)
		}
		if typeName == "character" {
			// PostgreSQL bpchar equality ignores trailing ASCII spaces. V2 uses
			// the same semantic normal form for Fact identity and DISTINCT keys.
			text = strings.TrimRight(text, " ")
		}
		return "s:" + text, nil
	default:
		return "", fmt.Errorf("%w: PostgreSQL type %q is outside %s", ErrInvalid, sqlType, ProfileV2)
	}
}

// ValidateCanonicalSQLValueEncoding independently checks an already encoded
// literal at the Control trust boundary. Most scalar encodings can be decoded
// to their public Go representation and passed through CanonicalSQLValue.
// TIME and JSONB use internal representations, so they are validated directly
// without changing their durable FactID bytes.
func ValidateCanonicalSQLValueEncoding(sqlType, encoded string) error {
	typeName, err := CanonicalSQLTypeV2(sqlType)
	if err != nil {
		return err
	}
	if encoded == "null" {
		return nil
	}
	var value any
	requirePrefix := func(prefix string) (string, error) {
		if !strings.HasPrefix(encoded, prefix) {
			return "", fmt.Errorf("%w: canonical %s literal requires %q prefix", ErrInvalid, typeName, prefix)
		}
		return strings.TrimPrefix(encoded, prefix), nil
	}
	switch typeName {
	case "smallint", "integer", "bigint":
		value, err = requirePrefix("i:")
	case "numeric":
		value, err = requirePrefix("n:")
	case "real", "double precision":
		var text string
		text, err = requirePrefix("f:")
		if err == nil {
			switch text {
			case "nan":
				value = math.NaN()
			case "+infinity":
				value = math.Inf(1)
			case "-infinity":
				value = math.Inf(-1)
			default:
				bitSize := 64
				if typeName == "real" {
					bitSize = 32
				}
				value, err = strconv.ParseFloat(text, bitSize)
			}
		}
	case "boolean":
		var text string
		text, err = requirePrefix("b:")
		if err == nil {
			value, err = strconv.ParseBool(text)
		}
	case "bytea":
		var text string
		text, err = requirePrefix("x:")
		if err == nil {
			value, err = hex.DecodeString(text)
		}
	case "date":
		value, err = requirePrefix("d:")
	case "time without time zone":
		var text string
		text, err = requirePrefix("tm:")
		if err == nil {
			var microseconds int64
			microseconds, err = strconv.ParseInt(text, 10, 64)
			const microsecondsPerDay = int64(24 * time.Hour / time.Microsecond)
			if err == nil && (microseconds < 0 || microseconds > microsecondsPerDay ||
				strconv.FormatInt(microseconds, 10) != text) {
				err = fmt.Errorf("%w: non-canonical time without time zone encoding", ErrInvalid)
			}
		}
		if err != nil {
			return err
		}
		return nil
	case "timestamp with time zone":
		value, err = requirePrefix("tz:")
	case "timestamp without time zone":
		value, err = requirePrefix("ts:")
	case "jsonb":
		var text string
		text, err = requirePrefix("j:")
		if err == nil {
			err = validateCanonicalJSONBEncoding([]byte(text))
		}
		if err != nil {
			return err
		}
		return nil
	case "uuid":
		value, err = requirePrefix("u:")
	case "text", "character", "character varying":
		value, err = requirePrefix("s:")
	default:
		err = fmt.Errorf("%w: unsupported canonical literal type %q", ErrInvalid, typeName)
	}
	if err != nil {
		return err
	}
	reencoded, err := CanonicalSQLValue(typeName, value)
	if err != nil {
		return err
	}
	if reencoded != encoded {
		return fmt.Errorf("%w: non-canonical literal encoding", ErrInvalid)
	}
	return nil
}

func normalizeSQLType(value string) string {
	normalized := strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
	switch normalized {
	case "int2":
		return "smallint"
	case "int4", "int":
		return "integer"
	case "int8":
		return "bigint"
	case "decimal":
		return "numeric"
	case "float4":
		return "real"
	case "float8":
		return "double precision"
	case "bool":
		return "boolean"
	case "char":
		return "character"
	case "varchar":
		return "character varying"
	case "time", "time without time zone":
		return "time without time zone"
	case "timetz", "time with time zone":
		return "time with time zone"
	case "timestamp", "timestamp without time zone":
		return "timestamp without time zone"
	case "timestamptz", "timestamp with time zone":
		return "timestamp with time zone"
	default:
		return normalized
	}
}

// CanonicalSQLTypeV2 returns the profile-level PostgreSQL type name committed
// by FactID. It deliberately erases PostgreSQL aliases such as int8 and
// timestamptz so schema spelling cannot manufacture a fresh semantic fact.
func CanonicalSQLTypeV2(value string) (string, error) {
	result := normalizeSQLType(value)
	switch result {
	case "smallint", "integer", "bigint", "numeric", "real", "double precision",
		"boolean", "bytea", "date", "time without time zone",
		"timestamp with time zone", "timestamp without time zone", "jsonb",
		"uuid", "text", "character", "character varying":
		return result, nil
	case "time with time zone":
		return "", fmt.Errorf("%w: PostgreSQL time with time zone is outside %s", ErrInvalid, ProfileV2)
	default:
		return "", fmt.Errorf("%w: PostgreSQL type %q is outside %s", ErrInvalid, value, ProfileV2)
	}
}

func canonicalInteger(value any) (string, error) {
	switch typed := value.(type) {
	case int:
		return strconv.FormatInt(int64(typed), 10), nil
	case int16:
		return strconv.FormatInt(int64(typed), 10), nil
	case int32:
		return strconv.FormatInt(int64(typed), 10), nil
	case int64:
		return strconv.FormatInt(typed, 10), nil
	case uint:
		return strconv.FormatUint(uint64(typed), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(typed), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(typed), 10), nil
	case uint64:
		return strconv.FormatUint(typed, 10), nil
	case json.Number:
		if _, ok := new(big.Int).SetString(string(typed), 10); ok {
			return string(typed), nil
		}
	case string:
		if integer, ok := new(big.Int).SetString(typed, 10); ok {
			return integer.String(), nil
		}
	}
	return "", fmt.Errorf("%w: %T is not an exact integer", ErrInvalid, value)
}

func validateIntegerRange(sqlType, value string) error {
	integer, ok := new(big.Int).SetString(value, 10)
	if !ok {
		return fmt.Errorf("%w: invalid integer", ErrInvalid)
	}
	bits := uint(64)
	switch sqlType {
	case "smallint":
		bits = 16
	case "integer":
		bits = 32
	}
	minimum := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), bits-1))
	maximum := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bits-1), big.NewInt(1))
	if integer.Cmp(minimum) < 0 || integer.Cmp(maximum) > 0 {
		return fmt.Errorf("%w: integer is outside PostgreSQL %s range", ErrInvalid, sqlType)
	}
	return nil
}

func canonicalNumeric(value any) (string, error) {
	var text string
	switch typed := value.(type) {
	case json.Number:
		text = string(typed)
	case string:
		text = typed
	case int, int16, int32, int64, uint, uint16, uint32, uint64:
		integer, err := canonicalInteger(value)
		if err != nil {
			return "", err
		}
		text = integer
	case float32, float64:
		return "", fmt.Errorf("%w: exact numeric cannot be constructed from a binary float", ErrInvalid)
	case pgtype.Numeric:
		if !typed.Valid {
			return "null", nil
		}
		if typed.NaN {
			return "nan", nil
		}
		if typed.InfinityModifier == pgtype.Infinity {
			return "+infinity", nil
		}
		if typed.InfinityModifier == pgtype.NegativeInfinity {
			return "-infinity", nil
		}
		if typed.Int == nil {
			return "", fmt.Errorf("%w: invalid pgx numeric", ErrInvalid)
		}
		rational := new(big.Rat).SetInt(typed.Int)
		power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absInt32(typed.Exp))), nil)
		if typed.Exp >= 0 {
			rational.Mul(rational, new(big.Rat).SetInt(power))
		} else {
			rational.Quo(rational, new(big.Rat).SetInt(power))
		}
		return rational.RatString(), nil
	default:
		// pgx numeric values and database adapters commonly implement Stringer.
		if stringer, ok := value.(fmt.Stringer); ok {
			text = stringer.String()
		}
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "nan":
		return "nan", nil
	case "infinity", "+infinity":
		return "+infinity", nil
	case "-infinity":
		return "-infinity", nil
	}
	rational, ok := new(big.Rat).SetString(strings.TrimSpace(text))
	if !ok {
		return "", fmt.Errorf("%w: %T is not an exact numeric", ErrInvalid, value)
	}
	return rational.RatString(), nil
}

func absInt32(value int32) int32 {
	if value < 0 {
		return -value
	}
	return value
}

func canonicalFloat(sqlType string, value any) (string, error) {
	var number float64
	switch typed := value.(type) {
	case float32:
		number = float64(typed)
	case float64:
		if sqlType == "real" {
			number = float64(float32(typed))
		} else {
			number = typed
		}
	case json.Number:
		bitSize := 64
		if sqlType == "real" {
			bitSize = 32
		}
		parsed, err := strconv.ParseFloat(string(typed), bitSize)
		if err != nil {
			return "", fmt.Errorf("%w: invalid floating value", ErrInvalid)
		}
		number = parsed
	default:
		return "", fmt.Errorf("%w: %T is not floating point", ErrInvalid, value)
	}
	if math.IsNaN(number) {
		return "nan", nil
	}
	if math.IsInf(number, 1) {
		return "+infinity", nil
	}
	if math.IsInf(number, -1) {
		return "-infinity", nil
	}
	if number == 0 {
		number = 0 // normalize negative zero
	}
	bitSize := 64
	if sqlType == "real" {
		bitSize = 32
	}
	return strconv.FormatFloat(number, 'x', -1, bitSize), nil
}

func canonicalDate(value any) (string, error) {
	switch typed := value.(type) {
	case time.Time:
		return typed.Format("2006-01-02"), nil
	case pgtype.Date:
		if !typed.Valid {
			return "null", nil
		}
		switch typed.InfinityModifier {
		case pgtype.Infinity:
			return "+infinity", nil
		case pgtype.NegativeInfinity:
			return "-infinity", nil
		}
		return typed.Time.Format("2006-01-02"), nil
	case string:
		if typed == "infinity" || typed == "+infinity" {
			return "+infinity", nil
		}
		if typed == "-infinity" {
			return "-infinity", nil
		}
		parsed, err := time.Parse("2006-01-02", typed)
		if err != nil {
			return "", fmt.Errorf("%w: invalid temporal value", ErrInvalid)
		}
		return parsed.Format("2006-01-02"), nil
	default:
		return "", fmt.Errorf("%w: %T is not temporal", ErrInvalid, value)
	}
}

func canonicalTimeOfDay(value any) (string, error) {
	const microsecondsPerDay = int64(24 * time.Hour / time.Microsecond)
	var microseconds int64
	switch typed := value.(type) {
	case pgtype.Time:
		if !typed.Valid {
			return "null", nil
		}
		microseconds = typed.Microseconds
	case time.Duration:
		microseconds = typed.Microseconds()
	case time.Time:
		microseconds = int64(typed.Hour())*int64(time.Hour/time.Microsecond) +
			int64(typed.Minute())*int64(time.Minute/time.Microsecond) +
			int64(typed.Second())*int64(time.Second/time.Microsecond) +
			int64(typed.Nanosecond()/int(time.Microsecond))
	case string:
		parsed, err := time.Parse("15:04:05.999999", typed)
		if err != nil {
			return "", fmt.Errorf("%w: invalid time without time zone", ErrInvalid)
		}
		microseconds = int64(parsed.Hour())*int64(time.Hour/time.Microsecond) +
			int64(parsed.Minute())*int64(time.Minute/time.Microsecond) +
			int64(parsed.Second())*int64(time.Second/time.Microsecond) +
			int64(parsed.Nanosecond()/int(time.Microsecond))
	default:
		return "", fmt.Errorf("%w: %T is not a time without time zone", ErrInvalid, value)
	}
	if microseconds < 0 || microseconds > microsecondsPerDay {
		return "", fmt.Errorf("%w: time without time zone is outside 00:00:00..24:00:00", ErrInvalid)
	}
	return strconv.FormatInt(microseconds, 10), nil
}

func canonicalTimestamp(value any, withTimezone bool) (string, error) {
	var timestamp time.Time
	switch typed := value.(type) {
	case time.Time:
		timestamp = typed
	case pgtype.Timestamp:
		if !typed.Valid {
			return "null", nil
		}
		switch typed.InfinityModifier {
		case pgtype.Infinity:
			return "+infinity", nil
		case pgtype.NegativeInfinity:
			return "-infinity", nil
		}
		timestamp = typed.Time
	case pgtype.Timestamptz:
		if !typed.Valid {
			return "null", nil
		}
		switch typed.InfinityModifier {
		case pgtype.Infinity:
			return "+infinity", nil
		case pgtype.NegativeInfinity:
			return "-infinity", nil
		}
		timestamp = typed.Time
	case string:
		if typed == "infinity" || typed == "+infinity" {
			return "+infinity", nil
		}
		if typed == "-infinity" {
			return "-infinity", nil
		}
		layout := time.RFC3339Nano
		if !withTimezone {
			layout = "2006-01-02T15:04:05.999999999"
			if strings.Contains(typed, " ") && !strings.Contains(typed, "T") {
				layout = "2006-01-02 15:04:05.999999999"
			}
		}
		parsed, err := time.Parse(layout, typed)
		if err != nil {
			return "", fmt.Errorf("%w: invalid timestamp", ErrInvalid)
		}
		timestamp = parsed
	default:
		return "", fmt.Errorf("%w: %T is not a timestamp", ErrInvalid, value)
	}
	if withTimezone {
		return timestamp.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.999999Z07:00"), nil
	}
	return timestamp.Truncate(time.Microsecond).Format("2006-01-02T15:04:05.999999"), nil
}

func canonicalJSONBValue(value any) ([]byte, error) {
	var decoded any
	switch typed := value.(type) {
	case []byte:
		decoder := json.NewDecoder(bytes.NewReader(typed))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("%w: invalid JSON value", ErrInvalid)
		}
		if err := requireJSONEOF(decoder); err != nil {
			return nil, err
		}
	case json.RawMessage:
		decoder := json.NewDecoder(bytes.NewReader(typed))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("%w: invalid JSON value", ErrInvalid)
		}
		if err := requireJSONEOF(decoder); err != nil {
			return nil, err
		}
	case string:
		decoder := json.NewDecoder(strings.NewReader(typed))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("%w: invalid JSON value", ErrInvalid)
		}
		if err := requireJSONEOF(decoder); err != nil {
			return nil, err
		}
	default:
		decoded = value
	}
	var encoded bytes.Buffer
	if err := writeCanonicalJSONValue(&encoded, decoded); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if err == nil {
		return fmt.Errorf("%w: JSON value has trailing content", ErrInvalid)
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: invalid trailing JSON content", ErrInvalid)
	}
	return nil
}

func writeCanonicalJSONValue(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("z")
	case bool:
		if typed {
			buffer.WriteString("b1")
		} else {
			buffer.WriteString("b0")
		}
	case string:
		buffer.WriteString("s")
		writeCanonicalString(buffer, typed)
	case json.Number:
		rational, ok := new(big.Rat).SetString(string(typed))
		if !ok {
			return fmt.Errorf("%w: JSON number is not exact", ErrInvalid)
		}
		buffer.WriteString("n")
		writeCanonicalString(buffer, rational.RatString())
	case float64:
		if math.IsInf(typed, 0) || math.IsNaN(typed) {
			return fmt.Errorf("%w: JSON number is non-finite", ErrInvalid)
		}
		rational, ok := new(big.Rat).SetString(strconv.FormatFloat(typed, 'g', -1, 64))
		if !ok {
			return fmt.Errorf("%w: JSON number is not exact", ErrInvalid)
		}
		buffer.WriteString("n")
		writeCanonicalString(buffer, rational.RatString())
	case []any:
		buffer.WriteString("a")
		writeCanonicalUint64(buffer, uint64(len(typed)))
		for _, item := range typed {
			if err := writeCanonicalJSONValue(buffer, item); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteString("o")
		writeCanonicalUint64(buffer, uint64(len(keys)))
		for _, key := range keys {
			writeCanonicalString(buffer, key)
			if err := writeCanonicalJSONValue(buffer, typed[key]); err != nil {
				return err
			}
		}
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("%w: invalid JSON value: %v", ErrInvalid, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		var normalized any
		if err := decoder.Decode(&normalized); err != nil {
			return fmt.Errorf("%w: invalid JSON value: %v", ErrInvalid, err)
		}
		return writeCanonicalJSONValue(buffer, normalized)
	}
	return nil
}

type canonicalJSONBNode struct {
	tag      byte
	text     string
	boolean  bool
	children []canonicalJSONBNode
	members  []canonicalJSONBMember
}

type canonicalJSONBMember struct {
	key   string
	value canonicalJSONBNode
}

type canonicalJSONBDecoder struct {
	data   []byte
	offset int
}

// validateCanonicalJSONBEncoding recognizes the durable tagged representation
// emitted by writeCanonicalJSONValue. It is deliberately independent of the
// standard JSON decoder: this byte stream is canonical JSONB identity, not JSON
// source text.
func validateCanonicalJSONBEncoding(encoded []byte) error {
	decoder := canonicalJSONBDecoder{data: encoded}
	node, err := decoder.value()
	if err != nil {
		return err
	}
	if decoder.offset != len(encoded) {
		return fmt.Errorf("%w: canonical JSONB has trailing bytes", ErrInvalid)
	}
	var reproduced bytes.Buffer
	writeCanonicalJSONBNode(&reproduced, node)
	if !bytes.Equal(reproduced.Bytes(), encoded) {
		return fmt.Errorf("%w: non-canonical JSONB encoding", ErrInvalid)
	}
	return nil
}

func (decoder *canonicalJSONBDecoder) value() (canonicalJSONBNode, error) {
	if decoder.offset >= len(decoder.data) {
		return canonicalJSONBNode{}, fmt.Errorf("%w: truncated canonical JSONB value", ErrInvalid)
	}
	tag := decoder.data[decoder.offset]
	decoder.offset++
	node := canonicalJSONBNode{tag: tag}
	switch tag {
	case 'z':
		return node, nil
	case 'b':
		if decoder.offset >= len(decoder.data) ||
			(decoder.data[decoder.offset] != '0' && decoder.data[decoder.offset] != '1') {
			return canonicalJSONBNode{}, fmt.Errorf("%w: malformed canonical JSONB boolean", ErrInvalid)
		}
		node.boolean = decoder.data[decoder.offset] == '1'
		decoder.offset++
		return node, nil
	case 's':
		text, err := decoder.text()
		if err != nil {
			return canonicalJSONBNode{}, err
		}
		node.text = text
		return node, nil
	case 'n':
		text, err := decoder.text()
		if err != nil {
			return canonicalJSONBNode{}, err
		}
		rational, ok := new(big.Rat).SetString(text)
		if !ok || rational.RatString() != text {
			return canonicalJSONBNode{}, fmt.Errorf("%w: non-canonical JSONB rational", ErrInvalid)
		}
		node.text = text
		return node, nil
	case 'a':
		count, err := decoder.count(1)
		if err != nil {
			return canonicalJSONBNode{}, err
		}
		node.children = make([]canonicalJSONBNode, 0, count)
		for index := 0; index < count; index++ {
			child, childErr := decoder.value()
			if childErr != nil {
				return canonicalJSONBNode{}, childErr
			}
			node.children = append(node.children, child)
		}
		return node, nil
	case 'o':
		count, err := decoder.count(9)
		if err != nil {
			return canonicalJSONBNode{}, err
		}
		node.members = make([]canonicalJSONBMember, 0, count)
		previous := ""
		for index := 0; index < count; index++ {
			key, keyErr := decoder.text()
			if keyErr != nil {
				return canonicalJSONBNode{}, keyErr
			}
			if index > 0 && key <= previous {
				return canonicalJSONBNode{}, fmt.Errorf("%w: canonical JSONB object keys are not strictly sorted", ErrInvalid)
			}
			child, childErr := decoder.value()
			if childErr != nil {
				return canonicalJSONBNode{}, childErr
			}
			node.members = append(node.members, canonicalJSONBMember{key: key, value: child})
			previous = key
		}
		return node, nil
	default:
		return canonicalJSONBNode{}, fmt.Errorf("%w: unknown canonical JSONB tag %q", ErrInvalid, tag)
	}
}

func (decoder *canonicalJSONBDecoder) count(minimumBytes uint64) (int, error) {
	value, err := decoder.uint64()
	if err != nil {
		return 0, err
	}
	remaining := uint64(len(decoder.data) - decoder.offset)
	if value > remaining/minimumBytes || value > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("%w: canonical JSONB count exceeds remaining input", ErrInvalid)
	}
	return int(value), nil
}

func (decoder *canonicalJSONBDecoder) text() (string, error) {
	length, err := decoder.uint64()
	if err != nil {
		return "", err
	}
	remaining := uint64(len(decoder.data) - decoder.offset)
	if length > remaining {
		return "", fmt.Errorf("%w: truncated canonical JSONB string", ErrInvalid)
	}
	value := string(decoder.data[decoder.offset : decoder.offset+int(length)])
	decoder.offset += int(length)
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%w: canonical JSONB string is not UTF-8", ErrInvalid)
	}
	return value, nil
}

func (decoder *canonicalJSONBDecoder) uint64() (uint64, error) {
	if len(decoder.data)-decoder.offset < 8 {
		return 0, fmt.Errorf("%w: truncated canonical JSONB length", ErrInvalid)
	}
	value := binary.BigEndian.Uint64(decoder.data[decoder.offset : decoder.offset+8])
	decoder.offset += 8
	return value, nil
}

func writeCanonicalJSONBNode(buffer *bytes.Buffer, node canonicalJSONBNode) {
	buffer.WriteByte(node.tag)
	switch node.tag {
	case 'b':
		if node.boolean {
			buffer.WriteByte('1')
		} else {
			buffer.WriteByte('0')
		}
	case 's', 'n':
		writeCanonicalString(buffer, node.text)
	case 'a':
		writeCanonicalUint64(buffer, uint64(len(node.children)))
		for _, child := range node.children {
			writeCanonicalJSONBNode(buffer, child)
		}
	case 'o':
		writeCanonicalUint64(buffer, uint64(len(node.members)))
		for _, member := range node.members {
			writeCanonicalString(buffer, member.key)
			writeCanonicalJSONBNode(buffer, member.value)
		}
	}
}

func canonicalUUID(value any) (string, error) {
	var raw []byte
	switch typed := value.(type) {
	case pgtype.UUID:
		if !typed.Valid {
			return "null", nil
		}
		raw = typed.Bytes[:]
	case [16]byte:
		raw = typed[:]
	case []byte:
		if len(typed) == 16 {
			raw = typed
		} else {
			return canonicalUUID(string(typed))
		}
	case string:
		compact := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(typed)), "-", "")
		if len(compact) != 32 {
			return "", fmt.Errorf("%w: invalid UUID", ErrInvalid)
		}
		decoded, err := hex.DecodeString(compact)
		if err != nil {
			return "", fmt.Errorf("%w: invalid UUID", ErrInvalid)
		}
		raw = decoded
	default:
		return "", fmt.Errorf("%w: %T is not UUID", ErrInvalid, value)
	}
	encoded := hex.EncodeToString(raw)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func writeCanonicalString(buffer *bytes.Buffer, value string) {
	writeCanonicalUint64(buffer, uint64(len(value)))
	buffer.WriteString(value)
}

func writeCanonicalUint64(buffer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	buffer.Write(encoded[:])
}

// ValueVersion hashes the typed JSON representation of a V1 released value.
func ValueVersion(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("%w: value is not canonical JSON: %v", ErrInvalid, err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ComposeKey creates a stable V1 entity key from one or more values.
func ComposeKey(values ...any) (string, error) {
	if len(values) == 0 {
		return "", fmt.Errorf("%w: entity key requires at least one value", ErrInvalid)
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("%w: encode entity key: %v", ErrInvalid, err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ComposeCanonicalKeyV2 hashes an ordered, length-delimited semantic tuple.
func ComposeCanonicalKeyV2(domain string, values ...string) (string, error) {
	if invalidToken(domain) || len(values) == 0 {
		return "", fmt.Errorf("%w: canonical key domain and values are required", ErrInvalid)
	}
	var payload bytes.Buffer
	writeCanonicalString(&payload, domain)
	writeCanonicalUint64(&payload, uint64(len(values)))
	for _, value := range values {
		writeCanonicalString(&payload, value)
	}
	digest := sha256.Sum256(payload.Bytes())
	return hex.EncodeToString(digest[:]), nil
}

func NewFact(product, snapshot, entityKey, field string, value any) (FactID, error) {
	version, err := ValueVersion(value)
	if err != nil {
		return FactID{}, err
	}
	fact := FactID{Product: product, Snapshot: snapshot, EntityKey: entityKey, Field: field, ValueVersion: version}
	if err := fact.Validate(); err != nil {
		return FactID{}, err
	}
	return fact, nil
}

// FactSet is keyed by hash but collision-aware: the full canonical payload is
// compared before a repeated hash is accepted.
type FactSet map[[32]byte]FactID

func NewFactSet(facts ...FactID) (FactSet, error) {
	result := make(FactSet, len(facts))
	for _, fact := range facts {
		if err := result.Add(fact); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s FactSet) Add(fact FactID) error {
	if s == nil {
		return fmt.Errorf("%w: nil fact set", ErrInvalid)
	}
	hash, err := fact.HashBytes()
	if err != nil {
		return err
	}
	if existing, present := s[hash]; present {
		left, leftErr := existing.CanonicalPayload()
		right, rightErr := fact.CanonicalPayload()
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return fmt.Errorf("%w: fact hash collision for %s", ErrInvalid, hex.EncodeToString(hash[:]))
		}
		return nil
	}
	s[hash] = fact
	return nil
}

func (s FactSet) Clone() FactSet {
	result := make(FactSet, len(s))
	for hash, fact := range s {
		result[hash] = fact
	}
	return result
}

// Merge is retained for V1 call sites. New V2 code should use MergeChecked so
// a hypothetical hash collision cannot be silently discarded in memory.
func (s FactSet) Merge(other FactSet) {
	_ = s.MergeChecked(other)
}

func (s FactSet) MergeChecked(other FactSet) error {
	for _, fact := range other {
		if err := s.Add(fact); err != nil {
			return err
		}
	}
	return nil
}

func (s FactSet) Values() []FactID {
	hashes := make([][32]byte, 0, len(s))
	for hash := range s {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool {
		return bytes.Compare(hashes[i][:], hashes[j][:]) < 0
	})
	result := make([]FactID, 0, len(hashes))
	for _, hash := range hashes {
		result = append(result, s[hash])
	}
	return result
}

// Observation is the ledger effect of a buffered result. Influence is the
// compatibility wire/storage label for the positive-output dependency
// footprint; it does not denote minimal causal influence or physical reads.
// V3 adds Outcome facts that bind the normalized query proposition to the
// released result, including empty and zero-valued answers.
type Observation struct {
	ProfileVersion string   `json:"profile_version"`
	Release        []FactID `json:"release"`
	Influence      []FactID `json:"influence"`
	Outcome        []FactID `json:"outcome,omitempty"`
}

func (o Observation) Normalize() (Observation, error) {
	if strings.TrimSpace(o.ProfileVersion) == "" || o.ProfileVersion != strings.TrimSpace(o.ProfileVersion) {
		return Observation{}, fmt.Errorf("%w: profile_version is required", ErrInvalid)
	}
	release, err := NewFactSet(o.Release...)
	if err != nil {
		return Observation{}, err
	}
	influence, err := NewFactSet(o.Influence...)
	if err != nil {
		return Observation{}, err
	}
	outcome, err := NewFactSet(o.Outcome...)
	if err != nil {
		return Observation{}, err
	}
	for _, set := range []FactSet{release, influence} {
		for _, fact := range set {
			if (o.ProfileVersion == ProfileV2 || o.ProfileVersion == ProfileV3 || o.ProfileVersion == ProfileV4 || o.ProfileVersion == ProfileV5) && !fact.IsV2() {
				return Observation{}, fmt.Errorf("%w: V2/V3/V4/V5 release or influence set contains a non-V2 fact", ErrInvalid)
			}
			if o.ProfileVersion != ProfileV2 && o.ProfileVersion != ProfileV3 && o.ProfileVersion != ProfileV4 && o.ProfileVersion != ProfileV5 && fact.isVersioned() {
				return Observation{}, fmt.Errorf("%w: V2 fact cannot enter profile %q", ErrInvalid, o.ProfileVersion)
			}
		}
	}
	if o.ProfileVersion == ProfileV5 {
		atoms := make([]FactID, 0, len(outcome))
		var composite *FactID
		for _, fact := range outcome {
			switch {
			case fact.IsV5() && fact.Kind == FactPredicateAtom:
				atoms = append(atoms, fact)
			case fact.IsV5() && fact.Kind == FactCompositeOutcome && composite == nil:
				copy := fact
				composite = &copy
			default:
				return Observation{}, fmt.Errorf("%w: V5 outcome requires predicate atoms and exactly one composite", ErrInvalid)
			}
		}
		if composite == nil || len(outcome) != len(atoms)+1 || composite.PredicateAtomCount != int64(len(atoms)) {
			return Observation{}, fmt.Errorf("%w: V5 outcome cardinality is not atoms plus one composite", ErrInvalid)
		}
		setDigest, err := PredicateSetHashV1(atoms)
		if err != nil || setDigest != composite.PredicateSetSHA256 {
			return Observation{}, fmt.Errorf("%w: V5 composite predicate set binding mismatch", ErrInvalid)
		}
		for _, atom := range atoms {
			if atom.PredicateContextSHA256 != composite.PredicateContextSHA256 {
				return Observation{}, fmt.Errorf("%w: V5 atom/composite context mismatch", ErrInvalid)
			}
		}
	} else {
		for _, fact := range outcome {
			if (o.ProfileVersion != ProfileV3 && o.ProfileVersion != ProfileV4) || !fact.IsV3() || fact.Kind != FactOutcome {
				return Observation{}, fmt.Errorf("%w: outcome facts require profile %q or %q", ErrInvalid, ProfileV3, ProfileV4)
			}
		}
	}
	if o.ProfileVersion != ProfileV3 && o.ProfileVersion != ProfileV4 && o.ProfileVersion != ProfileV5 && len(outcome) != 0 {
		return Observation{}, fmt.Errorf("%w: profile %q cannot carry outcome facts", ErrInvalid, o.ProfileVersion)
	}
	return Observation{ProfileVersion: o.ProfileVersion, Release: release.Values(), Influence: influence.Values(), Outcome: outcome.Values()}, nil
}

func MergeObservations(profile string, observations ...Observation) (Observation, error) {
	release := make(FactSet)
	influence := make(FactSet)
	outcome := make(FactSet)
	for _, observation := range observations {
		normalized, err := observation.Normalize()
		if err != nil {
			return Observation{}, err
		}
		if normalized.ProfileVersion != profile {
			return Observation{}, fmt.Errorf("%w: mixed exposure profiles", ErrInvalid)
		}
		set, _ := NewFactSet(normalized.Release...)
		if err := release.MergeChecked(set); err != nil {
			return Observation{}, err
		}
		set, _ = NewFactSet(normalized.Influence...)
		if err := influence.MergeChecked(set); err != nil {
			return Observation{}, err
		}
		set, _ = NewFactSet(normalized.Outcome...)
		if err := outcome.MergeChecked(set); err != nil {
			return Observation{}, err
		}
	}
	return Observation{ProfileVersion: profile, Release: release.Values(), Influence: influence.Values(), Outcome: outcome.Values()}, nil
}
