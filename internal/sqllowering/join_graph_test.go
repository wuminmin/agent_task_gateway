package sqllowering

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func TestJoinGraphCanonicalDigestIgnoresGraphSpelling(t *testing.T) {
	products := graphTestProducts(5)
	base := JoinGraph{
		Nodes: []RelationNode{
			{Relation: "relation_00", Product: "product_00"},
			{Relation: "relation_01", Product: "product_01"},
			{Relation: "relation_02", Product: "product_02"},
			{Relation: "relation_03", Product: "product_03"},
			{Relation: "relation_04", Product: "product_04"},
		},
		Edges: []JoinEdge{
			{LeftRelation: "relation_00", RightRelation: "relation_01", Predicates: []EqualityPredicate{{LeftColumn: "id", RightColumn: "id"}, {LeftColumn: "tenant_id", RightColumn: "tenant_id"}}},
			{LeftRelation: "relation_01", RightRelation: "relation_02", Predicates: []EqualityPredicate{{LeftColumn: "id", RightColumn: "id"}}},
			{LeftRelation: "relation_02", RightRelation: "relation_03", Predicates: []EqualityPredicate{{LeftColumn: "id", RightColumn: "id"}}},
			{LeftRelation: "relation_03", RightRelation: "relation_04", Predicates: []EqualityPredicate{{LeftColumn: "id", RightColumn: "id"}}},
		},
	}
	wantDigest, err := base.Digest(products)
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical, err := base.Canonical(products)
	if err != nil {
		t.Fatal(err)
	}

	for seed := int64(0); seed < 100; seed++ {
		random := rand.New(rand.NewSource(seed))
		candidate := cloneJoinGraph(base)
		random.Shuffle(len(candidate.Nodes), func(i, j int) { candidate.Nodes[i], candidate.Nodes[j] = candidate.Nodes[j], candidate.Nodes[i] })
		random.Shuffle(len(candidate.Edges), func(i, j int) { candidate.Edges[i], candidate.Edges[j] = candidate.Edges[j], candidate.Edges[i] })
		for edgeIndex := range candidate.Edges {
			edge := &candidate.Edges[edgeIndex]
			random.Shuffle(len(edge.Predicates), func(i, j int) { edge.Predicates[i], edge.Predicates[j] = edge.Predicates[j], edge.Predicates[i] })
			if random.Intn(2) == 0 {
				edge.LeftRelation, edge.RightRelation = edge.RightRelation, edge.LeftRelation
				for predicateIndex := range edge.Predicates {
					edge.Predicates[predicateIndex].LeftColumn, edge.Predicates[predicateIndex].RightColumn = edge.Predicates[predicateIndex].RightColumn, edge.Predicates[predicateIndex].LeftColumn
				}
			}
		}
		gotDigest, digestErr := candidate.Digest(products)
		if digestErr != nil {
			t.Fatalf("seed %d: %v", seed, digestErr)
		}
		gotCanonical, canonicalErr := candidate.Canonical(products)
		if canonicalErr != nil {
			t.Fatalf("seed %d: %v", seed, canonicalErr)
		}
		twiceCanonical, twiceErr := gotCanonical.Canonical(products)
		if twiceErr != nil {
			t.Fatalf("seed %d canonical idempotence: %v", seed, twiceErr)
		}
		if gotDigest != wantDigest || !reflect.DeepEqual(gotCanonical, wantCanonical) {
			t.Fatalf("seed %d changed canonical graph: digest %s != %s\ngot=%#v\nwant=%#v", seed, gotDigest, wantDigest, gotCanonical, wantCanonical)
		}
		if !reflect.DeepEqual(twiceCanonical, gotCanonical) {
			t.Fatalf("seed %d canonicalization is not idempotent:\nonce=%#v\ntwice=%#v", seed, gotCanonical, twiceCanonical)
		}
	}
}

func TestJoinGraphCanonicalGroupsMultiplePredicatesPerEdge(t *testing.T) {
	products := graphTestProducts(2)
	graph := JoinGraph{
		Nodes: []RelationNode{{Relation: "relation_01", Product: "product_01"}, {Relation: "relation_00", Product: "product_00"}},
		Edges: []JoinEdge{
			{LeftRelation: "relation_00", RightRelation: "relation_01", Predicates: []EqualityPredicate{{LeftColumn: "tenant_id", RightColumn: "tenant_id"}}},
			{LeftRelation: "relation_01", RightRelation: "relation_00", Predicates: []EqualityPredicate{{LeftColumn: "id", RightColumn: "id"}}},
		},
	}
	canonical, err := graph.Canonical(products)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical.Edges) != 1 || len(canonical.Edges[0].Predicates) != 2 {
		t.Fatalf("canonical edge grouping = %#v", canonical.Edges)
	}
	join, err := canonical.JoinMany(products)
	if err != nil {
		t.Fatal(err)
	}
	if len(join.Sources) != 2 || len(join.On) != 2 {
		t.Fatalf("join_many = %#v", join)
	}
}

func TestJoinGraphRejectsDisconnectedAndDuplicateEqualities(t *testing.T) {
	products := graphTestProducts(3)
	nodes := []RelationNode{
		{Relation: "relation_00", Product: "product_00"},
		{Relation: "relation_01", Product: "product_01"},
		{Relation: "relation_02", Product: "product_02"},
	}
	if _, err := (JoinGraph{Nodes: nodes, Edges: []JoinEdge{{LeftRelation: "relation_00", RightRelation: "relation_01", Predicates: []EqualityPredicate{{LeftColumn: "id", RightColumn: "id"}}}}}).Canonical(products); err == nil {
		t.Fatal("disconnected graph was accepted")
	}
	duplicate := JoinGraph{Nodes: nodes[:2], Edges: []JoinEdge{{LeftRelation: "relation_00", RightRelation: "relation_01", Predicates: []EqualityPredicate{
		{LeftColumn: "id", RightColumn: "id"}, {LeftColumn: "id", RightColumn: "id"},
	}}}}
	if _, err := duplicate.Canonical(products); err == nil {
		t.Fatal("duplicate equality was accepted")
	}
}

func graphTestProducts(count int) map[string]queryplan.Product {
	products := make(map[string]queryplan.Product, count)
	for index := 0; index < count; index++ {
		productName := fmt.Sprintf("product_%02d", index)
		relation := fmt.Sprintf("relation_%02d", index)
		products[productName] = queryplan.Product{
			Name: productName, StableRole: relation, SourceNamespace: "graph." + relation, Snapshot: "snapshot-1",
			StableEntityKey: []string{"id"},
			Columns:         map[string]struct{}{"id": {}, "tenant_id": {}, "value": {}},
			ColumnTypes:     map[string]string{"id": "integer", "tenant_id": "integer", "value": "integer"},
			AllowedAggregates: map[string]struct{}{
				"count": {}, "sum": {}, "min": {}, "max": {},
			},
		}
	}
	return products
}

func cloneJoinGraph(graph JoinGraph) JoinGraph {
	result := JoinGraph{Nodes: append([]RelationNode(nil), graph.Nodes...), Edges: make([]JoinEdge, len(graph.Edges))}
	for index, edge := range graph.Edges {
		result.Edges[index] = edge
		result.Edges[index].Predicates = append([]EqualityPredicate(nil), edge.Predicates...)
	}
	return result
}

func TestJoinGraphDigestDistinguishesChangedEdgeAndColumn(t *testing.T) {
	products := graphTestProducts(3)
	base := JoinGraph{
		Nodes: []RelationNode{
			{Relation: "relation_00", Product: "product_00"},
			{Relation: "relation_01", Product: "product_01"},
			{Relation: "relation_02", Product: "product_02"},
		},
		Edges: []JoinEdge{
			{LeftRelation: "relation_00", RightRelation: "relation_01", Predicates: []EqualityPredicate{{LeftColumn: "id", RightColumn: "id"}}},
			{LeftRelation: "relation_01", RightRelation: "relation_02", Predicates: []EqualityPredicate{{LeftColumn: "id", RightColumn: "id"}}},
		},
	}
	baseDigest, err := base.Digest(products)
	if err != nil {
		t.Fatal(err)
	}

	changedEdge := cloneJoinGraph(base)
	changedEdge.Edges[1].LeftRelation = "relation_00"
	changedEdgeDigest, err := changedEdge.Digest(products)
	if err != nil {
		t.Fatal(err)
	}
	changedColumn := cloneJoinGraph(base)
	changedColumn.Edges[0].Predicates[0] = EqualityPredicate{LeftColumn: "tenant_id", RightColumn: "tenant_id"}
	changedColumnDigest, err := changedColumn.Digest(products)
	if err != nil {
		t.Fatal(err)
	}
	if baseDigest == changedEdgeDigest {
		t.Fatal("changing a graph edge did not change the join-graph digest")
	}
	if baseDigest == changedColumnDigest {
		t.Fatal("changing an equality column did not change the join-graph digest")
	}
	if changedEdgeDigest == changedColumnDigest {
		t.Fatal("distinct edge and column mutations collapsed to the same join-graph digest")
	}
}

func TestJoinGraphDigestBindsCatalogSnapshotAndLineage(t *testing.T) {
	products := graphTestProducts(2)
	graph := JoinGraph{
		Nodes: []RelationNode{{Relation: "relation_00", Product: "product_00"}, {Relation: "relation_01", Product: "product_01"}},
		Edges: []JoinEdge{{LeftRelation: "relation_00", RightRelation: "relation_01",
			Predicates: []EqualityPredicate{{LeftColumn: "id", RightColumn: "id"}}}},
	}
	base, err := graph.Digest(products)
	if err != nil {
		t.Fatal(err)
	}
	changedSnapshot := graphTestProducts(2)
	product := changedSnapshot["product_01"]
	product.Snapshot = "snapshot-2"
	changedSnapshot["product_01"] = product
	snapshotDigest, err := graph.Digest(changedSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	changedLineage := graphTestProducts(2)
	product = changedLineage["product_01"]
	product.LineageDigest = "lineage-2"
	changedLineage["product_01"] = product
	lineageDigest, err := graph.Digest(changedLineage)
	if err != nil {
		t.Fatal(err)
	}
	if base == snapshotDigest || base == lineageDigest || snapshotDigest == lineageDigest {
		t.Fatalf("Catalog-bound graph digests collapsed: base=%s snapshot=%s lineage=%s", base, snapshotDigest, lineageDigest)
	}
}
