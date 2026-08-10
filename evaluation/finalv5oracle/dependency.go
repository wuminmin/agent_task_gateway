package finalv5oracle

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
)

const (
	ExposureScaleDependencyGeneratorVersion = "taskgate-final-v5-exposure-scale-dependency-v1"
	ExposureScaleProductID                  = "final_v5_exposure_scale"
	ExposureScaleSourceNamespace            = "final_v5.exposure_scale"
	ExposureScaleSnapshot                   = "final-v5-exposure-scale-2026-v1"
	ExposureScaleStableRole                 = "final_v5_exposure_scale"
	ExposureScaleFactsPerRow                = int64(5)
	ExposureScaleMaximumCandidateFacts      = int64(1_035_000)
	ExposureScaleMaximumDatasetFacts        = int64(2_070_000) // 414,000 rows, sufficient for N/N at o0.

	DependencyScale10K     = int64(10_000)
	DependencyScale100K    = int64(100_000)
	DependencyScale1035000 = int64(1_035_000)
)

type ExposureScaleDependencyRequest struct {
	CandidateFacts int64
	ExistingFacts  int64
	OverlapFacts   int64
	SetOptions     StreamSetOptions
}

// ExposureScaleOverlapFacts translates the four preregistered overlap labels
// into an exact, row-aligned Fact cardinality for a formal N-fact scale.
func ExposureScaleOverlapFacts(candidateFacts int64, percent int) (int64, error) {
	if !isFormalDependencyScale(candidateFacts) {
		return 0, errors.New("dependency overlap requires a formal candidate scale")
	}
	switch percent {
	case 0, 50, 90, 100:
	default:
		return 0, errors.New("dependency overlap percent must be one of 0, 50, 90, or 100")
	}
	overlap := candidateFacts * int64(percent) / 100
	if overlap%ExposureScaleFactsPerRow != 0 {
		return 0, errors.New("dependency overlap is not row aligned")
	}
	return overlap, nil
}

type DependencyGenerationStats struct {
	FactEmissions       int64 `json:"fact_emissions"`
	PeakBufferedMembers int   `json:"peak_buffered_members"`
	SpillRuns           int   `json:"spill_runs"`
	PeakMergeHeads      int   `json:"peak_merge_heads"`
}

// DependencyOracleReport is the complete ordinary-set algebra for the real,
// source-controlled final_v5_exposure_scale Product. Candidate members use
// ranks (0,M]; history uses (M-K,2M-K]. Thus both contain N=5M facts, their
// exact overlap is K rows (5K facts), and no physical database order matters.
type DependencyOracleReport struct {
	GeneratorVersion           string                    `json:"generator_version"`
	ProductID                  string                    `json:"product_id"`
	SourceNamespace            string                    `json:"source_namespace"`
	Snapshot                   string                    `json:"snapshot"`
	CandidateRows              int64                     `json:"candidate_rows"`
	ExistingRows               int64                     `json:"existing_rows"`
	FormalScale                bool                      `json:"formal_scale"`
	Candidate                  StreamSetSummary          `json:"candidate"`
	CandidateWitnessCommitment string                    `json:"candidate_witness_commitment"`
	Existing                   StreamSetSummary          `json:"existing"`
	Overlap                    StreamSetSummary          `json:"overlap"`
	Novel                      StreamSetSummary          `json:"novel"`
	Union                      StreamSetSummary          `json:"union"`
	Stats                      DependencyGenerationStats `json:"stats"`
}

// GenerateExposureScaleDependency derives facts from the source-controlled
// Dataset Spec rather than from a BDG response. Every retained row contributes
// exactly five facts:
//
//	row existence + member_rank + metric + family_id + partition_key.
//
// The four cells are all query evidence: member_rank and metric are filter
// fields, while family_id and partition_key are fixed predicate/scope fields.
func GenerateExposureScaleDependency(request ExposureScaleDependencyRequest) (DependencyOracleReport, error) {
	if request.CandidateFacts <= 0 || request.CandidateFacts > ExposureScaleMaximumCandidateFacts ||
		request.CandidateFacts%ExposureScaleFactsPerRow != 0 {
		return DependencyOracleReport{}, fmt.Errorf("candidate facts must be a positive multiple of %d no greater than %d",
			ExposureScaleFactsPerRow, ExposureScaleMaximumCandidateFacts)
	}
	if request.ExistingFacts != request.CandidateFacts {
		return DependencyOracleReport{}, errors.New("formal dependency root and candidate must have the same N-fact scale")
	}
	if request.OverlapFacts < 0 || request.OverlapFacts > request.CandidateFacts ||
		request.OverlapFacts%ExposureScaleFactsPerRow != 0 {
		return DependencyOracleReport{}, errors.New("dependency overlap must be a row-aligned subset of root and candidate")
	}

	n, k := request.CandidateFacts, request.OverlapFacts
	report := DependencyOracleReport{
		GeneratorVersion: ExposureScaleDependencyGeneratorVersion,
		ProductID:        ExposureScaleProductID,
		SourceNamespace:  ExposureScaleSourceNamespace,
		Snapshot:         ExposureScaleSnapshot,
		CandidateRows:    n / ExposureScaleFactsPerRow,
		ExistingRows:     request.ExistingFacts / ExposureScaleFactsPerRow,
		FormalScale:      isFormalDependencyScale(n),
	}
	emissions := int64(0)
	summarize := func(roles []string, first, last int64, withCandidateWitness bool) (map[string]StreamSetSummary, error) {
		emissions += last - first
		stream := func(yield func(string) error) error {
			return StreamExposureScaleFacts(first, last, func(fact CanonicalFact) error { return yield(fact.SHA256) })
		}
		var summaries map[string]StreamSetSummary
		var err error
		if withCandidateWitness {
			summaries, report.CandidateWitnessCommitment, err = SummarizeUnitWitnessSemanticSetRoles(roles, stream, request.SetOptions)
		} else {
			summaries, err = SummarizeSemanticSetRoles(roles, stream, request.SetOptions)
		}
		if err == nil {
			stats := summaries[roles[0]].Stats
			if stats.PeakBufferedMembers > report.Stats.PeakBufferedMembers {
				report.Stats.PeakBufferedMembers = stats.PeakBufferedMembers
			}
			if stats.PeakMergeHeads > report.Stats.PeakMergeHeads {
				report.Stats.PeakMergeHeads = stats.PeakMergeHeads
			}
			report.Stats.SpillRuns += stats.SpillRuns
		}
		return summaries, err
	}
	empty := func(roles []string) (map[string]StreamSetSummary, error) {
		return SummarizeSemanticSetRoles(roles, func(func(string) error) error { return nil }, request.SetOptions)
	}

	switch k {
	case 0:
		candidate, err := summarize([]string{"candidate", "novel"}, 0, n, true)
		if err != nil {
			return DependencyOracleReport{}, fmt.Errorf("summarize candidate dependency set: %w", err)
		}
		existing, err := summarize([]string{"existing"}, n, 2*n, false)
		if err != nil {
			return DependencyOracleReport{}, fmt.Errorf("summarize existing dependency set: %w", err)
		}
		union, err := summarize([]string{"union"}, 0, 2*n, false)
		if err != nil {
			return DependencyOracleReport{}, fmt.Errorf("summarize union dependency set: %w", err)
		}
		zero, err := empty([]string{"overlap"})
		if err != nil {
			return DependencyOracleReport{}, err
		}
		report.Candidate, report.Novel = candidate["candidate"], candidate["novel"]
		report.Existing, report.Overlap, report.Union = existing["existing"], zero["overlap"], union["union"]
	case n:
		full, err := summarize([]string{"candidate", "existing", "overlap", "union"}, 0, n, true)
		if err != nil {
			return DependencyOracleReport{}, fmt.Errorf("summarize replay dependency set: %w", err)
		}
		zero, err := empty([]string{"novel"})
		if err != nil {
			return DependencyOracleReport{}, err
		}
		report.Candidate, report.Existing, report.Overlap, report.Union =
			full["candidate"], full["existing"], full["overlap"], full["union"]
		report.Novel = zero["novel"]
	default:
		candidate, err := summarize([]string{"candidate"}, 0, n, true)
		if err != nil {
			return DependencyOracleReport{}, fmt.Errorf("summarize candidate dependency set: %w", err)
		}
		existing, err := summarize([]string{"existing"}, n-k, 2*n-k, false)
		if err != nil {
			return DependencyOracleReport{}, fmt.Errorf("summarize existing dependency set: %w", err)
		}
		overlap, err := summarize([]string{"overlap"}, n-k, n, false)
		if err != nil {
			return DependencyOracleReport{}, fmt.Errorf("summarize overlap dependency set: %w", err)
		}
		novel, err := summarize([]string{"novel"}, 0, n-k, false)
		if err != nil {
			return DependencyOracleReport{}, fmt.Errorf("summarize novel dependency set: %w", err)
		}
		union, err := summarize([]string{"union"}, 0, 2*n-k, false)
		if err != nil {
			return DependencyOracleReport{}, fmt.Errorf("summarize union dependency set: %w", err)
		}
		report.Candidate, report.Existing, report.Overlap, report.Novel, report.Union =
			candidate["candidate"], existing["existing"], overlap["overlap"], novel["novel"], union["union"]
	}

	if report.Candidate.Cardinality != n || report.Existing.Cardinality != n ||
		report.Overlap.Cardinality != k || report.Novel.Cardinality != n-k || report.Union.Cardinality != 2*n-k {
		return DependencyOracleReport{}, errors.New("exposure-scale dependency generator violated exact set algebra")
	}
	if !validSHA256(report.CandidateWitnessCommitment) {
		return DependencyOracleReport{}, errors.New("exposure-scale dependency generator omitted the candidate witness commitment")
	}
	report.Stats.FactEmissions = emissions
	return report, nil
}

// StreamExposureScaleFacts emits a half-open fact-index range in deterministic
// member_rank order. The range is row aligned; set canonicalization itself is
// order independent and is performed by the bounded external sorter.
func StreamExposureScaleFacts(firstFact, lastFact int64, yield func(CanonicalFact) error) error {
	if yield == nil || firstFact < 0 || lastFact < firstFact || lastFact > ExposureScaleMaximumDatasetFacts ||
		firstFact%ExposureScaleFactsPerRow != 0 || lastFact%ExposureScaleFactsPerRow != 0 {
		return errors.New("exposure-scale dependency range is invalid or not row aligned")
	}
	for rowIndex := firstFact / ExposureScaleFactsPerRow; rowIndex < lastFact/ExposureScaleFactsPerRow; rowIndex++ {
		facts, err := buildExposureScaleRowFacts(rowIndex)
		if err != nil {
			return err
		}
		for _, fact := range facts {
			if err := yield(fact); err != nil {
				return err
			}
		}
	}
	return nil
}

// ExposureScaleFactAt exposes one deterministic member for audit and mutation
// tests without materializing a formal-scale set.
func ExposureScaleFactAt(factIndex int64) (CanonicalFact, error) {
	if factIndex < 0 || factIndex >= ExposureScaleMaximumDatasetFacts {
		return CanonicalFact{}, errors.New("exposure-scale fact index is outside the frozen dataset")
	}
	facts, err := buildExposureScaleRowFacts(factIndex / ExposureScaleFactsPerRow)
	if err != nil {
		return CanonicalFact{}, err
	}
	return facts[factIndex%ExposureScaleFactsPerRow], nil
}

func buildExposureScaleRowFacts(rowIndex int64) ([5]CanonicalFact, error) {
	values, err := exposureScaleDatasetRow(rowIndex)
	if err != nil {
		return [5]CanonicalFact{}, err
	}
	rankValue, err := exposureScaleFactCanonicalValue(SQLBigInt, values[0])
	if err != nil {
		return [5]CanonicalFact{}, err
	}
	metricValue, err := exposureScaleFactCanonicalValue(SQLNumeric, values[1])
	if err != nil {
		return [5]CanonicalFact{}, err
	}
	familyValue, err := exposureScaleFactCanonicalValue(SQLInteger, values[2])
	if err != nil {
		return [5]CanonicalFact{}, err
	}
	partitionValue, err := exposureScaleFactCanonicalValue(SQLInteger, values[3])
	if err != nil {
		return [5]CanonicalFact{}, err
	}
	entityKey, err := ComposeOracleCanonicalKeyV2("base-entity",
		ExposureScaleSourceNamespace, "member_rank", "bigint", rankValue)
	if err != nil {
		return [5]CanonicalFact{}, err
	}
	row, err := BuildV2BaseRowFact(V2BaseRowInput{SourceNamespace: ExposureScaleSourceNamespace,
		Snapshot: ExposureScaleSnapshot, EntityKey: entityKey})
	if err != nil {
		return [5]CanonicalFact{}, err
	}
	cell := func(field, sqlType, value string) (CanonicalFact, error) {
		return BuildV2BaseCellFact(V2BaseCellInput{SourceNamespace: ExposureScaleSourceNamespace,
			Snapshot: ExposureScaleSnapshot, EntityKey: entityKey,
			// The fixed Scale templates lower through the single-Product plan,
			// whose public base-cell identity is the unqualified field ID.
			Field: field, SQLType: sqlType, CanonicalValue: value})
	}
	rank, err := cell("member_rank", "bigint", rankValue)
	if err != nil {
		return [5]CanonicalFact{}, err
	}
	metric, err := cell("metric", "numeric", metricValue)
	if err != nil {
		return [5]CanonicalFact{}, err
	}
	family, err := cell("family_id", "integer", familyValue)
	if err != nil {
		return [5]CanonicalFact{}, err
	}
	partition, err := cell("partition_key", "integer", partitionValue)
	if err != nil {
		return [5]CanonicalFact{}, err
	}
	return [5]CanonicalFact{row, rank, metric, family, partition}, nil
}

func exposureScaleFactCanonicalValue(sqlType SQLType, raw any) (string, error) {
	typed, err := NormalizeTypedValue(sqlType, raw)
	if err != nil {
		return "", err
	}
	if typed.IsNull() {
		return "", errors.New("exposure-scale frozen Dataset row contains NULL")
	}
	switch sqlType {
	case SQLBigInt:
		value, err := canonicalSignedInteger(raw, 64)
		if err != nil {
			return "", err
		}
		return "i:" + strconv.FormatInt(value, 10), nil
	case SQLInteger:
		value, err := canonicalSignedInteger(raw, 32)
		if err != nil {
			return "", err
		}
		return "i:" + strconv.FormatInt(value, 10), nil
	case SQLNumeric:
		rational, ok := new(big.Rat).SetString(string(typed.CanonicalBytes()))
		if !ok {
			return "", errors.New("exposure-scale numeric value is not an exact rational")
		}
		return "n:" + rational.RatString(), nil
	default:
		return "", fmt.Errorf("exposure-scale Fact field type %q is unsupported", sqlType)
	}
}

func dependencyGCD(left, right int64) int64 {
	for right != 0 {
		left, right = right, left%right
	}
	if left < 0 {
		return -left
	}
	return left
}

func isFormalDependencyScale(value int64) bool {
	return value == DependencyScale10K || value == DependencyScale100K || value == DependencyScale1035000
}
