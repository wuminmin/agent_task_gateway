package queryplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	// OrdinalProgramVersion identifies the deterministic dependency program
	// consumed by the snapshot-indexed V4 execution path.
	OrdinalProgramVersion = "taskgate-ordinal-program-v1"
	ordinalProgramDomain  = "TASKGATE-ORDINAL-PROGRAM-V1\x00"
)

// OrdinalCompilation is the single-product equivalent of
// RelationalCompilation. VisibleSQL may contain hidden evidence columns; the
// caller releases only VisibleFields after exact settlement. ProvenanceSQL
// emits one positive base row at a time in the order described by the program.
type OrdinalCompilation struct {
	VisibleSQL       string
	ProvenanceSQL    string
	VisibleFields    []string
	InternalFields   []string
	ProvenanceFields []string
	OrdinalProgram   OrdinalProgram
}

// OrdinalProgram contains no maps: its canonical JSON and digest are stable
// across Go processes and map insertion orders. Field bindings are sufficient
// to turn a provenance row into source entity/cell lookups, while witness
// rules describe the exact additive or alternative-proof multiplicity used by
// the V2 semantics without rebuilding annotated relations.
type OrdinalProgram struct {
	Version              string                   `json:"version"`
	Kind                 string                   `json:"kind"`
	Sources              []OrdinalSource          `json:"sources"`
	Visible              []OrdinalVisibleSpec     `json:"visible"`
	Groups               []OrdinalGroupSpec       `json:"groups,omitempty"`
	Aggregates           []OrdinalAggregateSpec   `json:"aggregates,omitempty"`
	OuterPredicates      []OrdinalPredicateSpec   `json:"outer_predicates,omitempty"`
	Joins                []OrdinalJoinSpec        `json:"joins,omitempty"`
	WitnessRules         []OrdinalWitnessRule     `json:"witness_rules"`
	ProvenanceOrder      []OrdinalOrderSpec       `json:"provenance_order,omitempty"`
	CanonicalExpressions []string                 `json:"canonical_expressions"`
	SnapshotBundle       []OrdinalSnapshotBinding `json:"snapshot_bundle"`
}

// OrdinalSource binds one physical SQL alias to one immutable semantic source.
// Branch is -1 outside UNION DISTINCT and otherwise matches the tg_branch
// provenance column.
type OrdinalSource struct {
	Product              string                 `json:"product"`
	SourceAlias          string                 `json:"source_alias"`
	SourceNamespace      string                 `json:"source_namespace"`
	Snapshot             string                 `json:"snapshot"`
	Role                 string                 `json:"role"`
	LineageDigest        string                 `json:"lineage_digest,omitempty"`
	HandleAlias          string                 `json:"handle_alias"`
	HandleRequired       bool                   `json:"handle_required"`
	SidecarBinding       OrdinalSidecarBinding  `json:"sidecar_binding"`
	Branch               int                    `json:"branch"`
	EntityKey            []OrdinalFieldBinding  `json:"entity_key"`
	EvidenceFields       []OrdinalFieldBinding  `json:"evidence_fields"`
	LeafPredicates       []OrdinalPredicateSpec `json:"leaf_predicates,omitempty"`
	OuterPredicateFields []OrdinalFieldUse      `json:"outer_predicate_fields,omitempty"`
	JoinKeyFields        []OrdinalFieldUse      `json:"join_key_fields,omitempty"`
	UnionKeyFields       []OrdinalFieldUse      `json:"union_key_fields,omitempty"`
	GroupKeyFields       []OrdinalFieldUse      `json:"group_key_fields,omitempty"`
	AggregateFields      []OrdinalFieldUse      `json:"aggregate_fields,omitempty"`
}

// OrdinalFieldBinding is the deterministic mapping from one provenance column
// to a canonical base-cell identity. EntityKeyPosition is meaningful only when
// IsEntityKey is true and preserves the Catalog tuple order.
type OrdinalFieldBinding struct {
	FieldID             string `json:"field_id"`
	Column              string `json:"column"`
	ProvenanceAlias     string `json:"provenance_alias"`
	SQLType             string `json:"sql_type"`
	Collation           string `json:"collation,omitempty"`
	CollationVersion    string `json:"collation_version,omitempty"`
	CanonicalExpression string `json:"canonical_expression"`
	IsEntityKey         bool   `json:"is_entity_key"`
	EntityKeyPosition   int    `json:"entity_key_position"`
}

// OrdinalFieldUse records how often one field fact enters a witness for one
// positive provenance row at a named operator stage.
type OrdinalFieldUse struct {
	FieldID             string `json:"field_id"`
	ProvenanceAlias     string `json:"provenance_alias"`
	CanonicalExpression string `json:"canonical_expression"`
	Multiplicity        uint64 `json:"multiplicity"`
}

type OrdinalPredicateSpec struct {
	Scope               string          `json:"scope"`
	Field               OrdinalFieldUse `json:"field"`
	SQLType             string          `json:"sql_type"`
	Operator            string          `json:"operator"`
	Value               json.RawMessage `json:"value"`
	CanonicalExpression string          `json:"canonical_expression"`
}

type OrdinalJoinSpec struct {
	Left                OrdinalFieldUse `json:"left"`
	Right               OrdinalFieldUse `json:"right"`
	CanonicalExpression string          `json:"canonical_expression"`
}

type OrdinalVisibleSpec struct {
	Kind                string `json:"kind"`
	OutputID            string `json:"output_id"`
	ResultAlias         string `json:"result_alias"`
	FieldID             string `json:"field_id,omitempty"`
	SQLType             string `json:"sql_type"`
	CanonicalExpression string `json:"canonical_expression"`
}

type OrdinalGroupSpec struct {
	Field               OrdinalFieldUse `json:"field"`
	ResultAlias         string          `json:"result_alias"`
	CanonicalExpression string          `json:"canonical_expression"`
	WitnessMultiplicity uint64          `json:"witness_multiplicity"`
}

type OrdinalAggregateSpec struct {
	Function            string          `json:"function"`
	InputKind           string          `json:"input_kind"`
	Input               OrdinalFieldUse `json:"input"`
	OutputID            string          `json:"output_id"`
	ResultAlias         string          `json:"result_alias"`
	SQLType             string          `json:"sql_type"`
	CanonicalExpression string          `json:"canonical_expression"`
	WitnessMultiplicity uint64          `json:"witness_multiplicity"`
}

// OrdinalWitnessRule is interpreted once per positive provenance member.
// InputKind is base_row, row, or field. Merge is add for same-proof
// composition and max for UNION DISTINCT alternative proofs.
type OrdinalWitnessRule struct {
	StageOrder       int    `json:"stage_order"`
	Stage            string `json:"stage"`
	TargetID         string `json:"target_id"`
	TargetExpression string `json:"target_expression"`
	SourceAlias      string `json:"source_alias,omitempty"`
	InputKind        string `json:"input_kind"`
	InputExpression  string `json:"input_expression"`
	ProvenanceAlias  string `json:"provenance_alias,omitempty"`
	Multiplicity     uint64 `json:"multiplicity"`
	Merge            string `json:"merge"`
}

type OrdinalOrderSpec struct {
	Kind            string `json:"kind"`
	FieldID         string `json:"field_id,omitempty"`
	SourceAlias     string `json:"source_alias,omitempty"`
	ProvenanceAlias string `json:"provenance_alias"`
	Direction       string `json:"direction"`
}

type OrdinalSnapshotBinding struct {
	SourceNamespace       string `json:"source_namespace"`
	Snapshot              string `json:"snapshot"`
	LineageDigest         string `json:"lineage_digest,omitempty"`
	PublicationID         string `json:"publication_id,omitempty"`
	SidecarManifestDigest string `json:"sidecar_manifest_digest,omitempty"`
}

// OrdinalSidecarBinding states the Catalog evidence required before V4 may
// consume HandleAlias. The SQL compiler deliberately does not guess a
// physical sidecar relation; a later Catalog-aware binding step must project
// the alias from this pinned publication.
type OrdinalSidecarBinding struct {
	Required       bool   `json:"required"`
	PublicationID  string `json:"publication_id,omitempty"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
}

// CanonicalJSON returns the stable serialization used by Digest and semantic
// replay keys. CompileOrdinal and CompileRelational already return programs in
// this normalized order; normalizing here also makes copied/reordered values
// safe to digest.
func (program OrdinalProgram) CanonicalJSON() ([]byte, error) {
	normal, err := normalizeOrdinalProgram(program)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normal)
}

func (program OrdinalProgram) Digest() (string, error) {
	encoded, err := program.CanonicalJSON()
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(ordinalProgramDomain))
	_, _ = hash.Write(encoded)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ValidateBoundSidecars fails closed until every source is bound to an
// immutable Catalog publication. It intentionally does not accept a logical
// product or entity-key lookup as a substitute for the publication sidecar.
func (program OrdinalProgram) ValidateBoundSidecars() error {
	normal, err := normalizeOrdinalProgram(program)
	if err != nil {
		return err
	}
	for _, source := range normal.Sources {
		if !source.HandleRequired || source.HandleAlias == "" || !source.SidecarBinding.Required ||
			strings.TrimSpace(source.SidecarBinding.PublicationID) == "" || strings.TrimSpace(source.SidecarBinding.ManifestDigest) == "" {
			return fmt.Errorf("ordinal source %q has no Catalog-pinned row-handle sidecar", source.SourceAlias)
		}
	}
	return nil
}

// ValidateProvenanceFields verifies the fail-closed row contract after the
// Catalog-aware SQL binder has projected row handles. Entity-key columns may
// remain as audit evidence but never satisfy HandleAlias.
func (program OrdinalProgram) ValidateProvenanceFields(fields []string) error {
	if err := program.ValidateBoundSidecars(); err != nil {
		return err
	}
	available := valueSet(fields)
	if len(available) != len(fields) {
		return errors.New("provenance fields contain duplicate aliases")
	}
	for _, source := range program.Sources {
		if _, present := available[source.HandleAlias]; !present {
			return fmt.Errorf("provenance omits required row handle %q", source.HandleAlias)
		}
		for _, field := range source.EvidenceFields {
			if _, present := available[field.ProvenanceAlias]; !present {
				return fmt.Errorf("provenance omits evidence alias %q", field.ProvenanceAlias)
			}
		}
	}
	return nil
}

// CompileOrdinal builds the paired single-product SQL and its dependency
// program. Compile remains unchanged for callers that do not use V4.
func CompileOrdinal(plan QueryPlan, product Product) (OrdinalCompilation, error) {
	if plan.From != nil {
		return OrdinalCompilation{}, errors.New("multi-product QueryPlan requires CompileRelational")
	}
	if product.SourceNamespace == "" || product.Snapshot == "" || product.StableRole == "" || len(product.StableEntityKey) == 0 {
		return OrdinalCompilation{}, errors.New("ordinal compilation requires namespace, snapshot, stable role, and entity key")
	}
	if _, err := Compile(plan, product); err != nil {
		return OrdinalCompilation{}, err
	}
	if err := validateProductSQLProfileV2(product); err != nil {
		return OrdinalCompilation{}, err
	}
	executionProduct := ordinalExecutionProduct(product)

	grouped := len(plan.GroupBy) > 0 || len(plan.Aggregates) > 0
	evidence, err := singleProductEvidenceFields(plan, product)
	if err != nil {
		return OrdinalCompilation{}, err
	}
	visibleFields := append([]string(nil), plan.Columns...)
	for _, aggregate := range plan.Aggregates {
		visibleFields = append(visibleFields, aggregate.Alias)
	}
	mainPlan := cloneOrdinalQueryPlan(plan)
	if grouped {
		selected := valueSet(mainPlan.Columns)
		for _, group := range plan.GroupBy {
			if _, present := selected[group]; !present {
				mainPlan.Columns = append(mainPlan.Columns, group)
				selected[group] = struct{}{}
			}
		}
		if plan.Limit > 0 || plan.Offset > 0 {
			mainPlan.OrderBy = appendStableOrders(mainPlan.OrderBy, sortedUnique(plan.GroupBy))
		}
	} else {
		selected := valueSet(mainPlan.Columns)
		for _, field := range evidence {
			if _, present := selected[field]; !present {
				mainPlan.Columns = append(mainPlan.Columns, field)
				selected[field] = struct{}{}
			}
		}
		mainPlan.OrderBy = appendStableOrders(mainPlan.OrderBy, product.StableEntityKey)
	}
	visibleSQL, err := Compile(mainPlan, executionProduct)
	if err != nil {
		return OrdinalCompilation{}, err
	}

	provenanceSQL := visibleSQL
	provenanceFields := append([]string(nil), mainPlan.Columns...)
	var order []OrdinalOrderSpec
	if grouped {
		keys := append([]string(nil), sortedUnique(plan.GroupBy)...)
		keys = appendUnique(keys, product.StableEntityKey...)
		provenancePlan := QueryPlan{Product: plan.Product, Columns: append([]string(nil), evidence...), Filters: cloneFilters(plan.Filters)}
		provenancePlan.OrderBy = appendStableOrders(nil, keys)
		provenanceSQL, err = Compile(provenancePlan, executionProduct)
		if err != nil {
			return OrdinalCompilation{}, err
		}
		provenanceFields = append([]string(nil), evidence...)
		for index, key := range keys {
			kind := "entity"
			if index < len(sortedUnique(plan.GroupBy)) {
				kind = "group"
			}
			order = append(order, OrdinalOrderSpec{Kind: kind, FieldID: key, SourceAlias: product.Name, ProvenanceAlias: key, Direction: "ASC"})
		}
	} else {
		for _, item := range mainPlan.OrderBy {
			order = append(order, OrdinalOrderSpec{Kind: "visible", FieldID: item.Column, SourceAlias: product.Name,
				ProvenanceAlias: item.Column, Direction: ordinalDirection(item.Direction)})
		}
	}
	program, err := buildSingleOrdinalProgram(plan, product, evidence, order)
	if err != nil {
		return OrdinalCompilation{}, err
	}
	return OrdinalCompilation{VisibleSQL: visibleSQL, ProvenanceSQL: provenanceSQL,
		VisibleFields: visibleFields, InternalFields: append([]string(nil), mainPlan.Columns...),
		ProvenanceFields: provenanceFields, OrdinalProgram: program}, nil
}

func buildSingleOrdinalProgram(plan QueryPlan, product Product, evidence []string, order []OrdinalOrderSpec) (OrdinalProgram, error) {
	bindings := make([]OrdinalFieldBinding, 0, len(evidence))
	byColumn := make(map[string]OrdinalFieldBinding, len(evidence))
	entityPositions := make(map[string]int, len(product.StableEntityKey))
	for position, key := range product.StableEntityKey {
		entityPositions[key] = position
	}
	for _, column := range evidence {
		binding, err := makeOrdinalBinding(product, product.Name, product.StableRole, column, column, false, entityPositions)
		if err != nil {
			return OrdinalProgram{}, err
		}
		bindings = append(bindings, binding)
		byColumn[column] = binding
	}
	entity, err := ordinalEntityKey(product.StableEntityKey, byColumn)
	if err != nil {
		return OrdinalProgram{}, err
	}
	source := OrdinalSource{Product: product.Name, SourceAlias: product.Name, SourceNamespace: product.SourceNamespace,
		Snapshot: product.Snapshot, Role: product.StableRole, LineageDigest: product.LineageDigest,
		HandleAlias: ordinalHandleAlias(product.Name), HandleRequired: true,
		SidecarBinding: OrdinalSidecarBinding{Required: true, PublicationID: product.SnapshotPublication, ManifestDigest: product.SidecarManifestDigest}, Branch: -1,
		EntityKey: entity, EvidenceFields: bindings}
	outer, err := ordinalPredicates(plan.Filters, "outer", byColumn)
	if err != nil {
		return OrdinalProgram{}, err
	}
	source.OuterPredicateFields = uniquePredicateFieldUses(outer)
	source.GroupKeyFields, err = ordinalUsesForColumns(plan.GroupBy, byColumn)
	if err != nil {
		return OrdinalProgram{}, err
	}
	aggregateColumns := make([]string, 0, len(plan.Aggregates))
	for _, aggregate := range plan.Aggregates {
		if aggregate.Column != "*" {
			aggregateColumns = append(aggregateColumns, aggregate.Column)
		}
	}
	source.AggregateFields, err = ordinalUsesForColumns(aggregateColumns, byColumn)
	if err != nil {
		return OrdinalProgram{}, err
	}
	program := OrdinalProgram{Version: OrdinalProgramVersion, Kind: "scan", Sources: []OrdinalSource{source},
		OuterPredicates: outer, ProvenanceOrder: append([]OrdinalOrderSpec(nil), order...),
		SnapshotBundle: []OrdinalSnapshotBinding{{SourceNamespace: product.SourceNamespace, Snapshot: product.Snapshot,
			LineageDigest: product.LineageDigest, PublicationID: product.SnapshotPublication, SidecarManifestDigest: product.SidecarManifestDigest}}}
	program.Visible, err = singleVisibleSpecs(plan, product, byColumn)
	if err != nil {
		return OrdinalProgram{}, err
	}
	program.Groups, err = ordinalGroupSpecs(plan.GroupBy, byColumn, func(field string) string { return field })
	if err != nil {
		return OrdinalProgram{}, err
	}
	program.Aggregates, err = ordinalAggregateSpecs(plan.Aggregates, product, byColumn, func(alias string) string { return alias })
	if err != nil {
		return OrdinalProgram{}, err
	}
	program.WitnessRules = ordinalWitnessRules(program)
	program.CanonicalExpressions = ordinalCanonicalExpressions(program)
	return normalizeOrdinalProgram(program)
}

func singleVisibleSpecs(plan QueryPlan, product Product, bindings map[string]OrdinalFieldBinding) ([]OrdinalVisibleSpec, error) {
	result := make([]OrdinalVisibleSpec, 0, len(plan.Columns)+len(plan.Aggregates))
	for _, column := range plan.Columns {
		binding, present := bindings[column]
		if !present {
			return nil, fmt.Errorf("visible field %q has no provenance binding", column)
		}
		result = append(result, OrdinalVisibleSpec{Kind: "field", OutputID: column, ResultAlias: column,
			FieldID: binding.FieldID, SQLType: binding.SQLType, CanonicalExpression: binding.CanonicalExpression})
	}
	aggregates, err := ordinalAggregateSpecs(plan.Aggregates, product, bindings, func(alias string) string { return alias })
	if err != nil {
		return nil, err
	}
	for _, aggregate := range aggregates {
		result = append(result, OrdinalVisibleSpec{Kind: "aggregate", OutputID: aggregate.OutputID,
			ResultAlias: aggregate.ResultAlias, SQLType: aggregate.SQLType, CanonicalExpression: aggregate.CanonicalExpression})
	}
	return result, nil
}

func singleProductEvidenceFields(plan QueryPlan, product Product) ([]string, error) {
	set := make(map[string]struct{})
	for _, field := range append(append([]string(nil), product.StableEntityKey...), product.RequiredEvidence...) {
		if err := ordinalProductField(field, product); err != nil {
			return nil, err
		}
		set[field] = struct{}{}
	}
	for _, field := range append(append([]string(nil), plan.Columns...), plan.GroupBy...) {
		set[field] = struct{}{}
	}
	for _, filter := range plan.Filters {
		set[filter.Column] = struct{}{}
	}
	for _, aggregate := range plan.Aggregates {
		if aggregate.Column != "*" {
			set[aggregate.Column] = struct{}{}
		}
	}
	return sortedColumns(set), nil
}

func buildRelationalOrdinalProgram(plan QueryPlan, products map[string]Product, compilation RelationalCompilation, order []OrdinalOrderSpec) (OrdinalProgram, error) {
	program := OrdinalProgram{Version: OrdinalProgramVersion, Kind: compilation.Kind,
		ProvenanceOrder: append([]OrdinalOrderSpec(nil), order...)}
	bindings := make(map[string]OrdinalFieldBinding)
	for _, relationalSource := range compilation.Sources {
		product := products[relationalSource.Product]
		semanticRole := relationalSource.Role
		branch := -1
		if compilation.Kind == "union_distinct" {
			semanticRole = plan.From.UnionDistinct.Role
			branch = relationalSource.Branch
		}
		entityPositions := make(map[string]int, len(product.StableEntityKey))
		for position, key := range product.StableEntityKey {
			entityPositions[key] = position
		}
		evidence := make([]OrdinalFieldBinding, 0, len(relationalSource.EvidenceFields))
		byColumn := make(map[string]OrdinalFieldBinding, len(relationalSource.EvidenceFields))
		for _, column := range relationalSource.EvidenceFields {
			binding, err := makeOrdinalBinding(product, relationalSource.Role, semanticRole, column,
				relationalSource.EvidenceAlias[column], true, entityPositions)
			if err != nil {
				return OrdinalProgram{}, err
			}
			evidence = append(evidence, binding)
			byColumn[column] = binding
			if existing, present := bindings[binding.FieldID]; present && !sameOrdinalSemanticBinding(existing, binding) {
				return OrdinalProgram{}, fmt.Errorf("field %q has ambiguous source bindings", binding.FieldID)
			}
			bindings[binding.FieldID] = binding
		}
		entity, err := ordinalEntityKey(product.StableEntityKey, byColumn)
		if err != nil {
			return OrdinalProgram{}, err
		}
		leaf, err := ordinalPredicates(relationalSource.Filters, "leaf", byColumn)
		if err != nil {
			return OrdinalProgram{}, err
		}
		source := OrdinalSource{Product: product.Name, SourceAlias: relationalSource.Role, SourceNamespace: product.SourceNamespace,
			Snapshot: product.Snapshot, Role: semanticRole, LineageDigest: product.LineageDigest,
			HandleAlias: ordinalHandleAlias(relationalSource.Role), HandleRequired: true,
			SidecarBinding: OrdinalSidecarBinding{Required: true, PublicationID: product.SnapshotPublication, ManifestDigest: product.SidecarManifestDigest}, Branch: branch,
			EntityKey: entity, EvidenceFields: evidence, LeafPredicates: leaf}
		source.OuterPredicateFields = relationalSourceOuterUses(plan.Filters, semanticRole, byColumn)
		source.JoinKeyFields = relationalSourceJoinUses(compilation.JoinPredicates, semanticRole, byColumn)
		if compilation.Kind == "union_distinct" {
			source.UnionKeyFields, err = ordinalUsesForColumns(compilation.UnionColumns, byColumn)
			if err != nil {
				return OrdinalProgram{}, err
			}
		}
		source.GroupKeyFields = relationalSourcePlanUses(plan.GroupBy, semanticRole, byColumn)
		aggregateFields := make([]string, 0, len(plan.Aggregates))
		for _, aggregate := range plan.Aggregates {
			role, column, ok := splitFieldID(aggregate.Column)
			if aggregate.Column != "*" && ok && role == semanticRole {
				aggregateFields = append(aggregateFields, column)
			}
		}
		source.AggregateFields, err = ordinalUsesForColumns(aggregateFields, byColumn)
		if err != nil {
			return OrdinalProgram{}, err
		}
		program.Sources = append(program.Sources, source)
	}
	outer, err := relationalOrdinalPredicates(plan.Filters, "outer", bindings)
	if err != nil {
		return OrdinalProgram{}, err
	}
	program.OuterPredicates = outer
	program.Joins, err = relationalOrdinalJoins(compilation.JoinPredicates, bindings)
	if err != nil {
		return OrdinalProgram{}, err
	}
	program.Visible, err = relationalVisibleSpecs(plan, products, compilation, bindings)
	if err != nil {
		return OrdinalProgram{}, err
	}
	program.Groups, err = relationalGroupSpecs(plan.GroupBy, compilation, bindings)
	if err != nil {
		return OrdinalProgram{}, err
	}
	program.Aggregates, err = relationalAggregateSpecs(plan.Aggregates, products, compilation, bindings)
	if err != nil {
		return OrdinalProgram{}, err
	}
	program.SnapshotBundle, err = ordinalSnapshotBundle(program.Sources)
	if err != nil {
		return OrdinalProgram{}, err
	}
	program.WitnessRules = ordinalWitnessRules(program)
	program.CanonicalExpressions = ordinalCanonicalExpressions(program)
	return normalizeOrdinalProgram(program)
}

func relationalGroupedProvenanceOrder(plan QueryPlan, compilation RelationalCompilation, products map[string]Product) (string, []OrdinalOrderSpec, error) {
	parts := make([]string, 0)
	result := make([]OrdinalOrderSpec, 0)
	seen := make(map[string]struct{})
	appendOrder := func(sqlExpression string, spec OrdinalOrderSpec) {
		if _, present := seen[sqlExpression]; present {
			return
		}
		seen[sqlExpression] = struct{}{}
		parts = append(parts, sqlExpression+" ASC")
		result = append(result, spec)
	}
	for _, fieldID := range sortedUnique(plan.GroupBy) {
		role, column, ok := splitFieldID(fieldID)
		if !ok {
			return "", nil, fmt.Errorf("invalid group field %q", fieldID)
		}
		if compilation.Kind == "union_distinct" {
			if role != plan.From.UnionDistinct.Role || len(compilation.Sources) == 0 {
				return "", nil, fmt.Errorf("group field %q has no union source", fieldID)
			}
			alias := compilation.Sources[0].EvidenceAlias[column]
			if alias == "" {
				return "", nil, fmt.Errorf("group field %q has no provenance alias", fieldID)
			}
			appendOrder(quoteIdentifier(alias), OrdinalOrderSpec{Kind: "group", FieldID: fieldID,
				ProvenanceAlias: alias, Direction: "ASC"})
			continue
		}
		var found bool
		for _, source := range compilation.Sources {
			if source.Role != role {
				continue
			}
			alias := source.EvidenceAlias[column]
			if alias == "" {
				return "", nil, fmt.Errorf("group field %q has no provenance alias", fieldID)
			}
			appendOrder(qualified(source.Role, column), OrdinalOrderSpec{Kind: "group", FieldID: fieldID,
				SourceAlias: source.Role, ProvenanceAlias: alias, Direction: "ASC"})
			found = true
			break
		}
		if !found {
			return "", nil, fmt.Errorf("group field %q has no source", fieldID)
		}
	}
	if compilation.Kind == "union_distinct" {
		if len(compilation.Sources) == 0 {
			return "", nil, errors.New("UNION DISTINCT provenance has no source")
		}
		role := plan.From.UnionDistinct.Role
		for _, column := range compilation.UnionColumns {
			alias := compilation.Sources[0].EvidenceAlias[column]
			if alias == "" {
				return "", nil, fmt.Errorf("union field %q has no provenance alias", column)
			}
			appendOrder(quoteIdentifier(alias), OrdinalOrderSpec{Kind: "union", FieldID: role + "." + column,
				ProvenanceAlias: alias, Direction: "ASC"})
		}
	}

	sources := append([]RelationalSource(nil), compilation.Sources...)
	sort.Slice(sources, func(i, j int) bool {
		leftProduct, rightProduct := products[sources[i].Product], products[sources[j].Product]
		left := leftProduct.SourceNamespace + "\x00" + leftProduct.Snapshot + "\x00" + leftProduct.StableRole + "\x00" + sources[i].Role
		right := rightProduct.SourceNamespace + "\x00" + rightProduct.Snapshot + "\x00" + rightProduct.StableRole + "\x00" + sources[j].Role
		return left < right
	})
	for _, source := range sources {
		product := products[source.Product]
		semanticRole := source.Role
		if compilation.Kind == "union_distinct" {
			semanticRole = plan.From.UnionDistinct.Role
		}
		for _, key := range product.StableEntityKey {
			alias := source.EvidenceAlias[key]
			if alias == "" {
				return "", nil, fmt.Errorf("entity key %q has no provenance alias", key)
			}
			sqlExpression := qualified(source.Role, key)
			sourceAlias := source.Role
			if compilation.Kind == "union_distinct" {
				sqlExpression = quoteIdentifier(alias)
				sourceAlias = ""
			}
			appendOrder(sqlExpression, OrdinalOrderSpec{Kind: "entity", FieldID: semanticRole + "." + key,
				SourceAlias: sourceAlias, ProvenanceAlias: alias, Direction: "ASC"})
		}
	}
	if compilation.Kind == "union_distinct" {
		appendOrder(quoteIdentifier("tg_branch"), OrdinalOrderSpec{Kind: "branch", ProvenanceAlias: "tg_branch", Direction: "ASC"})
	}
	if len(parts) == 0 {
		return "", nil, errors.New("grouped provenance requires stable group or entity ordering")
	}
	return " ORDER BY " + strings.Join(parts, ", "), result, nil
}

func makeOrdinalBinding(product Product, sourceAlias, semanticRole, column, provenanceAlias string, roleQualified bool, entityPositions map[string]int) (OrdinalFieldBinding, error) {
	if err := ordinalProductField(column, product); err != nil {
		return OrdinalFieldBinding{}, err
	}
	typeName, _ := canonicalProductType(product.ColumnTypes[column])
	fieldID := column
	if roleQualified {
		fieldID = semanticRole + "." + column
	}
	position, entity := entityPositions[column]
	return OrdinalFieldBinding{FieldID: fieldID, Column: column, ProvenanceAlias: provenanceAlias,
		SQLType: typeName, Collation: product.ColumnCollations[column], CollationVersion: product.CollationVersions[column],
		CanonicalExpression: product.SourceNamespace + "." + fieldID, IsEntityKey: entity, EntityKeyPosition: position}, nil
}

func ordinalProductField(column string, product Product) error {
	if !safeIdentifier(column) || strings.TrimSpace(product.ColumnTypes[column]) == "" {
		return fmt.Errorf("ordinal field %q lacks a typed identity", column)
	}
	if _, err := canonicalProductType(product.ColumnTypes[column]); err != nil {
		return err
	}
	return nil
}

func ordinalEntityKey(columns []string, bindings map[string]OrdinalFieldBinding) ([]OrdinalFieldBinding, error) {
	result := make([]OrdinalFieldBinding, 0, len(columns))
	for position, column := range columns {
		binding, present := bindings[column]
		if !present || !binding.IsEntityKey || binding.EntityKeyPosition != position {
			return nil, fmt.Errorf("entity key %q has no ordered provenance binding", column)
		}
		result = append(result, binding)
	}
	return result, nil
}

func ordinalUsesForColumns(columns []string, bindings map[string]OrdinalFieldBinding) ([]OrdinalFieldUse, error) {
	counts := make(map[string]uint64)
	for _, column := range columns {
		if _, present := bindings[column]; !present {
			return nil, fmt.Errorf("field %q has no provenance binding", column)
		}
		counts[column]++
	}
	result := make([]OrdinalFieldUse, 0, len(counts))
	for column, multiplicity := range counts {
		binding := bindings[column]
		result = append(result, fieldUse(binding, multiplicity))
	}
	sortOrdinalFieldUses(result)
	return result, nil
}

func fieldUse(binding OrdinalFieldBinding, multiplicity uint64) OrdinalFieldUse {
	return OrdinalFieldUse{FieldID: binding.FieldID, ProvenanceAlias: binding.ProvenanceAlias,
		CanonicalExpression: binding.CanonicalExpression, Multiplicity: multiplicity}
}

func ordinalPredicates(filters []Filter, scope string, bindings map[string]OrdinalFieldBinding) ([]OrdinalPredicateSpec, error) {
	result := make([]OrdinalPredicateSpec, 0, len(filters))
	for _, filter := range filters {
		binding, present := bindings[filter.Column]
		if !present {
			return nil, fmt.Errorf("predicate field %q has no provenance binding", filter.Column)
		}
		predicate, err := ordinalPredicate(scope, binding, filter)
		if err != nil {
			return nil, err
		}
		result = append(result, predicate)
	}
	sort.Slice(result, func(i, j int) bool { return ordinalPredicateKey(result[i]) < ordinalPredicateKey(result[j]) })
	return result, nil
}

func ordinalPredicate(scope string, binding OrdinalFieldBinding, filter Filter) (OrdinalPredicateSpec, error) {
	value, err := canonicalJSON(filter.Value)
	if err != nil {
		return OrdinalPredicateSpec{}, err
	}
	op := strings.ToUpper(strings.TrimSpace(filter.Op))
	if op == "!=" {
		op = "<>"
	}
	expressionBytes, _ := json.Marshal(struct {
		Field string          `json:"field"`
		Type  string          `json:"type"`
		Op    string          `json:"op"`
		Value json.RawMessage `json:"value"`
	}{binding.CanonicalExpression, binding.SQLType, op, value})
	return OrdinalPredicateSpec{Scope: scope, Field: fieldUse(binding, 1), SQLType: binding.SQLType,
		Operator: op, Value: value, CanonicalExpression: "predicate:" + string(expressionBytes)}, nil
}

func uniquePredicateFieldUses(predicates []OrdinalPredicateSpec) []OrdinalFieldUse {
	byField := make(map[string]OrdinalFieldUse)
	for _, predicate := range predicates {
		// SelectV2 admits every referenced field once even when two conjuncts
		// compare the same field.
		byField[predicate.Field.FieldID] = predicate.Field
	}
	result := make([]OrdinalFieldUse, 0, len(byField))
	for _, use := range byField {
		use.Multiplicity = 1
		result = append(result, use)
	}
	sortOrdinalFieldUses(result)
	return result
}

func relationalOrdinalPredicates(filters []Filter, scope string, bindings map[string]OrdinalFieldBinding) ([]OrdinalPredicateSpec, error) {
	result := make([]OrdinalPredicateSpec, 0, len(filters))
	for _, filter := range filters {
		binding, present := bindings[filter.Column]
		if !present {
			return nil, fmt.Errorf("predicate field %q has no semantic binding", filter.Column)
		}
		predicate, err := ordinalPredicate(scope, binding, filter)
		if err != nil {
			return nil, err
		}
		result = append(result, predicate)
	}
	sort.Slice(result, func(i, j int) bool { return ordinalPredicateKey(result[i]) < ordinalPredicateKey(result[j]) })
	return result, nil
}

func relationalSourceOuterUses(filters []Filter, semanticRole string, bindings map[string]OrdinalFieldBinding) []OrdinalFieldUse {
	columns := make([]string, 0, len(filters))
	for _, filter := range filters {
		role, column, ok := splitFieldID(filter.Column)
		if ok && role == semanticRole {
			columns = append(columns, column)
		}
	}
	uses, _ := ordinalUsesForColumns(uniqueOrdered(columns), bindings)
	for index := range uses {
		uses[index].Multiplicity = 1
	}
	return uses
}

func relationalSourceJoinUses(predicates []JoinPredicate, semanticRole string, bindings map[string]OrdinalFieldBinding) []OrdinalFieldUse {
	columns := make([]string, 0, len(predicates))
	for _, predicate := range predicates {
		for _, field := range []string{predicate.Left, predicate.Right} {
			role, column, ok := splitFieldID(field)
			if ok && role == semanticRole {
				columns = append(columns, column)
			}
		}
	}
	uses, _ := ordinalUsesForColumns(columns, bindings)
	return uses
}

func relationalSourcePlanUses(fields []string, semanticRole string, bindings map[string]OrdinalFieldBinding) []OrdinalFieldUse {
	columns := make([]string, 0, len(fields))
	for _, field := range fields {
		role, column, ok := splitFieldID(field)
		if ok && role == semanticRole {
			columns = append(columns, column)
		}
	}
	uses, _ := ordinalUsesForColumns(columns, bindings)
	return uses
}

func relationalOrdinalJoins(predicates []JoinPredicate, bindings map[string]OrdinalFieldBinding) ([]OrdinalJoinSpec, error) {
	result := make([]OrdinalJoinSpec, 0, len(predicates))
	for _, predicate := range predicates {
		left, leftOK := bindings[predicate.Left]
		right, rightOK := bindings[predicate.Right]
		if !leftOK || !rightOK {
			return nil, errors.New("join predicate has no provenance binding")
		}
		expressions := []string{left.CanonicalExpression, right.CanonicalExpression}
		sort.Strings(expressions)
		result = append(result, OrdinalJoinSpec{Left: fieldUse(left, 1), Right: fieldUse(right, 1),
			CanonicalExpression: "eq(" + strings.Join(expressions, ",") + ")"})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CanonicalExpression < result[j].CanonicalExpression })
	return result, nil
}

func relationalVisibleSpecs(plan QueryPlan, products map[string]Product, compilation RelationalCompilation, bindings map[string]OrdinalFieldBinding) ([]OrdinalVisibleSpec, error) {
	result := make([]OrdinalVisibleSpec, 0, len(plan.Columns)+len(plan.Aggregates))
	for _, field := range plan.Columns {
		binding, present := bindings[field]
		if !present {
			return nil, fmt.Errorf("visible field %q has no provenance binding", field)
		}
		result = append(result, OrdinalVisibleSpec{Kind: "field", OutputID: field, ResultAlias: compilation.OutputAliases[field],
			FieldID: field, SQLType: binding.SQLType, CanonicalExpression: binding.CanonicalExpression})
	}
	aggregates, err := relationalAggregateSpecs(plan.Aggregates, products, compilation, bindings)
	if err != nil {
		return nil, err
	}
	for _, aggregate := range aggregates {
		result = append(result, OrdinalVisibleSpec{Kind: "aggregate", OutputID: aggregate.OutputID,
			ResultAlias: aggregate.ResultAlias, SQLType: aggregate.SQLType, CanonicalExpression: aggregate.CanonicalExpression})
	}
	return result, nil
}

func relationalGroupSpecs(fields []string, compilation RelationalCompilation, bindings map[string]OrdinalFieldBinding) ([]OrdinalGroupSpec, error) {
	result := make([]OrdinalGroupSpec, 0, len(fields))
	for _, field := range fields {
		binding, present := bindings[field]
		if !present {
			return nil, fmt.Errorf("group field %q has no provenance binding", field)
		}
		result = append(result, OrdinalGroupSpec{Field: fieldUse(binding, 1), ResultAlias: compilation.OutputAliases[field],
			CanonicalExpression: "group(" + binding.CanonicalExpression + ")", WitnessMultiplicity: 1})
	}
	return result, nil
}

func ordinalGroupSpecs(fields []string, bindings map[string]OrdinalFieldBinding, resultAlias func(string) string) ([]OrdinalGroupSpec, error) {
	result := make([]OrdinalGroupSpec, 0, len(fields))
	for _, field := range fields {
		binding, present := bindings[field]
		if !present {
			return nil, fmt.Errorf("group field %q has no provenance binding", field)
		}
		result = append(result, OrdinalGroupSpec{Field: fieldUse(binding, 1), ResultAlias: resultAlias(field),
			CanonicalExpression: "group(" + binding.CanonicalExpression + ")", WitnessMultiplicity: 1})
	}
	return result, nil
}

func ordinalAggregateSpecs(aggregates []Aggregate, product Product, bindings map[string]OrdinalFieldBinding, resultAlias func(string) string) ([]OrdinalAggregateSpec, error) {
	result := make([]OrdinalAggregateSpec, 0, len(aggregates))
	for _, aggregate := range aggregates {
		function := strings.ToLower(strings.TrimSpace(aggregate.Function))
		spec := OrdinalAggregateSpec{Function: function, InputKind: "row", OutputID: aggregate.Alias,
			ResultAlias: resultAlias(aggregate.Alias), SQLType: "bigint", CanonicalExpression: function + "(*)", WitnessMultiplicity: 1}
		if aggregate.Column != "*" {
			binding, present := bindings[aggregate.Column]
			if !present {
				return nil, fmt.Errorf("aggregate field %q has no provenance binding", aggregate.Column)
			}
			spec.InputKind = "field"
			spec.Input = fieldUse(binding, 1)
			spec.SQLType = aggregateOutputType(function, product.ColumnTypes[aggregate.Column])
			spec.CanonicalExpression = function + "(" + binding.CanonicalExpression + ")"
		}
		if spec.SQLType == "" {
			return nil, fmt.Errorf("aggregate %s has no canonical output type", spec.CanonicalExpression)
		}
		result = append(result, spec)
	}
	return result, nil
}

func relationalAggregateSpecs(aggregates []Aggregate, products map[string]Product, compilation RelationalCompilation, bindings map[string]OrdinalFieldBinding) ([]OrdinalAggregateSpec, error) {
	result := make([]OrdinalAggregateSpec, 0, len(aggregates))
	for _, aggregate := range aggregates {
		function := strings.ToLower(strings.TrimSpace(aggregate.Function))
		spec := OrdinalAggregateSpec{Function: function, InputKind: "row", OutputID: aggregate.Alias,
			ResultAlias: compilation.OutputAliases[aggregate.Alias], SQLType: "bigint", CanonicalExpression: function + "(*)", WitnessMultiplicity: 1}
		if aggregate.Column != "*" {
			binding, present := bindings[aggregate.Column]
			if !present {
				return nil, fmt.Errorf("aggregate field %q has no provenance binding", aggregate.Column)
			}
			spec.InputKind = "field"
			spec.Input = fieldUse(binding, 1)
			spec.CanonicalExpression = function + "(" + binding.CanonicalExpression + ")"
			_, column, _ := splitFieldID(aggregate.Column)
			for _, source := range compilation.Sources {
				semanticRole := source.Role
				if compilation.Kind == "union_distinct" {
					semanticRole, _, _ = splitFieldID(aggregate.Column)
				}
				role, _, _ := splitFieldID(aggregate.Column)
				if semanticRole == role {
					spec.SQLType = aggregateOutputType(function, products[source.Product].ColumnTypes[column])
					break
				}
			}
		}
		if spec.SQLType == "" {
			return nil, fmt.Errorf("aggregate %s has no canonical output type", spec.CanonicalExpression)
		}
		result = append(result, spec)
	}
	return result, nil
}

func ordinalWitnessRules(program OrdinalProgram) []OrdinalWitnessRule {
	result := make([]OrdinalWitnessRule, 0)
	for _, source := range program.Sources {
		result = append(result, OrdinalWitnessRule{StageOrder: 10, Stage: "scan", TargetID: "$row", TargetExpression: "$row", SourceAlias: source.SourceAlias,
			InputKind: "base_row", InputExpression: source.SourceNamespace + ".$row", Multiplicity: 1, Merge: "add"})
		for _, use := range uniquePredicateFieldUses(source.LeafPredicates) {
			result = append(result, witnessFieldRule(20, "leaf_filter", "$row", "$row", source.SourceAlias, use, "add"))
		}
	}
	for _, join := range program.Joins {
		result = append(result, witnessFieldRule(30, "join", "$row", "$row", sourceAliasForUse(program.Sources, join.Left), join.Left, "add"))
		result = append(result, witnessFieldRule(30, "join", "$row", "$row", sourceAliasForUse(program.Sources, join.Right), join.Right, "add"))
	}
	if program.Kind == "union_distinct" {
		for _, source := range program.Sources {
			result = append(result, OrdinalWitnessRule{StageOrder: 40, Stage: "union_row", TargetID: "$row", TargetExpression: "$row", SourceAlias: source.SourceAlias,
				InputKind: "row", InputExpression: "$row", Multiplicity: 1, Merge: "max"})
			for _, use := range source.UnionKeyFields {
				result = append(result, witnessFieldRule(40, "union_row", "$row", "$row", source.SourceAlias, use, "max"))
				result = append(result, witnessFieldRule(40, "union_cell", use.FieldID, use.CanonicalExpression, source.SourceAlias, use, "max"))
			}
		}
	}
	for _, use := range uniquePredicateFieldUses(program.OuterPredicates) {
		result = append(result, witnessFieldRule(50, "outer_filter", "$row", "$row", sourceAliasForUse(program.Sources, use), use, "add"))
	}
	if len(program.Groups) > 0 || len(program.Aggregates) > 0 {
		result = append(result, OrdinalWitnessRule{StageOrder: 60, Stage: "group", TargetID: "$group_row", TargetExpression: "$group_row", InputKind: "row",
			InputExpression: "$row", Multiplicity: 1, Merge: "add"})
		for _, group := range program.Groups {
			alias := sourceAliasForUse(program.Sources, group.Field)
			result = append(result, witnessFieldRule(60, "group_row", "$group_row", "$group_row", alias, group.Field, "add"))
			result = append(result, witnessFieldRule(60, "group_cell", group.Field.FieldID, group.CanonicalExpression, alias, group.Field, "add"))
		}
		for _, aggregate := range program.Aggregates {
			if aggregate.InputKind == "row" {
				result = append(result, OrdinalWitnessRule{StageOrder: 70, Stage: "aggregate", TargetID: aggregate.OutputID, TargetExpression: aggregate.CanonicalExpression,
					InputKind: "row", InputExpression: "$row", Multiplicity: aggregate.WitnessMultiplicity, Merge: "add"})
			} else {
				result = append(result, witnessFieldRule(70, "aggregate", aggregate.OutputID, aggregate.CanonicalExpression,
					sourceAliasForUse(program.Sources, aggregate.Input), aggregate.Input, "add"))
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return ordinalWitnessRuleKey(result[i]) < ordinalWitnessRuleKey(result[j]) })
	return result
}

func witnessFieldRule(stageOrder int, stage, targetID, targetExpression, sourceAlias string, use OrdinalFieldUse, merge string) OrdinalWitnessRule {
	return OrdinalWitnessRule{StageOrder: stageOrder, Stage: stage, TargetID: targetID, TargetExpression: targetExpression, SourceAlias: sourceAlias, InputKind: "field",
		InputExpression: use.CanonicalExpression, ProvenanceAlias: use.ProvenanceAlias, Multiplicity: use.Multiplicity, Merge: merge}
}

func sourceAliasForUse(sources []OrdinalSource, use OrdinalFieldUse) string {
	result := ""
	for _, source := range sources {
		for _, binding := range source.EvidenceFields {
			if binding.FieldID == use.FieldID && binding.ProvenanceAlias == use.ProvenanceAlias {
				if result != "" && result != source.SourceAlias {
					return "" // a post-UNION semantic field has no single source alias
				}
				result = source.SourceAlias
			}
		}
	}
	return result
}

func ordinalCanonicalExpressions(program OrdinalProgram) []string {
	set := make(map[string]struct{})
	for _, source := range program.Sources {
		for _, field := range source.EvidenceFields {
			set[field.CanonicalExpression] = struct{}{}
		}
		for _, predicate := range source.LeafPredicates {
			set[predicate.CanonicalExpression] = struct{}{}
		}
	}
	for _, visible := range program.Visible {
		set[visible.CanonicalExpression] = struct{}{}
	}
	for _, group := range program.Groups {
		set[group.CanonicalExpression] = struct{}{}
	}
	for _, aggregate := range program.Aggregates {
		set[aggregate.CanonicalExpression] = struct{}{}
	}
	for _, predicate := range program.OuterPredicates {
		set[predicate.CanonicalExpression] = struct{}{}
	}
	for _, join := range program.Joins {
		set[join.CanonicalExpression] = struct{}{}
	}
	return sortedColumns(set)
}

func ordinalSnapshotBundle(sources []OrdinalSource) ([]OrdinalSnapshotBinding, error) {
	byNamespace := make(map[string]OrdinalSnapshotBinding)
	for _, source := range sources {
		binding := OrdinalSnapshotBinding{SourceNamespace: source.SourceNamespace, Snapshot: source.Snapshot, LineageDigest: source.LineageDigest,
			PublicationID: source.SidecarBinding.PublicationID, SidecarManifestDigest: source.SidecarBinding.ManifestDigest}
		if previous, present := byNamespace[source.SourceNamespace]; present && previous != binding {
			return nil, errors.New("one namespace is bound to conflicting snapshot metadata")
		}
		byNamespace[source.SourceNamespace] = binding
	}
	result := make([]OrdinalSnapshotBinding, 0, len(byNamespace))
	for _, binding := range byNamespace {
		result = append(result, binding)
	}
	sort.Slice(result, func(i, j int) bool { return ordinalSnapshotKey(result[i]) < ordinalSnapshotKey(result[j]) })
	return result, nil
}

func normalizeOrdinalProgram(program OrdinalProgram) (OrdinalProgram, error) {
	if program.Version != OrdinalProgramVersion {
		return OrdinalProgram{}, errors.New("invalid ordinal program version")
	}
	switch program.Kind {
	case "scan", "join", "union_distinct":
	default:
		return OrdinalProgram{}, errors.New("invalid ordinal program kind")
	}
	if len(program.Sources) == 0 || len(program.Visible) == 0 || len(program.SnapshotBundle) == 0 || len(program.WitnessRules) == 0 {
		return OrdinalProgram{}, errors.New("ordinal program is incomplete")
	}
	switch program.Kind {
	case "scan":
		if len(program.Sources) != 1 {
			return OrdinalProgram{}, errors.New("ordinal program source count disagrees with its kind")
		}
	case "join":
		if len(program.Sources) < 2 || len(program.Sources) > MaxJoinSources {
			return OrdinalProgram{}, errors.New("ordinal join source count is outside the operational complexity limit")
		}
	case "union_distinct":
		if len(program.Sources) != 2 {
			return OrdinalProgram{}, errors.New("ordinal program source count disagrees with its kind")
		}
	}
	result := program
	result.Sources = append([]OrdinalSource(nil), program.Sources...)
	unionBranches := [2]bool{}
	seenSourceAliases := make(map[string]struct{}, len(result.Sources))
	seenProvenanceAliases := make(map[string]struct{})
	sharedUnionEvidence := make(map[string]OrdinalFieldBinding)
	for index := range result.Sources {
		source := &result.Sources[index]
		if source.Product == "" || source.SourceAlias == "" || source.SourceNamespace == "" || source.Snapshot == "" || source.Role == "" ||
			source.HandleAlias == "" || !source.HandleRequired || !source.SidecarBinding.Required || len(source.EntityKey) == 0 || len(source.EvidenceFields) == 0 {
			return OrdinalProgram{}, errors.New("ordinal source is incomplete")
		}
		if _, duplicate := seenSourceAliases[source.SourceAlias]; duplicate {
			return OrdinalProgram{}, errors.New("ordinal source aliases must be globally unique")
		}
		seenSourceAliases[source.SourceAlias] = struct{}{}
		if _, duplicate := seenProvenanceAliases[source.HandleAlias]; duplicate {
			return OrdinalProgram{}, errors.New("ordinal provenance aliases must be globally unique")
		}
		seenProvenanceAliases[source.HandleAlias] = struct{}{}
		if program.Kind == "union_distinct" {
			if source.Branch < 0 || source.Branch >= len(unionBranches) || unionBranches[source.Branch] {
				return OrdinalProgram{}, errors.New("UNION DISTINCT source branches must be the exact disjoint pair 0/1")
			}
			unionBranches[source.Branch] = true
		} else if source.Branch != -1 {
			return OrdinalProgram{}, errors.New("non-UNION ordinal source has a branch identity")
		}
		source.EntityKey = append([]OrdinalFieldBinding(nil), source.EntityKey...)
		source.EvidenceFields = append([]OrdinalFieldBinding(nil), source.EvidenceFields...)
		seenAliases := make(map[string]struct{}, len(source.EvidenceFields)+1)
		evidenceByAlias := make(map[string]OrdinalFieldBinding, len(source.EvidenceFields))
		seenAliases[source.HandleAlias] = struct{}{}
		for _, field := range source.EvidenceFields {
			if field.FieldID == "" || field.Column == "" || field.ProvenanceAlias == "" || field.SQLType == "" || field.CanonicalExpression == "" {
				return OrdinalProgram{}, errors.New("ordinal evidence field is incomplete")
			}
			if _, duplicate := seenAliases[field.ProvenanceAlias]; duplicate {
				return OrdinalProgram{}, errors.New("ordinal source aliases collide")
			}
			if _, duplicate := seenProvenanceAliases[field.ProvenanceAlias]; duplicate {
				previous, sharedEvidence := sharedUnionEvidence[field.ProvenanceAlias]
				if program.Kind != "union_distinct" || !sharedEvidence || !sameOrdinalUnionProjection(previous, field) {
					return OrdinalProgram{}, errors.New("ordinal provenance aliases must be globally unique outside an equivalent UNION projection")
				}
			} else {
				seenProvenanceAliases[field.ProvenanceAlias] = struct{}{}
				sharedUnionEvidence[field.ProvenanceAlias] = field
			}
			seenAliases[field.ProvenanceAlias] = struct{}{}
			evidenceByAlias[field.ProvenanceAlias] = field
		}
		for position, field := range source.EntityKey {
			evidence, present := evidenceByAlias[field.ProvenanceAlias]
			if !present || !field.IsEntityKey || field.EntityKeyPosition != position || !sameOrdinalSemanticBinding(field, evidence) {
				return OrdinalProgram{}, errors.New("ordinal entity key does not match its evidence binding")
			}
		}
		for _, uses := range [][]OrdinalFieldUse{source.OuterPredicateFields, source.JoinKeyFields,
			source.UnionKeyFields, source.GroupKeyFields, source.AggregateFields} {
			for _, use := range uses {
				evidence, present := evidenceByAlias[use.ProvenanceAlias]
				if !present || use.Multiplicity == 0 || use.FieldID != evidence.FieldID ||
					use.CanonicalExpression != evidence.CanonicalExpression {
					return OrdinalProgram{}, errors.New("ordinal source field use escapes its evidence binding")
				}
			}
		}
		for _, predicate := range source.LeafPredicates {
			evidence, present := evidenceByAlias[predicate.Field.ProvenanceAlias]
			if !present || predicate.Field.Multiplicity == 0 || predicate.Field.FieldID != evidence.FieldID ||
				predicate.Field.CanonicalExpression != evidence.CanonicalExpression {
				return OrdinalProgram{}, errors.New("ordinal source predicate escapes its evidence binding")
			}
		}
		sort.Slice(source.EvidenceFields, func(i, j int) bool {
			return ordinalBindingKey(source.EvidenceFields[i]) < ordinalBindingKey(source.EvidenceFields[j])
		})
		source.LeafPredicates = append([]OrdinalPredicateSpec(nil), source.LeafPredicates...)
		sort.Slice(source.LeafPredicates, func(i, j int) bool {
			return ordinalPredicateKey(source.LeafPredicates[i]) < ordinalPredicateKey(source.LeafPredicates[j])
		})
		for _, uses := range []*[]OrdinalFieldUse{&source.OuterPredicateFields, &source.JoinKeyFields, &source.UnionKeyFields, &source.GroupKeyFields, &source.AggregateFields} {
			*uses = append([]OrdinalFieldUse(nil), (*uses)...)
			sortOrdinalFieldUses(*uses)
		}
	}
	if program.Kind == "union_distinct" && (!unionBranches[0] || !unionBranches[1]) {
		return OrdinalProgram{}, errors.New("UNION DISTINCT source branches must be the exact disjoint pair 0/1")
	}
	sort.Slice(result.Sources, func(i, j int) bool { return ordinalSourceKey(result.Sources[i]) < ordinalSourceKey(result.Sources[j]) })
	result.Visible = append([]OrdinalVisibleSpec(nil), program.Visible...)
	result.Groups = append([]OrdinalGroupSpec(nil), program.Groups...)
	sort.Slice(result.Groups, func(i, j int) bool {
		return result.Groups[i].CanonicalExpression < result.Groups[j].CanonicalExpression
	})
	result.Aggregates = append([]OrdinalAggregateSpec(nil), program.Aggregates...)
	sort.Slice(result.Aggregates, func(i, j int) bool {
		return ordinalAggregateKey(result.Aggregates[i]) < ordinalAggregateKey(result.Aggregates[j])
	})
	result.OuterPredicates = append([]OrdinalPredicateSpec(nil), program.OuterPredicates...)
	sort.Slice(result.OuterPredicates, func(i, j int) bool {
		return ordinalPredicateKey(result.OuterPredicates[i]) < ordinalPredicateKey(result.OuterPredicates[j])
	})
	result.Joins = append([]OrdinalJoinSpec(nil), program.Joins...)
	sort.Slice(result.Joins, func(i, j int) bool { return result.Joins[i].CanonicalExpression < result.Joins[j].CanonicalExpression })
	result.WitnessRules = append([]OrdinalWitnessRule(nil), program.WitnessRules...)
	for _, rule := range result.WitnessRules {
		if rule.StageOrder <= 0 || rule.Stage == "" || rule.TargetID == "" || rule.TargetExpression == "" || rule.InputKind == "" ||
			rule.InputExpression == "" || rule.Multiplicity == 0 || (rule.Merge != "add" && rule.Merge != "max") {
			return OrdinalProgram{}, errors.New("ordinal witness rule is incomplete")
		}
	}
	sort.Slice(result.WitnessRules, func(i, j int) bool {
		return ordinalWitnessRuleKey(result.WitnessRules[i]) < ordinalWitnessRuleKey(result.WitnessRules[j])
	})
	result.ProvenanceOrder = append([]OrdinalOrderSpec(nil), program.ProvenanceOrder...)
	result.CanonicalExpressions = sortedUniqueExact(program.CanonicalExpressions)
	result.SnapshotBundle = append([]OrdinalSnapshotBinding(nil), program.SnapshotBundle...)
	sort.Slice(result.SnapshotBundle, func(i, j int) bool {
		return ordinalSnapshotKey(result.SnapshotBundle[i]) < ordinalSnapshotKey(result.SnapshotBundle[j])
	})
	expectedBundle, err := ordinalSnapshotBundle(result.Sources)
	if err != nil || len(expectedBundle) != len(result.SnapshotBundle) {
		return OrdinalProgram{}, errors.New("ordinal snapshot bundle disagrees with its sources")
	}
	for index := range expectedBundle {
		if expectedBundle[index] != result.SnapshotBundle[index] {
			return OrdinalProgram{}, errors.New("ordinal snapshot bundle disagrees with its sources")
		}
	}
	return result, nil
}

func sameOrdinalSemanticBinding(left, right OrdinalFieldBinding) bool {
	return left.FieldID == right.FieldID && left.SQLType == right.SQLType && left.CanonicalExpression == right.CanonicalExpression
}

func sameOrdinalUnionProjection(left, right OrdinalFieldBinding) bool {
	return sameOrdinalSemanticBinding(left, right) && left.Column == right.Column && left.Collation == right.Collation &&
		left.CollationVersion == right.CollationVersion && left.IsEntityKey == right.IsEntityKey &&
		left.EntityKeyPosition == right.EntityKeyPosition
}

func cloneOrdinalQueryPlan(plan QueryPlan) QueryPlan {
	result := plan
	result.Columns = append([]string(nil), plan.Columns...)
	result.Aggregates = append([]Aggregate(nil), plan.Aggregates...)
	result.Filters = cloneFilters(plan.Filters)
	result.GroupBy = append([]string(nil), plan.GroupBy...)
	result.OrderBy = append([]Order(nil), plan.OrderBy...)
	return result
}

func appendStableOrders(orders []Order, fields []string) []Order {
	result := append([]Order(nil), orders...)
	seen := make(map[string]struct{}, len(result)+len(fields))
	for _, order := range result {
		seen[order.Column] = struct{}{}
	}
	for _, field := range fields {
		if _, present := seen[field]; present {
			continue
		}
		result = append(result, Order{Column: field, Direction: "ASC"})
		seen[field] = struct{}{}
	}
	return result
}

func appendUnique(values []string, additions ...string) []string {
	seen := valueSet(values)
	for _, value := range additions {
		if _, present := seen[value]; present {
			continue
		}
		values = append(values, value)
		seen[value] = struct{}{}
	}
	return values
}

func valueSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func sortedUniqueExact(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, present := seen[value]; present {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func ordinalExecutionProduct(product Product) Product {
	result := product
	result.Columns = make(map[string]struct{}, len(product.Columns)+len(product.StableEntityKey)+len(product.RequiredEvidence))
	for field := range product.Columns {
		result.Columns[field] = struct{}{}
	}
	for _, field := range append(append([]string(nil), product.StableEntityKey...), product.RequiredEvidence...) {
		result.Columns[field] = struct{}{}
	}
	return result
}

func ordinalDirection(direction string) string {
	direction = strings.ToUpper(strings.TrimSpace(direction))
	if direction == "" {
		return "ASC"
	}
	return direction
}

func ordinalHandleAlias(sourceAlias string) string {
	digest := sha256.Sum256([]byte("taskgate-ordinal-handle\x00" + sourceAlias))
	return "tg_h_" + hex.EncodeToString(digest[:8])
}

func sortOrdinalFieldUses(values []OrdinalFieldUse) {
	sort.Slice(values, func(i, j int) bool { return ordinalFieldUseKey(values[i]) < ordinalFieldUseKey(values[j]) })
}

func ordinalSourceKey(value OrdinalSource) string {
	return value.SourceNamespace + "\x00" + value.Snapshot + "\x00" + value.Role + "\x00" + value.SourceAlias + fmt.Sprintf("\x00%010d", value.Branch)
}
func ordinalBindingKey(value OrdinalFieldBinding) string {
	return value.FieldID + "\x00" + value.ProvenanceAlias
}
func ordinalFieldUseKey(value OrdinalFieldUse) string {
	return value.FieldID + "\x00" + value.ProvenanceAlias + fmt.Sprintf("\x00%020d", value.Multiplicity)
}
func ordinalPredicateKey(value OrdinalPredicateSpec) string {
	return value.Scope + "\x00" + value.CanonicalExpression
}
func ordinalAggregateKey(value OrdinalAggregateSpec) string {
	return value.CanonicalExpression + "\x00" + value.OutputID + "\x00" + value.ResultAlias
}
func ordinalWitnessRuleKey(value OrdinalWitnessRule) string {
	return fmt.Sprintf("%010d", value.StageOrder) + "\x00" + value.Stage + "\x00" + value.TargetID + "\x00" + value.TargetExpression + "\x00" + value.SourceAlias + "\x00" + value.InputKind + "\x00" +
		value.InputExpression + "\x00" + value.ProvenanceAlias + fmt.Sprintf("\x00%020d\x00%s", value.Multiplicity, value.Merge)
}
func ordinalSnapshotKey(value OrdinalSnapshotBinding) string {
	return value.SourceNamespace + "\x00" + value.Snapshot + "\x00" + value.LineageDigest + "\x00" +
		value.PublicationID + "\x00" + value.SidecarManifestDigest
}
