package gateway

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// TestProfileOrdinalDerivationAtScale drives the ordinal deriver over synthetic
// snapshots of the campaign's two heavy shapes without a database, so the
// Go-side derivation can be profiled (go test -run TestProfileOrdinalDerivationAtScale
// -cpuprofile cpu.out). It runs only when TASKGATE_PROFILE_DERIVATION is set;
// TASKGATE_PROFILE_ROWS overrides the row count (default 100000).
func TestProfileOrdinalDerivationAtScale(t *testing.T) {
	if os.Getenv("TASKGATE_PROFILE_DERIVATION") == "" {
		t.Skip("TASKGATE_PROFILE_DERIVATION is not set")
	}
	rowCount := 100000
	if value := os.Getenv("TASKGATE_PROFILE_ROWS"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
		rowCount = parsed
	}
	t.Run("scan-16-columns", func(t *testing.T) {
		product := wideOrdinalProduct(16)
		columns := make([]string, 0, 16)
		for index := 0; index < 16; index++ {
			columns = append(columns, fmt.Sprintf("c%02d", index))
		}
		compiled, err := queryplan.CompileOrdinal(queryplan.QueryPlan{Product: product.Name, Columns: columns}, product)
		if err != nil {
			t.Fatal(err)
		}
		rows := wideOrdinalRows(rowCount, 16)
		fixture := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, rows)
		visible := scanVisibleResult(t, fixture.program, rows)
		inputs := make([]ordinalProvenanceInput, rowCount)
		for index := range inputs {
			inputs[index] = ordinalProvenanceInput{row: index, branch: -1}
		}
		started := time.Now()
		effect := fixture.derive(t, visible, inputs)
		t.Logf("scan rows=%d release=%d influence=%d derive=%s", rowCount, effect.Release.Cardinality(), effect.Influence.Cardinality(), time.Since(started))
	})
	t.Run("join-grouped-sum-count", func(t *testing.T) {
		// The campaign's S2 shape: two sources joined on a key column, grouped
		// by the left key with a sum and a count, provenance rows pairing one
		// left row with one right row.
		left := wideOrdinalProduct(4)
		left.Name, left.StableRole = "left_product", "left"
		right := wideOrdinalProduct(4)
		right.Name, right.StableRole = "right_product", "right"
		plan := queryplan.QueryPlan{
			From: &queryplan.From{Join: &queryplan.Join{
				Left:  queryplan.Scan{Product: left.Name, Role: "left"},
				Right: queryplan.Scan{Product: right.Name, Role: "right"},
				On:    []queryplan.JoinPredicate{{Left: "left.c00", Right: "right.c00"}},
			}},
			Columns:    []string{"left.c00"},
			GroupBy:    []string{"left.c00"},
			Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "right.c01", Alias: "total"}, {Function: "count", Column: "*", Alias: "items"}},
		}
		compiled, err := queryplan.CompileRelational(plan, map[string]queryplan.Product{left.Name: left, right.Name: right})
		if err != nil {
			t.Fatal(err)
		}
		rows := wideOrdinalRows(rowCount, 4)
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i]["c00"].(string) != rows[j]["c00"].(string) {
				return rows[i]["c00"].(string) < rows[j]["c00"].(string)
			}
			return rows[i]["id"].(int64) < rows[j]["id"].(int64)
		})
		fixture := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, rows)
		totals, counts, order := map[string]int64{}, map[string]int64{}, []string{}
		for _, row := range rows {
			key := row["c00"].(string)
			if _, seen := counts[key]; !seen {
				order = append(order, key)
			}
			totals[key] += row["c01"].(int64)
			counts[key]++
		}
		visibleMaps := make([]map[string]any, 0, len(order))
		for _, key := range order {
			values := map[string]any{}
			for _, output := range fixture.program.Visible {
				switch {
				case output.Kind == "field":
					values[output.ResultAlias] = key
				case output.ResultAlias == "total":
					values[output.ResultAlias] = strconv.FormatInt(totals[key], 10)
				default:
					values[output.ResultAlias] = counts[key]
				}
			}
			visibleMaps = append(visibleMaps, values)
		}
		visible := ordinalVisibleResult(fixture.program, visibleMaps)
		inputs := make([]ordinalProvenanceInput, rowCount)
		for index := range inputs {
			inputs[index] = ordinalProvenanceInput{row: index, branch: -1}
		}
		started := time.Now()
		effect := fixture.derive(t, visible, inputs)
		t.Logf("join-grouped rows=%d groups=%d release=%d influence=%d derive=%s", rowCount, len(order), effect.Release.Cardinality(), effect.Influence.Cardinality(), time.Since(started))
	})
	t.Run("grouped-sum-count", func(t *testing.T) {
		product := wideOrdinalProduct(4)
		compiled, err := queryplan.CompileOrdinal(queryplan.QueryPlan{Product: product.Name, Columns: []string{"c00"},
			Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "c01", Alias: "total"}, {Function: "count", Column: "*", Alias: "items"}},
			GroupBy:    []string{"c00"}}, product)
		if err != nil {
			t.Fatal(err)
		}
		rows := wideOrdinalRows(rowCount, 4)
		// The provenance stream must present groups contiguously in canonical
		// key order, as the companion statement's ORDER BY guarantees.
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i]["c00"].(string) != rows[j]["c00"].(string) {
				return rows[i]["c00"].(string) < rows[j]["c00"].(string)
			}
			return rows[i]["id"].(int64) < rows[j]["id"].(int64)
		})
		fixture := newOrdinalDerivationFixture(t, compiled.OrdinalProgram, rows)
		groups := map[string]*struct {
			total int64
			items int64
		}{}
		order := []string{}
		for _, row := range rows {
			key := row["c00"].(string)
			if groups[key] == nil {
				groups[key] = &struct {
					total int64
					items int64
				}{}
				order = append(order, key)
			}
			groups[key].total += row["c01"].(int64)
			groups[key].items++
		}
		visibleRows := make([]map[string]any, 0, len(order))
		for _, key := range order {
			visibleRows = append(visibleRows, map[string]any{"c00": key, "total": strconv.FormatInt(groups[key].total, 10), "items": groups[key].items})
		}
		visible := ordinalVisibleResult(fixture.program, visibleRows)
		inputs := make([]ordinalProvenanceInput, rowCount)
		for index := range inputs {
			inputs[index] = ordinalProvenanceInput{row: index, branch: -1}
		}
		started := time.Now()
		effect := fixture.derive(t, visible, inputs)
		t.Logf("grouped rows=%d groups=%d release=%d influence=%d derive=%s", rowCount, len(order), effect.Release.Cardinality(), effect.Influence.Cardinality(), time.Since(started))
	})
}

func wideOrdinalProduct(width int) queryplan.Product {
	columns := map[string]struct{}{"id": {}}
	types := map[string]string{"id": "bigint"}
	collations := map[string]string{}
	versions := map[string]string{}
	for index := 0; index < width; index++ {
		name := fmt.Sprintf("c%02d", index)
		columns[name] = struct{}{}
		if index == 0 {
			types[name] = "text"
			collations[name] = "C"
			versions[name] = "builtin"
		} else {
			types[name] = "bigint"
		}
	}
	return queryplan.Product{
		Name: "wide", Columns: columns, AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}},
		ColumnTypes: types, ColumnCollations: collations, CollationVersions: versions,
		SourceNamespace: "travel.wide", Snapshot: "snapshot-v1", StableRole: "wide", StableEntityKey: []string{"id"},
		SnapshotPublication: "wide-publication-v1", SidecarManifestDigest: strings.Repeat("e", 64),
	}
}

func wideOrdinalRows(count, width int) []map[string]any {
	rows := make([]map[string]any, count)
	for index := 0; index < count; index++ {
		row := map[string]any{"id": int64(index + 1)}
		for column := 0; column < width; column++ {
			name := fmt.Sprintf("c%02d", column)
			if column == 0 {
				row[name] = fmt.Sprintf("group-%d", index%3)
			} else {
				row[name] = int64((index*7 + column*13) % 1000)
			}
		}
		rows[index] = row
	}
	return rows
}
