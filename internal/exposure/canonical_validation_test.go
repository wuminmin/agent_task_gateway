package exposure

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

func TestValidateCanonicalSQLValueEncodingCoversAdmissibleDomain(t *testing.T) {
	tests := []struct {
		sqlType      string
		value        any
		noncanonical string
		malformed    string
	}{
		{sqlType: "smallint", value: int16(12), noncanonical: "i:012", malformed: "i:no"},
		{sqlType: "integer", value: int32(-12), noncanonical: "i:-012", malformed: "n:-12"},
		{sqlType: "bigint", value: int64(12), noncanonical: "i:+12", malformed: "i:"},
		{sqlType: "numeric", value: "1.25", noncanonical: "n:10/8", malformed: "n:no"},
		{sqlType: "real", value: float32(1.5), noncanonical: "f:0x1.800p+00", malformed: "f:no"},
		{sqlType: "double precision", value: float64(1.5), noncanonical: "f:0x1.800p+00", malformed: "f:no"},
		{sqlType: "boolean", value: true, noncanonical: "b:1", malformed: "b:truth"},
		{sqlType: "bytea", value: []byte{0, 255}, noncanonical: "x:00FF", malformed: "x:0"},
		{sqlType: "date", value: "2026-08-01", noncanonical: "d:infinity", malformed: "d:2026-8-1"},
		{sqlType: "time without time zone", value: "12:00:00", noncanonical: "tm:043200000000", malformed: "tm:12:00:00"},
		{sqlType: "timestamp with time zone", value: "2026-08-01T12:00:00+08:00", noncanonical: "tz:2026-08-01T04:00:00+00:00", malformed: "tz:no"},
		{sqlType: "timestamp without time zone", value: "2026-08-01 12:00:00", noncanonical: "ts:2026-08-01 12:00:00", malformed: "ts:no"},
		{sqlType: "jsonb", value: []byte(`{"b":1.0,"a":[true,null]}`), noncanonical: nonCanonicalJSONBObjectOrder(), malformed: "j:o"},
		{sqlType: "uuid", value: "550e8400-e29b-41d4-a716-446655440000", noncanonical: "u:550E8400-E29B-41D4-A716-446655440000", malformed: "u:no"},
		{sqlType: "text", value: "hello", noncanonical: "i:hello", malformed: "hello"},
		{sqlType: "character", value: "x   ", noncanonical: "s:x ", malformed: "x"},
		{sqlType: "character varying", value: "hello", noncanonical: "i:hello", malformed: "hello"},
	}
	for _, test := range tests {
		t.Run(test.sqlType, func(t *testing.T) {
			canonical, err := CanonicalSQLValue(test.sqlType, test.value)
			if err != nil {
				t.Fatalf("canonicalize valid value: %v", err)
			}
			if err := ValidateCanonicalSQLValueEncoding(test.sqlType, canonical); err != nil {
				t.Fatalf("validate %q: %v", canonical, err)
			}
			if err := ValidateCanonicalSQLValueEncoding(test.sqlType, test.noncanonical); err == nil {
				t.Fatalf("accepted non-canonical representation %q", test.noncanonical)
			}
			if err := ValidateCanonicalSQLValueEncoding(test.sqlType, test.malformed); err == nil {
				t.Fatalf("accepted malformed representation %q", test.malformed)
			}
			if err := ValidateCanonicalSQLValueEncoding(test.sqlType, "null"); err != nil {
				t.Fatalf("reject NULL: %v", err)
			}
		})
	}
}

func TestValidateCanonicalSQLValueEncodingSpecialValues(t *testing.T) {
	for _, test := range []struct {
		sqlType string
		values  []string
	}{
		{sqlType: "numeric", values: []string{"n:nan", "n:+infinity", "n:-infinity"}},
		{sqlType: "real", values: []string{"f:nan", "f:+infinity", "f:-infinity"}},
		{sqlType: "double precision", values: []string{"f:nan", "f:+infinity", "f:-infinity"}},
		{sqlType: "date", values: []string{"d:+infinity", "d:-infinity"}},
		{sqlType: "timestamp with time zone", values: []string{"tz:+infinity", "tz:-infinity"}},
		{sqlType: "timestamp without time zone", values: []string{"ts:+infinity", "ts:-infinity"}},
		{sqlType: "time without time zone", values: []string{"tm:0", "tm:86400000000"}},
	} {
		for _, encoded := range test.values {
			if err := ValidateCanonicalSQLValueEncoding(test.sqlType, encoded); err != nil {
				t.Errorf("%s %q: %v", test.sqlType, encoded, err)
			}
		}
	}
	for _, invalid := range []string{"tm:-1", "tm:86400000001", "tm:+1", "tm:00"} {
		if err := ValidateCanonicalSQLValueEncoding("time", invalid); err == nil {
			t.Errorf("accepted invalid TIME encoding %q", invalid)
		}
	}
	if encoded, err := CanonicalSQLValue("double precision", math.Copysign(0, -1)); err != nil || encoded != "f:0x0p+00" {
		t.Fatalf("negative zero normalization = %q, %v", encoded, err)
	}
}

func TestValidateCanonicalJSONBEncodingRejectsMalformedTrees(t *testing.T) {
	validInputs := []string{`null`, `true`, `"text"`, `1.25`, `[null,false,{"a":1}]`, `{"b":1,"a":[true,null]}`}
	for _, input := range validInputs {
		encoded, err := CanonicalSQLValue("jsonb", []byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateCanonicalSQLValueEncoding("jsonb", encoded); err != nil {
			t.Fatalf("valid JSONB %s: %v", input, err)
		}
	}

	duplicate := canonicalJSONBObject([]canonicalJSONBMember{
		{key: "a", value: canonicalJSONBNode{tag: 'z'}},
		{key: "a", value: canonicalJSONBNode{tag: 'b', boolean: true}},
	})
	noncanonicalNumber := "j:n" + canonicalTestString("2/2")
	valid, err := CanonicalSQLValue("jsonb", []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	invalidUTF8 := append([]byte("j:s"), make([]byte, 8)...)
	invalidUTF8[len(invalidUTF8)-1] = 1
	invalidUTF8 = append(invalidUTF8, 0xff)
	for _, encoded := range []string{
		"j:", "j:x", "j:b", "j:b2", "j:ztrailing", "j:a", "j:o",
		nonCanonicalJSONBObjectOrder(), duplicate, noncanonicalNumber,
		valid + "z", string(invalidUTF8),
	} {
		if err := ValidateCanonicalSQLValueEncoding("jsonb", encoded); err == nil {
			t.Errorf("accepted malformed/non-canonical JSONB bytes %q", encoded)
		}
	}
}

func TestPredicateAtomsAcceptCanonicalTimeAndJSONB(t *testing.T) {
	contextDigest := strings.Repeat("1", 64)
	for _, test := range []struct {
		field   string
		sqlType string
		value   any
	}{
		{field: "business_time", sqlType: "time without time zone", value: "12:00:00"},
		{field: "payload", sqlType: "jsonb", value: []byte(`{"b":1.0,"a":[true,null]}`)},
	} {
		literal, err := CanonicalSQLValue(test.sqlType, test.value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewPredicateAtomFactV5(PredicateAtomFactV5{
			PredicateContextSHA256: contextDigest, SemanticProductID: "events", StableRole: "events",
			PublicFieldID: test.field, SQLType: test.sqlType, Operator: "EQ", CanonicalLiteral: literal,
		}); err != nil {
			t.Fatalf("%s atom rejected canonical literal %q: %v", test.sqlType, literal, err)
		}
	}
}

func nonCanonicalJSONBObjectOrder() string {
	return canonicalJSONBObject([]canonicalJSONBMember{
		{key: "b", value: canonicalJSONBNode{tag: 'z'}},
		{key: "a", value: canonicalJSONBNode{tag: 'b', boolean: true}},
	})
}

func canonicalJSONBObject(members []canonicalJSONBMember) string {
	var buffer bytes.Buffer
	writeCanonicalJSONBNode(&buffer, canonicalJSONBNode{tag: 'o', members: members})
	return "j:" + buffer.String()
}

func canonicalTestString(value string) string {
	var buffer bytes.Buffer
	writeCanonicalString(&buffer, value)
	return buffer.String()
}

func TestCanonicalTimeValidatorMatchesEncoderAtMicrosecondBoundaries(t *testing.T) {
	for _, duration := range []time.Duration{0, time.Microsecond, 12 * time.Hour, 24 * time.Hour} {
		encoded, err := CanonicalSQLValue("time", duration)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateCanonicalSQLValueEncoding("time", encoded); err != nil {
			t.Fatalf("duration %s encoded as %q: %v", duration, encoded, err)
		}
	}
}
