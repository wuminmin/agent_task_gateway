package finalv5oracle

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestCanonicalSchemaGoldenAndMutations(t *testing.T) {
	schema := []ResultColumn{
		{Name: "ledger_id", Type: SQLBigInt},
		{Name: "sequence_no", Type: SQLInteger},
		{Name: "amount", Type: SQLNumeric},
		{Name: "description", Type: SQLText},
		{Name: "event_date", Type: SQLDate},
		{Name: "event_timestamp", Type: SQLTimestampWithoutTZ},
		{Name: "approved", Type: SQLBoolean},
	}
	digest, err := ResultSchemaSHA256(schema)
	if err != nil {
		t.Fatal(err)
	}
	const want = "26fc4101757ba05e2d3377aa95d0885e1dc2d22228f4458ee21ba8f77af30a6c"
	if digest != want {
		t.Fatalf("schema digest = %s, want %s", digest, want)
	}

	mutations := []struct {
		name   string
		mutate func([]ResultColumn)
	}{
		{name: "name", mutate: func(value []ResultColumn) { value[0].Name = "other_id" }},
		{name: "type", mutate: func(value []ResultColumn) { value[1].Type = SQLBigInt }},
		{name: "order", mutate: func(value []ResultColumn) { value[0], value[1] = value[1], value[0] }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := append([]ResultColumn(nil), schema...)
			test.mutate(changed)
			got, err := ResultSchemaSHA256(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == digest {
				t.Fatal("schema mutation retained the canonical digest")
			}
		})
	}
}

func TestCanonicalValueNormalization(t *testing.T) {
	equivalent := []struct {
		name     string
		typeName SQLType
		left     any
		right    any
	}{
		{name: "bigint Go widths", typeName: SQLBigInt, left: int32(-7), right: int64(-7)},
		{name: "integer JSON", typeName: SQLInteger, left: json.Number("42"), right: int32(42)},
		{name: "numeric zeros", typeName: SQLNumeric, left: "00123.45000", right: "123.45"},
		{name: "numeric exponent", typeName: SQLNumeric, left: "1.2345e2", right: "123.45"},
		{name: "numeric pgx drain", typeName: SQLNumeric, left: pgtype.Numeric{Int: big.NewInt(12345), Exp: -2, Valid: true}, right: "123.45"},
		{name: "numeric negative zero", typeName: SQLNumeric, left: "-0.000", right: "0"},
		{name: "date transport", typeName: SQLDate, left: "2026-08-03", right: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)},
		{name: "timestamp separator", typeName: SQLTimestampWithoutTZ, left: "2026-08-03T04:05:06.1200", right: "2026-08-03 04:05:06.12"},
	}
	for _, test := range equivalent {
		t.Run(test.name, func(t *testing.T) {
			left, err := NormalizeTypedValue(test.typeName, test.left)
			if err != nil {
				t.Fatal(err)
			}
			right, err := NormalizeTypedValue(test.typeName, test.right)
			if err != nil {
				t.Fatal(err)
			}
			if left.SQLType() != right.SQLType() || left.IsNull() != right.IsNull() ||
				!bytes.Equal(left.CanonicalBytes(), right.CanonicalBytes()) {
				t.Fatalf("normalized values differ: %x vs %x", left.CanonicalBytes(), right.CanonicalBytes())
			}
		})
	}

	null, err := NormalizeTypedValue(SQLText, nil)
	if err != nil || !null.IsNull() || len(null.CanonicalBytes()) != 0 {
		t.Fatalf("NULL normalization = %+v, err=%v", null, err)
	}
	empty, err := NormalizeTypedValue(SQLText, "")
	if err != nil || empty.IsNull() || len(empty.CanonicalBytes()) != 0 {
		t.Fatalf("empty text normalization = %+v, err=%v", empty, err)
	}
}

func TestCanonicalValueRejectsLossyOrOutOfContractInputs(t *testing.T) {
	tests := []struct {
		name     string
		typeName SQLType
		value    any
	}{
		{name: "numeric float", typeName: SQLNumeric, value: 1.25},
		{name: "integer float", typeName: SQLInteger, value: 1.0},
		{name: "integer overflow", typeName: SQLInteger, value: int64(1 << 40)},
		{name: "numeric NaN", typeName: SQLNumeric, value: "NaN"},
		{name: "padded numeric", typeName: SQLNumeric, value: " 1.0"},
		{name: "invalid date", typeName: SQLDate, value: "2026-02-30"},
		{name: "zoned timestamp", typeName: SQLTimestampWithoutTZ, value: "2026-08-03T04:05:06Z"},
		{name: "string boolean", typeName: SQLBoolean, value: "true"},
		{name: "unsupported type", typeName: SQLType("double precision"), value: 1.0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NormalizeTypedValue(test.typeName, test.value); err == nil {
				t.Fatal("invalid canonical value was accepted")
			}
		})
	}
}

func TestSQLTypeAliasesAndOIDs(t *testing.T) {
	tests := []struct {
		alias string
		oid   uint32
		want  SQLType
	}{
		{alias: "int8", oid: 20, want: SQLBigInt},
		{alias: "INT4", oid: 23, want: SQLInteger},
		{alias: "decimal", oid: 1700, want: SQLNumeric},
		{alias: "character varying", oid: 25, want: SQLText},
		{alias: "date", oid: 1082, want: SQLDate},
		{alias: "timestamp", oid: 1114, want: SQLTimestampWithoutTZ},
		{alias: "bool", oid: 16, want: SQLBoolean},
	}
	for _, test := range tests {
		fromName, nameErr := NormalizeSQLType(test.alias)
		fromOID, oidErr := SQLTypeFromPostgresOID(test.oid)
		if nameErr != nil || oidErr != nil || fromName != test.want || fromOID != test.want {
			t.Fatalf("alias=%q oid=%d -> %q/%q, errors=%v/%v", test.alias, test.oid, fromName, fromOID, nameErr, oidErr)
		}
	}
	if _, err := SQLTypeFromPostgresOID(701); err == nil {
		t.Fatal("out-of-contract float OID was accepted")
	}
}
