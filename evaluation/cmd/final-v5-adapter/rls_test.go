package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/finalv5rls"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
)

func TestRLSAdapterAcceptsOnlyFrozenCells(t *testing.T) {
	for _, cell := range []experiment.AdapterOperation{
		{ExperimentID: "rls", WorkloadID: "adaptive-100-v1", Scale: "100-queries", Mode: "rls"},
		{ExperimentID: "rls", WorkloadID: "adaptive-100-v1", Scale: "100-queries", Mode: "unlimited"},
		{ExperimentID: "rls", WorkloadID: "adaptive-100-v1", Scale: "100-queries", Mode: "bounded"},
		{ExperimentID: "rls", WorkloadID: "policy-denied-control", Scale: "single", Mode: "rls"},
		{ExperimentID: "rls", WorkloadID: "policy-denied-control", Scale: "single", Mode: "unlimited"},
		{ExperimentID: "rls", WorkloadID: "policy-denied-control", Scale: "single", Mode: "bounded"},
	} {
		if !validRLSCell(cell) {
			t.Fatalf("frozen cell rejected: %#v", cell)
		}
	}
	for _, cell := range []experiment.AdapterOperation{
		{ExperimentID: "attack", WorkloadID: "adaptive-100-v1", Scale: "100-queries", Mode: "rls"},
		{ExperimentID: "rls", WorkloadID: "adaptive-99", Scale: "100-queries", Mode: "rls"},
		{ExperimentID: "rls", WorkloadID: "adaptive-100-v1", Scale: "99-queries", Mode: "rls"},
		{ExperimentID: "rls", WorkloadID: "adaptive-100-v1", Scale: "100-queries", Mode: "novel"},
	} {
		if validRLSCell(cell) {
			t.Fatalf("unfrozen cell accepted: %#v", cell)
		}
	}
}

func TestRLSUnsupportedCellIsRetainedAsInvalid(t *testing.T) {
	operation := experiment.AdapterOperation{SchemaVersion: 1, CampaignID: "campaign", DeploymentID: "deployment-01", ExperimentID: "rls",
		CellID: "unsupported/single/rls", SampleID: "sample", Iteration: 1, OrderPosition: 1, RandomSeed: 20260801,
		PairID: "pair", PairedSystemOrder: "rls", RootGroupID: "rls", WorkloadID: "unsupported", Scale: "single", Mode: "rls"}
	sample := (&rlsAdapter{}).Execute(context.Background(), operation)
	if sample.Status != "invalid" || sample.ErrorCode != "unsupported_source_controlled_rls_cell" || sample.ExperimentID != "rls" {
		t.Fatalf("unsupported RLS cell was not retained as invalid: %+v", sample)
	}
}

func TestRLSPolicyControlKeepsForceRLSProbeSeparateFromExplicitRejection(t *testing.T) {
	manifest, err := finalv5rls.Load()
	if err != nil {
		t.Fatal(err)
	}
	adapter := &rlsAdapter{manifest: manifest, datasetSHA: finalv5rls.DatasetSHA256(manifest)}
	evidence, policy, authorization, err := adapter.policyFilterEvidence(rlsPolicyEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.OracleTrace) != 2 || len(evidence.OraclePrefixes) != 2 || evidence.SuccessfulQueries != 1 ||
		evidence.FirstRejectionIndex != 2 || evidence.UnrelatedAuthorizationDenials != 1 || evidence.NegativeControl == nil ||
		!evidence.NegativeControl.PolicyFiltered || policy.ID == authorization.ID ||
		policy.Variant != "force-rls-zero-row" || authorization.Variant != "unapproved-column" {
		t.Fatalf("policy/rejection evidence was conflated: evidence=%+v policy=%+v authorization=%+v", evidence, policy, authorization)
	}
}

func TestFailedRLSSampleRetainsPartialEvidence(t *testing.T) {
	diagnostics := captureAdapterDiagnostics(t)
	operation := experiment.AdapterOperation{SchemaVersion: 1, CampaignID: "campaign", DeploymentID: "deployment-01", ExperimentID: "rls",
		CellID: "adaptive-100-v1/100-queries/bounded", SampleID: "sample", Iteration: 1, OrderPosition: 1, RandomSeed: 20260801,
		PairID: "pair", PairedSystemOrder: "bounded", RootGroupID: "bounded", WorkloadID: "adaptive-100-v1", Scale: "100-queries", Mode: "bounded"}
	partial := baseSample(operation, "taskgate")
	partial.RLSVerification = &experiment.RLSVerificationEvidence{Version: rlsAdapterEvidenceVersion,
		Steps: []experiment.RLSStepEvidence{{Index: 1, StepID: "retained"}}}
	failed := failedRLSSample(operation, partial, errors.New("backend"))
	if failed.Status != "fail" || failed.ErrorCode != "rls_real_execution_failure" || failed.RLSVerification == nil ||
		len(failed.RLSVerification.Steps) != 1 || failed.RLSVerification.Steps[0].StepID != "retained" {
		t.Fatalf("partial RLS evidence was erased: %#v", failed)
	}
	failed = failedRLSSample(operation, partial, &rlsInvariantError{reason: "private validator detail"})
	if failed.Status != "fail" || failed.ErrorCode != "rls_invariant_violation" || strings.Contains(failed.Reason, "private validator detail") {
		t.Fatalf("invariant failure = %#v", failed)
	}
	if got := diagnostics.String(); !strings.Contains(got, "backend") || !strings.Contains(got, "private validator detail") {
		t.Fatalf("RLS backend and invariant causes were not retained in adapter stderr: %q", got)
	}
}

func TestRLSCatalogBindingIsExactlyPrecomputed(t *testing.T) {
	product, profile, outcome := rlsTaskgateBinding("bounded")
	if product != finalv5rls.BoundedProduct || profile != finalv5rls.BoundedProfile || outcome != finalv5rls.BoundedMaxOutcomeFacts {
		t.Fatal("bounded binding drifted")
	}
	product, profile, outcome = rlsTaskgateBinding("unlimited")
	if product != finalv5rls.UnlimitedProduct || profile != finalv5rls.UnlimitedProfile || outcome != 1000 {
		t.Fatal("unlimited binding drifted")
	}
}

func TestRLSBudgetErrorReasonIsDerivedOnlyForTheRealExposureCode(t *testing.T) {
	manifest, err := finalv5rls.Load()
	if err != nil {
		t.Fatal(err)
	}
	steps, err := manifest.Trace()
	if err != nil {
		t.Fatal(err)
	}
	stop, err := finalv5rls.ComputeBoundedStop(steps)
	if err != nil {
		t.Fatal(err)
	}
	if got := safeRLSBudgetErrorReason("EXPOSURE_BUDGET_EXHAUSTED", stop); got != "ROOT_DEPENDENCY_CEILING_EXCEEDED" {
		t.Fatalf("safe RLS budget reason = %q", got)
	}
	if got := safeRLSBudgetErrorReason("INTERNAL_ERROR", stop); got != "" {
		t.Fatalf("unregistered backend code acquired a synthetic reason: %q", got)
	}
}

// This regression crosses the independence boundary deliberately: the frozen
// oracle remains free of production imports, while this Adapter test lowers
// every corpus statement with the real SQL/queryplan V5 implementation and
// checks that its per-member influence and caller-atom accounting matches the
// oracle's cardinality model.
func TestRLSOracleMatchesProductionV5AccountingContract(t *testing.T) {
	manifest, err := finalv5rls.Load()
	if err != nil {
		t.Fatal(err)
	}
	steps, err := manifest.Trace()
	if err != nil {
		t.Fatal(err)
	}
	product := queryplan.Product{
		Name: finalv5rls.UnlimitedProduct,
		Columns: map[string]struct{}{
			"receipt_no": {}, "department": {}, "amount": {},
		},
		AllowedAggregates: map[string]struct{}{"count": {}},
		ColumnTypes: map[string]string{
			"receipt_no": "text", "department": "text", "amount": "numeric",
		},
		ColumnCollations: map[string]string{
			"receipt_no": "en_US.utf8", "department": "en_US.utf8",
		},
		CollationVersions: map[string]string{
			"receipt_no": "2.36", "department": "2.36",
		},
		SourceNamespace: "travel.expense_receipt", Snapshot: finalv5rls.DatasetID,
		StableRole: "expense_detail", StableEntityKey: []string{"receipt_no"}, RequiredEvidence: []string{"department"},
		LineageDigest: strings.Repeat("1", 64), SnapshotPublication: "expense-detail-v1",
		SidecarManifestDigest: strings.Repeat("2", 64),
	}
	products := map[string]queryplan.Product{product.Name: product}
	prefixes, err := finalv5oracle.EvaluatePrefixes(finalv5rls.OracleTrace(steps))
	if err != nil {
		t.Fatal(err)
	}
	productionRelease := map[string]struct{}{}
	productionDependency := map[string]struct{}{}
	productionOutcome := map[string]struct{}{}
	for _, step := range steps {
		lowered, lowerErr := sqllowering.Lower(step.LogicalSQL(product.Name), products)
		if lowerErr != nil {
			t.Fatalf("step %d production lowering: %v", step.Index, lowerErr)
		}
		compiled, compileErr := queryplan.CompileOrdinal(lowered.Plan, product)
		if compileErr != nil {
			t.Fatalf("step %d production ordinal compile: %v", step.Index, compileErr)
		}
		program := compiled.OrdinalProgram
		if len(program.Sources) != 1 || !ordinalProgramHasEvidenceField(program, "department") {
			t.Fatalf("step %d production program omitted mandatory evidence: %+v", step.Index, program.Sources)
		}

		members, memberErr := productionRLSMembers(lowered.Plan, manifest)
		if memberErr != nil {
			t.Fatalf("step %d production members: %v", step.Index, memberErr)
		}
		if step.Scalar != nil {
			if *step.Scalar != int64(len(members)) {
				t.Fatalf("step %d production aggregate members=%d, frozen scalar=%d", step.Index, len(members), *step.Scalar)
			}
		} else if len(members) != len(step.ExpectedRows) {
			t.Fatalf("step %d production members=%d, frozen rows=%d", step.Index, len(members), len(step.ExpectedRows))
		} else {
			for index, row := range members {
				if len(step.ExpectedRows[index]) != 1 || step.ExpectedRows[index][0] != row.ReceiptNo {
					t.Fatalf("step %d production member %d=%q, frozen row=%v", step.Index, index, row.ReceiptNo, step.ExpectedRows[index])
				}
			}
		}

		stepDependency := map[string]struct{}{}
		influenceFields := productionInfluenceFields(program)
		for _, row := range members {
			stepDependency["base-row\x00"+row.ReceiptNo] = struct{}{}
			for field := range influenceFields {
				value, valueErr := productionRLSFieldValue(row, field)
				if valueErr != nil {
					t.Fatalf("step %d production influence field: %v", step.Index, valueErr)
				}
				stepDependency["base-cell\x00"+row.ReceiptNo+"\x00"+field+"\x00"+value] = struct{}{}
			}
		}
		perMember := int64(1 + len(productionInfluenceFields(program))) // immutable base row plus referenced cells
		if got, want := int64(len(step.Oracle.Dependency)), int64(len(members))*perMember; got != want || int64(len(stepDependency)) != want {
			t.Fatalf("step %d oracle dependency=%d, production V5 contract=%d (%d members x %d)",
				step.Index, got, want, len(members), perMember)
		}

		stepRelease := map[string]struct{}{}
		memberIDs := make([]string, len(members))
		for index, row := range members {
			memberIDs[index] = row.ReceiptNo
		}
		if len(program.Groups) > 0 || len(program.Aggregates) > 0 {
			for _, output := range program.Visible {
				stepRelease["derived-output\x00"+output.OutputID+"\x00"+output.CanonicalExpression+"\x00"+
					finalv5rls.VerifiedResultSHA256(step)+"\x00"+strings.Join(memberIDs, ",")] = struct{}{}
			}
		} else {
			for _, row := range members {
				for _, visible := range program.Visible {
					value, valueErr := productionRLSFieldValue(row, visible.FieldID)
					if valueErr != nil {
						t.Fatalf("step %d production visible field: %v", step.Index, valueErr)
					}
					stepRelease["base-cell\x00"+row.ReceiptNo+"\x00"+visible.FieldID+"\x00"+value] = struct{}{}
				}
			}
		}
		if len(step.Oracle.Release) != len(stepRelease) {
			t.Fatalf("step %d oracle release=%d, production output facts=%d", step.Index, len(step.Oracle.Release), len(stepRelease))
		}

		footprint, footprintErr := queryplan.BuildPredicateFootprint(lowered.Plan, queryplan.PredicateBindings{
			CatalogSHA256: strings.Repeat("3", 64), Products: map[queryplan.PredicateProductKey]queryplan.Product{
				{Role: product.StableRole, Product: product.Name}: product,
			},
		}, strings.Repeat("4", 64), queryplan.DefaultPredicateLimits())
		if footprintErr != nil {
			t.Fatalf("step %d production V5 predicate footprint: %v", step.Index, footprintErr)
		}
		if got, want := len(step.Oracle.Outcome), footprint.UniqueAtomCount+1; got != want {
			t.Fatalf("step %d oracle outcome=%d, production caller atoms + composite=%d", step.Index, got, want)
		}
		stepOutcome := map[string]struct{}{}
		for _, atom := range footprint.Atoms {
			hash, hashErr := atom.Hash()
			if hashErr != nil {
				t.Fatalf("step %d production predicate atom: %v", step.Index, hashErr)
			}
			stepOutcome["predicate-atom\x00"+hash] = struct{}{}
		}
		normal, normalErr := queryplan.NormalizeV4(lowered.Plan, product)
		if normalErr != nil {
			t.Fatalf("step %d production V5 normal form: %v", step.Index, normalErr)
		}
		normalDigest, digestErr := normal.Digest()
		if digestErr != nil {
			t.Fatalf("step %d production V5 normal-form digest: %v", step.Index, digestErr)
		}
		stepOutcome["composite-outcome\x00"+normalDigest+"\x00"+productionRLSSemanticSet(stepRelease)+"\x00"+
			footprint.AtomSetSHA256+"\x00"+strconv.FormatInt(int64(len(step.ExpectedRows)), 10)] = struct{}{}

		beforeRelease, beforeDependency, beforeOutcome := len(productionRelease), len(productionDependency), len(productionOutcome)
		mergeRLSProductionMembers(productionRelease, stepRelease)
		mergeRLSProductionMembers(productionDependency, stepDependency)
		mergeRLSProductionMembers(productionOutcome, stepOutcome)
		prefix := prefixes[step.Index-1]
		if int64(len(productionRelease)) != prefix.Release.Cardinality ||
			int64(len(productionDependency)) != prefix.Dependency.Cardinality ||
			int64(len(productionOutcome)) != prefix.Outcome.Cardinality {
			t.Fatalf("step %d production prefix=%d/%d/%d, oracle prefix=%d/%d/%d",
				step.Index, len(productionRelease), len(productionDependency), len(productionOutcome),
				prefix.Release.Cardinality, prefix.Dependency.Cardinality, prefix.Outcome.Cardinality)
		}
		if step.Index > 1 {
			previous := prefixes[step.Index-2]
			productionNovel := len(productionRelease) != beforeRelease || len(productionDependency) != beforeDependency || len(productionOutcome) != beforeOutcome
			oracleNovel := prefix.Release.SetSHA256 != previous.Release.SetSHA256 ||
				prefix.Dependency.SetSHA256 != previous.Dependency.SetSHA256 || prefix.Outcome.SetSHA256 != previous.Outcome.SetSHA256
			if productionNovel != oracleNovel {
				t.Fatalf("step %d production/oracle novelty=%t/%t", step.Index, productionNovel, oracleNovel)
			}
		}
	}

	if len(steps[0].Oracle.Dependency) != 2 || len(steps[0].Oracle.Outcome) != 2 ||
		len(steps[36].Oracle.Dependency) != 18 || len(steps[36].Oracle.Outcome) != 2 {
		t.Fatal("representative production-vs-oracle cardinalities drifted")
	}
}

func ordinalProgramHasEvidenceField(program queryplan.OrdinalProgram, field string) bool {
	for _, source := range program.Sources {
		for _, binding := range source.EvidenceFields {
			if binding.FieldID == field {
				return true
			}
		}
	}
	return false
}

func productionInfluenceFields(program queryplan.OrdinalProgram) map[string]struct{} {
	fields := map[string]struct{}{}
	for _, source := range program.Sources {
		for _, predicate := range source.LeafPredicates {
			fields[predicate.Field.FieldID] = struct{}{}
		}
	}
	for _, predicate := range program.OuterPredicates {
		fields[predicate.Field.FieldID] = struct{}{}
	}
	if len(program.Groups) == 0 && len(program.Aggregates) == 0 {
		for _, visible := range program.Visible {
			if visible.Kind == "field" {
				fields[visible.FieldID] = struct{}{}
			}
		}
	}
	for _, group := range program.Groups {
		fields[group.Field.FieldID] = struct{}{}
	}
	for _, aggregate := range program.Aggregates {
		if aggregate.InputKind == "field" {
			fields[aggregate.Input.FieldID] = struct{}{}
		}
	}
	return fields
}

func productionRLSMembers(plan queryplan.QueryPlan, manifest finalv5rls.Manifest) ([]finalv5rls.FixtureRow, error) {
	rows := make([]finalv5rls.FixtureRow, 0, len(manifest.Rows))
	for _, row := range manifest.Rows {
		if row.Department != manifest.PolicyDepartment {
			continue
		}
		matches := true
		for _, filter := range plan.Filters {
			var err error
			matches, err = productionRLSFilterMatches(row, filter)
			if err != nil {
				return nil, err
			}
			if !matches {
				break
			}
		}
		if matches {
			rows = append(rows, row)
		}
	}
	for _, order := range plan.OrderBy {
		if order.Column != "receipt_no" || !strings.EqualFold(order.Direction, "asc") {
			return nil, fmt.Errorf("unsupported frozen RLS order %s %s", order.Column, order.Direction)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ReceiptNo < rows[j].ReceiptNo })
	if plan.Offset < 0 || plan.Offset > len(rows) || plan.Limit < 0 {
		return nil, fmt.Errorf("invalid frozen RLS pagination limit=%d offset=%d members=%d", plan.Limit, plan.Offset, len(rows))
	}
	rows = rows[plan.Offset:]
	if plan.Limit > 0 && plan.Limit < len(rows) {
		rows = rows[:plan.Limit]
	}
	return rows, nil
}

func productionRLSFilterMatches(row finalv5rls.FixtureRow, filter queryplan.Filter) (bool, error) {
	op := strings.ToUpper(strings.TrimSpace(filter.Op))
	switch filter.Column {
	case "receipt_no":
		value := fmt.Sprint(filter.Value)
		switch op {
		case "=":
			return row.ReceiptNo == value, nil
		case "<>", "!=":
			return row.ReceiptNo != value, nil
		default:
			return false, fmt.Errorf("unsupported receipt filter operator %q", filter.Op)
		}
	case "amount":
		value, err := strconv.ParseInt(fmt.Sprint(filter.Value), 10, 64)
		if err != nil {
			return false, fmt.Errorf("invalid amount filter literal %v", filter.Value)
		}
		switch op {
		case "=":
			return row.Amount == value, nil
		case ">=":
			return row.Amount >= value, nil
		case ">":
			return row.Amount > value, nil
		case "<=":
			return row.Amount <= value, nil
		case "<":
			return row.Amount < value, nil
		default:
			return false, fmt.Errorf("unsupported amount filter operator %q", filter.Op)
		}
	default:
		return false, fmt.Errorf("unsupported frozen RLS filter field %q", filter.Column)
	}
}

func productionRLSFieldValue(row finalv5rls.FixtureRow, field string) (string, error) {
	switch field {
	case "receipt_no":
		return row.ReceiptNo, nil
	case "department":
		return row.Department, nil
	case "amount":
		return strconv.FormatInt(row.Amount, 10), nil
	default:
		return "", fmt.Errorf("unsupported frozen RLS production field %q", field)
	}
}

func mergeRLSProductionMembers(target, source map[string]struct{}) {
	for member := range source {
		target[member] = struct{}{}
	}
}

func productionRLSSemanticSet(members map[string]struct{}) string {
	values := make([]string, 0, len(members))
	for member := range members {
		values = append(values, member)
	}
	sort.Strings(values)
	return strings.Join(values, "\x01")
}
