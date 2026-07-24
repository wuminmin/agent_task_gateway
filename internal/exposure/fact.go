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
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

var ErrInvalid = errors.New("invalid exposure value")

const (
	RowExistenceField = "$row"
	ProfileV1         = "taskgate-exposure-v1"
	ProfileV2         = "taskgate-exposure-v2"
	factDomainV2      = "TASKGATE-FACT-V2\x00"
)

// FactKind identifies the semantic category of a V2 fact. The hash is only an
// index for this object; it is not the definition of the object.
type FactKind string

const (
	FactBaseRow  FactKind = "base-row"
	FactBaseCell FactKind = "base-cell"
	FactDerived  FactKind = "derived"
)

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
}

func (f FactID) IsV2() bool { return f.Profile != "" || f.Kind != "" }

func (f FactID) Validate() error {
	if f.IsV2() {
		return f.validateV2()
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

func (f FactID) validateV2() error {
	if f.Profile != ProfileV2 {
		return fmt.Errorf("%w: V2 fact requires profile %q", ErrInvalid, ProfileV2)
	}
	if f.Product != "" || f.ValueVersion != "" {
		return fmt.Errorf("%w: V1 and V2 fact fields cannot be mixed", ErrInvalid)
	}
	switch f.Kind {
	case FactBaseRow:
		if invalidToken(f.SourceNamespace) || invalidToken(f.Snapshot) || invalidToken(f.EntityKey) {
			return fmt.Errorf("%w: base row namespace, snapshot, and entity key are required", ErrInvalid)
		}
		if f.Field != "" || f.SQLType != "" || f.CanonicalValue != "" || len(f.SnapshotBundle) != 0 ||
			f.OutputRowKey != "" || f.NormalizedExpression != "" || f.WitnessCommitment != "" {
			return fmt.Errorf("%w: base row carries cell or derived fields", ErrInvalid)
		}
	case FactBaseCell:
		if invalidToken(f.SourceNamespace) || invalidToken(f.Snapshot) || invalidToken(f.EntityKey) ||
			invalidToken(f.Field) || invalidToken(f.SQLType) || f.CanonicalValue == "" {
			return fmt.Errorf("%w: base cell payload is incomplete", ErrInvalid)
		}
		if len(f.SnapshotBundle) != 0 || f.OutputRowKey != "" || f.NormalizedExpression != "" || f.WitnessCommitment != "" {
			return fmt.Errorf("%w: base cell carries derived fields", ErrInvalid)
		}
	case FactDerived:
		if f.SourceNamespace != "" || f.Snapshot != "" || f.EntityKey != "" || f.Field != "" ||
			invalidToken(f.OutputRowKey) || invalidToken(f.NormalizedExpression) || invalidToken(f.SQLType) ||
			f.CanonicalValue == "" || !isSHA256(f.WitnessCommitment) || len(f.SnapshotBundle) == 0 {
			return fmt.Errorf("%w: derived fact payload is incomplete or mixed", ErrInvalid)
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
	default:
		return fmt.Errorf("%w: unknown V2 fact kind %q", ErrInvalid, f.Kind)
	}
	return nil
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
	if !f.IsV2() {
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
	}
	return payload.Bytes(), nil
}

// Hash returns the durable PostgreSQL ledger index for the semantic payload.
func (f FactID) Hash() (string, error) {
	payload, err := f.CanonicalPayload()
	if err != nil {
		return "", err
	}
	if f.IsV2() {
		payload = append([]byte(factDomainV2), payload...)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func NewBaseRowFactV2(sourceNamespace, snapshot, entityKey string) (FactID, error) {
	fact := FactID{Profile: ProfileV2, Kind: FactBaseRow, SourceNamespace: sourceNamespace, Snapshot: snapshot, EntityKey: entityKey}
	return fact, fact.Validate()
}

func NewBaseCellFactV2(sourceNamespace, snapshot, entityKey, fieldID, sqlType string, value any) (FactID, error) {
	canonical, err := CanonicalSQLValue(sqlType, value)
	if err != nil {
		return FactID{}, err
	}
	fact := FactID{Profile: ProfileV2, Kind: FactBaseCell, SourceNamespace: sourceNamespace, Snapshot: snapshot,
		EntityKey: entityKey, Field: fieldID, SQLType: normalizeSQLType(sqlType), CanonicalValue: canonical}
	return fact, fact.Validate()
}

func NewDerivedFactV2(snapshotBundle []SnapshotBinding, outputRowKey, normalizedExpression, sqlType string, value any, witnessCommitment string) (FactID, error) {
	canonical, err := CanonicalSQLValue(sqlType, value)
	if err != nil {
		return FactID{}, err
	}
	fact := FactID{Profile: ProfileV2, Kind: FactDerived, SnapshotBundle: append([]SnapshotBinding(nil), snapshotBundle...),
		OutputRowKey: outputRowKey, NormalizedExpression: normalizedExpression, SQLType: normalizeSQLType(sqlType),
		CanonicalValue: canonical, WitnessCommitment: witnessCommitment}
	return fact, fact.Validate()
}

// CanonicalSQLValue encodes the supported PostgreSQL value domain without a
// float64 coercion. The SQL type is part of the fact payload, while this value
// encoding normalizes equivalent representations inside that type.
func CanonicalSQLValue(sqlType string, value any) (string, error) {
	typeName := normalizeSQLType(sqlType)
	if typeName == "" {
		return "", fmt.Errorf("%w: SQL type is required", ErrInvalid)
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
		return "i:" + integer, nil
	case "numeric", "decimal":
		number, err := canonicalNumeric(value)
		if err != nil {
			return "", err
		}
		return "n:" + number, nil
	case "real", "double precision":
		number, err := canonicalFloat(value)
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
		parsed, err := canonicalTime(value, "2006-01-02")
		if err != nil {
			return "", err
		}
		return "d:" + parsed, nil
	case "timestamp with time zone", "timestamptz":
		parsed, err := canonicalTimestamp(value, true)
		if err != nil {
			return "", err
		}
		return "tz:" + parsed, nil
	case "timestamp without time zone", "timestamp":
		parsed, err := canonicalTimestamp(value, false)
		if err != nil {
			return "", err
		}
		return "ts:" + parsed, nil
	case "json", "jsonb":
		encoded, err := canonicalJSONValue(value)
		if err != nil {
			return "", err
		}
		return "j:" + string(encoded), nil
	case "uuid":
		switch typed := value.(type) {
		case pgtype.UUID:
			if !typed.Valid {
				return "null", nil
			}
			encoded := hex.EncodeToString(typed.Bytes[:])
			return "u:" + encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
		case [16]byte:
			encoded := hex.EncodeToString(typed[:])
			return "u:" + encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
		default:
			return "u:" + strings.ToLower(fmt.Sprint(value)), nil
		}
	default:
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("%w: %T is not valid for SQL type %q", ErrInvalid, value, typeName)
		}
		return "s:" + text, nil
	}
}

func normalizeSQLType(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
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
	case float32:
		text = strconv.FormatFloat(float64(typed), 'g', -1, 32)
	case float64:
		if math.IsInf(typed, 0) || math.IsNaN(typed) {
			return "", fmt.Errorf("%w: non-finite numeric", ErrInvalid)
		}
		text = strconv.FormatFloat(typed, 'g', -1, 64)
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

func canonicalFloat(value any) (string, error) {
	var number float64
	switch typed := value.(type) {
	case float32:
		number = float64(typed)
	case float64:
		number = typed
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return "", fmt.Errorf("%w: invalid floating value", ErrInvalid)
		}
		number = parsed
	default:
		return "", fmt.Errorf("%w: %T is not floating point", ErrInvalid, value)
	}
	if math.IsInf(number, 0) || math.IsNaN(number) {
		return "", fmt.Errorf("%w: non-finite floating value", ErrInvalid)
	}
	if number == 0 {
		number = 0 // normalize negative zero
	}
	return strconv.FormatFloat(number, 'x', -1, 64), nil
}

func canonicalTime(value any, layout string) (string, error) {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC().Format(layout), nil
	case string:
		parsed, err := time.Parse(layout, typed)
		if err != nil {
			return "", fmt.Errorf("%w: invalid temporal value", ErrInvalid)
		}
		return parsed.Format(layout), nil
	default:
		return "", fmt.Errorf("%w: %T is not temporal", ErrInvalid, value)
	}
}

func canonicalTimestamp(value any, withTimezone bool) (string, error) {
	var timestamp time.Time
	switch typed := value.(type) {
	case time.Time:
		timestamp = typed
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, typed)
		if err != nil {
			return "", fmt.Errorf("%w: invalid timestamp", ErrInvalid)
		}
		timestamp = parsed
	default:
		return "", fmt.Errorf("%w: %T is not a timestamp", ErrInvalid, value)
	}
	if withTimezone {
		return timestamp.UTC().Format(time.RFC3339Nano), nil
	}
	return timestamp.Format("2006-01-02T15:04:05.999999999"), nil
}

func canonicalJSONValue(value any) ([]byte, error) {
	var decoded any
	switch typed := value.(type) {
	case []byte:
		decoder := json.NewDecoder(bytes.NewReader(typed))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("%w: invalid JSON value", ErrInvalid)
		}
	case json.RawMessage:
		decoder := json.NewDecoder(bytes.NewReader(typed))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("%w: invalid JSON value", ErrInvalid)
		}
	default:
		decoded = value
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid JSON value: %v", ErrInvalid, err)
	}
	return encoded, nil
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
type FactSet map[string]FactID

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
	hash, err := fact.Hash()
	if err != nil {
		return err
	}
	if existing, present := s[hash]; present {
		left, leftErr := existing.CanonicalPayload()
		right, rightErr := fact.CanonicalPayload()
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return fmt.Errorf("%w: fact hash collision for %s", ErrInvalid, hash)
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
	hashes := make([]string, 0, len(s))
	for hash := range s {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	result := make([]FactID, 0, len(hashes))
	for _, hash := range hashes {
		result = append(result, s[hash])
	}
	return result
}

// Observation is the dual-ledger effect of a buffered result.
type Observation struct {
	ProfileVersion string   `json:"profile_version"`
	Release        []FactID `json:"release"`
	Influence      []FactID `json:"influence"`
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
	for _, set := range []FactSet{release, influence} {
		for _, fact := range set {
			if o.ProfileVersion == ProfileV2 && (!fact.IsV2() || fact.Profile != ProfileV2) {
				return Observation{}, fmt.Errorf("%w: V2 observation contains a non-V2 fact", ErrInvalid)
			}
			if o.ProfileVersion != ProfileV2 && fact.IsV2() {
				return Observation{}, fmt.Errorf("%w: V2 fact cannot enter profile %q", ErrInvalid, o.ProfileVersion)
			}
		}
	}
	return Observation{ProfileVersion: o.ProfileVersion, Release: release.Values(), Influence: influence.Values()}, nil
}

func MergeObservations(profile string, observations ...Observation) (Observation, error) {
	release := make(FactSet)
	influence := make(FactSet)
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
	}
	return Observation{ProfileVersion: profile, Release: release.Values(), Influence: influence.Values()}, nil
}
