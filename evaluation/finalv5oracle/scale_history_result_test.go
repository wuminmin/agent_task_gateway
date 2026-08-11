package finalv5oracle

import "testing"

func TestExposureScaleHistoryResultSummaryClosesAllTwelveCells(t *testing.T) {
	cells := ExposureScaleDependencyCells()
	if len(cells) != 12 {
		t.Fatalf("Scale cells = %d, want 12", len(cells))
	}
	seen := make(map[string]bool, len(cells))
	for _, cell := range cells {
		summary, err := ExposureScaleHistoryResultSummary(cell.Scale)
		if err != nil {
			t.Fatalf("history result %s: %v", cell.Scale, err)
		}
		if summary.RowCount != 1 || summary.ColumnCount != 1 ||
			!validSHA256(summary.NormalizedSchemaSHA256) || !validSHA256(summary.CanonicalResultSHA256) {
			t.Fatalf("history result %s is incomplete: %+v", cell.Scale, summary)
		}
		seen[cell.Scale] = true
	}
	if len(seen) != 12 {
		t.Fatalf("history result closure contains %d unique cells, want 12", len(seen))
	}
}

func TestExposureScaleHistoryResultSummaryRejectsUnknownCell(t *testing.T) {
	if _, err := ExposureScaleHistoryResultSummary("10k-overlap-unknown"); err == nil {
		t.Fatal("unknown Scale cell was accepted")
	}
}

func TestProvSQLResultSchemaIsDetachedAndExact(t *testing.T) {
	first := ProvSQLResultSchema()
	second := ProvSQLResultSchema()
	if len(first) != 4 || len(second) != 4 {
		t.Fatalf("ProvSQL result schema lengths = %d/%d, want 4/4", len(first), len(second))
	}
	first[0].Name = "mutated"
	if second[0].Name != "status" || second[1].Name != "price" ||
		second[2].Name != "lines" || second[3].Name != "members" {
		t.Fatalf("ProvSQL result schema is not detached or exact: %+v", second)
	}
}
