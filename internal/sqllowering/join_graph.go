package sqllowering

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

const joinGraphDigestDomain = "taskgate-join-graph-v1\x00"

// JoinGraph is the canonical intermediate representation between PostgreSQL's
// binary JOIN AST and QueryPlan.JoinMany. SQL aliases and parenthesization are
// deliberately absent: relations are identified by their Catalog stable role.
type JoinGraph struct {
	Nodes []RelationNode `json:"nodes"`
	Edges []JoinEdge     `json:"edges,omitempty"`
}

// RelationNode identifies one approved Catalog product in an equijoin graph.
// Relation is the Catalog stable role, not the caller-selected SQL alias.
type RelationNode struct {
	Relation string `json:"relation"`
	Product  string `json:"product"`
}

// JoinEdge groups every equality predicate between the same pair of
// relations. Edge and predicate direction have no semantic significance.
type JoinEdge struct {
	LeftRelation  string              `json:"left_relation"`
	RightRelation string              `json:"right_relation"`
	Predicates    []EqualityPredicate `json:"predicates"`
}

// EqualityPredicate names one approved column from each endpoint of its edge.
type EqualityPredicate struct {
	LeftColumn  string `json:"left_column"`
	RightColumn string `json:"right_column"`
}

// Canonical returns a validated graph whose nodes, edges, endpoint direction,
// and predicate conjunctions have a unique order. It accepts cycles and any
// connected graph shape, but rejects self edges, duplicate equalities,
// unapproved fields, and incompatible equality keys.
func (graph JoinGraph) Canonical(products map[string]queryplan.Product) (JoinGraph, error) {
	if len(graph.Nodes) < 2 || len(graph.Nodes) > queryplan.MaxJoinSources {
		return JoinGraph{}, fmt.Errorf("join graph requires between 2 and %d relations", queryplan.MaxJoinSources)
	}

	result := JoinGraph{Nodes: append([]RelationNode(nil), graph.Nodes...)}
	byRelation := make(map[string]RelationNode, len(result.Nodes))
	for _, node := range result.Nodes {
		product, present := products[node.Product]
		if !present || product.Name != node.Product {
			return JoinGraph{}, fmt.Errorf("join graph product %q is not approved", node.Product)
		}
		if node.Relation == "" || node.Relation != product.StableRole {
			return JoinGraph{}, fmt.Errorf("join graph relation %q is not the Catalog stable role for product %q", node.Relation, node.Product)
		}
		if _, duplicate := byRelation[node.Relation]; duplicate {
			return JoinGraph{}, fmt.Errorf("join graph relation %q is repeated", node.Relation)
		}
		byRelation[node.Relation] = node
	}
	sort.Slice(result.Nodes, func(i, j int) bool {
		return joinGraphNodeKey(result.Nodes[i], products) < joinGraphNodeKey(result.Nodes[j], products)
	})
	rank := make(map[string]int, len(result.Nodes))
	for index, node := range result.Nodes {
		rank[node.Relation] = index
	}

	edgesByPair := make(map[string]*JoinEdge, len(graph.Edges))
	for _, input := range graph.Edges {
		leftNode, leftPresent := byRelation[input.LeftRelation]
		rightNode, rightPresent := byRelation[input.RightRelation]
		if !leftPresent || !rightPresent || input.LeftRelation == input.RightRelation {
			return JoinGraph{}, errors.New("join graph edge must connect two distinct relations")
		}
		if len(input.Predicates) == 0 {
			return JoinGraph{}, errors.New("join graph edge requires at least one equality predicate")
		}
		predicates := append([]EqualityPredicate(nil), input.Predicates...)
		leftRelation, rightRelation := input.LeftRelation, input.RightRelation
		if rank[leftRelation] > rank[rightRelation] {
			leftRelation, rightRelation = rightRelation, leftRelation
			leftNode, rightNode = rightNode, leftNode
			for index := range predicates {
				predicates[index].LeftColumn, predicates[index].RightColumn = predicates[index].RightColumn, predicates[index].LeftColumn
			}
		}
		key := leftRelation + "\x00" + rightRelation
		edge := edgesByPair[key]
		if edge == nil {
			edge = &JoinEdge{LeftRelation: leftRelation, RightRelation: rightRelation}
			edgesByPair[key] = edge
		}
		for _, predicate := range predicates {
			if err := validateGraphEquality(leftNode, rightNode, predicate, products); err != nil {
				return JoinGraph{}, err
			}
			edge.Predicates = append(edge.Predicates, predicate)
		}
	}

	result.Edges = make([]JoinEdge, 0, len(edgesByPair))
	for _, edge := range edgesByPair {
		sort.Slice(edge.Predicates, func(i, j int) bool {
			left := edge.Predicates[i].LeftColumn + "\x00" + edge.Predicates[i].RightColumn
			right := edge.Predicates[j].LeftColumn + "\x00" + edge.Predicates[j].RightColumn
			return left < right
		})
		for index := 1; index < len(edge.Predicates); index++ {
			if edge.Predicates[index] == edge.Predicates[index-1] {
				return JoinGraph{}, errors.New("duplicate join graph equality predicate")
			}
		}
		result.Edges = append(result.Edges, *edge)
	}
	sort.Slice(result.Edges, func(i, j int) bool {
		left := result.Edges[i].LeftRelation + "\x00" + result.Edges[i].RightRelation
		right := result.Edges[j].LeftRelation + "\x00" + result.Edges[j].RightRelation
		return left < right
	})

	if len(result.Edges) == 0 || !joinGraphConnected(result.Nodes, result.Edges) {
		return JoinGraph{}, errors.New("join graph must be connected")
	}
	return result, nil
}

// JoinMany validates and canonicalizes the graph, then erases the graph-only
// edge grouping into the existing trusted flat QueryPlan representation.
func (graph JoinGraph) JoinMany(products map[string]queryplan.Product) (queryplan.JoinMany, error) {
	canonical, err := graph.Canonical(products)
	if err != nil {
		return queryplan.JoinMany{}, err
	}
	sources := make([]queryplan.Scan, 0, len(canonical.Nodes))
	for _, node := range canonical.Nodes {
		sources = append(sources, queryplan.Scan{Product: node.Product, Role: node.Relation})
	}
	predicates := make([]queryplan.JoinPredicate, 0)
	for _, edge := range canonical.Edges {
		for _, predicate := range edge.Predicates {
			predicates = append(predicates, queryplan.JoinPredicate{
				Left:  edge.LeftRelation + "." + predicate.LeftColumn,
				Right: edge.RightRelation + "." + predicate.RightColumn,
			})
		}
	}
	return queryplan.JoinMany{Sources: sources, On: predicates}, nil
}

// Digest identifies only the canonical, Catalog-bound equijoin graph. It is a
// structural sub-digest for tests and diagnostics, never the authorization,
// replay, settlement, or OutcomeFact identity; those continue to use the
// complete typed algebra normal-form digest computed downstream.
func (graph JoinGraph) Digest(products map[string]queryplan.Product) (string, error) {
	canonical, err := graph.Canonical(products)
	if err != nil {
		return "", err
	}
	type digestNode struct {
		Relation        string `json:"relation"`
		Product         string `json:"product"`
		SourceNamespace string `json:"source_namespace"`
		Snapshot        string `json:"snapshot"`
		LineageDigest   string `json:"lineage_digest,omitempty"`
	}
	type digestPredicate struct {
		LeftColumn            string `json:"left_column"`
		RightColumn           string `json:"right_column"`
		LeftSQLType           string `json:"left_sql_type"`
		RightSQLType          string `json:"right_sql_type"`
		LeftCollation         string `json:"left_collation,omitempty"`
		RightCollation        string `json:"right_collation,omitempty"`
		LeftCollationVersion  string `json:"left_collation_version,omitempty"`
		RightCollationVersion string `json:"right_collation_version,omitempty"`
	}
	type digestEdge struct {
		LeftRelation  string            `json:"left_relation"`
		RightRelation string            `json:"right_relation"`
		Predicates    []digestPredicate `json:"predicates"`
	}
	payload := struct {
		Nodes []digestNode `json:"nodes"`
		Edges []digestEdge `json:"edges"`
	}{Nodes: make([]digestNode, 0, len(canonical.Nodes)), Edges: make([]digestEdge, 0, len(canonical.Edges))}
	nodesByRelation := make(map[string]RelationNode, len(canonical.Nodes))
	for _, node := range canonical.Nodes {
		product := products[node.Product]
		nodesByRelation[node.Relation] = node
		payload.Nodes = append(payload.Nodes, digestNode{Relation: node.Relation, Product: node.Product,
			SourceNamespace: product.SourceNamespace, Snapshot: product.Snapshot, LineageDigest: product.LineageDigest})
	}
	for _, edge := range canonical.Edges {
		leftNode, rightNode := nodesByRelation[edge.LeftRelation], nodesByRelation[edge.RightRelation]
		leftProduct, rightProduct := products[leftNode.Product], products[rightNode.Product]
		digestEntry := digestEdge{LeftRelation: edge.LeftRelation, RightRelation: edge.RightRelation,
			Predicates: make([]digestPredicate, 0, len(edge.Predicates))}
		for _, predicate := range edge.Predicates {
			leftType, _ := exposure.CanonicalSQLTypeV2(leftProduct.ColumnTypes[predicate.LeftColumn])
			rightType, _ := exposure.CanonicalSQLTypeV2(rightProduct.ColumnTypes[predicate.RightColumn])
			digestEntry.Predicates = append(digestEntry.Predicates, digestPredicate{
				LeftColumn: predicate.LeftColumn, RightColumn: predicate.RightColumn,
				LeftSQLType: leftType, RightSQLType: rightType,
				LeftCollation: leftProduct.ColumnCollations[predicate.LeftColumn], RightCollation: rightProduct.ColumnCollations[predicate.RightColumn],
				LeftCollationVersion: leftProduct.CollationVersions[predicate.LeftColumn], RightCollationVersion: rightProduct.CollationVersions[predicate.RightColumn],
			})
		}
		payload.Edges = append(payload.Edges, digestEntry)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(joinGraphDigestDomain), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func validateGraphEquality(left, right RelationNode, predicate EqualityPredicate, products map[string]queryplan.Product) error {
	leftProduct, rightProduct := products[left.Product], products[right.Product]
	if _, approved := leftProduct.Columns[predicate.LeftColumn]; !approved {
		return fmt.Errorf("join graph column %q is not approved for product %q", predicate.LeftColumn, left.Product)
	}
	if _, approved := rightProduct.Columns[predicate.RightColumn]; !approved {
		return fmt.Errorf("join graph column %q is not approved for product %q", predicate.RightColumn, right.Product)
	}
	leftType, leftErr := exposure.CanonicalSQLTypeV2(leftProduct.ColumnTypes[predicate.LeftColumn])
	rightType, rightErr := exposure.CanonicalSQLTypeV2(rightProduct.ColumnTypes[predicate.RightColumn])
	if leftErr != nil || rightErr != nil || leftType != rightType {
		return fmt.Errorf("join graph keys %q and %q require identical types", left.Relation+"."+predicate.LeftColumn, right.Relation+"."+predicate.RightColumn)
	}
	leftCollation, rightCollation := leftProduct.ColumnCollations[predicate.LeftColumn], rightProduct.ColumnCollations[predicate.RightColumn]
	leftVersion, rightVersion := leftProduct.CollationVersions[predicate.LeftColumn], rightProduct.CollationVersions[predicate.RightColumn]
	if leftCollation != rightCollation || leftVersion != rightVersion {
		return fmt.Errorf("join graph keys %q and %q require identical deterministic collation profiles", left.Relation+"."+predicate.LeftColumn, right.Relation+"."+predicate.RightColumn)
	}
	return nil
}

func joinGraphNodeKey(node RelationNode, products map[string]queryplan.Product) string {
	product := products[node.Product]
	return strings.Join([]string{product.SourceNamespace, product.Snapshot, product.StableRole, product.Name}, "\x00")
}

func joinGraphConnected(nodes []RelationNode, edges []JoinEdge) bool {
	parents := make([]int, len(nodes))
	byRelation := make(map[string]int, len(nodes))
	for index, node := range nodes {
		parents[index] = index
		byRelation[node.Relation] = index
	}
	var find func(int) int
	find = func(value int) int {
		if parents[value] != value {
			parents[value] = find(parents[value])
		}
		return parents[value]
	}
	for _, edge := range edges {
		left, right := find(byRelation[edge.LeftRelation]), find(byRelation[edge.RightRelation])
		if left != right {
			parents[right] = left
		}
	}
	root := find(0)
	for index := 1; index < len(nodes); index++ {
		if find(index) != root {
			return false
		}
	}
	return true
}
