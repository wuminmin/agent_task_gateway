package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

// This file is the differential harness the target-preparation extraction moves
// behind.
//
// The legacy derivation is invoked ONLY from here. Production keeps exactly one
// implementation at all times: before the extraction it is the Gateway's, after
// it is internal/physicalquery's. A feature flag or a fallback that chose
// between them would put both in the running system, which is the outcome the
// whole exercise exists to avoid -- two implementations that agree in tests and
// diverge in the one execution nobody reproduced.
//
// Old-vs-new equality is necessary and not sufficient. A new implementation that
// faithfully reproduces a legacy defect passes it. So every case is also checked
// against evidence that was not produced by either implementation: the Catalog
// and its snapshot publications, the policy engine, and -- where the case has a
// physical relation to run against -- PostgreSQL itself.

// ------------------------------------------------------------ observed shape

// preparationShape is everything one preparation determines.
//
// It holds values rather than digests wherever a value is available. A digest
// comparison can only say that two things differ; comparing the ordinal program,
// the dictionary members and the grants themselves says which member differs,
// and that is what makes a parity failure diagnosable rather than merely red.
type preparationShape struct {
	VisibleSQL   string
	CompanionSQL string

	VisibleFields    []string
	FactFields       []string
	ProvenanceFields []string
	MeteringColumns  []string

	Grouped              bool
	ExpandedEvidence     bool
	UsesExpandedEvidence bool

	PlanDigest              string
	NormalFormSHA256        string
	AlgebraNormalFormSHA256 string

	OrdinalProgram       *queryplan.OrdinalProgram
	DictionarySetDigest  string
	DictionarySetMembers []ordinal.DictionarySetMember
	SidecarGrants        []sqlpolicy.ProductGrant
	SidecarGrantsSHA256  string
	SourcePublications   map[string]string
	EstimatedBaseFacts   uint64

	PredicateFootprint *queryplan.PredicateFootprint

	ViewBindingDigest    string
	ViewRegistryRevision string

	// PolicyGrant is the authorization the statements are admitted against:
	// the task grant, widened by the exposure metering columns and then by the
	// ordinal sidecars, exactly as production widens it.
	PolicyGrant sqlpolicy.Grant

	// The bindings the execution binding computes from all of the above.
	PreparedOperationSHA256 string
	VisibleTargetSHA256     string
	CompanionTargetSHA256   string
}

// requireSameShape names every member two preparations disagree on.
//
// Reporting only "the shapes differ" would make a parity failure a bisect
// exercise. The members are compared individually and every difference is
// reported, not just the first: a derivation that moved one thing usually moved
// several, and seeing all of them is what identifies the cause.
func requireSameShape(t *testing.T, want, got preparationShape) {
	t.Helper()
	var differences []string
	compare := func(name string, left, right any) {
		if !reflect.DeepEqual(left, right) {
			differences = append(differences, fmt.Sprintf("%s:\n  legacy = %s\n  new    = %s",
				name, shapeMember(left), shapeMember(right)))
		}
	}
	compare("visible SQL bytes", want.VisibleSQL, got.VisibleSQL)
	compare("companion SQL bytes", want.CompanionSQL, got.CompanionSQL)
	compare("visible field ordering", want.VisibleFields, got.VisibleFields)
	compare("fact field ordering", want.FactFields, got.FactFields)
	compare("provenance field ordering", want.ProvenanceFields, got.ProvenanceFields)
	compare("metering columns", want.MeteringColumns, got.MeteringColumns)
	compare("grouped", want.Grouped, got.Grouped)
	compare("expanded evidence", want.ExpandedEvidence, got.ExpandedEvidence)
	compare("uses expanded evidence", want.UsesExpandedEvidence, got.UsesExpandedEvidence)
	compare("plan digest", want.PlanDigest, got.PlanDigest)
	compare("normal form digest", want.NormalFormSHA256, got.NormalFormSHA256)
	compare("algebra normal form digest", want.AlgebraNormalFormSHA256, got.AlgebraNormalFormSHA256)
	compare("ordinal program", want.OrdinalProgram, got.OrdinalProgram)
	compare("dictionary set digest", want.DictionarySetDigest, got.DictionarySetDigest)
	compare("dictionary set members", want.DictionarySetMembers, got.DictionarySetMembers)
	compare("sidecar grants", want.SidecarGrants, got.SidecarGrants)
	compare("sidecar grants digest", want.SidecarGrantsSHA256, got.SidecarGrantsSHA256)
	compare("source publications", want.SourcePublications, got.SourcePublications)
	compare("estimated base facts", want.EstimatedBaseFacts, got.EstimatedBaseFacts)
	compare("predicate footprint", want.PredicateFootprint, got.PredicateFootprint)
	compare("view binding digest", want.ViewBindingDigest, got.ViewBindingDigest)
	compare("view registry revision", want.ViewRegistryRevision, got.ViewRegistryRevision)
	compare("policy grant", want.PolicyGrant, got.PolicyGrant)
	compare("prepared operation binding", want.PreparedOperationSHA256, got.PreparedOperationSHA256)
	compare("visible target binding", want.VisibleTargetSHA256, got.VisibleTargetSHA256)
	compare("companion target binding", want.CompanionTargetSHA256, got.CompanionTargetSHA256)
	if len(differences) > 0 {
		t.Fatalf("two preparations of one operation disagree on %d member(s):\n%s",
			len(differences), strings.Join(differences, "\n"))
	}
}

func shapeMember(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%#v", value)
	}
	if len(encoded) > 400 {
		return string(encoded[:400]) + "... (truncated)"
	}
	return string(encoded)
}

// shapeSHA256 is the whole shape's identity.
//
// The mutation suite uses it: an input change must move this value or fail
// closed, and comparing one digest is what lets that suite state the property
// once rather than per member.
func (shape preparationShape) shapeSHA256(t *testing.T) string {
	t.Helper()
	canonical, err := approval.CanonicalJSON(shape)
	if err != nil {
		t.Fatalf("canonicalize preparation shape: %v", err)
	}
	sum := sha256.Sum256(append([]byte("TASKGATE-PARITY-PREPARATION-SHAPE\x00"), canonical...))
	return hex.EncodeToString(sum[:])
}

// ------------------------------------------------------- the legacy derivation

// legacyPrepareForParity runs the Gateway's own target preparation exactly as
// executeSQL runs it, and reads off every property the extraction must preserve.
//
// The sequence below is not a reconstruction: it is the same four calls in the
// same order that internal/gateway/query.go makes between prepareTaskPlan and
// derivePhysicalQuery. Anything that diverged here would make the harness prove
// parity with something production does not do.
func legacyPrepareForParity(ctx context.Context, service *Service, task control.Task,
	grant control.TaskGrant, plan queryplan.QueryPlan, evidence datasourceEvidence,
	manifestDigest, grantDigest string) (preparationShape, error) {
	prepared, err := service.prepareTaskPlan(ctx, task, grant, plan)
	if err != nil {
		return preparationShape{}, err
	}
	return legacyShapeOf(service, prepared, plan, grant, evidence, manifestDigest, grantDigest)
}

// legacyShapeOf reads every property off one prepared plan.
//
// It is separate from the call above because the semantic View path reaches
// prepareSemanticViewPlan after the Control Store has resolved the task's view
// binding, and preparation itself performs no store I/O. Sharing this half is
// what keeps the View case reading the same properties as every other one.
func legacyShapeOf(service *Service, prepared preparedQueryPlan, plan queryplan.QueryPlan,
	grant control.TaskGrant, evidence datasourceEvidence,
	manifestDigest, grantDigest string) (preparationShape, error) {
	policy, err := service.policyGrant(grant)
	if err != nil {
		return preparationShape{}, err
	}
	context := prepared.Exposure
	if context != nil {
		if policy, err = context.extendGrant(policy); err != nil {
			return preparationShape{}, err
		}
		if context.ordinal != nil {
			if policy, err = extendOrdinalPolicyGrant(policy, context.ordinal.SidecarGrants); err != nil {
				return preparationShape{}, err
			}
		}
	}

	shape := preparationShape{VisibleSQL: prepared.SQL, PolicyGrant: policy}
	if context == nil {
		// A plain query builds no exposure context, so its projection is not held
		// there. It is still a projection the Gateway computes and uses:
		// queryPlanResultNames is what names the columns of the stored result, so
		// reading it here compares the extraction against what production actually
		// projects rather than against a nil the legacy shape merely happens to
		// leave. The binding below is not built at all -- executionBindingApplies
		// refuses one without a plan digest.
		shape.VisibleFields = queryPlanResultNames(plan)
		return shape, nil
	}
	shape.CompanionSQL = context.provenanceSQL
	shape.VisibleFields = append([]string(nil), context.visibleFields...)
	shape.FactFields = append([]string(nil), context.factFields...)
	shape.ProvenanceFields = append([]string(nil), context.provenanceFields...)
	shape.MeteringColumns = append([]string(nil), context.meteringColumns...)
	shape.Grouped = context.grouped
	shape.ExpandedEvidence = context.expandedEvidence
	shape.UsesExpandedEvidence = context.usesExpandedEvidence()
	shape.PlanDigest = context.planDigest
	shape.ViewBindingDigest = context.viewBindingDigest
	shape.ViewRegistryRevision = context.viewRegistryRevision
	shape.PredicateFootprint = context.predicateFootprint
	if context.normalForm != nil {
		digest, digestErr := context.normalForm.Digest()
		if digestErr != nil {
			return preparationShape{}, digestErr
		}
		shape.NormalFormSHA256 = digest
	}
	if context.algebraNormalForm != nil {
		shape.AlgebraNormalFormSHA256 = context.algebraNormalForm.SHA256
	}
	if context.ordinal != nil {
		program := context.ordinal.Program
		shape.OrdinalProgram = &program
		shape.DictionarySetDigest = context.ordinal.DictionarySetDigest
		shape.DictionarySetMembers = append([]ordinal.DictionarySetMember(nil),
			context.ordinal.DictionarySet.Members...)
		shape.SidecarGrants = append([]sqlpolicy.ProductGrant(nil), context.ordinal.SidecarGrants...)
		shape.SidecarGrantsSHA256 = sidecarGrantsDigest(context.ordinal.SidecarGrants)
		shape.EstimatedBaseFacts = context.ordinal.EstimatedBaseFacts
		// The alias-to-publication mapping is not held as a member today; it is
		// implicit in the resolved index map. Reading it out here is what lets the
		// extracted preparation carry it explicitly without changing behaviour.
		shape.SourcePublications = make(map[string]string, len(program.Sources))
		for _, source := range program.Sources {
			shape.SourcePublications[source.SourceAlias] = source.SidecarBinding.PublicationID
		}
	}

	operation, err := newPreparedOperation(preparedOperation{
		PlanSHA256:                 context.planDigest,
		ExposureProfileVersion:     grant.Exposure.ProfileVersion,
		GrantDigest:                grantDigest,
		ManifestDigest:             manifestDigest,
		CatalogSHA256:              service.catalog.SHA256,
		DatasourceID:               evidence.DatasourceID,
		SchemaDigest:               evidence.SchemaDigest,
		ViewBindingDigest:          context.viewBindingDigest,
		OrdinalDictionarySetSHA256: shape.DictionarySetDigest,
		SidecarGrantsSHA256:        shape.SidecarGrantsSHA256,
		VisibleSQL:                 prepared.SQL,
		CompanionSQL:               context.provenanceSQL,
	})
	if err != nil {
		return preparationShape{}, err
	}
	shape.PreparedOperationSHA256 = operation.digest()
	shape.VisibleTargetSHA256 = operation.visibleTargetSHA
	shape.CompanionTargetSHA256 = operation.companionTargetSHA
	return shape, nil
}

// ------------------------------------------------------------------- fixtures

// parityService is a Service carrying only what preparation reads.
//
// Preparation performs no database I/O and touches no Control state, so the
// store is deliberately absent: a case that silently began to need one would
// fail here rather than pass against a store the extracted package will not
// have.
func parityService(t *testing.T, withRegistry bool) *Service {
	t.Helper()
	loaded, err := catalog.Load(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	service := &Service{catalog: loaded}
	if withRegistry {
		service.snapshotRegistry = paritySnapshotRegistry(t, loaded)
	}
	return service
}

// parityRegistryOnce holds the compiled registry for the whole test binary.
//
// Compiling the publications means scanning tens of thousands of source rows and
// building each hot dictionary, which is real work; the parity cases resolve the
// same immutable artifacts, so doing it once is the difference between a suite
// that runs in a minute and one that runs in twenty. The registry is only read
// by preparation, so sharing it cannot let one case observe another's state.
var (
	parityRegistryOnce  sync.Once
	parityRegistryValue *ordinal.Registry
	parityRegistrySkip  string
)

// paritySnapshotRegistry compiles the committed snapshot bundles and registers
// the verified indexes, without a Control Store.
//
// It is the store-free twin of installCatalogV4SnapshotRegistry. The store is
// not needed to prepare: PutOrdinalSnapshotPublication is what publishes the
// artifact for later observation, and preparation only resolves it.
func paritySnapshotRegistry(t *testing.T, loaded *catalog.Catalog) *ordinal.Registry {
	t.Helper()
	parityRegistryOnce.Do(func() {
		// The skip is captured rather than taken here: t.Skip inside a sync.Once
		// would end one case and leave every later one to find a nil registry.
		if strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN")) == "" {
			for _, publication := range loaded.SnapshotPublications {
				if parityPublicationCarriesRows(t, publication.Name) {
					continue
				}
				parityRegistrySkip = fmt.Sprintf("publication %s carries no committed source rows; "+
					"set BUSINESS_TEST_POSTGRES_DSN (scripts/db-test-env.sh) so the parity registry "+
					"can be materialized", publication.Name)
				return
			}
		}
		parityRegistryValue = buildParitySnapshotRegistry(t, loaded)
	})
	if parityRegistrySkip != "" {
		t.Skip(parityRegistrySkip)
	}
	if parityRegistryValue == nil {
		t.Fatal("the parity snapshot registry failed to build")
	}
	return parityRegistryValue
}

func parityPublicationCarriesRows(t *testing.T, name string) bool {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "..", "config", "snapshots", name+".json"))
	if err != nil {
		t.Fatalf("open snapshot compiler input %s: %v", name, err)
	}
	defer func() { _ = file.Close() }()
	input, err := snapshotbundle.DecodeCompilerInput(file)
	if err != nil {
		t.Fatalf("decode snapshot compiler input %s: %v", name, err)
	}
	return len(input.Snapshot.Rows) > 0
}

func buildParitySnapshotRegistry(t *testing.T, loaded *catalog.Catalog) *ordinal.Registry {
	t.Helper()
	registry, err := ordinal.NewRegistry()
	if err != nil {
		t.Fatalf("create snapshot registry: %v", err)
	}
	for _, publication := range loaded.SnapshotPublications {
		path := filepath.Join("..", "..", "config", "snapshots", publication.Name+".json")
		file, openErr := os.Open(path)
		if openErr != nil {
			t.Fatalf("open snapshot compiler input %s: %v", publication.Name, openErr)
		}
		input, decodeErr := snapshotbundle.DecodeCompilerInput(file)
		closeErr := file.Close()
		if decodeErr != nil {
			t.Fatalf("decode snapshot compiler input %s: %v", publication.Name, decodeErr)
		}
		if closeErr != nil {
			t.Fatalf("close snapshot compiler input %s: %v", publication.Name, closeErr)
		}
		if len(input.Snapshot.Rows) == 0 {
			input = scanLiveSnapshotRows(t, input, publication.Name)
		}
		bundle, compileErr := snapshotbundle.Compile(input)
		if compileErr != nil {
			t.Fatalf("compile snapshot publication %s: %v", publication.Name, compileErr)
		}
		parsed, parseErr := ordinal.ParseHotDictionary(bundle.Hot, publication.ManifestDigest)
		if parseErr != nil {
			t.Fatalf("parse snapshot publication %s: %v", publication.Name, parseErr)
		}
		assertCompiledBundleMatchesExpectedDigests(t, publication.Name, parsed.Manifest(),
			parsed.ManifestDigest(), input.ExpectedDigests)
		if err := registry.RegisterPublication(ordinal.PublicationKey{
			CatalogDigest: loaded.SHA256, PublicationName: publication.Name,
		}, publication.ManifestDigest, parsed); err != nil {
			t.Fatalf("register snapshot publication %s: %v", publication.Name, err)
		}
	}
	return registry
}

// parityCase is one named shape the extraction must preserve.
type parityCase struct {
	name string
	// profile is the exposure profile the task grant carries. Empty disables
	// exposure entirely, which is the single-query non-exposure shape.
	profile  string
	products []string
	columns  map[string][]string
	// scope is the task's mandatory scope. Nil is no scope at all, which is a
	// distinct shape from a scope that happens to be satisfied.
	scope json.RawMessage
	plan  queryplan.QueryPlan
	// needsRegistry is true for the profiles that resolve snapshot publications.
	needsRegistry bool
	// executable is true when the visible statement addresses a relation the
	// business database actually carries, so PostgreSQL can be used as an
	// independent oracle for it.
	executable bool
}

func (test parityCase) grant() control.TaskGrant {
	grant := control.TaskGrant{
		TaskID: "parity-" + test.name, Subject: "alice", Purpose: "preparation parity",
		ApprovedProducts: append([]string(nil), test.products...),
		ApprovedColumns:  test.columns, MandatoryScope: test.scope,
		CatalogVersion: "parity", Budget: control.BudgetLimits{Queries: 10, Rows: 500, DBMS: 30000},
	}
	if test.profile != "" {
		grant.Exposure = control.ExposureGrant{
			ProfileVersion: test.profile,
			Limits: control.ExposureLimits{
				ReleaseFacts: 1_000_000, InfluenceFacts: 1_000_000, OutcomeFacts: 1_000_000,
			},
		}
		if test.profile == exposure.ProfileV5 {
			grant.Exposure.PredicateFootprint = &control.PredicateFootprintLimitsV1{
				Version: domain.PredicateFootprintV1, MaxRawLiteralsPerQuery: 1000,
				MaxUniqueAtomsPerQuery: 32, MaxAtomPayloadBytes: 4096,
				MaxTotalAtomPayloadBytes: 36864,
			}
		}
	}
	return grant
}

// The Catalog requires a mandatory scope value for every scope its products
// declare, so "no scope" is not a preparable shape for any of them. What varies
// between cases is therefore the scope's content, and the scope key differs by
// product family because the Catalog says it does.
const (
	paritySalesScope     = `{"department":["销售部"]}`
	parityTwoDeptScope   = `{"department":["销售部","研发部"]}`
	parityPartitionScope = `{"partition_key":["p0"]}`
	parityCategoryScope  = `{"category":["travel"]}`
)

var (
	summaryColumns = []string{"month", "department", "expense_type", "total_amount", "request_count"}
	detailColumns  = []string{"receipt_no", "employee_no", "department", "expense_date",
		"expense_type", "amount", "city", "purpose", "status"}
	provsqlOrderColumns = []string{"orderkey", "status", "partition_key"}
	resultHeavyColumns  = []string{"row_id", "category", "amount", "event_date", "sequence_no"}
)

// parityCases is the named shape set.
//
// Each entry is here because it exercises an independent property of the
// derivation, not to reach a count. The earlier plan called this twelve shapes
// while naming more properties than that; the names are what matters, so a case
// may only be removed by showing that the property it covers is covered
// elsewhere.
func parityCases() []parityCase {
	simple := queryplan.QueryPlan{Product: "expense_summary",
		Columns: []string{"month", "total_amount"}, Limit: 50}
	grouped := queryplan.QueryPlan{Product: "expense_summary",
		Columns: []string{"month"}, GroupBy: []string{"month"},
		Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "total_amount", Alias: "total"}},
		Limit:      50}
	// The relational plans deliberately carry no limit: pagination is outside the
	// online Join/Union fragment, so a limit here would make these cases fail to
	// compile for a reason unrelated to the property each is named for.
	join := queryplan.QueryPlan{From: &queryplan.From{Join: &queryplan.Join{
		Left:  queryplan.Scan{Product: "expense_detail", Role: "expense_detail"},
		Right: queryplan.Scan{Product: "expense_summary", Role: "expense_summary"},
		On: []queryplan.JoinPredicate{{
			Left: "expense_detail.department", Right: "expense_summary.department"}},
	}}, Columns: []string{"expense_detail.receipt_no", "expense_summary.total_amount"}}
	// Two forms of the same UNION shape. The branch-filtered one is what a V4
	// duplicate-product binding is checked with; the unfiltered one is what the
	// V5 Union case uses, because a branch-role-qualified predicate field cannot
	// be prepared under V5 today -- see
	// TestUnionBranchFilteredPredicatesFailClosedUnderV5, which pins that gap.
	branchFilteredUnion := queryplan.QueryPlan{From: &queryplan.From{UnionDistinct: &queryplan.UnionDistinct{
		Role: "expense_summary", Columns: []string{"department", "month"},
		Left: queryplan.Scan{Product: "expense_summary", Role: "left_branch",
			Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "机票"}}},
		Right: queryplan.Scan{Product: "expense_summary", Role: "right_branch",
			Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "酒店"}}},
	}}, Columns: []string{"expense_summary.department"}}
	union := queryplan.QueryPlan{From: &queryplan.From{UnionDistinct: &queryplan.UnionDistinct{
		Role: "expense_summary", Columns: []string{"department", "month"},
		Left:  queryplan.Scan{Product: "expense_summary", Role: "left_branch"},
		Right: queryplan.Scan{Product: "expense_summary", Role: "right_branch"},
	}}, Columns: []string{"expense_summary.department"}}

	summaryOnly := map[string][]string{"expense_summary": summaryColumns}
	bothProducts := map[string][]string{
		"expense_summary": summaryColumns, "expense_detail": detailColumns,
	}

	return []parityCase{
		{
			name: "simple_non_grouped_product", profile: exposure.ProfileV2,
			products: []string{"expense_summary"}, columns: summaryOnly,
			plan: simple, executable: true,
		},
		{
			name: "grouped_aggregate", profile: exposure.ProfileV2,
			products: []string{"expense_summary"}, columns: summaryOnly,
			plan: grouped, executable: true,
		},
		{
			name: "single_query_non_exposure", profile: "",
			products: []string{"expense_summary"}, columns: summaryOnly,
			plan: simple, executable: true,
		},
		{
			name: "ordinal_v4", profile: exposure.ProfileV4,
			products: []string{"expense_summary"}, columns: summaryOnly,
			plan: simple, needsRegistry: true, executable: true,
		},
		{
			name: "ordinal_v5", profile: exposure.ProfileV5,
			products: []string{"expense_summary"}, columns: summaryOnly,
			plan: simple, needsRegistry: true, executable: true,
		},
		{
			// A grouped plan sets usesExpandedEvidence, which is what moves the
			// companion's evidence-row budget away from the visible row limit.
			name: "expanded_evidence", profile: exposure.ProfileV5,
			products: []string{"expense_summary"}, columns: summaryOnly,
			plan: grouped, needsRegistry: true, executable: true,
		},
		{
			name: "non_expanded_evidence", profile: exposure.ProfileV5,
			products: []string{"expense_summary"}, columns: summaryOnly,
			plan: simple, needsRegistry: true, executable: true,
		},
		{
			name: "relational_join", profile: exposure.ProfileV5,
			products: []string{"expense_summary", "expense_detail"}, columns: bothProducts,
			plan: join, needsRegistry: true, executable: true,
		},
		{
			name: "relational_union", profile: exposure.ProfileV5,
			products: []string{"expense_summary"}, columns: summaryOnly,
			plan: union, needsRegistry: true, executable: true,
		},
		{
			// One product appearing twice must resolve to one sidecar grant and one
			// dictionary member, not two -- the union above reaches the same
			// publication through both branches.
			name: "duplicate_product_binding", profile: exposure.ProfileV4,
			products: []string{"expense_summary"}, columns: summaryOnly,
			plan: branchFilteredUnion, needsRegistry: true, executable: true,
		},
		{
			// A wider scope is a different authorization, so it must be a
			// different preparation: the predicate reaches both the rendered SQL
			// and the policy grant.
			name: "mandatory_scope", profile: exposure.ProfileV5,
			products: []string{"expense_summary"}, columns: summaryOnly,
			scope: json.RawMessage(parityTwoDeptScope),
			plan:  simple, needsRegistry: true, executable: true,
		},
		{
			name: "provsql_taskgate", profile: exposure.ProfileV5,
			products: []string{"provsql_orders"},
			columns:  map[string][]string{"provsql_orders": provsqlOrderColumns},
			scope:    json.RawMessage(parityPartitionScope),
			plan: queryplan.QueryPlan{Product: "provsql_orders",
				Columns: []string{"orderkey", "status"}, Limit: 25},
			needsRegistry: true, executable: true,
		},
		{
			name: "result_heavy_100x4", profile: exposure.ProfileV5,
			products: []string{"final_v5_result_heavy"},
			columns:  map[string][]string{"final_v5_result_heavy": resultHeavyColumns},
			scope:    json.RawMessage(parityCategoryScope),
			plan: queryplan.QueryPlan{Product: "final_v5_result_heavy",
				Columns: []string{"row_id", "amount", "event_date", "sequence_no"}, Limit: 100},
			needsRegistry: true, executable: true,
		},
	}
}

// withDefaultScope fills in the scope a case did not name.
//
// Every Catalog product used here declares a mandatory scope, and a task grant
// that carries none is refused before any statement is compiled. Defaulting is
// what keeps a case's definition about the property it is named for rather than
// about restating an authorization requirement.
func (test parityCase) withDefaultScope() parityCase {
	if len(test.scope) > 0 {
		return test
	}
	switch {
	case len(test.products) == 1 && strings.HasPrefix(test.products[0], "provsql_"):
		test.scope = json.RawMessage(parityPartitionScope)
	case len(test.products) == 1 && test.products[0] == "final_v5_result_heavy":
		test.scope = json.RawMessage(parityCategoryScope)
	default:
		test.scope = json.RawMessage(paritySalesScope)
	}
	return test
}

// resolveParityCase adjusts a case to the Catalog it will actually run against.
//
// The approved-column lists above name what the products are expected to carry.
// Rather than trusting that, each is intersected with the Catalog's own fields,
// so a case cannot silently approve a column the Catalog does not declare and
// then fail in compilation for a reason unrelated to the property under test.
func resolveParityCase(t *testing.T, service *Service, test parityCase) parityCase {
	t.Helper()
	resolved := test.withDefaultScope()
	resolved.columns = make(map[string][]string, len(test.columns))
	for name, columns := range test.columns {
		product, present := service.catalog.LookupProduct(name)
		if !present {
			t.Fatalf("case %s names product %q, which this Catalog does not declare", test.name, name)
		}
		declared := stringSetFromSlice(product.FieldNames())
		approved := make([]string, 0, len(columns))
		for _, column := range columns {
			if _, found := declared[column]; found {
				approved = append(approved, column)
			}
		}
		if len(approved) == 0 {
			t.Fatalf("case %s approves no column the Catalog declares for %q", test.name, name)
		}
		resolved.columns[name] = approved
	}
	return resolved
}

func parityEvidence(t *testing.T, service *Service, products []string) (datasourceEvidence, string, string) {
	t.Helper()
	source, err := service.catalogSourceForProducts(products)
	if err != nil {
		t.Fatalf("resolve datasource evidence: %v", err)
	}
	// The manifest and grant digests are not derived here: preparation reads them
	// as opaque identities, so any fixed pair proves the binding covers them
	// without pretending this harness reproduced an approval.
	return datasourceEvidence{DatasourceID: source.DatasourceID, SchemaDigest: source.SchemaDigest},
		strings.Repeat("a", 64), strings.Repeat("b", 64)
}

// prepareParityCase runs the legacy derivation for one case.
func prepareParityCase(t *testing.T, test parityCase) (*Service, preparationShape) {
	t.Helper()
	service := parityService(t, test.needsRegistry)
	resolved := resolveParityCase(t, service, test)
	evidence, manifestDigest, grantDigest := parityEvidence(t, service, resolved.products)
	shape, err := legacyPrepareForParity(context.Background(), service, control.Task{},
		resolved.grant(), resolved.plan, evidence, manifestDigest, grantDigest)
	if err != nil {
		t.Fatalf("case %s: legacy preparation failed: %v", test.name, err)
	}
	return service, shape
}

// ------------------------------------------------- the legacy characterization

// Every named shape must prepare, and must prepare something complete.
//
// This is not a golden comparison: a golden produced by running the legacy
// implementation would be the legacy implementation asserting about itself. What
// is checked instead is that each shape carries the material its profile
// requires -- a companion where exposure is enabled, an ordinal program where
// the profile compiles one, a footprint under V5 -- so that a preparation which
// silently stopped producing one of them fails here rather than in whatever
// consumes it later.
func TestEveryNamedPreparationShapePrepares(t *testing.T) {
	for _, test := range parityCases() {
		t.Run(test.name, func(t *testing.T) {
			_, shape := prepareParityCase(t, test)
			if strings.TrimSpace(shape.VisibleSQL) == "" {
				t.Fatal("preparation produced no visible statement")
			}
			if len(shape.PolicyGrant.Products) == 0 {
				t.Fatal("preparation produced no policy grant to authorize against")
			}
			if test.profile == "" {
				if shape.CompanionSQL != "" {
					t.Fatal("a non-exposure operation prepared a companion statement")
				}
				if shape.PlanDigest != "" {
					t.Fatal("a non-exposure operation prepared a plan digest")
				}
				return
			}
			if strings.TrimSpace(shape.CompanionSQL) == "" {
				t.Fatal("an exposure operation prepared no companion statement")
			}
			if len(shape.VisibleFields) == 0 || len(shape.FactFields) == 0 {
				t.Fatal("an exposure operation prepared no projection")
			}
			if shape.PlanDigest == "" {
				t.Fatal("an exposure operation prepared no plan digest")
			}
			if shape.PreparedOperationSHA256 == "" || shape.VisibleTargetSHA256 == "" ||
				shape.CompanionTargetSHA256 == "" {
				t.Fatal("an exposure operation prepared an incomplete binding set")
			}
			ordinalProfile := test.profile == exposure.ProfileV4 || test.profile == exposure.ProfileV5
			if ordinalProfile {
				if shape.OrdinalProgram == nil {
					t.Fatal("an ordinal profile compiled no ordinal program")
				}
				if shape.DictionarySetDigest == "" || len(shape.DictionarySetMembers) == 0 {
					t.Fatal("an ordinal profile resolved no dictionary universe")
				}
				if len(shape.SidecarGrants) == 0 || shape.SidecarGrantsSHA256 == "" {
					t.Fatal("an ordinal profile bound no sidecar grants")
				}
				if shape.EstimatedBaseFacts == 0 {
					t.Fatal("an ordinal profile estimated no base facts")
				}
				if len(shape.SourcePublications) != len(shape.OrdinalProgram.Sources) {
					t.Fatalf("the ordinal program has %d sources but %d resolved publications",
						len(shape.OrdinalProgram.Sources), len(shape.SourcePublications))
				}
			}
			if test.profile == exposure.ProfileV5 && shape.PredicateFootprint == nil {
				t.Fatal("a V5 operation prepared no predicate footprint")
			}
			if test.profile != exposure.ProfileV5 && shape.PredicateFootprint != nil {
				t.Fatal("a profile that accounts no predicate footprint prepared one")
			}
		})
	}
}

// ---------------------------------------------------------- independent oracles

// The dictionary universe must be the Catalog's, not the derivation's own idea
// of it.
//
// This is an oracle both implementations are checked against rather than a
// property one of them defines: the Catalog is loaded from config/catalog.yaml,
// which neither derivation writes. A preparation that resolved a subset would
// pin a root ledger to whichever product it first executed, and old-vs-new
// equality alone would never notice, because both would resolve the same subset.
func TestThePreparedDictionaryUniverseIsTheCatalogs(t *testing.T) {
	for _, test := range parityCases() {
		if !test.needsRegistry {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			service, shape := prepareParityCase(t, test)
			declared := make(map[string]catalog.SnapshotPublication, len(service.catalog.SnapshotPublications))
			for _, publication := range service.catalog.SnapshotPublications {
				declared[publication.Name] = publication
			}
			if len(shape.DictionarySetMembers) != len(declared) {
				t.Fatalf("the prepared dictionary universe has %d members, the Catalog declares %d publications",
					len(shape.DictionarySetMembers), len(declared))
			}
			for _, member := range shape.DictionarySetMembers {
				publication, present := declared[member.PublicationName]
				if !present {
					t.Fatalf("the prepared dictionary universe carries %q, which the Catalog does not declare",
						member.PublicationName)
				}
				if member.DictionaryDigest != publication.DictionaryDigest ||
					member.ManifestDigest != publication.ManifestDigest {
					t.Fatalf("publication %q was prepared with dictionary %s/manifest %s, "+
						"the Catalog declares %s/%s", member.PublicationName,
						member.DictionaryDigest[:12], member.ManifestDigest[:12],
						publication.DictionaryDigest[:12], publication.ManifestDigest[:12])
				}
			}
			// And every sidecar grant must name the physical relation the Catalog
			// publishes, rather than one the binder chose.
			for _, grant := range shape.SidecarGrants {
				qualified := grant.PhysicalSchema + "." + grant.PhysicalView
				matched := false
				for _, publication := range declared {
					if publication.OrdinalSidecar == qualified {
						matched = true
						break
					}
				}
				if !matched {
					t.Fatalf("sidecar grant %q binds %q, which no Catalog publication declares",
						grant.LogicalName, qualified)
				}
			}
		})
	}
}

// Both prepared statements must be admitted by the policy grant preparation
// itself produced.
//
// sqlpolicy is the third party here: it did not participate in the derivation
// and it is what production authorizes with. A derivation that widened the grant
// to fit its own SQL, or emitted SQL its own grant forbids, fails here even if
// old and new agree exactly.
func TestBothPreparedStatementsAreAdmittedByThePreparedGrant(t *testing.T) {
	engine := sqlpolicy.New(sqlpolicy.Config{})
	for _, test := range parityCases() {
		t.Run(test.name, func(t *testing.T) {
			_, shape := prepareParityCase(t, test)
			for _, statement := range []struct{ role, sql string }{
				{"visible", shape.VisibleSQL}, {"companion", shape.CompanionSQL},
			} {
				if statement.sql == "" {
					continue
				}
				if _, err := engine.Authorize(sqlpolicy.Request{
					SQL: statement.sql, Grant: shape.PolicyGrant, RowLimit: 10,
				}); err != nil {
					t.Fatalf("the prepared %s statement is not admitted by the grant preparation produced: %v\n%s",
						statement.role, err, statement.sql)
				}
			}
		})
	}
}

// The metering columns preparation adds must be exactly what the widened grant
// gained -- no more.
//
// The widening is a real privilege increase over the task's approved surface,
// and it is invisible in the approved-column list a reviewer reads. Checking it
// against the task grant, which is an input rather than a derivation product,
// is what keeps the increase bounded by the exposure requirement.
func TestTheGrantIsWidenedOnlyByWhatMeteringRequires(t *testing.T) {
	for _, test := range parityCases() {
		if test.profile == "" {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			service, shape := prepareParityCase(t, test)
			resolved := resolveParityCase(t, service, test)
			sidecars := make(map[string]bool, len(shape.SidecarGrants))
			for _, grant := range shape.SidecarGrants {
				sidecars[grant.LogicalName] = true
			}
			for _, product := range shape.PolicyGrant.Products {
				if sidecars[product.LogicalName] {
					continue
				}
				approved := stringSetFromSlice(resolved.columns[product.LogicalName])
				metering := stringSetFromSlice(shape.MeteringColumns)
				for _, column := range product.ApprovedColumns {
					_, wasApproved := approved[column]
					_, isMetering := metering[column]
					if wasApproved || isMetering {
						continue
					}
					// A relational preparation meters per product rather than
					// through the single-product list, so the Catalog's own entity
					// key and scopes are the bound there.
					catalogProduct, present := service.catalog.LookupProduct(product.LogicalName)
					if present && (contains(catalogProduct.EntityKey, column) ||
						contains(catalogProduct.Scopes, column)) {
						continue
					}
					t.Errorf("the prepared grant approves column %q of %q, which the task did not approve "+
						"and metering does not require", column, product.LogicalName)
				}
			}
		})
	}
}

// A branch-role-qualified predicate cannot be prepared under V5, and must fail
// closed rather than prepare something the footprint does not describe.
//
// This is a real limit of the current derivation, found by this harness and
// recorded here rather than routed around. queryplan.PredicateBindings keys its
// products by product NAME, while a UNION DISTINCT branch qualifies its columns
// by branch ROLE, so `left_branch.expense_type` resolves to nothing. The V4 path
// has no footprint and prepares the same plan; the V5 path refuses it.
//
// The property under test is the refusal, not the message: a preparation that
// silently produced a footprint omitting the branch predicates would under-count
// the atoms the query actually reveals. When the qualified-column work lands,
// this test fails, and the V5 Union parity case must then be promoted from the
// unfiltered form back to the branch-filtered one.
func TestUnionBranchFilteredPredicatesFailClosedUnderV5(t *testing.T) {
	test := parityCase{
		name: "union_branch_filtered", profile: exposure.ProfileV5,
		products: []string{"expense_summary"},
		columns:  map[string][]string{"expense_summary": summaryColumns},
		plan: queryplan.QueryPlan{From: &queryplan.From{UnionDistinct: &queryplan.UnionDistinct{
			Role: "expense_summary", Columns: []string{"department", "month"},
			Left: queryplan.Scan{Product: "expense_summary", Role: "left_branch",
				Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "机票"}}},
			Right: queryplan.Scan{Product: "expense_summary", Role: "right_branch",
				Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "酒店"}}},
		}}, Columns: []string{"expense_summary.department"}},
		needsRegistry: true,
	}
	service := parityService(t, true)
	resolved := resolveParityCase(t, service, test)
	evidence, manifestDigest, grantDigest := parityEvidence(t, service, resolved.products)
	shape, err := legacyPrepareForParity(context.Background(), service, control.Task{},
		resolved.grant(), resolved.plan, evidence, manifestDigest, grantDigest)
	if err == nil {
		t.Fatalf("a V5 union with branch-qualified predicates prepared; "+
			"promote the V5 Union parity case back to the branch-filtered plan "+
			"(footprint atoms=%d)", len(shape.PredicateFootprint.Atoms))
	}
	requireToolCode(t, err, apierr.CodePolicyDenied)

	// The same plan under V4, which accounts no predicate footprint, prepares.
	// Without this half the test would be satisfied by a union that cannot be
	// prepared at all, which is a different and much weaker claim.
	v4 := test
	v4.profile = exposure.ProfileV4
	resolvedV4 := resolveParityCase(t, service, v4)
	if _, err := legacyPrepareForParity(context.Background(), service, control.Task{},
		resolvedV4.grant(), resolvedV4.plan, evidence, manifestDigest, grantDigest); err != nil {
		t.Fatalf("the same union is not preparable under V4 either, so the V5 refusal "+
			"is not about the predicate footprint: %v", err)
	}
}

// The semantic View shape.
//
// It is not in parityCases because it does not enter through prepareTaskPlan's
// Catalog lookup: production resolves the task's view binding from the Control
// Store first and then calls prepareSemanticViewPlan with it. Preparation itself
// performs no store I/O, so the harness supplies the resolved binding the same
// way -- which is also how the extracted package will receive it.
//
// The properties this shape adds to the set are the view binding digest, the
// registry revision, and the internal terminal-product closure that widens the
// policy grant beyond the task's approved root.
func TestTheSemanticViewShapePrepares(t *testing.T) {
	for _, aggregated := range []bool{false, true} {
		name := "projection"
		outerColumns := []string{"amount"}
		if aggregated {
			name = "aggregate_barrier"
			outerColumns = []string{"request_count", "business_unit", "total_amount"}
		}
		t.Run(name, func(t *testing.T) {
			fixture := newSemanticRuntimeFixture(t, aggregated)
			outer := queryplan.QueryPlan{Product: fixture.root.Name, Columns: outerColumns}
			composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name, outer, fixture.artifact)
			if err != nil {
				t.Fatalf("compose semantic View plan: %v", err)
			}
			prepared, err := fixture.service.prepareSemanticViewPlan(
				fixture.grant, fixture.root, fixture.artifact, composition, fixture.binding, outer)
			if err != nil {
				t.Fatalf("prepare semantic View plan: %v", err)
			}
			evidence := datasourceEvidence{DatasourceID: "parity-datasource",
				SchemaDigest: strings.Repeat("9", 64)}
			shape, err := legacyShapeOf(fixture.service, prepared, outer, fixture.grant, evidence,
				strings.Repeat("a", 64), strings.Repeat("b", 64))
			if err != nil {
				t.Fatalf("read semantic View shape: %v", err)
			}

			if shape.ViewBindingDigest != fixture.binding.Digest {
				t.Fatalf("prepared view binding digest is %q, the resolved binding is %q",
					shape.ViewBindingDigest, fixture.binding.Digest)
			}
			if shape.ViewRegistryRevision == "" {
				t.Fatal("a semantic View preparation carries no registry revision")
			}
			if strings.TrimSpace(shape.VisibleSQL) == "" || strings.TrimSpace(shape.CompanionSQL) == "" {
				t.Fatal("a semantic View preparation is missing a statement")
			}
			// The root is what the task approved; the terminal is what the compiled
			// View actually reads. Both must be in the grant the statements are
			// admitted against, and the terminal must not have leaked into the
			// task's own approved products.
			granted := map[string]bool{}
			for _, product := range shape.PolicyGrant.Products {
				granted[product.LogicalName] = true
			}
			if !granted[fixture.terminal.Name] {
				t.Fatalf("the prepared grant does not admit terminal product %q; "+
					"the expanded statement could not be authorized", fixture.terminal.Name)
			}
			if contains(fixture.grant.ApprovedProducts, fixture.terminal.Name) {
				t.Fatal("the terminal product reached the task's approved products")
			}
			engine := sqlpolicy.New(sqlpolicy.Config{})
			for _, statement := range []struct{ role, sql string }{
				{"visible", shape.VisibleSQL}, {"companion", shape.CompanionSQL},
			} {
				if _, err := engine.Authorize(sqlpolicy.Request{
					SQL: statement.sql, Grant: shape.PolicyGrant, RowLimit: 10,
				}); err != nil {
					t.Fatalf("the prepared %s statement is not admitted by the grant "+
						"preparation produced: %v\n%s", statement.role, err, statement.sql)
				}
			}
		})
	}
}

// The prepared shape must not move when the exposure ledger has been consumed.
//
// This is the Scale pre-consumed shape, and it is a property of the split
// between preparation and derivation rather than of preparation alone.
// Preparation is limit-independent by construction -- the prepared target
// binding deliberately excludes the row limit, so two executions of one compiled
// statement under different budgets share it -- and the limits live in
// physicalquery.Derive. If a consumed ledger moved the prepared shape, the same
// statement would carry two prepared identities and no receipt could tie them to
// one compilation; if it did NOT move the derived decisions, the budget would
// not be being spent.
func TestAConsumedLedgerMovesTheDerivationAndNotThePreparation(t *testing.T) {
	test := parityCase{
		name: "scale_pre_consumed", profile: exposure.ProfileV5,
		products: []string{"expense_summary"},
		columns:  map[string][]string{"expense_summary": summaryColumns},
		plan: queryplan.QueryPlan{Product: "expense_summary",
			Columns: []string{"month", "total_amount"}, Limit: 400},
		needsRegistry: true,
	}
	service, fresh := prepareParityCase(t, test)
	_ = service
	_, consumed := prepareParityCase(t, test)
	requireSameShape(t, fresh, consumed)

	engine := sqlpolicy.New(sqlpolicy.Config{})
	derive := func(state physicalquery.LedgerPreState) derivedQuery {
		t.Helper()
		derived, err := (&Service{}).derivePhysicalQuery(engine, fresh.VisibleSQL, fresh.CompanionSQL,
			fresh.PolicyGrant, state, true)
		if err != nil {
			t.Fatalf("derive with %d remaining rows: %v", state.RemainingRows, err)
		}
		return derived
	}
	full := derive(physicalquery.LedgerPreState{
		RemainingRows: 500, InfluenceFacts: 1_000_000,
		HasExposureContext: true, UsesExpandedEvidence: fresh.UsesExpandedEvidence,
	})
	narrowed := derive(physicalquery.LedgerPreState{
		RemainingRows: 7, InfluenceFacts: 1_000_000,
		HasExposureContext: true, UsesExpandedEvidence: fresh.UsesExpandedEvidence,
	})
	if narrowed.visible.RowLimit >= full.visible.RowLimit {
		t.Fatalf("a consumed row budget did not narrow the derivation: %d then %d",
			full.visible.RowLimit, narrowed.visible.RowLimit)
	}
	if full.visible.SQL == narrowed.visible.SQL {
		t.Fatal("two different row limits rendered the same statement bytes")
	}
	// And the prepared identity is the same for both, which is what lets one
	// compiled statement be executed twice under different budgets.
	if physicalquery.ExactDigest(full.visible.SQL) == physicalquery.ExactDigest(narrowed.visible.SQL) {
		t.Fatal("the executed statement identities did not move with the limit")
	}
}

// ------------------------------------------------------------ mutation oracles

// Every load-bearing input must move the prepared shape, or fail closed.
//
// This is the property that makes the parity harness worth trusting. Comparing
// two implementations proves they agree; it says nothing about whether either
// distinguishes inputs it must distinguish. An input that changed without moving
// the prepared identity would be an input the binding does not cover, and a
// receipt over that binding would describe a preparation other inputs also
// produce.
func TestEveryLoadBearingInputMovesThePreparedShape(t *testing.T) {
	base := parityCase{
		name: "mutation_base", profile: exposure.ProfileV5,
		products: []string{"expense_summary"},
		columns:  map[string][]string{"expense_summary": summaryColumns},
		scope:    json.RawMessage(paritySalesScope),
		plan: queryplan.QueryPlan{Product: "expense_summary",
			Columns: []string{"month", "total_amount"}, Limit: 50},
		needsRegistry: true,
	}
	service, baseline := prepareParityCase(t, base)
	baselineDigest := baseline.shapeSHA256(t)

	// failsClosed says which outcome the change must produce. Accepting "moved OR
	// refused" for every entry would let a mutation that silently became a no-op
	// pass as a refusal, which is how "a removed mandatory scope" passed while the
	// harness was quietly filling the scope back in.
	type inputMutation struct {
		apply       func(*parityCase)
		failsClosed bool
	}
	for name, mutation := range map[string]inputMutation{
		"an approved column": {apply: func(c *parityCase) {
			c.columns = map[string][]string{"expense_summary": {"month", "total_amount"}}
		}},
		"the mandatory scope": {apply: func(c *parityCase) {
			c.scope = json.RawMessage(`{"department":["研发部"]}`)
		}},
		"an emptied mandatory scope": {apply: func(c *parityCase) {
			c.scope = json.RawMessage(`{}`)
		}, failsClosed: true},
		"a scope for another dimension": {apply: func(c *parityCase) {
			c.scope = json.RawMessage(`{"expense_type":["机票"]}`)
		}, failsClosed: true},
		"an unapproved product": {apply: func(c *parityCase) {
			c.products = []string{"expense_detail"}
			c.columns = map[string][]string{"expense_detail": detailColumns}
		}, failsClosed: true},
		"the exposure profile": {apply: func(c *parityCase) { c.profile = exposure.ProfileV4 }},
		"the projected columns": {apply: func(c *parityCase) {
			c.plan.Columns = []string{"month", "request_count"}
		}},
		"the projection order": {apply: func(c *parityCase) {
			c.plan.Columns = []string{"total_amount", "month"}
		}},
		"the row limit": {apply: func(c *parityCase) { c.plan.Limit = 51 }},
		"an added filter": {apply: func(c *parityCase) {
			c.plan.Filters = []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "机票"}}
		}},
		"a filter literal": {apply: func(c *parityCase) {
			c.plan.Filters = []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "酒店"}}
		}},
		"grouping": {apply: func(c *parityCase) {
			c.plan.Columns = []string{"month"}
			c.plan.GroupBy = []string{"month"}
			c.plan.Aggregates = []queryplan.Aggregate{
				{Function: "sum", Column: "total_amount", Alias: "total"}}
		}},
		"the approved product": {apply: func(c *parityCase) {
			c.products = []string{"expense_detail"}
			c.columns = map[string][]string{"expense_detail": detailColumns}
			c.plan = queryplan.QueryPlan{Product: "expense_detail",
				Columns: []string{"receipt_no", "amount"}, Limit: 50}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base
			mutated.plan = cloneQueryPlan(base.plan)
			mutation.apply(&mutated)
			resolved := resolveParityCase(t, service, mutated)
			evidence, manifestDigest, grantDigest := parityEvidence(t, service, resolved.products)
			registryService := parityService(t, mutated.needsRegistry)
			shape, err := legacyPrepareForParity(context.Background(), registryService, control.Task{},
				resolved.grant(), resolved.plan, evidence, manifestDigest, grantDigest)
			if mutation.failsClosed {
				if err == nil {
					t.Fatalf("%s was accepted; it must fail closed", name)
				}
				return
			}
			if err != nil {
				t.Fatalf("changing %s was refused, but this change is preparable: %v", name, err)
			}
			if shape.shapeSHA256(t) == baselineDigest {
				t.Fatalf("changing %s did not move the prepared shape", name)
			}
		})
	}
}

// The Catalog, its publications and the compiler are inputs too, and a change to
// any of them must move the prepared bindings.
//
// These cannot be varied through the plan or the grant, so they are varied where
// the binding reads them. A binding that did not move would let a statement
// compiled against one Catalog be presented as one compiled against another.
func TestCatalogAndPublicationIdentitiesReachThePreparedBinding(t *testing.T) {
	test := parityCase{
		name: "binding_inputs", profile: exposure.ProfileV5,
		products: []string{"expense_summary"},
		columns:  map[string][]string{"expense_summary": summaryColumns},
		plan: queryplan.QueryPlan{Product: "expense_summary",
			Columns: []string{"month", "total_amount"}, Limit: 50},
		needsRegistry: true,
	}
	service, baseline := prepareParityCase(t, test)
	resolved := resolveParityCase(t, service, test)
	evidence, manifestDigest, grantDigest := parityEvidence(t, service, resolved.products)

	build := func(mutate func(*preparedOperation)) string {
		base := preparedOperation{
			PlanSHA256: baseline.PlanDigest, ExposureProfileVersion: test.profile,
			GrantDigest: grantDigest, ManifestDigest: manifestDigest,
			CatalogSHA256: service.catalog.SHA256,
			DatasourceID:  evidence.DatasourceID, SchemaDigest: evidence.SchemaDigest,
			OrdinalDictionarySetSHA256: baseline.DictionarySetDigest,
			SidecarGrantsSHA256:        baseline.SidecarGrantsSHA256,
			VisibleSQL:                 baseline.VisibleSQL, CompanionSQL: baseline.CompanionSQL,
		}
		mutate(&base)
		operation, err := newPreparedOperation(base)
		if err != nil {
			t.Fatalf("build prepared operation: %v", err)
		}
		return operation.digest()
	}
	unchanged := build(func(*preparedOperation) {})
	if unchanged != baseline.PreparedOperationSHA256 {
		t.Fatal("the harness does not reproduce the binding the legacy preparation computed")
	}
	for name, mutate := range map[string]func(*preparedOperation){
		"the Catalog digest":      func(o *preparedOperation) { o.CatalogSHA256 = strings.Repeat("1", 64) },
		"the dictionary set":      func(o *preparedOperation) { o.OrdinalDictionarySetSHA256 = strings.Repeat("2", 64) },
		"the sidecar grants":      func(o *preparedOperation) { o.SidecarGrantsSHA256 = strings.Repeat("3", 64) },
		"the grant digest":        func(o *preparedOperation) { o.GrantDigest = strings.Repeat("4", 64) },
		"the manifest digest":     func(o *preparedOperation) { o.ManifestDigest = strings.Repeat("5", 64) },
		"the schema digest":       func(o *preparedOperation) { o.SchemaDigest = strings.Repeat("6", 64) },
		"the datasource":          func(o *preparedOperation) { o.DatasourceID = "other-datasource" },
		"the view binding":        func(o *preparedOperation) { o.ViewBindingDigest = strings.Repeat("7", 64) },
		"the plan digest":         func(o *preparedOperation) { o.PlanSHA256 = strings.Repeat("8", 64) },
		"the exposure profile":    func(o *preparedOperation) { o.ExposureProfileVersion = exposure.ProfileV4 },
		"the visible statement":   func(o *preparedOperation) { o.VisibleSQL += " OFFSET 0" },
		"the companion statement": func(o *preparedOperation) { o.CompanionSQL += " OFFSET 0" },
	} {
		t.Run(name, func(t *testing.T) {
			if build(mutate) == baseline.PreparedOperationSHA256 {
				t.Fatalf("changing %s did not move the prepared operation binding", name)
			}
		})
	}
}

// One publication reached twice must resolve once.
//
// The union case reaches expense_summary through both branches. If duplicate
// resolution produced two dictionary members or two sidecar grants, the
// dictionary universe would depend on how a plan happened to name its sources,
// and the ordinal ledger's cross-product union would stop being exact.
func TestADuplicatedProductResolvesToOneBinding(t *testing.T) {
	var duplicate parityCase
	for _, test := range parityCases() {
		if test.name == "duplicate_product_binding" {
			duplicate = test
		}
	}
	if duplicate.name == "" {
		t.Fatal("the duplicate-product shape is no longer in the named case set")
	}
	_, shape := prepareParityCase(t, duplicate)
	names := make(map[string]int, len(shape.DictionarySetMembers))
	for _, member := range shape.DictionarySetMembers {
		names[member.PublicationName]++
	}
	for name, count := range names {
		if count != 1 {
			t.Errorf("publication %q resolved to %d dictionary members", name, count)
		}
	}
	logical := make(map[string]int, len(shape.SidecarGrants))
	for _, grant := range shape.SidecarGrants {
		logical[grant.LogicalName]++
	}
	for name, count := range logical {
		if count != 1 {
			t.Errorf("sidecar %q resolved to %d grants", name, count)
		}
	}
	if len(shape.OrdinalProgram.Sources) < 2 {
		t.Fatalf("the duplicate-product shape compiled %d sources; it must reach one publication twice",
			len(shape.OrdinalProgram.Sources))
	}
	seen := map[string]bool{}
	for _, publication := range shape.SourcePublications {
		seen[publication] = true
	}
	if len(seen) != 1 {
		t.Fatalf("the duplicate-product shape resolved %d distinct publications, want 1", len(seen))
	}
}

// ------------------------------------------------------------- the parity hook

// requireLegacyParity is what the extraction's own test calls.
//
// It is defined here, beside the legacy capture, so that the two halves of the
// comparison cannot drift into reading different properties. Until
// internal/physicalquery grows a Prepare, nothing calls it: the legacy side of
// this harness is characterization and oracles, and the differential half
// becomes live with the extraction.
func requireLegacyParity(t *testing.T, test parityCase, extracted preparationShape) {
	t.Helper()
	_, legacy := prepareParityCase(t, test)
	requireSameShape(t, legacy, extracted)
}
