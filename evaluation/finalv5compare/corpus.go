// Package finalv5compare is the frozen comparison sequence for the P9.F
// budget-semantics study: a seeded random-range SUM workload over the
// scale-e7 relation, half of whose statements repeat earlier ones. The BDG
// arm answers repeats at zero charge and spends the Dependency budget only
// on unique footprints; the DProvDB arm's numbers come from its reproduced
// artifacts unmodified. Budgets and footprints are derived a priori from
// the closed-form dataset model, never from measurements.
package finalv5compare

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"

	"taskbound.local/agent-data-gateway/evaluation/finalv5scale7"
)

const (
	CorpusID      = "taskgate-final-v5-compare7-sequence-corpus-v1"
	SchemaVersion = 1

	Product = finalv5scale7.Product

	// The sequence runs against the scale7 profile's a-priori recipe
	// (final-v5-scale7-v1); the study observes where the Dependency ledger
	// refuses, so the corpus records the recipe ceiling it was derived
	// against rather than inventing its own.
	RecipeMaxDependencyFacts int64 = finalv5scale7.MaxDependencyFacts

	SequenceSeed   int64 = 20260903
	UniqueCount          = 30
	RepeatCount          = 30
	BoundLow       int64 = 1000
	BoundHigh      int64 = 100000
)

//go:embed corpus-v1.json
var corpusBytes []byte

// Step is one statement of the frozen sequence. RepeatOf is zero for a
// unique statement, else the 1-based index of the step it repeats
// byte-for-byte. Footprint is the statement's own declared Dependency
// footprint (fourteen facts per surviving row); the ledger charge of a
// repeat is zero and of a unique overlap-free prefix is cumulative.
type Step struct {
	Index           int      `json:"index"`
	RepeatOf        int      `json:"repeat_of,omitempty"`
	Bound           int64    `json:"bound"`
	SQL             string   `json:"sql"`
	ExpectedScalars []string `json:"expected_scalars,omitempty"`
	Footprint       int64    `json:"footprint"`
}

type Manifest struct {
	SchemaVersion            int    `json:"schema_version"`
	CorpusID                 string `json:"corpus_id"`
	Product                  string `json:"product"`
	SequenceSeed             int64  `json:"sequence_seed"`
	RecipeMaxDependencyFacts int64  `json:"recipe_max_dependency_facts"`
	Steps                    []Step `json:"steps"`
}

// BuildManifest generates the frozen sequence: thirty unique bounds drawn
// from the seeded generator, then thirty repeats drawn (same generator)
// from the unique prefix, interleaved deterministically (alternating from
// step 11 on so early steps establish history).
func BuildManifest() (Manifest, error) {
	manifest := Manifest{SchemaVersion: SchemaVersion, CorpusID: CorpusID, Product: Product,
		SequenceSeed: SequenceSeed, RecipeMaxDependencyFacts: RecipeMaxDependencyFacts}
	generator := rand.New(rand.NewSource(SequenceSeed))
	uniques := make([]int64, 0, UniqueCount)
	seen := map[int64]bool{}
	for len(uniques) < UniqueCount {
		bound := BoundLow + generator.Int63n(BoundHigh-BoundLow+1)
		if seen[bound] {
			continue
		}
		seen[bound] = true
		uniques = append(uniques, bound)
	}
	repeats := make([]int, 0, RepeatCount)
	for len(repeats) < RepeatCount {
		repeats = append(repeats, 1+generator.Intn(len(uniques)))
	}
	nextUnique, nextRepeat := 0, 0
	uniqueIndexToStep := map[int]int{}
	for position := 1; position <= UniqueCount+RepeatCount; position++ {
		useRepeat := position > 10 && position%2 == 0 && nextRepeat < RepeatCount
		if nextUnique >= UniqueCount {
			useRepeat = true
		}
		var step Step
		if useRepeat {
			source := repeats[nextRepeat]
			nextRepeat++
			if source > nextUnique {
				source = ((source - 1) % nextUnique) + 1
			}
			original := manifest.Steps[uniqueIndexToStep[source]-1]
			step = Step{Index: position, RepeatOf: original.Index, Bound: original.Bound,
				SQL: original.SQL, Footprint: original.Footprint}
		} else {
			bound := uniques[nextUnique]
			nextUnique++
			built, err := buildStep(position, bound)
			if err != nil {
				return Manifest{}, err
			}
			step = built
			uniqueIndexToStep[nextUnique] = position
		}
		manifest.Steps = append(manifest.Steps, step)
	}
	return manifest, nil
}

func buildStep(position int, bound int64) (Step, error) {
	step := Step{Index: position, Bound: bound,
		SQL:       StepSQL(bound),
		Footprint: bound * int64(len(finalv5scale7.LadderColumns)+len(finalv5scale7.PredicateColumns)+1)}
	for _, column := range finalv5scale7.LadderColumns {
		total := new(big.Rat)
		for rowID := int64(1); rowID <= bound; rowID++ {
			value, err := columnRat(column, rowID)
			if err != nil {
				return Step{}, err
			}
			total.Add(total, value)
		}
		step.ExpectedScalars = append(step.ExpectedScalars, total.RatString())
	}
	return step, nil
}

// StepSQL is the scale7 rung SQL with a parameterized bound.
func StepSQL(bound int64) string {
	template := finalv5scale7.Load
	_ = template
	return fmt.Sprintf(sqlTemplate, bound)
}

const sqlTemplate = "SELECT sum(amount) AS total_amount,\n" +
	"       sum(sequence_no) AS total_sequence_no,\n" +
	"       sum(quantity) AS total_quantity,\n" +
	"       sum(unit_price) AS total_unit_price,\n" +
	"       sum(tax_amount) AS total_tax_amount,\n" +
	"       sum(revision) AS total_revision\n" +
	"FROM final_v5_scale_e7\n" +
	"WHERE row_id <= %d\n" +
	"  AND category IN ('alpha', 'beta', 'gamma', 'delta')\n" +
	"  AND region IN ('north', 'south', 'east', 'west', 'central')\n" +
	"  AND event_date <= '2031-12-31'\n" +
	"  AND settled_date <= '2031-12-31'\n" +
	"  AND event_timestamp <= '2031-12-31 00:00:00'\n" +
	"  AND processed_at <= '2031-12-31 00:00:00';"

func columnRat(column string, rowID int64) (*big.Rat, error) {
	switch column {
	case "amount":
		return finalv5scale7.RowAmount(rowID), nil
	case "sequence_no":
		return new(big.Rat).SetInt64(finalv5scale7.RowSequenceNo(rowID)), nil
	case "quantity":
		return new(big.Rat).SetInt64(finalv5scale7.RowQuantity(rowID)), nil
	case "unit_price":
		return finalv5scale7.RowUnitPrice(rowID), nil
	case "tax_amount":
		return finalv5scale7.RowTaxAmount(rowID), nil
	case "revision":
		return new(big.Rat).SetInt64(finalv5scale7.RowRevision(rowID)), nil
	}
	return nil, fmt.Errorf("column %q has no scalar expectation", column)
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
		return Manifest{}, fmt.Errorf("decode embedded compare7 corpus: %w", err)
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.CorpusID != CorpusID ||
		manifest.Product != Product || manifest.SequenceSeed != SequenceSeed ||
		len(manifest.Steps) != UniqueCount+RepeatCount {
		return Manifest{}, fmt.Errorf("embedded compare7 corpus disagrees with the frozen constants")
	}
	for index, step := range manifest.Steps {
		if step.Index != index+1 || step.Bound < BoundLow || step.Bound > BoundHigh ||
			step.SQL != StepSQL(step.Bound) || step.Footprint != step.Bound*14 {
			return Manifest{}, fmt.Errorf("compare7 step %d is malformed", index+1)
		}
		if step.RepeatOf != 0 {
			original := manifest.Steps[step.RepeatOf-1]
			if original.RepeatOf != 0 || original.SQL != step.SQL {
				return Manifest{}, fmt.Errorf("compare7 step %d repeat identity is broken", index+1)
			}
		}
	}
	return manifest, nil
}
