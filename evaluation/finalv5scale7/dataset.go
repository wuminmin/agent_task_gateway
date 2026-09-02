// Package finalv5scale7 is the frozen SUM-ladder corpus for the P9.E scale
// point: aggregates of increasing exact Dependency footprint over the
// deterministic 1,250,000-row final_v5_scale_e7 relation, whose largest rung
// settles more than 10^7 declared Dependency facts in one admitted query.
// Expectations and budgets are derived a priori from the closed-form dataset
// model and the declared Dependency rule via the independent oracle
// vocabulary, never from measurements.
package finalv5scale7

import (
	"fmt"
	"math/big"
)

// The closed-form column model mirrors, formula for formula, the reviewed
// generator sql/datasets/benchmark-v1-generate.sql for
// final_v5_benchmark.scale_e7 (byte-identical formulas to result_heavy with
// only the row bound changed).
const DatasetRows int64 = 1250000

var datasetCategories = [4]string{"alpha", "beta", "gamma", "delta"}

// RowCategory is (ARRAY['alpha','beta','gamma','delta'])[((row_id-1) % 4) + 1].
func RowCategory(rowID int64) string { return datasetCategories[(rowID-1)%4] }

// RowAmount is ((row_id * 7919) % 100000000)::numeric / 100.
func RowAmount(rowID int64) *big.Rat {
	return new(big.Rat).SetFrac64((rowID*7919)%100000000, 100)
}

// RowSequenceNo is (row_id % 1000000)::integer.
func RowSequenceNo(rowID int64) int64 { return rowID % 1000000 }

// RowQuantity is 1 + ((row_id - 1) % 10000).
func RowQuantity(rowID int64) int64 { return 1 + ((rowID - 1) % 10000) }

// RowUnitPrice is ((row_id * 104729) % 10000000)::numeric / 10000.
func RowUnitPrice(rowID int64) *big.Rat {
	return new(big.Rat).SetFrac64((rowID*104729)%10000000, 10000)
}

// RowTaxAmount is (row_id % 11 == 0 ? -1 : 1) * ((row_id * 37) % 1000000)::numeric / 100.
func RowTaxAmount(rowID int64) *big.Rat {
	value := new(big.Rat).SetFrac64((rowID*37)%1000000, 100)
	if rowID%11 == 0 {
		value.Neg(value)
	}
	return value
}

// RowRevision is ((row_id - 1) % 97)::integer.
func RowRevision(rowID int64) int64 { return (rowID - 1) % 97 }

// CanonicalColumnValue returns the production canonical value string of one
// ladder-relevant column of one row.
func CanonicalColumnValue(column string, rowID int64) (sqlType, canonical string, err error) {
	switch column {
	case "row_id":
		return "bigint", fmt.Sprintf("i:%d", rowID), nil
	case "category":
		return "text", "s:" + RowCategory(rowID), nil
	case "amount":
		return "numeric", "n:" + RowAmount(rowID).RatString(), nil
	case "sequence_no":
		return "integer", fmt.Sprintf("i:%d", RowSequenceNo(rowID)), nil
	case "quantity":
		return "bigint", fmt.Sprintf("i:%d", RowQuantity(rowID)), nil
	case "unit_price":
		return "numeric", "n:" + RowUnitPrice(rowID).RatString(), nil
	case "tax_amount":
		return "numeric", "n:" + RowTaxAmount(rowID).RatString(), nil
	case "revision":
		return "integer", fmt.Sprintf("i:%d", RowRevision(rowID)), nil
	default:
		return "", "", fmt.Errorf("column %q is not part of the scale-7 dataset model", column)
	}
}

// columnRat returns the column's exact rational value for scalar expectations.
func columnRat(column string, rowID int64) (*big.Rat, error) {
	switch column {
	case "amount":
		return RowAmount(rowID), nil
	case "sequence_no":
		return new(big.Rat).SetInt64(RowSequenceNo(rowID)), nil
	case "quantity":
		return new(big.Rat).SetInt64(RowQuantity(rowID)), nil
	case "unit_price":
		return RowUnitPrice(rowID), nil
	case "tax_amount":
		return RowTaxAmount(rowID), nil
	case "revision":
		return new(big.Rat).SetInt64(RowRevision(rowID)), nil
	}
	return nil, fmt.Errorf("column %q has no scalar expectation", column)
}
