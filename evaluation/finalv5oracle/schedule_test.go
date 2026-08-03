package finalv5oracle

import (
	"strconv"
	"strings"
	"testing"
)

func TestOutcomeX1ScheduleRealizesExactCellLevelOverlap(t *testing.T) {
	wantTotals := map[int]int64{0: 0, 50: 15, 90: 27, 100: 30}
	for _, percent := range []int{0, 50, 90, 100} {
		schedule, err := BuildOutcomeOverlapSchedule(20260801, 1, percent)
		if err != nil {
			t.Fatalf("x1/o%d: %v", percent, err)
		}
		var total int64
		for _, overlap := range schedule.PerSampleOverlap {
			if overlap != 0 && overlap != 1 {
				t.Fatalf("x1/o%d has impossible per-sample overlap %d", percent, overlap)
			}
			total += overlap
		}
		if len(schedule.PerSampleOverlap) != 30 || total != wantTotals[percent] ||
			schedule.TotalOverlapMemberships != wantTotals[percent] || schedule.TotalCandidateMemberships != 30 {
			t.Fatalf("x1/o%d schedule = %+v", percent, schedule)
		}
		if err := ValidateOutcomeOverlapSchedule(schedule); err != nil {
			t.Fatal(err)
		}
	}
	schedule, _ := BuildOutcomeOverlapSchedule(20260801, 1, 50)
	if bits := overlapBits(schedule.PerSampleOverlap); bits != "100001011100001010101011111010" ||
		schedule.ScheduleSHA256 != "661292e37f907888aff851f2f32e592a04528c689815674b2989870a5c623a22" {
		t.Fatalf("x1/o50 fixed schedule bits=%s sha=%s", bits, schedule.ScheduleSHA256)
	}
}

func TestOutcomeX100AndX10KAreExactInsideEverySample(t *testing.T) {
	for _, candidate := range []int64{100, 10_000} {
		for _, percent := range []int{0, 50, 90, 100} {
			schedule, err := BuildOutcomeOverlapSchedule(77, candidate, percent)
			if err != nil {
				t.Fatal(err)
			}
			want := candidate * int64(percent) / 100
			for sample, overlap := range schedule.PerSampleOverlap {
				if overlap != want {
					t.Fatalf("x%d/o%d sample %d = %d, want %d", candidate, percent, sample+1, overlap, want)
				}
			}
			if schedule.TotalOverlapMemberships != int64(OutcomeMeasuredSamples)*want {
				t.Fatalf("x%d/o%d total = %d", candidate, percent, schedule.TotalOverlapMemberships)
			}
		}
	}
}

func TestOutcomeScheduleExcludesWarmupsAndDetectsMutation(t *testing.T) {
	schedule, err := BuildOutcomeOverlapSchedule(1234, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 1; iteration <= OutcomeWarmupSamples; iteration++ {
		overlap, included, err := schedule.OverlapForRun(iteration, true)
		if err != nil || included || overlap != 0 {
			t.Fatalf("warmup %d consumed schedule: overlap=%d included=%v err=%v", iteration, overlap, included, err)
		}
	}
	var measured int64
	for iteration := 1; iteration <= OutcomeMeasuredSamples; iteration++ {
		overlap, included, err := schedule.OverlapForRun(iteration, false)
		if err != nil || !included {
			t.Fatalf("measured %d was excluded: %v", iteration, err)
		}
		measured += overlap
	}
	if measured != 15 {
		t.Fatalf("measured total = %d, want 15", measured)
	}
	mutated := schedule
	mutated.PerSampleOverlap = append([]int64(nil), schedule.PerSampleOverlap...)
	for index := range mutated.PerSampleOverlap {
		if mutated.PerSampleOverlap[index] == 0 {
			mutated.PerSampleOverlap[index] = 1
			break
		}
	}
	if err := ValidateOutcomeOverlapSchedule(mutated); err == nil {
		t.Fatal("changed per-sample overlap with a stale schedule digest was accepted")
	}
	changedSeed, err := BuildOutcomeOverlapSchedule(1235, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if changedSeed.ScheduleSHA256 == schedule.ScheduleSHA256 || overlapBits(changedSeed.PerSampleOverlap) == overlapBits(schedule.PerSampleOverlap) {
		t.Fatal("seed mutation reused the x1 assignment")
	}
}

func TestOutcomeScheduleRejectsNonContractInputsAndRunIndices(t *testing.T) {
	for _, input := range []struct {
		candidate int64
		percent   int
	}{{2, 50}, {1, 25}, {0, 0}, {10_000, 101}} {
		if _, err := BuildOutcomeOverlapSchedule(1, input.candidate, input.percent); err == nil {
			t.Fatalf("accepted candidate=%d percent=%d", input.candidate, input.percent)
		}
	}
	schedule, _ := BuildOutcomeOverlapSchedule(1, 1, 0)
	if _, _, err := schedule.OverlapForRun(0, false); err == nil {
		t.Fatal("measured iteration zero was accepted")
	}
	if _, _, err := schedule.OverlapForRun(6, true); err == nil {
		t.Fatal("sixth warmup was accepted")
	}
}

func overlapBits(values []int64) string {
	var result strings.Builder
	for _, value := range values {
		result.WriteString(strconv.FormatInt(value, 10))
	}
	return result.String()
}
