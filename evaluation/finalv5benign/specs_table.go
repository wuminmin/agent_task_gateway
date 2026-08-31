package finalv5benign

import (
	"fmt"
)

// provsqlSpec evaluates a single-source provsql statement whose survivors are
// enumerated by closed-form loops.
func provsqlSpec(product string, atoms int64,
	evaluate func(context evaluationContext, binding sourceBinding) (statementEvaluation, error)) statementSpecification {
	return statementSpecification{products: []string{product}, predicateAtoms: atoms,
		evaluate: func(context evaluationContext) (statementEvaluation, error) {
			binding, err := bindSource(context, product)
			if err != nil {
				return statementEvaluation{}, err
			}
			return evaluate(context, binding)
		}}
}

func ordersEntityKey(binding sourceBinding, orderKey int64) (string, error) {
	return entityKey(binding, "orderkey", "bigint", fmt.Sprintf("i:%d", orderKey))
}

func lineitemEntityKey(binding sourceBinding, orderKey, lineNumber int64) (string, error) {
	return entityKey(binding,
		"orderkey", "bigint", fmt.Sprintf("i:%d", orderKey),
		"linenumber", "integer", fmt.Sprintf("i:%d", lineNumber))
}

func resultHeavyEntityKey(binding sourceBinding, rowID int64) (string, error) {
	return entityKey(binding, "row_id", "bigint", fmt.Sprintf("i:%d", rowID))
}

func streamOrders(binding sourceBinding, from, to int64, yield func(string) error) error {
	for orderKey := from; orderKey <= to; orderKey++ {
		key, err := ordersEntityKey(binding, orderKey)
		if err != nil {
			return err
		}
		local := orderKey
		if err := emitRowFacts(binding, key, func(column string) (string, string, error) {
			return OrdersColumnValue(local, column)
		}, yield); err != nil {
			return err
		}
	}
	return nil
}

func streamLineitems(binding sourceBinding, fromOrder, toOrder int64, yield func(string) error) error {
	for orderKey := fromOrder; orderKey <= toOrder; orderKey++ {
		for line := int64(1); line <= ProvSQLLinesPerOrder; line++ {
			key, err := lineitemEntityKey(binding, orderKey, line)
			if err != nil {
				return err
			}
			localOrder, localLine := orderKey, line
			if err := emitRowFacts(binding, key, func(column string) (string, string, error) {
				return LineitemColumnValue(localOrder, localLine, column)
			}, yield); err != nil {
				return err
			}
		}
	}
	return nil
}

// resultHeavySpec evaluates a single-source result_heavy detail statement.
func resultHeavySpec(atoms int64, survives func(rowID int64) bool,
	released func(survivors int64) (releasedRows, releaseFacts int64)) statementSpecification {
	return statementSpecification{products: []string{"final_v5_result_heavy"}, predicateAtoms: atoms,
		evaluate: func(context evaluationContext) (statementEvaluation, error) {
			binding, err := bindSource(context, "final_v5_result_heavy")
			if err != nil {
				return statementEvaluation{}, err
			}
			var survivors int64
			for rowID := int64(1); rowID <= ResultHeavyRows; rowID++ {
				if survives(rowID) {
					survivors++
				}
			}
			releasedRows, releaseFacts := released(survivors)
			return statementEvaluation{releasedRows: releasedRows, releaseFacts: releaseFacts,
				evidenceRows: survivors,
				streamFacts: func(yield func(string) error) error {
					for rowID := int64(1); rowID <= ResultHeavyRows; rowID++ {
						if !survives(rowID) {
							continue
						}
						key, err := resultHeavyEntityKey(binding, rowID)
						if err != nil {
							return err
						}
						local := rowID
						if err := emitRowFacts(binding, key, func(column string) (string, string, error) {
							return ResultHeavyColumnValue(local, column)
						}, yield); err != nil {
							return err
						}
					}
					return nil
				}}, nil
		}}
}

// statementSpecifications is the closed-form evaluation table for every
// lowerable statement; specifications for statements the production chain
// refuses are never evaluated.
var statementSpecifications = map[string]statementSpecification{
	// q01: March expenses grouped by department -> three groups of two
	// visible columns.
	"q01": expenseSpec(2,
		func(row ExpenseRow) bool { return row.ExpenseDate >= "2026-03-01" && row.ExpenseDate < "2026-04-01" },
		func(rows []ExpenseRow) (int64, int64) {
			groups := groupCount(rows, func(row ExpenseRow) string { return row.Department })
			return groups, groups * 2
		}),
	// q02: department = 'Sales' never matches the Chinese department values.
	"q02": expenseSpec(3,
		func(row ExpenseRow) bool {
			return row.Department == "Sales" && row.ExpenseDate >= "2026-01-01" && row.ExpenseDate < "2027-01-01"
		},
		func(rows []ExpenseRow) (int64, int64) {
			groups := groupCount(rows, func(row ExpenseRow) string { return row.EmployeeNo })
			return groups, groups * 3
		}),
	"q03": expenseSpec(1,
		func(row ExpenseRow) bool { return row.Department == "Sales" },
		func(rows []ExpenseRow) (int64, int64) {
			released := int64(len(rows))
			if released > 10 {
				released = 10
			}
			return released, released * 4 // row fact + three cells
		}),
	// q04 (avg) is expected to be chain-refused; the specification exists so
	// an unexpected authorization fails loudly rather than silently.
	"q04": expenseSpec(0, func(ExpenseRow) bool { return false },
		func([]ExpenseRow) (int64, int64) { return 0, 0 }),
	// q05: HAVING SUM(amount) > 100000 excludes every department.
	"q05": expenseSpec(2,
		func(row ExpenseRow) bool { return row.ExpenseDate >= "2026-01-01" && row.ExpenseDate < "2027-01-01" },
		func(rows []ExpenseRow) (int64, int64) {
			totals := map[string]int64{}
			for _, row := range rows {
				totals[row.Department] += row.AmountCents
			}
			var groups int64
			for _, cents := range totals {
				if cents > 100000*100 {
					groups++
				}
			}
			return groups, groups * 2
		}),
	"q07": expenseSpec(3,
		func(row ExpenseRow) bool {
			return row.Department == "Finance" && (row.Status == "rejected" || row.Status == "pending")
		},
		func(rows []ExpenseRow) (int64, int64) {
			released := int64(len(rows))
			return released, released * 5 // row fact + four cells
		}),
	"q08": expenseSpec(0, func(ExpenseRow) bool { return true },
		func(rows []ExpenseRow) (int64, int64) {
			groups := groupCount(rows, func(row ExpenseRow) string { return row.City })
			return groups, groups * 3
		}),
	"q11": expenseSpec(0, func(ExpenseRow) bool { return true },
		func(rows []ExpenseRow) (int64, int64) {
			groups := groupCount(rows, func(row ExpenseRow) string { return row.Department })
			return groups, groups * 2
		}),
	// q12: HAVING SUM(amount) > 20000 excludes every employee.
	"q12": expenseSpec(2,
		func(row ExpenseRow) bool { return row.ExpenseDate >= "2026-01-01" && row.ExpenseDate < "2027-01-01" },
		func(rows []ExpenseRow) (int64, int64) {
			totals := map[string]int64{}
			for _, row := range rows {
				totals[row.EmployeeNo] += row.AmountCents
			}
			var groups int64
			for _, cents := range totals {
				if cents > 20000*100 {
					groups++
				}
			}
			return groups, groups * 3
		}),
	"q13": expenseSpec(0, func(ExpenseRow) bool { return true },
		func(rows []ExpenseRow) (int64, int64) {
			groups := groupCount(rows, func(row ExpenseRow) string { return row.Department })
			return groups, groups * 4
		}),
	// q15: HAVING SUM(amount) > 5000 excludes every (department, type) group.
	"q15": expenseSpec(1, func(ExpenseRow) bool { return true },
		func(rows []ExpenseRow) (int64, int64) {
			totals := map[string]int64{}
			for _, row := range rows {
				totals[row.Department+"\x00"+row.ExpenseType] += row.AmountCents
			}
			var groups int64
			for _, cents := range totals {
				if cents > 5000*100 {
					groups++
				}
			}
			return groups, groups * 3
		}),
	"q16": summarySpec(2,
		func(row SummaryRow) bool {
			return row.Department == "Sales" && len(row.Month) >= 5 && row.Month[:5] == "2026-"
		},
		func(rows []SummaryRow) (int64, int64) {
			groups := groupCount(rows, func(row SummaryRow) string { return row.Month })
			return groups, groups * 3
		}),
	"q17": summarySpec(1,
		func(row SummaryRow) bool { return row.Month == "2026-06" },
		func(rows []SummaryRow) (int64, int64) {
			groups := groupCount(rows, func(row SummaryRow) string { return row.Department })
			if groups > 1 {
				groups = 1 // LIMIT 1
			}
			return groups, groups * 2
		}),
	"q18": summarySpec(0, func(SummaryRow) bool { return true },
		func(rows []SummaryRow) (int64, int64) {
			groups := groupCount(rows, func(row SummaryRow) string { return row.Department })
			return groups, groups * 3
		}),
	// q21: all orders grouped by the three statuses.
	"q21": provsqlSpec("provsql_orders", 0,
		func(context evaluationContext, binding sourceBinding) (statementEvaluation, error) {
			return statementEvaluation{releasedRows: 3, releaseFacts: 3 * 2, evidenceRows: ProvSQLOrders,
				streamFacts: func(yield func(string) error) error {
					return streamOrders(binding, 1, ProvSQLOrders, yield)
				}}, nil
		}),
	// q22: lineitems of the first thousand orders, one global sum.
	"q22": provsqlSpec("provsql_lineitem", 1,
		func(context evaluationContext, binding sourceBinding) (statementEvaluation, error) {
			return statementEvaluation{releasedRows: 1, releaseFacts: 1, evidenceRows: 1000 * ProvSQLLinesPerOrder,
				streamFacts: func(yield func(string) error) error {
					return streamLineitems(binding, 1, 1000, yield)
				}}, nil
		}),
	// q23: the full join grouped by status; the companion carries both
	// sources' evidence fields for every joined row.
	"q23": {products: []string{"provsql_orders", "provsql_lineitem"}, expectKind: "join", predicateAtoms: 0,
		evaluate: func(context evaluationContext) (statementEvaluation, error) {
			orders, err := bindSource(context, "provsql_orders")
			if err != nil {
				return statementEvaluation{}, err
			}
			lineitem, err := bindSource(context, "provsql_lineitem")
			if err != nil {
				return statementEvaluation{}, err
			}
			return statementEvaluation{releasedRows: 3, releaseFacts: 3 * 2,
				evidenceRows: ProvSQLOrders * ProvSQLLinesPerOrder,
				streamFacts: func(yield func(string) error) error {
					if err := streamOrders(orders, 1, ProvSQLOrders, yield); err != nil {
						return err
					}
					return streamLineitems(lineitem, 1, ProvSQLOrders, yield)
				}}, nil
		}},
	// q25: HAVING COUNT(*) > 5 excludes every five-line order; the companion
	// still carries every lineitem row.
	"q25": provsqlSpec("provsql_lineitem", 0,
		func(context evaluationContext, binding sourceBinding) (statementEvaluation, error) {
			return statementEvaluation{releasedRows: 0, releaseFacts: 0,
				evidenceRows: ProvSQLOrders * ProvSQLLinesPerOrder,
				streamFacts: func(yield func(string) error) error {
					return streamLineitems(binding, 1, ProvSQLOrders, yield)
				}}, nil
		}),
	// q26: partition_key <= 3 keeps every row; one group.
	"q26": provsqlSpec("provsql_lineitem", 1,
		func(context evaluationContext, binding sourceBinding) (statementEvaluation, error) {
			return statementEvaluation{releasedRows: 1, releaseFacts: 1 * 2,
				evidenceRows: ProvSQLOrders * ProvSQLLinesPerOrder,
				streamFacts: func(yield func(string) error) error {
					return streamLineitems(binding, 1, ProvSQLOrders, yield)
				}}, nil
		}),
	// q28: extendedprice > 50000 matches nothing (the closed form tops out
	// near 1001), so the join companion is empty.
	"q28": {products: []string{"provsql_orders", "provsql_lineitem"}, expectKind: "join", predicateAtoms: 1,
		evaluate: func(context evaluationContext) (statementEvaluation, error) {
			return statementEvaluation{releasedRows: 0, releaseFacts: 0, evidenceRows: 0,
				streamFacts: func(func(string) error) error { return nil }}, nil
		}},
	// q29: category 'A' is not a closed-form category value.
	"q29": resultHeavySpec(2, func(rowID int64) bool {
		_, _, err := ResultHeavyColumnValue(rowID, "category")
		if err != nil {
			return false
		}
		return ResultHeavyCategory(rowID) == "A" && resultHeavyEventDateAtMost(rowID, "2026-01-31")
	}, func(survivors int64) (int64, int64) {
		return survivors, survivors * 5 // row fact + four cells
	}),
	// q30: active AND approved; LIMIT 100 releases one hundred sixteen-column
	// rows while the companion carries every survivor.
	"q30": resultHeavySpec(2, func(rowID int64) bool {
		return ResultHeavyActive(rowID) && ResultHeavyApproved(rowID)
	}, func(survivors int64) (int64, int64) {
		released := survivors
		if released > 100 {
			released = 100
		}
		return released, released * 17 // row fact + sixteen cells
	}),
	// q34 (LIKE) is expected to be chain-refused.
	"q34": resultHeavySpec(1, func(int64) bool { return false },
		func(int64) (int64, int64) { return 0, 0 }),
	// q35: six explicit row_ids, all sixteen columns.
	"q35": resultHeavySpec(6, func(rowID int64) bool {
		switch rowID {
		case 1, 2, 3, 5, 8, 13:
			return true
		}
		return false
	}, func(survivors int64) (int64, int64) {
		return survivors, survivors * 17
	}),
	"q36": expenseSpec(2,
		func(row ExpenseRow) bool { return row.ExpenseDate >= "2026-01-01" && row.ExpenseDate < "2027-01-01" },
		func(rows []ExpenseRow) (int64, int64) {
			groups := groupCount(rows, func(row ExpenseRow) string { return row.Department + "\x00" + row.ExpenseType })
			return groups, groups * 3
		}),
	"q37": expenseSpec(3,
		func(row ExpenseRow) bool {
			return row.Department == "Sales" && row.City == "Shanghai" && row.AmountCents > 2000*100
		},
		func(rows []ExpenseRow) (int64, int64) {
			released := int64(len(rows))
			return released, released * 11 // row fact + all ten cells
		}),
	"q38": expenseSpec(0, func(ExpenseRow) bool { return true },
		func(rows []ExpenseRow) (int64, int64) {
			groups := groupCount(rows, func(row ExpenseRow) string { return row.Status })
			return groups, groups * 3
		}),
}

// resultHeavyEventDateAtMost compares the closed-form event_date with an ISO
// bound (canonical d: form compares lexicographically).
func resultHeavyEventDateAtMost(rowID int64, bound string) bool {
	_, canonical, err := ResultHeavyColumnValue(rowID, "event_date")
	if err != nil {
		return false
	}
	return canonical[2:] <= bound
}
