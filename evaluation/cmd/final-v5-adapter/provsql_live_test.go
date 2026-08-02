package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
)

// TestProvSQLLiveExternalPair is opt-in because it requires the two pinned
// PostgreSQL services from compose.provsql.yaml. It exercises the real field
// OIDs, complete typed drains, aggregate scalar recovery, hidden row roots,
// gate types, circuit metrics, and fixture fingerprints. Unit-only runs skip
// it without manufacturing a database or a synthetic result.
func TestProvSQLLiveExternalPair(t *testing.T) {
	directDSN := os.Getenv("TASKGATE_FINAL_V5_DIRECT_DSN")
	provSQLDSN := os.Getenv("TASKGATE_FINAL_V5_PROVSQL_DSN")
	if directDSN == "" || provSQLDSN == "" {
		t.Skip("set TASKGATE_FINAL_V5_DIRECT_DSN and TASKGATE_FINAL_V5_PROVSQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	direct, err := pgx.Connect(ctx, directDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Close(context.Background()) })
	provenance, err := pgx.Connect(ctx, provSQLDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provenance.Close(context.Background()) })
	if err := configureProvSQLSession(ctx, direct, false); err != nil {
		t.Fatal(err)
	}
	if err := configureProvSQLSession(ctx, provenance, true); err != nil {
		t.Fatal(err)
	}
	directSystem, err := inspectProvSQLSystem(ctx, direct, false)
	if err != nil {
		t.Fatal(err)
	}
	provSQLSystem, err := inspectProvSQLSystem(ctx, provenance, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProvSQLSystems(directSystem, provSQLSystem); err != nil {
		t.Fatal(err)
	}
	for name, probe := range map[string]struct {
		conn           *pgx.Conn
		disableProvSQL bool
	}{"direct": {direct, false}, "provsql": {provenance, true}} {
		digest, err := fingerprintProvSQLDataset(ctx, probe.conn, probe.disableProvSQL)
		if err != nil || digest != provsqlfixture.ExpectedDatasetSHA256() {
			t.Fatalf("%s fixture fingerprint = %q, err=%v", name, digest, err)
		}
	}
	artifactBytesGrew := false
	for index, scale := range []string{"1k", "10k", "45k"} {
		t.Run(scale, func(t *testing.T) {
			spec, err := provsqlfixture.ParseScale(scale)
			if err != nil {
				t.Fatal(err)
			}
			// These valid fixture nonces are deliberately outside the 105 frozen
			// campaign bindings, so the opt-in probe cannot consume a formal cell.
			nonce := int64(999 - index)
			directExecution, err := executeProvSQLExternal(ctx, direct, spec, nonce, false, directSystem)
			if err != nil {
				t.Fatal(err)
			}
			provSQLExecution, err := executeProvSQLExternal(ctx, provenance, spec, nonce, true, provSQLSystem)
			if err != nil {
				t.Fatal(err)
			}
			if directExecution.ResultSHA256 != provSQLExecution.ResultSHA256 ||
				directExecution.Rows != provsqlfixture.ExpectedRows || directExecution.Columns != 4 ||
				provSQLExecution.Rows != provsqlfixture.ExpectedRows || provSQLExecution.Columns != 5 ||
				provSQLExecution.TypedDrainFields != provsqlfixture.ExpectedRows*5 ||
				provSQLExecution.AggregateTokens != provsqlfixture.ExpectedRows*provsqlfixture.CarrierColumns ||
				provSQLExecution.RowTokens != provsqlfixture.ExpectedRows || !provSQLExecution.RootTypesVerified {
				t.Fatalf("live direct=%+v provsql=%+v", directExecution, provSQLExecution)
			}
			if provSQLExecution.After.Gates <= provSQLExecution.Before.Gates ||
				provSQLExecution.Before.ArtifactBytes <= 0 ||
				provSQLExecution.After.ArtifactBytes < provSQLExecution.Before.ArtifactBytes {
				t.Fatalf("fresh live circuit regressed or added no gates: before=%+v after=%+v", provSQLExecution.Before, provSQLExecution.After)
			}
			artifactBytesGrew = artifactBytesGrew || provSQLExecution.After.ArtifactBytes > provSQLExecution.Before.ArtifactBytes
		})
	}
	if os.Getenv("TASKGATE_PROVSQL_LIVE_REQUIRE_GROWTH") == "1" && !artifactBytesGrew {
		t.Fatal("ProvSQL mmap representation bytes never grew across 1k/10k/45k")
	}
}
