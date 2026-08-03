package finalv5oracle

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	resultSchemaDomainV1 = "TASKGATE-FINAL-V5-RESULT-SCHEMA-V1\x00"
	resultRowsDomainV1   = "TASKGATE-FINAL-V5-RESULT-ROWS-V1\x00"
	resultDigestDomainV1 = "TASKGATE-FINAL-V5-CANONICAL-RESULT-V1\x00"
)

// SQLType is the evaluation-only SQL type committed by the canonical result
// format. It deliberately covers only the types approved by the final V5
// result-heavy contract.
type SQLType string

const (
	SQLBigInt                SQLType = "bigint"
	SQLInteger               SQLType = "integer"
	SQLNumeric               SQLType = "numeric"
	SQLText                  SQLType = "text"
	SQLDate                  SQLType = "date"
	SQLTimestampWithoutTZ    SQLType = "timestamp without time zone"
	SQLBoolean               SQLType = "boolean"
	maxCanonicalNumericBytes         = 1 << 20
)

var timestampWithoutTZPattern = regexp.MustCompile(
	`^[0-9]{4}-[0-9]{2}-[0-9]{2}[ T][0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?$`,
)

// ResultColumn is one ordered logical result column. Names are preserved
// exactly; SQL type aliases are normalized before the schema is hashed.
type ResultColumn struct {
	Name string  `json:"name"`
	Type SQLType `json:"sql_type"`
}

// TypedValue is an immutable, independently normalized logical SQL value.
// CanonicalBytes returns a defensive copy so callers cannot mutate a value
// after it has entered a streaming result hash.
type TypedValue struct {
	sqlType   SQLType
	null      bool
	canonical []byte
}

func (value TypedValue) SQLType() SQLType { return value.sqlType }
func (value TypedValue) IsNull() bool     { return value.null }
func (value TypedValue) CanonicalBytes() []byte {
	return append([]byte(nil), value.canonical...)
}

// NormalizeSQLType maps the small approved alias surface to the exact names
// committed by ResultColumn.
func NormalizeSQLType(value string) (SQLType, error) {
	normalized := strings.ToLower(strings.Join(strings.Fields(value), " "))
	switch normalized {
	case "bigint", "int8":
		return SQLBigInt, nil
	case "integer", "int", "int4":
		return SQLInteger, nil
	case "numeric", "decimal":
		return SQLNumeric, nil
	case "text", "varchar", "character varying":
		return SQLText, nil
	case "date":
		return SQLDate, nil
	case "timestamp", "timestamp without time zone":
		return SQLTimestampWithoutTZ, nil
	case "boolean", "bool":
		return SQLBoolean, nil
	default:
		return "", fmt.Errorf("SQL type %q is outside the final V5 canonical-result contract", value)
	}
}

// SQLTypeFromPostgresOID maps PostgreSQL field descriptions to the same
// evaluation-only type vocabulary used by delivered Parquet rows.
func SQLTypeFromPostgresOID(oid uint32) (SQLType, error) {
	switch oid {
	case 20:
		return SQLBigInt, nil
	case 23:
		return SQLInteger, nil
	case 1700:
		return SQLNumeric, nil
	case 25, 1042, 1043:
		return SQLText, nil
	case 1082:
		return SQLDate, nil
	case 1114:
		return SQLTimestampWithoutTZ, nil
	case 16:
		return SQLBoolean, nil
	default:
		return "", fmt.Errorf("PostgreSQL type OID %d is outside the final V5 canonical-result contract", oid)
	}
}

// NormalizeResultSchema validates names, normalizes SQL types, and returns a
// detached ordered schema.
func NormalizeResultSchema(columns []ResultColumn) ([]ResultColumn, error) {
	if len(columns) == 0 {
		return nil, errors.New("canonical result schema is empty")
	}
	normalized := make([]ResultColumn, len(columns))
	seen := make(map[string]bool, len(columns))
	for index, column := range columns {
		if strings.TrimSpace(column.Name) == "" || column.Name != strings.TrimSpace(column.Name) ||
			strings.ContainsRune(column.Name, '\x00') || !utf8.ValidString(column.Name) || seen[column.Name] {
			return nil, fmt.Errorf("canonical result column %d has an invalid or duplicate name", index+1)
		}
		typeName, err := NormalizeSQLType(string(column.Type))
		if err != nil {
			return nil, fmt.Errorf("canonical result column %q: %w", column.Name, err)
		}
		seen[column.Name] = true
		normalized[index] = ResultColumn{Name: column.Name, Type: typeName}
	}
	return normalized, nil
}

func canonicalResultSchemaPayload(columns []ResultColumn) []byte {
	var payload bytes.Buffer
	writeOracleUint64(&payload, uint64(len(columns)))
	for _, column := range columns {
		writeOracleString(&payload, column.Name)
		writeOracleString(&payload, string(column.Type))
	}
	return payload.Bytes()
}

// NormalizeTypedValue converts a transport value to one exact typed value.
// Numeric inputs are exact decimal strings or integers; binary floating-point
// inputs are rejected rather than silently changing a PostgreSQL NUMERIC.
func NormalizeTypedValue(sqlType SQLType, raw any) (TypedValue, error) {
	normalizedType, err := NormalizeSQLType(string(sqlType))
	if err != nil {
		return TypedValue{}, err
	}
	if raw == nil {
		return TypedValue{sqlType: normalizedType, null: true}, nil
	}
	value := TypedValue{sqlType: normalizedType}
	switch normalizedType {
	case SQLBigInt:
		integer, integerErr := canonicalSignedInteger(raw, 64)
		if integerErr != nil {
			return TypedValue{}, integerErr
		}
		value.canonical = make([]byte, 8)
		binary.BigEndian.PutUint64(value.canonical, uint64(integer))
	case SQLInteger:
		integer, integerErr := canonicalSignedInteger(raw, 32)
		if integerErr != nil {
			return TypedValue{}, integerErr
		}
		value.canonical = make([]byte, 4)
		binary.BigEndian.PutUint32(value.canonical, uint32(int32(integer)))
	case SQLNumeric:
		numeric, numericErr := canonicalDecimal(raw)
		if numericErr != nil {
			return TypedValue{}, numericErr
		}
		value.canonical = []byte(numeric)
	case SQLText:
		text, ok := raw.(string)
		if !ok || !utf8.ValidString(text) {
			return TypedValue{}, fmt.Errorf("%T is not valid UTF-8 text", raw)
		}
		value.canonical = []byte(text)
	case SQLDate:
		date, dateErr := canonicalDateValue(raw)
		if dateErr != nil {
			return TypedValue{}, dateErr
		}
		value.canonical = []byte(date)
	case SQLTimestampWithoutTZ:
		timestamp, timestampErr := canonicalTimestampWithoutTZ(raw)
		if timestampErr != nil {
			return TypedValue{}, timestampErr
		}
		value.canonical = []byte(timestamp)
	case SQLBoolean:
		boolean, ok := raw.(bool)
		if !ok {
			return TypedValue{}, fmt.Errorf("%T is not boolean", raw)
		}
		if boolean {
			value.canonical = []byte{1}
		} else {
			value.canonical = []byte{0}
		}
	}
	return value, nil
}

func canonicalSignedInteger(raw any, bits int) (int64, error) {
	var value int64
	switch typed := raw.(type) {
	case int:
		value = int64(typed)
	case int8:
		value = int64(typed)
	case int16:
		value = int64(typed)
	case int32:
		value = int64(typed)
	case int64:
		value = typed
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, errors.New("unsigned integer exceeds bigint")
		}
		value = int64(typed)
	case uint8:
		value = int64(typed)
	case uint16:
		value = int64(typed)
	case uint32:
		value = int64(typed)
	case uint64:
		if typed > math.MaxInt64 {
			return 0, errors.New("unsigned integer exceeds bigint")
		}
		value = int64(typed)
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, bits)
		if err != nil {
			return 0, fmt.Errorf("invalid %d-bit integer: %w", bits, err)
		}
		value = parsed
	default:
		return 0, fmt.Errorf("%T is not an exact integer", raw)
	}
	if bits == 32 && (value < math.MinInt32 || value > math.MaxInt32) {
		return 0, errors.New("integer is outside PostgreSQL integer range")
	}
	return value, nil
}

func canonicalDecimal(raw any) (string, error) {
	var text string
	switch typed := raw.(type) {
	case string:
		text = typed
	case []byte:
		text = string(typed)
	case json.Number:
		text = string(typed)
	case int:
		text = strconv.FormatInt(int64(typed), 10)
	case int8:
		text = strconv.FormatInt(int64(typed), 10)
	case int16:
		text = strconv.FormatInt(int64(typed), 10)
	case int32:
		text = strconv.FormatInt(int64(typed), 10)
	case int64:
		text = strconv.FormatInt(typed, 10)
	case uint:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint8:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint16:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint32:
		text = strconv.FormatUint(uint64(typed), 10)
	case uint64:
		text = strconv.FormatUint(typed, 10)
	case pgtype.Numeric:
		return canonicalPGXNumeric(typed)
	case *pgtype.Numeric:
		if typed == nil {
			return "", errors.New("pgx numeric pointer is nil")
		}
		return canonicalPGXNumeric(*typed)
	default:
		return "", fmt.Errorf("%T is not an exact numeric value", raw)
	}
	if len(text) == 0 || len(text) > maxCanonicalNumericBytes || text != strings.TrimSpace(text) {
		return "", errors.New("numeric text is empty, oversized, or padded")
	}
	negative := false
	if text[0] == '+' || text[0] == '-' {
		negative = text[0] == '-'
		text = text[1:]
	}
	if text == "" {
		return "", errors.New("numeric value has no digits")
	}
	exponent := 0
	if position := strings.IndexAny(text, "eE"); position >= 0 {
		if strings.IndexAny(text[position+1:], "eE") >= 0 {
			return "", errors.New("numeric value contains multiple exponents")
		}
		parsed, err := strconv.ParseInt(text[position+1:], 10, 32)
		if err != nil || parsed < -maxCanonicalNumericBytes || parsed > maxCanonicalNumericBytes {
			return "", errors.New("numeric exponent is invalid or too large")
		}
		exponent = int(parsed)
		text = text[:position]
	}
	point := strings.IndexByte(text, '.')
	if point >= 0 && strings.IndexByte(text[point+1:], '.') >= 0 {
		return "", errors.New("numeric value contains multiple decimal points")
	}
	fractionalDigits := 0
	if point >= 0 {
		fractionalDigits = len(text) - point - 1
		text = text[:point] + text[point+1:]
	}
	if text == "" {
		return "", errors.New("numeric value has no digits")
	}
	for _, character := range text {
		if character < '0' || character > '9' {
			return "", errors.New("numeric value contains a non-digit")
		}
	}
	digits := strings.TrimLeft(text, "0")
	if digits == "" {
		return "0", nil
	}
	scale := fractionalDigits - exponent
	if scale <= 0 {
		if -scale > maxCanonicalNumericBytes-len(digits) {
			return "", errors.New("numeric expansion is too large")
		}
		digits += strings.Repeat("0", -scale)
		if negative {
			return "-" + digits, nil
		}
		return digits, nil
	}
	var result string
	if scale >= len(digits) {
		if scale-len(digits) > maxCanonicalNumericBytes-len(digits) {
			return "", errors.New("numeric expansion is too large")
		}
		result = "0." + strings.Repeat("0", scale-len(digits)) + digits
	} else {
		position := len(digits) - scale
		result = digits[:position] + "." + digits[position:]
	}
	result = strings.TrimRight(result, "0")
	result = strings.TrimSuffix(result, ".")
	if negative {
		result = "-" + result
	}
	return result, nil
}

func canonicalPGXNumeric(value pgtype.Numeric) (string, error) {
	if !value.Valid || value.Int == nil || value.NaN || value.InfinityModifier != pgtype.Finite {
		return "", errors.New("pgx numeric is invalid, non-finite, or lacks a coefficient")
	}
	text := value.Int.String()
	if value.Exp != 0 {
		text += "e" + strconv.FormatInt(int64(value.Exp), 10)
	}
	return canonicalDecimal(text)
}

func canonicalDateValue(raw any) (string, error) {
	switch typed := raw.(type) {
	case time.Time:
		return typed.Format("2006-01-02"), nil
	case string:
		parsed, err := time.Parse("2006-01-02", typed)
		if err != nil || parsed.Format("2006-01-02") != typed {
			return "", fmt.Errorf("invalid date %q", typed)
		}
		return typed, nil
	default:
		return "", fmt.Errorf("%T is not a date", raw)
	}
}

func canonicalTimestampWithoutTZ(raw any) (string, error) {
	const layout = "2006-01-02 15:04:05.999999999"
	var parsed time.Time
	switch typed := raw.(type) {
	case time.Time:
		parsed = typed
	case string:
		if !timestampWithoutTZPattern.MatchString(typed) {
			return "", fmt.Errorf("invalid timestamp without time zone %q", typed)
		}
		normalized := typed[:10] + " " + typed[11:]
		value, err := time.ParseInLocation(layout, normalized, time.UTC)
		if err != nil {
			return "", fmt.Errorf("invalid timestamp without time zone %q", typed)
		}
		parsed = value
	default:
		return "", fmt.Errorf("%T is not a timestamp without time zone", raw)
	}
	return parsed.Format(layout), nil
}

type oracleByteWriter interface {
	Write([]byte) (int, error)
}

func writeOracleUint64(writer oracleByteWriter, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func writeOracleBytes(writer oracleByteWriter, value []byte) {
	writeOracleUint64(writer, uint64(len(value)))
	_, _ = writer.Write(value)
}

func writeOracleString(writer oracleByteWriter, value string) {
	writeOracleBytes(writer, []byte(value))
}
