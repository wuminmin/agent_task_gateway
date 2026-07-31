package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

// prepareTaskPlan resolves semantic roots after the exact-replay check and
// before any reservation. Ordinary plans retain the legacy pure preparation
// path. A query-time graph that contains a semantic root is deliberately not
// grafted into another graph in Phase B; the root's own compiled graph may
// still contain up to queryplan.MaxJoinSources terminal products.
func (s *Service) prepareTaskPlan(ctx context.Context, task control.Task, grant control.TaskGrant, plan queryplan.QueryPlan) (preparedQueryPlan, error) {
	if plan.From != nil {
		productNames, err := queryplan.RelationalProductNames(plan)
		if err == nil {
			for _, name := range productNames {
				product, present := s.catalog.LookupProduct(name)
				if present && product.ViewContract != nil {
					return preparedQueryPlan{}, viewQueryUnsupported("SEMANTIC_VIEW_GRAPH_GRAFT")
				}
			}
		}
		return s.preparePlan(grant, plan)
	}

	root, present := s.catalog.LookupProduct(plan.Product)
	if !present || root.ViewContract == nil {
		return s.preparePlan(grant, plan)
	}
	if !contains(grant.ApprovedProducts, root.Name) {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "QueryPlan 请求了任务授权外的数据产品"}
	}
	if !semanticRelationalProfile(grant.Exposure) {
		return preparedQueryPlan{}, viewQueryUnsupported("SEMANTIC_VIEW_EXPOSURE_PROFILE")
	}

	// Validate the agent-authored plan against the signed public root before
	// any terminal field is introduced. This is the non-expansion boundary:
	// compilation below cannot grant a root column the task did not approve.
	approvedRoot := stringSetFromSlice(grant.ApprovedColumns[root.Name])
	rootAggregates := stringSetFromSlice(root.AllowedAggregates)
	rootSQL, err := queryplan.Compile(plan, queryplan.Product{
		Name: root.Name, Columns: approvedRoot, AllowedAggregates: rootAggregates,
	})
	if err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "QueryPlan 无法在语义 View 的任务授权内编译"}
	}
	publicPolicy, err := s.policyGrant(grant)
	if err != nil {
		return preparedQueryPlan{}, err
	}
	if _, err := sqlpolicy.New(sqlpolicy.Config{}).Authorize(sqlpolicy.Request{
		SQL: rootSQL, Grant: publicPolicy, RowLimit: 1,
	}); err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "QueryPlan 使用了语义 View root 未授权的运算"}
	}

	current, err := s.currentTaskViewBinding(ctx, task, grant)
	if err != nil {
		return preparedQueryPlan{}, err
	}
	artifact, present := current.Artifacts[root.Name]
	if !present {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务的语义 View binding 缺少可执行计划"}
	}
	composition, err := viewcompiler.ComposeQueryPlan(root.Name, plan, artifact)
	if err != nil {
		return preparedQueryPlan{}, viewQueryUnsupported(viewCompositionReason(err))
	}
	return s.prepareSemanticViewPlan(grant, root, artifact, composition, current)
}

func semanticRelationalProfile(grant control.ExposureGrant) bool {
	if !grant.Enabled() {
		return false
	}
	switch grant.ProfileVersion {
	case exposure.ProfileV2, exposure.ProfileV3, exposure.ProfileV4:
		return true
	default:
		return false
	}
}

func viewQueryUnsupported(reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "SEMANTIC_VIEW_QUERY_FRAGMENT"
	}
	return &mcp.ToolError{Code: apierr.CodeViewQueryUnsupported,
		Message: "查询超出 TaskGate Phase B 语义 View 受限片段",
		Details: map[string]any{"reason": reason, "retryable_after_rewrite": true}}
}

func viewCompositionReason(err error) string {
	var rejected *viewcompiler.Error
	if errors.As(err, &rejected) && rejected.Code != "" {
		return string(rejected.Code)
	}
	return "SEMANTIC_VIEW_QUERY_FRAGMENT"
}

func (s *Service) currentTaskViewBinding(ctx context.Context, task control.Task, grant control.TaskGrant) (*resolvedViewBinding, error) {
	if grant.ViewBindingDigest == "" {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "语义 View 任务缺少签名 binding"}
	}
	persisted, err := decodePersistedPending(task)
	if err != nil || persisted.ViewBinding == nil ||
		!sameSnapshotSHA256(persisted.ViewBinding.Digest, grant.ViewBindingDigest) {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务的语义 View binding 证据不一致"}
	}
	status, err := s.store.GetTaskViewBindingStatus(ctx, task.ID)
	if err != nil || status.Status != control.TaskViewBindingActive ||
		!sameSnapshotSHA256(status.BoundDigest, grant.ViewBindingDigest) {
		return nil, semanticViewChangedToolError()
	}
	products, err := boundViewProducts(persisted.ViewBinding)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务的语义 View binding 无法解码"}
	}
	current, resolveErr := s.resolveViewBinding(ctx, products)
	if resolveErr != nil && !dataconnector.IsCode(resolveErr, dataconnector.CodeViewSemanticChanged) {
		return nil, resolveErr
	}
	if resolveErr == nil && viewBindingMatchesCurrent(persisted.ViewBinding, current) &&
		sameSnapshotSHA256(grant.ViewBindingDigest, current.Digest) {
		return current, nil
	}
	observed := viewSemanticObservationDigest(grant.ViewBindingDigest, resolveErr)
	if current != nil && current.Digest != "" {
		observed = current.Digest
	}
	_, _ = s.store.MarkTaskViewSemanticChanged(ctx, task.ID, observed)
	return nil, semanticViewChangedToolError()
}

func semanticViewChangedToolError() error {
	return &mcp.ToolError{Code: string(dataconnector.CodeViewSemanticChanged),
		Message: "语义 View 已变化；历史结果仍可重放，但新查询必须创建新任务并重新审批"}
}

type semanticViewGovernance struct {
	products          map[string]catalog.Product
	rootScopeByColumn map[string]map[string]string
}

// semanticViewGovernanceFor proves that every terminal source remains inside
// the root's source, sensitivity, stable-role, and dynamic-scope envelope.
// Scope propagation is explicit: a public root scope output must resolve to
// one concrete terminal FieldID, and every terminal mandatory scope must be
// covered. Join equalities and constants are never treated as scope proofs.
func (s *Service) semanticViewGovernanceFor(root catalog.Product, artifact viewcompiler.Artifact) (semanticViewGovernance, error) {
	result := semanticViewGovernance{
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
		terminal, present := s.catalog.LookupProduct(scan.Product)
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
		if !contains(terminal.Scopes, terminalColumn) || !s.equivalentScopePolicies(rootScope, terminalColumn) {
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

func (s *Service) equivalentScopePolicies(rootName, terminalName string) bool {
	root, rootOK := catalogScopeByName(s.catalog.Scopes, rootName)
	terminal, terminalOK := catalogScopeByName(s.catalog.Scopes, terminalName)
	if !rootOK || !terminalOK || root.Type != terminal.Type || root.Min != terminal.Min || root.Max != terminal.Max {
		return false
	}
	return sameStringSet(root.AllowedValues, terminal.AllowedValues)
}

func catalogScopeByName(scopes []catalog.Scope, name string) (catalog.Scope, bool) {
	for _, scope := range scopes {
		if scope.Name == name {
			return scope, true
		}
	}
	return catalog.Scope{}, false
}

func (s *Service) prepareSemanticViewPlan(grant control.TaskGrant, root catalog.Product, artifact viewcompiler.Artifact,
	composition viewcompiler.Composition, binding *resolvedViewBinding) (preparedQueryPlan, error) {
	governance, err := s.semanticViewGovernanceFor(root, artifact)
	if err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "语义 View 的终端治理闭包无效"}
	}
	if len(composition.VisibleFields) == 0 ||
		len(composition.VisibleFields) != len(composition.Plan.Columns)+len(composition.Plan.Aggregates) {
		return preparedQueryPlan{}, viewQueryUnsupported("SEMANTIC_VIEW_OUTPUT_MAPPING")
	}

	var signedScope map[string]any
	if err := json.Unmarshal(grant.MandatoryScope, &signedScope); err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务的强制范围证据无效"}
	}
	terminalScopes := make(map[string]map[string]any, len(governance.products))
	for productName, bindings := range governance.rootScopeByColumn {
		terminalScopes[productName] = make(map[string]any, len(bindings))
		for terminalColumn, rootScope := range bindings {
			value, present := signedScope[rootScope]
			if !present {
				return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "任务授权缺少语义 View 终端要求的强制范围"}
			}
			terminalScopes[productName][terminalColumn] = value
		}
	}

	requiredColumns, err := semanticPlanRequiredColumns(composition.Plan, governance.products)
	if err != nil {
		return preparedQueryPlan{}, viewQueryUnsupported("SEMANTIC_VIEW_COLUMN_CLOSURE")
	}
	queryProducts := make(map[string]queryplan.Product, len(governance.products))
	for name, product := range governance.products {
		columns := requiredColumns[name]
		queryProduct := relationalQueryProduct(product, columns)
		if grant.Exposure.ProfileVersion == exposure.ProfileV4 {
			queryProduct, err = s.ordinalQueryProduct(product, columns)
			if err != nil {
				return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "语义 View 的终端 Product 未绑定可信 V4 快照发布物"}
			}
		}
		queryProducts[name] = queryProduct
	}
	compilation, err := queryplan.CompileRelational(composition.Plan, queryProducts)
	if err != nil {
		return preparedQueryPlan{}, viewQueryUnsupported("SEMANTIC_VIEW_COMPOSED_PLAN_INVALID")
	}

	// CompileRelational computes the exact positive-output evidence closure.
	// Only that closure is allowed into the private terminal grant; allFields
	// above was a validator snapshot for the trusted artifact, not authority.
	internalApproved := make(map[string][]string, len(governance.products))
	for _, source := range compilation.Sources {
		set := stringSetFromSlice(internalApproved[source.Product])
		for _, field := range source.EvidenceFields {
			set[field] = struct{}{}
		}
		internalApproved[source.Product] = sortedStringSet(set)
	}
	context, err := buildRelationalExposureContext(composition.Plan, compilation, governance.products, internalApproved)
	if err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "语义 View 缺少完整的正输出依赖证据"}
	}
	context.visibleFields = append([]string(nil), composition.VisibleFields...)
	context.factFields = append([]string(nil), composition.VisibleFields...)
	context.internalPolicyProducts = make(map[string]sqlpolicy.ProductGrant, len(governance.products))
	for name, product := range governance.products {
		policyProduct, policyErr := catalogPolicyProductGrant(product, context.meteringByProduct[name], terminalScopes[name])
		if policyErr != nil {
			return preparedQueryPlan{}, policyErr
		}
		context.internalPolicyProducts[name] = policyProduct
	}
	context.viewBindingDigest = binding.Digest
	context.viewRegistryRevision = binding.Expectation.ExpectedRevisionDigest

	prepared := preparedQueryPlan{SQL: context.mainSQL, Exposure: context}
	if grant.Exposure.ProfileVersion == exposure.ProfileV4 {
		bound, bindErr := s.bindOrdinalSidecars(compilation.ProvenanceSQL, compilation.ProvenanceFields, compilation.OrdinalProgram)
		if bindErr != nil {
			return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "语义 View 的 V4 快照索引或 sidecar 与 Catalog 不一致"}
		}
		context.provenanceSQL = bound.ProvenanceSQL
		context.provenanceFields = append([]string(nil), bound.ProvenanceFields...)
		context.ordinal = &bound
	}
	return prepared, nil
}

// semanticPlanRequiredColumns computes the exact terminal column closure
// before CompileRelational. Besides minimizing private authority, this keeps a
// leaf-filter subquery from projecting every Catalog field merely because the
// trusted validator snapshot knew that full schema.
func semanticPlanRequiredColumns(plan queryplan.QueryPlan, products map[string]catalog.Product) (map[string]map[string]struct{}, error) {
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
