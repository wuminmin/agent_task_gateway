package finalv5scale7

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
	CorpusID      = "taskgate-final-v5-scale7-ladder-corpus-v1"
	SchemaVersion = 1

	// The frozen relation and its publication identity, as declared by the
	// contract-indexed master Catalog (publication final-v5-scale-e7-v1).
	SourceNamespace = "final_v5.scale_e7"
	Snapshot        = "final-v5-scale-e7-2026-v1"
	CanonicalTable  = "final_v5_benchmark.scale_e7"

	Product = "final_v5_scale_e7"

	// A-priori budgets, derived from the declared Dependency rule (nine facts
	// per surviving row: base-row fact, row_id and category predicate cells,
	// six summed argument cells), never from measurements. The largest rung
	// settles 9 * 1,250,000 = 11,250,000 Dependency facts; the ceiling adds
	// headroom so the ladder never refuses on the measured dimension.
	MaxDependencyFacts int64 = 12000000
	// Each rung releases six derived aggregate facts; the whole ladder plus
	// generous retry headroom stays far below this.
	MaxReleaseFacts int64 = 100
	// One composite plus five predicate atoms per accepted rung.
	MaxOutcomeFacts int64 = 40
	MaxQueries      int64 = 160
)

//go:embed corpus-v1.json
var corpusBytes []byte

type SetCommitment struct {
	Cardinality int64  `json:"cardinality"`
	SetSHA256   string `json:"set_sha256"`
}

// Rung is one SUM-ladder query. DirectSQL runs against the canonical
// benchmark relation; LogicalSQL substitutes the governed Product.
// ExpectedScalars are the exact sums as reduced rational strings, from the
// closed-form dataset model. Dependency is the exact set commitment in the
// independent oracle's fact vocabulary.
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
}

type Manifest struct {
	SchemaVersion      int    `json:"schema_version"`
	CorpusID           string `json:"corpus_id"`
	SourceNamespace    string `json:"source_namespace"`
	Snapshot           string `json:"snapshot"`
	DatasetRows        int64  `json:"dataset_rows"`
	MaxDependencyFacts int64  `json:"max_dependency_facts"`
	MaxReleaseFacts    int64  `json:"max_release_facts"`
	MaxOutcomeFacts    int64  `json:"max_outcome_facts"`
	MaxQueries         int64  `json:"max_queries"`
	Rungs              []Rung `json:"rungs"`
}

// LadderRows is the frozen rung matrix: dependency footprints of 1.125e6,
// 2.8125e6, 5.625e6, and 1.125e7 facts at nine facts per surviving row.
var LadderRows = []int64{125000, 312500, 625000, 1250000}

// LadderColumns are the six exact-summable argument columns.
var LadderColumns = []string{"amount", "sequence_no", "quantity", "unit_price", "tax_amount", "revision"}

func (rung Rung) LogicalSQL(product string) string {
	return strings.Replace(rung.DirectSQL, CanonicalTable, product, 1)
}

func rungSQL(table string, rows int64) string {
	sums := make([]string, 0, len(LadderColumns))
	for _, column := range LadderColumns {
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
		MaxDependencyFacts: MaxDependencyFacts, MaxReleaseFacts: MaxReleaseFacts,
		MaxOutcomeFacts: MaxOutcomeFacts, MaxQueries: MaxQueries}
	for index, rows := range LadderRows {
		rung, err := buildRung(index+1, rows)
		if err != nil {
			return Manifest{}, err
		}
		manifest.Rungs = append(manifest.Rungs, rung)
	}
	return manifest, nil
}

func buildRung(index int, rows int64) (Rung, error) {
	rung := Rung{Index: index, ID: fmt.Sprintf("rung-%02d-%drows", index, rows),
		Rows: rows, Columns: append([]string(nil), LadderColumns...),
		DirectSQL: rungSQL(CanonicalTable, rows),
		// One row_id atom plus the four category IN literals.
		ExpectedPredicateAtoms: 5}
	for _, column := range LadderColumns {
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
	// argument columns never overlap the predicate columns).
	dependencyColumns := append([]string{"row_id", "category"}, LadderColumns...)
	dependency, err := summarizeCellStream("dependency", rows, dependencyColumns, true)
	if err != nil {
		return Rung{}, err
	}
	rung.Dependency = dependency
	// Release: six derived aggregate facts whose identities bind the witness
	// commitment only the production pipeline materializes cheaply; the
	// corpus commits to the cardinality and leaves the digest to the
	// per-fact validators.
	rung.Release = SetCommitment{Cardinality: int64(len(LadderColumns)), SetSHA256: ""}
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

// CorpusSHA256 returns the digest of the embedded frozen corpus bytes.
func CorpusSHA256() string {
	digest := sha256.Sum256(corpusBytes)
	return hex.EncodeToString(digest[:])
}

// Load parses and validates the embedded frozen corpus.
func Load() (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(corpusBytes, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode embedded scale-7 corpus: %w", err)
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.CorpusID != CorpusID ||
		manifest.SourceNamespace != SourceNamespace || manifest.Snapshot != Snapshot ||
		manifest.DatasetRows != DatasetRows || len(manifest.Rungs) != len(LadderRows) {
		return Manifest{}, fmt.Errorf("embedded scale-7 corpus disagrees with the frozen constants")
	}
	for index, rung := range manifest.Rungs {
		if rung.Index != index+1 || rung.Rows != LadderRows[index] ||
			!reflect.DeepEqual(rung.Columns, LadderColumns) ||
			rung.DirectSQL != rungSQL(CanonicalTable, rung.Rows) ||
			len(rung.ExpectedScalars) != len(LadderColumns) ||
			rung.Dependency.Cardinality != rung.Rows*int64(len(LadderColumns)+3) ||
			!validHex64(rung.Dependency.SetSHA256) {
			return Manifest{}, fmt.Errorf("scale-7 rung %d is malformed", index+1)
		}
	}
	return manifest, nil
}

func validHex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
