package gateway

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

const semanticRuntimeRoot = "semantic_expense_runtime"

type semanticRuntimeFixture struct {
	service  *Service
	root     catalog.Product
	terminal catalog.Product
	artifact viewcompiler.Artifact
	registry viewcompiler.RegistrySnapshot
	binding  *resolvedViewBinding
	grant    control.TaskGrant
}

type semanticJoinManyRuntimeFixture struct {
	service   *Service
	root      catalog.Product
	terminals map[string]catalog.Product
	artifact  viewcompiler.Artifact
	binding   *resolvedViewBinding
	grant     control.TaskGrant
}

func TestPrepareSemanticViewPlanExpandsToLeastPrivilegeTerminalPlan(t *testing.T) {
	fixture := newSemanticRuntimeFixture(t, false)
	outer := queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}}
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name, outer, fixture.artifact)
	if err != nil {
		t.Fatalf("compose semantic View plan: %v", err)
	}

	prepared, err := fixture.service.prepareSemanticViewPlan(
		fixture.grant, fixture.root, fixture.artifact, composition, fixture.binding,
	)
	if err != nil {
		t.Fatalf("prepare semantic View plan: %v", err)
	}
	if prepared.Exposure == nil || prepared.Exposure.relational == nil {
		t.Fatal("semantic View did not enter relational exposure preparation")
	}
	if !strings.Contains(prepared.SQL, `"expense_detail"`) || strings.Contains(prepared.SQL, `"semantic_expense_runtime"`) {
		t.Fatalf("visible SQL was not expanded to the terminal product: %s", prepared.SQL)
	}
	if prepared.Exposure.plan.From == nil || prepared.Exposure.plan.From.Scan == nil ||
		prepared.Exposure.plan.From.Scan.Product != fixture.terminal.Name {
		t.Fatalf("prepared plan is not a terminal scan: %#v", prepared.Exposure.plan)
	}
	if got := prepared.Exposure.relational.compilation.Products; !reflect.DeepEqual(got, []string{fixture.terminal.Name}) {
		t.Fatalf("compiled products = %v, want terminal only", got)
	}
	if prepared.Exposure.viewBindingDigest != fixture.binding.Digest ||
		prepared.Exposure.viewRegistryRevision != fixture.binding.Expectation.ExpectedRevisionDigest {
		t.Fatalf("execution evidence lost View binding: digest=%q revision=%q",
			prepared.Exposure.viewBindingDigest, prepared.Exposure.viewRegistryRevision)
	}

	publicGrant, err := fixture.service.policyGrant(fixture.grant)
	if err != nil {
		t.Fatalf("build public policy grant: %v", err)
	}
	before := clonePolicyGrant(publicGrant)
	extended, err := prepared.Exposure.extendGrant(publicGrant)
	if err != nil {
		t.Fatalf("extend terminal policy grant: %v", err)
	}
	if !reflect.DeepEqual(publicGrant, before) {
		t.Fatalf("terminal expansion mutated public Grant: before=%#v after=%#v", before, publicGrant)
	}
	if len(publicGrant.Products) != 1 || publicGrant.Products[0].LogicalName != fixture.root.Name ||
		!reflect.DeepEqual(publicGrant.Products[0].ApprovedColumns, []string{"amount"}) {
		t.Fatalf("public Grant was broadened: %#v", publicGrant)
	}
	if len(extended.Products) != 2 {
		t.Fatalf("extended products = %#v, want public root plus one terminal", extended.Products)
	}
	terminalGrant, ok := policyProductByName(extended, fixture.terminal.Name)
	if !ok {
		t.Fatalf("extended Grant lacks terminal product: %#v", extended.Products)
	}
	wantColumns := []string{"amount", "department", "receipt_no"}
	if !reflect.DeepEqual(terminalGrant.ApprovedColumns, wantColumns) {
		t.Fatalf("terminal approved columns = %v, want exact positive-evidence closure %v",
			terminalGrant.ApprovedColumns, wantColumns)
	}
	wantScope := []sqlpolicy.ScopePredicate{{
		Column: "department", Operator: sqlpolicy.ScopeEqual, Values: []string{"销售部"},
	}}
	if !reflect.DeepEqual(terminalGrant.MandatoryScope, wantScope) {
		t.Fatalf("mapped terminal scope = %#v, want %#v", terminalGrant.MandatoryScope, wantScope)
	}
	rootGrant, ok := policyProductByName(extended, fixture.root.Name)
	if !ok || len(rootGrant.MandatoryScope) != 1 || rootGrant.MandatoryScope[0].Column != "business_unit" {
		t.Fatalf("public root scope was replaced instead of privately extended: %#v", rootGrant)
	}
}

func TestPrepareSemanticJoinManyPreservesThreeTerminalLeastPrivilegeClosure(t *testing.T) {
	fixture := newSemanticJoinManyRuntimeFixture(t)
	contract := fixture.root.ViewContract
	if contract == nil || contract.DefinitionDigest != fixture.artifact.DefinitionDigest ||
		contract.DependencyDigest != fixture.artifact.DependencyDigest ||
		contract.CanonicalPlanDigest != fixture.artifact.CanonicalPlanDigest ||
		contract.InterfaceDigest != fixture.artifact.InterfaceDigest {
		t.Fatalf("root View contract does not bind the compiled artifact: contract=%#v artifact=%#v",
			contract, fixture.artifact)
	}
	if fixture.artifact.Plan.From == nil {
		t.Fatalf("compiled artifact has no relational source: %#v", fixture.artifact.Plan)
	}
	artifactJoin := fixture.artifact.Plan.From.JoinMany
	if artifactJoin == nil || len(artifactJoin.Sources) != 3 || len(artifactJoin.On) != 3 {
		t.Fatalf("compiled artifact is not the expected three-source JoinMany: %#v", fixture.artifact.Plan)
	}
	wantPredicates := map[string]struct{}{
		"runtime_account.account_id\x00runtime_order.account_ref":      {},
		"runtime_account.account_tenant\x00runtime_order.order_tenant": {},
		"runtime_order.order_id\x00runtime_payment.order_ref":          {},
	}
	gotPredicates := make(map[string]struct{}, len(artifactJoin.On))
	for _, predicate := range artifactJoin.On {
		left, right := predicate.Left, predicate.Right
		if right < left {
			left, right = right, left
		}
		gotPredicates[left+"\x00"+right] = struct{}{}
	}
	if !reflect.DeepEqual(gotPredicates, wantPredicates) {
		t.Fatalf("compiled JoinMany predicates = %#v, want %#v", artifactJoin.On, wantPredicates)
	}

	outer := queryplan.QueryPlan{Product: fixture.root.Name,
		Columns: []string{"account_label", "order_state", "payment_state"}}
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name, outer, fixture.artifact)
	if err != nil {
		t.Fatalf("compose three-terminal semantic View: %v", err)
	}
	if composition.Plan.From == nil || composition.Plan.From.JoinMany == nil ||
		len(composition.Plan.From.JoinMany.Sources) != 3 ||
		!reflect.DeepEqual(composition.Plan.From.JoinMany.On, artifactJoin.On) {
		t.Fatalf("composition lost JoinMany sources or predicates: %#v", composition.Plan)
	}

	prepared, err := fixture.service.prepareSemanticViewPlan(
		fixture.grant, fixture.root, fixture.artifact, composition, fixture.binding,
	)
	if err != nil {
		t.Fatalf("prepare three-terminal semantic View: %v", err)
	}
	if prepared.Exposure == nil || prepared.Exposure.relational == nil ||
		prepared.Exposure.plan.From == nil || prepared.Exposure.plan.From.JoinMany == nil {
		t.Fatalf("prepared semantic View lost its relational JoinMany: %#v", prepared.Exposure)
	}
	preparedJoin := prepared.Exposure.plan.From.JoinMany
	if len(preparedJoin.Sources) != 3 || !reflect.DeepEqual(preparedJoin.On, artifactJoin.On) {
		t.Fatalf("prepared JoinMany lost sources or predicates: %#v", preparedJoin)
	}
	compilation := prepared.Exposure.relational.compilation
	if compilation.Kind != "join" || len(compilation.Sources) != 3 ||
		!reflect.DeepEqual(compilation.JoinPredicates, artifactJoin.On) {
		t.Fatalf("relational compilation lost JoinMany semantics: kind=%q sources=%#v predicates=%#v",
			compilation.Kind, compilation.Sources, compilation.JoinPredicates)
	}

	wantColumns := map[string][]string{
		"runtime_account": {"account_id", "account_label", "account_tenant"},
		"runtime_order":   {"account_ref", "order_id", "order_state", "order_tenant"},
		"runtime_payment": {"order_ref", "payment_id", "payment_state", "payment_tenant"},
	}
	wantScopes := map[string]string{
		"runtime_account": "account_tenant",
		"runtime_order":   "order_tenant",
		"runtime_payment": "payment_tenant",
	}
	if len(prepared.Exposure.internalPolicyProducts) != len(wantColumns) {
		t.Fatalf("internal terminal grants = %#v, want exactly three terminals",
			prepared.Exposure.internalPolicyProducts)
	}
	for name, columns := range wantColumns {
		internal, present := prepared.Exposure.internalPolicyProducts[name]
		if !present {
			t.Fatalf("internal grant lacks terminal %q: %#v", name, prepared.Exposure.internalPolicyProducts)
		}
		if !reflect.DeepEqual(internal.ApprovedColumns, columns) {
			t.Fatalf("internal grant %q columns = %v, want exact closure %v", name, internal.ApprovedColumns, columns)
		}
		wantScope := []sqlpolicy.ScopePredicate{{
			Column: wantScopes[name], Operator: sqlpolicy.ScopeEqual, Values: []string{"tenant-42"},
		}}
		if !reflect.DeepEqual(internal.MandatoryScope, wantScope) {
			t.Fatalf("internal grant %q scope = %#v, want %#v", name, internal.MandatoryScope, wantScope)
		}
	}

	publicGrant, err := fixture.service.policyGrant(fixture.grant)
	if err != nil {
		t.Fatalf("build public root policy grant: %v", err)
	}
	publicBefore := clonePolicyGrant(publicGrant)
	rootBefore, present := policyProductByName(publicBefore, fixture.root.Name)
	if !present {
		t.Fatalf("public grant lacks semantic root: %#v", publicBefore)
	}
	extended, err := prepared.Exposure.extendGrant(publicGrant)
	if err != nil {
		t.Fatalf("extend public grant with terminal closure: %v", err)
	}
	if !reflect.DeepEqual(publicGrant, publicBefore) {
		t.Fatalf("terminal extension mutated public root grant: before=%#v after=%#v", publicBefore, publicGrant)
	}
	if len(extended.Products) != len(wantColumns)+1 {
		t.Fatalf("extended grants = %#v, want root plus three terminals", extended.Products)
	}
	rootAfter, present := policyProductByName(extended, fixture.root.Name)
	if !present || !reflect.DeepEqual(rootAfter, rootBefore) {
		t.Fatalf("public root grant changed during private extension: before=%#v after=%#v", rootBefore, rootAfter)
	}
	for name, columns := range wantColumns {
		terminal, present := policyProductByName(extended, name)
		if !present || !reflect.DeepEqual(terminal.ApprovedColumns, columns) {
			t.Fatalf("extended terminal grant %q = %#v, want exact columns %v", name, terminal, columns)
		}
		wantScope := []sqlpolicy.ScopePredicate{{
			Column: wantScopes[name], Operator: sqlpolicy.ScopeEqual, Values: []string{"tenant-42"},
		}}
		if !reflect.DeepEqual(terminal.MandatoryScope, wantScope) {
			t.Fatalf("extended terminal grant %q scope = %#v, want %#v", name, terminal.MandatoryScope, wantScope)
		}
	}

	policy := sqlpolicy.New(sqlpolicy.Config{})
	for name, statement := range map[string]string{
		"visible": prepared.SQL, "provenance": prepared.Exposure.provenanceSQL,
	} {
		decision, authorizeErr := policy.Authorize(sqlpolicy.Request{
			SQL: statement, Grant: extended, RowLimit: 25,
		})
		if authorizeErr != nil {
			t.Fatalf("%s three-terminal SQL rejected after private grant extension: %v\nSQL: %s",
				name, authorizeErr, statement)
		}
		for _, terminal := range fixture.terminals {
			if !strings.Contains(decision.SQL, `"`+strings.TrimPrefix(terminal.ReportingView, "reporting.")+`"`) {
				t.Fatalf("%s authorized SQL lost terminal %q: %s", name, terminal.Name, decision.SQL)
			}
		}
	}
}

func TestPrepareSemanticAggregateBarrierPreservesProjectionOrder(t *testing.T) {
	fixture := newSemanticRuntimeFixture(t, true)
	outer := queryplan.QueryPlan{Product: fixture.root.Name,
		Columns: []string{"request_count", "business_unit", "total_amount"}}
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name, outer, fixture.artifact)
	if err != nil {
		t.Fatalf("compose aggregate barrier projection: %v", err)
	}
	wantVisible := []string{"request_count", "expense_detail.department", "total_amount"}
	if !reflect.DeepEqual(composition.VisibleFields, wantVisible) {
		t.Fatalf("composition visible order = %v, want %v", composition.VisibleFields, wantVisible)
	}
	prepared, err := fixture.service.prepareSemanticViewPlan(
		fixture.grant, fixture.root, fixture.artifact, composition, fixture.binding,
	)
	if err != nil {
		t.Fatalf("prepare aggregate semantic View: %v", err)
	}
	if !reflect.DeepEqual(prepared.Exposure.visibleFields, wantVisible) {
		t.Fatalf("prepared visible order = %v, want %v", prepared.Exposure.visibleFields, wantVisible)
	}
	if prepared.Exposure.relational == nil || !prepared.Exposure.grouped ||
		len(prepared.Exposure.plan.GroupBy) != 1 || len(prepared.Exposure.plan.Aggregates) != 2 {
		t.Fatalf("aggregate barrier was not retained: %#v", prepared.Exposure.plan)
	}

	compilation := prepared.Exposure.relational.compilation
	values := map[string]any{
		"expense_detail.department": "销售部",
		"request_count":             int64(4),
		"total_amount":              "3433.00",
	}
	types := map[string]uint32{
		"expense_detail.department": 25,
		"request_count":             20,
		"total_amount":              1700,
	}
	raw := dataconnector.Result{RowCount: 1, DatabaseTime: time.Millisecond}
	raw.Rows = [][]any{make([]any, len(compilation.InternalFields))}
	for index, semantic := range compilation.InternalFields {
		raw.Columns = append(raw.Columns, dataconnector.Column{
			Name: compilation.OutputAliases[semantic], DataTypeOID: types[semantic],
		})
		raw.Rows[0][index] = values[semantic]
	}
	visible, err := prepared.Exposure.visibleResult(raw)
	if err != nil {
		t.Fatalf("reorder aggregate visible result: %v", err)
	}
	wantValues := []any{int64(4), "销售部", "3433.00"}
	if !reflect.DeepEqual(columnNamesForTest(visible.Columns), wantVisible) ||
		!reflect.DeepEqual(visible.Rows, [][]any{wantValues}) {
		t.Fatalf("visible result was not request-ordered: columns=%v rows=%#v",
			columnNamesForTest(visible.Columns), visible.Rows)
	}
	if got := []uint32{visible.Columns[0].DataTypeOID, visible.Columns[1].DataTypeOID, visible.Columns[2].DataTypeOID}; !reflect.DeepEqual(got, []uint32{20, 25, 1700}) {
		t.Fatalf("visible result column metadata was not reordered with values: %v", got)
	}
	publicColumns, publicRows, err := publicStoredResult(storedQueryResult{
		Columns: visible.Columns, Rows: visible.Rows,
		DisplayColumns: append([]string(nil), outer.Columns...), ResultOrder: identityResultOrder(len(outer.Columns)),
	})
	if err != nil {
		t.Fatalf("render public aggregate result: %v", err)
	}
	if !reflect.DeepEqual(columnNamesForTest(publicColumns), outer.Columns) ||
		!reflect.DeepEqual(publicRows, [][]any{wantValues}) {
		t.Fatalf("public aggregate result lost requested order: columns=%v rows=%#v",
			columnNamesForTest(publicColumns), publicRows)
	}

	tests := []struct {
		name string
		plan queryplan.QueryPlan
	}{
		{name: "filter", plan: queryplan.QueryPlan{Product: fixture.root.Name,
			Columns: []string{"business_unit"},
			Filters: []queryplan.Filter{{Column: "total_amount", Op: ">", Value: 0}}}},
		{name: "reaggregate", plan: queryplan.QueryPlan{Product: fixture.root.Name,
			Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "total_amount", Alias: "grand_total"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, composeErr := viewcompiler.ComposeQueryPlan(fixture.root.Name, test.plan, fixture.artifact)
			var rejected *viewcompiler.Error
			if !errors.As(composeErr, &rejected) || rejected.Code != viewcompiler.CodeCompositionUnsupported {
				t.Fatalf("error = %T %v, want %s", composeErr, composeErr, viewcompiler.CodeCompositionUnsupported)
			}
		})
	}
}

func TestPrepareTaskPlanEnforcesRootAllowedOperatorsBeforeExpansion(t *testing.T) {
	fixture := newSemanticRuntimeFixture(t, false)
	if !contains(fixture.terminal.AllowedOperators, ">") {
		t.Fatal("fixture terminal must allow > to prove the root is authoritative")
	}
	fixture.root.AllowedOperators = []string{"="}
	replaceCatalogProductForTest(t, fixture.service.catalog, fixture.root)
	plan := queryplan.QueryPlan{
		Product: fixture.root.Name, Columns: []string{"amount"},
		Filters: []queryplan.Filter{{Column: "amount", Op: ">", Value: 0}},
	}
	_, err := fixture.service.prepareTaskPlan(context.Background(), control.Task{}, fixture.grant, plan)
	requireToolCode(t, err, apierr.CodePolicyDenied)
}

func TestPrepareSemanticLeafFilterUsesExactTerminalColumnClosure(t *testing.T) {
	fixture := newSemanticRuntimeFixture(t, false)
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name,
		queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}}, fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	if composition.Plan.From == nil || composition.Plan.From.Scan == nil {
		t.Fatalf("fixture is not a leaf scan: %#v", composition.Plan.From)
	}
	composition.Plan.From.Scan.Filters = []queryplan.Filter{{
		Column: "status", Op: "=", Value: "approved",
	}}
	wantColumns := []string{"amount", "department", "receipt_no", "status"}
	closure, err := semanticPlanRequiredColumns(composition.Plan,
		map[string]catalog.Product{fixture.terminal.Name: fixture.terminal})
	if err != nil {
		t.Fatalf("compute terminal column closure: %v", err)
	}
	if got := sortedStringSet(closure[fixture.terminal.Name]); !reflect.DeepEqual(got, wantColumns) {
		t.Fatalf("terminal column closure = %v, want %v", got, wantColumns)
	}

	prepared, err := fixture.service.prepareSemanticViewPlan(
		fixture.grant, fixture.root, fixture.artifact, composition, fixture.binding,
	)
	if err != nil {
		t.Fatalf("prepare leaf-filter semantic View: %v", err)
	}
	publicGrant, err := fixture.service.policyGrant(fixture.grant)
	if err != nil {
		t.Fatalf("build public policy grant: %v", err)
	}
	extended, err := prepared.Exposure.extendGrant(publicGrant)
	if err != nil {
		t.Fatalf("extend private terminal policy: %v", err)
	}
	terminalGrant, present := policyProductByName(extended, fixture.terminal.Name)
	if !present || !reflect.DeepEqual(terminalGrant.ApprovedColumns, wantColumns) {
		t.Fatalf("private terminal authority = %#v, want exact columns %v", terminalGrant, wantColumns)
	}
	for _, unrelated := range []string{"employee_no", "employee_name", "expense_date", "expense_type", "city", "purpose"} {
		if contains(terminalGrant.ApprovedColumns, unrelated) {
			t.Fatalf("private terminal authority includes unrelated column %q: %v",
				unrelated, terminalGrant.ApprovedColumns)
		}
	}
	engine := sqlpolicy.New(sqlpolicy.Config{})
	for name, statement := range map[string]string{
		"visible": prepared.SQL, "provenance": prepared.Exposure.provenanceSQL,
	} {
		decision, authorizeErr := engine.Authorize(sqlpolicy.Request{
			SQL: statement, Grant: extended, RowLimit: 25,
		})
		if authorizeErr != nil {
			t.Fatalf("%s expanded SQL rejected by private policy: %v\nSQL: %s", name, authorizeErr, statement)
		}
		if !strings.Contains(decision.SQL, `"status"`) || !strings.Contains(decision.SQL, `'approved'`) {
			t.Fatalf("%s policy SQL lost leaf filter: %s", name, decision.SQL)
		}
	}
}

func TestPrepareTaskPlanRejectsSemanticViewWhenExposureDisabled(t *testing.T) {
	fixture := newSemanticRuntimeFixture(t, false)
	fixture.grant.Exposure = control.ExposureGrant{}
	_, err := fixture.service.prepareTaskPlan(context.Background(), control.Task{}, fixture.grant,
		queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}})
	requireViewQueryReasonForTest(t, err, "SEMANTIC_VIEW_EXPOSURE_PROFILE")
}

func TestSemanticViewRawAndPlanBypassesRejectWhenExposureDisabled(t *testing.T) {
	harness := newGatewayHarness(t)
	fixture := newSemanticRuntimeFixture(t, false)
	installSemanticRuntimeCatalogForTest(t, harness.catalog, fixture)
	taskID := "semantic-view-exposure-disabled"
	harness.createTaskWithGrantAndExposureProfile(t, taskID, nil, control.ExposureLimits{}, "",
		[]string{fixture.root.Name}, map[string][]string{fixture.root.Name: {"amount"}}, domain.SensitivityHigh)

	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{name: "raw SQL", tool: "query_sql", args: map[string]any{
			"task_id": taskID, "request_id": "disabled-semantic-raw",
			"sql": `SELECT amount FROM semantic_expense_runtime`,
		}},
		{name: "QueryPlan", tool: "execute_plan", args: map[string]any{
			"task_id": taskID, "request_id": "disabled-semantic-plan",
			"plan": queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := callGatewayTool(harness.service, harness.alice, test.tool, test.args)
			requireViewQueryReasonForTest(t, err, "SEMANTIC_VIEW_EXPOSURE_PROFILE")
		})
	}
	if len(harness.connector.requests) != 0 {
		t.Fatalf("disabled semantic View bypass reached connector: %#v", harness.connector.requests)
	}
}

func TestReplayPreparationFailurePrefersDurableTerminalOutcome(t *testing.T) {
	harness := newGatewayHarness(t)
	const taskID = "preparation-race-terminal-task"
	const requestID = "preparation-race-terminal-request"
	harness.createActiveSummaryTask(t, taskID)
	first := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": taskID, "request_id": requestID, "sql": testSummarySQL,
	})
	record, err := harness.store.GetQueryByRequestID(context.Background(), taskID, requestID)
	if err != nil {
		t.Fatalf("load durable terminal query: %v", err)
	}
	if record.Status != control.QueryCompleted || record.CompletedAt == nil || record.RequestDigest == "" {
		t.Fatalf("query is not a durable terminal outcome: %#v", record)
	}

	preparationErr := viewQueryUnsupported("SEMANTIC_VIEW_EXPOSURE_PROFILE")
	replayed, err := harness.service.replayPreparationFailure(
		context.Background(), taskID, requestID, record.RequestDigest, preparationErr,
	)
	if err != nil {
		t.Fatalf("durable exact outcome did not override preparation error: %v", err)
	}
	response, ok := replayed.(map[string]any)
	if !ok || response["idempotent_replay"] != true || response["query_id"] != first["query_id"] ||
		response["status"] != control.QueryCompleted {
		t.Fatalf("preparation failure replay = %#v, want durable terminal query %v", replayed, first["query_id"])
	}
	if len(harness.connector.requests) != 1 {
		t.Fatalf("preparation replay re-executed connector: %d calls", len(harness.connector.requests))
	}

	_, err = harness.service.replayPreparationFailure(
		context.Background(), taskID, requestID, digest("different request payload"), preparationErr,
	)
	requireToolCode(t, err, apierr.CodeConflict)
}

func TestPrepareSemanticViewPlanRejectsUnprovenTerminalScope(t *testing.T) {
	fixture := newSemanticRuntimeFixture(t, false)
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name,
		queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}}, fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	broken := fixture.artifact
	broken.Outputs = append([]viewcompiler.Output(nil), fixture.artifact.Outputs...)
	for index := range broken.Outputs {
		if broken.Outputs[index].Name == "business_unit" {
			// A same-typed base field is still not a proof of the terminal's
			// mandatory department scope.
			broken.Outputs[index].FieldID = "expense_detail.receipt_no"
		}
	}
	_, err = fixture.service.prepareSemanticViewPlan(
		fixture.grant, fixture.root, broken, composition, fixture.binding,
	)
	requireToolCode(t, err, apierr.CodePolicyDenied)
}

func TestPrepareSemanticViewPlanRejectsLowerRootSensitivity(t *testing.T) {
	fixture := newSemanticRuntimeFixture(t, false)
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name,
		queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}}, fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	lowRoot := fixture.root
	lowRoot.Sensitivity = domain.SensitivityLow
	_, err = fixture.service.prepareSemanticViewPlan(
		fixture.grant, lowRoot, fixture.artifact, composition, fixture.binding,
	)
	requireToolCode(t, err, apierr.CodePolicyDenied)
}

func TestPrepareSemanticViewPlanV4BindsOnlyTerminalOrdinalSources(t *testing.T) {
	fixture := newSemanticRuntimeFixture(t, false)
	installSemanticRuntimeSnapshotRegistry(t, fixture.service)
	fixture.grant.Exposure.ProfileVersion = exposure.ProfileV4
	if fixture.root.SnapshotPublication != "" {
		t.Fatalf("fixture root unexpectedly owns snapshot publication %q", fixture.root.SnapshotPublication)
	}
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name,
		queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}}, fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := fixture.service.prepareSemanticViewPlan(
		fixture.grant, fixture.root, fixture.artifact, composition, fixture.binding,
	)
	if err != nil {
		t.Fatalf("prepare V4 semantic View: %v", err)
	}
	if prepared.Exposure.ordinal == nil {
		t.Fatal("V4 semantic View did not bind an ordinal execution")
	}
	sources := prepared.Exposure.ordinal.Program.Sources
	if len(sources) != 1 || sources[0].Product != fixture.terminal.Name ||
		sources[0].SourceAlias != fixture.terminal.StableRelationRole {
		t.Fatalf("ordinal sources = %#v, want terminal only", sources)
	}
	if _, present := prepared.Exposure.ordinal.Indexes[fixture.terminal.StableRelationRole]; !present {
		t.Fatalf("ordinal indexes = %#v, terminal role missing", prepared.Exposure.ordinal.Indexes)
	}
	if _, present := prepared.Exposure.ordinal.Indexes[fixture.root.StableRelationRole]; present {
		t.Fatalf("semantic root incorrectly became an ordinal source: %#v", prepared.Exposure.ordinal.Indexes)
	}
	if len(prepared.Exposure.ordinal.SidecarGrants) != 1 {
		t.Fatalf("sidecar grants = %#v, want terminal publication only", prepared.Exposure.ordinal.SidecarGrants)
	}
}

func TestPrepareTaskPlanRejectsSemanticViewGraphGraft(t *testing.T) {
	fixture := newSemanticRuntimeFixture(t, false)
	plan := queryplan.QueryPlan{
		From:    &queryplan.From{Scan: &queryplan.Scan{Product: fixture.root.Name, Role: "semantic_root"}},
		Columns: []string{"semantic_root.amount"},
	}
	_, err := fixture.service.prepareTaskPlan(context.Background(), control.Task{}, fixture.grant, plan)
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != apierr.CodeViewQueryUnsupported {
		t.Fatalf("error = %T %v, want %s", err, err, apierr.CodeViewQueryUnsupported)
	}
	if reason, _ := toolErr.Details["reason"].(string); reason != "SEMANTIC_VIEW_GRAPH_GRAFT" {
		t.Fatalf("reason = %q, want stable graph-graft rejection", reason)
	}
}

func TestExecutePlanSemanticViewCarriesRegistryExpectationToPairedQueries(t *testing.T) {
	harness := newGatewayHarness(t)
	fixture := newSemanticRuntimeFixture(t, false)
	harness.catalog.Products = append(harness.catalog.Products, fixture.root)
	for _, scope := range fixture.service.catalog.Scopes {
		if scope.Name == "business_unit" {
			harness.catalog.Scopes = append(harness.catalog.Scopes, scope)
			break
		}
	}
	registryConnector := &registryFakeConnector{fakeConnector: harness.connector, snapshot: fixture.registry}
	harness.service.connector = registryConnector
	indexes := harness.installCatalogV4SnapshotRegistry(t)
	taskID := requestAndApproveSemanticRuntimeTask(t, harness)

	// Build the exact terminal ordinal row contract used by the public path,
	// then feed one immutable published row through the streaming fake.
	fixture.service = harness.service
	fixture.grant.Exposure.ProfileVersion = exposure.ProfileV4
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name,
		queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}}, fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := fixture.service.prepareSemanticViewPlan(
		fixture.grant, fixture.root, fixture.artifact, composition, fixture.binding,
	)
	if err != nil {
		t.Fatalf("prepare expected V4 row contract: %v", err)
	}
	bound := prepared.Exposure.ordinal
	if bound == nil {
		t.Fatal("expected V4 ordinal binding")
	}
	row := map[string]any{
		"receipt_no": "TR-2026-0001", "department": "销售部", "amount": "1680.00",
	}
	harness.connector.result = scanVisibleResult(t, bound.Program, []map[string]any{row})
	harness.connector.result.DatabaseTime = 2 * time.Millisecond
	provenanceColumns, positions := ordinalProvenanceColumns(bound.Program)
	provenanceRow := make([]any, len(provenanceColumns))
	for _, source := range bound.Program.Sources {
		entityKey := ordinalFixtureEntityKey(t, source, row)
		handle, present := indexes["expense-detail-v1"].LookupRowHandle(entityKey)
		if !present {
			t.Fatalf("published terminal index misses entity %q", entityKey)
		}
		provenanceRow[positions[source.HandleAlias]] = uint64(handle)
		for _, field := range source.EvidenceFields {
			provenanceRow[positions[field.ProvenanceAlias]] = row[field.Column]
		}
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: provenanceColumns, Rows: [][]any{provenanceRow}, RowCount: 1,
		DatabaseTime: time.Millisecond,
	}

	mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": taskID, "request_id": "semantic-view-execution-e2e",
		"plan": queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}},
	})
	record, err := harness.store.GetQueryByRequestID(context.Background(), taskID, "semantic-view-execution-e2e")
	if err != nil || record.ViewBindingDigest == "" {
		t.Fatalf("query record omitted View binding evidence: record=%#v err=%v", record, err)
	}
	if len(harness.connector.requests) != 2 {
		t.Fatalf("connector requests = %d, want visible/provenance pair", len(harness.connector.requests))
	}
	for index, request := range harness.connector.requests {
		if request.ViewRegistry == nil {
			t.Fatalf("paired request %d omitted ViewRegistry", index)
		}
		if request.ViewRegistry.ExpectedRevisionDigest != fixture.registry.RevisionDigest ||
			len(request.ViewRegistry.Roots) != 1 || request.ViewRegistry.Roots[0] != fixture.artifact.Root {
			t.Fatalf("paired request %d ViewRegistry = %#v", index, request.ViewRegistry)
		}
		if !reflect.DeepEqual(request.ViewRegistry.BaseProducts,
			map[string]string{"reporting.expense_detail": "expense_detail"}) {
			t.Fatalf("paired request %d terminal closure = %#v", index, request.ViewRegistry.BaseProducts)
		}
	}
}

func newSemanticJoinManyRuntimeFixture(t *testing.T) semanticJoinManyRuntimeFixture {
	t.Helper()
	const (
		rootName         = "semantic_runtime_join_many"
		source           = "runtime_source"
		snapshot         = "runtime-snapshot-v1"
		collation        = "en_US.utf8"
		collationVersion = "2.36"
	)
	operators := []string{"=", "<>"}
	terminals := []catalog.Product{
		{
			Name: "runtime_account", Source: source, ReportingView: "reporting.runtime_account",
			Sensitivity: domain.SensitivityHigh, Snapshot: snapshot, EntityKey: []string{"account_id"},
			FactNamespace: "runtime.account", StableRelationRole: "runtime_account",
			Scopes: []string{"account_tenant"}, AllowedOperators: operators,
			Fields: []catalog.Field{
				{Name: "account_id", Type: "text"},
				{Name: "account_tenant", Type: "text"},
				{Name: "account_label", Type: "text"},
				{Name: "unused_account_note", Type: "text"},
			},
		},
		{
			Name: "runtime_order", Source: source, ReportingView: "reporting.runtime_order",
			Sensitivity: domain.SensitivityHigh, Snapshot: snapshot, EntityKey: []string{"order_id"},
			FactNamespace: "runtime.order", StableRelationRole: "runtime_order",
			Scopes: []string{"order_tenant"}, AllowedOperators: operators,
			Fields: []catalog.Field{
				{Name: "order_id", Type: "text"},
				{Name: "account_ref", Type: "text"},
				{Name: "order_tenant", Type: "text"},
				{Name: "order_state", Type: "text"},
				{Name: "unused_order_note", Type: "text"},
			},
		},
		{
			Name: "runtime_payment", Source: source, ReportingView: "reporting.runtime_payment",
			Sensitivity: domain.SensitivityHigh, Snapshot: snapshot, EntityKey: []string{"payment_id"},
			FactNamespace: "runtime.payment", StableRelationRole: "runtime_payment",
			Scopes: []string{"payment_tenant"}, AllowedOperators: operators,
			Fields: []catalog.Field{
				{Name: "payment_id", Type: "text"},
				{Name: "order_ref", Type: "text"},
				{Name: "payment_tenant", Type: "text"},
				{Name: "payment_state", Type: "text"},
				{Name: "unused_payment_note", Type: "text"},
			},
		},
	}
	for productIndex := range terminals {
		for fieldIndex := range terminals[productIndex].Fields {
			terminals[productIndex].Fields[fieldIndex].Collation = collation
			terminals[productIndex].Fields[fieldIndex].CollationVersion = collationVersion
		}
	}

	rootRelation := viewcompiler.RelationName{Schema: "reporting", Name: rootName}
	definition := `SELECT a.account_tenant, b.order_tenant, c.payment_tenant,
		a.account_label, b.order_state, c.payment_state
		FROM reporting.runtime_account AS a
		INNER JOIN reporting.runtime_order AS b
			ON a.account_id = b.account_ref AND a.account_tenant = b.order_tenant
		INNER JOIN reporting.runtime_payment AS c
			ON b.order_id = c.order_ref`
	rootColumns := []viewcompiler.Column{
		{Name: "account_tenant", SQLType: "text"},
		{Name: "order_tenant", SQLType: "text"},
		{Name: "payment_tenant", SQLType: "text"},
		{Name: "account_label", SQLType: "text"},
		{Name: "order_state", SQLType: "text"},
		{Name: "payment_state", SQLType: "text"},
	}
	for index := range rootColumns {
		rootColumns[index].Collation = collation
		rootColumns[index].CollationVersion = collationVersion
	}
	registry := viewcompiler.RegistrySnapshot{
		PostgreSQLMajorVersion: 16,
		RevisionDigest:         strings.Repeat("6", 64),
		Relations:              make(map[viewcompiler.RelationName]viewcompiler.Relation, len(terminals)+1),
	}
	compilerProducts := make(map[string]queryplan.Product, len(terminals))
	dependencies := make([]viewcompiler.RelationName, 0, len(terminals))
	baseProducts := make(map[string]string, len(terminals))
	terminalByName := make(map[string]catalog.Product, len(terminals))
	for _, terminal := range terminals {
		relation := relationNameForTest(t, terminal.ReportingView)
		registry.Relations[relation] = viewcompiler.Relation{
			Name: relation, Kind: viewcompiler.RelationBase, ProductName: terminal.Name,
			Columns: viewColumns(terminal.Fields),
		}
		compilerProducts[terminal.Name] = relationalQueryProduct(
			terminal, stringSetFromSlice(terminal.FieldNames()),
		)
		dependencies = append(dependencies, relation)
		baseProducts[relation.String()] = terminal.Name
		terminalByName[terminal.Name] = terminal
	}
	registry.Relations[rootRelation] = viewcompiler.Relation{
		Name: rootRelation, Kind: viewcompiler.RelationView, DefinitionSQL: definition,
		DefinitionDigest: viewcompiler.ExactDefinitionDigest(definition),
		Columns:          rootColumns, Dependencies: dependencies,
	}
	compiler, err := viewcompiler.New(registry, compilerProducts)
	if err != nil {
		t.Fatalf("new three-terminal semantic View compiler: %v", err)
	}
	artifact, err := compiler.Compile(rootRelation)
	if err != nil {
		t.Fatalf("compile three-terminal semantic View: %v", err)
	}

	root := catalog.Product{
		Name: rootName, Source: source, ReportingView: rootRelation.String(),
		Sensitivity: domain.SensitivityHigh, Snapshot: snapshot,
		EntityKey: []string{"account_label"}, FactNamespace: "runtime.semantic_join_many",
		StableRelationRole: rootName,
		Scopes:             []string{"account_tenant", "order_tenant", "payment_tenant"},
		AllowedOperators:   operators,
		Fields: []catalog.Field{
			{Name: "account_tenant", Type: "text"},
			{Name: "order_tenant", Type: "text"},
			{Name: "payment_tenant", Type: "text"},
			{Name: "account_label", Type: "text"},
			{Name: "order_state", Type: "text"},
			{Name: "payment_state", Type: "text"},
		},
		ViewContract: &catalog.ViewContract{
			ProfileVersion:   catalog.ViewContractV1,
			DefinitionDigest: artifact.DefinitionDigest, DependencyDigest: artifact.DependencyDigest,
			CanonicalPlanDigest: artifact.CanonicalPlanDigest, InterfaceDigest: artifact.InterfaceDigest,
		},
	}
	for index := range root.Fields {
		root.Fields[index].Collation = collation
		root.Fields[index].CollationVersion = collationVersion
	}
	scopePolicy := func(name string) catalog.Scope {
		return catalog.Scope{Name: name, Type: catalog.ScopeTypeEnum,
			AllowedValues: []string{"tenant-42", "tenant-99"}}
	}
	logical := &catalog.Catalog{
		CatalogVersion: "runtime-join-many-test",
		Products:       append(append([]catalog.Product(nil), terminals...), root),
		Scopes: []catalog.Scope{
			scopePolicy("account_tenant"),
			scopePolicy("order_tenant"),
			scopePolicy("payment_tenant"),
		},
	}
	service := &Service{catalog: logical}
	grant := control.TaskGrant{
		ApprovedProducts: []string{root.Name},
		ApprovedColumns: map[string][]string{root.Name: {
			"account_label", "order_state", "payment_state",
		}},
		MandatoryScope:     []byte(`{"account_tenant":["tenant-42"],"order_tenant":["tenant-42"],"payment_tenant":["tenant-42"]}`),
		SensitivityCeiling: string(domain.SensitivityHigh),
		Exposure: control.ExposureGrant{
			Limits: control.ExposureLimits{
				ReleaseFacts: 100, InfluenceFacts: 500, OutcomeFacts: 10,
			},
			ProfileVersion: exposure.ProfileV2,
		},
	}
	binding := &resolvedViewBinding{
		pendingViewBinding: pendingViewBinding{Digest: strings.Repeat("5", 64)},
		Expectation: dataconnector.ViewRegistryExpectation{
			Roots: []viewcompiler.RelationName{rootRelation}, BaseProducts: baseProducts,
			ExpectedRevisionDigest: registry.RevisionDigest,
		},
		Artifacts: map[string]viewcompiler.Artifact{root.Name: artifact},
	}
	return semanticJoinManyRuntimeFixture{
		service: service, root: root, terminals: terminalByName,
		artifact: artifact, binding: binding, grant: grant,
	}
}

func newSemanticRuntimeFixture(t *testing.T, aggregated bool) semanticRuntimeFixture {
	t.Helper()
	logical, err := catalog.Load(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	terminal, present := logical.LookupProduct("expense_detail")
	if !present {
		t.Fatal("catalog lacks expense_detail terminal")
	}
	department, present := catalogFieldByName(terminal.Fields, "department")
	if !present {
		t.Fatal("expense_detail lacks department")
	}
	logical.Scopes = append(logical.Scopes, catalog.Scope{
		Name: "business_unit", Type: catalog.ScopeTypeEnum,
		AllowedValues: []string{"销售部", "研发部", "财务部"},
	})

	leaf := relationNameForTest(t, terminal.ReportingView)
	rootRelation := viewcompiler.RelationName{Schema: "reporting", Name: semanticRuntimeRoot}
	definition := `SELECT d.receipt_no, d.department AS business_unit, d.amount FROM reporting.expense_detail AS d`
	columns := []viewcompiler.Column{
		{Name: "receipt_no", SQLType: "text", Collation: terminal.Fields[0].Collation,
			CollationVersion: terminal.Fields[0].CollationVersion},
		{Name: "business_unit", SQLType: "text", Collation: department.Collation,
			CollationVersion: department.CollationVersion},
		{Name: "amount", SQLType: "numeric"},
	}
	if aggregated {
		definition = `SELECT d.department AS business_unit, SUM(d.amount) AS total_amount, COUNT(*) AS request_count
			FROM reporting.expense_detail AS d GROUP BY d.department`
		columns = []viewcompiler.Column{
			{Name: "business_unit", SQLType: "text", Collation: department.Collation,
				CollationVersion: department.CollationVersion},
			{Name: "total_amount", SQLType: "numeric"},
			{Name: "request_count", SQLType: "bigint"},
		}
	}
	registry := viewcompiler.RegistrySnapshot{
		PostgreSQLMajorVersion: 16,
		RevisionDigest:         strings.Repeat("7", 64),
		Relations: map[viewcompiler.RelationName]viewcompiler.Relation{
			leaf: {
				Name: leaf, Kind: viewcompiler.RelationBase, ProductName: terminal.Name,
				Columns: viewColumns(terminal.Fields),
			},
			rootRelation: {
				Name: rootRelation, Kind: viewcompiler.RelationView, DefinitionSQL: definition,
				DefinitionDigest: viewcompiler.ExactDefinitionDigest(definition),
				Columns:          columns, Dependencies: []viewcompiler.RelationName{leaf},
			},
		},
	}
	compiler, err := viewcompiler.New(registry, map[string]queryplan.Product{
		terminal.Name: relationalQueryProduct(terminal, stringSetFromSlice(terminal.FieldNames())),
	})
	if err != nil {
		t.Fatalf("new semantic View compiler: %v", err)
	}
	artifact, err := compiler.Compile(rootRelation)
	if err != nil {
		t.Fatalf("compile semantic View fixture: %v", err)
	}
	rootFields := []catalog.Field{
		{Name: "receipt_no", Type: "text", Collation: terminal.Fields[0].Collation,
			CollationVersion: terminal.Fields[0].CollationVersion},
		{Name: "business_unit", Type: "text", Collation: department.Collation,
			CollationVersion: department.CollationVersion},
		{Name: "amount", Type: "numeric"},
	}
	entityKey := []string{"receipt_no"}
	if aggregated {
		rootFields = []catalog.Field{
			{Name: "business_unit", Type: "text", Collation: department.Collation,
				CollationVersion: department.CollationVersion},
			{Name: "total_amount", Type: "numeric"},
			{Name: "request_count", Type: "bigint"},
		}
		entityKey = []string{"business_unit"}
	}
	root := catalog.Product{
		Name: semanticRuntimeRoot, Source: terminal.Source, ReportingView: rootRelation.String(),
		Sensitivity: domain.SensitivityHigh, Fields: rootFields, Scopes: []string{"business_unit"},
		AllowedOperators:  []string{"=", "<>", "<", "<=", ">", ">="},
		AllowedAggregates: []string{"count", "sum", "min", "max"},
		Snapshot:          terminal.Snapshot, EntityKey: entityKey,
		FactNamespace: "travel.semantic_expense_runtime", StableRelationRole: semanticRuntimeRoot,
		ViewContract: &catalog.ViewContract{
			ProfileVersion: catalog.ViewContractV1, DefinitionDigest: artifact.DefinitionDigest,
			DependencyDigest: artifact.DependencyDigest, CanonicalPlanDigest: artifact.CanonicalPlanDigest,
			InterfaceDigest: artifact.InterfaceDigest,
		},
	}
	logical.Products = append(logical.Products, root)
	service := &Service{catalog: logical}
	grant := control.TaskGrant{
		ApprovedProducts:   []string{root.Name},
		ApprovedColumns:    map[string][]string{root.Name: {"amount"}},
		MandatoryScope:     []byte(`{"business_unit":["销售部"]}`),
		SensitivityCeiling: string(domain.SensitivityHigh),
		Exposure: control.ExposureGrant{
			Limits:         control.ExposureLimits{ReleaseFacts: 100, InfluenceFacts: 500, OutcomeFacts: 10},
			ProfileVersion: exposure.ProfileV2,
		},
	}
	if aggregated {
		grant.ApprovedColumns[root.Name] = []string{"business_unit", "total_amount", "request_count"}
	}
	binding := &resolvedViewBinding{
		pendingViewBinding: pendingViewBinding{Digest: strings.Repeat("8", 64)},
		Expectation:        dataconnectorViewExpectationForTest(rootRelation, leaf, terminal.Name, registry.RevisionDigest),
		Artifacts:          map[string]viewcompiler.Artifact{root.Name: artifact},
	}
	return semanticRuntimeFixture{service: service, root: root, terminal: terminal,
		artifact: artifact, registry: registry, binding: binding, grant: grant}
}

func requestAndApproveSemanticRuntimeTask(t *testing.T, harness *gatewayHarness) string {
	t.Helper()
	requested := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", map[string]any{
		"objective":     "query a governed semantic expense View",
		"data_products": []string{semanticRuntimeRoot},
		"columns":       map[string][]string{semanticRuntimeRoot: {"amount"}},
		"scopes":        map[string]any{"business_unit": []any{"销售部"}},
	})
	taskID := requested["task_id"].(string)
	task, err := harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("load requested semantic task: %v", err)
	}
	if _, err := decodePersistedPending(task); err != nil {
		t.Fatalf("decode semantic pending task: %v", err)
	}
	draft := harness.approval.requests[len(harness.approval.requests)-1]
	submitted := oaCallbackEvent{
		EventID: "semantic-view-submit", TaskID: taskID, DraftID: task.ApprovalRef,
		Status: "submitted", Actor: harness.alice.Subject, OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest,
	}
	if response := sendGatewayCallback(t, harness, submitted, ""); response.Code != http.StatusOK {
		t.Fatalf("submit semantic task = %d %s", response.Code, response.Body.String())
	}
	core, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, harness.clock.value)
	if err != nil {
		t.Fatalf("build semantic task grant: %v", err)
	}
	coreDigest, err := approval.GrantCoreDigest(core)
	if err != nil {
		t.Fatalf("digest semantic task grant: %v", err)
	}
	receipt, err := approval.DemoReceiptSigner([]byte(harness.secret)).SignReceipt(approval.ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "semantic-view-receipt", TaskID: taskID,
		Decision: approval.ApprovalDecisionApprove, ManifestDigest: draft.ManifestDigest,
		ApprovedGrantDigest: coreDigest, ApproverID: "bob", IssuedAt: harness.clock.value,
	})
	if err != nil {
		t.Fatalf("sign semantic task approval: %v", err)
	}
	approved := oaCallbackEvent{
		EventID: "semantic-view-approve", TaskID: taskID, DraftID: task.ApprovalRef,
		Status: "approved", Actor: "bob", OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest, ApprovedGrant: &core, ApprovalReceipt: &receipt,
	}
	if response := sendGatewayCallback(t, harness, approved, ""); response.Code != http.StatusOK {
		t.Fatalf("approve semantic task = %d %s", response.Code, response.Body.String())
	}
	return taskID
}

func relationNameForTest(t *testing.T, qualified string) viewcompiler.RelationName {
	t.Helper()
	schema, name, ok := strings.Cut(qualified, ".")
	if !ok || schema == "" || name == "" {
		t.Fatalf("invalid relation name %q", qualified)
	}
	return viewcompiler.RelationName{Schema: schema, Name: name}
}

func columnNamesForTest(columns []dataconnector.Column) []string {
	result := make([]string, len(columns))
	for index, column := range columns {
		result[index] = column.Name
	}
	return result
}

func replaceCatalogProductForTest(t *testing.T, logical *catalog.Catalog, product catalog.Product) {
	t.Helper()
	for index := range logical.Products {
		if logical.Products[index].Name == product.Name {
			logical.Products[index] = product
			return
		}
	}
	t.Fatalf("catalog product %q is absent", product.Name)
}

func installSemanticRuntimeCatalogForTest(t *testing.T, logical *catalog.Catalog, fixture semanticRuntimeFixture) {
	t.Helper()
	if _, present := logical.LookupProduct(fixture.root.Name); present {
		t.Fatalf("catalog already contains semantic fixture %q", fixture.root.Name)
	}
	logical.Products = append(logical.Products, fixture.root)
	for _, scope := range fixture.service.catalog.Scopes {
		if scope.Name != "business_unit" {
			continue
		}
		if _, present := catalogScopeByName(logical.Scopes, scope.Name); present {
			t.Fatalf("catalog already contains semantic fixture scope %q", scope.Name)
		}
		logical.Scopes = append(logical.Scopes, scope)
		return
	}
	t.Fatal("semantic fixture lacks business_unit scope")
}

func requireViewQueryReasonForTest(t *testing.T, err error, reason string) {
	t.Helper()
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != apierr.CodeViewQueryUnsupported {
		t.Fatalf("error = %T %v, want %s", err, err, apierr.CodeViewQueryUnsupported)
	}
	if got, _ := toolErr.Details["reason"].(string); got != reason {
		t.Fatalf("reason = %q, want %q", got, reason)
	}
}

func dataconnectorViewExpectationForTest(root, leaf viewcompiler.RelationName, product, revision string) dataconnector.ViewRegistryExpectation {
	return dataconnector.ViewRegistryExpectation{
		Roots:                  []viewcompiler.RelationName{root},
		BaseProducts:           map[string]string{leaf.String(): product},
		ExpectedRevisionDigest: revision,
	}
}

func policyProductByName(grant sqlpolicy.Grant, name string) (sqlpolicy.ProductGrant, bool) {
	for _, product := range grant.Products {
		if product.LogicalName == name {
			return product, true
		}
	}
	return sqlpolicy.ProductGrant{}, false
}

func installSemanticRuntimeSnapshotRegistry(t *testing.T, service *Service) {
	t.Helper()
	registry, err := ordinal.NewRegistry()
	if err != nil {
		t.Fatalf("create snapshot registry: %v", err)
	}
	publications := append([]catalog.SnapshotPublication(nil), service.catalog.SnapshotPublications...)
	sort.Slice(publications, func(i, j int) bool { return publications[i].Name < publications[j].Name })
	for _, publication := range publications {
		path := filepath.Join("..", "..", "config", "snapshots", publication.Name+".json")
		file, openErr := os.Open(path)
		if openErr != nil {
			t.Fatalf("open snapshot input %s: %v", publication.Name, openErr)
		}
		input, decodeErr := snapshotbundle.DecodeCompilerInput(file)
		closeErr := file.Close()
		if decodeErr != nil {
			t.Fatalf("decode snapshot input %s: %v", publication.Name, decodeErr)
		}
		if closeErr != nil {
			t.Fatalf("close snapshot input %s: %v", publication.Name, closeErr)
		}
		bundle, compileErr := snapshotbundle.Compile(input)
		if compileErr != nil {
			t.Fatalf("compile snapshot input %s: %v", publication.Name, compileErr)
		}
		index, parseErr := ordinal.ParseHotDictionary(bundle.Hot, publication.ManifestDigest)
		if parseErr != nil {
			t.Fatalf("parse snapshot index %s: %v", publication.Name, parseErr)
		}
		if registerErr := registry.RegisterPublication(ordinal.PublicationKey{
			CatalogDigest: service.catalog.SHA256, PublicationName: publication.Name,
		}, publication.ManifestDigest, index); registerErr != nil {
			t.Fatalf("register snapshot index %s: %v", publication.Name, registerErr)
		}
	}
	service.snapshotRegistry = registry
}
