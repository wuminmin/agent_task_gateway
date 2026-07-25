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
		canonicalType, err := CanonicalSQLTypeV2(f.SQLType)
		if err != nil || canonicalType != f.SQLType {
			return fmt.Errorf("%w: base cell SQL type is not canonical", ErrInvalid)
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
	case "time with time zone":
		return "", fmt.Errorf("%w: PostgreSQL time with time zone is outside %s", ErrInvalid, ProfileV2)
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
	case "json", "jsonb":
		encoded, err := canonicalJSONValue(value)
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
		"timestamp with time zone", "timestamp without time zone", "json", "jsonb",
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

func canonicalJSONValue(value any) ([]byte, error) {
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
