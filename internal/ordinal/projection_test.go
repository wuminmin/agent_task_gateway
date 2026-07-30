package ordinal

import "testing"

func TestProjectedSnapshotIndexMatchesDefensiveRowLookup(t *testing.T) {
	spec := SnapshotSpec{
		SourceID:        "projection_source",
		SourceNamespace: "projection_namespace",
		Snapshot:        "projection_snapshot",
		SchemaDigest:    testDigest("a"),
		Fields: []SnapshotField{
			{Name: "id", SQLType: "bigint"},
			{Name: "amount", SQLType: "bigint"},
		},
		Rows: []SnapshotRow{
			{EntityKey: "projection-row-1", Values: map[string]any{"id": int64(1), "amount": int64(20)}},
			{EntityKey: "projection-row-2", Values: map[string]any{"id": int64(2), "amount": int64(30)}},
		},
	}
	dictionary, err := CompileSnapshot(spec)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := dictionary.Split()
	if err != nil {
		t.Fatal(err)
	}

	indexes := []SnapshotIndex{dictionary, artifact.Hot}
	for _, index := range indexes {
		projected, ok := index.(ProjectedSnapshotIndex)
		if !ok {
			t.Fatalf("%T does not implement ProjectedSnapshotIndex", index)
		}
		for handle := RowHandle(1); uint64(handle) <= index.RowCount(); handle++ {
			row, found := index.LookupRow(handle)
			if !found {
				t.Fatalf("%T row %d missing", index, handle)
			}
			entityKey, rowRef, found := projected.LookupRowIdentity(handle)
			if !found || entityKey != row.EntityKey || rowRef != row.Row {
				t.Fatalf("%T projected identity differs for row %d", index, handle)
			}
			for fieldID, want := range row.Cells {
				got, found := projected.LookupCellRef(handle, fieldID)
				if !found || got != want {
					t.Fatalf("%T projected %s differs for row %d", index, fieldID, handle)
				}
			}
		}
		if _, _, found := projected.LookupRowIdentity(0); found {
			t.Fatalf("%T accepted zero row handle", index)
		}
		if _, found := projected.LookupCellRef(1, "missing"); found {
			t.Fatalf("%T accepted an unknown field", index)
		}
	}

	handle, found := artifact.Hot.LookupRowHandle("projection-row-1")
	if !found {
		t.Fatal("HOT test row is absent")
	}
	if allocations := testing.AllocsPerRun(1000, func() {
		_, _, _ = artifact.Hot.LookupRowIdentity(handle)
		_, _ = artifact.Hot.LookupCellRef(handle, "amount")
	}); allocations != 0 {
		t.Fatalf("projected HOT lookup allocations = %v, want 0", allocations)
	}
}
