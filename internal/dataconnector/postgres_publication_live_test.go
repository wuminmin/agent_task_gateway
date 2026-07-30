package dataconnector

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

const demoCatalogSchemaDigest = "02b4a211cfbab7347fdce28e2dd76406b1118c5f18e1d2146cc2e85a38ccf1cc"

// TestDemoPublicationIsFrozenAndCatalogAttested runs against the fresh
// business database created by db/init. The Compose acceptance suite supplies
// BUSINESS_TEST_POSTGRES_DSN as gateway_reader; ordinary unit runs skip it.
func TestDemoPublicationIsFrozenAndCatalogAttested(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("BUSINESS_TEST_POSTGRES_DSN is required for the frozen publication check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connector, err := New(ctx, Config{
		DSN: dsn, StatementTimeout: time.Second, ConnectTimeout: 5 * time.Second,
		MaxRows: 20, MaxConnections: 1,
		ExpectedSchema:       demoPublicationSchema(),
		ExpectedSchemaDigest: demoCatalogSchemaDigest,
		ExpectedAttestation: ExpectedAttestation{
			DatasourceID: "taskgate-demo-travel", Database: "travel_demo",
			User: "gateway_reader", PostgreSQLMajorVersion: 16,
		},
	})
	if err != nil {
		t.Fatalf("attest frozen demo publication: %v", err)
	}
	defer connector.Close()

	attestation, err := connector.Attestation(ctx)
	if err != nil {
		t.Fatalf("reattest frozen demo publication: %v", err)
	}
	if attestation.SchemaDigest != demoCatalogSchemaDigest {
		t.Fatalf("schema digest = %s, want Catalog digest %s", attestation.SchemaDigest, demoCatalogSchemaDigest)
	}

	rows, err := connector.pool.Query(ctx, `
SELECT cls.relname, cls.relkind::text, cls.relispopulated, pg_get_userbyid(cls.relowner)
FROM pg_class AS cls
JOIN pg_namespace AS ns ON ns.oid = cls.relnamespace
WHERE ns.nspname = 'reporting' AND cls.relname IN ('expense_detail', 'expense_summary')
ORDER BY cls.relname`)
	if err != nil {
		t.Fatalf("inspect reporting publications: %v", err)
	}
	seen := 0
	for rows.Next() {
		var name, kind, owner string
		var populated bool
		if err := rows.Scan(&name, &kind, &populated, &owner); err != nil {
			rows.Close()
			t.Fatalf("scan reporting publication: %v", err)
		}
		if kind != "m" || !populated || owner != "taskgate_snapshot_owner" {
			rows.Close()
			t.Fatalf("publication %s is not a populated NOLOGIN-owned materialized snapshot: kind=%s populated=%t owner=%s", name, kind, populated, owner)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate reporting publications: %v", err)
	}
	rows.Close()
	if seen != 2 {
		t.Fatalf("reporting publication count = %d, want 2", seen)
	}

	var ownerCanLogin, gatewayIsOwnerMember bool
	if err := connector.pool.QueryRow(ctx, `
SELECT owner.rolcanlogin,
       pg_has_role(current_user, 'taskgate_snapshot_owner', 'MEMBER')
FROM pg_roles AS owner
WHERE owner.rolname = 'taskgate_snapshot_owner'`).Scan(&ownerCanLogin, &gatewayIsOwnerMember); err != nil {
		t.Fatalf("inspect snapshot owner: %v", err)
	}
	if ownerCanLogin || gatewayIsOwnerMember {
		t.Fatalf("snapshot owner boundary is unsafe: can_login=%t gateway_member=%t", ownerCanLogin, gatewayIsOwnerMember)
	}

	var canReadReporting, canReadSidecar, canWriteReporting, canWriteSidecar, canUseLegacy bool
	if err := connector.pool.QueryRow(ctx, `
SELECT has_table_privilege(current_user, 'reporting.expense_detail', 'SELECT'),
       has_table_privilege(current_user, 'taskgate_ordinal.expense_detail_v1', 'SELECT'),
       has_table_privilege(current_user, 'reporting.expense_detail', 'INSERT') OR
           has_table_privilege(current_user, 'reporting.expense_detail', 'UPDATE') OR
           has_table_privilege(current_user, 'reporting.expense_detail', 'DELETE') OR
           has_table_privilege(current_user, 'reporting.expense_detail', 'TRUNCATE'),
       has_table_privilege(current_user, 'taskgate_ordinal.expense_detail_v1', 'INSERT') OR
           has_table_privilege(current_user, 'taskgate_ordinal.expense_detail_v1', 'UPDATE') OR
           has_table_privilege(current_user, 'taskgate_ordinal.expense_detail_v1', 'DELETE') OR
           has_table_privilege(current_user, 'taskgate_ordinal.expense_detail_v1', 'TRUNCATE'),
       has_schema_privilege(current_user, 'legacy', 'USAGE')`).Scan(
		&canReadReporting, &canReadSidecar, &canWriteReporting, &canWriteSidecar, &canUseLegacy,
	); err != nil {
		t.Fatalf("inspect gateway publication privileges: %v", err)
	}
	if !canReadReporting || !canReadSidecar || canWriteReporting || canWriteSidecar || canUseLegacy {
		t.Fatalf("gateway publication privileges are unsafe: reporting_read=%t sidecar_read=%t reporting_write=%t sidecar_write=%t legacy_usage=%t",
			canReadReporting, canReadSidecar, canWriteReporting, canWriteSidecar, canUseLegacy)
	}

	connection, err := connector.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire mutation-probe connection: %v", err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `SET default_transaction_read_only = off`); err != nil {
		t.Fatalf("disable defensive session default for grant probe: %v", err)
	}
	for name, statement := range map[string]string{
		"refresh reporting snapshot": `REFRESH MATERIALIZED VIEW reporting.expense_detail`,
		"mutate reporting snapshot":  `UPDATE reporting.expense_detail SET amount = amount WHERE false`,
		"mutate ordinal sidecar":     `UPDATE taskgate_ordinal.expense_detail_v1 SET row_handle = row_handle WHERE false`,
		"mutate seed relation":       `UPDATE legacy.expenses SET amount = amount WHERE false`,
	} {
		if _, err := connection.Exec(ctx, statement); err == nil {
			t.Fatalf("gateway_reader unexpectedly succeeded at %s", name)
		}
	}
}

func demoPublicationSchema() []ViewSchema {
	textColumn := func(name string) SchemaColumn {
		return SchemaColumn{
			Name: name, PostgreSQLType: "text", Collation: "en_US.utf8",
			CollationVersion: "2.36", CollationDeterministic: true,
		}
	}
	return []ViewSchema{
		{
			Schema: "reporting", View: "expense_detail",
			Columns: []SchemaColumn{
				textColumn("receipt_no"), textColumn("employee_no"), textColumn("employee_name"),
				textColumn("department"), {Name: "expense_date", PostgreSQLType: "date"},
				textColumn("expense_type"), {Name: "amount", PostgreSQLType: "numeric"},
				textColumn("city"), textColumn("purpose"), textColumn("status"),
			},
		},
		{
			Schema: "reporting", View: "expense_summary",
			Columns: []SchemaColumn{
				textColumn("month"), textColumn("department"), textColumn("expense_type"),
				{Name: "total_amount", PostgreSQLType: "numeric"},
				{Name: "request_count", PostgreSQLType: "bigint"},
			},
		},
	}
}
