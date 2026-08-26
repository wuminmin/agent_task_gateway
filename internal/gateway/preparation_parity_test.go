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

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

// This file is the harness the target-preparation extraction moved behind, and
// what it asserts changed at T1d.
//
// While the extraction was in progress it was a differential: the Gateway
// derived the statements, internal/physicalquery derived them again, and every
// named shape had to agree. That comparison is over -- the Gateway's derivation
// is deleted and production calls Prepare, so a differential against it would
// now be Prepare compared with itself.
//
// What the comparison became is still worth making, and it is a different claim:
// the production preparation path must EXPOSE exactly what preparation produced.
// productionShapeOf reads every member off preparedQueryPlan and the
// planExposureContext the Gateway built; extractedShapeOf reads the same members
// off an independently prepared PreparedOperation. A Gateway that dropped a
// member, reordered a projection, widened a grant or rebuilt a digest fails here.
//
// Equality between the two is necessary and not sufficient, exactly as before.
// So every case is also checked against evidence neither side produced: the
// Catalog and its snapshot publications, the policy engine, and -- where the
// case has a physical relation to run against -- PostgreSQL itself.

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

	// The sealed preparation identity and the two prepared target identities the
	// Query Execution Binding V2 carries.
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
			differences = append(differences, fmt.Sprintf("%s:\n  production = %s\n  prepared   = %s",
				name, shapeMember(left), shapeMember(right)))
		}
	}
	compare("visible SQL bytes", want.VisibleSQL, got.VisibleSQL)
	compare("companion SQL bytes", want.CompanionSQL, got.CompanionSQL)
	compare("visible field ordering", want.VisibleFields, got.VisibleFields)
	compare("fact field ordering", want.FactFields, got.FactFields)
	compare("provenance field ordering", want.ProvenanceFields, got.ProvenanceFields)
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

// ---------------------------------------------------- the production surface

// productionPrepareForParity runs the Gateway's production preparation for one
// case and reads off every property the extraction must have preserved.
//
// It enters at prepareTaskPlan, which is where executeSQL enters, so what is
// read below is what the running system holds and not a reconstruction of it.
func productionPrepareForParity(ctx context.Context, service *Service, task control.Task,
	grant control.TaskGrant, plan queryplan.QueryPlan) (preparationShape, error) {
	prepared, err := service.prepareTaskPlan(ctx, task, grant, plan)
	if err != nil {
		return preparationShape{}, err
	}
	return productionShapeOf(prepared, plan)
}

// productionShapeOf reads every property off one prepared plan.
//
// It is separate from the call above because the semantic View path reaches
// prepareSemanticViewPlan after the Control Store has resolved the task's view
// binding, and preparation itself performs no store I/O. Sharing this half is
// what keeps the View case reading the same properties as every other one.
func productionShapeOf(prepared preparedQueryPlan, plan queryplan.QueryPlan) (preparationShape, error) {
	context := prepared.Exposure
	shape := preparationShape{VisibleSQL: prepared.SQL, PolicyGrant: prepared.PolicyGrant}
	if context == nil {
		// A plain query builds no exposure context, so its projection is not held
		// there. It is still a projection the Gateway computes and uses:
		// queryPlanResultNames is what names the columns of the stored result, so
		// reading it here compares the extraction against what production actually
		// projects rather than against a nil the shape merely happens to leave.
		shape.VisibleFields = queryPlanResultNames(plan)
		return shape, nil
	}
	shape.CompanionSQL = context.provenanceSQL
	shape.VisibleFields = append([]string(nil), context.visibleFields...)
	shape.FactFields = append([]string(nil), context.factFields...)
	shape.ProvenanceFields = append([]string(nil), context.provenanceFields...)
	shape.Grouped = context.grouped
	shape.ExpandedEvidence = context.expandedEvidence
	shape.UsesExpandedEvidence = context.usesExpandedEvidence()
	shape.PlanDigest = context.planDigest
	shape.ViewBindingDigest = context.viewBindingDigest
	shape.ViewRegistryRevision = context.viewRegistryRevision
	shape.PredicateFootprint = context.predicateFootprint
	// The canonical identity is one value under one member; which of the two
	// members it lands in is determined by the plan shape, because a relational
	// plan is identified by an algebra normal form and a single-product one by the
	// relation normal form. Placing it by shape is what keeps a relational
	// preparation from being compared against a single-product one.
	if context.relational != nil {
		shape.AlgebraNormalFormSHA256 = context.planDigest
	} else {
		shape.NormalFormSHA256 = context.planDigest
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
		shape.SourcePublications = make(map[string]string, len(program.Sources))
		for _, source := range program.Sources {
			shape.SourcePublications[source.SourceAlias] = source.SidecarBinding.PublicationID
		}
	}
	binding := context.prepared.Binding()
	shape.PreparedOperationSHA256 = binding.SHA256
	visible, err := binding.TargetSHA256(preparedbinding.RoleVisible)
	if err != nil {
		return preparationShape{}, err
	}
	shape.VisibleTargetSHA256 = visible
	if binding.HasCompanion {
		companion, companionErr := binding.TargetSHA256(preparedbinding.RoleCompanion)
		if companionErr != nil {
			return preparationShape{}, companionErr
		}
		shape.CompanionTargetSHA256 = companion
	}
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
			if !fullSnapshotRegistryRequested() {
				continue
			}
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

func branchFilteredUnionPlan() queryplan.QueryPlan {
	return queryplan.QueryPlan{From: &queryplan.From{UnionDistinct: &queryplan.UnionDistinct{
		Role: "expense_summary", Columns: []string{"department", "month"},
		Left: queryplan.Scan{Product: "expense_summary", Role: "left_branch",
			Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "机票"}}},
		Right: queryplan.Scan{Product: "expense_summary", Role: "right_branch",
			Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "酒店"}}},
	}}, Columns: []string{"expense_summary.department"}}
}

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
	// Both profiles exercise the same branch-filtered shape. Under V5 the two
	// branch-local literals must become distinct predicate atoms; under V4 the
	// duplicate-product case continues to pin one shared publication binding.
	union := branchFilteredUnionPlan()

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
			plan: union, needsRegistry: true, executable: true,
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

// prepareParityCase runs the production preparation for one case.
func prepareParityCase(t *testing.T, test parityCase) (*Service, preparationShape) {
	t.Helper()
	service := parityService(t, test.needsRegistry)
	resolved := resolveParityCase(t, service, test)
	shape, err := productionPrepareForParity(context.Background(), service, control.Task{},
		resolved.grant(), resolved.plan)
	if err != nil {
		t.Fatalf("case %s: production preparation failed: %v", test.name, err)
	}
	return service, shape
}

// ------------------------------------------------- the legacy characterization

// ---------------------------------------------------------- independent oracles

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
				fixture.grant, fixture.artifact, composition, fixture.binding, outer)
			if err != nil {
				t.Fatalf("prepare semantic View plan: %v", err)
			}
			shape, err := productionShapeOf(prepared, composition.Plan)
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

// ------------------------------------------------------------ mutation oracles
