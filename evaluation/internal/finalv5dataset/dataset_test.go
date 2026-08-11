package finalv5dataset

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

func TestProductDefinitionsAreTheExactFiveReviewedStreams(t *testing.T) {
	definitions, err := ProductDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		productID string
		query     string
		oids      []uint32
		keys      []bool
	}{
		{finalv5oracle.ProvSQLOrdersProductID,
			"SELECT orderkey, status, partition_key\nFROM reporting.provsql_orders\nORDER BY orderkey",
			[]uint32{20, 20, 23}, []bool{true, false, false}},
		{finalv5oracle.ProvSQLLineitemProductID,
			"SELECT orderkey, linenumber, extendedprice, partition_key\nFROM reporting.provsql_lineitem\nORDER BY orderkey, linenumber",
			[]uint32{20, 23, 1700, 23}, []bool{true, true, false, false}},
		{finalv5oracle.ProvSQLNonceProductID,
			"SELECT nonce_id, partition_key\nFROM reporting.provsql_nonce\nORDER BY nonce_id",
			[]uint32{20, 23}, []bool{true, false}},
		{finalv5oracle.ExposureScaleProductID,
			"SELECT member_rank, metric, family_id, partition_key\nFROM reporting.final_v5_exposure_scale\nORDER BY member_rank",
			[]uint32{20, 1700, 23, 23}, []bool{true, false, false, false}},
		{resultHeavyProductID,
			"SELECT row_id, category, amount, event_date, sequence_no, approved,\n" +
				"       event_timestamp, description, quantity, unit_price, tax_amount,\n" +
				"       settled_date, processed_at, region, revision, active\n" +
				"FROM reporting.final_v5_result_heavy\nORDER BY row_id",
			[]uint32{20, 25, 1700, 1082, 23, 16, 1114, 25, 20, 1700, 1700, 1082, 1114, 25, 23, 16},
			[]bool{true, false, false, false, false, false, false, false, false, false, false, false, false, false, false, false}},
	}
	if len(definitions) != len(want) {
		t.Fatalf("Product definitions = %d, want %d", len(definitions), len(want))
	}
	logical := finalv5oracle.BenchmarkDatasetProductSpecs()
	for index, expected := range want {
		definition := definitions[index]
		if definition.ProductID != expected.productID || definition.Relation != "reporting."+expected.productID ||
			definition.RelationKind != "m" || definition.Query != expected.query ||
			len(definition.Columns) != len(expected.oids) || logical[index].ProductID != expected.productID {
			t.Fatalf("Product definition %d = %+v", index+1, definition)
		}
		for columnIndex, column := range definition.Columns {
			if column.PostgreSQLOID != expected.oids[columnIndex] || column.StableKey != expected.keys[columnIndex] ||
				column.Name != logical[index].Fields[columnIndex].Name ||
				column.SQLType != logical[index].Fields[columnIndex].SQLType {
				t.Fatalf("Product %s column %d = %+v", definition.ProductID, columnIndex+1, column)
			}
		}
	}
	for _, columnIndex := range []int{1, 7, 13} {
		column := definitions[4].Columns[columnIndex]
		if column.CollationName != "en_US.utf8" || column.CollationVersion != "2.36" ||
			column.CollationActualVersion != "2.36" {
			t.Fatalf("result-heavy text column %d = %+v", columnIndex+1, column)
		}
	}
}

func TestProductDefinitionsAreDetachedAndClosed(t *testing.T) {
	definitions, err := ProductDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	definitions[0].Query = "SELECT changed"
	definitions[0].Columns[0].Name = "changed"
	again, err := ProductDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Query != provSQLOrdersQuery || again[0].Columns[0].Name != "orderkey" {
		t.Fatal("caller mutation changed the fixed Product transport")
	}
	if _, err := ProductDefinitionFor("unknown"); err == nil {
		t.Fatal("unknown Product acquired a Dataset transport")
	}
}

func TestSharedTransportHasNoPreparationOrProductionDependency(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate shared Dataset transport")
	}
	directory := filepath.Dir(source)
	root := filepath.Clean(filepath.Join(directory, "..", "..", ".."))
	command := exec.Command("go", "list", "-deps", "./evaluation/internal/finalv5dataset")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list shared Dataset transport dependencies: %v", err)
	}
	for _, dependency := range strings.Fields(string(output)) {
		for _, forbidden := range []string{
			"taskbound.local/agent-data-gateway/evaluation/internal/experiment",
			"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding",
			"taskbound.local/agent-data-gateway/internal/exposure",
			"taskbound.local/agent-data-gateway/internal/control",
			"taskbound.local/agent-data-gateway/internal/gateway",
			"taskbound.local/agent-data-gateway/internal/physicalquery",
			"taskbound.local/agent-data-gateway/internal/preparedbinding",
			"taskbound.local/agent-data-gateway/internal/queryplan",
			"taskbound.local/agent-data-gateway/internal/sqlpolicy",
			"taskbound.local/agent-data-gateway/internal/sqlidentity",
			"taskbound.local/agent-data-gateway/internal/sqllowering",
			"github.com/pganalyze/pg_query_go",
		} {
			if dependency == forbidden || strings.HasPrefix(dependency, forbidden+"/") {
				t.Fatalf("shared Dataset transport imports forbidden preparation/production dependency %q", dependency)
			}
		}
	}

	value, err := os.ReadFile(filepath.Join(directory, "dataset.go"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), "dataset.go", value, 0)
	if err != nil {
		t.Fatal(err)
	}
	queryCalls, simpleProtocolCalls := 0, 0
	beginTxCalls, readOnlyRepeatableReadCalls := 0, 0
	ast.Inspect(parsed, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok {
			switch selector.Sel.Name {
			case "Prepare", "PrepareContext", "Derive":
				t.Errorf("shared Dataset transport calls forbidden API %s", selector.Sel.Name)
			}
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok = call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if selector.Sel.Name == "BeginTx" {
			beginTxCalls++
			if len(call.Args) != 2 {
				return true
			}
			options, ok := call.Args[1].(*ast.CompositeLit)
			if !ok {
				return true
			}
			isolation, readOnly := false, false
			for _, element := range options.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				name, nameOK := field.Key.(*ast.Ident)
				value, valueOK := field.Value.(*ast.SelectorExpr)
				if !nameOK || !valueOK {
					continue
				}
				packageName, packageOK := value.X.(*ast.Ident)
				if !packageOK || packageName.Name != "pgx" {
					continue
				}
				isolation = isolation || (name.Name == "IsoLevel" && value.Sel.Name == "RepeatableRead")
				readOnly = readOnly || (name.Name == "AccessMode" && value.Sel.Name == "ReadOnly")
			}
			if isolation && readOnly {
				readOnlyRepeatableReadCalls++
			}
			return true
		}
		if selector.Sel.Name != "Query" && selector.Sel.Name != "QueryRow" {
			return true
		}
		queryCalls++
		if len(call.Args) == 0 {
			return true
		}
		mode, ok := call.Args[len(call.Args)-1].(*ast.SelectorExpr)
		packageName, packageOK := mode.X.(*ast.Ident)
		if ok && packageOK && packageName.Name == "pgx" && mode.Sel.Name == "QueryExecModeSimpleProtocol" {
			simpleProtocolCalls++
		}
		return true
	})
	if queryCalls != 3 || simpleProtocolCalls != queryCalls {
		t.Fatalf("shared Dataset transport has %d query call sites / %d explicit simple-protocol sites; want 3/3",
			queryCalls, simpleProtocolCalls)
	}
	if beginTxCalls != 1 || readOnlyRepeatableReadCalls != 1 {
		t.Fatalf("shared Dataset transport has %d transaction starts / %d read-only repeatable-read starts; want 1/1",
			beginTxCalls, readOnlyRepeatableReadCalls)
	}
}

func TestBenchmarkAgreementShapeIsCredentialFreeAndDeterministic(t *testing.T) {
	definitions, err := ProductDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	products := make([]PostgreSQLProduct, len(definitions))
	for index := range definitions {
		products[index] = definitions[index].PostgreSQLProduct
	}
	if !reflect.DeepEqual(products[0].Columns, productDefinitions[0].Columns) {
		t.Fatal("detached evidence shape differs from its reviewed definition")
	}
	for _, product := range products {
		encoded := strings.ToLower(product.Relation + product.ProductID)
		for _, forbidden := range []string{"postgres://", "password", "dsn", "select "} {
			if strings.Contains(encoded, forbidden) {
				t.Fatalf("credential-free Product shape contains %q", forbidden)
			}
		}
	}
}

func TestValidateBenchmarkAgreementIsTheStrictPureAcceptanceRule(t *testing.T) {
	valid := func(t *testing.T) BenchmarkAgreement {
		t.Helper()
		definitions, err := ProductDefinitions()
		if err != nil {
			t.Fatal(err)
		}
		products := make([]PostgreSQLProduct, len(definitions))
		for index, definition := range definitions {
			products[index] = definition.PostgreSQLProduct
		}
		summary, err := finalv5oracle.BenchmarkDatasetFingerprint()
		if err != nil {
			t.Fatal(err)
		}
		return BenchmarkAgreement{
			Version: BenchmarkAgreementVersion, Products: products,
			Reference: summary, Observed: summary, PreparedStatementCount: 0, Agreed: true,
		}
	}
	agreement := valid(t)
	before := valid(t)
	if err := ValidateBenchmarkAgreement(agreement); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(agreement, before) {
		t.Fatal("benchmark Dataset agreement validation mutated its input")
	}

	tests := []struct {
		name   string
		mutate func(*BenchmarkAgreement)
	}{
		{name: "version", mutate: func(value *BenchmarkAgreement) { value.Version = "changed" }},
		{name: "missing Product", mutate: func(value *BenchmarkAgreement) { value.Products = value.Products[:4] }},
		{name: "relation kind", mutate: func(value *BenchmarkAgreement) { value.Products[0].RelationKind = "r" }},
		{name: "column OID", mutate: func(value *BenchmarkAgreement) { value.Products[1].Columns[2].PostgreSQLOID = 25 }},
		{name: "collation actual version", mutate: func(value *BenchmarkAgreement) {
			value.Products[4].Columns[1].CollationActualVersion = "changed"
		}},
		{name: "reference rows", mutate: func(value *BenchmarkAgreement) { value.Reference.RowCount = 814_999 }},
		{name: "reference SHA", mutate: func(value *BenchmarkAgreement) { value.Reference.SHA256 = strings.Repeat("0", 64) }},
		{name: "probe Product SHA", mutate: func(value *BenchmarkAgreement) {
			value.Observed.Products[0].SHA256 = strings.Repeat("0", 64)
		}},
		{name: "not agreed", mutate: func(value *BenchmarkAgreement) { value.Agreed = false }},
		{name: "prepared statement", mutate: func(value *BenchmarkAgreement) { value.PreparedStatementCount = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := valid(t)
			test.mutate(&changed)
			if err := ValidateBenchmarkAgreement(changed); err == nil {
				t.Fatal("strict benchmark Dataset agreement validator accepted a mutation")
			}
		})
	}
}
