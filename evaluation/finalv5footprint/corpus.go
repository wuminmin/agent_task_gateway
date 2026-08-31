package finalv5footprint

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

const (
	CorpusID      = "taskgate-final-v5-footprint-ladder-corpus-v1"
	SchemaVersion = 1

	// The frozen relation and its publication identity, as declared by the
	// contract-indexed master Catalog (publication final-v5-result-heavy-v1).
	SourceNamespace = "final_v5.result_heavy"
	Snapshot        = "final-v5-result-heavy-2026-v1"
	CanonicalTable  = "final_v5_benchmark.result_heavy"

	UnlimitedProduct = "final_v5_footprint_unlimited_result_heavy"
	BoundedProduct   = "final_v5_footprint_bounded_result_heavy"
	UnlimitedProfile = "final-v5-footprint-unlimited-v1"
	BoundedProfile   = "final-v5-footprint-bounded-v1"

	// Every rung settles under a fresh root, so the bounded arm's refusals are
	// single-query charges, never accumulation. The Dependency budget is the
	// smallest rung's derived footprint: rows*(row fact + row_id predicate
	// cell + category predicate cell + argument cells) = 100*(1+1+1+1).
	// Derived a priori from the declared Dependency rule
	// (evaluation/generatedalgebra/reference.go contribute/aggregate rules;
	// corroborated by the sealed S6 arithmetic: 100k rows x 16 projected
	// columns -> 1.7M dependency facts = rows*(16 cells + 1 row fact) with
	// predicate columns deduplicated into the projection).
	BoundedMaxDependencyFacts int64 = 400
	// The aggregate rungs release at most four derived facts and settle one
	// composite with five predicate atoms (row_id plus four category
	// literals); the remaining ceilings are set well clear of every rung.
	BoundedMaxReleaseFacts int64 = 16
	BoundedMaxOutcomeFacts int64 = 16
)

//go:embed corpus-v1.json
var corpusBytes []byte

type SetCommitment struct {
	Cardinality int64  `json:"cardinality"`
	SetSHA256   string `json:"set_sha256"`
}

// Rung is one ladder query. DirectSQL runs against the canonical benchmark
// relation; LogicalSQL substitutes the governed Product. ExpectedScalars are
// the exact sums as reduced rational strings, derived from the closed-form
// dataset model. Dependency/Release are exact set commitments in the
// independent oracle's fact vocabulary. BoundedRefused is the a-priori
// decision under BoundedMaxDependencyFacts with a fresh root per rung.
type Rung struct {
	Index                  int           `json:"index"`
	ID                     string        `json:"id"`
	Rows                   int64         `json:"rows"`
	Columns                []string      `json:"columns"`
	DirectSQL              string        `json:"direct_sql"`
	ExpectedScalars        []string      `json:"expected_scalars"`
	Dependency             SetCommitment `json:"dependency"`
	Release                SetCommitment `json:"release"`
	ExpectedPredicateAtoms int64         `json:"expected_predicate_atoms"`
	BoundedRefused         bool          `json:"bounded_refused"`
}

type Manifest struct {
	SchemaVersion             int    `json:"schema_version"`
	CorpusID                  string `json:"corpus_id"`
	SourceNamespace           string `json:"source_namespace"`
	Snapshot                  string `json:"snapshot"`
	DatasetRows               int64  `json:"dataset_rows"`
	BoundedMaxDependencyFacts int64  `json:"bounded_max_dependency_facts"`
	BoundedMaxReleaseFacts    int64  `json:"bounded_max_release_facts"`
	BoundedMaxOutcomeFacts    int64  `json:"bounded_max_outcome_facts"`
	Rungs                     []Rung `json:"rungs"`
}

// LadderRows and LadderColumnSets are the frozen rung matrix.
var LadderRows = []int64{100, 1000, 10000, 100000}

var LadderColumnSets = [][]string{
	{"amount"},
	{"amount", "quantity"},
	{"amount", "quantity", "unit_price", "tax_amount"},
}

// BoundedRefusalCode is the a-priori refusal code of a BoundedRefused rung.
// The Gateway refuses at one of two sites: when the rung's evidence-row span
// (its base rows) still fits the task's Dependency limit, the pre-execution
// estimate crosses the remaining budget and the reservation refuses with
// EXPOSURE_BUDGET_EXHAUSTED; when the span itself exceeds the Dependency
// limit, complete provenance evidence can never be produced and the query
// fails closed with EXPOSURE_EVIDENCE_REQUIRED, charging nothing. Both are
// uncharged refusals under the exposure contract (observed on
// pilot-footprint-04; rungs 2-3 vs rung 4).
func (rung Rung) BoundedRefusalCode() string {
	if rung.Rows > BoundedMaxDependencyFacts {
		return "EXPOSURE_EVIDENCE_REQUIRED"
	}
	return "EXPOSURE_BUDGET_EXHAUSTED"
}

func (rung Rung) LogicalSQL(product string) string {
	return strings.Replace(rung.DirectSQL, CanonicalTable, product, 1)
}

func rungSQL(table string, rows int64, columns []string) string {
	sums := make([]string, 0, len(columns))
	for _, column := range columns {
		sums = append(sums, fmt.Sprintf("sum(%s) AS total_%s", column, column))
	}
	return fmt.Sprintf("SELECT %s\nFROM %s\nWHERE row_id <= %d\n  AND category IN ('alpha', 'beta', 'gamma', 'delta');",
		strings.Join(sums, ",\n       "), table, rows)
}

// BuildManifest recomputes the complete frozen corpus from the closed-form
// dataset model and the declared Dependency rule. The embedded corpus must be
// byte-identical to its JSON encoding (the pin test enforces this), so every
// expectation is derivable, never captured from an execution.
func BuildManifest() (Manifest, error) {
	manifest := Manifest{SchemaVersion: SchemaVersion, CorpusID: CorpusID,
		SourceNamespace: SourceNamespace, Snapshot: Snapshot, DatasetRows: DatasetRows,
		BoundedMaxDependencyFacts: BoundedMaxDependencyFacts,
		BoundedMaxReleaseFacts:    BoundedMaxReleaseFacts,
		BoundedMaxOutcomeFacts:    BoundedMaxOutcomeFacts}
	index := 0
	for _, rows := range LadderRows {
		for _, columns := range LadderColumnSets {
			index++
			rung, err := buildRung(index, rows, columns)
			if err != nil {
				return Manifest{}, err
			}
			manifest.Rungs = append(manifest.Rungs, rung)
		}
	}
	return manifest, nil
}

func buildRung(index int, rows int64, columns []string) (Rung, error) {
	rung := Rung{Index: index, ID: fmt.Sprintf("rung-%02d-%dx%d", index, rows, len(columns)),
		Rows: rows, Columns: append([]string(nil), columns...),
		DirectSQL: rungSQL(CanonicalTable, rows, columns),
		// One row_id atom plus the four category IN literals.
		ExpectedPredicateAtoms: 5}
	// Exact scalar expectations.
	for _, column := range columns {
		total := new(big.Rat)
		for rowID := int64(1); rowID <= rows; rowID++ {
			value, err := columnRat(column, rowID)
			if err != nil {
				return Rung{}, err
			}
			total.Add(total, value)
		}
		rung.ExpectedScalars = append(rung.ExpectedScalars, total.RatString())
	}
	// Dependency: per surviving row one base-row fact, the row_id and
	// category predicate cells, and each argument cell (set semantics; the
	// ladder's argument columns never overlap its predicate columns).
	dependencyColumns := append([]string{"row_id", "category"}, columns...)
	dependency, err := summarizeCellStream("dependency", rows, dependencyColumns, true)
	if err != nil {
		return Rung{}, err
	}
	rung.Dependency = dependency
	// Release: the rung's derived aggregate facts. Their identities bind the
	// witness commitment of the full member multiset, which only the
	// production pipeline materializes cheaply; the corpus therefore commits
	// to the cardinality and leaves the digest to the per-fact validators.
	rung.Release = SetCommitment{Cardinality: int64(len(columns)), SetSHA256: ""}
	rung.BoundedRefused = dependency.Cardinality > BoundedMaxDependencyFacts
	return rung, nil
}

// summarizeCellStream streams the rung's dependency fact hashes (row facts
// plus the named cells of every surviving row) into the bounded external
// sorter and returns the exact set commitment.
func summarizeCellStream(role string, rows int64, columns []string, includeRowFact bool) (SetCommitment, error) {
	stream := func(yield func(string) error) error {
		for rowID := int64(1); rowID <= rows; rowID++ {
			entityKey, err := rowEntityKey(rowID)
			if err != nil {
				return err
			}
			if includeRowFact {
				fact, err := finalv5oracle.BuildV2BaseRowFact(finalv5oracle.V2BaseRowInput{
					SourceNamespace: SourceNamespace, Snapshot: Snapshot, EntityKey: entityKey})
				if err != nil {
					return err
				}
				if err := yield(fact.SHA256); err != nil {
					return err
				}
			}
			for _, column := range columns {
				sqlType, canonical, err := CanonicalColumnValue(column, rowID)
				if err != nil {
					return err
				}
				fact, err := finalv5oracle.BuildV2BaseCellFact(finalv5oracle.V2BaseCellInput{
					SourceNamespace: SourceNamespace, Snapshot: Snapshot, EntityKey: entityKey,
					Field: column, SQLType: sqlType, CanonicalValue: canonical})
				if err != nil {
					return err
				}
				if err := yield(fact.SHA256); err != nil {
					return err
				}
			}
		}
		return nil
	}
	summary, err := finalv5oracle.SummarizeSemanticSet(role, stream, finalv5oracle.StreamSetOptions{})
	if err != nil {
		return SetCommitment{}, err
	}
	return SetCommitment{Cardinality: summary.Cardinality, SetSHA256: summary.SetSHA256}, nil
}

func rowEntityKey(rowID int64) (string, error) {
	return finalv5oracle.ComposeOracleCanonicalKeyV2("base-entity",
		SourceNamespace, "row_id", "bigint", fmt.Sprintf("i:%d", rowID))
}

// EncodeManifest is the frozen byte encoding of the corpus.
func EncodeManifest(manifest Manifest) ([]byte, error) {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// CorpusSHA256 returns the digest of the embedded corpus bytes.
func CorpusSHA256() string {
	digest := sha256.Sum256(corpusBytes)
	return hex.EncodeToString(digest[:])
}

// Load parses and validates the embedded frozen corpus.
func Load() (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(corpusBytes, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode embedded footprint corpus: %w", err)
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.CorpusID != CorpusID ||
		manifest.SourceNamespace != SourceNamespace || manifest.Snapshot != Snapshot ||
		manifest.DatasetRows != DatasetRows ||
		manifest.BoundedMaxDependencyFacts != BoundedMaxDependencyFacts ||
		manifest.BoundedMaxReleaseFacts != BoundedMaxReleaseFacts ||
		manifest.BoundedMaxOutcomeFacts != BoundedMaxOutcomeFacts ||
		len(manifest.Rungs) != len(LadderRows)*len(LadderColumnSets) {
		return Manifest{}, fmt.Errorf("embedded footprint corpus disagrees with the frozen constants")
	}
	for position, rung := range manifest.Rungs {
		if rung.Index != position+1 || rung.Rows < 1 || rung.Rows > DatasetRows ||
			len(rung.Columns) == 0 || len(rung.ExpectedScalars) != len(rung.Columns) ||
			rung.DirectSQL == "" || rung.Dependency.Cardinality < 1 ||
			!validHex64(rung.Dependency.SetSHA256) ||
			rung.BoundedRefused != (rung.Dependency.Cardinality > BoundedMaxDependencyFacts) {
			return Manifest{}, fmt.Errorf("embedded footprint rung %d is invalid", position+1)
		}
	}
	return manifest, nil
}

func validHex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

// VerifyAgainstBuild recomputes the corpus and requires byte identity with
// the embedded bytes; the pin test and the generator share it.
func VerifyAgainstBuild() error {
	manifest, err := BuildManifest()
	if err != nil {
		return err
	}
	encoded, err := EncodeManifest(manifest)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(encoded, corpusBytes) {
		return fmt.Errorf("embedded corpus differs from the recomputed build (regenerate with evaluation/cmd/footprint-ladder-corpus)")
	}
	return nil
}
