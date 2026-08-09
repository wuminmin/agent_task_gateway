package finalv5oracle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"
)

// ResultSummary is the bounded Oracle output for an ordered logical result.
// It deliberately contains neither Parquet bytes nor a production receipt
// digest.
type ResultSummary struct {
	RowCount               int64  `json:"row_count"`
	ColumnCount            int    `json:"column_count"`
	NormalizedSchemaSHA256 string `json:"normalized_schema_sha256"`
	CanonicalResultSHA256  string `json:"canonical_result_sha256"`
}

// ResultHasher incrementally commits rows in their observed order. Callers
// must provide a query with a deterministic total order; this type never sorts
// rows or retains the result.
type ResultHasher struct {
	columns       []ResultColumn
	schemaDigest  [sha256.Size]byte
	rowStreamHash hash.Hash
	rowCount      int64
	closed        bool
	summary       ResultSummary
}

const (
	maximumSortedResultRows         = 200_000
	maximumSortedResultEncodedBytes = int64(256 << 20)
)

// SortedResultHasher provides the deterministic total-order fallback required
// by the approved Join, semantic-View, and UNION contracts. Production's
// frozen queries for these shapes do not depend on returned row order, so this
// evaluation-only path sorts complete canonical typed-row payloads lexicographically;
// its existence is not because the grammar forbids ORDER BY. Duplicate rows are
// retained.
type SortedResultHasher struct {
	columns      []ResultColumn
	rows         [][]byte
	encodedBytes int64
	closed       bool
	summary      ResultSummary
}

func NewResultHasher(columns []ResultColumn) (*ResultHasher, error) {
	normalized, err := NormalizeResultSchema(columns)
	if err != nil {
		return nil, err
	}
	schemaHash := sha256.New()
	_, _ = schemaHash.Write([]byte(resultSchemaDomainV1))
	_, _ = schemaHash.Write(canonicalResultSchemaPayload(normalized))
	var schemaDigest [sha256.Size]byte
	copy(schemaDigest[:], schemaHash.Sum(nil))
	rows := sha256.New()
	_, _ = rows.Write([]byte(resultRowsDomainV1))
	return &ResultHasher{columns: normalized, schemaDigest: schemaDigest, rowStreamHash: rows}, nil
}

// ResultSchemaSHA256 returns the digest used by ResultHasher without creating
// a row stream.
func ResultSchemaSHA256(columns []ResultColumn) (string, error) {
	hasher, err := NewResultHasher(columns)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.schemaDigest[:]), nil
}

// WriteRow normalizes one Direct or decoded-Parquet row using the declared
// schema and commits it atomically to the ordered stream.
func (hasher *ResultHasher) WriteRow(row []any) error {
	if hasher == nil {
		return errors.New("canonical result hasher is nil")
	}
	if hasher.closed {
		return errors.New("canonical result hasher is finalized")
	}
	typed, err := normalizeResultRow(hasher.columns, row)
	if err != nil {
		return err
	}
	return hasher.WriteTypedRow(typed)
}

func normalizeResultRow(columns []ResultColumn, row []any) ([]TypedValue, error) {
	if len(row) != len(columns) {
		return nil, fmt.Errorf("canonical result row has %d columns; expected %d", len(row), len(columns))
	}
	typed := make([]TypedValue, len(row))
	for index := range row {
		value, err := NormalizeTypedValue(columns[index].Type, row[index])
		if err != nil {
			return nil, fmt.Errorf("canonical result column %q: %w", columns[index].Name, err)
		}
		typed[index] = value
	}
	return typed, nil
}

// WriteTypedRow accepts values already normalized through NormalizeTypedValue.
func (hasher *ResultHasher) WriteTypedRow(row []TypedValue) error {
	if hasher == nil {
		return errors.New("canonical result hasher is nil")
	}
	if hasher.closed {
		return errors.New("canonical result hasher is finalized")
	}
	encoded, err := canonicalTypedRowPayload(hasher.columns, row)
	if err != nil {
		return err
	}
	return hasher.writeEncodedRow(encoded)
}

func canonicalTypedRowPayload(columns []ResultColumn, row []TypedValue) ([]byte, error) {
	if len(row) != len(columns) {
		return nil, fmt.Errorf("canonical typed row has %d columns; expected %d", len(row), len(columns))
	}
	var encoded bytes.Buffer
	writeOracleUint64(&encoded, uint64(len(row)))
	for index, value := range row {
		if value.sqlType != columns[index].Type {
			return nil, fmt.Errorf("canonical typed column %q has type %q; expected %q",
				columns[index].Name, value.sqlType, columns[index].Type)
		}
		writeOracleString(&encoded, string(value.sqlType))
		if value.null {
			_, _ = encoded.Write([]byte{0})
			writeOracleBytes(&encoded, nil)
		} else {
			_, _ = encoded.Write([]byte{1})
			writeOracleBytes(&encoded, value.canonical)
		}
	}
	return encoded.Bytes(), nil
}

func (hasher *ResultHasher) writeEncodedRow(encoded []byte) error {
	if hasher == nil {
		return errors.New("canonical result hasher is nil")
	}
	if hasher.closed {
		return errors.New("canonical result hasher is finalized")
	}
	if hasher.rowCount == math.MaxInt64 {
		return errors.New("canonical result row count exceeds int64")
	}
	writeOracleBytes(hasher.rowStreamHash, encoded)
	hasher.rowCount++
	return nil
}

// Finalize returns a stable summary and closes the stream. Repeated calls are
// idempotent; writes after finalization fail.
func (hasher *ResultHasher) Finalize() (ResultSummary, error) {
	if hasher == nil {
		return ResultSummary{}, errors.New("canonical result hasher is nil")
	}
	if hasher.closed {
		return hasher.summary, nil
	}
	rowDigest := hasher.rowStreamHash.Sum(nil)
	result := sha256.New()
	_, _ = result.Write([]byte(resultDigestDomainV1))
	writeOracleBytes(result, hasher.schemaDigest[:])
	writeOracleUint64(result, uint64(hasher.rowCount))
	writeOracleUint64(result, uint64(len(hasher.columns)))
	writeOracleBytes(result, rowDigest)
	hasher.summary = ResultSummary{
		RowCount: hasher.rowCount, ColumnCount: len(hasher.columns),
		NormalizedSchemaSHA256: hex.EncodeToString(hasher.schemaDigest[:]),
		CanonicalResultSHA256:  hex.EncodeToString(result.Sum(nil)),
	}
	hasher.closed = true
	return hasher.summary, nil
}

// NewSortedResultHasher starts a bounded canonical-row sorter. It exists only
// for registered workloads whose production grammar cannot express ORDER BY.
func NewSortedResultHasher(columns []ResultColumn) (*SortedResultHasher, error) {
	normalized, err := NormalizeResultSchema(columns)
	if err != nil {
		return nil, err
	}
	return &SortedResultHasher{columns: normalized}, nil
}

func (hasher *SortedResultHasher) WriteRow(row []any) error {
	if hasher == nil {
		return errors.New("sorted canonical result hasher is nil")
	}
	if hasher.closed {
		return errors.New("sorted canonical result hasher is finalized")
	}
	typed, err := normalizeResultRow(hasher.columns, row)
	if err != nil {
		return err
	}
	return hasher.WriteTypedRow(typed)
}

func (hasher *SortedResultHasher) WriteTypedRow(row []TypedValue) error {
	if hasher == nil {
		return errors.New("sorted canonical result hasher is nil")
	}
	if hasher.closed {
		return errors.New("sorted canonical result hasher is finalized")
	}
	if len(hasher.rows) >= maximumSortedResultRows {
		return fmt.Errorf("sorted canonical result exceeds %d rows", maximumSortedResultRows)
	}
	encoded, err := canonicalTypedRowPayload(hasher.columns, row)
	if err != nil {
		return err
	}
	if int64(len(encoded)) > maximumSortedResultEncodedBytes-hasher.encodedBytes {
		return fmt.Errorf("sorted canonical result exceeds %d encoded bytes", maximumSortedResultEncodedBytes)
	}
	hasher.encodedBytes += int64(len(encoded))
	hasher.rows = append(hasher.rows, append([]byte(nil), encoded...))
	return nil
}

func (hasher *SortedResultHasher) Finalize() (ResultSummary, error) {
	if hasher == nil {
		return ResultSummary{}, errors.New("sorted canonical result hasher is nil")
	}
	if hasher.closed {
		return hasher.summary, nil
	}
	sort.Slice(hasher.rows, func(i, j int) bool { return bytes.Compare(hasher.rows[i], hasher.rows[j]) < 0 })
	ordered, err := NewResultHasher(hasher.columns)
	if err != nil {
		return ResultSummary{}, err
	}
	for _, row := range hasher.rows {
		if err := ordered.writeEncodedRow(row); err != nil {
			return ResultSummary{}, err
		}
	}
	hasher.summary, err = ordered.Finalize()
	if err != nil {
		return ResultSummary{}, err
	}
	hasher.rows = nil
	hasher.closed = true
	return hasher.summary, nil
}

// CanonicalResult hashes an already drained Direct result without changing its
// row order. It is a convenience wrapper around the streaming API.
func CanonicalResult(columns []ResultColumn, rows [][]any) (ResultSummary, error) {
	hasher, err := NewResultHasher(columns)
	if err != nil {
		return ResultSummary{}, err
	}
	for index, row := range rows {
		if err := hasher.WriteRow(row); err != nil {
			return ResultSummary{}, fmt.Errorf("canonical result row %d: %w", index+1, err)
		}
	}
	return hasher.Finalize()
}

// CanonicalSortedResult normalizes and sorts complete typed rows before
// hashing. It is the approved deterministic order for S2, S4, and S5 only.
func CanonicalSortedResult(columns []ResultColumn, rows [][]any) (ResultSummary, error) {
	hasher, err := NewSortedResultHasher(columns)
	if err != nil {
		return ResultSummary{}, err
	}
	for index, row := range rows {
		if err := hasher.WriteRow(row); err != nil {
			return ResultSummary{}, fmt.Errorf("sorted canonical result row %d: %w", index+1, err)
		}
	}
	return hasher.Finalize()
}

// CanonicalResultFromParquet independently reads a flat Parquet result and
// feeds its typed rows to ResultHasher. The caller supplies the reviewed
// logical schema; Parquet column names, shape, physical kinds, and row count
// are checked before a summary is returned.
func CanonicalResultFromParquet(input io.ReaderAt, size int64, columns []ResultColumn) (ResultSummary, error) {
	return canonicalResultFromParquet(input, size, columns, false)
}

// CanonicalSortedResultFromParquet applies the same canonical typed-row order
// as CanonicalSortedResult after independently decoding the emitted file.
func CanonicalSortedResultFromParquet(input io.ReaderAt, size int64, columns []ResultColumn) (ResultSummary, error) {
	return canonicalResultFromParquet(input, size, columns, true)
}

type canonicalRowHasher interface {
	WriteRow([]any) error
	Finalize() (ResultSummary, error)
}

func canonicalResultFromParquet(input io.ReaderAt, size int64, columns []ResultColumn, sortedRows bool) (ResultSummary, error) {
	if input == nil || size <= 0 {
		return ResultSummary{}, errors.New("canonical Parquet input and positive size are required")
	}
	normalized, err := NormalizeResultSchema(columns)
	if err != nil {
		return ResultSummary{}, err
	}
	file, err := parquet.OpenFile(input, size)
	if err != nil {
		return ResultSummary{}, fmt.Errorf("open canonical Parquet: %w", err)
	}
	paths := file.Schema().Columns()
	if len(paths) != len(normalized) {
		return ResultSummary{}, fmt.Errorf("Parquet has %d leaf columns; expected %d", len(paths), len(normalized))
	}
	decoders := make([]func(parquet.Value) (any, error), len(normalized))
	for index, path := range paths {
		if len(path) != 1 || path[0] != normalized[index].Name {
			return ResultSummary{}, fmt.Errorf("Parquet column %d does not match logical column %q", index+1, normalized[index].Name)
		}
		leaf, ok := file.Schema().Lookup(path...)
		if !ok || leaf.MaxRepetitionLevel != 0 {
			return ResultSummary{}, fmt.Errorf("Parquet column %q is missing or repeated", normalized[index].Name)
		}
		decoder, decoderErr := parquetValueDecoder(normalized[index].Type, leaf)
		if decoderErr != nil {
			return ResultSummary{}, fmt.Errorf("Parquet column %q: %w", normalized[index].Name, decoderErr)
		}
		decoders[index] = decoder
	}
	var hasher canonicalRowHasher
	if sortedRows {
		hasher, err = NewSortedResultHasher(normalized)
	} else {
		hasher, err = NewResultHasher(normalized)
	}
	if err != nil {
		return ResultSummary{}, err
	}
	reader := parquet.NewReader(file)
	rows := make([]parquet.Row, 128)
	var rowsRead int64
	for {
		count, readErr := reader.ReadRows(rows)
		for rowIndex := 0; rowIndex < count; rowIndex++ {
			decoded, decodeErr := decodeCanonicalParquetRow(rows[rowIndex], normalized, decoders)
			rows[rowIndex] = nil
			if decodeErr != nil {
				_ = reader.Close()
				return ResultSummary{}, fmt.Errorf("decode Parquet row %d: %w", rowsRead+1, decodeErr)
			}
			if err := hasher.WriteRow(decoded); err != nil {
				_ = reader.Close()
				return ResultSummary{}, fmt.Errorf("normalize Parquet row %d: %w", rowsRead+1, err)
			}
			rowsRead++
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			_ = reader.Close()
			return ResultSummary{}, fmt.Errorf("read canonical Parquet: %w", readErr)
		}
		if count == 0 {
			_ = reader.Close()
			return ResultSummary{}, errors.New("canonical Parquet reader made no progress")
		}
	}
	if err := reader.Close(); err != nil {
		return ResultSummary{}, fmt.Errorf("close canonical Parquet: %w", err)
	}
	if rowsRead != file.NumRows() {
		return ResultSummary{}, fmt.Errorf("Parquet yielded %d rows; metadata declares %d", rowsRead, file.NumRows())
	}
	return hasher.Finalize()
}

func decodeCanonicalParquetRow(
	row parquet.Row,
	columns []ResultColumn,
	decoders []func(parquet.Value) (any, error),
) ([]any, error) {
	if len(decoders) != len(columns) {
		return nil, errors.New("Parquet decoder count does not match the logical schema")
	}
	decoded := make([]any, len(columns))
	seen := make([]bool, len(columns))
	var decodeErr error
	row.Range(func(columnIndex int, values []parquet.Value) bool {
		if columnIndex < 0 || columnIndex >= len(columns) || seen[columnIndex] || len(values) != 1 {
			decodeErr = errors.New("Parquet row is nested, repeated, or has a duplicate column")
			return false
		}
		seen[columnIndex] = true
		if values[0].IsNull() {
			decoded[columnIndex] = nil
			return true
		}
		decoded[columnIndex], decodeErr = decoders[columnIndex](values[0])
		return decodeErr == nil
	})
	if decodeErr != nil {
		return nil, decodeErr
	}
	for columnIndex, present := range seen {
		if !present {
			return nil, fmt.Errorf("Parquet row is missing logical column %q", columns[columnIndex].Name)
		}
	}
	return decoded, nil
}

func parquetValueDecoder(sqlType SQLType, leaf parquet.LeafColumn) (func(parquet.Value) (any, error), error) {
	kind := leaf.Node.Type().Kind()
	switch sqlType {
	case SQLBigInt:
		if kind != parquet.Int64 {
			return nil, fmt.Errorf("bigint requires INT64, found %s", kind)
		}
		return func(value parquet.Value) (any, error) { return value.Int64(), nil }, nil
	case SQLInteger:
		if kind != parquet.Int32 {
			return nil, fmt.Errorf("integer requires INT32, found %s", kind)
		}
		return func(value parquet.Value) (any, error) { return int64(value.Int32()), nil }, nil
	case SQLNumeric:
		if kind != parquet.ByteArray && kind != parquet.FixedLenByteArray {
			return nil, fmt.Errorf("numeric contract encoding requires bytes, found %s", kind)
		}
		return func(value parquet.Value) (any, error) { return string(value.ByteArray()), nil }, nil
	case SQLText:
		if kind != parquet.ByteArray && kind != parquet.FixedLenByteArray {
			return nil, fmt.Errorf("text requires bytes, found %s", kind)
		}
		return func(value parquet.Value) (any, error) { return string(value.ByteArray()), nil }, nil
	case SQLDate:
		switch kind {
		case parquet.ByteArray, parquet.FixedLenByteArray:
			return func(value parquet.Value) (any, error) { return string(value.ByteArray()), nil }, nil
		case parquet.Int32:
			logical := leaf.Node.Type().LogicalType()
			if logical == nil || logical.Date == nil {
				return nil, errors.New("INT32 date lacks a DATE logical annotation")
			}
			return func(value parquet.Value) (any, error) {
				seconds := int64(value.Int32()) * 86_400
				return time.Unix(seconds, 0).UTC().Format("2006-01-02"), nil
			}, nil
		default:
			return nil, fmt.Errorf("date requires bytes or DATE INT32, found %s", kind)
		}
	case SQLTimestampWithoutTZ:
		switch kind {
		case parquet.ByteArray, parquet.FixedLenByteArray:
			return func(value parquet.Value) (any, error) { return string(value.ByteArray()), nil }, nil
		case parquet.Int64:
			logical := leaf.Node.Type().LogicalType()
			if logical == nil || logical.Timestamp == nil || logical.Timestamp.IsAdjustedToUTC {
				return nil, errors.New("INT64 timestamp lacks an unadjusted TIMESTAMP logical annotation")
			}
			multiplier := int64(0)
			switch {
			case logical.Timestamp.Unit.Millis != nil:
				multiplier = int64(time.Millisecond)
			case logical.Timestamp.Unit.Micros != nil:
				multiplier = int64(time.Microsecond)
			case logical.Timestamp.Unit.Nanos != nil:
				multiplier = int64(time.Nanosecond)
			default:
				return nil, errors.New("Parquet timestamp has no time unit")
			}
			return func(value parquet.Value) (any, error) {
				raw := value.Int64()
				if raw != 0 && (raw > math.MaxInt64/multiplier || raw < math.MinInt64/multiplier) {
					return nil, errors.New("Parquet timestamp overflows nanoseconds")
				}
				return time.Unix(0, raw*multiplier).UTC(), nil
			}, nil
		default:
			return nil, fmt.Errorf("timestamp requires bytes or TIMESTAMP INT64, found %s", kind)
		}
	case SQLBoolean:
		if kind != parquet.Boolean {
			return nil, fmt.Errorf("boolean requires BOOLEAN, found %s", kind)
		}
		return func(value parquet.Value) (any, error) { return value.Boolean(), nil }, nil
	default:
		return nil, fmt.Errorf("unsupported canonical SQL type %q", sqlType)
	}
}
