package physicalquery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// Prepare derives one operation's physical statements from immutable inputs.
//
// This is the function the whole extraction exists for. The Gateway calls it
// before executing and the finalizer calls it afterwards from independently
// obtained frozen material; both hand the same values to the same code, so the
// finalizer reproduces the statements the Gateway executed rather than checking
// the Gateway's word for them. Nothing here reads a registry, a store or a
// clock: everything it depends on is in PreparationInputs, which is what makes
// the two calls comparable at all.
//
// It performs no row-limit arithmetic. The limits depend on runtime ledger
// state, which is not an input here and belongs to DeriveLimits and Derive --
// that split is why one compiled statement can be executed twice under
// different budgets and still carry one prepared identity.
func Prepare(inputs PreparationInputs) (PreparedOperation, error) {
	compiler, err := LocalCompilerIdentity()
	if err != nil {
		return PreparedOperation{}, err
	}
	return PrepareWith(inputs, compiler)
}

// PrepareWith is Prepare against a stated compiler identity.
//
// The finalizer uses it to reproduce a preparation made by a binary other than
// its own: the identity it passes comes from the signed binding, so a mismatch
// is reported as two compilers disagreeing rather than as two statements
// differing for no stated reason.
func PrepareWith(inputs PreparationInputs, compiler CompilerIdentityV1) (PreparedOperation, error) {
	if err := inputs.Validate(); err != nil {
		return PreparedOperation{}, err
	}
	draft, err := derivePreparation(inputs)
	if err != nil {
		return PreparedOperation{}, err
	}
	return NewPreparedOperation(inputs, compiler, draft)
}

// ErrShapeNotExtracted is what a plan shape whose derivation still lives in the
// Gateway returns.
//
// It is a named error rather than a silent fallback. During the extraction the
// Gateway remains the only production path, so a shape reaching here before its
// derivation has moved must say so plainly -- a Prepare that quietly produced
// something for a shape it does not implement is exactly the failure mode the
// differential harness exists to catch, and it should not need to.
var ErrShapeNotExtracted = errors.New(
	"this plan shape's derivation has not been moved into internal/physicalquery yet")

// derivePreparation is the moved derivation.
func derivePreparation(inputs PreparationInputs) (OperationDraft, error) {
	if inputs.Plan.From != nil {
		return deriveRelationalPreparation(inputs)
	}
	product, found := inputs.Catalog.LookupProduct(inputs.Plan.Product)
	if !found {
		return OperationDraft{}, fmt.Errorf("this Catalog declares no data product %q", inputs.Plan.Product)
	}
	if product.ViewContract != nil {
		return OperationDraft{}, fmt.Errorf("%w: semantic View root", ErrShapeNotExtracted)
	}
	approved := map[string]bool{}
	for _, name := range inputs.Grant.ApprovedProducts {
		approved[name] = true
	}
	if !approved[inputs.Plan.Product] {
		return OperationDraft{}, fmt.Errorf("this task does not approve data product %q", inputs.Plan.Product)
	}

	columns := stringSet(inputs.Grant.ApprovedColumns[inputs.Plan.Product])
	aggregates := make(map[string]struct{}, len(product.AllowedAggregates))
	for _, aggregate := range product.AllowedAggregates {
		aggregates[strings.ToLower(strings.TrimSpace(aggregate))] = struct{}{}
	}

	policy, err := preparationPolicyGrant(inputs.Grant, inputs.Catalog)
	if err != nil {
		return OperationDraft{}, err
	}

	if !inputs.Grant.ExposureEnabled() {
		// A plain query has one statement, no companion and no accounting. It is
		// compiled against the approved columns alone: there is no metering
		// projection to widen them with.
		visibleSQL, compileErr := queryplan.Compile(inputs.Plan,
			queryplan.Product{Name: inputs.Plan.Product, Columns: columns, AllowedAggregates: aggregates})
		if compileErr != nil {
			return OperationDraft{}, compileErr
		}
		return OperationDraft{
			VisibleSQL:    visibleSQL,
			VisibleFields: visibleProjection(inputs.Plan),
			PolicyGrant:   policy,
		}, nil
	}

	shape, err := deriveExposureShape(inputs.Plan, product, columns, aggregates)
	if err != nil {
		return OperationDraft{}, err
	}
	draft := OperationDraft{
		VisibleSQL:       shape.visibleSQL,
		CompanionSQL:     shape.companionSQL,
		VisibleFields:    shape.visibleFields,
		FactFields:       shape.factFields,
		ProvenanceFields: shape.provenanceFields,
		Grouped:          shape.grouped,
	}

	// The canonical plan identity, computed in the same order the profiles apply
	// it: V2 onward normalize against the approved surface, and V5 then replaces
	// that with the V4 normal form built against the ordinal product. The order
	// matters -- taking the V2 form for V5 would identify the query by a form its
	// ledger does not key on.
	if inputs.Grant.UsesNormalizedIdentity() {
		if draft.NormalFormSHA256, err = normalizeV2Identity(
			inputs.Plan, product, columns, aggregates); err != nil {
			return OperationDraft{}, err
		}
	}

	if inputs.Grant.UsesOrdinalProgram() {
		ordinalProduct, ordinalErr := ordinalQueryProduct(inputs.sources(), product, columns)
		if ordinalErr != nil {
			return OperationDraft{}, ordinalErr
		}
		compiled, compileErr := queryplan.CompileOrdinal(inputs.Plan, ordinalProduct)
		if compileErr != nil {
			return OperationDraft{}, compileErr
		}
		bound, bindErr := bindOrdinalSidecars(inputs.sources(), compiled.ProvenanceSQL,
			compiled.ProvenanceFields, compiled.OrdinalProgram)
		if bindErr != nil {
			return OperationDraft{}, bindErr
		}
		// The ordinal compilation supersedes the exposure shape's statements: it
		// is the same plan compiled with the provenance handles the sidecars join.
		draft.VisibleSQL = compiled.VisibleSQL
		draft.CompanionSQL = bound.companionSQL
		draft.ProvenanceFields = bound.provenanceFields
		draft.OrdinalProgram = &bound.program
		draft.DictionarySet = &bound.dictionarySet
		draft.SidecarGrants = bound.sidecarGrants
		draft.SourcePublications = bound.sourcePublications
		draft.EstimatedBaseFacts = bound.estimatedBaseFacts
		if inputs.Plan.Limit > 0 {
			draft.EstimatedBaseFacts = estimateBaseFacts(inputs.sources(), bound, uint64(inputs.Plan.Limit))
		}
		if inputs.Grant.UsesPredicateFootprint() {
			footprint, footprintErr := derivePredicateFootprint(inputs,
				map[string]queryplan.Product{inputs.Plan.Product: ordinalProduct})
			if footprintErr != nil {
				return OperationDraft{}, footprintErr
			}
			draft.PredicateFootprint = footprint
			normal, normalizeErr := queryplan.NormalizeV4(inputs.Plan, ordinalProduct)
			if normalizeErr != nil {
				return OperationDraft{}, normalizeErr
			}
			if draft.NormalFormSHA256, err = normal.Digest(); err != nil {
				return OperationDraft{}, err
			}
		}
	}

	// The grant is widened last, and in production order: metering columns first,
	// then the ordinal sidecars. Reversing them would produce the same set here
	// but a different one the first time a sidecar logical name collided with a
	// task product, which extendOrdinalPolicyGrant is what refuses.
	if policy, err = widenForMetering(policy, product.Name, shape.meteringColumns); err != nil {
		return OperationDraft{}, err
	}
	if len(draft.SidecarGrants) > 0 {
		if policy, err = widenForSidecars(policy, draft.SidecarGrants); err != nil {
			return OperationDraft{}, err
		}
	}
	draft.PolicyGrant = policy
	return draft, nil
}

// deriveRelationalPreparation is the online Join/Union derivation.
//
// It differs from the single-product one in what supersedes what. There, the
// ordinal compilation replaces the visible statement, because the ordinal
// compiler recompiles the same single scan with provenance handles. Here the
// relational compiler produces both statements at once and the ordinal binding
// only wraps the provenance one, so the visible statement is the relational
// compiler's throughout.
func deriveRelationalPreparation(inputs PreparationInputs) (OperationDraft, error) {
	// Online Join/Union is an accounted fragment only. Without a profile there is
	// no dependency evidence, and a Join with no evidence is a cartesian
	// disclosure nothing could account for.
	if !inputs.Grant.UsesNormalizedIdentity() {
		return OperationDraft{}, fmt.Errorf(
			"online Join/Union requires an exposure profile from V2 onward, this task carries %q",
			inputs.Grant.ExposureProfile)
	}
	names, err := queryplan.RelationalProductNames(inputs.Plan)
	if err != nil {
		return OperationDraft{}, err
	}
	approved := map[string]bool{}
	for _, name := range inputs.Grant.ApprovedProducts {
		approved[name] = true
	}
	queryProducts := make(map[string]queryplan.Product, len(names))
	catalogProducts := make(map[string]catalog.Product, len(names))
	for _, name := range names {
		product, found := inputs.Catalog.LookupProduct(name)
		if !found || !approved[name] {
			return OperationDraft{}, fmt.Errorf(
				"this relational plan reads data product %q, which the task does not approve", name)
		}
		columns := stringSet(inputs.Grant.ApprovedColumns[name])
		queryProduct := relationalQueryProduct(product, columns)
		if inputs.Grant.UsesOrdinalProgram() {
			if queryProduct, err = ordinalQueryProduct(inputs.sources(), product, columns); err != nil {
				return OperationDraft{}, err
			}
		}
		queryProducts[name] = queryProduct
		catalogProducts[name] = product
	}
	compiled, err := queryplan.CompileRelational(inputs.Plan, queryProducts)
	if err != nil {
		return OperationDraft{}, err
	}
	shape, err := deriveRelationalShape(inputs.Plan, inputs.Grant.ApprovedColumns, compiled, catalogProducts)
	if err != nil {
		return OperationDraft{}, err
	}

	policy, err := preparationPolicyGrant(inputs.Grant, inputs.Catalog)
	if err != nil {
		return OperationDraft{}, err
	}
	draft := OperationDraft{
		VisibleSQL:   compiled.VisibleSQL,
		CompanionSQL: compiled.ProvenanceSQL,
		// A relational compilation projects one field list; the visible and fact
		// projections are the same list, because every delivered field is an
		// accounted one.
		VisibleFields:    append([]string(nil), compiled.VisibleFields...),
		FactFields:       append([]string(nil), compiled.VisibleFields...),
		ProvenanceFields: append([]string(nil), compiled.ProvenanceFields...),
		Grouped:          len(inputs.Plan.GroupBy) > 0 || len(inputs.Plan.Aggregates) > 0,
		ExpandedEvidence: compiled.ExpandedEvidence,
		NormalFormSHA256: shape.normalFormSHA256,
	}

	if inputs.Grant.UsesOrdinalProgram() {
		bound, bindErr := bindOrdinalSidecars(inputs.sources(), compiled.ProvenanceSQL,
			compiled.ProvenanceFields, compiled.OrdinalProgram)
		if bindErr != nil {
			return OperationDraft{}, bindErr
		}
		draft.CompanionSQL = bound.companionSQL
		draft.ProvenanceFields = bound.provenanceFields
		draft.OrdinalProgram = &bound.program
		draft.DictionarySet = &bound.dictionarySet
		draft.SidecarGrants = bound.sidecarGrants
		draft.SourcePublications = bound.sourcePublications
		draft.EstimatedBaseFacts = bound.estimatedBaseFacts
		if inputs.Grant.UsesPredicateFootprint() {
			footprint, footprintErr := derivePredicateFootprint(inputs, queryProducts)
			if footprintErr != nil {
				return OperationDraft{}, footprintErr
			}
			draft.PredicateFootprint = footprint
			// A relational V5 query is identified by the algebra normal form, not
			// by the single-relation V4 one: there is no single relation to
			// normalize against.
			normal, normalizeErr := queryplan.SemanticNormalFormV4(inputs.Plan, compiled, queryProducts)
			if normalizeErr != nil {
				return OperationDraft{}, normalizeErr
			}
			draft.NormalFormSHA256 = normal.SHA256
		}
	}

	// Every product the compilation meters must be in the grant, and each is
	// widened only by its own metering closure. A product absent from the grant
	// is a refusal rather than an addition: the evidence would be accounted
	// against an authorization that never covered it.
	for _, name := range sortedStrings(names) {
		if policy, err = widenForMetering(policy, name, shape.meteringByProduct[name]); err != nil {
			return OperationDraft{}, err
		}
	}
	if len(draft.SidecarGrants) > 0 {
		if policy, err = widenForSidecars(policy, draft.SidecarGrants); err != nil {
			return OperationDraft{}, err
		}
	}
	draft.PolicyGrant = policy
	return draft, nil
}

// relationalShape is the accounted material a relational compilation determines
// beyond its statements.
type relationalShape struct {
	meteringByProduct map[string][]string
	normalFormSHA256  string
}

// deriveRelationalShape computes the per-product metering closures and the
// algebra normal form.
//
// The closure is per product rather than global: a Join over two products
// meters each against its own entity key, scopes and evidence fields, and
// widening both by the union would grant each product columns only the other
// requires.
// The metering seed is a parameter rather than read off a grant, because the
// two relational paths seed it differently: an ordinary plan starts from the
// task's approved columns, while a semantic View starts from the positive
// evidence closure the compilation itself computed. Everything after the seed is
// identical, so it is one function.
func deriveRelationalShape(plan queryplan.QueryPlan, approvedColumns map[string][]string,
	compiled queryplan.RelationalCompilation,
	products map[string]catalog.Product) (relationalShape, error) {
	if compiled.VisibleSQL == "" || compiled.ProvenanceSQL == "" || len(compiled.Sources) == 0 {
		return relationalShape{}, errors.New("relational compilation is incomplete")
	}
	shape := relationalShape{meteringByProduct: make(map[string][]string, len(compiled.Products))}
	for _, name := range compiled.Products {
		product, present := products[name]
		if !present || product.FactNamespace == "" || product.Snapshot == "" ||
			product.StableRelationRole == "" || len(product.EntityKey) == 0 {
			return relationalShape{}, fmt.Errorf("product %q lacks stable semantic metadata", name)
		}
		metering := stringSet(approvedColumns[name])
		for _, column := range append(append([]string(nil), product.EntityKey...), product.Scopes...) {
			if !productPublishes(product, column) {
				return relationalShape{}, fmt.Errorf("metering field %q is absent from product %q", column, name)
			}
			metering[column] = struct{}{}
		}
		for _, source := range compiled.Sources {
			if source.Product != name {
				continue
			}
			for _, field := range source.EvidenceFields {
				metering[field] = struct{}{}
			}
		}
		shape.meteringByProduct[name] = sortedSet(metering)
	}
	// The semantic normal form is built against the FULL published surface of
	// every product, not against the approved one. It is a canonical identity for
	// the query, and making it depend on what one task happened to approve would
	// give two tasks running one query two identities.
	semantic := make(map[string]queryplan.Product, len(products))
	for name, product := range products {
		semantic[name] = relationalQueryProduct(product, stringSet(product.FieldNames()))
	}
	normal, err := queryplan.SemanticNormalForm(plan, compiled, semantic)
	if err != nil {
		return relationalShape{}, err
	}
	shape.normalFormSHA256 = normal.SHA256
	return shape, nil
}

func productPublishes(product catalog.Product, column string) bool {
	for _, field := range product.Fields {
		if field.Name == column {
			return true
		}
	}
	return false
}

// visibleProjection is what a plain query returns, in request order.
func visibleProjection(plan queryplan.QueryPlan) []string {
	fields := append([]string(nil), plan.Columns...)
	for _, aggregate := range plan.Aggregates {
		fields = append(fields, aggregate.Alias)
	}
	return fields
}

// exposureShape is the accounted fragment's projections and statements.
type exposureShape struct {
	visibleSQL       string
	companionSQL     string
	visibleFields    []string
	factFields       []string
	provenanceFields []string
	meteringColumns  []string
	grouped          bool
}

// deriveExposureShape compiles the visible and provenance statements for one
// single-product accounted plan.
//
// The hidden projection is what makes the pair comparable. An ungrouped visible
// statement selects the provenance fields too and orders by the entity key, so
// the two capped row sets are equal; a grouped one selects its group keys
// internally and compiles a separate unlimited provenance statement, because a
// group's dependency set is not bounded by the groups delivered.
func deriveExposureShape(plan queryplan.QueryPlan, product catalog.Product,
	approvedColumns, allowedAggregates map[string]struct{}) (exposureShape, error) {
	if strings.TrimSpace(product.Snapshot) == "" || len(product.EntityKey) == 0 {
		return exposureShape{}, errors.New("product does not define a stable snapshot and entity key")
	}
	shape := exposureShape{grouped: len(plan.GroupBy) > 0 || len(plan.Aggregates) > 0}
	shape.visibleFields = append([]string(nil), plan.Columns...)
	shape.factFields = append([]string(nil), plan.Columns...)
	for _, aggregate := range plan.Aggregates {
		function := strings.ToLower(aggregate.Function)
		switch function {
		case "count", "sum", "min", "max":
		default:
			return exposureShape{}, fmt.Errorf("aggregate %q is outside the exposure-accounted fragment",
				aggregate.Function)
		}
		shape.visibleFields = append(shape.visibleFields, aggregate.Alias)
		shape.factFields = append(shape.factFields, function+"("+aggregate.Column+")")
	}

	published := make(map[string]struct{}, len(product.Fields))
	for _, field := range product.Fields {
		published[field.Name] = struct{}{}
	}
	metering := make(map[string]struct{}, len(approvedColumns)+len(product.EntityKey)+len(product.Scopes))
	for column := range approvedColumns {
		metering[column] = struct{}{}
	}
	for _, column := range append(append([]string(nil), product.EntityKey...), product.Scopes...) {
		if _, ok := published[column]; !ok {
			return exposureShape{}, fmt.Errorf("metering column %q is not published", column)
		}
		metering[column] = struct{}{}
	}
	shape.meteringColumns = sortedSet(metering)
	internal := queryplan.Product{Name: product.Name, Columns: metering, AllowedAggregates: allowedAggregates}

	provenance := map[string]struct{}{}
	for _, column := range product.EntityKey {
		provenance[column] = struct{}{}
	}
	for _, column := range product.Scopes {
		provenance[column] = struct{}{}
	}
	for _, column := range plan.Columns {
		provenance[column] = struct{}{}
	}
	for _, aggregate := range plan.Aggregates {
		if aggregate.Column != "*" {
			provenance[aggregate.Column] = struct{}{}
		}
	}
	for _, filter := range plan.Filters {
		provenance[filter.Column] = struct{}{}
	}
	for _, group := range plan.GroupBy {
		provenance[group] = struct{}{}
	}
	shape.provenanceFields = sortedSet(provenance)

	visiblePlan := clonePlan(plan)
	if !shape.grouped {
		selected := stringSet(visiblePlan.Columns)
		for _, field := range shape.provenanceFields {
			if _, present := selected[field]; !present {
				visiblePlan.Columns = append(visiblePlan.Columns, field)
				selected[field] = struct{}{}
			}
		}
		ordered := make(map[string]struct{}, len(visiblePlan.OrderBy)+len(product.EntityKey))
		for _, order := range visiblePlan.OrderBy {
			ordered[order.Column] = struct{}{}
		}
		for _, key := range product.EntityKey {
			if _, present := ordered[key]; !present {
				visiblePlan.OrderBy = append(visiblePlan.OrderBy,
					queryplan.Order{Column: key, Direction: "asc"})
				ordered[key] = struct{}{}
			}
		}
	} else {
		selected := stringSet(visiblePlan.Columns)
		for _, group := range plan.GroupBy {
			if _, present := selected[group]; !present {
				visiblePlan.Columns = append(visiblePlan.Columns, group)
				selected[group] = struct{}{}
			}
		}
	}
	if shape.grouped && (plan.Limit > 0 || plan.Offset > 0) {
		ordered := make(map[string]struct{}, len(visiblePlan.OrderBy)+len(plan.GroupBy))
		for _, order := range visiblePlan.OrderBy {
			ordered[order.Column] = struct{}{}
		}
		for _, key := range sortedSet(stringSet(plan.GroupBy)) {
			if _, present := ordered[key]; !present {
				visiblePlan.OrderBy = append(visiblePlan.OrderBy,
					queryplan.Order{Column: key, Direction: "asc"})
				ordered[key] = struct{}{}
			}
		}
	}
	visibleSQL, err := queryplan.Compile(visiblePlan, internal)
	if err != nil {
		return exposureShape{}, err
	}
	shape.visibleSQL = visibleSQL
	shape.companionSQL = visibleSQL
	if shape.grouped {
		companionPlan := queryplan.QueryPlan{Product: plan.Product, Columns: shape.provenanceFields,
			Filters: append([]queryplan.Filter(nil), plan.Filters...)}
		if shape.companionSQL, err = queryplan.Compile(companionPlan, internal); err != nil {
			return exposureShape{}, err
		}
	}
	return shape, nil
}

// normalizeV2Identity is the canonical plan identity every profile from V2
// onward carries.
//
// It normalizes against the approved surface rather than the metering one: the
// identity is what the task asked for, and widening it with the columns
// preparation added for accounting would make two different requests share one
// identity whenever their metering closures happened to coincide.
func normalizeV2Identity(plan queryplan.QueryPlan, product catalog.Product,
	approvedColumns, allowedAggregates map[string]struct{}) (string, error) {
	if strings.TrimSpace(product.FactNamespace) == "" ||
		strings.TrimSpace(product.StableRelationRole) == "" {
		return "", errors.New("this product lacks a canonical fact namespace or stable relation role")
	}
	types := make(map[string]string, len(product.Fields))
	collations := make(map[string]string, len(product.Fields))
	versions := make(map[string]string, len(product.Fields))
	for _, field := range product.Fields {
		types[field.Name] = field.Type
		collations[field.Name] = field.Collation
		versions[field.Name] = field.CollationVersion
	}
	normal, err := queryplan.NormalizeV2(plan, queryplan.Product{
		Name: product.Name, Columns: approvedColumns, AllowedAggregates: allowedAggregates,
		ColumnTypes: types, ColumnCollations: collations, CollationVersions: versions,
		SourceNamespace: product.FactNamespace, Snapshot: product.Snapshot,
		StableEntityKey: append([]string(nil), product.EntityKey...),
		LineageDigest:   product.LineageManifestDigest,
	})
	if err != nil {
		return "", err
	}
	return normal.Digest()
}

// ------------------------------------------------------------ policy grants

// preparationPolicyGrant builds the sqlpolicy grant from the task authorization
// and the Catalog.
//
// It is produced here rather than accepted from the caller so that the Gateway
// and the finalizer authorize identically. A grant supplied by the party being
// checked would make the whole comparison circular.
func preparationPolicyGrant(grant Grant, view CatalogView) (sqlpolicy.Grant, error) {
	var scope map[string]any
	if len(grant.MandatoryScope) > 0 {
		if err := json.Unmarshal(grant.MandatoryScope, &scope); err != nil {
			return sqlpolicy.Grant{}, fmt.Errorf("preparation grant mandatory scope is not JSON: %w", err)
		}
	}
	// The products are taken in the order the task grant declares them rather
	// than sorted. Order does not affect authorization -- sqlpolicy resolves a
	// product by logical name -- and PolicyGrantSHA256 canonicalizes before
	// digesting, so the identity is order-insensitive either way. What sorting
	// here WOULD change is the value the Gateway hands the policy engine, and a
	// derivation being extracted must not alter that as a side effect.
	products := make([]sqlpolicy.ProductGrant, 0, len(grant.ApprovedProducts))
	for _, name := range grant.ApprovedProducts {
		product, found := view.LookupProduct(name)
		if !found {
			return sqlpolicy.Grant{}, fmt.Errorf("this task references data product %q, "+
				"which this Catalog does not declare", name)
		}
		granted, err := catalogProductGrant(product, grant.ApprovedColumns[name], scope)
		if err != nil {
			return sqlpolicy.Grant{}, err
		}
		products = append(products, granted)
	}
	return sqlpolicy.Grant{Products: products}, nil
}

func catalogProductGrant(product catalog.Product, approvedColumns []string,
	scope map[string]any) (sqlpolicy.ProductGrant, error) {
	schema, view, found := strings.Cut(product.ReportingView, ".")
	if !found || strings.TrimSpace(schema) == "" || strings.TrimSpace(view) == "" ||
		strings.Contains(view, ".") {
		return sqlpolicy.ProductGrant{}, fmt.Errorf("product %q has reporting view %q, "+
			"which is not schema.relation", product.Name, product.ReportingView)
	}
	for _, name := range product.Scopes {
		if _, present := scope[name]; !present {
			return sqlpolicy.ProductGrant{}, fmt.Errorf(
				"this task carries no value for mandatory scope %q of product %q", name, product.Name)
		}
	}
	predicates, err := scopePredicates(product, scope)
	if err != nil {
		return sqlpolicy.ProductGrant{}, err
	}
	return sqlpolicy.ProductGrant{
		LogicalName: product.Name, PhysicalSchema: schema, PhysicalView: view,
		ApprovedColumns:   append([]string(nil), approvedColumns...),
		AllowedFunctions:  append([]string(nil), product.AllowedFunctions...),
		AllowedAggregates: append([]string(nil), product.AllowedAggregates...),
		AllowedOperators:  append([]string(nil), product.AllowedOperators...),
		MandatoryScope:    predicates,
	}, nil
}

// scopePredicates turns the task's stored scope into policy predicates.
//
// Only scopes the product actually publishes become predicates: a task-wide
// scope naming a column another product carries is not a predicate this one can
// be filtered on, and rendering it would produce a statement referencing a
// column that does not exist.
func scopePredicates(product catalog.Product, scope map[string]any) ([]sqlpolicy.ScopePredicate, error) {
	published := make(map[string]struct{}, len(product.Fields))
	for _, field := range product.Fields {
		published[field.Name] = struct{}{}
	}
	var predicates []sqlpolicy.ScopePredicate
	for _, name := range sortedKeys(scope) {
		if _, relevant := published[name]; !relevant {
			continue
		}
		switch value := scope[name].(type) {
		case string:
			predicates = append(predicates, sqlpolicy.ScopePredicate{
				Column: name, Operator: sqlpolicy.ScopeEqual, Values: []string{value}})
		case []any:
			values := make([]string, 0, len(value))
			for _, item := range value {
				text, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("mandatory scope %q carries a non-string enum value", name)
				}
				values = append(values, text)
			}
			operator := sqlpolicy.ScopeIn
			if len(values) == 1 {
				operator = sqlpolicy.ScopeEqual
			}
			predicates = append(predicates, sqlpolicy.ScopePredicate{
				Column: name, Operator: operator, Values: values})
		case map[string]any:
			if from, _ := value["from"].(string); from != "" {
				predicates = append(predicates, sqlpolicy.ScopePredicate{
					Column: name, Operator: sqlpolicy.ScopeGreaterEqual, Values: []string{from}})
			}
			if to, _ := value["to"].(string); to != "" {
				predicates = append(predicates, sqlpolicy.ScopePredicate{
					Column: name, Operator: sqlpolicy.ScopeLessEqual, Values: []string{to}})
			}
		default:
			return nil, fmt.Errorf("mandatory scope %q carries an unsupported value", name)
		}
	}
	return predicates, nil
}

// widenForMetering adds the metering projection to the accounted product's
// approved columns.
//
// This is a real privilege increase over what the task approved, and it is
// bounded here: only the accounted product is widened, and only by columns
// deriveExposureShape already required to be published.
func widenForMetering(grant sqlpolicy.Grant, product string,
	metering []string) (sqlpolicy.Grant, error) {
	result := clonePolicyGrant(grant)
	found := false
	for index := range result.Products {
		if result.Products[index].LogicalName != product {
			continue
		}
		found = true
		columns := stringSet(result.Products[index].ApprovedColumns)
		for _, column := range metering {
			columns[column] = struct{}{}
		}
		result.Products[index].ApprovedColumns = sortedSet(columns)
	}
	if !found {
		return sqlpolicy.Grant{}, fmt.Errorf("the accounted product %q is absent from the task grant", product)
	}
	return result, nil
}

// widenForSidecars folds the ordinal sidecar grants into the policy grant.
func widenForSidecars(grant sqlpolicy.Grant,
	sidecars []sqlpolicy.ProductGrant) (sqlpolicy.Grant, error) {
	result := clonePolicyGrant(grant)
	seen := make(map[string]struct{}, len(result.Products)+len(sidecars))
	for _, product := range result.Products {
		if _, duplicate := seen[product.LogicalName]; duplicate {
			return sqlpolicy.Grant{}, fmt.Errorf("duplicate policy product %q", product.LogicalName)
		}
		seen[product.LogicalName] = struct{}{}
	}
	for _, sidecar := range sidecars {
		if _, collision := seen[sidecar.LogicalName]; collision {
			return sqlpolicy.Grant{}, fmt.Errorf(
				"ordinal sidecar logical name %q collides with a task product", sidecar.LogicalName)
		}
		seen[sidecar.LogicalName] = struct{}{}
		result.Products = append(result.Products, cloneProductGrant(sidecar))
	}
	return result, nil
}

// ------------------------------------------------------------- ordinal binding

// ordinalQueryProduct is the compilation product for an ordinal profile.
//
// It differs from the plain one by carrying the snapshot publication and its
// sidecar manifest digest, which is what binds the compiled program to an
// immutable artifact rather than to a relation name.
func ordinalQueryProduct(sources preparationSources, product catalog.Product,
	approved map[string]struct{}) (queryplan.Product, error) {
	publication, found := sources.catalog.LookupSnapshotPublication(product.SnapshotPublication)
	if !found || publication.Source != product.Source ||
		publication.SourceNamespace != product.FactNamespace ||
		publication.Snapshot != product.Snapshot || publication.ManifestDigest == "" {
		return queryplan.Product{}, fmt.Errorf(
			"product %q has no matching immutable snapshot publication", product.Name)
	}
	result := relationalQueryProduct(product, approved)
	result.RequiredEvidence = append([]string(nil), product.Scopes...)
	result.SnapshotPublication = publication.Name
	result.SidecarManifestDigest = publication.ManifestDigest
	return result, nil
}

// relationalQueryProduct is the compilation product carrying the canonical
// semantic identity every profile from V2 onward requires.
func relationalQueryProduct(product catalog.Product, approved map[string]struct{}) queryplan.Product {
	types := make(map[string]string, len(product.Fields))
	collations := make(map[string]string, len(product.Fields))
	versions := make(map[string]string, len(product.Fields))
	for _, field := range product.Fields {
		types[field.Name] = field.Type
		collations[field.Name] = field.Collation
		versions[field.Name] = field.CollationVersion
	}
	aggregates := make(map[string]struct{}, len(product.AllowedAggregates))
	for _, aggregate := range product.AllowedAggregates {
		aggregates[strings.ToLower(strings.TrimSpace(aggregate))] = struct{}{}
	}
	return queryplan.Product{
		Name: product.Name, Columns: approved, AllowedAggregates: aggregates,
		ColumnTypes: types, ColumnCollations: collations, CollationVersions: versions,
		SourceNamespace: product.FactNamespace, Snapshot: product.Snapshot,
		StableRole: product.StableRelationRole, StableEntityKey: append([]string(nil), product.EntityKey...),
		LineageDigest: product.LineageManifestDigest, RequiredEvidence: append([]string(nil), product.Scopes...),
	}
}

// boundOrdinalPreparation is what sidecar binding produces.
type boundOrdinalPreparation struct {
	program            queryplan.OrdinalProgram
	companionSQL       string
	provenanceFields   []string
	dictionarySet      ordinal.DictionarySetManifest
	sidecarGrants      []sqlpolicy.ProductGrant
	sourcePublications map[string]string
	estimatedBaseFacts uint64
}

// bindOrdinalSidecars wraps the compiled provenance statement so it joins each
// pinned immutable sidecar and projects the required handle aliases.
//
// This is the one place trusted Catalog physical sidecars enter generated SQL.
// It reads the artifacts through PreparationInputs.SnapshotBindings rather than
// through a live registry: the Gateway resolves those from its verified registry
// and the finalizer from retained Publication evidence, each having already
// checked them against the Catalog, so the derivation depends on values both
// sides can obtain rather than on whoever holds the registry.
func bindOrdinalSidecars(sources preparationSources, baseSQL string, fields []string,
	program queryplan.OrdinalProgram) (boundOrdinalPreparation, error) {
	if strings.TrimSpace(baseSQL) == "" || len(fields) == 0 || len(program.Sources) == 0 {
		return boundOrdinalPreparation{}, errors.New("ordinal sidecar binding input is incomplete")
	}
	result := boundOrdinalPreparation{program: program,
		sourcePublications: make(map[string]string, len(program.Sources))}

	const innerAlias = "tg_v4_provenance"
	selected := make([]string, 0, len(fields)+len(program.Sources))
	seen := make(map[string]struct{}, len(fields)+len(program.Sources))
	for _, field := range fields {
		if !safeGeneratedIdentifier(field) {
			return boundOrdinalPreparation{}, fmt.Errorf("unsafe generated provenance field %q", field)
		}
		if _, duplicate := seen[field]; duplicate {
			return boundOrdinalPreparation{}, fmt.Errorf("duplicate generated provenance field %q", field)
		}
		seen[field] = struct{}{}
		selected = append(selected, qualified(innerAlias, field)+" AS "+quoted(field))
		result.provenanceFields = append(result.provenanceFields, field)
	}

	type sidecarJoin struct {
		logicalName string
		alias       string
		entity      []queryplan.OrdinalFieldBinding
	}
	joins := make([]sidecarJoin, 0, len(program.Sources))
	joinByKey := map[string]int{}
	members := map[string]ordinal.DictionarySetMember{}
	grants := map[string]sqlpolicy.ProductGrant{}

	register := func(publication catalog.SnapshotPublication) error {
		binding, found := sources.snapshotBindings[publication.Name]
		if !found {
			return fmt.Errorf("publication %q has no resolved snapshot binding", publication.Name)
		}
		if err := binding.RequireCatalog(publication); err != nil {
			return err
		}
		member := ordinal.DictionarySetMember{PublicationName: publication.Name,
			DictionaryDigest: binding.DictionaryDigest, ManifestDigest: binding.ManifestDigest}
		if previous, duplicate := members[publication.Name]; duplicate && previous != member {
			return errors.New("one publication resolves to conflicting dictionary evidence")
		}
		members[publication.Name] = member
		return nil
	}

	for _, source := range program.Sources {
		publication, found := sources.catalog.LookupSnapshotPublication(source.SidecarBinding.PublicationID)
		if !found || publication.Name == "" ||
			source.SidecarBinding.ManifestDigest != publication.ManifestDigest ||
			publication.SourceNamespace != source.SourceNamespace ||
			publication.Snapshot != source.Snapshot {
			return boundOrdinalPreparation{}, fmt.Errorf(
				"source %q is not bound to its Catalog snapshot publication", source.SourceAlias)
		}
		if err := register(publication); err != nil {
			return boundOrdinalPreparation{}, err
		}
		result.sourcePublications[source.SourceAlias] = publication.Name

		if !safeGeneratedIdentifier(source.HandleAlias) {
			return boundOrdinalPreparation{}, fmt.Errorf("unsafe generated handle alias %q", source.HandleAlias)
		}
		if _, duplicate := seen[source.HandleAlias]; duplicate {
			return boundOrdinalPreparation{}, fmt.Errorf("duplicate generated handle alias %q", source.HandleAlias)
		}
		seen[source.HandleAlias] = struct{}{}

		logical := sidecarLogicalName(publication.Name)
		schema, view, ok := strings.Cut(publication.OrdinalSidecar, ".")
		if !ok || !safeGeneratedIdentifier(schema) || !safeGeneratedIdentifier(view) {
			return boundOrdinalPreparation{}, errors.New("Catalog ordinal sidecar identifier is invalid")
		}
		approvedColumns := []string{"row_handle"}
		entityParts := make([]string, 0, len(source.EntityKey))
		for _, keyField := range source.EntityKey {
			if !safeGeneratedIdentifier(keyField.Column) ||
				!safeGeneratedIdentifier(keyField.ProvenanceAlias) {
				return boundOrdinalPreparation{}, errors.New("ordinal entity-key binding is invalid")
			}
			approvedColumns = append(approvedColumns, keyField.Column)
			entityParts = append(entityParts, keyField.Column+"\x00"+keyField.ProvenanceAlias)
		}
		joinKey := publication.Name + "\x01" + strings.Join(entityParts, "\x01")
		joinIndex, present := joinByKey[joinKey]
		if !present {
			joinIndex = len(joins)
			joinByKey[joinKey] = joinIndex
			joins = append(joins, sidecarJoin{logicalName: logical,
				alias:  fmt.Sprintf("tg_v4_sidecar_%d", len(joins)),
				entity: append([]queryplan.OrdinalFieldBinding(nil), source.EntityKey...)})
		}
		selected = append(selected,
			qualified(joins[joinIndex].alias, "row_handle")+" AS "+quoted(source.HandleAlias))
		result.provenanceFields = append(result.provenanceFields, source.HandleAlias)
		if existing, present := grants[logical]; present {
			if existing.PhysicalSchema != schema || existing.PhysicalView != view {
				return boundOrdinalPreparation{}, errors.New(
					"one ordinal sidecar logical name resolves to conflicting physical bindings")
			}
			existing.ApprovedColumns = canonicalStrings(append(existing.ApprovedColumns, approvedColumns...))
			grants[logical] = existing
		} else {
			grants[logical] = sqlpolicy.ProductGrant{LogicalName: logical, PhysicalSchema: schema,
				PhysicalView: view, ApprovedColumns: canonicalStrings(approvedColumns),
				AllowedOperators: []string{"="}}
		}
	}
	// The dictionary universe is Catalog-wide by construction. A query-local
	// member set would pin a root ledger to whichever product it first executed
	// and make exact cross-product union impossible.
	for _, publication := range sources.catalog.SnapshotPublications {
		if err := register(publication); err != nil {
			return boundOrdinalPreparation{}, err
		}
	}

	var sql strings.Builder
	sql.WriteString("SELECT ")
	sql.WriteString(strings.Join(selected, ", "))
	sql.WriteString(" FROM (")
	sql.WriteString(baseSQL)
	sql.WriteString(") AS ")
	sql.WriteString(quoted(innerAlias))
	for _, join := range joins {
		sql.WriteString(" INNER JOIN ")
		sql.WriteString(quoted(join.logicalName))
		sql.WriteString(" AS ")
		sql.WriteString(quoted(join.alias))
		sql.WriteString(" ON ")
		predicates := make([]string, 0, len(join.entity))
		for _, keyField := range join.entity {
			predicates = append(predicates,
				qualified(innerAlias, keyField.ProvenanceAlias)+" = "+qualified(join.alias, keyField.Column))
		}
		if len(predicates) == 0 {
			return boundOrdinalPreparation{}, errors.New("ordinal sidecar join has no entity key")
		}
		sql.WriteString(strings.Join(predicates, " AND "))
	}
	if len(program.ProvenanceOrder) > 0 {
		orders := make([]string, 0, len(program.ProvenanceOrder))
		for _, order := range program.ProvenanceOrder {
			if !safeGeneratedIdentifier(order.ProvenanceAlias) ||
				(order.Direction != "ASC" && order.Direction != "DESC") {
				return boundOrdinalPreparation{}, errors.New("ordinal provenance order is invalid")
			}
			orders = append(orders, qualified(innerAlias, order.ProvenanceAlias)+" "+order.Direction)
		}
		sql.WriteString(" ORDER BY ")
		sql.WriteString(strings.Join(orders, ", "))
	}
	result.companionSQL = sql.String()

	set := make([]ordinal.DictionarySetMember, 0, len(members))
	for _, member := range members {
		set = append(set, member)
	}
	manifest, err := ordinal.NewDictionarySetManifest(sources.catalog.Digest, set...)
	if err != nil {
		return boundOrdinalPreparation{}, err
	}
	result.dictionarySet = manifest
	for _, grant := range grants {
		result.sidecarGrants = append(result.sidecarGrants, grant)
	}
	sort.Slice(result.sidecarGrants, func(left, right int) bool {
		return result.sidecarGrants[left].LogicalName < result.sidecarGrants[right].LogicalName
	})
	if err := result.program.ValidateBoundSidecars(); err != nil {
		return boundOrdinalPreparation{}, err
	}
	if err := result.program.ValidateProvenanceFields(result.provenanceFields); err != nil {
		return boundOrdinalPreparation{}, err
	}
	result.estimatedBaseFacts = estimateBaseFacts(sources, result, 0)
	return result, nil
}

// estimateBaseFacts is the conservative pre-execution working-set estimate.
//
// It selects the million-fact lane; it is not a budget charge. The row counts
// come from the resolved snapshot bindings rather than from a live index, which
// is what lets the finalizer reproduce it.
func estimateBaseFacts(sources preparationSources, bound boundOrdinalPreparation, scanRowLimit uint64) uint64 {
	var total uint64
	limitedScan := bound.program.Kind == "scan" && len(bound.program.Sources) == 1 &&
		len(bound.program.Groups) == 0 && len(bound.program.Aggregates) == 0 && scanRowLimit > 0
	for _, source := range bound.program.Sources {
		publication, found := bound.sourcePublications[source.SourceAlias]
		if !found {
			return ^uint64(0)
		}
		binding, found := sources.snapshotBindings[publication]
		if !found {
			return ^uint64(0)
		}
		rows := binding.RowCount
		if limitedScan && rows > scanRowLimit {
			rows = scanRowLimit
		}
		perRow := uint64(len(source.EvidenceFields)) + 1
		if rows != 0 && perRow > ^uint64(0)/rows {
			return ^uint64(0)
		}
		facts := rows * perRow
		if facts > ^uint64(0)-total {
			return ^uint64(0)
		}
		total += facts
	}
	return total
}

// derivePredicateFootprint builds the V5 footprint for a single-product plan.
func derivePredicateFootprint(inputs PreparationInputs,
	products map[string]queryplan.Product) (*queryplan.PredicateFootprint, error) {
	scope := sha256.Sum256(append([]byte("TASKGATE-EFFECTIVE-MANDATORY-SCOPE-V1\x00"),
		inputs.Grant.MandatoryScope...))
	footprint, err := queryplan.BuildPredicateFootprint(inputs.Plan, queryplan.PredicateBindings{
		CatalogSHA256: inputs.Catalog.Digest, Products: products,
		ViewBindingSHA256: inputs.Grant.ViewBindingDigest,
	}, hex.EncodeToString(scope[:]), inputs.Grant.PredicateLimits)
	if err != nil {
		return nil, err
	}
	return &footprint, nil
}

// ---------------------------------------------------------------- identifiers

func sidecarLogicalName(publication string) string {
	sum := sha256.Sum256([]byte("taskgate-sidecar-logical\x00" + publication))
	return "ordinal_sidecar_" + hex.EncodeToString(sum[:8])
}

func quoted(value string) string { return `"` + value + `"` }

func qualified(alias, field string) string { return quoted(alias) + "." + quoted(field) }

// safeGeneratedIdentifier admits only what the generated SQL quotes unchanged.
// Anything else is rejected rather than escaped.
func safeGeneratedIdentifier(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || character == '_' ||
			(index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}

// --------------------------------------------------------------- set helpers

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func sortedKeys(values map[string]any) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func clonePlan(plan queryplan.QueryPlan) queryplan.QueryPlan {
	plan.Columns = append([]string(nil), plan.Columns...)
	plan.Aggregates = append([]queryplan.Aggregate(nil), plan.Aggregates...)
	plan.Filters = append([]queryplan.Filter(nil), plan.Filters...)
	plan.GroupBy = append([]string(nil), plan.GroupBy...)
	plan.OrderBy = append([]queryplan.Order(nil), plan.OrderBy...)
	if plan.From != nil {
		from := *plan.From
		if from.Scan != nil {
			scan := *from.Scan
			scan.Filters = append([]queryplan.Filter(nil), scan.Filters...)
			from.Scan = &scan
		}
		if from.Join != nil {
			join := *from.Join
			join.Left.Filters = append([]queryplan.Filter(nil), join.Left.Filters...)
			join.Right.Filters = append([]queryplan.Filter(nil), join.Right.Filters...)
			join.On = append([]queryplan.JoinPredicate(nil), join.On...)
			from.Join = &join
		}
		if from.JoinMany != nil {
			join := *from.JoinMany
			join.Sources = append([]queryplan.Scan(nil), join.Sources...)
			for index := range join.Sources {
				join.Sources[index].Filters = append([]queryplan.Filter(nil), join.Sources[index].Filters...)
			}
			join.On = append([]queryplan.JoinPredicate(nil), join.On...)
			from.JoinMany = &join
		}
		if from.UnionDistinct != nil {
			union := *from.UnionDistinct
			union.Columns = append([]string(nil), union.Columns...)
			union.Left.Filters = append([]queryplan.Filter(nil), union.Left.Filters...)
			union.Right.Filters = append([]queryplan.Filter(nil), union.Right.Filters...)
			from.UnionDistinct = &union
		}
		plan.From = &from
	}
	return plan
}
