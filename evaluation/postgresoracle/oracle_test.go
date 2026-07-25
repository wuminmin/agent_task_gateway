package postgresoracle

import "testing"

func TestCampaignRewritesAreUnique(t *testing.T) {
	summary := CoverageSummary()
	if summary.GeneratedAttempts != ExpectedAttempts || summary.UniqueNormalizedPairs != ExpectedAttempts ||
		summary.ExecutedUniquePairs != ExpectedAttempts || summary.DuplicateAttempts != 0 ||
		summary.RewriteTemplates != ExpectedTemplates || len(summary.PairSetSHA256) != 64 ||
		len(summary.PairSignatures) != ExpectedAttempts {
		t.Fatalf("coverage = %+v; want %d unique normalized pairs from %d templates",
			summary, ExpectedAttempts, ExpectedTemplates)
	}
}

func TestIndependentOracleCoversNullAndBagRows(t *testing.T) {
	actual := evaluateFixture(oracleFixture(), scenario{Department: "sales", MinimumAmount: 10, MinimumDate: "2026-01-01"})
	if len(actual) != 3 || !sameRows(actual[0:1], actual[2:3]) {
		t.Fatalf("independent oracle rows = %v", actual)
	}
}
