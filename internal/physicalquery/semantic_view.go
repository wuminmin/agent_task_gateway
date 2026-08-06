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
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

const (
	governanceEnvelopeDomain = "TASKGATE-SEMANTIC-VIEW-GOVERNANCE-ENVELOPE-V1"
	terminalClosureDomain    = "TASKGATE-SEMANTIC-VIEW-TERMINAL-CLOSURE-V1"

	// The trailing NUL is part of the domain string, not added by the hasher --
	// see viewEvidenceDigest.
	viewPredicateExpressionDomain = "TASKGATE-V5-VIEW-PREDICATE-EXPRESSION-V1\x00"
)

// viewEvidenceDigest reproduces the Gateway's view-evidence digest exactly.
//
// It deliberately does NOT use domainDigest. That helper canonicalizes with
// approval.CanonicalJSON, while the digest already embedded in every V5
// predicate footprint the Gateway has produced is over encoding/json's default
// output with the domain prefixed literally. The two encoders do not agree byte
// for byte, so canonicalizing here would silently change ResolvedExpressionSHA256
// and with it the whole footprint -- a preparation that no longer reproduces
// what was executed. The construction is preserved as it is; changing it is a
// contract-release decision, not an extraction detail.
func viewEvidenceDigest(domain string, value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(domain), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

// PrepareSemanticView derives a semantic View operation's physical statements
// from immutable inputs.
//
// It is the View counterpart of Prepare and exists for the same reason: the
// Gateway calls it before executing and the finalizer calls it afterwards from
// independently obtained frozen evidence, so both hand the same values to the
// same code. What it does NOT do is anything the Gateway must still own --
// looking a ViewBinding up in the Control Store, verifying a registry signature,
// decoding results, building an exposure Observation, shaping a response. Those
// read live state or produce Gateway-shaped output; preparation is a function of
// its inputs, which is what makes the two calls comparable.
func PrepareSemanticView(inputs SemanticViewPreparationInputsV1) (PreparedOperation, error) {
	compiler, err := LocalCompilerIdentity()
	if err != nil {
		return PreparedOperation{}, err
	}
	return PrepareSemanticViewWith(inputs, compiler)
}

// PrepareSemanticViewWith is PrepareSemanticView against a stated compiler
// identity, for a finalizer reproducing a preparation made by another binary.
func PrepareSemanticViewWith(inputs SemanticViewPreparationInputsV1,
	compiler CompilerIdentityV1) (PreparedOperation, error) {
	if err := inputs.Validate(); err != nil {
		return PreparedOperation{}, err
	}
	draft, governance, err := deriveSemanticViewPreparation(inputs)
	if err != nil {
		return PreparedOperation{}, err
	}
	identities, err := inputs.identities(governance)
	if err != nil {
		return PreparedOperation{}, err
	}
	return newPreparedOperation(identities, compiler, draft)
}

// identities derives the digest set for a semantic View preparation.
//
// The four View members are measured from what preparation actually read rather
// than taken from the ViewBinding, for the reason recorded on the binding type.
func (inputs SemanticViewPreparationInputsV1) identities(
	governance semanticViewGovernance) (preparationIdentities, error) {
	set := preparationIdentities{
		catalog:     inputs.Catalog.Digest,
		viewBinding: inputs.ViewBindingDigest,
	}
	var err error
	if set.inputs, err = inputs.SHA256(); err != nil {
		return preparationIdentities{}, err
	}
	if set.grant, err = inputs.Grant.SHA256(); err != nil {
		return preparationIdentities{}, err
	}
	// The outer plan is what PlanSHA256 identifies for a View operation. The
	// composed plan is a derived value and has its own digest; signing it as
	// though it were the submitted plan would leave the agent's actual query
	// unnamed.
	if set.plan, err = inputs.OuterPlanSHA256(); err != nil {
		return preparationIdentities{}, err
	}
	if set.snapshots, err = inputs.SnapshotBindingSetSHA256(); err != nil {
		return preparationIdentities{}, err
	}
	if set.viewRevision, err = textDigest(viewRevisionDomain, inputs.ExpectedRevisionDigest); err != nil {
		return preparationIdentities{}, err
	}
	if set.viewArtifact, err = inputs.ArtifactSHA256(); err != nil {
		return preparationIdentities{}, err
	}
	if set.viewComposition, err = inputs.CompositionSHA256(); err != nil {
		return preparationIdentities{}, err
	}
	if set.terminalClosure, err = governance.terminalClosureSHA256(); err != nil {
		return preparationIdentities{}, err
	}
	if set.governanceEnvelope, err = governance.envelopeSHA256(); err != nil {
		return preparationIdentities{}, err
	}
	return set, nil
}

// deriveSemanticViewPreparation is the moved derivation.
//
// It follows the Gateway's order exactly, because the order is load-bearing:
// governance is proven before any terminal column is introduced, the column
// closure is computed before compilation so a leaf filter cannot project the
// whole Catalog schema, and the internal grant is built from the compilation's
// own positive-evidence closure rather than from the validator snapshot.
func deriveSemanticViewPreparation(inputs SemanticViewPreparationInputsV1) (
	OperationDraft, semanticViewGovernance, error) {
	root, err := inputs.RootProduct()
	if err != nil {
		return OperationDraft{}, semanticViewGovernance{}, err
	}
	// The root must be one the task actually approved. A View preparation that
	// skipped this would compile terminal statements for a public relation the
	// approval never covered.
	if !containsString(inputs.Grant.ApprovedProducts, root.Name) {
		return OperationDraft{}, semanticViewGovernance{}, fmt.Errorf(
			"this task does not approve semantic View root %q", root.Name)
	}
	governance, err := semanticViewGovernanceFor(inputs.Catalog, root, inputs.Artifact)
	if err != nil {
		return OperationDraft{}, semanticViewGovernance{}, err
	}
	// Terminal Products are private. They must never appear in the task's public
	// ApprovedProducts: that list is what the agent's own plan is compiled and
	// authorized against, so a terminal Product in it would let the agent query
	// the underlying relation directly instead of only through the View.
	for name := range governance.products {
		if containsString(inputs.Grant.ApprovedProducts, name) {
			return OperationDraft{}, semanticViewGovernance{}, fmt.Errorf(
				"semantic View terminal product %q appears in the task's public approved products", name)
		}
	}
	terminalScopes, err := governance.terminalScopeValues(inputs.Grant.MandatoryScope)
	if err != nil {
		return OperationDraft{}, semanticViewGovernance{}, err
	}

	requiredColumns, err := semanticPlanRequiredColumns(inputs.Composition.Plan, governance.products)
	if err != nil {
		return OperationDraft{}, semanticViewGovernance{}, err
	}
	sources := inputs.sources()
	queryProducts := make(map[string]queryplan.Product, len(governance.products))
	for name, product := range governance.products {
		columns := requiredColumns[name]
		queryProduct := relationalQueryProduct(product, columns)
		if inputs.Grant.UsesOrdinalProgram() {
			if queryProduct, err = ordinalQueryProduct(sources, product, columns); err != nil {
				return OperationDraft{}, semanticViewGovernance{}, err
			}
		}
		queryProducts[name] = queryProduct
	}
	compiled, err := queryplan.CompileRelational(inputs.Composition.Plan, queryProducts)
	if err != nil {
		return OperationDraft{}, semanticViewGovernance{}, err
	}

	// CompileRelational computes the exact positive-output evidence closure. Only
	// that closure is allowed into the private terminal grant; the artifact's
	// field list was a validator snapshot for a trusted artifact, not authority.
	internalApproved := make(map[string][]string, len(governance.products))
	for _, source := range compiled.Sources {
		set := stringSet(internalApproved[source.Product])
		for _, field := range source.EvidenceFields {
			set[field] = struct{}{}
		}
		internalApproved[source.Product] = sortedSet(set)
	}
	shape, err := deriveRelationalShape(inputs.Composition.Plan, internalApproved, compiled, governance.products)
	if err != nil {
		return OperationDraft{}, semanticViewGovernance{}, err
	}

	draft := OperationDraft{
		VisibleSQL:   compiled.VisibleSQL,
		CompanionSQL: compiled.ProvenanceSQL,
		// The visible and fact projections are the Composition's declared public
		// field names, not the compilation's internal ones: what the agent sees is
		// the View's public interface.
		VisibleFields:    append([]string(nil), inputs.Composition.VisibleFields...),
		FactFields:       append([]string(nil), inputs.Composition.VisibleFields...),
		ProvenanceFields: append([]string(nil), compiled.ProvenanceFields...),
		Grouped: len(inputs.Composition.Plan.GroupBy) > 0 ||
			len(inputs.Composition.Plan.Aggregates) > 0,
		ExpandedEvidence: compiled.ExpandedEvidence,
		NormalFormSHA256: shape.normalFormSHA256,
	}

	if inputs.Grant.UsesOrdinalProgram() {
		bound, bindErr := bindOrdinalSidecars(sources, compiled.ProvenanceSQL,
			compiled.ProvenanceFields, compiled.OrdinalProgram)
		if bindErr != nil {
			return OperationDraft{}, semanticViewGovernance{}, bindErr
		}
		draft.CompanionSQL = bound.companionSQL
		draft.ProvenanceFields = bound.provenanceFields
		draft.OrdinalProgram = &bound.program
		draft.DictionarySet = &bound.dictionarySet
		draft.SidecarGrants = bound.sidecarGrants
		draft.SourcePublications = bound.sourcePublications
		draft.EstimatedBaseFacts = bound.estimatedBaseFacts
	}
	if inputs.Grant.UsesPredicateFootprint() {
		footprint, footprintErr := deriveViewPredicateFootprint(inputs, root, queryProducts)
		if footprintErr != nil {
			return OperationDraft{}, semanticViewGovernance{}, footprintErr
		}
		draft.PredicateFootprint = footprint
		// V5 identifies a composed View by the V4 algebra normal form, not by the
		// V2-era semantic one the relational shape computed. This is the same
		// replacement deriveRelationalPreparation makes, and it is what the
		// exposure ledger keys on -- carrying the earlier form would identify the
		// query by something its ledger does not use.
		normal, normalizeErr := queryplan.SemanticNormalFormV4(inputs.Composition.Plan, compiled, queryProducts)
		if normalizeErr != nil {
			return OperationDraft{}, semanticViewGovernance{}, normalizeErr
		}
		draft.NormalFormSHA256 = normal.SHA256
	}

	// The grant is built in production's order: the task's own public grant
	// first, then the internal terminal products merged into it, then the metering
	// widening, then the sidecars.
	//
	// The public grant is the starting point rather than the terminal set alone.
	// The root View relation stays in it because the task was approved against the
	// root, and dropping it here would narrow what production admits -- an
	// extraction must not change the authorization, in either direction.
	policy, err := preparationPolicyGrant(inputs.Grant, inputs.Catalog)
	if err != nil {
		return OperationDraft{}, semanticViewGovernance{}, err
	}
	internal := make(map[string]sqlpolicy.ProductGrant, len(governance.products))
	for name, product := range governance.products {
		granted, grantErr := catalogProductGrant(product, shape.meteringByProduct[name], terminalScopes[name])
		if grantErr != nil {
			return OperationDraft{}, semanticViewGovernance{}, grantErr
		}
		internal[name] = granted
	}
	if policy, err = mergeInternalTerminalGrants(policy, internal); err != nil {
		return OperationDraft{}, semanticViewGovernance{}, err
	}
	for _, name := range sortedStrings(keysOfProducts(governance.products)) {
		if policy, err = widenForMetering(policy, name, shape.meteringByProduct[name]); err != nil {
			return OperationDraft{}, semanticViewGovernance{}, err
		}
	}
	if len(draft.SidecarGrants) > 0 {
		if policy, err = widenForSidecars(policy, draft.SidecarGrants); err != nil {
			return OperationDraft{}, semanticViewGovernance{}, err
		}
	}
	draft.PolicyGrant = policy
	return draft, governance, nil
}

// mergeInternalTerminalGrants folds the private terminal grants into the task's
// public one.
//
// A terminal Product the public grant already names is merged rather than
// appended -- columns unioned, scope predicates added if absent -- because two
// grants for one logical name would leave which of them authorizes a statement
// undetermined. A physical relation conflict is a refusal: the same logical name
// resolving to two relations means the View and the task disagree about what
// they are reading.
func mergeInternalTerminalGrants(grant sqlpolicy.Grant,
	internal map[string]sqlpolicy.ProductGrant) (sqlpolicy.Grant, error) {
	if len(internal) == 0 {
		return grant, nil
	}
	result := clonePolicyGrant(grant)
	names := make([]string, 0, len(internal))
	for name := range internal {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		terminal := cloneProductGrant(internal[name])
		found := false
		for index := range result.Products {
			if result.Products[index].LogicalName != name {
				continue
			}
			found = true
			if result.Products[index].PhysicalSchema != terminal.PhysicalSchema ||
				result.Products[index].PhysicalView != terminal.PhysicalView {
				return sqlpolicy.Grant{}, fmt.Errorf(
					"semantic View terminal product %q conflicts with the task grant", name)
			}
			columns := stringSet(result.Products[index].ApprovedColumns)
			for _, column := range terminal.ApprovedColumns {
				columns[column] = struct{}{}
			}
			result.Products[index].ApprovedColumns = sortedSet(columns)
			for _, predicate := range terminal.MandatoryScope {
				if !containsScopePredicate(result.Products[index].MandatoryScope, predicate) {
					cloned := predicate
					cloned.Values = append([]string(nil), predicate.Values...)
					result.Products[index].MandatoryScope = append(result.Products[index].MandatoryScope, cloned)
				}
			}
			break
		}
		if !found {
			result.Products = append(result.Products, terminal)
		}
	}
	return result, nil
}

func containsScopePredicate(predicates []sqlpolicy.ScopePredicate, want sqlpolicy.ScopePredicate) bool {
	for _, predicate := range predicates {
		if predicate.Column != want.Column || predicate.Operator != want.Operator ||
			len(predicate.Values) != len(want.Values) {
			continue
		}
		same := true
		for index, value := range predicate.Values {
			if want.Values[index] != value {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// deriveViewPredicateFootprint builds the V5 footprint for a semantic View.
//
// The caller-visible predicates are the agent's outer-plan filters, which name
// public View outputs, so each is resolved through the Artifact's output mapping
// to the terminal FieldID it actually constrains. Moving only the SQL
// compilation and leaving this behind would have left the Gateway and the
// finalizer preparing one View with two different predicate footprints.
func deriveViewPredicateFootprint(inputs SemanticViewPreparationInputsV1, root catalog.Product,
	queryProducts map[string]queryplan.Product) (*queryplan.PredicateFootprint, error) {
	outputByName := make(map[string]viewcompiler.Output, len(inputs.Artifact.Outputs))
	for _, output := range inputs.Artifact.Outputs {
		outputByName[output.Name] = output
	}
	stableRole := root.StableRelationRole
	if stableRole == "" {
		stableRole = root.Name
	}
	callerFilters := make([]queryplan.PredicateFilterBinding, 0, len(inputs.OuterPlan.Filters))
	fieldBindings := make(map[string]queryplan.PredicateFieldBinding, len(inputs.OuterPlan.Filters))
	for _, filter := range inputs.OuterPlan.Filters {
		output, present := outputByName[filter.Column]
		if !present || output.Kind != viewcompiler.OutputField || output.FieldID == "" {
			return nil, fmt.Errorf("semantic View filter on %q does not resolve to a public base-field output",
				filter.Column)
		}
		resolved, err := viewEvidenceDigest(viewPredicateExpressionDomain, struct {
			Binding string              `json:"binding"`
			Plan    string              `json:"plan"`
			Output  viewcompiler.Output `json:"output"`
		}{Binding: inputs.ViewBindingDigest, Plan: inputs.Artifact.CanonicalPlanDigest, Output: output})
		if err != nil {
			return nil, err
		}
		fieldBindings[output.FieldID] = queryplan.PredicateFieldBinding{
			SemanticProductID: root.Name, StableRole: stableRole, PublicFieldID: output.Name,
			ResolvedExpressionSHA256: resolved, SQLType: output.SQLType,
			CollationName: output.Collation, CollationVersion: output.CollationVersion,
		}
		callerFilters = append(callerFilters, queryplan.PredicateFilterBinding{
			Field:  output.FieldID,
			Filter: queryplan.Filter{Column: output.FieldID, Op: filter.Op, Value: filter.Value},
		})
	}
	scope := sha256.Sum256(append([]byte("TASKGATE-EFFECTIVE-MANDATORY-SCOPE-V1\x00"),
		inputs.Grant.MandatoryScope...))
	footprint, err := queryplan.BuildPredicateFootprint(inputs.Composition.Plan, queryplan.PredicateBindings{
		CatalogSHA256: inputs.Catalog.Digest, Products: queryProducts,
		ViewBindingSHA256: inputs.ViewBindingDigest,
		CallerFilters:     callerFilters, Fields: fieldBindings,
		SemanticProductID: root.Name,
	}, hex.EncodeToString(scope[:]), inputs.Grant.PredicateLimits)
	if err != nil {
		return nil, err
	}
	return &footprint, nil
}

func keysOfProducts(products map[string]catalog.Product) []string {
	names := make([]string, 0, len(products))
	for name := range products {
		names = append(names, name)
	}
	return names
}

// semanticViewGovernance is the proven terminal envelope of one View.
type semanticViewGovernance struct {
	// products are the terminal Products the View reaches, by name.
	products map[string]catalog.Product
	// rootScopeByColumn maps each terminal product's scope column to the public
	// root scope that propagates into it.
	rootScopeByColumn map[string]map[string]string
	// root is the public View relation the envelope is measured against.
	root catalog.Product
}

// semanticViewGovernanceFor proves that every terminal source remains inside the
// root's source, sensitivity, stable-role and dynamic-scope envelope.
//
// Scope propagation is explicit: a public root scope output must resolve to one
// concrete terminal FieldID, and every terminal mandatory scope must be covered.
// Join equalities and constants are never treated as scope proofs -- an equality
// between two columns says they are equal, not that either is confined to what
// the task was granted.
//
// It is a function of the CatalogView rather than a method on a Gateway Service,
// which is the whole point of moving it: the Gateway proved this against its
// live Catalog and the finalizer must reach the same verdict from the frozen
// one, and it can only do that if the proof reads a value both sides hold.
func semanticViewGovernanceFor(view CatalogView, root catalog.Product,
	artifact viewcompiler.Artifact) (semanticViewGovernance, error) {
	result := semanticViewGovernance{
		root:              root,
		products:          make(map[string]catalog.Product, len(artifact.BaseProducts)),
		rootScopeByColumn: make(map[string]map[string]string, len(artifact.BaseProducts)),
	}
	scans, err := semanticArtifactScans(artifact.Plan)
	if err != nil {
		return semanticViewGovernance{}, err
	}
	if len(scans) != len(artifact.BaseProducts) {
		return semanticViewGovernance{}, errors.New("semantic View base-product closure is inconsistent")
	}
	artifactBases := make(map[string]struct{}, len(artifact.BaseProducts))
	for _, name := range artifact.BaseProducts {
		if _, duplicate := artifactBases[name]; duplicate {
			return semanticViewGovernance{}, errors.New("semantic View repeats a base-product binding")
		}
		artifactBases[name] = struct{}{}
	}
	rootSensitivity, err := root.EffectiveSensitivity()
	if err != nil {
		return semanticViewGovernance{}, err
	}
	roleProduct := make(map[string]string, len(scans))
	seenProducts := make(map[string]struct{}, len(scans))
	for _, scan := range scans {
		if _, duplicate := roleProduct[scan.Role]; duplicate {
			return semanticViewGovernance{}, errors.New("semantic View repeats a stable role")
		}
		if _, duplicate := seenProducts[scan.Product]; duplicate {
			return semanticViewGovernance{}, errors.New("semantic View repeats a terminal product")
		}
		terminal, present := view.LookupProduct(scan.Product)
		if !present || terminal.ViewContract != nil || terminal.Source != root.Source ||
			terminal.StableRelationRole != scan.Role {
			return semanticViewGovernance{}, errors.New("semantic View terminal product is not governed by the root source")
		}
		terminalSensitivity, sensitivityErr := terminal.EffectiveSensitivity()
		if sensitivityErr != nil || !terminalSensitivity.AtMost(rootSensitivity) {
			return semanticViewGovernance{}, errors.New("semantic View root sensitivity is below a terminal product")
		}
		roleProduct[scan.Role] = scan.Product
		seenProducts[scan.Product] = struct{}{}
		result.products[scan.Product] = terminal
		result.rootScopeByColumn[scan.Product] = make(map[string]string)
	}
	for name := range artifactBases {
		if _, present := seenProducts[name]; !present {
			return semanticViewGovernance{}, errors.New("semantic View artifact names an unreachable terminal product")
		}
	}
	for name := range seenProducts {
		if _, present := artifactBases[name]; !present {
			return semanticViewGovernance{}, errors.New("semantic View artifact omits a reachable terminal product")
		}
	}

	outputs := make(map[string]viewcompiler.Output, len(artifact.Outputs))
	for _, output := range artifact.Outputs {
		if _, duplicate := outputs[output.Name]; duplicate {
			return semanticViewGovernance{}, errors.New("semantic View repeats a public output")
		}
		outputs[output.Name] = output
	}
	for _, rootScope := range root.Scopes {
		output, present := outputs[rootScope]
		if !present || output.Kind != viewcompiler.OutputField {
			return semanticViewGovernance{}, errors.New("semantic View scope is not a public base-field output")
		}
		role, terminalColumn, valid := splitRelationalField(output.FieldID)
		productName, present := roleProduct[role]
		if !valid || !present {
			return semanticViewGovernance{}, errors.New("semantic View scope has no terminal field binding")
		}
		terminal := result.products[productName]
		if !containsString(terminal.Scopes, terminalColumn) ||
			!view.EquivalentScopePolicies(rootScope, terminalColumn) {
			return semanticViewGovernance{}, errors.New("semantic View scope does not match a terminal mandatory scope")
		}
		if _, duplicate := result.rootScopeByColumn[productName][terminalColumn]; duplicate {
			return semanticViewGovernance{}, errors.New("semantic View maps multiple root scopes to one terminal scope")
		}
		result.rootScopeByColumn[productName][terminalColumn] = rootScope
	}
	for productName, terminal := range result.products {
		for _, terminalScope := range terminal.Scopes {
			if _, covered := result.rootScopeByColumn[productName][terminalScope]; !covered {
				return semanticViewGovernance{}, errors.New("semantic View does not expose every terminal mandatory scope")
			}
		}
	}
	return result, nil
}

// terminalClosureSHA256 identifies which terminal Products the View reaches and
// what each of them is.
//
// The whole Product value is digested, not merely the name. A closure that named
// its members would not move when a terminal Product's source, sensitivity,
// stable role or scope list changed underneath it, and those are exactly the
// properties the envelope was proven against.
func (governance semanticViewGovernance) terminalClosureSHA256() (string, error) {
	products := make([]catalog.Product, 0, len(governance.products))
	for _, product := range governance.products {
		products = append(products, product)
	}
	sort.Slice(products, func(left, right int) bool { return products[left].Name < products[right].Name })
	return domainDigest(terminalClosureDomain, products)
}

// envelopeSHA256 identifies the governance decision itself: the root it was
// measured against and every proven root-scope-to-terminal-scope propagation.
func (governance semanticViewGovernance) envelopeSHA256() (string, error) {
	type propagation struct {
		Product        string `json:"product"`
		TerminalColumn string `json:"terminal_column"`
		RootScope      string `json:"root_scope"`
	}
	var propagations []propagation
	for product, bindings := range governance.rootScopeByColumn {
		for column, rootScope := range bindings {
			propagations = append(propagations, propagation{product, column, rootScope})
		}
	}
	sort.Slice(propagations, func(left, right int) bool {
		if propagations[left].Product != propagations[right].Product {
			return propagations[left].Product < propagations[right].Product
		}
		return propagations[left].TerminalColumn < propagations[right].TerminalColumn
	})
	return domainDigest(governanceEnvelopeDomain, struct {
		Root         catalog.Product `json:"root"`
		Propagations []propagation   `json:"scope_propagations"`
	}{governance.root, propagations})
}

func semanticArtifactScans(plan queryplan.QueryPlan) ([]queryplan.Scan, error) {
	if plan.From == nil || plan.Product != "" {
		return nil, errors.New("semantic View artifact is not relational")
	}
	switch {
	case plan.From.Scan != nil && plan.From.Join == nil && plan.From.JoinMany == nil && plan.From.UnionDistinct == nil:
		return []queryplan.Scan{*plan.From.Scan}, nil
	case plan.From.JoinMany != nil && plan.From.Scan == nil && plan.From.Join == nil && plan.From.UnionDistinct == nil:
		return append([]queryplan.Scan(nil), plan.From.JoinMany.Sources...), nil
	default:
		return nil, errors.New("semantic View artifact has an unsupported source operator")
	}
}

// semanticPlanRequiredColumns computes the exact terminal column closure before
// CompileRelational.
//
// Besides minimizing private authority, this keeps a leaf-filter subquery from
// projecting every Catalog field merely because the trusted validator snapshot
// knew the full schema. Widening it to all Catalog fields during the extraction
// would be a silent authority increase, so the closure moves unchanged.
func semanticPlanRequiredColumns(plan queryplan.QueryPlan,
	products map[string]catalog.Product) (map[string]map[string]struct{}, error) {
	scans, err := semanticArtifactScans(plan)
	if err != nil {
		return nil, err
	}
	roleProduct := make(map[string]string, len(scans))
	result := make(map[string]map[string]struct{}, len(scans))
	addColumn := func(productName, column string) error {
		product, present := products[productName]
		if !present {
			return errors.New("semantic plan names an unknown terminal product")
		}
		if _, present := catalogFieldByName(product.Fields, column); !present {
			return errors.New("semantic plan names an unknown terminal field")
		}
		if result[productName] == nil {
			result[productName] = make(map[string]struct{})
		}
		result[productName][column] = struct{}{}
		return nil
	}
	for _, scan := range scans {
		if _, duplicate := roleProduct[scan.Role]; duplicate {
			return nil, errors.New("semantic plan repeats a stable role")
		}
		roleProduct[scan.Role] = scan.Product
		product, present := products[scan.Product]
		if !present {
			return nil, errors.New("semantic plan names an unknown terminal product")
		}
		// The entity key and every mandatory scope are required whether or not the
		// plan projects them: the statement joins on the first and is filtered on
		// the second.
		for _, column := range append(append([]string(nil), product.EntityKey...), product.Scopes...) {
			if err := addColumn(scan.Product, column); err != nil {
				return nil, err
			}
		}
		for _, filter := range scan.Filters {
			if err := addColumn(scan.Product, filter.Column); err != nil {
				return nil, err
			}
		}
	}
	addFieldID := func(fieldID string) error {
		role, column, valid := splitRelationalField(fieldID)
		productName, present := roleProduct[role]
		if !valid || !present {
			return errors.New("semantic plan contains an invalid stable FieldID")
		}
		return addColumn(productName, column)
	}
	for _, fieldID := range append(append([]string(nil), plan.Columns...), plan.GroupBy...) {
		if err := addFieldID(fieldID); err != nil {
			return nil, err
		}
	}
	for _, filter := range plan.Filters {
		if err := addFieldID(filter.Column); err != nil {
			return nil, err
		}
	}
	for _, aggregate := range plan.Aggregates {
		if aggregate.Column != "*" {
			if err := addFieldID(aggregate.Column); err != nil {
				return nil, err
			}
		}
	}
	if plan.From != nil && plan.From.JoinMany != nil {
		for _, predicate := range plan.From.JoinMany.On {
			if err := addFieldID(predicate.Left); err != nil {
				return nil, err
			}
			if err := addFieldID(predicate.Right); err != nil {
				return nil, err
			}
		}
	}
	for name := range products {
		if len(result[name]) == 0 {
			return nil, errors.New("semantic plan terminal has an empty column closure")
		}
	}
	return result, nil
}

// terminalScopeValues resolves each terminal product's mandatory scope to the
// value the task's signed scope carries for the root scope that propagates into
// it.
//
// A root scope with no signed value is a refusal, not a missing filter: the
// terminal statement would otherwise be compiled without the confinement the
// approval was granted under.
func (governance semanticViewGovernance) terminalScopeValues(
	mandatoryScope json.RawMessage) (map[string]map[string]any, error) {
	var signed map[string]any
	if err := json.Unmarshal(mandatoryScope, &signed); err != nil {
		return nil, fmt.Errorf("the task's mandatory scope evidence is not JSON: %w", err)
	}
	values := make(map[string]map[string]any, len(governance.products))
	for productName, bindings := range governance.rootScopeByColumn {
		values[productName] = make(map[string]any, len(bindings))
		for terminalColumn, rootScope := range bindings {
			value, present := signed[rootScope]
			if !present {
				return nil, fmt.Errorf("the task authorization carries no value for mandatory scope %q, "+
					"which semantic View terminal product %q requires", rootScope, productName)
			}
			values[productName][terminalColumn] = value
		}
	}
	return values, nil
}

// splitRelationalField splits a stable FieldID into its role and column.
func splitRelationalField(value string) (string, string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func catalogFieldByName(fields []catalog.Field, name string) (catalog.Field, bool) {
	for _, field := range fields {
		if field.Name == name {
			return field, true
		}
	}
	return catalog.Field{}, false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
