package finalv5oracle

import (
	"reflect"
	"testing"
)

func TestArtifactX4IsStableSubsetOfX16(t *testing.T) {
	four, err := ArtifactSchema(4)
	if err != nil {
		t.Fatal(err)
	}
	sixteen, err := ArtifactSchema(16)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(four, sixteen[:4]) {
		t.Fatalf("x4 schema = %+v, x16 prefix = %+v", four, sixteen[:4])
	}
	for _, index := range []int64{0, 1, 99, 9_999, 99_999} {
		x4, err := ArtifactRow(index, 4)
		if err != nil {
			t.Fatal(err)
		}
		x16, err := ArtifactRow(index, 16)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(x4, x16[:4]) {
			t.Fatalf("row %d x4 = %#v, x16 prefix = %#v", index, x4, x16[:4])
		}
	}
}

func TestArtifactFormalShapesAnd100Kx16Golden(t *testing.T) {
	shapes := []struct {
		rows    int64
		columns int
	}{
		{100, 4}, {10_000, 4}, {100_000, 4},
		{100, 16}, {10_000, 16}, {100_000, 16},
	}
	var largest ResultSummary
	for _, shape := range shapes {
		summary, err := ArtifactResultSummary(shape.rows, shape.columns)
		if err != nil {
			t.Fatalf("%dx%d: %v", shape.rows, shape.columns, err)
		}
		if summary.RowCount != shape.rows || summary.ColumnCount != shape.columns {
			t.Fatalf("%dx%d summary = %+v", shape.rows, shape.columns, summary)
		}
		if shape.rows == 100_000 && shape.columns == 16 {
			largest = summary
		}
	}
	const wantSchema = "394378d8ac2bbf7f296de5b3655fe04bd7fe70a6dcd18e44c32e2a5ee4a9c3d6"
	const wantResult = "d2831ad661272b677f38a2d743da3b988e9a0fe8fbd0c9cf39adedf72e79441c"
	if largest.NormalizedSchemaSHA256 != wantSchema || largest.CanonicalResultSHA256 != wantResult {
		t.Fatalf("100k x16 summary = %+v", largest)
	}
	repeated, err := ArtifactResultSummary(100_000, 16)
	if err != nil || repeated != largest {
		t.Fatalf("repeated 100k x16 summary = %+v, err=%v", repeated, err)
	}
}

func TestArtifactGeneratorRejectsNonFormalShapes(t *testing.T) {
	for _, shape := range []struct {
		rows    int64
		columns int
	}{{99, 4}, {1_000, 4}, {100, 8}, {100_001, 16}} {
		if _, err := ArtifactResultSummary(shape.rows, shape.columns); err == nil {
			t.Fatalf("non-formal shape %dx%d was accepted", shape.rows, shape.columns)
		}
	}
	if _, err := ArtifactRow(-1, 4); err == nil {
		t.Fatal("negative artifact row was accepted")
	}
	if _, err := ArtifactRow(100_000, 16); err == nil {
		t.Fatal("artifact row above formal maximum was accepted")
	}
}
