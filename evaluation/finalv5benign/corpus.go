package finalv5benign

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	CorpusID      = "taskgate-final-v5-benign-trace-corpus-v1"
	SchemaVersion = 1
	// TraceWorkloadID names the (c3) benign-trace workload.
	TraceWorkloadID = "benign-agent-trace-v1"
)

// Classification is the a-priori three-layer statement classification.
type Classification string

const (
	// ClassPolicyRefused: the production lowering/compile/policy chain refuses
	// the statement as written; the refusal is budget-independent.
	ClassPolicyRefused Classification = "policy_refused"
	// ClassZeroRelease: authorized, but the WHERE predicate matches no base
	// row, so it releases nothing and charges no Result/Dependency facts.
	ClassZeroRelease Classification = "zero_release"
	// ClassReleased: authorized with a nonzero footprint.
	ClassReleased Classification = "released"
)

// SetCommitment is an exact set cardinality plus digest (digest optional).
type SetCommitment struct {
	Cardinality int64  `json:"cardinality"`
	SetSHA256   string `json:"set_sha256,omitempty"`
}

// Statement is one benign-trace statement with its a-priori expectations.
type Statement struct {
	Index          int            `json:"index"`
	ID             string         `json:"id"`
	SQL            string         `json:"sql"`
	SQLSHA256      string         `json:"sql_sha256"`
	Products       []string       `json:"products"`
	Classification Classification `json:"classification"`
	// PolicyCode/PolicyReason are set exactly when Classification is
	// policy_refused: the first refusal the production chain reports.
	PolicyCode   string `json:"policy_code,omitempty"`
	PolicyReason string `json:"policy_reason,omitempty"`
	// ReleasedRows is the visible result-row count (after HAVING and LIMIT).
	ReleasedRows int64 `json:"released_rows"`
	// ReleaseFacts is the Result-dimension fact cardinality the release
	// charges: detail rows release one row fact plus one fact per cell;
	// grouped aggregates release one derived fact per aggregate cell.
	ReleaseFacts int64 `json:"release_facts"`
	// Dependency commits the exact WHERE-surviving evidence fact set. The
	// companion applies WHERE only (never HAVING or LIMIT), so an
	// empty-result statement can still carry a large dependency footprint.
	Dependency SetCommitment `json:"dependency"`
	// EvidenceRows is the companion's row count (WHERE survivors, joined
	// rows for join plans) the reservation estimate derives from.
	EvidenceRows int64 `json:"evidence_rows"`
	// EvidenceFields is the compiled companion projection per plan, recorded
	// for review; the fact stream uses the per-source evidence fields.
	EvidenceFields []string `json:"evidence_fields,omitempty"`
	// PredicateAtoms is the unique predicate-atom count of the statement.
	PredicateAtoms int64 `json:"predicate_atoms"`
}

// RecipeBudget is one a-priori budget profile computed from the corpus by the
// section 5.2 recipe before any execution.
type RecipeBudget struct {
	Name            string `json:"name"`
	Multiplier      int64  `json:"multiplier"`
	MaxReleaseFacts int64  `json:"max_release_facts"`
	MaxInfluence    int64  `json:"max_influence_facts"`
	MaxOutcome      int64  `json:"max_outcome_facts"`
	MaxQueries      int64  `json:"max_queries"`
}

// Manifest is the frozen corpus document.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	CorpusID      string `json:"corpus_id"`
	WorkloadID    string `json:"workload_id"`
	// SourceDirectory names the unedited agent-written statement set.
	SourceDirectory string      `json:"source_directory"`
	Statements      []Statement `json:"statements"`
	// Budgets are the recipe / x2 / x4 profiles derived from the statements.
	Budgets []RecipeBudget `json:"budgets"`
	// Totals for review: authorized statements, policy refusals, zero-release
	// statements, summed release facts, and the maximum single-statement
	// dependency footprint the recipe's Dependency budget equals.
	AuthorizedStatements int64 `json:"authorized_statements"`
	// TraceUnionDependencyFacts is the set-union dependency cardinality of
	// the whole trace: cumulative accounting is set-valued, so this is the
	// true a-priori requirement, recorded alongside the recipe's
	// max-single-statement Dependency budget.
	TraceUnionDependencyFacts  int64  `json:"trace_union_dependency_facts"`
	TraceUnionDependencySHA256 string `json:"trace_union_dependency_sha256,omitempty"`
	PolicyRefused              int64  `json:"policy_refused"`
	ZeroRelease                int64  `json:"zero_release"`
	TotalReleaseFacts          int64  `json:"total_release_facts"`
	MaxDependencyFacts         int64  `json:"max_dependency_facts"`
}

//go:embed corpus-v1.json
var corpusBytes []byte

// CorpusSHA256 returns the digest of the embedded frozen corpus bytes.
func CorpusSHA256() string {
	digest := sha256.Sum256(corpusBytes)
	return hex.EncodeToString(digest[:])
}

// EncodeManifest is the frozen byte encoding of the corpus.
func EncodeManifest(manifest Manifest) ([]byte, error) {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// Load parses and validates the embedded frozen corpus.
func Load() (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(corpusBytes, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode embedded benign corpus: %w", err)
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.CorpusID != CorpusID ||
		manifest.WorkloadID != TraceWorkloadID || len(manifest.Statements) == 0 ||
		len(manifest.Budgets) != 3 {
		return Manifest{}, fmt.Errorf("embedded benign corpus disagrees with the frozen constants")
	}
	for index, statement := range manifest.Statements {
		if statement.Index != index+1 || statement.ID == "" || statement.SQL == "" ||
			len(statement.SQLSHA256) != 64 {
			return Manifest{}, fmt.Errorf("benign corpus statement %d is malformed", index+1)
		}
		digest := sha256.Sum256([]byte(statement.SQL))
		if hex.EncodeToString(digest[:]) != statement.SQLSHA256 {
			return Manifest{}, fmt.Errorf("benign corpus statement %s bytes drifted", statement.ID)
		}
	}
	return manifest, nil
}
