package dataconnector

import (
	"strings"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// The whole point of contracts v1.5 is that these two statements are
// distinguishable by structure. Before v1.5 they had the same parse shape, so
// pg_stat_statements -- which replaces constants, including the setting names,
// with placeholders -- gave them one queryid and reported them as a single
// template called twice. An accounting built on normalized templates could not
// tell a safety pin from a representation pin, nor detect one substituted for
// the other.
func TestSessionPinsHaveDistinctStructuralFingerprints(t *testing.T) {
	safety, err := pg_query.Fingerprint(SafetySessionPinSQL)
	if err != nil {
		t.Fatalf("fingerprint safety pin: %v", err)
	}
	representation, err := pg_query.Fingerprint(RepresentationPinSQL)
	if err != nil {
		t.Fatalf("fingerprint representation pin: %v", err)
	}
	if safety == representation {
		t.Fatalf("the two session pins still share structural fingerprint %s; "+
			"pin domain separation failed", safety)
	}
}

// Structural separation must survive constant replacement, because that is
// exactly the transformation that erased it before. Comparing the normalized
// forms proves the distinction is carried by the parse tree and not by the
// setting names.
func TestSessionPinsRemainDistinctAfterConstantNormalization(t *testing.T) {
	safety, err := pg_query.Normalize(SafetySessionPinSQL)
	if err != nil {
		t.Fatalf("normalize safety pin: %v", err)
	}
	representation, err := pg_query.Normalize(RepresentationPinSQL)
	if err != nil {
		t.Fatalf("normalize representation pin: %v", err)
	}
	if safety == representation {
		t.Fatal("the session pins collapse to one normalized template; pin domain separation failed")
	}
	// The distinction must be structural. A normalized representation pin that
	// still carried a bare setting name would mean the separation depended on a
	// constant that pg_stat_statements is free to erase.
	for _, setting := range []string{"'TimeZone'", "'extra_float_digits'"} {
		if strings.Contains(representation, setting) {
			t.Fatalf("normalized representation pin still carries the literal %s", setting)
		}
	}
}

// Requirement: one statement, one round trip. A pin that had become two
// statements would change the very accounting it exists to make checkable.
func TestSessionPinsAreEachExactlyOneStatement(t *testing.T) {
	for name, sql := range map[string]string{
		"safety": SafetySessionPinSQL, "representation": RepresentationPinSQL,
	} {
		parsed, err := pg_query.Parse(sql)
		if err != nil {
			t.Fatalf("parse %s pin: %v", name, err)
		}
		if got := len(parsed.GetStmts()); got != 1 {
			t.Fatalf("%s pin is %d statements, want exactly 1", name, got)
		}
	}
}

// Both pins must stay transaction-local. A session-scoped pin would leak
// settings across pooled connections into unrelated governed transactions.
func TestSessionPinsAreTransactionLocal(t *testing.T) {
	for name, sql := range map[string]string{
		"safety": SafetySessionPinSQL, "representation": RepresentationPinSQL,
	} {
		// set_config's third argument is is_local; every call in both pins must
		// pass true.
		calls := strings.Count(sql, "set_config(")
		if calls == 0 {
			t.Fatalf("%s pin issues no set_config call", name)
		}
		if got := strings.Count(sql, ", true)"); got != calls {
			t.Fatalf("%s pin has %d set_config calls but %d are transaction-local", name, calls, got)
		}
	}
}

// The representation pin must still pin exactly the settings it is named for.
// Structural separation is worthless if it silently dropped a setting.
func TestRepresentationPinStillPinsBothSettings(t *testing.T) {
	for _, required := range []string{"'TimeZone', 'UTC'", "'extra_float_digits', '3'"} {
		if !strings.Contains(RepresentationPinSQL, required) {
			t.Fatalf("representation pin no longer sets %s", required)
		}
	}
	// It must also read the settings back, which is what lets the Connector
	// verify them in the same round trip that applied them.
	if !strings.Contains(RepresentationPinSQL, "SELECT time_zone, extra_float_digits FROM") {
		t.Fatal("representation pin does not read its settings back for verification")
	}
}

func TestSafetyPinStillPinsBothSettings(t *testing.T) {
	for _, required := range []string{"'search_path', 'pg_catalog'", "'standard_conforming_strings', 'on'"} {
		if !strings.Contains(SafetySessionPinSQL, required) {
			t.Fatalf("safety pin no longer sets %s", required)
		}
	}
}
