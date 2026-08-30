// Package finalv5footprint is the frozen refused-footprint ladder corpus for
// the (c2) timing-channel measurement: aggregates of increasing exact
// Dependency footprint over the deterministic 100,000-row
// final_v5_result_heavy relation, executed once against an unlimited budget
// profile (every rung accepted) and once against a bounded profile whose
// Dependency budget equals the smallest rung's derived footprint (every
// larger rung refused). Budgets and refusal decisions are derived a priori
// from the declared Dependency rule via the independent oracle vocabulary,
// never from measurements.
package finalv5footprint

import (
	"fmt"
	"math/big"
)

// The closed-form column model mirrors, formula for formula, the reviewed
// generator sql/datasets/benchmark-v1-generate.sql for
// final_v5_benchmark.result_heavy. Canonical value strings use the production
// FactID encoding: "i:<decimal>" for integers, "n:<reduced rational>" for
// numerics, "s:<text>" for text.
const DatasetRows int64 = 100000

var datasetCategories = [4]string{"alpha", "beta", "gamma", "delta"}

// RowCategory is (ARRAY['alpha','beta','gamma','delta'])[((row_id-1) % 4) + 1].
func RowCategory(rowID int64) string { return datasetCategories[(rowID-1)%4] }

// RowAmount is ((row_id * 7919) % 100000000)::numeric / 100.
func RowAmount(rowID int64) *big.Rat {
	return new(big.Rat).SetFrac64((rowID*7919)%100000000, 100)
}

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
	case "quantity":
		return "bigint", fmt.Sprintf("i:%d", RowQuantity(rowID)), nil
	case "unit_price":
		return "numeric", "n:" + RowUnitPrice(rowID).RatString(), nil
	case "tax_amount":
		return "numeric", "n:" + RowTaxAmount(rowID).RatString(), nil
	default:
		return "", "", fmt.Errorf("column %q is not part of the ladder dataset model", column)
	}
}

// columnRat returns the column's exact rational value for scalar expectations.
func columnRat(column string, rowID int64) (*big.Rat, error) {
	switch column {
	case "amount":
		return RowAmount(rowID), nil
	case "quantity":
		return new(big.Rat).SetInt64(RowQuantity(rowID)), nil
	case "unit_price":
		return RowUnitPrice(rowID), nil
	case "tax_amount":
		return RowTaxAmount(rowID), nil
	default:
		return nil, fmt.Errorf("column %q is not summable in the ladder", column)
	}
}
