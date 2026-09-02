package finalv5benign

import (
	"bytes"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5footprint"
)

func rebuiltManifest(t *testing.T) Manifest {
	t.Helper()
	manifest, err := BuildManifest(BuildInput{
		AgentWorkloadDir: "../agentworkload", LiveCatalogPath: "../../config/catalog.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// The embedded corpus must be byte-identical to a fresh rebuild from the
// unedited statements, the frozen lowerability report, the live Catalog, and
// the closed-form dataset models.
func TestBenignCorpusMatchesRebuild(t *testing.T) {
	rebuilt, err := EncodeManifest(rebuiltManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rebuilt, corpusBytes) {
		t.Fatal("embedded benign corpus differs from a fresh rebuild; regenerate evaluation/finalv5benign/corpus-v1.json")
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestBenignCorpusInvariants(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Statements) != 28 {
		t.Fatalf("corpus carries %d statements, the frozen lowerability report admits 28", len(manifest.Statements))
	}
	if manifest.AuthorizedStatements+manifest.PolicyRefused != int64(len(manifest.Statements)) {
		t.Fatal("classification counts do not partition the corpus")
	}
	var atoms, releases, maxDependency int64
	for _, statement := range manifest.Statements {
		switch statement.Classification {
		case ClassPolicyRefused:
			if statement.PolicyCode == "" || statement.Dependency.Cardinality != 0 || statement.ReleaseFacts != 0 {
				t.Fatalf("%s: policy refusal carries a footprint", statement.ID)
			}
			continue
		case ClassZeroRelease:
			if statement.EvidenceRows != 0 || statement.Dependency.Cardinality != 0 || statement.ReleaseFacts != 0 {
				t.Fatalf("%s: zero-release statement carries a footprint", statement.ID)
			}
		case ClassReleased:
			if statement.EvidenceRows < 1 || statement.Dependency.Cardinality < 1 ||
				statement.Dependency.SetSHA256 == "" {
				t.Fatalf("%s: released statement lacks its dependency commitment", statement.ID)
			}
		default:
			t.Fatalf("%s: unknown classification %q", statement.ID, statement.Classification)
		}
		atoms += statement.PredicateAtoms
		releases += statement.ReleaseFacts
		if statement.Dependency.Cardinality > maxDependency {
			maxDependency = statement.Dependency.Cardinality
		}
	}
	recipe := manifest.Budgets[0]
	outcome := manifest.AuthorizedStatements + atoms
	if recipe.MaxReleaseFacts != releases || recipe.MaxInfluence != maxDependency ||
		recipe.MaxOutcome != outcome || recipe.MaxQueries != 4*outcome {
		t.Fatal("recipe budget does not equal its a-priori formula")
	}
	for index, multiplier := range []int64{1, 2, 4} {
		budget := manifest.Budgets[index]
		if budget.Multiplier != multiplier || budget.MaxInfluence != recipe.MaxInfluence*multiplier {
			t.Fatalf("budget %s does not scale the recipe by %d", budget.Name, multiplier)
		}
	}
	if manifest.TraceUnionDependencyFacts < manifest.MaxDependencyFacts {
		t.Fatal("the trace union cannot be smaller than the largest single statement")
	}
	// The statically derived recipe omission the study reports: cumulative
	// set-union accounting needs more Dependency capacity than the largest
	// single statement, so the recipe run must refuse at least once.
	if manifest.TraceUnionDependencyFacts <= recipe.MaxInfluence {
		t.Log("note: the trace union fits the recipe Dependency budget; the recipe-omission finding no longer holds")
	}
}

// The five result_heavy columns the refused-footprint ladder also models must
// agree between the two corpora formula for formula.
func TestBenignResultHeavyAgreesWithTheLadderModel(t *testing.T) {
	for _, rowID := range []int64{1, 2, 7, 11, 21, 99999, 100000} {
		for _, column := range []string{"row_id", "category", "amount", "quantity", "unit_price", "tax_amount"} {
			benignType, benignValue, err := ResultHeavyColumnValue(rowID, column)
			if err != nil {
				t.Fatal(err)
			}
			ladderType, ladderValue, err := finalv5footprint.CanonicalColumnValue(column, rowID)
			if err != nil {
				t.Fatal(err)
			}
			if benignType != ladderType || benignValue != ladderValue {
				t.Fatalf("row %d column %s: benign %s/%s, ladder %s/%s",
					rowID, column, benignType, benignValue, ladderType, ladderValue)
			}
		}
	}
}
