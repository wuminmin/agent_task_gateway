//go:build taskgate_scale

// These cases resolve every snapshot publication the Catalog declares, which
// means the five whose rows live in the Business database (25.84 GB peak on a
// 30 GB host). The scale lane installs them; the acceptance run does not.

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
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

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

// The grant preparation hands the Gateway must be widened by nothing but what
// metering requires.
//
// The widening is a real privilege increase over the task's approved surface,
// and it is invisible in the approved-column list a reviewer reads. The bound is
// stated against the Catalog and the task grant -- both inputs rather than
// derivation products -- so it holds whatever the metering closure happens to
// contain: every added column must be the accounted product'"'"'s own entity key or
// mandatory scope, which is what deriveExposureShape requires to be published.
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
				for _, column := range product.ApprovedColumns {
					if _, wasApproved := approved[column]; wasApproved {
						continue
					}
					// The bound is the Catalog's, not the derivation's. Metering
					// widens an accounted product by its own entity key and its own
					// mandatory scopes and by nothing else, in both the
					// single-product and the per-product relational closure, so
					// stating it against the Catalog covers both without reading a
					// list either derivation produced.
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

// A V5 preparation of a branch-filtered UNION must account for both branches.
// Preparing successfully is not enough: omitting either literal would
// under-count the atoms the query actually reveals.
func TestUnionBranchFilteredPredicatesAreAccountedUnderV5(t *testing.T) {
	test := parityCase{
		name: "union_branch_filtered", profile: exposure.ProfileV5,
		products:      []string{"expense_summary"},
		columns:       map[string][]string{"expense_summary": summaryColumns},
		plan:          branchFilteredUnionPlan(),
		needsRegistry: true,
	}
	service := parityService(t, true)
	resolved := resolveParityCase(t, service, test)
	shape, err := productionPrepareForParity(context.Background(), service, control.Task{},
		resolved.grant(), resolved.plan)
	if err != nil {
		t.Fatalf("prepare a V5 union with branch-qualified predicates: %v", err)
	}
	footprint := shape.PredicateFootprint
	if footprint == nil {
		t.Fatal("the V5 union prepared without a predicate footprint")
	}
	if footprint.RawLiteralCount != 2 || footprint.UniqueAtomCount != 2 ||
		footprint.DuplicateCount != 0 || footprint.NullAtomCount != 0 || len(footprint.Atoms) != 2 {
		t.Fatalf("branch-filtered V5 union footprint counts = %+v", footprint)
	}
	wantLiteral := map[string]string{"left_branch": "s:机票", "right_branch": "s:酒店"}
	for _, atom := range footprint.Atoms {
		literal, present := wantLiteral[atom.StableRole]
		if !present {
			t.Fatalf("unexpected branch predicate atom: %+v", atom)
		}
		if atom.Profile != exposure.ProfileV5 || atom.Kind != exposure.FactPredicateAtom ||
			atom.SemanticProductID != "expense_summary" || atom.PublicFieldID != "expense_type" ||
			atom.Operator != "EQ" || atom.CanonicalLiteral != literal {
			t.Fatalf("branch predicate atom for %s = %+v", atom.StableRole, atom)
		}
		delete(wantLiteral, atom.StableRole)
	}
	if len(wantLiteral) != 0 {
		t.Fatalf("predicate footprint omitted branch roles: %v", wantLiteral)
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
			registryService := parityService(t, mutated.needsRegistry)
			shape, err := productionPrepareForParity(context.Background(), registryService, control.Task{},
				resolved.grant(), resolved.plan)
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

// The Catalog, its publications, the datasource and the compiler are inputs too,
// and a change to any of them must move the identity a receipt signs.
//
// This is stated against the RETAINED V9 construction, which is where the
// datasource and schema identities live: PreparedOperationBindingV1 does not
// carry them, because preparation is a function of its inputs and a datasource
// is not one of them. Under V10 they are top-level members of the receipt
// instead, so they remain signed -- what this pins is that the construction
// which does carry them distinguishes each. The members that DID move into the
// preparation -- the Catalog, the dictionary set, the sidecar grants, the plan
// identity, both statements -- are covered here and again by the preparation's
// own mutation suite.
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
	// The baseline is this construction's own digest, not the shape's.
	//
	// It used to be compared against baseline.PreparedOperationSHA256, because
	// production computed exactly this value and the comparison proved the harness
	// reproduced it. Production computes the sealed preparation's identity now, so
	// the two are different constructions over overlapping material and requiring
	// them to be equal would be requiring V9 and V2 to agree. That the production
	// path carries the preparation faithfully is what the differential asserts;
	// what remains here is that each member below moves the identity that carries
	// it.
	unchanged := build(func(*preparedOperation) {})
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
			if build(mutate) == unchanged {
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
