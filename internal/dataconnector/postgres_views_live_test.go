package dataconnector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

func TestLiveViewRegistryTracksNestedDefinitionsAndStopsAtMaterializedLeaves(t *testing.T) {
	dsn := testpostgres.SchemaDSN(t)
	schema := viewRegistryTestSchema(t, dsn)
	ctx := context.Background()
	db := openViewRegistryTestDB(t, dsn)

	mustExec(t, db, `CREATE TABLE seed_orders(id bigint, amount numeric, label text)`)
	mustExec(t, db, `INSERT INTO seed_orders VALUES (1, 10, 'open')`)
	mustExec(t, db, `CREATE MATERIALIZED VIEW orders_snapshot AS SELECT id, amount, label FROM seed_orders`)
	mustExec(t, db, `CREATE VIEW orders_level_1 AS SELECT id, amount, label FROM orders_snapshot WHERE id > 0`)
	mustExec(t, db, `CREATE VIEW orders_level_2 AS SELECT id, amount, label FROM orders_level_1`)
	mustExec(t, db, `CREATE VIEW orders_level_3 AS SELECT id, amount, label FROM orders_level_2`)

	connector := openViewRegistryTestConnector(t, ctx, dsn)
	root := viewcompiler.RelationName{Schema: schema, Name: "orders_level_3"}
	base := viewcompiler.RelationName{Schema: schema, Name: "orders_snapshot"}
	baseProducts := map[string]string{base.String(): "orders"}

	if _, err := connector.DiscoverViewRegistry(ctx, []viewcompiler.RelationName{root}, nil); !IsCode(err, CodeViewSemanticChanged) {
		t.Fatalf("unmapped materialized leaf = %v, want %s", err, CodeViewSemanticChanged)
	}
	withUnusedCandidate, err := connector.DiscoverViewRegistry(ctx, []viewcompiler.RelationName{root}, map[string]string{
		base.String(): "orders", schema + ".unreachable_snapshot": "unused",
	})
	if err != nil {
		t.Fatalf("unused authorized base candidate changed discovery: %v", err)
	}
	initial, err := connector.DiscoverViewRegistry(ctx, []viewcompiler.RelationName{root}, baseProducts)
	if err != nil {
		t.Fatalf("DiscoverViewRegistry: %v", err)
	}
	if len(initial.Relations) != 4 {
		t.Fatalf("registry relation count = %d, want the three views and one materialized leaf", len(initial.Relations))
	}
	if withUnusedCandidate.RevisionDigest != initial.RevisionDigest || len(withUnusedCandidate.Relations) != len(initial.Relations) {
		t.Fatal("unused base candidate changed the reachable registry snapshot")
	}
	leaf := initial.Relations[base]
	if leaf.Kind != viewcompiler.RelationBase || leaf.ProductName != "orders" || len(leaf.Dependencies) != 0 {
		t.Fatalf("materialized leaf = %+v", leaf)
	}
	if _, includedRawSeed := initial.Relations[viewcompiler.RelationName{Schema: schema, Name: "seed_orders"}]; includedRawSeed {
		t.Fatal("materialized-view seed table escaped the opaque publication boundary")
	}
	assertViewRegistryDependency(t, initial, root, viewcompiler.RelationName{Schema: schema, Name: "orders_level_2"})
	assertViewRegistryDependency(t, initial, viewcompiler.RelationName{Schema: schema, Name: "orders_level_2"}, viewcompiler.RelationName{Schema: schema, Name: "orders_level_1"})
	assertViewRegistryDependency(t, initial, viewcompiler.RelationName{Schema: schema, Name: "orders_level_1"}, base)

	expectation := &ViewRegistryExpectation{
		Roots:                  []viewcompiler.RelationName{root},
		BaseProducts:           baseProducts,
		ExpectedRevisionDigest: initial.RevisionDigest,
	}
	result, err := connector.Query(ctx, QueryRequest{
		SQL:              fmt.Sprintf(`SELECT id, amount, label FROM %s.orders_level_3`, schema),
		StatementTimeout: time.Second,
		MaxRows:          10,
		ViewRegistry:     expectation,
	})
	if err != nil {
		t.Fatalf("Query with matching view expectation: %v", err)
	}
	if result.RowCount != 1 {
		t.Fatalf("matching query row count = %d, want 1", result.RowCount)
	}

	// The materialized publication is an immutable terminal leaf for registry
	// purposes. Mutating its private seed does not change the published closure.
	mustExec(t, db, `UPDATE seed_orders SET amount = 999`)
	afterSeedMutation, err := connector.DiscoverViewRegistry(ctx, []viewcompiler.RelationName{root}, baseProducts)
	if err != nil {
		t.Fatalf("rediscover after seed mutation: %v", err)
	}
	if afterSeedMutation.RevisionDigest != initial.RevisionDigest {
		t.Fatalf("private seed mutation changed registry revision: %s != %s", afterSeedMutation.RevisionDigest, initial.RevisionDigest)
	}

	// Replacing a transitive child leaves the root's exact pg_get_viewdef bytes
	// untouched, but must still change the closure revision and invalidate the
	// task-scoped execution expectation.
	mustExec(t, db, `CREATE OR REPLACE VIEW orders_level_1 AS SELECT id, amount, label FROM orders_snapshot WHERE id >= 0`)
	_, err = connector.Query(ctx, QueryRequest{
		SQL:              fmt.Sprintf(`SELECT id, amount, label FROM %s.orders_level_3`, schema),
		StatementTimeout: time.Second,
		MaxRows:          10,
		ViewRegistry:     expectation,
	})
	if !IsCode(err, CodeViewSemanticChanged) {
		t.Fatalf("query after transitive definition drift = %v, want %s", err, CodeViewSemanticChanged)
	}
	afterChildReplacement, err := connector.DiscoverViewRegistry(ctx, []viewcompiler.RelationName{root}, baseProducts)
	if err != nil {
		t.Fatalf("rediscover after child replacement: %v", err)
	}
	if afterChildReplacement.RevisionDigest == initial.RevisionDigest {
		t.Fatal("transitive child replacement did not change registry revision")
	}
	if afterChildReplacement.Relations[root].DefinitionDigest != initial.Relations[root].DefinitionDigest {
		t.Fatal("root definition unexpectedly changed while testing transitive drift")
	}
}

func TestLiveQueryUsesRepeatableReadForViewAttestationSnapshot(t *testing.T) {
	dsn := testpostgres.SchemaDSN(t)
	ctx := context.Background()
	connector := openViewRegistryTestConnector(t, ctx, dsn)

	result, err := connector.Query(ctx, QueryRequest{
		SQL:              `SELECT pg_catalog.current_setting('transaction_isolation') AS isolation_level`,
		StatementTimeout: time.Second,
		MaxRows:          1,
	})
	if err != nil {
		t.Fatalf("Query transaction isolation: %v", err)
	}
	if result.RowCount != 1 || len(result.Rows[0]) != 1 || result.Rows[0][0] != "repeatable read" {
		t.Fatalf("Query transaction isolation rows = %#v", result.Rows)
	}
}

func TestLiveViewRegistryRejectsRawVolatileAndCyclicDependencies(t *testing.T) {
	t.Run("raw relation", func(t *testing.T) {
		dsn := testpostgres.SchemaDSN(t)
		schema := viewRegistryTestSchema(t, dsn)
		ctx := context.Background()
		db := openViewRegistryTestDB(t, dsn)
		mustExec(t, db, `CREATE TABLE raw_events(id bigint)`)
		mustExec(t, db, `CREATE VIEW unsafe_raw_view AS SELECT id FROM raw_events`)
		connector := openViewRegistryTestConnector(t, ctx, dsn)

		_, err := connector.DiscoverViewRegistry(ctx, []viewcompiler.RelationName{{Schema: schema, Name: "unsafe_raw_view"}}, nil)
		if !IsCode(err, CodeViewSemanticChanged) {
			t.Fatalf("raw relation dependency = %v, want %s", err, CodeViewSemanticChanged)
		}
	})

	t.Run("volatile function", func(t *testing.T) {
		dsn := testpostgres.SchemaDSN(t)
		schema := viewRegistryTestSchema(t, dsn)
		ctx := context.Background()
		db := openViewRegistryTestDB(t, dsn)
		mustExec(t, db, `CREATE TABLE seed_events(id bigint)`)
		mustExec(t, db, `CREATE MATERIALIZED VIEW events_snapshot AS SELECT id FROM seed_events`)
		mustExec(t, db, `CREATE VIEW unsafe_clock_view AS SELECT id, clock_timestamp() AS observed_at FROM events_snapshot`)
		connector := openViewRegistryTestConnector(t, ctx, dsn)

		_, err := connector.DiscoverViewRegistry(ctx,
			[]viewcompiler.RelationName{{Schema: schema, Name: "unsafe_clock_view"}},
			map[string]string{schema + ".events_snapshot": "events"})
		if !IsCode(err, CodeViewSemanticChanged) {
			t.Fatalf("volatile dependency = %v, want %s", err, CodeViewSemanticChanged)
		}
	})

	t.Run("user-defined type", func(t *testing.T) {
		dsn := testpostgres.SchemaDSN(t)
		schema := viewRegistryTestSchema(t, dsn)
		ctx := context.Background()
		db := openViewRegistryTestDB(t, dsn)
		mustExec(t, db, `CREATE DOMAIN amount_domain AS numeric`)
		mustExec(t, db, `CREATE TABLE domain_seed(amount amount_domain)`)
		mustExec(t, db, `CREATE MATERIALIZED VIEW domain_snapshot AS SELECT amount FROM domain_seed`)
		connector := openViewRegistryTestConnector(t, ctx, dsn)

		_, err := connector.DiscoverViewRegistry(ctx,
			[]viewcompiler.RelationName{{Schema: schema, Name: "domain_snapshot"}},
			map[string]string{schema + ".domain_snapshot": "amounts"})
		if !IsCode(err, CodeViewSemanticChanged) {
			t.Fatalf("user-defined type = %v, want %s", err, CodeViewSemanticChanged)
		}
	})

	t.Run("cycle", func(t *testing.T) {
		dsn := testpostgres.SchemaDSN(t)
		schema := viewRegistryTestSchema(t, dsn)
		ctx := context.Background()
		db := openViewRegistryTestDB(t, dsn)
		mustExec(t, db, `CREATE RECURSIVE VIEW recursive_numbers(n) AS VALUES (1) UNION ALL SELECT n + 1 FROM recursive_numbers WHERE n < 2`)
		connector := openViewRegistryTestConnector(t, ctx, dsn)

		_, err := connector.DiscoverViewRegistry(ctx, []viewcompiler.RelationName{{Schema: schema, Name: "recursive_numbers"}}, nil)
		if !IsCode(err, CodeViewSemanticChanged) {
			t.Fatalf("cyclic dependency = %v, want %s", err, CodeViewSemanticChanged)
		}
		if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), "cycle") {
			t.Fatalf("cyclic dependency cause = %v, want an explicit cycle rejection", cause)
		}
	})
}

func assertViewRegistryDependency(t *testing.T, snapshot viewcompiler.RegistrySnapshot,
	from, want viewcompiler.RelationName) {
	t.Helper()
	relation, ok := snapshot.Relations[from]
	if !ok {
		t.Fatalf("registry omitted %s", from)
	}
	if relation.Kind != viewcompiler.RelationView || len(relation.Dependencies) != 1 || relation.Dependencies[0] != want {
		t.Fatalf("dependencies for %s = %+v, want [%s]", from, relation.Dependencies, want)
	}
}

func viewRegistryTestSchema(t *testing.T, dsn string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	schema := parsed.Query().Get("search_path")
	if schema == "" {
		t.Fatal("test DSN did not include search_path")
	}
	return schema
}

func openViewRegistryTestDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openViewRegistryTestConnector(t *testing.T, ctx context.Context, dsn string) *Connector {
	t.Helper()
	connector, err := New(ctx, Config{
		DSN: dsn, StatementTimeout: time.Second, ConnectTimeout: time.Second,
		MaxRows: 100, MaxConnections: 2,
	})
	if err != nil {
		t.Fatalf("New connector: %v", err)
	}
	t.Cleanup(connector.Close)
	return connector
}
