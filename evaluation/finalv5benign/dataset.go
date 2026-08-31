// Package finalv5benign owns the (c3) benign agent-workload corpus: the 27
// lowerable statements of evaluation/agentworkload executed as one benign
// trace, with a-priori per-statement classification and exact footprints
// derived from closed-form models of the three deterministic dataset
// families. It mirrors, formula for formula, the reviewed generators:
// db/init/00-schema.sql (frozen ten-row travel demo),
// db/init/01-final-v5-provsql-fixture.sql (closed-form orders/lineitem), and
// sql/datasets/benchmark-v1-generate.sql (closed-form result_heavy).
// Canonical value strings use the production FactID encoding
// (internal/exposure/fact.go): "i:", "n:", "s:", "d:", "b:", "ts:".
package finalv5benign

import (
	"fmt"
	"math/big"
)

// ---------------------------------------------------------------------------
// Expense family: the frozen ten-row travel demo, as reporting.expense_detail
// projects it (receipts joined to employees on employee_no).
// ---------------------------------------------------------------------------

// ExpenseRow is one reporting.expense_detail row.
type ExpenseRow struct {
	ReceiptNo    string
	EmployeeNo   string
	EmployeeName string
	Department   string
	ExpenseDate  string // d: canonical form YYYY-MM-DD
	ExpenseType  string
	AmountCents  int64 // amount = AmountCents/100
	City         string
	Purpose      string
	Status       string
}

// ExpenseRows is the frozen dataset in receipt order.
func ExpenseRows() []ExpenseRow {
	return []ExpenseRow{
		{"TR-2026-0001", "E001", "张伟", "销售部", "2026-01-08", "机票", 168000, "北京", "客户拜访", "approved"},
		{"TR-2026-0002", "E002", "李娜", "销售部", "2026-01-19", "酒店", 88000, "上海", "展会支持", "approved"},
		{"TR-2026-0003", "E001", "张伟", "销售部", "2026-02-11", "高铁", 55300, "杭州", "客户培训", "approved"},
		{"TR-2026-0004", "E002", "李娜", "销售部", "2026-02-21", "餐饮", 32000, "深圳", "商务洽谈", "approved"},
		{"TR-2026-0005", "E003", "王强", "研发部", "2026-01-12", "机票", 145000, "成都", "技术交流", "approved"},
		{"TR-2026-0006", "E003", "王强", "研发部", "2026-03-03", "酒店", 126000, "武汉", "项目交付", "approved"},
		{"TR-2026-0007", "E004", "赵敏", "财务部", "2026-03-15", "高铁", 42000, "南京", "财务培训", "approved"},
		{"TR-2026-0008", "E001", "张伟", "销售部", "2026-03-18", "酒店", 96000, "广州", "渠道会议", "pending"},
		{"TR-2026-0009", "E002", "李娜", "销售部", "2026-03-20", "机票", 191000, "北京", "年度签约", "approved"},
		{"TR-2026-0010", "E003", "王强", "研发部", "2026-04-02", "餐饮", 28000, "上海", "项目复盘", "rejected"},
	}
}

// Amount is the row's exact amount.
func (row ExpenseRow) Amount() *big.Rat { return new(big.Rat).SetFrac64(row.AmountCents, 100) }

// Month is to_char(date_trunc('month', expense_date), 'YYYY-MM').
func (row ExpenseRow) Month() string { return row.ExpenseDate[:7] }

// ExpenseColumnValue returns one expense_detail column's canonical value.
func ExpenseColumnValue(row ExpenseRow, column string) (sqlType, canonical string, err error) {
	switch column {
	case "receipt_no":
		return "text", "s:" + row.ReceiptNo, nil
	case "employee_no":
		return "text", "s:" + row.EmployeeNo, nil
	case "employee_name":
		return "text", "s:" + row.EmployeeName, nil
	case "department":
		return "text", "s:" + row.Department, nil
	case "expense_date":
		return "date", "d:" + row.ExpenseDate, nil
	case "expense_type":
		return "text", "s:" + row.ExpenseType, nil
	case "amount":
		return "numeric", "n:" + row.Amount().RatString(), nil
	case "city":
		return "text", "s:" + row.City, nil
	case "purpose":
		return "text", "s:" + row.Purpose, nil
	case "status":
		return "text", "s:" + row.Status, nil
	default:
		return "", "", fmt.Errorf("column %q is not part of expense_detail", column)
	}
}

// SummaryRow is one reporting.expense_summary group.
type SummaryRow struct {
	Month        string
	Department   string
	ExpenseType  string
	TotalCents   int64
	RequestCount int64
}

// SummaryRows derives expense_summary exactly as the view does, ordered by
// (month, department, expense_type) for determinism.
func SummaryRows() []SummaryRow {
	groups := map[[3]string]*SummaryRow{}
	var order [][3]string
	for _, row := range ExpenseRows() {
		key := [3]string{row.Month(), row.Department, row.ExpenseType}
		group, seen := groups[key]
		if !seen {
			group = &SummaryRow{Month: key[0], Department: key[1], ExpenseType: key[2]}
			groups[key] = group
			order = append(order, key)
		}
		group.TotalCents += row.AmountCents
		group.RequestCount++
	}
	sortKeys(order)
	rows := make([]SummaryRow, 0, len(order))
	for _, key := range order {
		rows = append(rows, *groups[key])
	}
	return rows
}

func sortKeys(keys [][3]string) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && lessKey(keys[j], keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}

func lessKey(left, right [3]string) bool {
	for i := 0; i < 3; i++ {
		if left[i] != right[i] {
			return left[i] < right[i]
		}
	}
	return false
}

// SummaryColumnValue returns one expense_summary column's canonical value.
func SummaryColumnValue(row SummaryRow, column string) (sqlType, canonical string, err error) {
	switch column {
	case "month":
		return "text", "s:" + row.Month, nil
	case "department":
		return "text", "s:" + row.Department, nil
	case "expense_type":
		return "text", "s:" + row.ExpenseType, nil
	case "total_amount":
		return "numeric", "n:" + new(big.Rat).SetFrac64(row.TotalCents, 100).RatString(), nil
	case "request_count":
		return "bigint", fmt.Sprintf("i:%d", row.RequestCount), nil
	default:
		return "", "", fmt.Errorf("column %q is not part of expense_summary", column)
	}
}

// ---------------------------------------------------------------------------
// ProvSQL family: closed-form orders (50,000) and lineitem (250,000).
// ---------------------------------------------------------------------------

const (
	ProvSQLOrders           int64 = 50000
	ProvSQLLinesPerOrder    int64 = 5
	ProvSQLNonces           int64 = 1000
	ProvSQLPartitionKey     int64 = 1
	provSQLLineitemModulus  int64 = 100000
	provSQLLineitemFloorAdd int64 = 100
)

// OrderStatus is (orderkey % 3).
func OrderStatus(orderKey int64) int64 { return orderKey % 3 }

// LineExtendedPrice is ((((orderkey*13)+(linenumber*7)) % 100000) + 100)::numeric/100.
func LineExtendedPrice(orderKey, lineNumber int64) *big.Rat {
	return new(big.Rat).SetFrac64(((orderKey*13+lineNumber*7)%provSQLLineitemModulus)+provSQLLineitemFloorAdd, 100)
}

// OrdersColumnValue returns one provsql_orders column's canonical value.
func OrdersColumnValue(orderKey int64, column string) (sqlType, canonical string, err error) {
	switch column {
	case "orderkey":
		return "bigint", fmt.Sprintf("i:%d", orderKey), nil
	case "status":
		return "bigint", fmt.Sprintf("i:%d", OrderStatus(orderKey)), nil
	case "partition_key":
		return "integer", fmt.Sprintf("i:%d", ProvSQLPartitionKey), nil
	default:
		return "", "", fmt.Errorf("column %q is not part of provsql_orders", column)
	}
}

// LineitemColumnValue returns one provsql_lineitem column's canonical value.
func LineitemColumnValue(orderKey, lineNumber int64, column string) (sqlType, canonical string, err error) {
	switch column {
	case "orderkey":
		return "bigint", fmt.Sprintf("i:%d", orderKey), nil
	case "linenumber":
		return "integer", fmt.Sprintf("i:%d", lineNumber), nil
	case "extendedprice":
		return "numeric", "n:" + LineExtendedPrice(orderKey, lineNumber).RatString(), nil
	case "partition_key":
		return "integer", fmt.Sprintf("i:%d", ProvSQLPartitionKey), nil
	default:
		return "", "", fmt.Errorf("column %q is not part of provsql_lineitem", column)
	}
}

// ---------------------------------------------------------------------------
// result_heavy family: the full sixteen-column closed form. The five columns
// the refused-footprint ladder models keep their formulas here verbatim; the
// two packages are checked against each other in the corpus test.
// ---------------------------------------------------------------------------

const ResultHeavyRows int64 = 100000

var resultHeavyCategories = [4]string{"alpha", "beta", "gamma", "delta"}
var resultHeavyRegions = [5]string{"north", "south", "east", "west", "central"}

// ResultHeavyColumnValue returns one final_v5_result_heavy column's canonical
// value, mirroring sql/datasets/benchmark-v1-generate.sql formula for formula.
func ResultHeavyColumnValue(rowID int64, column string) (sqlType, canonical string, err error) {
	switch column {
	case "row_id":
		return "bigint", fmt.Sprintf("i:%d", rowID), nil
	case "category":
		return "text", "s:" + resultHeavyCategories[(rowID-1)%4], nil
	case "amount":
		return "numeric", "n:" + new(big.Rat).SetFrac64((rowID*7919)%100000000, 100).RatString(), nil
	case "event_date":
		return "date", "d:" + addDays20200101((rowID-1)%3653), nil
	case "sequence_no":
		return "integer", fmt.Sprintf("i:%d", rowID%1000000), nil
	case "approved":
		return "boolean", "b:" + boolString(rowID%3 != 0), nil
	case "event_timestamp":
		seconds := rowID - 1
		micros := (rowID - 1) % 1000
		return "timestamp", "ts:" + timestamp20200101(seconds, micros), nil
	case "description":
		return "text", fmt.Sprintf("s:artifact-row-%d", rowID), nil
	case "quantity":
		return "bigint", fmt.Sprintf("i:%d", 1+((rowID-1)%10000)), nil
	case "unit_price":
		return "numeric", "n:" + new(big.Rat).SetFrac64((rowID*104729)%10000000, 10000).RatString(), nil
	case "tax_amount":
		value := new(big.Rat).SetFrac64((rowID*37)%1000000, 100)
		if rowID%11 == 0 {
			value.Neg(value)
		}
		return "numeric", "n:" + value.RatString(), nil
	case "settled_date":
		return "date", "d:" + addDays20200101((rowID-1+31)%3653), nil
	case "processed_at":
		return "timestamp", "ts:" + timestampNoon20200101((rowID-1)*60), nil
	case "region":
		return "text", "s:" + resultHeavyRegions[(rowID-1)%5], nil
	case "revision":
		return "integer", fmt.Sprintf("i:%d", (rowID-1)%97), nil
	case "active":
		return "boolean", "b:" + boolString(rowID%7 != 0), nil
	default:
		return "", "", fmt.Errorf("column %q is not part of final_v5_result_heavy", column)
	}
}

// ResultHeavyApproved is (row_id % 3) <> 0.
func ResultHeavyApproved(rowID int64) bool { return rowID%3 != 0 }

// ResultHeavyActive is (row_id % 7) <> 0.
func ResultHeavyActive(rowID int64) bool { return rowID%7 != 0 }

// ResultHeavyCategory mirrors the generator's category array.
func ResultHeavyCategory(rowID int64) string { return resultHeavyCategories[(rowID-1)%4] }

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
