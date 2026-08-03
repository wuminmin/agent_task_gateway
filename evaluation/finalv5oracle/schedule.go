package finalv5oracle

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

const (
	OutcomeOverlapScheduleVersion = "taskgate-final-v5-outcome-overlap-schedule-v1"
	OutcomeMeasuredSamples        = 30
	OutcomeWarmupSamples          = 5
	outcomeScheduleDomain         = "TASKGATE-FINAL-V5-OUTCOME-SCHEDULE-V1\x00"
)

// OutcomeOverlapSchedule realizes overlap over measured memberships, not by
// rounding each sample independently. Warmups are deliberately absent.
type OutcomeOverlapSchedule struct {
	Version                   string  `json:"version"`
	Seed                      int64   `json:"seed"`
	CandidateCardinality      int64   `json:"candidate_cardinality"`
	TargetPercent             int     `json:"target_percent"`
	MeasuredSamples           int     `json:"measured_samples"`
	PerSampleOverlap          []int64 `json:"per_sample_overlap"`
	TotalCandidateMemberships int64   `json:"total_candidate_memberships"`
	TotalOverlapMemberships   int64   `json:"total_overlap_memberships"`
	ScheduleSHA256            string  `json:"schedule_sha256"`
}

// BuildOutcomeOverlapSchedule builds the frozen 30-sample schedule. For x1,
// target totals are exactly 0/15/27/30. For x100 and x10k, every sample has an
// exact 0/50/90/100-percent overlap cardinality.
func BuildOutcomeOverlapSchedule(seed, candidateCardinality int64, targetPercent int) (OutcomeOverlapSchedule, error) {
	if candidateCardinality != 1 && candidateCardinality != 100 && candidateCardinality != 10_000 {
		return OutcomeOverlapSchedule{}, errors.New("outcome candidate cardinality must be one of 1, 100, or 10,000")
	}
	switch targetPercent {
	case 0, 50, 90, 100:
	default:
		return OutcomeOverlapSchedule{}, errors.New("outcome overlap percent must be one of 0, 50, 90, or 100")
	}
	totalCandidates := int64(OutcomeMeasuredSamples) * candidateCardinality
	totalOverlapNumerator := totalCandidates * int64(targetPercent)
	if totalOverlapNumerator%100 != 0 {
		return OutcomeOverlapSchedule{}, errors.New("outcome cell cannot realize its percentage exactly")
	}
	totalOverlap := totalOverlapNumerator / 100
	base := totalOverlap / int64(OutcomeMeasuredSamples)
	remainder := int(totalOverlap % int64(OutcomeMeasuredSamples))
	perSample := make([]int64, OutcomeMeasuredSamples)
	for index := range perSample {
		perSample[index] = base
	}
	if remainder > 0 {
		ranks := make([]outcomeSampleRank, OutcomeMeasuredSamples)
		for index := range ranks {
			ranks[index] = outcomeSampleRank{sample: index, score: outcomeScheduleScore(seed, candidateCardinality, targetPercent, index+1)}
		}
		sort.Slice(ranks, func(i, j int) bool {
			comparison := bytes.Compare(ranks[i].score[:], ranks[j].score[:])
			if comparison != 0 {
				return comparison < 0
			}
			return ranks[i].sample < ranks[j].sample
		})
		for index := 0; index < remainder; index++ {
			perSample[ranks[index].sample]++
		}
	}
	for _, overlap := range perSample {
		if overlap < 0 || overlap > candidateCardinality {
			return OutcomeOverlapSchedule{}, errors.New("outcome overlap schedule exceeds a sample's candidate set")
		}
	}
	schedule := OutcomeOverlapSchedule{
		Version: OutcomeOverlapScheduleVersion, Seed: seed, CandidateCardinality: candidateCardinality,
		TargetPercent: targetPercent, MeasuredSamples: OutcomeMeasuredSamples,
		PerSampleOverlap: perSample, TotalCandidateMemberships: totalCandidates,
		TotalOverlapMemberships: totalOverlap,
	}
	schedule.ScheduleSHA256 = hashOutcomeOverlapSchedule(schedule)
	return schedule, nil
}

// ValidateOutcomeOverlapSchedule detects changed overlap, order, totals, or
// seed binding by rebuilding the deterministic schedule.
func ValidateOutcomeOverlapSchedule(schedule OutcomeOverlapSchedule) error {
	if schedule.Version != OutcomeOverlapScheduleVersion || schedule.MeasuredSamples != OutcomeMeasuredSamples ||
		len(schedule.PerSampleOverlap) != OutcomeMeasuredSamples || !validSHA256(schedule.ScheduleSHA256) {
		return errors.New("outcome overlap schedule metadata is invalid")
	}
	expected, err := BuildOutcomeOverlapSchedule(schedule.Seed, schedule.CandidateCardinality, schedule.TargetPercent)
	if err != nil {
		return err
	}
	if expected.TotalCandidateMemberships != schedule.TotalCandidateMemberships ||
		expected.TotalOverlapMemberships != schedule.TotalOverlapMemberships ||
		expected.ScheduleSHA256 != schedule.ScheduleSHA256 {
		return errors.New("outcome overlap schedule totals or digest were mutated")
	}
	for index := range expected.PerSampleOverlap {
		if expected.PerSampleOverlap[index] != schedule.PerSampleOverlap[index] {
			return fmt.Errorf("outcome overlap schedule sample %d was mutated", index+1)
		}
	}
	return nil
}

// OverlapForRun returns included=false for warmups, making it impossible to
// accidentally consume a measured schedule entry during one of the five
// unmeasured warmup iterations. Iterations are one based.
func (schedule OutcomeOverlapSchedule) OverlapForRun(iteration int, warmup bool) (overlap int64, included bool, err error) {
	if warmup {
		if iteration < 1 || iteration > OutcomeWarmupSamples {
			return 0, false, errors.New("outcome warmup iteration is outside 1..5")
		}
		return 0, false, nil
	}
	if iteration < 1 || iteration > len(schedule.PerSampleOverlap) {
		return 0, false, errors.New("outcome measured iteration is outside 1..30")
	}
	return schedule.PerSampleOverlap[iteration-1], true, nil
}

func hashOutcomeOverlapSchedule(schedule OutcomeOverlapSchedule) string {
	h := sha256.New()
	_, _ = h.Write([]byte(outcomeScheduleDomain))
	scheduleWriteString(h, schedule.Version)
	scheduleWriteUint64(h, uint64(schedule.Seed))
	scheduleWriteUint64(h, uint64(schedule.CandidateCardinality))
	scheduleWriteUint64(h, uint64(schedule.TargetPercent))
	scheduleWriteUint64(h, uint64(schedule.MeasuredSamples))
	scheduleWriteUint64(h, uint64(len(schedule.PerSampleOverlap)))
	for _, overlap := range schedule.PerSampleOverlap {
		scheduleWriteUint64(h, uint64(overlap))
	}
	scheduleWriteUint64(h, uint64(schedule.TotalCandidateMemberships))
	scheduleWriteUint64(h, uint64(schedule.TotalOverlapMemberships))
	return hex.EncodeToString(h.Sum(nil))
}

func outcomeScheduleScore(seed, candidateCardinality int64, targetPercent, sample int) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("TASKGATE-FINAL-V5-OUTCOME-SCHEDULE-RANK-V1\x00"))
	scheduleWriteUint64(h, uint64(seed))
	scheduleWriteUint64(h, uint64(candidateCardinality))
	scheduleWriteUint64(h, uint64(targetPercent))
	scheduleWriteUint64(h, uint64(sample))
	var result [sha256.Size]byte
	copy(result[:], h.Sum(nil))
	return result
}

type outcomeSampleRank struct {
	sample int
	score  [sha256.Size]byte
}

func scheduleWriteString(target interface{ Write([]byte) (int, error) }, value string) {
	scheduleWriteUint64(target, uint64(len(value)))
	_, _ = target.Write([]byte(value))
}

func scheduleWriteUint64(target interface{ Write([]byte) (int, error) }, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = target.Write(encoded[:])
}
