package gateway

import (
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

func TestBindOrdinalSidecarsProjectsPinnedHandle(t *testing.T) {
	const catalogDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	artifact, err := ordinal.CompileSnapshotArtifact(ordinal.SnapshotSpec{
		SourceID: "source", SourceNamespace: "semantic.expense", Snapshot: "snapshot-v1",
		SchemaDigest: strings.Repeat("1", 64),
		Fields:       []ordinal.SnapshotField{{Name: "id", CanonicalFieldID: "id", SQLType: "text"}},
		Rows: []ordinal.SnapshotRow{
			{EntityKey: "entity-1", Values: map[string]any{"id": "A"}},
			{EntityKey: "entity-2", Values: map[string]any{"id": "B"}},
		},
	})
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact: %v", err)
	}
	publication := catalog.SnapshotPublication{
		Name: "expense-v1", Source: "source", SourceNamespace: "semantic.expense", Snapshot: "snapshot-v1",
		OrdinalSidecar: "taskgate_ordinal.expense_v1", SidecarDigest: artifact.Hot.Manifest().SidecarDigest,
		DictionaryDigest: artifact.Hot.DictionaryDigest(), ManifestDigest: artifact.Hot.ManifestDigest(),
	}
	otherArtifact, err := ordinal.CompileSnapshotArtifact(ordinal.SnapshotSpec{
		SourceID: "source", SourceNamespace: "semantic.other", Snapshot: "snapshot-v1",
		SchemaDigest: strings.Repeat("1", 64),
		Fields:       []ordinal.SnapshotField{{Name: "key", CanonicalFieldID: "key", SQLType: "text"}},
		Rows:         []ordinal.SnapshotRow{{EntityKey: "other-1", Values: map[string]any{"key": "B"}}},
	})
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact(other): %v", err)
	}
	otherPublication := catalog.SnapshotPublication{
		Name: "other-v1", Source: "source", SourceNamespace: "semantic.other", Snapshot: "snapshot-v1",
		OrdinalSidecar: "taskgate_ordinal.other_v1", SidecarDigest: otherArtifact.Hot.Manifest().SidecarDigest,
		DictionaryDigest: otherArtifact.Hot.DictionaryDigest(), ManifestDigest: otherArtifact.Hot.ManifestDigest(),
	}
	registry, err := ordinal.NewRegistry(artifact.Hot)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.RegisterPublication(ordinal.PublicationKey{CatalogDigest: catalogDigest, PublicationName: publication.Name},
		publication.ManifestDigest, artifact.Hot); err != nil {
		t.Fatalf("RegisterPublication: %v", err)
	}
	if err := registry.RegisterPublication(ordinal.PublicationKey{CatalogDigest: catalogDigest, PublicationName: otherPublication.Name},
		otherPublication.ManifestDigest, otherArtifact.Hot); err != nil {
		t.Fatalf("RegisterPublication(other): %v", err)
	}
	service := &Service{catalog: &catalog.Catalog{SHA256: catalogDigest,
		SnapshotPublications: []catalog.SnapshotPublication{publication, otherPublication}}, snapshotRegistry: registry}
	product := queryplan.Product{
		Name: "expense", Columns: map[string]struct{}{"id": {}}, AllowedAggregates: map[string]struct{}{"count": {}},
		ColumnTypes: map[string]string{"id": "text"}, ColumnCollations: map[string]string{"id": "C"},
		CollationVersions: map[string]string{"id": "1"}, SourceNamespace: publication.SourceNamespace,
		Snapshot: publication.Snapshot, StableRole: "expense", StableEntityKey: []string{"id"},
		SnapshotPublication: publication.Name, SidecarManifestDigest: publication.ManifestDigest,
	}
	compiled, err := queryplan.CompileOrdinal(queryplan.QueryPlan{Product: "expense", Columns: []string{"id"}}, product)
	if err != nil {
		t.Fatalf("CompileOrdinal: %v", err)
	}
	bound, err := service.bindOrdinalSidecars(compiled.ProvenanceSQL, compiled.ProvenanceFields, compiled.OrdinalProgram)
	if err != nil {
		t.Fatalf("bindOrdinalSidecars: %v", err)
	}
	if len(bound.SidecarGrants) != 1 || bound.SidecarGrants[0].PhysicalSchema != "taskgate_ordinal" ||
		bound.SidecarGrants[0].PhysicalView != "expense_v1" {
		t.Fatalf("sidecar grants = %#v", bound.SidecarGrants)
	}
	handle := compiled.OrdinalProgram.Sources[0].HandleAlias
	if !strings.Contains(bound.ProvenanceSQL, `"row_handle" AS "`+handle+`"`) ||
		!strings.Contains(bound.ProvenanceSQL, `INNER JOIN "ordinal_sidecar_`) ||
		!strings.Contains(bound.ProvenanceSQL, `"id" = "tg_v4_sidecar_0"."id"`) {
		t.Fatalf("bound provenance SQL omitted exact sidecar join:\n%s", bound.ProvenanceSQL)
	}
	if err := bound.Program.ValidateProvenanceFields(bound.ProvenanceFields); err != nil {
		t.Fatalf("bound provenance contract: %v", err)
	}
	if bound.DictionarySetDigest == "" || len(bound.Indexes) != 1 || len(bound.DictionarySet.Members) != 2 {
		t.Fatalf("bound dictionary evidence = %#v", bound)
	}
	if bound.EstimatedBaseFacts != 4 || estimateOrdinalBaseFacts(bound, 1) != 2 {
		t.Fatalf("ordinal fact-lane estimates: full=%d limited=%d", bound.EstimatedBaseFacts, estimateOrdinalBaseFacts(bound, 1))
	}
	if bound.DictionarySet.Members[0].PublicationName != "expense-v1" || bound.DictionarySet.Members[1].PublicationName != "other-v1" {
		t.Fatalf("dictionary universe is not Catalog-wide and canonical: %#v", bound.DictionarySet.Members)
	}
	otherProduct := queryplan.Product{Name: "other", Columns: map[string]struct{}{"key": {}},
		AllowedAggregates: map[string]struct{}{"count": {}}, ColumnTypes: map[string]string{"key": "text"},
		ColumnCollations: map[string]string{"key": "C"}, CollationVersions: map[string]string{"key": "1"},
		SourceNamespace: otherPublication.SourceNamespace, Snapshot: otherPublication.Snapshot,
		StableRole: "other", StableEntityKey: []string{"key"}, SnapshotPublication: otherPublication.Name,
		SidecarManifestDigest: otherPublication.ManifestDigest}
	otherCompiled, err := queryplan.CompileOrdinal(queryplan.QueryPlan{Product: "other", Columns: []string{"key"}}, otherProduct)
	if err != nil {
		t.Fatalf("CompileOrdinal(other): %v", err)
	}
	otherBound, err := service.bindOrdinalSidecars(otherCompiled.ProvenanceSQL, otherCompiled.ProvenanceFields, otherCompiled.OrdinalProgram)
	if err != nil {
		t.Fatalf("bindOrdinalSidecars(other): %v", err)
	}
	if otherBound.DictionarySetDigest != bound.DictionarySetDigest {
		t.Fatalf("two products bound different root dictionary universes: %s != %s",
			otherBound.DictionarySetDigest, bound.DictionarySetDigest)
	}
}

func TestBindOrdinalSidecarsUnionsKeyColumnsForSharedPublication(t *testing.T) {
	const catalogDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	artifact, err := ordinal.CompileSnapshotArtifact(ordinal.SnapshotSpec{
		SourceID: "source", SourceNamespace: "semantic.shared", Snapshot: "snapshot-v1",
		SchemaDigest: strings.Repeat("1", 64),
		Fields: []ordinal.SnapshotField{
			{Name: "order_id", CanonicalFieldID: "orders.order_id", SQLType: "text"},
			{Name: "line_id", CanonicalFieldID: "lines.line_id", SQLType: "text"},
			{Name: "link", CanonicalFieldID: "shared.link", SQLType: "text"},
		},
		Rows: []ordinal.SnapshotRow{{EntityKey: "shared-1", Values: map[string]any{
			"order_id": "O-1", "line_id": "L-1", "link": "K-1",
		}}},
	})
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact: %v", err)
	}
	publication := catalog.SnapshotPublication{
		Name: "shared-v1", Source: "source", SourceNamespace: "semantic.shared", Snapshot: "snapshot-v1",
		OrdinalSidecar: "taskgate_ordinal.shared_v1", SidecarDigest: artifact.Hot.Manifest().SidecarDigest,
		DictionaryDigest: artifact.Hot.DictionaryDigest(), ManifestDigest: artifact.Hot.ManifestDigest(),
	}
	registry, err := ordinal.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.RegisterPublication(ordinal.PublicationKey{
		CatalogDigest: catalogDigest, PublicationName: publication.Name,
	}, publication.ManifestDigest, artifact.Hot); err != nil {
		t.Fatalf("RegisterPublication: %v", err)
	}
	service := &Service{
		catalog: &catalog.Catalog{SHA256: catalogDigest,
			SnapshotPublications: []catalog.SnapshotPublication{publication}},
		snapshotRegistry: registry,
	}
	product := func(name, role, entityKey string, columns ...string) queryplan.Product {
		columnSet := make(map[string]struct{}, len(columns))
		types := make(map[string]string, len(columns))
		collations := make(map[string]string, len(columns))
		versions := make(map[string]string, len(columns))
		for _, column := range columns {
			columnSet[column] = struct{}{}
			types[column] = "text"
			collations[column] = "C"
			versions[column] = "1"
		}
		return queryplan.Product{
			Name: name, Columns: columnSet, AllowedAggregates: map[string]struct{}{"count": {}},
			ColumnTypes: types, ColumnCollations: collations, CollationVersions: versions,
			SourceNamespace: publication.SourceNamespace, Snapshot: publication.Snapshot,
			StableRole: role, StableEntityKey: []string{entityKey},
			SnapshotPublication: publication.Name, SidecarManifestDigest: publication.ManifestDigest,
		}
	}
	products := map[string]queryplan.Product{
		"orders": product("orders", "orders", "order_id", "order_id", "link"),
		"lines":  product("lines", "lines", "line_id", "line_id", "link"),
	}
	plan := queryplan.QueryPlan{
		From: &queryplan.From{JoinMany: &queryplan.JoinMany{
			Sources: []queryplan.Scan{{Product: "orders", Role: "orders"}, {Product: "lines", Role: "lines"}},
			On:      []queryplan.JoinPredicate{{Left: "orders.link", Right: "lines.link"}},
		}},
		Columns: []string{"orders.order_id", "lines.line_id"},
	}
	compiled, err := queryplan.CompileRelational(plan, products)
	if err != nil {
		t.Fatalf("CompileRelational: %v", err)
	}
	if len(compiled.OrdinalProgram.Sources) != 2 ||
		compiled.OrdinalProgram.Sources[0].SidecarBinding.PublicationID != publication.Name ||
		compiled.OrdinalProgram.Sources[1].SidecarBinding.PublicationID != publication.Name {
		t.Fatalf("program does not share one publication: %#v", compiled.OrdinalProgram.Sources)
	}
	bound, err := service.bindOrdinalSidecars(
		compiled.ProvenanceSQL, compiled.ProvenanceFields, compiled.OrdinalProgram,
	)
	if err != nil {
		t.Fatalf("bindOrdinalSidecars: %v", err)
	}
	if len(bound.SidecarGrants) != 1 {
		t.Fatalf("sidecar grants = %#v, want one shared logical grant", bound.SidecarGrants)
	}
	grant := bound.SidecarGrants[0]
	wantColumns := []string{"line_id", "order_id", "row_handle"}
	if grant.PhysicalSchema != "taskgate_ordinal" || grant.PhysicalView != "shared_v1" ||
		!reflect.DeepEqual(grant.ApprovedColumns, wantColumns) ||
		!reflect.DeepEqual(grant.AllowedOperators, []string{"="}) {
		t.Fatalf("merged sidecar grant = %#v, want columns %v", grant, wantColumns)
	}
	logical := ordinalSidecarLogicalName(publication.Name)
	if grant.LogicalName != logical ||
		strings.Count(bound.ProvenanceSQL, `INNER JOIN "`+logical+`"`) != 2 ||
		!strings.Contains(bound.ProvenanceSQL, `."order_id"`) ||
		!strings.Contains(bound.ProvenanceSQL, `."line_id"`) {
		t.Fatalf("shared-publication SQL does not contain two key-specific joins:\n%s", bound.ProvenanceSQL)
	}
	if len(bound.Indexes) != 2 || bound.Indexes["orders"] != artifact.Hot || bound.Indexes["lines"] != artifact.Hot {
		t.Fatalf("shared-publication indexes = %#v", bound.Indexes)
	}
	if err := bound.Program.ValidateProvenanceFields(bound.ProvenanceFields); err != nil {
		t.Fatalf("bound provenance contract: %v", err)
	}

	policyGrant, err := extendOrdinalPolicyGrant(sqlpolicy.Grant{Products: []sqlpolicy.ProductGrant{
		{LogicalName: "orders", PhysicalSchema: "reporting", PhysicalView: "orders",
			ApprovedColumns: []string{"order_id", "link"}, AllowedOperators: []string{"="}},
		{LogicalName: "lines", PhysicalSchema: "reporting", PhysicalView: "lines",
			ApprovedColumns: []string{"line_id", "link"}, AllowedOperators: []string{"="}},
	}}, bound.SidecarGrants)
	if err != nil {
		t.Fatalf("extendOrdinalPolicyGrant: %v", err)
	}
	decision, err := sqlpolicy.New(sqlpolicy.Config{}).Authorize(sqlpolicy.Request{
		SQL: bound.ProvenanceSQL, Grant: policyGrant, RowLimit: 25,
	})
	if err != nil {
		t.Fatalf("authorize shared-publication provenance SQL: %v\nSQL: %s", err, bound.ProvenanceSQL)
	}
	if strings.Count(decision.SQL, `"taskgate_ordinal"."shared_v1"`) != 1 ||
		!strings.Contains(decision.SQL, `SELECT "line_id", "order_id", "row_handle"`) ||
		strings.Count(decision.SQL, "JOIN "+logical) != 2 {
		t.Fatalf("policy did not expose one merged sidecar CTE to both joins:\n%s", decision.SQL)
	}
}

func TestBindOrdinalSidecarsRejectsCatalogRegistryMismatch(t *testing.T) {
	program := queryplan.OrdinalProgram{Version: queryplan.OrdinalProgramVersion, Kind: "scan",
		Sources: []queryplan.OrdinalSource{{Product: "expense", SourceAlias: "expense", SourceNamespace: "semantic.expense",
			Snapshot: "snapshot-v1", Role: "expense", HandleAlias: "tg_handle", HandleRequired: true,
			SidecarBinding: queryplan.OrdinalSidecarBinding{Required: true, PublicationID: "expense-v1", ManifestDigest: strings.Repeat("1", 64)}, Branch: -1,
			EntityKey:      []queryplan.OrdinalFieldBinding{{FieldID: "id", Column: "id", ProvenanceAlias: "id", SQLType: "text", CanonicalExpression: "semantic.expense.id", IsEntityKey: true}},
			EvidenceFields: []queryplan.OrdinalFieldBinding{{FieldID: "id", Column: "id", ProvenanceAlias: "id", SQLType: "text", CanonicalExpression: "semantic.expense.id", IsEntityKey: true}}}},
		Visible:              []queryplan.OrdinalVisibleSpec{{Kind: "field", OutputID: "id", ResultAlias: "id", FieldID: "id", SQLType: "text", CanonicalExpression: "semantic.expense.id"}},
		WitnessRules:         []queryplan.OrdinalWitnessRule{{StageOrder: 10, Stage: "scan", TargetID: "$row", TargetExpression: "$row", SourceAlias: "expense", InputKind: "base_row", InputExpression: "semantic.expense.$row", Multiplicity: 1, Merge: "add"}},
		CanonicalExpressions: []string{"semantic.expense.id"}, SnapshotBundle: []queryplan.OrdinalSnapshotBinding{{SourceNamespace: "semantic.expense", Snapshot: "snapshot-v1", PublicationID: "expense-v1", SidecarManifestDigest: strings.Repeat("1", 64)}},
	}
	registry, _ := ordinal.NewRegistry()
	service := &Service{catalog: &catalog.Catalog{SHA256: strings.Repeat("a", 64), SnapshotPublications: []catalog.SnapshotPublication{{
		Name: "expense-v1", Source: "source", SourceNamespace: "semantic.expense", Snapshot: "snapshot-v1",
		OrdinalSidecar: "taskgate_ordinal.expense_v1", ManifestDigest: strings.Repeat("1", 64),
		DictionaryDigest: strings.Repeat("2", 64), SidecarDigest: strings.Repeat("3", 64),
	}}}, snapshotRegistry: registry}
	if _, err := service.bindOrdinalSidecars(`SELECT "id" FROM "expense"`, []string{"id"}, program); err == nil {
		t.Fatal("missing Catalog-bound registry index was accepted")
	}
}
