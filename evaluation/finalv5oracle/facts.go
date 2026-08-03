package finalv5oracle

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

// These constants are copied from the paper's V2/V5 wire contract. This
// evaluation package intentionally does not import the production exposure
// implementation that realizes the same public specification.
const (
	OracleExposureProfileV2        = "taskgate-exposure-v2"
	OracleExposureProfileV5        = "taskgate-exposure-v5"
	OraclePredicateProfileV1       = "taskgate-predicate-footprint-v1"
	OracleFactKindBaseRow          = "base-row"
	OracleFactKindBaseCell         = "base-cell"
	OracleFactKindDerived          = "derived"
	OracleFactKindPredicateAtom    = "predicate-atom"
	OracleFactKindCompositeOutcome = "composite-outcome"

	oracleFactDomainV2         = "TASKGATE-FACT-V2\x00"
	oracleFactDomainV5         = "TASKGATE-FACT-V5\x00"
	oraclePredicateSetDomainV1 = "TASKGATE-PREDICATE-SET-V1\x00"
)

var ErrInvalidCanonicalFact = errors.New("invalid independent canonical fact")

// CanonicalFact is the independent oracle's byte-level fact vector. Payload
// is Canon(kind, profile, fields...), without the hash-domain separator.
type CanonicalFact struct {
	Profile string `json:"profile"`
	Kind    string `json:"kind"`
	Payload []byte `json:"canonical_payload"`
	SHA256  string `json:"sha256"`
}

type V2BaseRowInput struct {
	SourceNamespace string
	Snapshot        string
	EntityKey       string
}

type V2BaseCellInput struct {
	SourceNamespace string
	Snapshot        string
	EntityKey       string
	Field           string
	SQLType         string
	CanonicalValue  string
}

type V2SnapshotBinding struct {
	SourceNamespace string `json:"source_namespace"`
	Snapshot        string `json:"snapshot"`
}

type V2DerivedInput struct {
	SnapshotBundle       []V2SnapshotBinding
	OutputRowKey         string
	NormalizedExpression string
	SQLType              string
	CanonicalValue       string
	WitnessCommitment    string
}

// V5PredicateAtomInput is the paper-level atom tuple. Empty AtomizerVersion
// selects the only admitted predicate-footprint version.
type V5PredicateAtomInput struct {
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

// V5CompositeOutcomeInput is the paper-level composite tuple. Empty
// PredicateProfileVersion selects the only admitted atom profile.
type V5CompositeOutcomeInput struct {
	QueryNormalFormVersion  string
	QueryNormalFormSHA256   string
	ResultObservationSHA256 string
	VisibleRows             int64
	PredicateProfileVersion string
	PredicateContextSHA256  string
	PredicateSetSHA256      string
	PredicateAtomCount      int64
}

func BuildV2BaseRowFact(input V2BaseRowInput) (CanonicalFact, error) {
	if !oracleToken(input.SourceNamespace) || !oracleToken(input.Snapshot) || !oracleToken(input.EntityKey) {
		return CanonicalFact{}, fmt.Errorf("%w: base-row namespace, snapshot, and entity key are required", ErrInvalidCanonicalFact)
	}
	payload := canonicalFactPayload(OracleFactKindBaseRow, OracleExposureProfileV2,
		input.SourceNamespace, input.Snapshot, input.EntityKey)
	return finishCanonicalFact(OracleExposureProfileV2, OracleFactKindBaseRow, payload), nil
}

func BuildV2BaseCellFact(input V2BaseCellInput) (CanonicalFact, error) {
	if !oracleToken(input.SourceNamespace) || !oracleToken(input.Snapshot) || !oracleToken(input.EntityKey) ||
		!oracleToken(input.Field) || !oracleToken(input.SQLType) || input.CanonicalValue == "" {
		return CanonicalFact{}, fmt.Errorf("%w: base-cell tuple is incomplete", ErrInvalidCanonicalFact)
	}
	if !oracleCanonicalSQLType(input.SQLType) {
		return CanonicalFact{}, fmt.Errorf("%w: SQL type %q is not canonical", ErrInvalidCanonicalFact, input.SQLType)
	}
	if err := validateOracleCanonicalValue(input.SQLType, input.CanonicalValue); err != nil {
		return CanonicalFact{}, fmt.Errorf("%w: base-cell value: %v", ErrInvalidCanonicalFact, err)
	}
	payload := canonicalFactPayload(OracleFactKindBaseCell, OracleExposureProfileV2,
		input.SourceNamespace, input.Snapshot, input.EntityKey, input.Field, input.SQLType, input.CanonicalValue)
	return finishCanonicalFact(OracleExposureProfileV2, OracleFactKindBaseCell, payload), nil
}

// BuildV2DerivedFact implements the paper's derived release tuple. The
// snapshot bundle is copied and sorted by source namespace (then snapshot), so
// caller enumeration order is not semantic identity.
func BuildV2DerivedFact(input V2DerivedInput) (CanonicalFact, error) {
	if len(input.SnapshotBundle) == 0 || !oracleToken(input.OutputRowKey) ||
		!oracleToken(input.NormalizedExpression) || !oracleToken(input.SQLType) || input.CanonicalValue == "" ||
		!validSHA256(input.WitnessCommitment) {
		return CanonicalFact{}, fmt.Errorf("%w: derived tuple is incomplete", ErrInvalidCanonicalFact)
	}
	if !oracleCanonicalSQLType(input.SQLType) {
		return CanonicalFact{}, fmt.Errorf("%w: SQL type %q is not canonical", ErrInvalidCanonicalFact, input.SQLType)
	}
	if err := validateOracleCanonicalValue(input.SQLType, input.CanonicalValue); err != nil {
		return CanonicalFact{}, fmt.Errorf("%w: derived value: %v", ErrInvalidCanonicalFact, err)
	}
	bindings := append([]V2SnapshotBinding(nil), input.SnapshotBundle...)
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].SourceNamespace != bindings[j].SourceNamespace {
			return bindings[i].SourceNamespace < bindings[j].SourceNamespace
		}
		return bindings[i].Snapshot < bindings[j].Snapshot
	})
	for index, binding := range bindings {
		if !oracleToken(binding.SourceNamespace) || !oracleToken(binding.Snapshot) {
			return CanonicalFact{}, fmt.Errorf("%w: derived snapshot binding %d is incomplete", ErrInvalidCanonicalFact, index+1)
		}
		if index > 0 && binding.SourceNamespace == bindings[index-1].SourceNamespace {
			return CanonicalFact{}, fmt.Errorf("%w: duplicate derived snapshot namespace %q", ErrInvalidCanonicalFact, binding.SourceNamespace)
		}
	}
	var payload bytes.Buffer
	oracleWriteCanonicalString(&payload, OracleFactKindDerived)
	oracleWriteCanonicalString(&payload, OracleExposureProfileV2)
	oracleWriteCanonicalUint64(&payload, uint64(len(bindings)))
	for _, binding := range bindings {
		oracleWriteCanonicalString(&payload, binding.SourceNamespace)
		oracleWriteCanonicalString(&payload, binding.Snapshot)
	}
	oracleWriteCanonicalString(&payload, input.OutputRowKey)
	oracleWriteCanonicalString(&payload, input.NormalizedExpression)
	oracleWriteCanonicalString(&payload, input.SQLType)
	oracleWriteCanonicalString(&payload, input.CanonicalValue)
	oracleWriteCanonicalString(&payload, input.WitnessCommitment)
	return finishCanonicalFact(OracleExposureProfileV2, OracleFactKindDerived, payload.Bytes()), nil
}

func BuildV5PredicateAtomFact(input V5PredicateAtomInput) (CanonicalFact, error) {
	if input.AtomizerVersion == "" {
		input.AtomizerVersion = OraclePredicateProfileV1
	}
	if input.AtomizerVersion != OraclePredicateProfileV1 || !validSHA256(input.PredicateContextSHA256) ||
		!oracleToken(input.SemanticProductID) || !oracleToken(input.StableRole) || !oracleToken(input.PublicFieldID) ||
		!oracleToken(input.SQLType) || !oracleToken(input.Operator) || input.CanonicalLiteral == "" {
		return CanonicalFact{}, fmt.Errorf("%w: predicate-atom tuple is incomplete", ErrInvalidCanonicalFact)
	}
	if input.ResolvedExpressionSHA256 != "" && !validSHA256(input.ResolvedExpressionSHA256) {
		return CanonicalFact{}, fmt.Errorf("%w: resolved expression is not lowercase SHA-256", ErrInvalidCanonicalFact)
	}
	if !oracleCanonicalSQLType(input.SQLType) {
		return CanonicalFact{}, fmt.Errorf("%w: SQL type %q is not canonical", ErrInvalidCanonicalFact, input.SQLType)
	}
	if err := validateOracleCanonicalValue(input.SQLType, input.CanonicalLiteral); err != nil {
		return CanonicalFact{}, fmt.Errorf("%w: predicate literal: %v", ErrInvalidCanonicalFact, err)
	}
	if oracleCollatableType(input.SQLType) {
		if !oracleToken(input.CollationName) || !oracleToken(input.CollationVersion) {
			return CanonicalFact{}, fmt.Errorf("%w: collatable atom has no collation binding", ErrInvalidCanonicalFact)
		}
	} else if input.CollationName != "" || input.CollationVersion != "" {
		return CanonicalFact{}, fmt.Errorf("%w: non-collatable atom carries a collation", ErrInvalidCanonicalFact)
	}
	switch input.Operator {
	case "EQ", "NE", "LT", "LE", "GT", "GE", "LIKE":
	default:
		return CanonicalFact{}, fmt.Errorf("%w: unsupported predicate operator %q", ErrInvalidCanonicalFact, input.Operator)
	}
	payload := canonicalFactPayload(OracleFactKindPredicateAtom, OracleExposureProfileV5,
		input.AtomizerVersion, input.PredicateContextSHA256, input.SemanticProductID,
		input.StableRole, input.PublicFieldID, input.ResolvedExpressionSHA256,
		input.SQLType, input.CollationName, input.CollationVersion, input.Operator, input.CanonicalLiteral)
	return finishCanonicalFact(OracleExposureProfileV5, OracleFactKindPredicateAtom, payload), nil
}

func BuildV5CompositeOutcomeFact(input V5CompositeOutcomeInput) (CanonicalFact, error) {
	if input.PredicateProfileVersion == "" {
		input.PredicateProfileVersion = OraclePredicateProfileV1
	}
	if !oracleToken(input.QueryNormalFormVersion) || !validSHA256(input.QueryNormalFormSHA256) ||
		!validSHA256(input.ResultObservationSHA256) || input.VisibleRows < 0 ||
		input.PredicateProfileVersion != OraclePredicateProfileV1 || !validSHA256(input.PredicateContextSHA256) ||
		!validSHA256(input.PredicateSetSHA256) || input.PredicateAtomCount < 0 {
		return CanonicalFact{}, fmt.Errorf("%w: composite-outcome tuple is incomplete", ErrInvalidCanonicalFact)
	}
	var payload bytes.Buffer
	oracleWriteCanonicalString(&payload, OracleFactKindCompositeOutcome)
	oracleWriteCanonicalString(&payload, OracleExposureProfileV5)
	oracleWriteCanonicalString(&payload, input.QueryNormalFormVersion)
	oracleWriteCanonicalString(&payload, input.QueryNormalFormSHA256)
	oracleWriteCanonicalString(&payload, input.ResultObservationSHA256)
	oracleWriteCanonicalUint64(&payload, uint64(input.VisibleRows))
	oracleWriteCanonicalString(&payload, input.PredicateProfileVersion)
	oracleWriteCanonicalString(&payload, input.PredicateContextSHA256)
	oracleWriteCanonicalString(&payload, input.PredicateSetSHA256)
	oracleWriteCanonicalUint64(&payload, uint64(input.PredicateAtomCount))
	return finishCanonicalFact(OracleExposureProfileV5, OracleFactKindCompositeOutcome, payload.Bytes()), nil
}

// HashV5PredicateSet commits the sorted, duplicate-free raw atom hashes. It
// validates each supplied vector and never accepts opaque Merkle-control
// members as semantic V5 atoms.
func HashV5PredicateSet(atoms []CanonicalFact) (string, error) {
	members := make(map[[sha256.Size]byte][]byte, len(atoms))
	for index, atom := range atoms {
		if atom.Profile != OracleExposureProfileV5 || atom.Kind != OracleFactKindPredicateAtom {
			return "", fmt.Errorf("%w: predicate member %d is not a V5 atom", ErrInvalidCanonicalFact, index+1)
		}
		if err := ValidateCanonicalFact(atom); err != nil {
			return "", fmt.Errorf("predicate member %d: %w", index+1, err)
		}
		decoded, _ := hex.DecodeString(atom.SHA256)
		var key [sha256.Size]byte
		copy(key[:], decoded)
		if previous, exists := members[key]; exists && !bytes.Equal(previous, atom.Payload) {
			return "", fmt.Errorf("%w: predicate atom SHA-256 collision", ErrInvalidCanonicalFact)
		}
		members[key] = append([]byte(nil), atom.Payload...)
	}
	ordered := make([][sha256.Size]byte, 0, len(members))
	for member := range members {
		ordered = append(ordered, member)
	}
	sortOracleHashes(ordered)
	h := sha256.New()
	_, _ = h.Write([]byte(oraclePredicateSetDomainV1))
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(ordered)))
	_, _ = h.Write(count[:])
	for _, member := range ordered {
		_, _ = h.Write(member[:])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ValidateCanonicalFact catches payload/hash mutation in fixtures and Oracle
// manifests. It checks the domain-bound hash and the leading kind/profile
// framing; builders perform the stronger per-field validation.
func ValidateCanonicalFact(fact CanonicalFact) error {
	if !oracleToken(fact.Profile) || !oracleToken(fact.Kind) || len(fact.Payload) == 0 || !validSHA256(fact.SHA256) {
		return fmt.Errorf("%w: fact metadata is incomplete", ErrInvalidCanonicalFact)
	}
	domain := ""
	switch fact.Profile {
	case OracleExposureProfileV2:
		domain = oracleFactDomainV2
		if fact.Kind != OracleFactKindBaseRow && fact.Kind != OracleFactKindBaseCell && fact.Kind != OracleFactKindDerived {
			return fmt.Errorf("%w: V2 kind %q is not supported by this oracle", ErrInvalidCanonicalFact, fact.Kind)
		}
	case OracleExposureProfileV5:
		domain = oracleFactDomainV5
		if fact.Kind != OracleFactKindPredicateAtom && fact.Kind != OracleFactKindCompositeOutcome {
			return fmt.Errorf("%w: V5 kind %q is not supported by this oracle", ErrInvalidCanonicalFact, fact.Kind)
		}
	default:
		return fmt.Errorf("%w: unsupported profile %q", ErrInvalidCanonicalFact, fact.Profile)
	}
	first, rest, ok := oracleReadCanonicalString(fact.Payload)
	if !ok || first != fact.Kind {
		return fmt.Errorf("%w: payload kind does not match metadata", ErrInvalidCanonicalFact)
	}
	second, _, ok := oracleReadCanonicalString(rest)
	if !ok || second != fact.Profile {
		return fmt.Errorf("%w: payload profile does not match metadata", ErrInvalidCanonicalFact)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	_, _ = digest.Write(fact.Payload)
	if hex.EncodeToString(digest.Sum(nil)) != fact.SHA256 {
		return fmt.Errorf("%w: payload/hash mismatch", ErrInvalidCanonicalFact)
	}
	return nil
}

// ComposeOracleCanonicalKeyV2 is the evaluation-only copy of the paper's
// ordered, length-delimited semantic-key construction.
func ComposeOracleCanonicalKeyV2(domain string, values ...string) (string, error) {
	if !oracleToken(domain) || len(values) == 0 {
		return "", fmt.Errorf("%w: key domain and values are required", ErrInvalidCanonicalFact)
	}
	var payload bytes.Buffer
	oracleWriteCanonicalString(&payload, domain)
	oracleWriteCanonicalUint64(&payload, uint64(len(values)))
	for _, value := range values {
		oracleWriteCanonicalString(&payload, value)
	}
	digest := sha256.Sum256(payload.Bytes())
	return hex.EncodeToString(digest[:]), nil
}

func canonicalFactPayload(kind, profile string, fields ...string) []byte {
	var payload bytes.Buffer
	oracleWriteCanonicalString(&payload, kind)
	oracleWriteCanonicalString(&payload, profile)
	for _, field := range fields {
		oracleWriteCanonicalString(&payload, field)
	}
	return payload.Bytes()
}

func finishCanonicalFact(profile, kind string, payload []byte) CanonicalFact {
	domain := oracleFactDomainV2
	if profile == OracleExposureProfileV5 {
		domain = oracleFactDomainV5
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	_, _ = digest.Write(payload)
	return CanonicalFact{Profile: profile, Kind: kind, Payload: append([]byte(nil), payload...), SHA256: hex.EncodeToString(digest.Sum(nil))}
}

func oracleWriteCanonicalString(buffer *bytes.Buffer, value string) {
	oracleWriteCanonicalUint64(buffer, uint64(len(value)))
	_, _ = buffer.WriteString(value)
}

func oracleWriteCanonicalUint64(buffer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = buffer.Write(encoded[:])
}

func oracleReadCanonicalString(value []byte) (string, []byte, bool) {
	if len(value) < 8 {
		return "", nil, false
	}
	length := binary.BigEndian.Uint64(value[:8])
	if length > uint64(len(value)-8) {
		return "", nil, false
	}
	end := 8 + int(length)
	return string(value[8:end]), value[end:], true
}

func sortOracleHashes(values [][sha256.Size]byte) {
	// A small local insertion sort avoids importing a semantic implementation;
	// predicate footprints are bounded to a handful of atoms by the contract.
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && bytes.Compare(values[j][:], values[j-1][:]) < 0; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func oracleToken(value string) bool {
	return strings.TrimSpace(value) != "" && value == strings.TrimSpace(value)
}

func oracleCanonicalSQLType(value string) bool {
	switch value {
	case "smallint", "integer", "bigint", "numeric", "real", "double precision", "boolean", "bytea", "date",
		"time without time zone", "timestamp with time zone", "timestamp without time zone", "jsonb", "uuid",
		"text", "character", "character varying":
		return true
	default:
		return false
	}
}

func oracleCollatableType(value string) bool {
	return value == "text" || value == "character" || value == "character varying"
}

func validateOracleCanonicalValue(sqlType, value string) error {
	if value == "null" {
		return nil
	}
	prefix := func(want string) (string, error) {
		if !strings.HasPrefix(value, want) {
			return "", fmt.Errorf("type %q requires prefix %q", sqlType, want)
		}
		return strings.TrimPrefix(value, want), nil
	}
	switch sqlType {
	case "smallint", "integer", "bigint":
		text, err := prefix("i:")
		if err != nil {
			return err
		}
		integer, ok := new(big.Int).SetString(text, 10)
		if !ok || integer.String() != text {
			return errors.New("integer is not in canonical decimal form")
		}
		bits := uint(64)
		if sqlType == "smallint" {
			bits = 16
		} else if sqlType == "integer" {
			bits = 32
		}
		minimum := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), bits-1))
		maximum := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bits-1), big.NewInt(1))
		if integer.Cmp(minimum) < 0 || integer.Cmp(maximum) > 0 {
			return fmt.Errorf("integer is outside PostgreSQL %s range", sqlType)
		}
		return nil
	case "numeric":
		text, err := prefix("n:")
		if err != nil {
			return err
		}
		switch text {
		case "nan", "+infinity", "-infinity":
			return nil
		}
		rational, ok := new(big.Rat).SetString(text)
		if !ok || rational.RatString() != text {
			return errors.New("numeric is not a reduced canonical rational")
		}
		return nil
	case "real", "double precision":
		text, err := prefix("f:")
		if err != nil {
			return err
		}
		if text == "nan" || text == "+infinity" || text == "-infinity" {
			return nil
		}
		_, err = strconv.ParseFloat(text, map[bool]int{true: 32, false: 64}[sqlType == "real"])
		return err
	case "boolean":
		text, err := prefix("b:")
		if err != nil {
			return err
		}
		if text != "true" && text != "false" {
			return errors.New("boolean is not canonical")
		}
		return nil
	case "bytea":
		text, err := prefix("x:")
		if err != nil {
			return err
		}
		decoded, err := hex.DecodeString(text)
		if err != nil || hex.EncodeToString(decoded) != text {
			return errors.New("bytea is not lowercase hexadecimal")
		}
		return nil
	case "date":
		text, err := prefix("d:")
		if err != nil {
			return err
		}
		parsed, err := time.Parse("2006-01-02", text)
		if err != nil || parsed.Format("2006-01-02") != text {
			return errors.New("date is not canonical ISO-8601")
		}
		return nil
	case "time without time zone":
		text, err := prefix("tm:")
		if err != nil {
			return err
		}
		micros, err := strconv.ParseInt(text, 10, 64)
		if err != nil || strconv.FormatInt(micros, 10) != text || micros < 0 || micros > int64(24*time.Hour/time.Microsecond) {
			return errors.New("time is not canonical microseconds-since-midnight")
		}
		return nil
	case "timestamp with time zone":
		text, err := prefix("tz:")
		if err != nil {
			return err
		}
		if text == "infinity" || text == "-infinity" {
			return nil
		}
		_, err = time.Parse(time.RFC3339Nano, text)
		return err
	case "timestamp without time zone":
		text, err := prefix("ts:")
		if err != nil {
			return err
		}
		if text == "infinity" || text == "-infinity" {
			return nil
		}
		_, err = time.Parse("2006-01-02T15:04:05.999999999", text)
		return err
	case "uuid":
		text, err := prefix("u:")
		if err != nil {
			return err
		}
		compact := strings.ReplaceAll(text, "-", "")
		decoded, err := hex.DecodeString(compact)
		if err != nil || len(decoded) != 16 || strings.ToLower(text) != text || len(text) != 36 {
			return errors.New("UUID is not canonical lowercase text")
		}
		return nil
	case "text", "character", "character varying":
		_, err := prefix("s:")
		return err
	case "jsonb":
		text, err := prefix("j:")
		if err != nil || text == "" {
			return errors.New("jsonb encoding is empty or lacks its prefix")
		}
		// Full JSONB normalization is exercised by the Result Oracle. At this
		// boundary the atom contract additionally excludes surrounding space.
		if text != strings.TrimSpace(text) {
			return errors.New("jsonb encoding has boundary whitespace")
		}
		return nil
	default:
		return fmt.Errorf("unsupported canonical SQL type %q", sqlType)
	}
}
