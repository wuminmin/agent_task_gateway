package rq5fixture

import "testing"

func TestFrozenRQ5Cycles(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
	want := []Cycle{
		{Index: 1, From: "day3", To: "day0"},
		{Index: 2, From: "day0", To: "day1"},
		{Index: 3, From: "day1", To: "day2"},
		{Index: 4, From: "day2", To: "day3"},
	}
	for index, expected := range want {
		actual, err := LookupCycle(index + 1)
		if err != nil || actual != expected {
			t.Fatalf("cycle %d = %#v, %v; want %#v", index+1, actual, err, expected)
		}
	}
	if FixtureSHA256() != FixtureSHA256() || len(FixtureSHA256()) != 64 {
		t.Fatal("fixture digest is unstable")
	}
}

func TestRQ5CellFailsClosed(t *testing.T) {
	if !IsCell(WorkloadID, Scale, BuildMode, 1) || !IsCell(WorkloadID, Scale, RetainedMode, 4) {
		t.Fatal("frozen cell rejected")
	}
	for _, one := range []struct {
		workload, scale, mode string
		iteration             int
	}{
		{"other", Scale, BuildMode, 1},
		{WorkloadID, "2000", BuildMode, 1},
		{WorkloadID, Scale, "placeholder", 1},
		{WorkloadID, Scale, BuildMode, 0},
		{WorkloadID, Scale, BuildMode, 5},
	} {
		if IsCell(one.workload, one.scale, one.mode, one.iteration) {
			t.Fatalf("unsupported cell accepted: %#v", one)
		}
	}
}
