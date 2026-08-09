package physicalquery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// compilerV6CompatibilityBytes deliberately excludes CompilerIdentitySHA256:
// v7 names the extension-capable compiler. Every other member is frozen from
// the v6 implementation at 91fc5d9 and must stay byte identical for plans
// without relational ORDER BY. The compiler rejects result encoding without
// that ORDER shape, so there is no second extension excluded from this gate.
type compilerV6CompatibilityBytes struct {
	VisibleSQLSHA256      string `json:"visible_sql_sha256"`
	ProvenanceSQLSHA256   string `json:"provenance_sql_sha256"`
	SemanticBytesSHA256   string `json:"semantic_bytes_sha256"`
	SemanticSHA256        string `json:"semantic_sha256"`
	OrdinalBytesSHA256    string `json:"ordinal_bytes_sha256"`
	OrdinalSHA256         string `json:"ordinal_sha256"`
	PlanBytesSHA256       string `json:"plan_bytes_sha256"`
	PreparationPlanSHA256 string `json:"preparation_plan_sha256"`
}

func TestCompilerV7PreservesV6BytesForPlansWithoutRelationalOrder(t *testing.T) {
	if queryplan.OrdinalProgramVersion != "taskgate-ordinal-program-v1" {
		t.Fatalf("presentation-only compiler extension moved the ordinal program version to %q", queryplan.OrdinalProgramVersion)
	}
	products := compilerV6CompatibilityProducts()
	encodedWithoutOrder := queryplan.QueryPlan{From: &queryplan.From{Join: &queryplan.Join{
		Left:  queryplan.Scan{Product: "compat_detail", Role: "compat_detail"},
		Right: queryplan.Scan{Product: "compat_summary", Role: "compat_summary"},
		On:    []queryplan.JoinPredicate{{Left: "compat_detail.department", Right: "compat_summary.department"}},
	}}, Columns: []string{"compat_summary.department"},
		Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "compat_detail.amount", Alias: "total",
			ResultEncoding: queryplan.NumericTextResultEncoding}},
		GroupBy: []string{"compat_summary.department"}}
	if _, err := queryplan.CompileRelational(encodedWithoutOrder, products); err == nil {
		t.Fatal("compiler accepted result encoding on a plan without relational ORDER BY")
	}
	tests := []struct {
		name     string
		plan     queryplan.QueryPlan
		single   *queryplan.Product
		products map[string]queryplan.Product
		want     compilerV6CompatibilityBytes
	}{
		{name: "single", plan: queryplan.QueryPlan{Product: "compat_expense", Columns: []string{"id", "amount"},
			Filters: []queryplan.Filter{{Column: "department", Op: "=", Value: "sales"}}}, single: compatibilityProductPointer(products["compat_expense"]),
			want: compilerV6CompatibilityBytes{
				VisibleSQLSHA256: "c6187d1ee18e2c95541a94d548fffa6ee25a24ba70544800b2a76d2c45de3135", ProvenanceSQLSHA256: "c6187d1ee18e2c95541a94d548fffa6ee25a24ba70544800b2a76d2c45de3135",
				SemanticBytesSHA256: "fc1a7f21603e5ba24bf8ddefefa90b8513eb3cbed692614dfe1e01debccff7be", SemanticSHA256: "c8e62d550a1a743cbe5bc0dc42eb9c021b13265c2ade84ed4982c3047817f919",
				OrdinalBytesSHA256: "dbb4c25f6ef25b8715b1a6166c2de97961ac7814374025370e0816d620ddd960", OrdinalSHA256: "cf9cb653161d4b5e21c528185a859704f4f1688d4945c2921f3e382d3911db87",
				PlanBytesSHA256: "ddaa6c3915b46c6e7e29814c429063456f203b1259c20dbf43e6a5b126371fce", PreparationPlanSHA256: "f31456d47cefab5464b2e39bb09d724f6d1a03b088660b7b020f3f31665cdb35",
			}},
		{name: "join", plan: queryplan.QueryPlan{From: &queryplan.From{Join: &queryplan.Join{
			Left: queryplan.Scan{Product: "compat_detail", Role: "compat_detail"}, Right: queryplan.Scan{Product: "compat_summary", Role: "compat_summary"},
			On: []queryplan.JoinPredicate{{Left: "compat_detail.department", Right: "compat_summary.department"}},
		}}, Columns: []string{"compat_detail.receipt_no", "compat_summary.total"}}, products: products,
			want: compilerV6CompatibilityBytes{
				VisibleSQLSHA256: "e763b1db6c40076fba4c614e38226d3f5cd48e4d744862629ee12ca2ce1ab19a", ProvenanceSQLSHA256: "6fa592b4267a647009f9fadf54f39ffdc18ae1dfd1740651225477add3d08842",
				SemanticBytesSHA256: "7b940059bd44f3cebc2a6c8ceb1d91bb5201745892071cab24bc591e52a57742", SemanticSHA256: "afee8de6b9eae8d93b536f866e1d3ca151295212235a34162638ad7c117fe630",
				OrdinalBytesSHA256: "699f997b273217462467a632f137b449e55e86bd073da75c6f1b0c0f171dcaf0", OrdinalSHA256: "811e217a633e1123d20f7137500b68d9437e35c9dc75eee58a3d24a321dd7bb4",
				PlanBytesSHA256: "b2e28eaa5ae77eae42c2b204e0a26c081ec16b8f359ee8376a271ce7e7946ccc", PreparationPlanSHA256: "acb69cb04a5a408cf6e4581f9005c32f3e132403f38c29cf3c9208c20bb154c0",
			}},
		{name: "union", plan: queryplan.QueryPlan{From: &queryplan.From{UnionDistinct: &queryplan.UnionDistinct{
			Role: "compat_summary", Columns: []string{"department", "month"},
			Left:  queryplan.Scan{Product: "compat_summary", Role: "left_branch", Filters: []queryplan.Filter{{Column: "month", Op: "=", Value: "2026-01"}}},
			Right: queryplan.Scan{Product: "compat_summary", Role: "right_branch", Filters: []queryplan.Filter{{Column: "month", Op: "=", Value: "2026-02"}}},
		}}, Columns: []string{"compat_summary.department"}}, products: products,
			want: compilerV6CompatibilityBytes{
				VisibleSQLSHA256: "1c0dfb7554b79950297977f3d36504fe59a7c0321513bb3162d76e61654840d5", ProvenanceSQLSHA256: "7b798d2fbd5e2a686466acf3c03b873927616a33c4533d3f7344ad6c32a54aed",
				SemanticBytesSHA256: "08c1e7db87a50543e87b6dde290da87f6fd66ac245e8937c970b23a72c023d51", SemanticSHA256: "3b2cf38e0febd47945583e3eabf8f9d5b8b897e957ee6e3183bd0ff5103884ae",
				OrdinalBytesSHA256: "1236dd1cc4337a68588f528f81dd91c59b21640f0a71626731e9797872e15a49", OrdinalSHA256: "6821cd575e89e978f422c30c5e607776cee5be3c93232340b404aeeab87add50",
				PlanBytesSHA256: "2bbf87adaa095c8ebce7ec8bd5cde8b029a2b85939d8acd4493de91ed2c4353e", PreparationPlanSHA256: "0d220c7be6c09f2aee04de7a5fc5e60a302f690c23c638fa4d1817e2b62c1f9e",
			}},
		{name: "grouped", plan: queryplan.QueryPlan{From: &queryplan.From{Join: &queryplan.Join{
			Left: queryplan.Scan{Product: "compat_detail", Role: "compat_detail"}, Right: queryplan.Scan{Product: "compat_summary", Role: "compat_summary"},
			On: []queryplan.JoinPredicate{{Left: "compat_detail.department", Right: "compat_summary.department"}},
		}}, Columns: []string{"compat_summary.department"},
			Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "compat_detail.amount", Alias: "total"}},
			GroupBy:    []string{"compat_summary.department"}}, products: products,
			want: compilerV6CompatibilityBytes{
				VisibleSQLSHA256: "59ba913fe4e38ee04337098f5243bb85e5b74000f2afffc6ef46ba060f7c3528", ProvenanceSQLSHA256: "2d5e0ca1a7e52b25081e68f040f7b0b3be0449fc1610fa50f3494ec8cb87d8ee",
				SemanticBytesSHA256: "50ad4e644645bd61498aed708b311970dcb21cdbab21892405dd2fd95bae435c", SemanticSHA256: "773c83211edb9d08d41be26906dc38474e7ceb980f384ea5f0fd982a1ad73747",
				OrdinalBytesSHA256: "2e51f989b5a1cfe86f83093d6a9c166f50c110d5c1407cd71f8a1007313896aa", OrdinalSHA256: "a78dd5b774eb58c10dc42c114a21a5762a0d8159ef3918687b99ab29793bdd8f",
				PlanBytesSHA256: "15eec54fec4247899bfa48064ea9013373d0a51440f18793553e30a91fc7c496", PreparationPlanSHA256: "fa6a573d7dc3a0fbfcf7c98b1f5224fe4264bb18d17f793610bc68d8e227f6a5",
			}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.plan.OrderBy) != 0 {
				t.Fatal("v6 compatibility probe unexpectedly requests relational presentation order")
			}
			got := compileV6CompatibilityBytes(t, test.plan, test.single, test.products)
			if got != test.want {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(test.want)
				t.Fatalf("v7 changed v6 %s bytes outside compiler identity:\ngot  %s\nwant %s", test.name, gotJSON, wantJSON)
			}
		})
	}
}

func compileV6CompatibilityBytes(t *testing.T, plan queryplan.QueryPlan, single *queryplan.Product,
	products map[string]queryplan.Product) compilerV6CompatibilityBytes {
	t.Helper()
	var visibleSQL, provenanceSQL string
	var program queryplan.OrdinalProgram
	var semanticBytes []byte
	var semanticSHA string
	var err error
	if single != nil {
		compiled, compileErr := queryplan.CompileOrdinal(plan, *single)
		if compileErr != nil {
			t.Fatal(compileErr)
		}
		visibleSQL, provenanceSQL, program = compiled.VisibleSQL, compiled.ProvenanceSQL, compiled.OrdinalProgram
		normal, normalErr := queryplan.NormalizeV4(plan, *single)
		if normalErr != nil {
			t.Fatal(normalErr)
		}
		semanticBytes, err = json.Marshal(normal)
		if err == nil {
			semanticSHA, err = normal.Digest()
		}
	} else {
		compiled, compileErr := queryplan.CompileRelational(plan, products)
		if compileErr != nil {
			t.Fatal(compileErr)
		}
		visibleSQL, provenanceSQL, program = compiled.VisibleSQL, compiled.ProvenanceSQL, compiled.OrdinalProgram
		normal, normalErr := queryplan.SemanticNormalFormV4(plan, compiled, products)
		if normalErr != nil {
			t.Fatal(normalErr)
		}
		semanticBytes, semanticSHA = normal.Canonical, normal.SHA256
	}
	if err != nil {
		t.Fatal(err)
	}
	ordinalBytes, err := program.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	ordinalSHA, err := program.Digest()
	if err != nil {
		t.Fatal(err)
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	preparationPlanSHA, err := (PreparationInputs{Plan: plan}).PlanSHA256()
	if err != nil {
		t.Fatal(err)
	}
	return compilerV6CompatibilityBytes{
		VisibleSQLSHA256: hashV6CompatibilityBytes([]byte(visibleSQL)), ProvenanceSQLSHA256: hashV6CompatibilityBytes([]byte(provenanceSQL)),
		SemanticBytesSHA256: hashV6CompatibilityBytes(semanticBytes), SemanticSHA256: semanticSHA,
		OrdinalBytesSHA256: hashV6CompatibilityBytes(ordinalBytes), OrdinalSHA256: ordinalSHA,
		PlanBytesSHA256: hashV6CompatibilityBytes(planBytes), PreparationPlanSHA256: preparationPlanSHA,
	}
}

func compilerV6CompatibilityProducts() map[string]queryplan.Product {
	textCollations := map[string]string{"department": "C", "month": "C", "receipt_no": "C"}
	textVersions := map[string]string{"department": "compat-collation-v1", "month": "compat-collation-v1", "receipt_no": "compat-collation-v1"}
	return map[string]queryplan.Product{
		"compat_expense": {
			Name: "compat_expense", Columns: map[string]struct{}{"id": {}, "amount": {}, "department": {}},
			AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}},
			ColumnTypes:       map[string]string{"id": "bigint", "amount": "numeric", "department": "text"},
			ColumnCollations:  map[string]string{"department": "C"}, CollationVersions: map[string]string{"department": "compat-collation-v1"},
			SourceNamespace: "compat.expense", Snapshot: "compat-snapshot-v1", StableRole: "compat_expense", StableEntityKey: []string{"id"},
			SnapshotPublication: "compat-expense-v1", SidecarManifestDigest: strings.Repeat("1", 64),
		},
		"compat_detail": {
			Name: "compat_detail", Columns: map[string]struct{}{"receipt_no": {}, "department": {}, "amount": {}},
			AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}},
			ColumnTypes:       map[string]string{"receipt_no": "text", "department": "text", "amount": "numeric"},
			ColumnCollations:  textCollations, CollationVersions: textVersions,
			SourceNamespace: "compat.detail", Snapshot: "compat-snapshot-v1", StableRole: "compat_detail", StableEntityKey: []string{"receipt_no"},
			SnapshotPublication: "compat-detail-v1", SidecarManifestDigest: strings.Repeat("2", 64),
		},
		"compat_summary": {
			Name: "compat_summary", Columns: map[string]struct{}{"department": {}, "month": {}, "total": {}},
			AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}},
			ColumnTypes:       map[string]string{"department": "text", "month": "text", "total": "numeric"},
			ColumnCollations:  textCollations, CollationVersions: textVersions,
			SourceNamespace: "compat.summary", Snapshot: "compat-snapshot-v1", StableRole: "compat_summary", StableEntityKey: []string{"month", "department"},
			SnapshotPublication: "compat-summary-v1", SidecarManifestDigest: strings.Repeat("3", 64),
		},
	}
}

func compatibilityProductPointer(product queryplan.Product) *queryplan.Product { return &product }

func hashV6CompatibilityBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
