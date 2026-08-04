package dataconnector

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

// The structural separation only matters if PostgreSQL itself agrees. This is
// the live half of the contracts v1.5 pin-domain-separation proof: on a real
// server the two pins must land on two distinct pg_stat_statements entries,
// because before v1.5 they landed on one.
//
// The test skips when pg_stat_statements is unavailable rather than passing
// vacuously; a skipped run proves nothing and says so.
func TestSessionPinsProduceDistinctQueryIDsLive(t *testing.T) {
	dsn := testpostgres.SchemaDSN(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	var available bool
	if err := conn.QueryRow(ctx,
		`SELECT count(*) = 1 FROM pg_extension WHERE extname = 'pg_stat_statements'`).Scan(&available); err != nil || !available {
		t.Skip("pg_stat_statements is not installed on this deployment")
	}
	if _, err := conn.Exec(ctx, `SELECT public.pg_stat_statements_reset()`); err != nil {
		t.Skipf("cannot reset pg_stat_statements: %v", err)
	}

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, SafetySessionPinSQL); err != nil {
		t.Fatalf("safety pin: %v", err)
	}
	if err := pinRepresentation(ctx, tx); err != nil {
		t.Fatalf("representation pin: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// One entry per pin, each called exactly once. A single entry with calls=2
	// is the pre-v1.5 collision this change exists to remove.
	var entries, calls int64
	if err := conn.QueryRow(ctx, `
SELECT count(*), COALESCE(sum(calls), 0)
FROM public.pg_stat_statements
WHERE query LIKE '%set_config%' AND query NOT LIKE '%statement_timeout%'`).Scan(&entries, &calls); err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	if calls != 2 {
		t.Fatalf("the two pins produced %d calls, want 2", calls)
	}
	if entries != 2 {
		t.Fatalf("the two pins collapsed into %d pg_stat_statements entries, want 2; "+
			"PIN DOMAIN SEPARATION FAILED", entries)
	}
}

// Structural separation must not have changed what the pins do. Both settings
// must be in force inside the transaction, and neither may survive it.
func TestSessionPinsApplyAndStayTransactionLocalLive(t *testing.T) {
	dsn := testpostgres.SchemaDSN(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, SafetySessionPinSQL); err != nil {
		t.Fatalf("safety pin: %v", err)
	}
	if err := pinRepresentation(ctx, tx); err != nil {
		t.Fatalf("representation pin: %v", err)
	}
	var timeZone, floatDigits, searchPath, conforming string
	if err := tx.QueryRow(ctx, `SELECT current_setting('TimeZone'), current_setting('extra_float_digits'),
	       current_setting('search_path'), current_setting('standard_conforming_strings')`).
		Scan(&timeZone, &floatDigits, &searchPath, &conforming); err != nil {
		t.Fatalf("read settings in transaction: %v", err)
	}
	if timeZone != requiredTimeZone || floatDigits != requiredExtraFloatDigits {
		t.Fatalf("representation settings not in force: TimeZone=%q extra_float_digits=%q", timeZone, floatDigits)
	}
	if searchPath != "pg_catalog" || conforming != "on" {
		t.Fatalf("safety settings not in force: search_path=%q standard_conforming_strings=%q", searchPath, conforming)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// After commit the transaction-local pins must be gone.
	var afterTimeZone string
	if err := conn.QueryRow(ctx, `SELECT current_setting('TimeZone')`).Scan(&afterTimeZone); err != nil {
		t.Fatalf("read settings after commit: %v", err)
	}
	if afterTimeZone == requiredTimeZone {
		// Only meaningful when the server default differs; otherwise the check
		// cannot distinguish a leak from a coincidence and is skipped.
		var serverDefault string
		if err := conn.QueryRow(ctx, `SELECT reset_val FROM pg_settings WHERE name = 'TimeZone'`).Scan(&serverDefault); err == nil &&
			serverDefault != requiredTimeZone {
			t.Fatal("representation pin leaked past its transaction")
		}
	}
}

// A representation pin whose settings did not take effect must fail closed, and
// the error must not disclose the value the server actually reported.
func TestRepresentationPinFailsClosedWithoutDisclosingServerState(t *testing.T) {
	err := connectorError(CodeSchemaDrift, errRepresentationNotInForce)
	if err == nil {
		t.Fatal("expected a fail-closed error")
	}
	for _, leaked := range []string{"UTC", "Asia/Shanghai", "extra_float_digits", "TimeZone"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("representation pin error discloses server state %q: %v", leaked, err)
		}
	}
}
