package queryplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

const (
	PredicateFootprintVersion = exposure.PredicateFootprintVersion
	predicateContextDomain    = "TASKGATE-PREDICATE-CONTEXT-V1\x00"
	implementationAtomLimit   = 65536
)

// PredicateLimits bounds work before a business query is reserved or run.
// Zero fields select the conservative defaults from the V5 profile.
type PredicateLimits struct {
	MaxRawLiteralsPerQuery   int `json:"max_raw_literals_per_query" yaml:"max_raw_literals_per_query"`
	MaxUniqueAtomsPerQuery   int `json:"max_unique_atoms_per_query" yaml:"max_unique_atoms_per_query"`
	MaxAtomPayloadBytes      int `json:"max_atom_payload_bytes" yaml:"max_atom_payload_bytes"`
	MaxTotalAtomPayloadBytes int `json:"max_total_atom_payload_bytes" yaml:"max_total_atom_payload_bytes"`
}

func DefaultPredicateLimits() PredicateLimits {
	return PredicateLimits{MaxRawLiteralsPerQuery: 20000, MaxUniqueAtomsPerQuery: 10000,
		MaxAtomPayloadBytes: 4096, MaxTotalAtomPayloadBytes: 8 << 20}
}

func (limits PredicateLimits) normalized() (PredicateLimits, error) {
	defaults := DefaultPredicateLimits()
	if limits.MaxRawLiteralsPerQuery == 0 {
		limits.MaxRawLiteralsPerQuery = defaults.MaxRawLiteralsPerQuery
	}
	if limits.MaxUniqueAtomsPerQuery == 0 {
		limits.MaxUniqueAtomsPerQuery = defaults.MaxUniqueAtomsPerQuery
	}
	if limits.MaxAtomPayloadBytes == 0 {
		limits.MaxAtomPayloadBytes = defaults.MaxAtomPayloadBytes
	}
	if limits.MaxTotalAtomPayloadBytes == 0 {
		limits.MaxTotalAtomPayloadBytes = defaults.MaxTotalAtomPayloadBytes
	}
	if limits.MaxRawLiteralsPerQuery < 1 || limits.MaxUniqueAtomsPerQuery < 1 ||
		limits.MaxUniqueAtomsPerQuery > implementationAtomLimit || limits.MaxAtomPayloadBytes < 1 ||
		limits.MaxTotalAtomPayloadBytes < 1 {
		return PredicateLimits{}, errors.New("invalid predicate footprint limits")
	}
	return limits, nil
}

// PredicatePublicationBinding identifies one immutable data publication in a
// predicate context. Display aliases are intentionally absent.
type PredicatePublicationBinding struct {
	SemanticProductID string `json:"semantic_product_id"`
	StableRole        string `json:"stable_role"`
	SourceNamespace   string `json:"source_namespace"`
	Snapshot          string `json:"snapshot"`
	Publication       string `json:"publication,omitempty"`
	PublicationSHA256 string `json:"publication_sha256,omitempty"`
	LineageSHA256     string `json:"lineage_sha256,omitempty"`
}

// PredicateFieldBinding supplies the stable public identity and resolved View
// expression for a caller-addressable field. Fields is keyed by the canonical
// plan spelling (for example "orders.amount").
type PredicateFieldBinding struct {
	SemanticProductID        string
	StableRole               string
	PublicFieldID            string
	ResolvedExpressionSHA256 string
	SQLType                  string
	CollationName            string
	CollationVersion         string
}

// PredicateFilterBinding allows a View compiler to pass only the filters that
// came from the caller. When CallerFilters is nil, filters are derived from the
// public QueryPlan; an explicitly empty non-nil slice means there are none.
type PredicateFilterBinding struct {
	Field  string
	Filter Filter
}

// PredicateProductKey binds a plan-local source role to the Catalog Product it
// reads. Product alone is insufficient for repeated inputs such as the two
// branches of UNION DISTINCT, while role alone would allow two Products with
// different type or collation contracts to collide.
type PredicateProductKey struct {
	Role    string
	Product string
}

type PredicateBindings struct {
	CatalogSHA256     string
	PublicationBundle []PredicatePublicationBinding
	ViewBindingSHA256 string
	Products          map[PredicateProductKey]Product
	Fields            map[string]PredicateFieldBinding
	CallerFilters     []PredicateFilterBinding
	SemanticProductID string
}

// PredicateProductsForSources converts compiler-validated relational sources
// into the exact composite bindings consumed by predicate accounting. Keeping
// the conversion here lets the production preparer and compiler-identity probe
// exercise one implementation without giving Gateway a second preparation
// path.
func PredicateProductsForSources(products map[string]Product,
	sources []RelationalSource) (map[PredicateProductKey]Product, error) {
	result := make(map[PredicateProductKey]Product, len(sources))
	for _, source := range sources {
		product, present := products[source.Product]
		if !present || product.Name != source.Product {
			return nil, fmt.Errorf("predicate source role %q names unbound product %q", source.Role, source.Product)
		}
		key := PredicateProductKey{Role: source.Role, Product: source.Product}
		if key.Role == "" || key.Product == "" {
			return nil, errors.New("predicate source binding is incomplete")
		}
		if _, duplicate := result[key]; duplicate {
			return nil, fmt.Errorf("predicate source role %q repeats product %q", key.Role, key.Product)
		}
		result[key] = product
	}
	return result, nil
}

type PredicateFootprint struct {
	Version         string
	ContextSHA256   string
	Atoms           []exposure.FactID
	AtomSetSHA256   string
	RawLiteralCount int
	UniqueAtomCount int
	DuplicateCount  int
	NullAtomCount   int
}

type predicateContextPayload struct {
	Version              string                        `json:"version"`
	CatalogSHA256        string                        `json:"catalog_sha256"`
	PublicationBundle    []PredicatePublicationBinding `json:"publication_bundle"`
	ViewBindingSHA256    string                        `json:"view_binding_sha256,omitempty"`
	CanonicalFromGraph   predicateFromGraph            `json:"canonical_from_graph"`
	EffectiveScopeSHA256 string                        `json:"effective_scope_sha256"`
}

type predicateFromGraph struct {
	Kind      string              `json:"kind"`
	Relations []predicateRelation `json:"relations"`
	Edges     []predicateEdge     `json:"edges,omitempty"`
}

type predicateRelation struct {
	Product         string `json:"product"`
	StableRole      string `json:"stable_role"`
	SourceNamespace string `json:"source_namespace"`
	Snapshot        string `json:"snapshot"`
}

type predicateEdge struct {
	Left  string `json:"left"`
	Right string `json:"right"`
}

type footprintFilter struct {
	field          string
	filter         Filter
	product        PredicateProductKey
	productIsBound bool
}

// BuildPredicateFootprint atomizes only caller-controlled literal filters,
// canonicalizes values according to their PostgreSQL type, and returns a
// sorted exact set. It performs no database I/O.
func BuildPredicateFootprint(plan QueryPlan, bindings PredicateBindings, effectiveScopeDigest string, limits PredicateLimits) (PredicateFootprint, error) {
	limits, err := limits.normalized()
	if err != nil {
		return PredicateFootprint{}, err
	}
	contextDigest, err := buildPredicateContext(plan, bindings, effectiveScopeDigest)
	if err != nil {
		return PredicateFootprint{}, err
	}
	filters, err := callerPredicateFilters(plan, bindings)
	if err != nil {
		return PredicateFootprint{}, err
	}
	result := PredicateFootprint{Version: PredicateFootprintVersion, ContextSHA256: contextDigest}
	type atomEntry struct {
		fact    exposure.FactID
		payload []byte
	}
	atoms := make(map[string]atomEntry)
	totalPayload := 0
	for _, item := range filters {
		field, err := resolvePredicateField(plan, bindings, item)
		if err != nil {
			return PredicateFootprint{}, err
		}
		operator, list, err := atomOperatorAndValues(item.filter)
		if err != nil {
			return PredicateFootprint{}, fmt.Errorf("atomize filter %q: %w", item.field, err)
		}
		if result.RawLiteralCount > limits.MaxRawLiteralsPerQuery-len(list) {
			return PredicateFootprint{}, fmt.Errorf("predicate raw literal limit exceeded: maximum %d", limits.MaxRawLiteralsPerQuery)
		}
		result.RawLiteralCount += len(list)
		for _, value := range list {
			canonical, err := exposure.CanonicalSQLValue(field.SQLType, value)
			if err != nil {
				return PredicateFootprint{}, fmt.Errorf("canonicalize predicate %q as %s: %w", item.field, field.SQLType, err)
			}
			fact, err := exposure.NewPredicateAtomFactV5(exposure.PredicateAtomFactV5{
				PredicateContextSHA256: contextDigest, SemanticProductID: field.SemanticProductID,
				StableRole: field.StableRole, PublicFieldID: field.PublicFieldID,
				ResolvedExpressionSHA256: field.ResolvedExpressionSHA256, SQLType: field.SQLType,
				CollationName: field.CollationName, CollationVersion: field.CollationVersion,
				Operator: operator, CanonicalLiteral: canonical,
			})
			if err != nil {
				return PredicateFootprint{}, err
			}
			hash, err := fact.Hash()
			if err != nil {
				return PredicateFootprint{}, err
			}
			payload, err := fact.CanonicalPayload()
			if err != nil {
				return PredicateFootprint{}, err
			}
			if len(payload) > limits.MaxAtomPayloadBytes {
				return PredicateFootprint{}, fmt.Errorf("predicate atom payload exceeds %d bytes", limits.MaxAtomPayloadBytes)
			}
			if existing, duplicate := atoms[hash]; duplicate {
				if !bytes.Equal(existing.payload, payload) {
					return PredicateFootprint{}, errors.New("predicate atom SHA-256 collision")
				}
				result.DuplicateCount++
				continue
			}
			if len(atoms) >= limits.MaxUniqueAtomsPerQuery {
				return PredicateFootprint{}, fmt.Errorf("predicate unique atom limit exceeded: maximum %d", limits.MaxUniqueAtomsPerQuery)
			}
			if totalPayload > limits.MaxTotalAtomPayloadBytes-len(payload) {
				return PredicateFootprint{}, fmt.Errorf("predicate atom payload total exceeds %d bytes", limits.MaxTotalAtomPayloadBytes)
			}
			totalPayload += len(payload)
			atoms[hash] = atomEntry{fact: fact, payload: payload}
			if canonical == "null" {
				result.NullAtomCount++
			}
		}
	}
	hashes := make([]string, 0, len(atoms))
	for hash := range atoms {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	result.Atoms = make([]exposure.FactID, 0, len(hashes))
	for _, hash := range hashes {
		result.Atoms = append(result.Atoms, atoms[hash].fact)
	}
	result.UniqueAtomCount = len(result.Atoms)
	result.AtomSetSHA256, err = PredicateSetDigestV1(result.Atoms)
	if err != nil {
		return PredicateFootprint{}, err
	}
	return result, nil
}

func atomOperatorAndValues(filter Filter) (string, []any, error) {
	op := strings.ToUpper(strings.TrimSpace(filter.Op))
	switch op {
	case "=":
		op = "EQ"
	case "!=", "<>":
		op = "NE"
	case "<":
		op = "LT"
	case "<=":
		op = "LE"
	case ">":
		op = "GT"
	case ">=":
		op = "GE"
	case "LIKE":
	case "IN":
		op = "EQ"
	case "NOT IN":
		op = "NE"
	default:
		return "", nil, fmt.Errorf("operator %q is outside %s", filter.Op, PredicateFootprintVersion)
	}
	if strings.EqualFold(strings.TrimSpace(filter.Op), "IN") || strings.EqualFold(strings.TrimSpace(filter.Op), "NOT IN") {
		values, ok := literalSlice(filter.Value)
		if !ok || len(values) == 0 {
			return "", nil, errors.New("IN requires a non-empty flat literal list")
		}
		return op, values, nil
	}
	return op, []any{filter.Value}, nil
}

func literalSlice(value any) ([]any, bool) {
	if values, ok := value.([]any); ok {
		return values, true
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() || rv.Kind() != reflect.Slice || rv.Type().Elem().Kind() == reflect.Uint8 {
		return nil, false
	}
	result := make([]any, rv.Len())
	for index := 0; index < rv.Len(); index++ {
		item := rv.Index(index)
		if item.Kind() == reflect.Slice || item.Kind() == reflect.Array || item.Kind() == reflect.Map || item.Kind() == reflect.Func {
			return nil, false
		}
		result[index] = item.Interface()
	}
	return result, true
}

func callerPredicateFilters(plan QueryPlan, bindings PredicateBindings) ([]footprintFilter, error) {
	if bindings.CallerFilters != nil {
		result := make([]footprintFilter, 0, len(bindings.CallerFilters))
		for _, filter := range bindings.CallerFilters {
			field := filter.Field
			if field == "" {
				field = filter.Filter.Column
			}
			result = append(result, footprintFilter{field: field, filter: filter.Filter})
		}
		return result, nil
	}
	result := make([]footprintFilter, 0, len(plan.Filters))
	defaultRole := ""
	var defaultProduct PredicateProductKey
	if plan.From == nil {
		key, _, err := legacyPredicateProduct(bindings.Products, plan.Product)
		if err != nil {
			return nil, err
		}
		defaultProduct = key
		defaultRole = key.Role
	}
	for _, filter := range plan.Filters {
		field := filter.Column
		if !strings.Contains(field, ".") && defaultRole != "" {
			field = defaultRole + "." + field
		}
		item := footprintFilter{field: field, filter: filter}
		if plan.From == nil {
			item.product, item.productIsBound = defaultProduct, true
		}
		result = append(result, item)
	}
	appendScan := func(scan Scan) {
		for _, filter := range scan.Filters {
			result = append(result, footprintFilter{
				field: scan.Role + "." + filter.Column, filter: filter,
				product: PredicateProductKey{Role: scan.Role, Product: scan.Product}, productIsBound: true,
			})
		}
	}
	if plan.From != nil {
		switch {
		case plan.From.Scan != nil:
			appendScan(*plan.From.Scan)
		case plan.From.Join != nil:
			appendScan(plan.From.Join.Left)
			appendScan(plan.From.Join.Right)
		case plan.From.JoinMany != nil:
			for _, scan := range plan.From.JoinMany.Sources {
				appendScan(scan)
			}
		case plan.From.UnionDistinct != nil:
			appendScan(plan.From.UnionDistinct.Left)
			appendScan(plan.From.UnionDistinct.Right)
		}
	}
	return result, nil
}

func resolvePredicateField(plan QueryPlan, bindings PredicateBindings, item footprintFilter) (PredicateFieldBinding, error) {
	fieldID := item.field
	if binding, present := bindings.Fields[fieldID]; present {
		return canonicalPredicateField(binding)
	}
	role, column, ok := splitFieldID(fieldID)
	if !ok {
		return PredicateFieldBinding{}, fmt.Errorf("predicate field %q has no stable role", fieldID)
	}
	var keys []PredicateProductKey
	if item.productIsBound {
		if item.product.Role != role {
			return PredicateFieldBinding{}, fmt.Errorf(
				"predicate field %q disagrees with its bound source role %q", fieldID, item.product.Role)
		}
		keys = []PredicateProductKey{item.product}
	} else if plan.From == nil {
		key, _, err := legacyPredicateProduct(bindings.Products, plan.Product)
		if err != nil {
			return PredicateFieldBinding{}, err
		}
		keys = []PredicateProductKey{key}
	} else {
		keys = predicateProductKeysForRole(plan, role)
	}
	if len(keys) == 0 {
		return PredicateFieldBinding{}, fmt.Errorf("predicate field %q is absent from its bound relation", fieldID)
	}
	matches := make([]Product, 0, len(keys))
	for _, key := range keys {
		product, err := boundPredicateProduct(bindings.Products, key)
		if err != nil {
			return PredicateFieldBinding{}, err
		}
		if _, present := product.ColumnTypes[column]; present {
			matches = append(matches, product)
		}
	}
	if len(matches) == 0 {
		return PredicateFieldBinding{}, fmt.Errorf("predicate field %q is absent from its bound product", fieldID)
	}
	first := matches[0]
	typeName, err := exposure.CanonicalSQLTypeV2(first.ColumnTypes[column])
	if err != nil {
		return PredicateFieldBinding{}, err
	}
	distinctProducts := false
	for _, product := range matches[1:] {
		other, typeErr := exposure.CanonicalSQLTypeV2(product.ColumnTypes[column])
		if typeErr != nil || other != typeName || product.ColumnCollations[column] != first.ColumnCollations[column] ||
			product.CollationVersions[column] != first.CollationVersions[column] {
			return PredicateFieldBinding{}, fmt.Errorf("predicate field %q has ambiguous union type or collation", fieldID)
		}
		if product.Name != first.Name {
			distinctProducts = true
		}
	}
	productID := first.Name
	if bindings.SemanticProductID != "" {
		productID = bindings.SemanticProductID
	} else if distinctProducts {
		productID = "union:" + role
	}
	return canonicalPredicateField(PredicateFieldBinding{SemanticProductID: productID, StableRole: role,
		PublicFieldID: column, SQLType: typeName, CollationName: first.ColumnCollations[column],
		CollationVersion: first.CollationVersions[column]})
}

func predicateProductKeysForRole(plan QueryPlan, role string) []PredicateProductKey {
	if plan.From == nil {
		return nil
	}
	key := func(scan Scan) PredicateProductKey {
		return PredicateProductKey{Role: scan.Role, Product: scan.Product}
	}
	var result []PredicateProductKey
	appendScan := func(scan Scan) {
		if scan.Role == role {
			result = append(result, key(scan))
		}
	}
	switch {
	case plan.From.Scan != nil:
		appendScan(*plan.From.Scan)
	case plan.From.Join != nil:
		appendScan(plan.From.Join.Left)
		appendScan(plan.From.Join.Right)
	case plan.From.JoinMany != nil:
		for _, scan := range plan.From.JoinMany.Sources {
			appendScan(scan)
		}
	case plan.From.UnionDistinct != nil:
		union := plan.From.UnionDistinct
		if union.Role == role {
			return []PredicateProductKey{key(union.Left), key(union.Right)}
		}
		appendScan(union.Left)
		appendScan(union.Right)
	}
	return result
}

func boundPredicateProduct(products map[PredicateProductKey]Product, key PredicateProductKey) (Product, error) {
	product, present := products[key]
	if !present {
		return Product{}, fmt.Errorf("predicate bindings omit source role %q for product %q", key.Role, key.Product)
	}
	if product.Name != key.Product {
		return Product{}, fmt.Errorf(
			"predicate binding for source role %q and product %q contains product %q",
			key.Role, key.Product, product.Name)
	}
	return product, nil
}

func legacyPredicateProduct(products map[PredicateProductKey]Product, name string) (PredicateProductKey, Product, error) {
	var expected *PredicateProductKey
	for key, product := range products {
		if key.Product != name {
			continue
		}
		if product.Name != name {
			return PredicateProductKey{}, Product{}, fmt.Errorf(
				"predicate binding for product %q contains product %q", name, product.Name)
		}
		role := product.StableRole
		if role == "" {
			role = product.Name
		}
		candidate := PredicateProductKey{Role: role, Product: name}
		if key != candidate {
			return PredicateProductKey{}, Product{}, fmt.Errorf(
				"predicate binding for product %q does not use its stable role", name)
		}
		if expected != nil {
			return PredicateProductKey{}, Product{}, fmt.Errorf(
				"predicate bindings repeat product %q", name)
		}
		expected = &candidate
	}
	if expected == nil {
		return PredicateProductKey{}, Product{}, fmt.Errorf("predicate bindings omit product %q", name)
	}
	product, err := boundPredicateProduct(products, *expected)
	return *expected, product, err
}

func canonicalPredicateField(field PredicateFieldBinding) (PredicateFieldBinding, error) {
	var err error
	field.SQLType, err = exposure.CanonicalSQLTypeV2(field.SQLType)
	if err != nil {
		return PredicateFieldBinding{}, err
	}
	if strings.TrimSpace(field.SemanticProductID) == "" || strings.TrimSpace(field.StableRole) == "" ||
		strings.TrimSpace(field.PublicFieldID) == "" {
		return PredicateFieldBinding{}, errors.New("predicate field lacks a stable public identity")
	}
	return field, nil
}

func buildPredicateContext(plan QueryPlan, bindings PredicateBindings, scopeDigest string) (string, error) {
	graph, err := predicateGraph(plan, bindings.Products)
	if err != nil {
		return "", err
	}
	publications := append([]PredicatePublicationBinding(nil), bindings.PublicationBundle...)
	if len(publications) == 0 {
		seen := make(map[string]struct{})
		for _, relation := range graph.Relations {
			product, productErr := boundPredicateProduct(bindings.Products, PredicateProductKey{
				Role: relation.StableRole, Product: relation.Product,
			})
			if productErr != nil {
				return "", productErr
			}
			binding := PredicatePublicationBinding{SemanticProductID: product.Name, StableRole: relation.StableRole,
				SourceNamespace: product.SourceNamespace, Snapshot: product.Snapshot,
				Publication: product.SnapshotPublication, PublicationSHA256: product.SidecarManifestDigest,
				LineageSHA256: product.LineageDigest}
			keyBytes, _ := json.Marshal(binding)
			if _, duplicate := seen[string(keyBytes)]; !duplicate {
				seen[string(keyBytes)] = struct{}{}
				publications = append(publications, binding)
			}
		}
	}
	sort.Slice(publications, func(i, j int) bool {
		left, _ := json.Marshal(publications[i])
		right, _ := json.Marshal(publications[j])
		return bytes.Compare(left, right) < 0
	})
	payload := predicateContextPayload{Version: PredicateFootprintVersion, CatalogSHA256: bindings.CatalogSHA256,
		PublicationBundle: publications, ViewBindingSHA256: bindings.ViewBindingSHA256,
		CanonicalFromGraph: graph, EffectiveScopeSHA256: scopeDigest}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(predicateContextDomain), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func predicateGraph(plan QueryPlan, products map[PredicateProductKey]Product) (predicateFromGraph, error) {
	graph := predicateFromGraph{Kind: "scan"}
	addScan := func(scan Scan) error {
		product, err := boundPredicateProduct(products, PredicateProductKey{Role: scan.Role, Product: scan.Product})
		if err != nil {
			return err
		}
		role := scan.Role
		if role == "" {
			role = product.StableRole
		}
		graph.Relations = append(graph.Relations, predicateRelation{Product: product.Name, StableRole: role,
			SourceNamespace: product.SourceNamespace, Snapshot: product.Snapshot})
		return nil
	}
	if plan.From == nil {
		key, product, err := legacyPredicateProduct(products, plan.Product)
		if err != nil {
			return graph, err
		}
		return predicateFromGraph{Kind: "scan", Relations: []predicateRelation{{Product: product.Name,
			StableRole: key.Role, SourceNamespace: product.SourceNamespace, Snapshot: product.Snapshot}}}, nil
	}
	switch {
	case plan.From.Scan != nil:
		if err := addScan(*plan.From.Scan); err != nil {
			return graph, err
		}
	case plan.From.Join != nil:
		graph.Kind = "join"
		if err := addScan(plan.From.Join.Left); err != nil {
			return graph, err
		}
		if err := addScan(plan.From.Join.Right); err != nil {
			return graph, err
		}
		graph.Edges = predicateEdges(plan.From.Join.On)
	case plan.From.JoinMany != nil:
		graph.Kind = "join"
		for _, scan := range plan.From.JoinMany.Sources {
			if err := addScan(scan); err != nil {
				return graph, err
			}
		}
		graph.Edges = predicateEdges(plan.From.JoinMany.On)
	case plan.From.UnionDistinct != nil:
		graph.Kind = "union_distinct"
		if err := addScan(plan.From.UnionDistinct.Left); err != nil {
			return graph, err
		}
		if err := addScan(plan.From.UnionDistinct.Right); err != nil {
			return graph, err
		}
	default:
		return graph, errors.New("predicate context requires one from operator")
	}
	sort.Slice(graph.Relations, func(i, j int) bool {
		left := graph.Relations[i]
		right := graph.Relations[j]
		return left.SourceNamespace+"\x00"+left.Snapshot+"\x00"+left.StableRole+"\x00"+left.Product <
			right.SourceNamespace+"\x00"+right.Snapshot+"\x00"+right.StableRole+"\x00"+right.Product
	})
	return graph, nil
}

func predicateEdges(input []JoinPredicate) []predicateEdge {
	result := make([]predicateEdge, 0, len(input))
	for _, edge := range input {
		left, right := edge.Left, edge.Right
		if right < left {
			left, right = right, left
		}
		result = append(result, predicateEdge{Left: left, Right: right})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Left+"\x00"+result[i].Right < result[j].Left+"\x00"+result[j].Right
	})
	return result
}

// PredicateSetDigestV1 commits a sorted, duplicate-free set of full V5 atom
// hashes. Hash collisions with different canonical payloads fail closed.
func PredicateSetDigestV1(atoms []exposure.FactID) (string, error) {
	return exposure.PredicateSetHashV1(atoms)
}
