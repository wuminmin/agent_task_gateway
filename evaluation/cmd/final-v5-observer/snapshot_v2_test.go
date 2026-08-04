package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// The observer's identities arrive through exact argv. An identity taken from
// the environment can be set by anything in the process tree.
func TestObserverInvocationRequiresExactArgv(t *testing.T) {
	digest := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)

	invocation, err := parseObserverInvocation([]string{"observer",
		"--phase", "before",
		"--observer-window-id", digest,
		"--classifier-manifest-sha256", other,
	})
	if err != nil {
		t.Fatalf("a complete invocation was rejected: %v", err)
	}
	if invocation.phase != "before" || invocation.observerWindowID != digest ||
		invocation.classifierManifestSHA256 != other {
		t.Fatalf("invocation parsed as %+v", invocation)
	}

	for _, testCase := range []struct {
		name string
		args []string
	}{
		{"no phase", []string{"observer", "--observer-window-id", digest, "--classifier-manifest-sha256", other}},
		{"invalid phase", []string{"observer", "--phase", "during",
			"--observer-window-id", digest, "--classifier-manifest-sha256", other}},
		{"no window id", []string{"observer", "--phase", "before", "--classifier-manifest-sha256", other}},
		{"window id not a digest", []string{"observer", "--phase", "before",
			"--observer-window-id", "window-1", "--classifier-manifest-sha256", other}},
		{"no classifier manifest", []string{"observer", "--phase", "before", "--observer-window-id", digest}},
		{"uppercase digest", []string{"observer", "--phase", "before",
			"--observer-window-id", strings.ToUpper(digest), "--classifier-manifest-sha256", other}},
		{"malformed operation binding", []string{"observer", "--phase", "before",
			"--observer-window-id", digest, "--classifier-manifest-sha256", other,
			"--operation-binding-sha256", "nope"}},
		{"unknown flag", []string{"observer", "--phase", "before",
			"--observer-window-id", digest, "--classifier-manifest-sha256", other, "--project", "x"}},
		{"dangling flag", []string{"observer", "--phase"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseObserverInvocation(testCase.args); err == nil {
				t.Fatal("a malformed observer invocation was accepted")
			}
		})
	}
}

// censusEngine replays a canned census response.
type censusEngine struct{ response string }

func (engine censusEngine) exec(context.Context, string, []string) ([]byte, error) {
	return []byte(engine.response), nil
}

func censusLine(sql string, topLevel bool, calls int64, queryID string) string {
	flag := "f"
	if topLevel {
		flag = "t"
	}
	return "R|" + base64.StdEncoding.EncodeToString([]byte(sql)) + "|" + flag + "|" +
		itoa(calls) + "|" + queryID
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func canonicalCensus(rows ...string) string {
	head := []string{
		"E|160014|all|on|off",
		"S|2026-08-04 10:41:46.431868+00|0|2026-08-04 10:40:00+00|27438592",
	}
	var total int64
	for range rows {
		total++
	}
	_ = total
	return strings.Join(append(head, rows...), "\n")
}

// The total and the rows come from one materialized row set, so a disagreement
// means the reading was not atomic.
func TestCensusRejectsANonAtomicReading(t *testing.T) {
	response := canonicalCensus(
		"T|5|||",
		censusLine("SELECT 1", true, 2, "111"),
	)
	if _, err := readBusinessCensus(context.Background(), censusEngine{response}, "container"); err == nil {
		t.Fatal("a census whose total disagreed with its rows was accepted")
	}
}

func TestCensusParsesAnAtomicReading(t *testing.T) {
	response := canonicalCensus(
		"T|3|||",
		censusLine("SELECT 1", true, 2, "111"),
		censusLine("SELECT 2", false, 1, "222"),
	)
	census, err := readBusinessCensus(context.Background(), censusEngine{response}, "container")
	if err != nil {
		t.Fatalf("a well-formed census was rejected: %v", err)
	}
	if census.total != 3 || len(census.rows) != 2 {
		t.Fatalf("census total=%d rows=%d", census.total, len(census.rows))
	}
	if census.environment != experiment.RequiredMeasurementEnvironment() {
		t.Fatalf("environment = %+v", census.environment)
	}
	if census.dealloc != 0 || census.statsReset == "" || census.postmasterStartTime == "" {
		t.Fatalf("state row parsed as %+v", census)
	}
	for _, row := range census.rows {
		if len(row.StrictASTSHA256) != 64 {
			t.Fatalf("row carries no structural digest: %+v", row)
		}
	}
}

// A statement the digester cannot parse must be named by queryid only. The
// distinguishing SQL must not reach the error, which is what would put it into
// stderr and from there into any retained log.
func TestParserFailuresDoNotLeakSQL(t *testing.T) {
	secret := "SELECT secret_column FROM confidential.table WHERE token = 'super-secret-value'"
	response := canonicalCensus(
		"T|1|||",
		censusLine(secret+" ((( unparseable", true, 1, "999"),
	)
	_, err := readBusinessCensus(context.Background(), censusEngine{response}, "container")
	if err == nil {
		t.Skip("the digester parsed the deliberately malformed statement; nothing to leak here")
	}
	message := err.Error()
	for _, fragment := range []string{
		"secret_column", "confidential", "super-secret-value", "SELECT",
	} {
		if strings.Contains(message, fragment) {
			t.Fatalf("the parser error leaked %q: %s", fragment, message)
		}
	}
	if !strings.Contains(message, "999") {
		t.Fatalf("the parser error does not name the deployment-local queryid: %s", message)
	}
}

// Nothing textual may survive into the emitted snapshot.
func TestEmittedSnapshotCarriesNoSQLBearingField(t *testing.T) {
	secret := "SELECT payroll FROM hr.salaries WHERE employee = 'alice'"
	response := canonicalCensus(
		"T|4|||",
		censusLine(secret, true, 4, "777"),
	)
	census, err := readBusinessCensus(context.Background(), censusEngine{response}, "container")
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
	for _, fragment := range []string{"payroll", "salaries", "alice", "SELECT", "FROM", "WHERE"} {
		if strings.Contains(string(encoded), fragment) {
			t.Fatalf("the emitted snapshot contains the SQL fragment %q", fragment)
		}
	}
	// The base64 payload must not survive either.
	if strings.Contains(string(encoded), base64.StdEncoding.EncodeToString([]byte(secret))) {
		t.Fatal("the emitted snapshot carries the encoded statement text")
	}
}

// queryid is a deployment-local decimal diagnostic, never an identity, and a
// malformed one is a malformed census.
func TestCensusRequiresDecimalQueryIDs(t *testing.T) {
	response := canonicalCensus(
		"T|1|||",
		censusLine("SELECT 1", true, 1, "not-decimal"),
	)
	if _, err := readBusinessCensus(context.Background(), censusEngine{response}, "container"); err == nil {
		t.Fatal("a non-decimal queryid was accepted")
	}
}

func TestCensusRequiresItsEnvironmentAndStateRows(t *testing.T) {
	for _, testCase := range []struct{ name, response string }{
		{"no environment row", "S|reset|0|start|1\nT|0|||"},
		{"no state row", "E|160014|all|on|off\nT|0|||"},
		{"no total row", "E|160014|all|on|off\nS|reset|0|start|1"},
		{"unknown marker", "E|160014|all|on|off\nS|reset|0|start|1\nT|0|||\nX|a|b|c|d"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := readBusinessCensus(context.Background(),
				censusEngine{testCase.response}, "container"); err == nil {
				t.Fatal("an incomplete census was accepted")
			}
		})
	}
}
