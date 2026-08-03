package finalv5oracle

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/parquet-go/parquet-go"
)

var resultGoldenSchema = []ResultColumn{
	{Name: "ledger_id", Type: SQLBigInt},
	{Name: "sequence_no", Type: SQLInteger},
	{Name: "amount", Type: SQLNumeric},
	{Name: "description", Type: SQLText},
	{Name: "event_date", Type: SQLDate},
	{Name: "event_timestamp", Type: SQLTimestampWithoutTZ},
	{Name: "approved", Type: SQLBoolean},
}

var resultGoldenRows = [][]any{
	{int64(7), int32(3), "00123.4500", "销售部", "2026-08-03", "2026-08-03T04:05:06.1200", true},
	{nil, nil, nil, "", nil, nil, false},
	{int64(-8), int32(-4), "-9.876e2", "north\x00west", "2024-02-29", "2024-02-29 23:59:59.999999", true},
}

func TestCanonicalResultGoldenAndOrderedStreaming(t *testing.T) {
	direct, err := CanonicalResult(resultGoldenSchema, resultGoldenRows)
	if err != nil {
		t.Fatal(err)
	}
	const wantSchema = "26fc4101757ba05e2d3377aa95d0885e1dc2d22228f4458ee21ba8f77af30a6c"
	const wantResult = "3624491d0db24a962017690254058c1e884990d7910f19ea56deb183de774162"
	if direct.NormalizedSchemaSHA256 != wantSchema || direct.CanonicalResultSHA256 != wantResult ||
		direct.RowCount != 3 || direct.ColumnCount != 7 {
		t.Fatalf("canonical result = %+v", direct)
	}

	stream, err := NewResultHasher(resultGoldenSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range resultGoldenRows {
		values := make([]TypedValue, len(row))
		for index := range row {
			values[index], err = NormalizeTypedValue(resultGoldenSchema[index].Type, row[index])
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := stream.WriteTypedRow(values); err != nil {
			t.Fatal(err)
		}
	}
	streamed, err := stream.Finalize()
	if err != nil || streamed != direct {
		t.Fatalf("streamed = %+v, direct = %+v, err=%v", streamed, direct, err)
	}
	if repeated, err := stream.Finalize(); err != nil || repeated != direct {
		t.Fatalf("repeated finalize = %+v, err=%v", repeated, err)
	}
	if err := stream.WriteRow(resultGoldenRows[0]); err == nil {
		t.Fatal("write after finalize succeeded")
	}
}

func TestCanonicalResultMutationsChangeDigest(t *testing.T) {
	baseline, err := CanonicalResult(resultGoldenSchema, resultGoldenRows)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		schema []ResultColumn
		rows   [][]any
	}{
		{name: "row order", schema: resultGoldenSchema, rows: [][]any{resultGoldenRows[1], resultGoldenRows[0], resultGoldenRows[2]}},
		{name: "empty versus null", schema: resultGoldenSchema, rows: [][]any{resultGoldenRows[0], {nil, nil, nil, nil, nil, nil, false}, resultGoldenRows[2]}},
		{name: "value", schema: resultGoldenSchema, rows: [][]any{{int64(8), int32(3), "123.45", "销售部", "2026-08-03", "2026-08-03 04:05:06.12", true}, resultGoldenRows[1], resultGoldenRows[2]}},
		{name: "schema name", schema: []ResultColumn{{Name: "other_id", Type: SQLBigInt}, resultGoldenSchema[1], resultGoldenSchema[2], resultGoldenSchema[3], resultGoldenSchema[4], resultGoldenSchema[5], resultGoldenSchema[6]}, rows: resultGoldenRows},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed, err := CanonicalResult(test.schema, test.rows)
			if err != nil {
				t.Fatal(err)
			}
			if changed.CanonicalResultSHA256 == baseline.CanonicalResultSHA256 {
				t.Fatal("mutation retained canonical result digest")
			}
		})
	}
}

func TestCanonicalSortedResultSuppliesDeterministicRelationalOrder(t *testing.T) {
	forward, err := CanonicalSortedResult(resultGoldenSchema, resultGoldenRows)
	if err != nil {
		t.Fatal(err)
	}
	reversedRows := [][]any{resultGoldenRows[2], resultGoldenRows[1], resultGoldenRows[0]}
	reversed, err := CanonicalSortedResult(resultGoldenSchema, reversedRows)
	if err != nil || reversed != forward {
		t.Fatalf("canonical sorted permutation = %+v, want %+v, err=%v", reversed, forward, err)
	}
	withDuplicate := append(append([][]any(nil), resultGoldenRows...), resultGoldenRows[0])
	duplicated, err := CanonicalSortedResult(resultGoldenSchema, withDuplicate)
	if err != nil {
		t.Fatal(err)
	}
	if duplicated.RowCount != forward.RowCount+1 || duplicated.CanonicalResultSHA256 == forward.CanonicalResultSHA256 {
		t.Fatalf("sorted duplicate identity = %+v, base=%+v", duplicated, forward)
	}
	hasher, err := NewSortedResultHasher(resultGoldenSchema)
	if err != nil {
		t.Fatal(err)
	}
	if err := hasher.WriteRow(resultGoldenRows[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := hasher.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := hasher.WriteRow(resultGoldenRows[1]); err == nil {
		t.Fatal("sorted write after finalize succeeded")
	}
}

type independentParquetRow struct {
	LedgerID       *int64  `parquet:"ledger_id,optional"`
	SequenceNo     *int32  `parquet:"sequence_no,optional"`
	Amount         *string `parquet:"amount,optional"`
	Description    *string `parquet:"description,optional"`
	EventDate      *string `parquet:"event_date,optional"`
	EventTimestamp *string `parquet:"event_timestamp,optional"`
	Approved       *bool   `parquet:"approved,optional"`
}

func TestDirectAndIndependentParquetUseSameNormalizer(t *testing.T) {
	parquetRows := []independentParquetRow{
		{LedgerID: pointer(int64(7)), SequenceNo: pointer(int32(3)), Amount: pointer("123.45"),
			Description: pointer("销售部"), EventDate: pointer("2026-08-03"),
			EventTimestamp: pointer("2026-08-03 04:05:06.12"), Approved: pointer(true)},
		{Description: pointer(""), Approved: pointer(false)},
		{LedgerID: pointer(int64(-8)), SequenceNo: pointer(int32(-4)), Amount: pointer("-987.6"),
			Description: pointer("north\x00west"), EventDate: pointer("2024-02-29"),
			EventTimestamp: pointer("2024-02-29 23:59:59.999999"), Approved: pointer(true)},
	}
	var encoded bytes.Buffer
	writer := parquet.NewGenericWriter[independentParquetRow](&encoded)
	if written, err := writer.Write(parquetRows); err != nil || written != len(parquetRows) {
		t.Fatalf("write independent Parquet = %d, err=%v", written, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	direct, err := CanonicalResult(resultGoldenSchema, resultGoldenRows)
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := CanonicalResultFromParquet(bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), resultGoldenSchema)
	if err != nil {
		t.Fatal(err)
	}
	if delivered != direct {
		t.Fatalf("Parquet summary = %+v, Direct summary = %+v", delivered, direct)
	}

	wrongSchema := append([]ResultColumn(nil), resultGoldenSchema...)
	wrongSchema[0].Name = "wrong_id"
	if _, err := CanonicalResultFromParquet(bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), wrongSchema); err == nil {
		t.Fatal("Parquet reader accepted a different logical schema")
	}
}

func TestCanonicalSortedDirectAndParquetIgnoreTransportRowOrder(t *testing.T) {
	parquetRows := []independentParquetRow{
		{LedgerID: pointer(int64(-8)), SequenceNo: pointer(int32(-4)), Amount: pointer("-987.6"),
			Description: pointer("north\x00west"), EventDate: pointer("2024-02-29"),
			EventTimestamp: pointer("2024-02-29 23:59:59.999999"), Approved: pointer(true)},
		{Description: pointer(""), Approved: pointer(false)},
		{LedgerID: pointer(int64(7)), SequenceNo: pointer(int32(3)), Amount: pointer("123.45"),
			Description: pointer("销售部"), EventDate: pointer("2026-08-03"),
			EventTimestamp: pointer("2026-08-03 04:05:06.12"), Approved: pointer(true)},
	}
	var encoded bytes.Buffer
	writer := parquet.NewGenericWriter[independentParquetRow](&encoded)
	if written, err := writer.Write(parquetRows); err != nil || written != len(parquetRows) {
		t.Fatalf("write reversed Parquet = %d, err=%v", written, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	direct, err := CanonicalSortedResult(resultGoldenSchema, resultGoldenRows)
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := CanonicalSortedResultFromParquet(bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), resultGoldenSchema)
	if err != nil || delivered != direct {
		t.Fatalf("sorted Parquet = %+v, Direct = %+v, err=%v", delivered, direct, err)
	}
}

func TestResultHasherRejectsShapeAndTypeConfusion(t *testing.T) {
	hasher, err := NewResultHasher([]ResultColumn{{Name: "value", Type: SQLInteger}})
	if err != nil {
		t.Fatal(err)
	}
	if err := hasher.WriteRow(nil); err == nil {
		t.Fatal("wrong row width was accepted")
	}
	bigint, err := NormalizeTypedValue(SQLBigInt, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := hasher.WriteTypedRow([]TypedValue{bigint}); err == nil {
		t.Fatal("typed value from another SQL type was accepted")
	}
	if got, err := hasher.Finalize(); err != nil || !reflect.DeepEqual(got, ResultSummary{
		RowCount: 0, ColumnCount: 1, NormalizedSchemaSHA256: got.NormalizedSchemaSHA256,
		CanonicalResultSHA256: got.CanonicalResultSHA256,
	}) {
		t.Fatalf("empty result = %+v, err=%v", got, err)
	}
}

func TestParquetRowDecoderRejectsMissingColumnButAcceptsNull(t *testing.T) {
	columns := []ResultColumn{{Name: "id", Type: SQLBigInt}, {Name: "note", Type: SQLText}}
	decoders := []func(parquet.Value) (any, error){
		func(value parquet.Value) (any, error) { return value.Int64(), nil },
		func(value parquet.Value) (any, error) { return string(value.ByteArray()), nil },
	}
	missing := parquet.Row{
		parquet.Int64Value(7).Level(0, 1, 0),
	}
	if _, err := decodeCanonicalParquetRow(missing, columns, decoders); err == nil {
		t.Fatal("Parquet row decoder accepted a missing column")
	}
	completeWithNull := parquet.Row{
		parquet.Int64Value(7).Level(0, 1, 0),
		parquet.NullValue().Level(0, 0, 1),
	}
	decoded, err := decodeCanonicalParquetRow(completeWithNull, columns, decoders)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, []any{int64(7), nil}) {
		t.Fatalf("decoded row = %#v", decoded)
	}
}

func pointer[T any](value T) *T { return &value }
