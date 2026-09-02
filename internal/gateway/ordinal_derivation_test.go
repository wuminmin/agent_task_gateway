package gateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

const ordinalDerivationPlanDigest = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

type ordinalDerivationFixture struct {
	program    queryplan.OrdinalProgram
	artifact   ordinal.CompiledArtifact
	rows       []map[string]any
	entityKeys []string
	columns    []dataconnector.Column
	positions  map[string]int
}

type ordinalProvenanceInput struct {
	row    int
	branch int
	handle ordinal.RowHandle
}

func TestOrdinalDerivationScanExactlyRefinesV2Oracle(t *testing.T) {
	product := compactOrdinalProduct()
	compiled, err := queryplan.CompileOrdinal(queryplan.QueryPlan{
		Product: product.Name, Columns: []string{"department", "amount"},
	}, product)
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{
		{"id": int64(1), "department": "Engineering", "amount": int64(7)},
		{"id": int64(2), "department": "Sales", "amount": int64(20)},
	}
	fixture := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, rows)
	visible := scanVisibleResult(t, fixture.program, rows)
	effect := fixture.derive(t, visible, []ordinalProvenanceInput{{row: 0, branch: -1}, {row: 1, branch: -1}})

	oracleRelation := fixture.oracleRelation(t, fixture.program.Sources[0], []int{0, 1}, false)
	visibleFields := ordinalOracleVisibleFields(fixture.program)
	oracle, err := exposure.ObserveV2(oracleRelation, visibleFields...)
	if err != nil {
		t.Fatal(err)
	}
	assertOrdinalEffectEqualsOracle(t, effect, fixture.artifact.Cold, oracle, visible, ordinalDerivationPlanDigest)
}

func TestOrdinalDerivationSingleProductAliasesRefineLegacyScanAndPage(t *testing.T) {
	product := compactOrdinalProduct()
	rows := []map[string]any{
		{"id": int64(1), "department": "Engineering", "amount": int64(7)},
		{"id": int64(2), "department": "Sales", "amount": int64(20)},
		{"id": int64(3), "department": "Sales", "amount": int64(30)},
		{"id": int64(4), "department": "Finance", "amount": int64(40)},
	}
	tests := []struct {
		name     string
		plan     queryplan.QueryPlan
		selected []int
	}{
		{name: "scan", plan: queryplan.QueryPlan{Product: product.Name,
			Columns: []string{"id", "department", "amount"}, OrderBy: []queryplan.Order{{Column: "id", Direction: "asc"}}},
			selected: []int{0, 1, 2, 3}},
		{name: "page", plan: queryplan.QueryPlan{Product: product.Name,
			Columns: []string{"id", "department", "amount"}, OrderBy: []queryplan.Order{{Column: "id", Direction: "asc"}},
			Limit: 2, Offset: 1}, selected: []int{1, 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled, err := queryplan.CompileOrdinal(test.plan, product)
			if err != nil {
				t.Fatal(err)
			}
			fixture := newOrdinalDerivationFixtureWithRoleAliases(t, compiled.OrdinalProgram, rows)
			selectedRows := make([]map[string]any, len(test.selected))
			inputs := make([]ordinalProvenanceInput, len(test.selected))
			for index, rowIndex := range test.selected {
				selectedRows[index] = rows[rowIndex]
				inputs[index] = ordinalProvenanceInput{row: rowIndex, branch: -1}
			}
			visible := scanVisibleResult(t, fixture.program, selectedRows)
			effect := fixture.derive(t, visible, inputs)

			source := fixture.program.Sources[0]
			oracleRelation := fixture.oracleRelation(t, source, test.selected, false)
			oracle, err := exposure.ObserveV2(oracleRelation, ordinalOracleVisibleFields(fixture.program)...)
			if err != nil {
				t.Fatal(err)
			}
			assertOrdinalEffectEqualsOracle(t, effect, fixture.artifact.Cold, oracle, visible, ordinalDerivationPlanDigest)

			publishedRow, found := fixture.artifact.Hot.LookupRow(1)
			if !found {
				t.Fatal("hybrid publication omits its first row")
			}
			for _, binding := range source.EvidenceFields {
				roleQualified := source.Role + "." + binding.Column
				if _, legacyFound := publishedRow.Cells[binding.FieldID]; !legacyFound {
					t.Fatalf("publication omits legacy field alias %q", binding.FieldID)
				}
				if _, relationalFound := publishedRow.Cells[roleQualified]; !relationalFound {
					t.Fatalf("publication omits relational field alias %q", roleQualified)
				}
			}
		})
	}
}

func TestOrdinalDerivationGroupedExactlyRefinesV2Oracle(t *testing.T) {
	product := compactOrdinalProduct()
	compiled, err := queryplan.CompileOrdinal(queryplan.QueryPlan{
		Product: product.Name, Columns: []string{"department"}, GroupBy: []string{"department"},
		Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "amount", Alias: "total"}},
	}, product)
	if err != nil {
		t.Fatal(err)
	}
	// Provenance is companion-ordered by canonical group and then entity key.
	rows := []map[string]any{
		{"id": int64(1), "department": "Engineering", "amount": int64(7)},
		{"id": int64(2), "department": "Sales", "amount": int64(10)},
		{"id": int64(3), "department": "Sales", "amount": int64(20)},
	}
	fixture := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, rows)
	visibleMaps := []map[string]any{
		groupedVisibleValues(fixture.program, "Engineering", int64(7)),
		groupedVisibleValues(fixture.program, "Sales", int64(30)),
	}
	visible := ordinalVisibleResult(fixture.program, visibleMaps)
	effect := fixture.derive(t, visible, []ordinalProvenanceInput{
		{row: 0, branch: -1}, {row: 1, branch: -1}, {row: 2, branch: -1},
	})

	base := fixture.oracleRelation(t, fixture.program.Sources[0], []int{0, 1, 2}, false)
	aggregated := aggregateOracleRelation(t, fixture.program, base, visibleMaps)
	oracle, err := exposure.ObserveV2(aggregated, ordinalOracleVisibleFields(fixture.program)...)
	if err != nil {
		t.Fatal(err)
	}
	assertOrdinalEffectEqualsOracle(t, effect, fixture.artifact.Cold, oracle, visible, ordinalDerivationPlanDigest)
	if len(effect.DerivedRelease) != 4 {
		t.Fatalf("grouped dynamic releases = %d, want two fields for two groups", len(effect.DerivedRelease))
	}
}

func TestOrdinalDerivationJoinGroupExactlyRefinesV2Oracle(t *testing.T) {
	leftProduct := compactOrdinalProduct()
	leftProduct.Name = "left_product"
	leftProduct.StableRole = "left"
	rightProduct := compactOrdinalProduct()
	rightProduct.Name = "right_product"
	rightProduct.StableRole = "right"
	plan := queryplan.QueryPlan{
		From: &queryplan.From{Join: &queryplan.Join{
			Left:  queryplan.Scan{Product: leftProduct.Name, Role: "left"},
			Right: queryplan.Scan{Product: rightProduct.Name, Role: "right"},
			On:    []queryplan.JoinPredicate{{Left: "left.department", Right: "right.department"}},
		}},
		Columns:    []string{"left.department"},
		GroupBy:    []string{"left.department"},
		Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "right.amount", Alias: "total"}},
	}
	compiled, err := queryplan.CompileRelational(plan, map[string]queryplan.Product{
		leftProduct.Name: leftProduct, rightProduct.Name: rightProduct,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{{"id": int64(1), "department": "Sales", "amount": int64(10)}}
	fixture := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, rows)
	visibleMaps := []map[string]any{groupedVisibleValues(fixture.program, "Sales", int64(10))}
	visible := ordinalVisibleResult(fixture.program, visibleMaps)
	deriver := fixture.newDeriver(t, visible, fixture.indexes())
	member, err := deriver.buildMember(fixture.provenanceValues(t, ordinalProvenanceInput{row: 0, branch: -1}))
	if err != nil {
		t.Fatal(err)
	}
	if member.key != "" {
		t.Fatalf("grouped join materialized unused member key %q", member.key)
	}
	effect := fixture.derive(t, visible, []ordinalProvenanceInput{{row: 0, branch: -1}})

	left := fixture.oracleRelation(t, ordinalSourceForAlias(t, fixture.program, "left"), []int{0}, false)
	right := fixture.oracleRelation(t, ordinalSourceForAlias(t, fixture.program, "right"), []int{0}, false)
	predicates := make([]exposure.JoinPredicateV2, 0, len(fixture.program.Joins))
	for _, predicate := range fixture.program.Joins {
		predicates = append(predicates, exposure.JoinPredicateV2{LeftField: predicate.Left.FieldID, RightField: predicate.Right.FieldID})
	}
	joined, err := exposure.JoinOnV2(left, right, predicates)
	if err != nil {
		t.Fatal(err)
	}
	aggregated := aggregateOracleRelation(t, fixture.program, joined, visibleMaps)
	oracle, err := exposure.ObserveV2(aggregated, ordinalOracleVisibleFields(fixture.program)...)
	if err != nil {
		t.Fatal(err)
	}
	assertOrdinalEffectEqualsOracle(t, effect, fixture.artifact.Cold, oracle, visible, ordinalDerivationPlanDigest)
}

func TestOrdinalDerivationUngroupedJoinRetainsCanonicalRowKey(t *testing.T) {
	leftProduct := compactOrdinalProduct()
	leftProduct.Name = "left_product"
	leftProduct.StableRole = "left"
	rightProduct := compactOrdinalProduct()
	rightProduct.Name = "right_product"
	rightProduct.StableRole = "right"
	plan := queryplan.QueryPlan{
		From: &queryplan.From{Join: &queryplan.Join{
			Left:  queryplan.Scan{Product: leftProduct.Name, Role: "left"},
			Right: queryplan.Scan{Product: rightProduct.Name, Role: "right"},
			On:    []queryplan.JoinPredicate{{Left: "left.department", Right: "right.department"}},
		}},
		Columns: []string{"left.department", "right.amount"},
	}
	compiled, err := queryplan.CompileRelational(plan, map[string]queryplan.Product{
		leftProduct.Name: leftProduct, rightProduct.Name: rightProduct,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{{"id": int64(1), "department": "Sales", "amount": int64(10)}}
	fixture := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, rows)
	visible := scanVisibleResult(t, fixture.program, rows)
	deriver := fixture.newDeriver(t, visible, fixture.indexes())
	values := fixture.provenanceValues(t, ordinalProvenanceInput{row: 0, branch: -1})
	member, err := deriver.buildMember(values)
	if err != nil {
		t.Fatal(err)
	}
	sources := make([]ordinalSourceMember, 0, len(fixture.program.Sources))
	for _, source := range fixture.program.Sources {
		sourceMember, sourceErr := deriver.buildSourceMember(source, values)
		if sourceErr != nil {
			t.Fatal(sourceErr)
		}
		sources = append(sources, sourceMember)
	}
	want, err := ordinalJoinRowKey(sources)
	if err != nil {
		t.Fatal(err)
	}
	if member.key == "" || member.key != want {
		t.Fatalf("ungrouped join member key = %q, want %q", member.key, want)
	}
}

func TestOrdinalDerivationEmptyGlobalAggregateRefinesV2Oracle(t *testing.T) {
	product := compactOrdinalProduct()
	compiled, err := queryplan.CompileOrdinal(queryplan.QueryPlan{
		Product:    product.Name,
		Filters:    []queryplan.Filter{{Column: "amount", Op: ">", Value: float64(100)}},
		Aggregates: []queryplan.Aggregate{{Function: "count", Column: "*", Alias: "rows"}},
	}, product)
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{{"id": int64(1), "amount": int64(10)}}
	fixture := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, rows)
	visibleMaps := []map[string]any{groupedVisibleValues(fixture.program, nil, int64(0))}
	visible := ordinalVisibleResult(fixture.program, visibleMaps)
	effect := fixture.derive(t, visible, nil)

	empty := fixture.oracleRelation(t, fixture.program.Sources[0], nil, false)
	aggregated := aggregateOracleRelation(t, fixture.program, empty, visibleMaps)
	oracle, err := exposure.ObserveV2(aggregated, ordinalOracleVisibleFields(fixture.program)...)
	if err != nil {
		t.Fatal(err)
	}
	assertOrdinalEffectEqualsOracle(t, effect, fixture.artifact.Cold, oracle, visible, ordinalDerivationPlanDigest)
}

func TestOrdinalDerivationUnionAlternativesUseMaxAndRefineV2Oracle(t *testing.T) {
	product := compactUnionProduct()
	plan := queryplan.QueryPlan{
		From: &queryplan.From{UnionDistinct: &queryplan.UnionDistinct{
			Role: "summary", Columns: []string{"department", "month"},
			Left: queryplan.Scan{Product: product.Name, Role: "left_branch",
				Filters: []queryplan.Filter{{Column: "month", Op: "=", Value: "2026-01"}}},
			Right: queryplan.Scan{Product: product.Name, Role: "right_branch",
				Filters: []queryplan.Filter{{Column: "department", Op: "=", Value: "Sales"}}},
		}},
		Columns: []string{"summary.department"}, GroupBy: []string{"summary.department"},
		Aggregates: []queryplan.Aggregate{{Function: "count", Column: "*", Alias: "rows"}},
	}
	compiled, err := queryplan.CompileRelational(plan, map[string]queryplan.Product{product.Name: product})
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{{"id": int64(1), "department": "Sales", "month": "2026-01"}}
	fixture := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, rows)
	visibleMaps := []map[string]any{groupedVisibleValues(fixture.program, "Sales", int64(1))}
	visible := ordinalVisibleResult(fixture.program, visibleMaps)
	effect := fixture.derive(t, visible, []ordinalProvenanceInput{{row: 0, branch: 0}, {row: 0, branch: 1}})

	left := fixture.oracleRelation(t, ordinalSourceForBranch(t, fixture.program, 0), []int{0}, true)
	right := fixture.oracleRelation(t, ordinalSourceForBranch(t, fixture.program, 1), []int{0}, true)
	union, err := exposure.UnionDistinctV2(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if len(union.Rows) != 1 {
		t.Fatalf("union rows = %d, want one alternative class", len(union.Rows))
	}
	witnessByField := make(map[string]uint64)
	for _, item := range union.Rows[0].RowWitness {
		key := "$row"
		if item.Fact.Kind == exposure.FactBaseCell {
			key = item.Fact.Field
		}
		witnessByField[key] = item.Multiplicity
	}
	if witnessByField["$row"] != 1 || witnessByField["summary.department"] != 2 || witnessByField["summary.month"] != 2 {
		t.Fatalf("legacy alternative max witness = %#v, want row=1 department=2 month=2", witnessByField)
	}
	aggregated := aggregateOracleRelation(t, fixture.program, union, visibleMaps)
	oracle, err := exposure.ObserveV2(aggregated, ordinalOracleVisibleFields(fixture.program)...)
	if err != nil {
		t.Fatal(err)
	}
	assertOrdinalEffectEqualsOracle(t, effect, fixture.artifact.Cold, oracle, visible, ordinalDerivationPlanDigest)
}

func TestOrdinalDerivationUnionShortRowFailsClosed(t *testing.T) {
	product := compactUnionProduct()
	plan := queryplan.QueryPlan{
		From: &queryplan.From{UnionDistinct: &queryplan.UnionDistinct{
			Role: "summary", Columns: []string{"department", "month"},
			Left:  queryplan.Scan{Product: product.Name, Role: "left_branch"},
			Right: queryplan.Scan{Product: product.Name, Role: "right_branch"},
		}},
		Columns: []string{"summary.department", "summary.month"},
	}
	compiled, err := queryplan.CompileRelational(plan, map[string]queryplan.Product{product.Name: product})
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{{"id": int64(1), "department": "Sales", "month": "2026-01"}}
	fixture := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, rows)
	visible := scanVisibleResult(t, fixture.program, rows)
	deriver := fixture.newDeriver(t, visible, fixture.indexes())
	if err := derivationError(deriver, nil); err == nil || !strings.Contains(err.Error(), "branch identity") {
		t.Fatalf("short UNION provenance row error = %v", err)
	}
}

func TestOrdinalDerivationFailsClosedOnHandleBoundsAndManifest(t *testing.T) {
	product := compactOrdinalProduct()
	compiled, err := queryplan.CompileOrdinal(queryplan.QueryPlan{Product: product.Name, Columns: []string{"amount"}}, product)
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{{"id": int64(1), "amount": int64(10)}}
	fixture := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, rows)
	visible := scanVisibleResult(t, fixture.program, rows)

	t.Run("unknown handle", func(t *testing.T) {
		deriver := fixture.newDeriver(t, visible, fixture.indexes())
		values := fixture.provenanceValues(t, ordinalProvenanceInput{row: 0, branch: -1,
			handle: ordinal.RowHandle(fixture.artifact.Hot.RowCount() + 1)})
		if err := derivationError(deriver, values); err == nil || !strings.Contains(err.Error(), "unknown row handle") {
			t.Fatalf("unknown handle error = %v", err)
		}
	})

	t.Run("valid handle remapped to another entity", func(t *testing.T) {
		remapRows := []map[string]any{
			{"id": int64(1), "amount": int64(10)},
			{"id": int64(2), "amount": int64(20)},
		}
		remap := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, remapRows)
		deriver := remap.newDeriver(t, scanVisibleResult(t, remap.program, remapRows[:1]), remap.indexes())
		otherHandle, found := remap.artifact.Hot.LookupRowHandle(remap.entityKeys[1])
		if !found {
			t.Fatal("HOT index omits the remap target")
		}
		values := remap.provenanceValues(t, ordinalProvenanceInput{row: 0, branch: -1, handle: otherHandle})
		if err := derivationError(deriver, values); err == nil ||
			!strings.Contains(err.Error(), "does not match its provenance entity key") {
			t.Fatalf("remapped handle error = %v", err)
		}
	})

	t.Run("out of bounds ordinal", func(t *testing.T) {
		source := fixture.program.Sources[0]
		corrupt := &outOfBoundsRowIndex{SnapshotIndex: fixture.artifact.Hot}
		// Activate against the exact publication-bound index first. Replacing
		// only the query-private lookup view then models a corrupt ordinal
		// returned after activation, while the trusted MultiIndex retains the
		// original manifest and bounds used by Finish.
		deriver := fixture.newDeriver(t, visible, fixture.indexes())
		deriver.indexes[source.SourceAlias] = corrupt
		if err := deriver.Row(context.Background(), fixture.provenanceValues(t, ordinalProvenanceInput{row: 0, branch: -1})); err != nil {
			t.Fatalf("Row failed before final bounds check: %v", err)
		}
		if _, err := deriver.Finish(); !errors.Is(err, ordinal.ErrUnknownFact) {
			t.Fatalf("out-of-bounds Finish error = %v, want ErrUnknownFact", err)
		}
	})

	t.Run("concrete decorator cannot promote past defensive lookup", func(t *testing.T) {
		source := fixture.program.Sources[0]
		corrupt := &promotedOutOfBoundsRowIndex{HotDictionary: fixture.artifact.Hot}
		deriver := fixture.newDeriver(t, visible, fixture.indexes())
		deriver.indexes[source.SourceAlias] = corrupt
		if err := deriver.Row(context.Background(), fixture.provenanceValues(t,
			ordinalProvenanceInput{row: 0, branch: -1})); err != nil {
			t.Fatalf("Row failed before final bounds check: %v", err)
		}
		if _, err := deriver.Finish(); !errors.Is(err, ordinal.ErrUnknownFact) {
			t.Fatalf("promoted decorator bypassed LookupRow: %v, want ErrUnknownFact", err)
		}
	})

	t.Run("publication manifest mismatch", func(t *testing.T) {
		otherRows := []map[string]any{{"id": int64(1), "amount": int64(999)}}
		other := compileOrdinalArtifact(t, fixture.program, otherRows)
		source := fixture.program.Sources[0]
		if other.Hot.ManifestDigest() == fixture.artifact.Hot.ManifestDigest() {
			t.Fatal("test setup did not produce a distinct manifest")
		}
		if _, err := newOrdinalDeriver(fixture.program, map[string]ordinal.SnapshotIndex{source.SourceAlias: other.Hot},
			visible, ordinalDerivationPlanDigest); err == nil || !strings.Contains(err.Error(), "matching snapshot index") {
			t.Fatalf("manifest mismatch error = %v", err)
		}
	})
}

type outOfBoundsRowIndex struct{ ordinal.SnapshotIndex }

func (i *outOfBoundsRowIndex) LookupRow(handle ordinal.RowHandle) (ordinal.RowRefs, bool) {
	row, found := i.SnapshotIndex.LookupRow(handle)
	if !found {
		return ordinal.RowRefs{}, false
	}
	count, found := i.SnapshotIndex.SegmentFactCount(row.Row.SegmentID)
	if !found || count > uint64(^uint32(0)) {
		return ordinal.RowRefs{}, false
	}
	row.Row.Ordinal = uint32(count)
	return row, true
}

type promotedOutOfBoundsRowIndex struct{ *ordinal.HotDictionary }

func (i *promotedOutOfBoundsRowIndex) LookupRow(handle ordinal.RowHandle) (ordinal.RowRefs, bool) {
	row, found := i.HotDictionary.LookupRow(handle)
	if !found {
		return ordinal.RowRefs{}, false
	}
	count, found := i.HotDictionary.SegmentFactCount(row.Row.SegmentID)
	if !found || count > uint64(^uint32(0)) {
		return ordinal.RowRefs{}, false
	}
	row.Row.Ordinal = uint32(count)
	return row, true
}

func newOrdinalDerivationFixture(t *testing.T, program queryplan.OrdinalProgram, rows []map[string]any) ordinalDerivationFixture {
	t.Helper()
	// The program contains slices, so a struct copy alone would let one fixture
	// rewrite another fixture's publication binding through shared backing
	// arrays. Keep each immutable-publication test genuinely isolated.
	program.Sources = append([]queryplan.OrdinalSource(nil), program.Sources...)
	program.SnapshotBundle = append([]queryplan.OrdinalSnapshotBinding(nil), program.SnapshotBundle...)
	artifact := compileOrdinalArtifact(t, program, rows)
	for sourceIndex := range program.Sources {
		program.Sources[sourceIndex].SidecarBinding.ManifestDigest = artifact.Hot.ManifestDigest()
	}
	for bindingIndex := range program.SnapshotBundle {
		program.SnapshotBundle[bindingIndex].SidecarManifestDigest = artifact.Hot.ManifestDigest()
	}
	if err := program.ValidateBoundSidecars(); err != nil {
		t.Fatalf("bind program manifest: %v", err)
	}
	fixture := ordinalDerivationFixture{program: program, artifact: artifact, rows: cloneRawRows(rows)}
	fixture.entityKeys = make([]string, len(rows))
	for rowIndex, row := range rows {
		fixture.entityKeys[rowIndex] = ordinalFixtureEntityKey(t, program.Sources[0], row)
	}
	fixture.columns, fixture.positions = ordinalProvenanceColumns(program)
	return fixture
}

func newOrdinalDerivationFixtureWithRoleAliases(t *testing.T, program queryplan.OrdinalProgram,
	rows []map[string]any) ordinalDerivationFixture {
	t.Helper()
	if len(program.Sources) != 1 {
		t.Fatal("single-product alias fixture requires exactly one source")
	}
	source := program.Sources[0]
	extra := make([]ordinal.SnapshotField, 0, len(source.EvidenceFields))
	for _, binding := range source.EvidenceFields {
		extra = append(extra, ordinal.SnapshotField{Name: binding.Column,
			CanonicalFieldID: source.Role + "." + binding.Column, SQLType: binding.SQLType})
	}
	fixture := newOrdinalDerivationFixture(t, program, rows)
	fixture.artifact = compileOrdinalArtifactWithExtraFields(t, fixture.program, rows, extra)
	for index := range fixture.program.Sources {
		fixture.program.Sources[index].SidecarBinding.ManifestDigest = fixture.artifact.Hot.ManifestDigest()
	}
	for index := range fixture.program.SnapshotBundle {
		fixture.program.SnapshotBundle[index].SidecarManifestDigest = fixture.artifact.Hot.ManifestDigest()
	}
	return fixture
}

func compileOrdinalArtifact(t *testing.T, program queryplan.OrdinalProgram, rows []map[string]any) ordinal.CompiledArtifact {
	return compileOrdinalArtifactWithExtraFields(t, program, rows, nil)
}

func compileOrdinalArtifactWithExtraFields(t *testing.T, program queryplan.OrdinalProgram, rows []map[string]any,
	extra []ordinal.SnapshotField) ordinal.CompiledArtifact {
	t.Helper()
	if len(program.Sources) == 0 {
		t.Fatal("program has no sources")
	}
	fieldByID := make(map[string]ordinal.SnapshotField)
	inputTypes := make(map[string]string)
	for _, source := range program.Sources {
		for _, binding := range source.EvidenceFields {
			if previous, exists := fieldByID[binding.FieldID]; exists &&
				(previous.Name != binding.Column || previous.SQLType != binding.SQLType) {
				t.Fatalf("conflicting canonical field %q", binding.FieldID)
			}
			fieldByID[binding.FieldID] = ordinal.SnapshotField{Name: binding.Column,
				CanonicalFieldID: binding.FieldID, SQLType: binding.SQLType}
			if previous, exists := inputTypes[binding.Column]; exists && previous != binding.SQLType {
				t.Fatalf("conflicting physical field %q", binding.Column)
			}
			inputTypes[binding.Column] = binding.SQLType
		}
	}
	for _, field := range extra {
		if previous, exists := fieldByID[field.CanonicalFieldID]; exists && previous != field {
			t.Fatalf("conflicting extra canonical field %q", field.CanonicalFieldID)
		}
		if previous, exists := inputTypes[field.Name]; exists && previous != field.SQLType {
			t.Fatalf("conflicting extra physical field %q", field.Name)
		}
		fieldByID[field.CanonicalFieldID] = field
		inputTypes[field.Name] = field.SQLType
	}
	fieldIDs := make([]string, 0, len(fieldByID))
	for fieldID := range fieldByID {
		fieldIDs = append(fieldIDs, fieldID)
	}
	sort.Strings(fieldIDs)
	fields := make([]ordinal.SnapshotField, 0, len(fieldIDs))
	for _, fieldID := range fieldIDs {
		fields = append(fields, fieldByID[fieldID])
	}
	snapshotRows := make([]ordinal.SnapshotRow, len(rows))
	for rowIndex, row := range rows {
		values := make(map[string]any, len(inputTypes))
		for field := range inputTypes {
			value, present := row[field]
			if !present {
				t.Fatalf("row %d omits physical field %q", rowIndex, field)
			}
			values[field] = value
		}
		snapshotRows[rowIndex] = ordinal.SnapshotRow{EntityKey: ordinalFixtureEntityKey(t, program.Sources[0], row), Values: values}
	}
	artifact, err := ordinal.CompileSnapshotArtifact(ordinal.SnapshotSpec{
		SourceID: "ordinal-test-source", SourceNamespace: program.Sources[0].SourceNamespace,
		Snapshot: program.Sources[0].Snapshot, SchemaDigest: strings.Repeat("1", 64), Fields: fields, Rows: snapshotRows,
	})
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact: %v", err)
	}
	return artifact
}

func ordinalFixtureEntityKey(t *testing.T, source queryplan.OrdinalSource, row map[string]any) string {
	t.Helper()
	components := []string{source.SourceNamespace}
	for _, binding := range source.EntityKey {
		value, present := row[binding.Column]
		if !present {
			t.Fatalf("entity row omits %q", binding.Column)
		}
		canonical, err := exposure.CanonicalSQLValue(binding.SQLType, value)
		if err != nil {
			t.Fatal(err)
		}
		components = append(components, binding.Column, binding.SQLType, canonical)
	}
	key, err := exposure.ComposeCanonicalKeyV2("base-entity", components...)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func ordinalProvenanceColumns(program queryplan.OrdinalProgram) ([]dataconnector.Column, map[string]int) {
	set := make(map[string]struct{})
	if program.Kind == "union_distinct" {
		set["tg_branch"] = struct{}{}
	}
	for _, source := range program.Sources {
		set[source.HandleAlias] = struct{}{}
		for _, binding := range source.EvidenceFields {
			set[binding.ProvenanceAlias] = struct{}{}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	columns := make([]dataconnector.Column, len(names))
	positions := make(map[string]int, len(names))
	for index, name := range names {
		columns[index] = dataconnector.Column{Name: name}
		positions[name] = index
	}
	return columns, positions
}

func (f ordinalDerivationFixture) indexes() map[string]ordinal.SnapshotIndex {
	result := make(map[string]ordinal.SnapshotIndex, len(f.program.Sources))
	for _, source := range f.program.Sources {
		result[source.SourceAlias] = f.artifact.Hot
	}
	return result
}

func (f ordinalDerivationFixture) newDeriver(t *testing.T, visible dataconnector.Result,
	indexes map[string]ordinal.SnapshotIndex) *ordinalDeriver {
	t.Helper()
	deriver, err := newOrdinalDeriver(f.program, indexes, visible, ordinalDerivationPlanDigest)
	if err != nil {
		t.Fatalf("newOrdinalDeriver: %v", err)
	}
	if err := deriver.Begin(context.Background(), f.columns); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return deriver
}

func (f ordinalDerivationFixture) derive(t *testing.T, visible dataconnector.Result,
	inputs []ordinalProvenanceInput) ordinalEffect {
	t.Helper()
	deriver := f.newDeriver(t, visible, f.indexes())
	for rowIndex, input := range inputs {
		if err := deriver.Row(context.Background(), f.provenanceValues(t, input)); err != nil {
			t.Fatalf("Row %d: %v", rowIndex, err)
		}
	}
	effect, err := deriver.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return effect
}

func (f ordinalDerivationFixture) provenanceValues(t *testing.T, input ordinalProvenanceInput) []any {
	t.Helper()
	if input.row < 0 || input.row >= len(f.rows) {
		t.Fatalf("invalid provenance row %d", input.row)
	}
	values := make([]any, len(f.columns))
	row := f.rows[input.row]
	handle := input.handle
	if handle == 0 {
		var found bool
		handle, found = f.artifact.Hot.LookupRowHandle(f.entityKeys[input.row])
		if !found {
			t.Fatalf("HOT index misses entity %q", f.entityKeys[input.row])
		}
	}
	if position, present := f.positions["tg_branch"]; present {
		values[position] = int64(input.branch)
	}
	for _, source := range f.program.Sources {
		values[f.positions[source.HandleAlias]] = uint64(handle)
		for _, binding := range source.EvidenceFields {
			value, present := row[binding.Column]
			if !present {
				t.Fatalf("row omits provenance field %q", binding.Column)
			}
			values[f.positions[binding.ProvenanceAlias]] = value
		}
	}
	return values
}

func (f ordinalDerivationFixture) oracleRelation(t *testing.T, source queryplan.OrdinalSource,
	rowIndexes []int, unionProjection bool) exposure.RelationV2 {
	t.Helper()
	fields := make([]exposure.FieldV2, 0, len(source.EvidenceFields))
	for _, binding := range source.EvidenceFields {
		fields = append(fields, exposure.FieldV2{ID: binding.FieldID, SQLType: binding.SQLType,
			Collation: binding.Collation, CollationVersion: binding.CollationVersion,
			CollationDeterministic: binding.Collation != ""})
	}
	rows := make([]exposure.BaseRowV2, 0, len(rowIndexes))
	for _, rowIndex := range rowIndexes {
		values := make(map[string]any, len(source.EvidenceFields))
		for _, binding := range source.EvidenceFields {
			values[binding.FieldID] = f.rows[rowIndex][binding.Column]
		}
		rows = append(rows, exposure.BaseRowV2{EntityKey: f.entityKeys[rowIndex], Values: values})
	}
	relation, err := exposure.ScanV2(exposure.BaseRelationSpecV2{SourceNamespace: source.SourceNamespace,
		Snapshot: source.Snapshot, StableRole: source.Role, Fields: fields, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	if len(source.LeafPredicates) != 0 {
		predicateFields := make([]string, 0, len(source.LeafPredicates))
		for _, predicate := range source.LeafPredicates {
			predicateFields = append(predicateFields, predicate.Field.FieldID)
		}
		relation, err = exposure.SelectV2(relation, predicateFields, func(exposure.AnnotatedRowV2) exposure.SQLTruth {
			return exposure.SQLTrue
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if unionProjection {
		project := make([]string, 0, len(source.UnionKeyFields))
		for _, field := range source.UnionKeyFields {
			project = append(project, field.FieldID)
		}
		relation, err = exposure.ProjectV2(relation, project...)
		if err != nil {
			t.Fatal(err)
		}
	}
	return relation
}

func scanVisibleResult(t *testing.T, program queryplan.OrdinalProgram, rows []map[string]any) dataconnector.Result {
	t.Helper()
	visibleRows := make([]map[string]any, len(rows))
	for rowIndex, row := range rows {
		visibleRows[rowIndex] = make(map[string]any)
		for _, output := range program.Visible {
			if output.Kind == "derived" {
				visibleRows[rowIndex][output.ResultAlias] = row[output.ResultAlias]
				continue
			}
			binding, present := ordinalBinding(program, output.FieldID)
			if !present {
				t.Fatalf("visible field %q has no binding", output.FieldID)
			}
			visibleRows[rowIndex][output.ResultAlias] = row[binding.Column]
		}
	}
	return ordinalVisibleResult(program, visibleRows)
}

func ordinalVisibleResult(program queryplan.OrdinalProgram, rows []map[string]any) dataconnector.Result {
	names := make([]string, 0, len(program.Groups)+len(program.Visible))
	seen := make(map[string]struct{})
	for _, group := range program.Groups {
		if _, present := seen[group.ResultAlias]; !present {
			seen[group.ResultAlias] = struct{}{}
			names = append(names, group.ResultAlias)
		}
	}
	for _, output := range program.Visible {
		if _, present := seen[output.ResultAlias]; !present {
			seen[output.ResultAlias] = struct{}{}
			names = append(names, output.ResultAlias)
		}
	}
	columns := make([]dataconnector.Column, len(names))
	for index, name := range names {
		columns[index] = dataconnector.Column{Name: name}
	}
	resultRows := make([][]any, len(rows))
	for rowIndex, row := range rows {
		resultRows[rowIndex] = make([]any, len(names))
		for columnIndex, name := range names {
			resultRows[rowIndex][columnIndex] = row[name]
		}
	}
	return dataconnector.Result{Columns: columns, Rows: resultRows, RowCount: int64(len(resultRows))}
}

func groupedVisibleValues(program queryplan.OrdinalProgram, groupValue, aggregateValue any) map[string]any {
	result := make(map[string]any)
	for _, group := range program.Groups {
		result[group.ResultAlias] = groupValue
	}
	for _, aggregate := range program.Aggregates {
		result[aggregate.ResultAlias] = aggregateValue
	}
	return result
}

func aggregateOracleRelation(t *testing.T, program queryplan.OrdinalProgram, base exposure.RelationV2,
	visibleRows []map[string]any) exposure.RelationV2 {
	t.Helper()
	groups := make([]string, 0, len(program.Groups))
	for _, group := range program.Groups {
		groups = append(groups, group.Field.FieldID)
	}
	specs := make([]exposure.AggregateSpecV2, 0, len(program.Aggregates))
	for _, aggregate := range program.Aggregates {
		field := "*"
		if aggregate.InputKind == "field" {
			field = aggregate.Input.FieldID
		}
		specs = append(specs, exposure.AggregateSpecV2{Function: aggregate.Function, Field: field,
			OutputID: aggregate.OutputID, OutputType: aggregate.SQLType})
	}
	outputs := make([]map[string]any, len(visibleRows))
	for rowIndex, visible := range visibleRows {
		output := make(map[string]any, len(groups)+len(specs))
		for _, group := range program.Groups {
			output[group.Field.FieldID] = visible[group.ResultAlias]
		}
		for _, aggregate := range program.Aggregates {
			output[aggregate.OutputID] = visible[aggregate.ResultAlias]
		}
		outputs[rowIndex] = output
	}
	result, err := exposure.AggregateFromResultsV2(base, groups, specs, outputs)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func ordinalOracleVisibleFields(program queryplan.OrdinalProgram) []string {
	result := make([]string, 0, len(program.Visible))
	for _, output := range program.Visible {
		if output.Kind == "field" {
			result = append(result, output.FieldID)
		} else {
			result = append(result, output.OutputID)
		}
	}
	return result
}

func assertOrdinalEffectEqualsOracle(t *testing.T, effect ordinalEffect, resolver ordinal.Resolver,
	oracle exposure.Observation, visible dataconnector.Result, planDigest string) {
	t.Helper()
	release, err := ordinal.Decode(effect.Release, resolver)
	if err != nil {
		t.Fatalf("decode release: %v", err)
	}
	for _, fact := range effect.DerivedRelease {
		if err := release.Add(fact); err != nil {
			t.Fatalf("add dynamic release: %v", err)
		}
	}
	influence, err := ordinal.Decode(effect.Influence, resolver)
	if err != nil {
		t.Fatalf("decode influence: %v", err)
	}
	wantRelease, _ := exposure.NewFactSet(oracle.Release...)
	wantInfluence, _ := exposure.NewFactSet(oracle.Influence...)
	assertFactSetsEqual(t, "release", release, wantRelease)
	assertFactSetsEqual(t, "influence", influence, wantInfluence)
	outcomeDigest, err := exposure.ReleaseOutcomeDigest(oracle.Release, visible.RowCount)
	if err != nil {
		t.Fatal(err)
	}
	wantOutcome, err := exposure.NewOutcomeFactV3(queryplan.NormalFormVersion, planDigest, outcomeDigest, visible.RowCount)
	if err != nil {
		t.Fatal(err)
	}
	if len(effect.Outcome) != 1 {
		t.Fatalf("outcome cardinality = %d, want 1", len(effect.Outcome))
	}
	assertFactsEqual(t, "outcome", effect.Outcome[0], wantOutcome)
}

func assertFactSetsEqual(t *testing.T, label string, got, want exposure.FactSet) {
	t.Helper()
	if got.Len() != want.Len() {
		t.Fatalf("%s cardinality = %d, want %d\ngot=%v\nwant=%v", label, got.Len(), want.Len(), factHashes(got), factHashes(want))
	}
	if err := want.Range(func(hash [32]byte, wantFact exposure.FactID) error {
		gotFact, present, err := got.Contains(hash)
		if err != nil {
			return err
		}
		if !present {
			t.Fatalf("%s misses FactHash %s", label, hex.EncodeToString(hash[:]))
		}
		assertFactsEqual(t, label+"/"+hex.EncodeToString(hash[:]), gotFact, wantFact)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertFactsEqual(t *testing.T, label string, got, want exposure.FactID) {
	t.Helper()
	gotHash, gotErr := got.Hash()
	wantHash, wantErr := want.Hash()
	gotPayload, gotPayloadErr := got.CanonicalPayload()
	wantPayload, wantPayloadErr := want.CanonicalPayload()
	if gotErr != nil || wantErr != nil || gotPayloadErr != nil || wantPayloadErr != nil ||
		gotHash != wantHash || !bytes.Equal(gotPayload, wantPayload) {
		t.Fatalf("%s facts differ\ngot=%#v hash=%s errors=%v/%v\nwant=%#v hash=%s errors=%v/%v",
			label, got, gotHash, gotErr, gotPayloadErr, want, wantHash, wantErr, wantPayloadErr)
	}
}

func factHashes(set exposure.FactSet) []string {
	result := make([]string, 0, set.Len())
	if err := set.Range(func(hash [32]byte, _ exposure.FactID) error {
		result = append(result, hex.EncodeToString(hash[:]))
		return nil
	}); err != nil {
		return []string{"ERROR: " + err.Error()}
	}
	sort.Strings(result)
	return result
}

func ordinalSourceForBranch(t *testing.T, program queryplan.OrdinalProgram, branch int) queryplan.OrdinalSource {
	t.Helper()
	for _, source := range program.Sources {
		if source.Branch == branch {
			return source
		}
	}
	t.Fatalf("program has no branch %d", branch)
	return queryplan.OrdinalSource{}
}

func ordinalSourceForAlias(t *testing.T, program queryplan.OrdinalProgram, alias string) queryplan.OrdinalSource {
	t.Helper()
	for _, source := range program.Sources {
		if source.SourceAlias == alias {
			return source
		}
	}
	t.Fatalf("program has no source alias %q", alias)
	return queryplan.OrdinalSource{}
}

func cloneRawRows(rows []map[string]any) []map[string]any {
	result := make([]map[string]any, len(rows))
	for rowIndex, row := range rows {
		result[rowIndex] = make(map[string]any, len(row))
		for field, value := range row {
			result[rowIndex][field] = value
		}
	}
	return result
}

func compactOrdinalProduct() queryplan.Product {
	return queryplan.Product{
		Name: "expenses", Columns: map[string]struct{}{"id": {}, "department": {}, "amount": {}},
		AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}},
		ColumnTypes:       map[string]string{"id": "bigint", "department": "text", "amount": "numeric"},
		ColumnCollations:  map[string]string{"department": "C"},
		CollationVersions: map[string]string{"department": "builtin"},
		SourceNamespace:   "travel.expense", Snapshot: "snapshot-v1", StableRole: "expense", StableEntityKey: []string{"id"},
		SnapshotPublication: "expense-publication-v1", SidecarManifestDigest: strings.Repeat("b", 64),
	}
}

func compactUnionProduct() queryplan.Product {
	return queryplan.Product{
		Name: "summary", Columns: map[string]struct{}{"id": {}, "department": {}, "month": {}},
		AllowedAggregates: map[string]struct{}{"count": {}},
		ColumnTypes:       map[string]string{"id": "bigint", "department": "text", "month": "text"},
		ColumnCollations:  map[string]string{"department": "C", "month": "C"},
		CollationVersions: map[string]string{"department": "builtin", "month": "builtin"},
		SourceNamespace:   "travel.summary", Snapshot: "snapshot-v1", StableRole: "summary", StableEntityKey: []string{"id"},
		SnapshotPublication: "summary-publication-v1", SidecarManifestDigest: strings.Repeat("c", 64),
	}
}

// derivationError returns the failure the deriver reports for one provenance
// row: at Row on the row-at-a-time paths, or at Finish once the pipelined
// path prepares and commits its batch. Either way no effect is produced.
func derivationError(deriver *ordinalDeriver, values []any) error {
	if err := deriver.Row(context.Background(), values); err != nil {
		return err
	}
	_, err := deriver.Finish()
	return err
}
