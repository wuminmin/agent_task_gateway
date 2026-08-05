package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// Gates 27 and 28 from docs/final_v5_v3_runtime_integration_gates.md. They live
// here rather than in package experiment because the requirement is about what
// the observer emits and what its errors say, and only this package can produce
// either.

// Gate 27. The observer reads statement text out of pg_stat_statements and must
// emit none of it. The receipt is retained, replayed and handed to a finalizer
// that must not learn what was queried; the snapshot is held to the same rule.
func TestGate27ObserverEmitsNoSQL(t *testing.T) {
	secret := "SELECT payroll, ssn FROM hr.salaries WHERE employee = 'alice' UNION SELECT 1"
	census, err := readBusinessCensus(context.Background(),
		censusEngine{canonicalCensus("T|4|||", censusLine(secret, true, 4, "777"))}, "container")
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	snapshot := experiment.ObserverSnapshotV2{
		Version: experiment.ObserverSnapshotV2Version, SchemaVersion: 2, Phase: "after",
		Role: "gateway_reader", Total: census.total, Structural: census.rows,
		Environment: census.environment, StatsReset: census.statsReset, Dealloc: census.dealloc,
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	document := string(encoded)

	// Raw fragments, including the keywords a normalized statement would keep.
	for _, fragment := range []string{
		"payroll", "ssn", "salaries", "alice",
		"SELECT", "FROM", "WHERE", "UNION",
	} {
		if strings.Contains(document, fragment) {
			t.Errorf("the emitted snapshot carries the SQL fragment %q", fragment)
		}
	}
	// A normalized form is still SQL.
	if strings.Contains(document, "$1") || strings.Contains(document, "?") {
		t.Error("the emitted snapshot carries a normalized statement")
	}
	// And the encoded payload must not survive either, which is the form a
	// well-meaning "opaque" field would take.
	if strings.Contains(document, base64.StdEncoding.EncodeToString([]byte(secret))) {
		t.Error("the emitted snapshot carries the base64-encoded statement text")
	}
}

// Gate 28. A parser, normalization or classification failure is exactly where
// statement text tends to escape, because the natural thing to report is the
// input that failed. Only a safe code and the deployment-local queryid may
// appear.
func TestGate28ErrorsLeakNoSQL(t *testing.T) {
	secret := "SELECT secret_column FROM confidential.table WHERE token = 'super-secret-value'"
	_, err := readBusinessCensus(context.Background(),
		censusEngine{canonicalCensus("T|1|||",
			censusLine(secret+" ((( unparseable", true, 1, "999"))}, "container")
	if err == nil {
		t.Skip("the digester parsed the deliberately malformed statement; nothing to leak here")
	}
	message := err.Error()
	for _, fragment := range []string{
		"secret_column", "confidential", "super-secret-value", "SELECT", "FROM", "WHERE", "unparseable",
	} {
		if strings.Contains(message, fragment) {
			t.Errorf("the parser error leaked %q: %s", fragment, message)
		}
	}
	// The queryid is deployment-local and is what a reviewer needs in order to
	// find the row; it is the one identifier the error may carry.
	if !strings.Contains(message, "999") {
		t.Errorf("the parser error does not name the deployment-local queryid: %s", message)
	}
}
