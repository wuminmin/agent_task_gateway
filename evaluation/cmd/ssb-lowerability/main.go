// Command ssb-lowerability lowers the 13 Star Schema Benchmark queries (a
// reporting/BI star-schema workload) through the production reporting-SQL
// lowerer against a synthetic five-table Catalog and
// records, per query, whether it enters the closed fragment as written and
// otherwise the first rejection the lowerer reports.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
)

type result struct {
	Query     string `json:"query"`
	SQLSHA256 string `json:"sql_sha256"`
	Lowerable bool   `json:"lowerable"`
	Code      string `json:"code,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Clause    string `json:"clause,omitempty"`
	Message   string `json:"message,omitempty"`
	Sources   int    `json:"from_sources"`
}

type pass struct {
	Variant   string         `json:"variant"`
	Directory string         `json:"directory"`
	Queries   int            `json:"queries"`
	Lowerable int            `json:"lowerable"`
	ByReason  map[string]int `json:"by_reason"`
	Results   []result       `json:"results"`
}

type report struct {
	Version string `json:"version"`
	Profile string `json:"lowering_profile"`
	Passes  []pass `json:"passes"`
}

func product(name string, key string, columns map[string]string) queryplan.Product {
	cols := map[string]struct{}{}
	collations := map[string]string{}
	versions := map[string]string{}
	for column, typ := range columns {
		cols[column] = struct{}{}
		if typ == "text" {
			collations[column] = "C"
			versions[column] = "builtin"
		}
	}
	return queryplan.Product{
		Name: name, StableRole: name, SourceNamespace: "ssb." + name, Snapshot: "ssb-sf1",
		StableEntityKey: []string{key}, Columns: cols, ColumnTypes: columns,
		ColumnCollations: collations, CollationVersions: versions,
		AllowedAggregates: map[string]struct{}{"count": {}, "sum": {}, "min": {}, "max": {}},
	}
}

func products() map[string]queryplan.Product {
	return map[string]queryplan.Product{
		"lineorder": product("lineorder", "lo_orderkey", map[string]string{"lo_orderkey": "integer", "lo_linenumber": "integer", "lo_custkey": "integer", "lo_partkey": "integer", "lo_suppkey": "integer", "lo_orderdate": "integer", "lo_orderpriority": "text", "lo_shippriority": "text", "lo_quantity": "integer", "lo_extendedprice": "numeric", "lo_ordtotalprice": "numeric", "lo_discount": "integer", "lo_revenue": "numeric", "lo_supplycost": "numeric", "lo_tax": "integer", "lo_commitdate": "integer", "lo_shipmode": "text"}),
		"customer":  product("customer", "c_custkey", map[string]string{"c_custkey": "integer", "c_name": "text", "c_address": "text", "c_city": "text", "c_nation": "text", "c_region": "text", "c_phone": "text", "c_mktsegment": "text"}),
		"supplier":  product("supplier", "s_suppkey", map[string]string{"s_suppkey": "integer", "s_name": "text", "s_address": "text", "s_city": "text", "s_nation": "text", "s_region": "text", "s_phone": "text"}),
		"part":      product("part", "p_partkey", map[string]string{"p_partkey": "integer", "p_name": "text", "p_mfgr": "text", "p_category": "text", "p_brand1": "text", "p_color": "text", "p_type": "text", "p_size": "integer", "p_container": "text"}),
		"dwdate":    product("dwdate", "d_datekey", map[string]string{"d_datekey": "integer", "d_date": "text", "d_dayofweek": "text", "d_month": "text", "d_year": "integer", "d_yearmonthnum": "integer", "d_yearmonth": "text", "d_daynuminweek": "integer", "d_daynuminmonth": "integer", "d_daynuminyear": "integer", "d_monthnuminyear": "integer", "d_weeknuminyear": "integer", "d_sellingseason": "text", "d_lastdayinweekfl": "integer", "d_lastdayinmonthfl": "integer", "d_holidayfl": "integer", "d_weekdayfl": "integer"}),
	}
}

func runPass(variant, directory string) (pass, error) {
	files, err := filepath.Glob(filepath.Join(directory, "q*.sql"))
	if err != nil || len(files) == 0 {
		return pass{}, fmt.Errorf("no query files found in %s", directory)
	}
	sort.Strings(files)
	rep := pass{Variant: variant, Directory: directory, ByReason: map[string]int{}}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return pass{}, err
		}
		sum := sha256.Sum256(raw)
		name := strings.TrimSuffix(filepath.Base(file), ".sql")
		item := result{Query: name, SQLSHA256: hex.EncodeToString(sum[:]), Sources: strings.Count(strings.ToLower(string(raw)), "\nfrom ")}
		if _, lowerErr := sqllowering.Lower(string(raw), products()); lowerErr == nil {
			item.Lowerable = true
			rep.Lowerable++
		} else {
			var typed *sqllowering.Error
			if errors.As(lowerErr, &typed) {
				item.Code, item.Reason, item.Clause, item.Message = typed.Code, typed.Reason, typed.Location.Clause, typed.Message
			} else {
				item.Code, item.Message = "UNTYPED_ERROR", lowerErr.Error()
			}
			key := item.Code
			if item.Reason != "" {
				key = item.Code + "/" + item.Reason
			}
			rep.ByReason[key]++
		}
		rep.Results = append(rep.Results, item)
	}
	rep.Queries = len(rep.Results)
	return rep, nil
}

func main() {
	root := flag.String("root", "evaluation/ssblowerability", "directory holding queries/ and queries-explicit-join/")
	out := flag.String("out", "evaluation/ssblowerability/results.json", "output path")
	flag.Parse()
	rep := report{Version: "taskgate-ssb-lowerability-v1", Profile: sqllowering.Profile}
	for _, spec := range []struct{ variant, dir string }{
		{"as-written", filepath.Join(*root, "queries")},
		{"explicit-join", filepath.Join(*root, "queries-explicit-join")},
	} {
		p, err := runPass(spec.variant, spec.dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		rep.Passes = append(rep.Passes, p)
	}
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(encoded, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, p := range rep.Passes {
		fmt.Printf("%s: %d/%d SSB queries lower\n", p.Variant, p.Lowerable, p.Queries)
	}
	fmt.Printf("written %s\n", *out)
}
