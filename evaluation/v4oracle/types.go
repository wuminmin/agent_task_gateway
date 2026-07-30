// Package v4oracle implements the offline, independent maximum-point oracle
// used by the V4 supplemental acceptance campaign.
//
// The package deliberately does not import internal/gateway and never calls
// the ordinal derivation engine.  Expected facts are reconstructed from the
// frozen Business snapshot, while committed facts are expanded from the
// immutable COLD dictionaries and Control-PG bitmap containers.
package v4oracle

import "time"

const (
	ReportSchema = "taskgate-v4-million-fact-oracle-v1"
	OracleID     = "taskgate-independent-external-merge-oracle-v1"
)

type Config struct {
	ResultsPath     string
	ArtifactDir     string
	SpoolParent     string
	RepositoryRoot  string
	SortMemoryBytes int64
}

type Report struct {
	SchemaVersion string          `json:"schema_version"`
	OracleID      string          `json:"oracle_id"`
	Status        string          `json:"status"`
	StartedAt     time.Time       `json:"started_at"`
	FinishedAt    time.Time       `json:"finished_at"`
	Provenance    Provenance      `json:"provenance"`
	Boundary      Boundary        `json:"independence_boundary"`
	Observation   Observation     `json:"observation"`
	Facts         FactChecks      `json:"fact_checks"`
	Witnesses     WitnessChecks   `json:"witness_checks"`
	Resources     ResourceSummary `json:"resources"`
	Gates         []Gate          `json:"gates"`
	Errors        []string        `json:"errors,omitempty"`
}

type Provenance struct {
	ResultsSHA256          string            `json:"results_sha256"`
	OraclePackageSHA256    string            `json:"oracle_package_sha256"`
	RepositorySourceSHA256 string            `json:"repository_source_scope_sha256"`
	RepositorySourceFiles  int               `json:"repository_source_scope_files"`
	ExecutableSHA256       string            `json:"executable_sha256,omitempty"`
	Artifacts              []ArtifactBinding `json:"cold_artifacts"`
}

type ArtifactBinding struct {
	PublicationName  string `json:"publication_name"`
	DictionarySHA256 string `json:"dictionary_sha256"`
	ManifestSHA256   string `json:"manifest_sha256"`
	ArtifactSHA256   string `json:"artifact_sha256"`
	Bytes            int64  `json:"bytes"`
}

type Boundary struct {
	ExpectedSource     string `json:"expected_source"`
	ActualSource       string `json:"actual_source"`
	Algorithm          string `json:"algorithm"`
	IndependenceScope  string `json:"independence_scope"`
	EvidenceValidation string `json:"evidence_validation"`
	HotPathCalls       int    `json:"v4_bitmap_derivation_hot_path_calls"`
}

type Observation struct {
	SHA256              string `json:"sha256"`
	DictionarySetSHA256 string `json:"dictionary_set_sha256"`
	ReleaseSetSHA256    string `json:"release_set_sha256"`
	InfluenceSetSHA256  string `json:"influence_set_sha256"`
	OutcomeSetSHA256    string `json:"outcome_set_sha256"`
	RecomputedSHA256    string `json:"recomputed_sha256"`
	NormalFormSHA256    string `json:"normal_form_sha256"`
}

type FactChecks struct {
	ExpectedRelease            uint64   `json:"expected_release"`
	ActualRelease              uint64   `json:"actual_release"`
	MatchedRelease             uint64   `json:"matched_release"`
	ExpectedInfluence          uint64   `json:"expected_influence"`
	ActualInfluence            uint64   `json:"actual_influence"`
	MatchedInfluence           uint64   `json:"matched_influence"`
	ExpectedOutcome            uint64   `json:"expected_outcome"`
	ActualOutcome              uint64   `json:"actual_outcome"`
	MatchedOutcome             uint64   `json:"matched_outcome"`
	FactHashMatches            uint64   `json:"fact_hash_matches"`
	CanonicalPayloadMatches    uint64   `json:"canonical_payload_matches"`
	TotalCompared              uint64   `json:"total_compared"`
	HashMismatches             uint64   `json:"hash_mismatches"`
	CanonicalPayloadMismatches uint64   `json:"canonical_payload_mismatches"`
	MissingFacts               uint64   `json:"missing_facts"`
	ExtraFacts                 uint64   `json:"extra_facts"`
	InfluenceChunkSHA256       []string `json:"influence_chunk_sha256"`
}

type WitnessChecks struct {
	DerivedFacts              int    `json:"derived_facts"`
	MatchedCommitments        int    `json:"matched_commitments"`
	CommitmentMismatches      int    `json:"commitment_mismatches"`
	ExpectedWitnessItems      uint64 `json:"expected_witness_items"`
	ExpectedTotalMultiplicity uint64 `json:"expected_total_multiplicity"`
	CommitmentSetSHA256       string `json:"commitment_set_sha256"`
	MultiplicityStreamSHA256  string `json:"multiplicity_stream_sha256"`
}

type ResourceSummary struct {
	SortMemoryLimitBytes        int64    `json:"sort_memory_limit_bytes"`
	TheoreticalBufferBoundBytes int64    `json:"theoretical_buffer_bound_bytes"`
	SpoolBytes                  int64    `json:"spool_bytes"`
	SortRuns                    int      `json:"sort_runs"`
	SortRunSHA256               []string `json:"sort_run_sha256"`
	MaximumResidentRecords      int      `json:"maximum_resident_records"`
	PeakRSSBytes                uint64   `json:"peak_rss_bytes"`
	BusinessRows                int64    `json:"business_rows"`
	ColdFactsScanned            uint64   `json:"cold_facts_scanned"`
}

type Gate struct {
	ID          string `json:"id"`
	Requirement string `json:"requirement"`
	Status      string `json:"status"`
	Evidence    any    `json:"evidence,omitempty"`
	Reason      string `json:"reason,omitempty"`
}
