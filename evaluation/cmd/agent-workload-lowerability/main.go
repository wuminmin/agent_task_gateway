// Command agent-workload-lowerability lowers SQL written by an off-the-shelf
// text-to-SQL assistant (given only the campaign Products as
// describe_data_product exposes them and 40 natural-language questions)
// through the production reporting-SQL lowerer and
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

func product(name string, keys []string, columns map[string]string, aggregates ...string) queryplan.Product {
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
	allowed := map[string]struct{}{}
	for _, aggregate := range aggregates {
		allowed[aggregate] = struct{}{}
	}
	return queryplan.Product{
		Name: name, StableRole: name, SourceNamespace: "campaign." + name, Snapshot: "campaign-frozen",
		StableEntityKey: keys, Columns: cols, ColumnTypes: columns,
		ColumnCollations: collations, CollationVersions: versions,
		AllowedAggregates: allowed,
	}
}

// products mirrors the campaign Catalog's Products as describe_data_product
// exposes them (config/catalog.yaml): fields, types, entity keys, and the
// approved aggregates.
func products() map[string]queryplan.Product {
	return map[string]queryplan.Product{
		"expense_detail":        product("expense_detail", []string{"receipt_no"}, map[string]string{"receipt_no": "text", "employee_no": "text", "employee_name": "text", "department": "text", "expense_date": "date", "expense_type": "text", "amount": "numeric", "city": "text", "purpose": "text", "status": "text"}, "sum", "count", "min", "max", "avg"),
		"expense_summary":       product("expense_summary", []string{"month", "department", "expense_type"}, map[string]string{"month": "text", "department": "text", "expense_type": "text", "total_amount": "numeric", "request_count": "bigint"}, "sum", "count", "min", "max", "avg"),
		"provsql_orders":        product("provsql_orders", []string{"orderkey"}, map[string]string{"orderkey": "bigint", "status": "bigint", "partition_key": "integer"}, "sum", "count", "avg"),
		"provsql_lineitem":      product("provsql_lineitem", []string{"orderkey", "linenumber"}, map[string]string{"orderkey": "bigint", "linenumber": "integer", "extendedprice": "numeric", "partition_key": "integer"}, "sum", "count", "avg"),
		"final_v5_result_heavy": product("final_v5_result_heavy", []string{"row_id"}, map[string]string{"row_id": "bigint", "category": "text", "amount": "numeric", "event_date": "date", "sequence_no": "integer", "approved": "boolean", "event_timestamp": "timestamp", "description": "text", "quantity": "bigint", "unit_price": "numeric", "tax_amount": "numeric", "settled_date": "date", "processed_at": "timestamp", "region": "text", "revision": "integer", "active": "boolean"}),
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
	root := flag.String("root", "evaluation/agentworkload", "directory holding queries/")
	out := flag.String("out", "evaluation/agentworkload/results.json", "output path")
	flag.Parse()
	rep := report{Version: "taskgate-agent-workload-lowerability-v1", Profile: sqllowering.Profile}
	for _, spec := range []struct{ variant, dir string }{
		{"as-written", filepath.Join(*root, "queries")},
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
		fmt.Printf("%s: %d/%d agent-written queries lower\n", p.Variant, p.Lowerable, p.Queries)
	}
	fmt.Printf("written %s\n", *out)
}
