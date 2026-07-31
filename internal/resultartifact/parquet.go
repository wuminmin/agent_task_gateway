package resultartifact

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/parquet-go/parquet-go"
)

type Column struct {
	Name        string `json:"name"`
	DataTypeOID uint32 `json:"data_type_oid"`
}

type ColumnSchema struct {
	Name         string `json:"name"`
	PhysicalName string `json:"physical_name"`
	DataTypeOID  uint32 `json:"data_type_oid"`
	ParquetType  string `json:"parquet_type"`
}

type columnEncoding struct {
	schema         ColumnSchema
	typeOf         reflect.Type
	parquetOptions []string
	value          func(any) (parquet.Value, error)
	decode         func(parquet.Value) (any, error)
}

func WriteParquet(output io.Writer, resultID string, columns []Column, rows [][]any) ([]ColumnSchema, error) {
	if output == nil || strings.TrimSpace(resultID) == "" || len(columns) == 0 {
		return nil, errors.New("Parquet output, result ID, and columns are required")
	}
	encodings, structType, err := buildColumnEncodings(columns)
	if err != nil {
		return nil, err
	}
	schema := parquet.SchemaOf(reflect.New(structType).Interface())
	writer := parquet.NewGenericWriter[any](output, schema, parquet.MaxRowsPerRowGroup(64*1024))
	writer.SetKeyValueMetadata("taskgate.format", "taskgate-result-parquet-v1")
	writer.SetKeyValueMetadata("taskgate.result_id", resultID)
	schemaJSON, _ := json.Marshal(columnSchemas(encodings))
	writer.SetKeyValueMetadata("taskgate.schema", string(schemaJSON))
	builder := parquet.NewRowBuilder(schema)
	batch := make([]parquet.Row, 0, 128)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		written, err := writer.WriteRows(batch)
		if err != nil {
			return err
		}
		if written != len(batch) {
			return io.ErrShortWrite
		}
		batch = batch[:0]
		return nil
	}
	for rowIndex, row := range rows {
		if len(row) != len(encodings) {
			return nil, fmt.Errorf("row %d has %d values; expected %d", rowIndex, len(row), len(encodings))
		}
		builder.Reset()
		for columnIndex, raw := range row {
			if raw == nil {
				continue
			}
			value, err := encodings[columnIndex].value(raw)
			if err != nil {
				return nil, fmt.Errorf("row %d column %q: %w", rowIndex, columns[columnIndex].Name, err)
			}
			builder.Add(columnIndex, value)
		}
		batch = append(batch, builder.Row().Clone())
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return columnSchemas(encodings), nil
}

func ReadParquet(value []byte, resultID string, schema []ColumnSchema, offset, limit int64) ([][]any, error) {
	if len(value) < 8 || string(value[:4]) != "PAR1" || string(value[len(value)-4:]) != "PAR1" ||
		strings.TrimSpace(resultID) == "" || len(schema) == 0 || offset < 0 || limit <= 0 {
		return nil, errors.New("invalid Parquet preview request")
	}
	columns := make([]Column, len(schema))
	for index := range schema {
		columns[index] = Column{Name: schema[index].Name, DataTypeOID: schema[index].DataTypeOID}
	}
	encodings, _, err := buildColumnEncodings(columns)
	if err != nil {
		return nil, err
	}
	for index := range encodings {
		if encodings[index].schema.PhysicalName != schema[index].PhysicalName ||
			encodings[index].schema.ParquetType != schema[index].ParquetType {
			return nil, errors.New("Parquet schema metadata is inconsistent")
		}
	}
	reader := parquet.NewReader(bytes.NewReader(value))
	defer reader.Close()
	format, formatOK := reader.File().Lookup("taskgate.format")
	storedResultID, resultIDOK := reader.File().Lookup("taskgate.result_id")
	storedSchema, schemaOK := reader.File().Lookup("taskgate.schema")
	expectedSchema, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	if !formatOK || format != "taskgate-result-parquet-v1" || !resultIDOK || storedResultID != resultID ||
		!schemaOK || storedSchema != string(expectedSchema) {
		return nil, errors.New("Parquet identity metadata is inconsistent")
	}
	if offset >= reader.NumRows() {
		return [][]any{}, nil
	}
	if err := reader.SeekToRow(offset); err != nil {
		return nil, err
	}
	remaining := reader.NumRows() - offset
	if limit > remaining {
		limit = remaining
	}
	result := make([][]any, 0, int(limit))
	buffer := make([]parquet.Row, minInt64(limit, 128))
	for int64(len(result)) < limit {
		want := int64(len(buffer))
		if left := limit - int64(len(result)); want > left {
			want = left
		}
		read, readErr := reader.ReadRows(buffer[:int(want)])
		for _, row := range buffer[:read] {
			decoded := make([]any, len(encodings))
			var decodeErr error
			row.Range(func(columnIndex int, values []parquet.Value) bool {
				if columnIndex < 0 || columnIndex >= len(encodings) || len(values) != 1 {
					decodeErr = errors.New("Parquet row shape is invalid")
					return false
				}
				if values[0].IsNull() {
					decoded[columnIndex] = nil
					return true
				}
				decoded[columnIndex], decodeErr = encodings[columnIndex].decode(values[0])
				return decodeErr == nil
			})
			if decodeErr != nil {
				return nil, decodeErr
			}
			result = append(result, decoded)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		if read == 0 || errors.Is(readErr, io.EOF) {
			break
		}
	}
	return result, nil
}

func buildColumnEncodings(columns []Column) ([]columnEncoding, reflect.Type, error) {
	encodings := make([]columnEncoding, len(columns))
	fields := make([]reflect.StructField, len(columns))
	used := make(map[string]int, len(columns))
	for index, column := range columns {
		physical := uniquePhysicalName(column.Name, index, used)
		encoding, err := encodingForOID(column.DataTypeOID)
		if err != nil {
			return nil, nil, fmt.Errorf("column %q: %w", column.Name, err)
		}
		encoding.schema.Name = column.Name
		encoding.schema.PhysicalName = physical
		encoding.schema.DataTypeOID = column.DataTypeOID
		encodings[index] = encoding
		tagParts := append([]string{physical}, encoding.parquetOptions...)
		tagParts = append(tagParts, "optional")
		tagValue := strings.Join(tagParts, ",")
		fields[index] = reflect.StructField{
			Name: fmt.Sprintf("Column%06d", index), Type: encoding.typeOf,
			Tag: reflect.StructTag("parquet:" + strconv.Quote(tagValue)),
		}
	}
	return encodings, reflect.StructOf(fields), nil
}

func encodingForOID(oid uint32) (columnEncoding, error) {
	switch oid {
	case 16:
		return columnEncoding{schema: ColumnSchema{ParquetType: "BOOLEAN"}, typeOf: reflect.TypeOf(false),
			value: boolValue, decode: func(value parquet.Value) (any, error) { return value.Boolean(), nil }}, nil
	case 21, 23:
		return columnEncoding{schema: ColumnSchema{ParquetType: "INT32"}, typeOf: reflect.TypeOf(int32(0)),
			value: int32Value, decode: func(value parquet.Value) (any, error) { return int64(value.Int32()), nil }}, nil
	case 20, 26:
		return columnEncoding{schema: ColumnSchema{ParquetType: "INT64"}, typeOf: reflect.TypeOf(int64(0)),
			value: int64Value, decode: func(value parquet.Value) (any, error) { return value.Int64(), nil }}, nil
	case 700:
		return columnEncoding{schema: ColumnSchema{ParquetType: "FLOAT"}, typeOf: reflect.TypeOf(float32(0)),
			value: float32Value, decode: decodeFloat32}, nil
	case 701:
		return columnEncoding{schema: ColumnSchema{ParquetType: "DOUBLE"}, typeOf: reflect.TypeOf(float64(0)),
			value: float64Value, decode: decodeFloat64}, nil
	case 17:
		return columnEncoding{schema: ColumnSchema{ParquetType: "BYTE_ARRAY"}, typeOf: reflect.TypeOf([]byte(nil)),
			value: bytesValue, decode: func(value parquet.Value) (any, error) { return append([]byte(nil), value.ByteArray()...), nil }}, nil
	case 18:
		return stringEncoding("UTF8", qCharValue, decodeString), nil
	case 25, 1042, 1043:
		return stringEncoding("UTF8", textValue, decodeString), nil
	case 1082:
		return stringEncoding("DATE_STRING", dateValue, decodeString), nil
	case 1083:
		return columnEncoding{schema: ColumnSchema{ParquetType: "TIME_MICROS"}, typeOf: reflect.TypeOf(int64(0)),
			value: timeValue, decode: func(value parquet.Value) (any, error) {
				decoded, err := formatTimeMicros(value.Int64())
				return decoded, err
			}}, nil
	case 1114:
		return stringEncoding("TIMESTAMP_STRING", timestampValue, decodeString), nil
	case 1184:
		return stringEncoding("TIMESTAMPTZ_STRING", timestamptzValue, decodeString), nil
	case 1266:
		return stringEncoding("TIMETZ_STRING", timetzValue, decodeString), nil
	case 1700:
		return stringEncoding("DECIMAL_STRING", numericStringValue, decodeNumericString), nil
	case 2950:
		return columnEncoding{schema: ColumnSchema{ParquetType: "UUID"}, typeOf: reflect.TypeOf([16]byte{}),
			parquetOptions: []string{"uuid"}, value: uuidValue, decode: decodeUUID}, nil
	case 114, 3802:
		return stringEncoding("JSON_STRING", jsonStringValue, func(value parquet.Value) (any, error) {
			decoder := json.NewDecoder(bytes.NewReader(value.ByteArray()))
			decoder.UseNumber()
			var decoded any
			if err := decoder.Decode(&decoded); err != nil {
				return nil, err
			}
			return decoded, nil
		}), nil
	default:
		return columnEncoding{}, fmt.Errorf("unsupported PostgreSQL type OID %d", oid)
	}
}

func decodeString(value parquet.Value) (any, error) {
	return string(value.ByteArray()), nil
}

func decodeFloat32(value parquet.Value) (any, error) {
	decoded := value.Float()
	if special := canonicalFloatSpecial(float64(decoded)); special != "" {
		return special, nil
	}
	return decoded, nil
}

func decodeFloat64(value parquet.Value) (any, error) {
	decoded := value.Double()
	if special := canonicalFloatSpecial(decoded); special != "" {
		return special, nil
	}
	return decoded, nil
}

func canonicalFloatSpecial(value float64) string {
	switch {
	case math.IsNaN(value):
		return "nan"
	case math.IsInf(value, 1):
		return "+infinity"
	case math.IsInf(value, -1):
		return "-infinity"
	default:
		return ""
	}
}

func decodeNumericString(value parquet.Value) (any, error) {
	decoded, special, err := canonicalNumericText(string(value.ByteArray()))
	if err != nil {
		return nil, err
	}
	if special {
		return decoded, nil
	}
	return json.Number(decoded), nil
}

func stringEncoding(name string, encode func(any) (parquet.Value, error), decode func(parquet.Value) (any, error)) columnEncoding {
	return columnEncoding{schema: ColumnSchema{ParquetType: name}, typeOf: reflect.TypeOf(""), value: encode, decode: decode}
}

func boolValue(raw any) (parquet.Value, error) {
	value, ok := raw.(bool)
	if !ok {
		return parquet.Value{}, fmt.Errorf("expected boolean, got %T", raw)
	}
	return parquet.BooleanValue(value), nil
}

func int32Value(raw any) (parquet.Value, error) {
	value, err := toInt64(raw)
	if err != nil || value < -1<<31 || value > 1<<31-1 {
		return parquet.Value{}, fmt.Errorf("invalid INT32 value")
	}
	return parquet.Int32Value(int32(value)), nil
}

func int64Value(raw any) (parquet.Value, error) {
	value, err := toInt64(raw)
	if err != nil {
		return parquet.Value{}, err
	}
	return parquet.Int64Value(value), nil
}

func float32Value(raw any) (parquet.Value, error) {
	value, err := toFloat64(raw)
	if err != nil {
		return parquet.Value{}, err
	}
	return parquet.FloatValue(float32(value)), nil
}

func float64Value(raw any) (parquet.Value, error) {
	value, err := toFloat64(raw)
	if err != nil {
		return parquet.Value{}, err
	}
	return parquet.DoubleValue(value), nil
}

func bytesValue(raw any) (parquet.Value, error) {
	value, ok := raw.([]byte)
	if !ok {
		return parquet.Value{}, fmt.Errorf("expected bytea, got %T", raw)
	}
	return parquet.ByteArrayValue(value), nil
}

func numericStringValue(raw any) (parquet.Value, error) {
	var value string
	switch typed := raw.(type) {
	case json.Number:
		value = typed.String()
	case string:
		value = typed
	case []byte:
		value = string(typed)
	case int:
		value = strconv.FormatInt(int64(typed), 10)
	case int8:
		value = strconv.FormatInt(int64(typed), 10)
	case int16:
		value = strconv.FormatInt(int64(typed), 10)
	case int32:
		value = strconv.FormatInt(int64(typed), 10)
	case int64:
		value = strconv.FormatInt(typed, 10)
	case uint:
		value = strconv.FormatUint(uint64(typed), 10)
	case uint8:
		value = strconv.FormatUint(uint64(typed), 10)
	case uint16:
		value = strconv.FormatUint(uint64(typed), 10)
	case uint32:
		value = strconv.FormatUint(uint64(typed), 10)
	case uint64:
		value = strconv.FormatUint(typed, 10)
	case pgtype.Numeric:
		var err error
		value, err = canonicalPGNumeric(typed)
		if err != nil {
			return parquet.Value{}, err
		}
	case *pgtype.Numeric:
		if typed == nil {
			return parquet.Value{}, errors.New("invalid pgx numeric")
		}
		var err error
		value, err = canonicalPGNumeric(*typed)
		if err != nil {
			return parquet.Value{}, err
		}
	case float32, float64:
		return parquet.Value{}, errors.New("exact NUMERIC cannot be constructed from a binary float")
	default:
		return parquet.Value{}, fmt.Errorf("expected exact numeric value, got %T", raw)
	}
	canonical, _, err := canonicalNumericText(value)
	if err != nil {
		return parquet.Value{}, err
	}
	return parquet.ByteArrayValue([]byte(canonical)), nil
}

func canonicalPGNumeric(value pgtype.Numeric) (string, error) {
	if !value.Valid {
		return "", errors.New("pgx numeric must be valid")
	}
	if value.NaN {
		if value.Int != nil || value.Exp != 0 || value.InfinityModifier != pgtype.Finite {
			return "", errors.New("pgx numeric has conflicting NaN fields")
		}
		return "nan", nil
	}
	if value.InfinityModifier != pgtype.Finite {
		if value.Int != nil || value.Exp != 0 {
			return "", errors.New("pgx numeric has conflicting infinity fields")
		}
		switch value.InfinityModifier {
		case pgtype.Infinity:
			return "+infinity", nil
		case pgtype.NegativeInfinity:
			return "-infinity", nil
		default:
			return "", errors.New("pgx numeric has an invalid infinity modifier")
		}
	}
	if value.Int == nil {
		return "", errors.New("finite pgx numeric must have an integer coefficient")
	}

	// PostgreSQL NUMERIC values are coefficient * 10^Exp. Expanding that form
	// produces a decimal string with no binary-float conversion and retains the
	// scale encoded by negative exponents.
	coefficient := value.Int.String()
	sign := ""
	if strings.HasPrefix(coefficient, "-") {
		sign = "-"
		coefficient = coefficient[1:]
	}
	if coefficient == "" {
		return "", errors.New("invalid pgx numeric coefficient")
	}
	const maximumNumericTextBytes = 1 << 20
	if int64(len(coefficient))+int64(absInt32ToInt64(value.Exp))+3 > maximumNumericTextBytes {
		return "", errors.New("pgx numeric exponent is too large")
	}
	if value.Exp >= 0 {
		return sign + coefficient + strings.Repeat("0", int(value.Exp)), nil
	}
	decimalPosition := int64(len(coefficient)) + int64(value.Exp)
	if decimalPosition > 0 {
		return sign + coefficient[:decimalPosition] + "." + coefficient[decimalPosition:], nil
	}
	return sign + "0." + strings.Repeat("0", int(-decimalPosition)) + coefficient, nil
}

func canonicalNumericText(value string) (canonical string, special bool, err error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "nan":
		return "nan", true, nil
	case "infinity", "+infinity":
		return "+infinity", true, nil
	case "-infinity":
		return "-infinity", true, nil
	}
	if !numericPattern.MatchString(value) {
		return "", false, errors.New("numeric value is neither an exact decimal nor a PostgreSQL special value")
	}
	return value, false, nil
}

func absInt32ToInt64(value int32) int64 {
	if value < 0 {
		return -int64(value)
	}
	return int64(value)
}

func jsonStringValue(raw any) (parquet.Value, error) {
	if encoded, ok := raw.([]byte); ok && json.Valid(encoded) {
		return parquet.ByteArrayValue(append([]byte(nil), encoded...)), nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return parquet.Value{}, err
	}
	if rawMessage, ok := raw.(json.RawMessage); ok && json.Valid(rawMessage) {
		encoded = rawMessage
	}
	return parquet.ByteArrayValue(encoded), nil
}

func textValue(raw any) (parquet.Value, error) {
	var value string
	switch typed := raw.(type) {
	case string:
		value = typed
	case []byte:
		value = string(typed)
	default:
		return parquet.Value{}, fmt.Errorf("expected text, got %T", raw)
	}
	return parquet.ByteArrayValue([]byte(value)), nil
}

func qCharValue(raw any) (parquet.Value, error) {
	if value, ok := raw.(rune); ok {
		if value < 0 || value > 255 {
			return parquet.Value{}, errors.New(`PostgreSQL "char" is outside its 8-bit range`)
		}
		return parquet.ByteArrayValue([]byte(string(value))), nil
	}
	return textValue(raw)
}

func dateValue(raw any) (parquet.Value, error) {
	var value string
	switch typed := raw.(type) {
	case time.Time:
		value = typed.Format("2006-01-02")
	case pgtype.Date:
		if !typed.Valid {
			return parquet.Value{}, errors.New("invalid pgx date")
		}
		var err error
		value, err = temporalInfinity(typed.InfinityModifier)
		if err != nil {
			return parquet.Value{}, err
		}
		if value == "" {
			value = typed.Time.Format("2006-01-02")
		}
	case pgtype.InfinityModifier:
		var err error
		value, err = temporalInfinity(typed)
		if err != nil || value == "" {
			return parquet.Value{}, errors.New("date infinity modifier must be positive or negative infinity")
		}
	case string:
		if infinity, err := parseTemporalInfinity(typed); err != nil {
			return parquet.Value{}, err
		} else if infinity != "" {
			value = infinity
		} else {
			parsed, parseErr := time.Parse("2006-01-02", typed)
			if parseErr != nil {
				return parquet.Value{}, fmt.Errorf("invalid date: %w", parseErr)
			}
			value = parsed.Format("2006-01-02")
		}
	default:
		return parquet.Value{}, fmt.Errorf("expected PostgreSQL date, got %T", raw)
	}
	return parquet.ByteArrayValue([]byte(value)), nil
}

func timeValue(raw any) (parquet.Value, error) {
	var microseconds int64
	switch typed := raw.(type) {
	case pgtype.Time:
		if !typed.Valid {
			return parquet.Value{}, errors.New("invalid pgx time")
		}
		microseconds = typed.Microseconds
	case time.Duration:
		if typed%time.Microsecond != 0 {
			return parquet.Value{}, errors.New("time value has sub-microsecond precision")
		}
		microseconds = typed.Microseconds()
	case time.Time:
		if typed.Nanosecond()%int(time.Microsecond) != 0 {
			return parquet.Value{}, errors.New("time value has sub-microsecond precision")
		}
		microseconds = int64(typed.Hour())*int64(time.Hour/time.Microsecond) +
			int64(typed.Minute())*int64(time.Minute/time.Microsecond) +
			int64(typed.Second())*int64(time.Second/time.Microsecond) +
			int64(typed.Nanosecond()/int(time.Microsecond))
	case string:
		var parsed pgtype.Time
		if err := parsed.Scan(typed); err != nil {
			return parquet.Value{}, fmt.Errorf("invalid time without time zone: %w", err)
		}
		if !parsed.Valid {
			return parquet.Value{}, errors.New("invalid time without time zone")
		}
		microseconds = parsed.Microseconds
	default:
		return parquet.Value{}, fmt.Errorf("expected PostgreSQL time, got %T", raw)
	}
	if _, err := formatTimeMicros(microseconds); err != nil {
		return parquet.Value{}, err
	}
	return parquet.Int64Value(microseconds), nil
}

func formatTimeMicros(microseconds int64) (string, error) {
	const microsecondsPerDay = int64(24 * time.Hour / time.Microsecond)
	if microseconds < 0 || microseconds > microsecondsPerDay {
		return "", errors.New("time without time zone is outside 00:00:00..24:00:00")
	}
	hours := microseconds / int64(time.Hour/time.Microsecond)
	microseconds -= hours * int64(time.Hour/time.Microsecond)
	minutes := microseconds / int64(time.Minute/time.Microsecond)
	microseconds -= minutes * int64(time.Minute/time.Microsecond)
	seconds := microseconds / int64(time.Second/time.Microsecond)
	microseconds -= seconds * int64(time.Second/time.Microsecond)
	if microseconds == 0 {
		return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds), nil
	}
	return fmt.Sprintf("%02d:%02d:%02d.%06d", hours, minutes, seconds, microseconds), nil
}

func timetzValue(raw any) (parquet.Value, error) {
	var text string
	switch typed := raw.(type) {
	case string:
		text = typed
	case []byte:
		text = string(typed)
	default:
		return parquet.Value{}, fmt.Errorf("expected PostgreSQL time with time zone text, got %T", raw)
	}
	match := timetzPattern.FindStringSubmatch(text)
	if match == nil {
		return parquet.Value{}, errors.New("invalid PostgreSQL time with time zone")
	}
	hour, _ := strconv.Atoi(match[1])
	minute, _ := strconv.Atoi(match[2])
	second, _ := strconv.Atoi(match[3])
	fraction := match[4]
	offsetHour, _ := strconv.Atoi(match[6])
	offsetMinute := 0
	if match[7] != "" {
		offsetMinute, _ = strconv.Atoi(match[7])
	}
	offsetSecond := 0
	if match[8] != "" {
		offsetSecond, _ = strconv.Atoi(match[8])
	}
	if hour > 24 || minute > 59 || second > 59 || (hour == 24 && (minute != 0 || second != 0 || strings.TrimRight(fraction, "0") != "")) {
		return parquet.Value{}, errors.New("time with time zone is outside 00:00:00..24:00:00")
	}
	if offsetHour > 15 || offsetMinute > 59 || offsetSecond > 59 {
		return parquet.Value{}, errors.New("time with time zone UTC offset is outside PostgreSQL range")
	}
	fraction += strings.Repeat("0", 6-len(fraction))
	fractionMicros, _ := strconv.Atoi(fraction)
	microseconds := int64(hour)*int64(time.Hour/time.Microsecond) +
		int64(minute)*int64(time.Minute/time.Microsecond) +
		int64(second)*int64(time.Second/time.Microsecond) + int64(fractionMicros)
	canonicalTime, err := formatTimeMicros(microseconds)
	if err != nil {
		return parquet.Value{}, err
	}
	sign := match[5]
	if offsetHour == 0 && offsetMinute == 0 && offsetSecond == 0 {
		sign = "+"
	}
	offset := fmt.Sprintf("%s%02d:%02d", sign, offsetHour, offsetMinute)
	if offsetSecond != 0 {
		offset += fmt.Sprintf(":%02d", offsetSecond)
	}
	return parquet.ByteArrayValue([]byte(canonicalTime + offset)), nil
}

func timestampValue(raw any) (parquet.Value, error) {
	value, err := timestampText(raw, false)
	if err != nil {
		return parquet.Value{}, err
	}
	return parquet.ByteArrayValue([]byte(value)), nil
}

func timestamptzValue(raw any) (parquet.Value, error) {
	value, err := timestampText(raw, true)
	if err != nil {
		return parquet.Value{}, err
	}
	return parquet.ByteArrayValue([]byte(value)), nil
}

func timestampText(raw any, withTimezone bool) (string, error) {
	var value time.Time
	switch typed := raw.(type) {
	case time.Time:
		value = typed
	case pgtype.Timestamp:
		if withTimezone {
			return "", errors.New("timestamp without time zone cannot encode as timestamptz")
		}
		if !typed.Valid {
			return "", errors.New("invalid pgx timestamp")
		}
		if infinity, err := temporalInfinity(typed.InfinityModifier); err != nil || infinity != "" {
			return infinity, err
		}
		value = typed.Time
	case pgtype.Timestamptz:
		if !withTimezone {
			return "", errors.New("timestamptz cannot encode as timestamp without time zone")
		}
		if !typed.Valid {
			return "", errors.New("invalid pgx timestamptz")
		}
		if infinity, err := temporalInfinity(typed.InfinityModifier); err != nil || infinity != "" {
			return infinity, err
		}
		value = typed.Time
	case pgtype.InfinityModifier:
		infinity, err := temporalInfinity(typed)
		if err != nil || infinity == "" {
			return "", errors.New("timestamp infinity modifier must be positive or negative infinity")
		}
		return infinity, nil
	case string:
		if infinity, err := parseTemporalInfinity(typed); err != nil || infinity != "" {
			return infinity, err
		}
		layout := "2006-01-02T15:04:05.999999999"
		if withTimezone {
			layout = time.RFC3339Nano
		} else if strings.Contains(typed, " ") && !strings.Contains(typed, "T") {
			layout = "2006-01-02 15:04:05.999999999"
		}
		parsed, err := time.Parse(layout, typed)
		if err != nil {
			return "", fmt.Errorf("invalid PostgreSQL timestamp: %w", err)
		}
		value = parsed
	default:
		return "", fmt.Errorf("expected PostgreSQL timestamp, got %T", raw)
	}
	if withTimezone {
		return value.UTC().Format(time.RFC3339Nano), nil
	}
	return value.Format("2006-01-02T15:04:05.999999999"), nil
}

func temporalInfinity(modifier pgtype.InfinityModifier) (string, error) {
	switch modifier {
	case pgtype.Finite:
		return "", nil
	case pgtype.Infinity:
		return "infinity", nil
	case pgtype.NegativeInfinity:
		return "-infinity", nil
	default:
		return "", errors.New("invalid PostgreSQL infinity modifier")
	}
}

func parseTemporalInfinity(value string) (string, error) {
	switch value {
	case "infinity", "+infinity":
		return "infinity", nil
	case "-infinity":
		return "-infinity", nil
	default:
		return "", nil
	}
}

func uuidValue(raw any) (parquet.Value, error) {
	var value [16]byte
	switch typed := raw.(type) {
	case [16]byte:
		value = typed
	case []byte:
		if len(typed) != len(value) {
			return parquet.Value{}, fmt.Errorf("UUID has %d bytes; expected 16", len(typed))
		}
		copy(value[:], typed)
	case pgtype.UUID:
		if !typed.Valid {
			return parquet.Value{}, errors.New("invalid pgx UUID")
		}
		value = typed.Bytes
	case string:
		var parsed pgtype.UUID
		if err := parsed.Scan(typed); err != nil || !parsed.Valid {
			return parquet.Value{}, fmt.Errorf("invalid UUID %q", typed)
		}
		value = parsed.Bytes
	default:
		return parquet.Value{}, fmt.Errorf("expected PostgreSQL UUID, got %T", raw)
	}
	return parquet.FixedLenByteArrayValue(value[:]), nil
}

func decodeUUID(value parquet.Value) (any, error) {
	bytes := value.ByteArray()
	if len(bytes) != 16 {
		return nil, fmt.Errorf("Parquet UUID has %d bytes; expected 16", len(bytes))
	}
	encoded := hex.EncodeToString(bytes)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func toInt64(raw any) (int64, error) {
	switch value := raw.(type) {
	case int:
		return int64(value), nil
	case int8:
		return int64(value), nil
	case int16:
		return int64(value), nil
	case int32:
		return int64(value), nil
	case int64:
		return value, nil
	case uint:
		if uint64(value) <= 1<<63-1 {
			return int64(value), nil
		}
	case uint8:
		return int64(value), nil
	case uint16:
		return int64(value), nil
	case uint32:
		return int64(value), nil
	case uint64:
		if value <= 1<<63-1 {
			return int64(value), nil
		}
	case json.Number:
		return value.Int64()
	}
	return 0, fmt.Errorf("expected integer, got %T", raw)
}

func toFloat64(raw any) (float64, error) {
	switch value := raw.(type) {
	case float32:
		return float64(value), nil
	case float64:
		return value, nil
	case json.Number:
		return value.Float64()
	default:
		integer, err := toInt64(raw)
		return float64(integer), err
	}
}

func uniquePhysicalName(name string, index int, used map[string]int) string {
	var builder strings.Builder
	for _, value := range strings.TrimSpace(name) {
		if value == ',' || unicode.IsControl(value) || value == 0 {
			builder.WriteRune('_')
		} else {
			builder.WriteRune(value)
		}
	}
	base := builder.String()
	if base == "" {
		base = fmt.Sprintf("column_%d", index+1)
	}
	candidate := base
	for suffix := 2; used[candidate] != 0; suffix++ {
		candidate = fmt.Sprintf("%s__%d", base, suffix)
	}
	used[candidate] = 1
	return candidate
}

func columnSchemas(encodings []columnEncoding) []ColumnSchema {
	result := make([]ColumnSchema, len(encodings))
	for index := range encodings {
		result[index] = encodings[index].schema
	}
	return result
}

func minInt64(left, right int64) int {
	if left < right {
		return int(left)
	}
	return int(right)
}

var (
	numericPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
	timetzPattern  = regexp.MustCompile(`^([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]{1,6}))?([+-])([0-9]{2})(?::([0-9]{2})(?::([0-9]{2}))?)?$`)
)
