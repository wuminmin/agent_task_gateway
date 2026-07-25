package postgresoracle

import "testing"

func TestCampaignRewritesAreUnique(t *testing.T) {
	summary := CoverageSummary()
	if summary.GeneratedAttempts != ExpectedAttempts || summary.UniqueRewrites != ExpectedAttempts ||
		summary.RewriteTemplates != ExpectedTemplates {
		t.Fatalf("coverage = attempts %d, unique %d, templates %d; want %d/%d/%d",
			summary.GeneratedAttempts, summary.UniqueRewrites, summary.RewriteTemplates,
			ExpectedAttempts, ExpectedAttempts, ExpectedTemplates)
	}
}

func TestIndependentOracleCoversNullAndBagRows(t *testing.T) {
	actual := evaluateFixture(oracleFixture(), scenario{Department: "sales", MinimumAmount: 10, MinimumDate: "2026-01-01"})
	if len(actual) != 3 || !sameRows(actual[0:1], actual[2:3]) {
		t.Fatalf("independent oracle rows = %v", actual)
	}
}
