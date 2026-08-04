package catalogschema

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

func source() catalog.Source {
	return catalog.Source{Name: "travel_demo", SchemaDigest: "digest-of-source"}
}

// The entry count is the governed Product count, not the number of distinct
// reporting views. Two Products naming one view produce two ExpectedSchema
// entries and therefore two attestation passes, which is real statements the
// observer accounting must expect. An earlier evaluation-side derivation used a
// unique-view map and would undercount this Catalog by one entry per duplicate.
func TestDuplicateReportingViewsProduceOneEntryEach(t *testing.T) {
	built, err := Build(&catalog.Catalog{
		Sources: []catalog.Source{source()},
		Products: []catalog.Product{
			{Name: "a", Source: "travel_demo", ReportingView: "reporting.shared",
				Fields: []catalog.Field{{Name: "id", Type: "bigint"}}},
			{Name: "b", Source: "travel_demo", ReportingView: "reporting.shared",
				Fields: []catalog.Field{{Name: "id", Type: "bigint"}}},
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.Count != 2 {
		t.Fatalf("two Products on one reporting view produced %d entries, want 2", built.Count)
	}
	if len(built.Entries) != 2 {
		t.Fatalf("entry list has %d members, want 2", len(built.Entries))
	}
	for _, entry := range built.Entries {
		if entry.Schema != "reporting" || entry.View != "shared" {
			t.Fatalf("unexpected entry %+v", entry)
		}
	}
}

// Ordering is Catalog Product order and is significant: the attestations run in
// it, so a reordering is a different ExpectedSchema.
func TestEntryOrderFollowsCatalogProductOrderAndChangesTheDigest(t *testing.T) {
	first := catalog.Product{Name: "a", Source: "travel_demo", ReportingView: "reporting.one",
		Fields: []catalog.Field{{Name: "id", Type: "bigint"}}}
	second := catalog.Product{Name: "b", Source: "travel_demo", ReportingView: "reporting.two",
		Fields: []catalog.Field{{Name: "id", Type: "bigint"}}}

	forward, err := Build(&catalog.Catalog{Sources: []catalog.Source{source()},
		Products: []catalog.Product{first, second}})
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := Build(&catalog.Catalog{Sources: []catalog.Source{source()},
		Products: []catalog.Product{second, first}})
	if err != nil {
		t.Fatal(err)
	}
	if forward.Entries[0].View != "one" || reversed.Entries[0].View != "two" {
		t.Fatal("entry order does not follow Catalog Product order")
	}
	if forward.Digest == reversed.Digest {
		t.Fatal("reordering the ExpectedSchema did not change its digest")
	}
}

// Products carrying a ViewContract are attested by the task-scoped registry
// binding instead, so they must not add ExpectedSchema entries.
func TestViewContractProductsAreExcluded(t *testing.T) {
	built, err := Build(&catalog.Catalog{
		Sources: []catalog.Source{source()},
		Products: []catalog.Product{
			{Name: "governed", Source: "travel_demo", ReportingView: "reporting.governed",
				Fields: []catalog.Field{{Name: "id", Type: "bigint"}}},
			{Name: "semantic", Source: "travel_demo", ReportingView: "reporting.semantic",
				Fields: []catalog.Field{{Name: "id", Type: "bigint"}}, ViewContract: &catalog.ViewContract{}},
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.Count != 1 {
		t.Fatalf("a ViewContract Product contributed an ExpectedSchema entry: count=%d", built.Count)
	}
}

// The digest must react to every field the attestation compares, or a drifting
// Catalog could keep an unchanged digest.
func TestDigestCoversEveryAttestedField(t *testing.T) {
	base := catalog.Product{Name: "a", Source: "travel_demo", ReportingView: "reporting.one",
		Fields: []catalog.Field{{Name: "id", Type: "bigint", Collation: "en_US.utf8", CollationVersion: "2.36"}}}
	baseline, err := Build(&catalog.Catalog{Sources: []catalog.Source{source()},
		Products: []catalog.Product{base}})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*catalog.Product){
		"a different reporting view": func(p *catalog.Product) { p.ReportingView = "reporting.other" },
		"a different column name":    func(p *catalog.Product) { p.Fields[0].Name = "row_id" },
		"a different column type":    func(p *catalog.Product) { p.Fields[0].Type = "text" },
		"a different collation":      func(p *catalog.Product) { p.Fields[0].Collation = "C" },
		"a different collation version": func(p *catalog.Product) {
			p.Fields[0].CollationVersion = "2.37"
		},
		"an added column": func(p *catalog.Product) {
			p.Fields = append(p.Fields, catalog.Field{Name: "extra", Type: "text"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base
			mutated.Fields = append([]catalog.Field(nil), base.Fields...)
			mutate(&mutated)
			built, err := Build(&catalog.Catalog{Sources: []catalog.Source{source()},
				Products: []catalog.Product{mutated}})
			if err != nil {
				t.Fatal(err)
			}
			if built.Digest == baseline.Digest {
				t.Fatalf("%s did not change the ExpectedSchema digest", name)
			}
		})
	}
}

// Length delimiting: no regrouping of adjacent names may collide.
func TestDigestIsNotVulnerableToNameRegrouping(t *testing.T) {
	left, err := Build(&catalog.Catalog{Sources: []catalog.Source{source()},
		Products: []catalog.Product{{Name: "a", Source: "travel_demo", ReportingView: "reporting.ab",
			Fields: []catalog.Field{{Name: "id", Type: "bigint"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := Build(&catalog.Catalog{Sources: []catalog.Source{source()},
		Products: []catalog.Product{{Name: "a", Source: "travel_demo", ReportingView: "reportinga.b",
			Fields: []catalog.Field{{Name: "id", Type: "bigint"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if left.Digest == right.Digest {
		t.Fatal("regrouping the schema/view boundary produced the same digest")
	}
}

func TestBuildRejectsUnusableCatalogs(t *testing.T) {
	for name, input := range map[string]*catalog.Catalog{
		"nil":         nil,
		"no products": {Sources: []catalog.Source{source()}},
		"unknown source": {Sources: []catalog.Source{source()},
			Products: []catalog.Product{{Name: "a", Source: "other", ReportingView: "reporting.one"}}},
		"invalid reporting view": {Sources: []catalog.Source{source()},
			Products: []catalog.Product{{Name: "a", Source: "travel_demo", ReportingView: "nodot"}}},
		"missing source schema digest": {Sources: []catalog.Source{{Name: "travel_demo"}},
			Products: []catalog.Product{{Name: "a", Source: "travel_demo", ReportingView: "reporting.one"}}},
		"only ViewContract products": {Sources: []catalog.Source{source()},
			Products: []catalog.Product{{Name: "a", Source: "travel_demo", ReportingView: "reporting.one",
				ViewContract: &catalog.ViewContract{}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Build(input); err == nil {
				t.Fatal("an unusable Catalog produced an ExpectedSchema")
			}
		})
	}
}
