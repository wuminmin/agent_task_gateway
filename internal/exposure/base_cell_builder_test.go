package exposure

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// The builder must agree with NewBaseCellFactV2 + CanonicalPayloadHash on the
// fact, the payload bytes, the hash, and on whether an input is rejected.
func TestBaseCellFactBuilderMatchesConstructorPath(t *testing.T) {
	random := rand.New(rand.NewSource(20260831))
	namespaces := []string{"ns.schema", "orders", " padded", "", "x"}
	snapshots := []string{strings.Repeat("a", 64), "snapshot-1", "", "snap "}
	fields := []string{"amount", "f", " lead", ""}
	types := []string{"text", "TEXT", "bigint", "int8", "numeric", "decimal", "boolean", "date",
		"timestamp with time zone", "timestamptz", "uuid", "bytea", "jsonb", "character varying", "varchar", "money", "time with time zone"}
	entities := []string{"e-1", "", " e", "rowé", "0"}
	values := []any{nil, int64(7), int32(-3), int16(2), "text value", "", true, false, 3.5, float32(1.25),
		[]byte{1, 2, 3}, time.Date(2026, 8, 31, 1, 2, 3, 0, time.UTC), "2026-08-31", "not a date",
		"12.500", "1e3", map[string]any{"k": 1}, "{\"a\":1}", "9d3f6a2c-1111-4222-8333-444444444444"}
	builderCases, cellCases := 0, 0
	for iteration := 0; iteration < 4000; iteration++ {
		namespace := namespaces[random.Intn(len(namespaces))]
		snapshot := snapshots[random.Intn(len(snapshots))]
		field := fields[random.Intn(len(fields))]
		sqlType := types[random.Intn(len(types))]
		builder, builderErr := NewBaseCellFactBuilder(namespace, snapshot, field, sqlType)
		probe, probeErr := NewBaseCellFactV2(namespace, snapshot, "probe", field, sqlType, nil)
		if (builderErr == nil) != (probeErr == nil) {
			t.Fatalf("builder(%q,%q,%q,%q) err %v, constructor err %v", namespace, snapshot, field, sqlType, builderErr, probeErr)
		}
		if builderErr != nil {
			if builderErr.Error() != probeErr.Error() {
				t.Fatalf("error text differs: %v vs %v", builderErr, probeErr)
			}
			continue
		}
		builderCases++
		_ = probe
		for cell := 0; cell < 8; cell++ {
			entity := entities[random.Intn(len(entities))]
			value := values[random.Intn(len(values))]
			fact, payload, hash, err := builder.Fact(entity, value)
			expected, expectedErr := NewBaseCellFactV2(namespace, snapshot, entity, field, sqlType, value)
			if (err == nil) != (expectedErr == nil) {
				t.Fatalf("cell(%q,%v) builder err %v, constructor err %v", entity, value, err, expectedErr)
			}
			if err != nil {
				if err.Error() != expectedErr.Error() {
					t.Fatalf("cell error text differs: %v vs %v", err, expectedErr)
				}
				continue
			}
			cellCases++
			expectedPayload, expectedHash, err := expected.CanonicalPayloadHash()
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%#v", fact) != fmt.Sprintf("%#v", expected) {
				t.Fatalf("fact differs:\n%#v\n%#v", fact, expected)
			}
			if string(payload) != string(expectedPayload) || hash != expectedHash {
				t.Fatalf("payload/hash differ for %q %v", entity, value)
			}
			if err := fact.Validate(); err != nil {
				t.Fatalf("builder fact fails validation: %v", err)
			}
		}
	}
	if builderCases < 100 || cellCases < 500 {
		t.Fatalf("too few agreeing cases: builders %d cells %d", builderCases, cellCases)
	}
}

func TestNormalizeSQLTypeFastPathIsExact(t *testing.T) {
	for _, name := range []string{"smallint", "integer", "bigint", "numeric", "real", "double precision",
		"boolean", "bytea", "date", "time without time zone", "time with time zone",
		"timestamp with time zone", "timestamp without time zone", "jsonb",
		"uuid", "text", "character", "character varying"} {
		slow := strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(name))), " ")
		if normalizeSQLType(name) != name || normalizeSQLType(" "+strings.ToUpper(name)+" ") != name || slow != name {
			t.Fatalf("%q is not a fixed point of normalization", name)
		}
	}
	if normalizeSQLType("Integer") != "integer" || normalizeSQLType("int8") != "bigint" {
		t.Fatal("aliases must still normalize")
	}
}
