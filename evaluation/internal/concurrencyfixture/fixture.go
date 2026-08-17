// Package concurrencyfixture freezes the exact final-V5 same-root workload.
// Every request still traverses the production Gateway, OA, Control
// PostgreSQL, Business PostgreSQL, V8 receipt, and result-artifact paths.
package concurrencyfixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	gatewayapp "taskbound.local/agent-data-gateway/internal/gateway"
)

const (
	Version              = "taskgate-final-v5-concurrency-fixture-v1"
	ProductName          = "final_v5_concurrency_expense_detail"
	PhysicalRelation     = "reporting.final_v5_concurrency_expense_detail"
	BudgetProfile        = "final-v5-concurrency-v1"
	ResourceMaxQueries   = int64(600)
	RootBudgetLimit      = int64(5)
	UsageBeforeBoundary  = int64(4)
	ExpectedFinalOutcome = int64(5)

	ContenderSQL = "SELECT receipt_no, expense_type FROM final_v5_concurrency_expense_detail WHERE department = '销售部' ORDER BY receipt_no ASC LIMIT 1"
	OverflowSQL  = "SELECT receipt_no, city FROM final_v5_concurrency_expense_detail WHERE department = '销售部' ORDER BY receipt_no ASC LIMIT 1"
)

var PrefixSQL = []string{
	"SELECT receipt_no FROM final_v5_concurrency_expense_detail WHERE department = '销售部' ORDER BY receipt_no ASC LIMIT 1",
	"SELECT receipt_no, department FROM final_v5_concurrency_expense_detail WHERE department = '销售部' ORDER BY receipt_no ASC LIMIT 1",
	"SELECT receipt_no, expense_type, city FROM final_v5_concurrency_expense_detail WHERE department = '销售部' ORDER BY receipt_no ASC LIMIT 1",
}

type Cell struct {
	WorkloadID string
	Scale      string
	Mode       string
	Width      int
}

type RoundIdentity struct {
	CampaignID       string
	DeploymentID     string
	ExperimentID     string
	CellID           string
	SampleID         string
	Iteration        int
	ProcessReplicate int
	PairID           string
	RootGroupID      string
}

var FrozenCells = []Cell{
	{WorkloadID: "shared-root", Scale: "10", Mode: "forced_queue_safety", Width: 10},
	{WorkloadID: "shared-root", Scale: "10", Mode: "natural_contention", Width: 10},
	{WorkloadID: "shared-root", Scale: "50", Mode: "forced_queue_safety", Width: 50},
	{WorkloadID: "shared-root", Scale: "50", Mode: "natural_contention", Width: 50},
	{WorkloadID: "shared-root", Scale: "100", Mode: "forced_queue_safety", Width: 100},
	{WorkloadID: "shared-root", Scale: "100", Mode: "natural_contention", Width: 100},
	{WorkloadID: "shared-root", Scale: "500", Mode: "forced_queue_safety", Width: 500},
	{WorkloadID: "shared-root", Scale: "500", Mode: "natural_contention", Width: 500},
	{WorkloadID: "serial-control", Scale: "1", Mode: "serial", Width: 1},
}

func Lookup(workloadID, scale, mode string) (Cell, bool) {
	for _, cell := range FrozenCells {
		if cell.WorkloadID == workloadID && cell.Scale == scale && cell.Mode == mode {
			return cell, true
		}
	}
	return Cell{}, false
}

func FixtureSHA256() string {
	return jsonSHA256(struct {
		Version      string   `json:"version"`
		Cells        []Cell   `json:"cells"`
		PrefixSQL    []string `json:"prefix_sql"`
		ContenderSQL string   `json:"contender_sql"`
		OverflowSQL  string   `json:"overflow_sql"`
		BudgetLimit  int64    `json:"budget_limit"`
		UsageBefore  int64    `json:"usage_before"`
		FinalOutcome int64    `json:"final_outcome"`
		Product      string   `json:"product"`
		Relation     string   `json:"relation"`
		Profile      string   `json:"profile"`
		MaxQueries   int64    `json:"max_queries"`
	}{
		Version: Version, Cells: FrozenCells, PrefixSQL: PrefixSQL,
		ContenderSQL: ContenderSQL, OverflowSQL: OverflowSQL,
		BudgetLimit: RootBudgetLimit, UsageBefore: UsageBeforeBoundary,
		FinalOutcome: ExpectedFinalOutcome, Product: ProductName, Relation: PhysicalRelation,
		Profile: BudgetProfile, MaxQueries: ResourceMaxQueries,
	})
}

func PlansSHA256() string {
	return jsonSHA256(struct {
		Prefix    []string `json:"prefix"`
		Contender string   `json:"contender"`
		Overflow  string   `json:"overflow"`
	}{PrefixSQL, ContenderSQL, OverflowSQL})
}

func ExpectedContenderRows() [][]any {
	return [][]any{{"TR-2026-0001", "机票"}}
}

func ExpectedContenderResultSHA256() string {
	rows := ExpectedContenderRows()
	encoded := make([][]byte, len(rows))
	for index, row := range rows {
		value, err := json.Marshal(row)
		if err != nil {
			panic(err)
		}
		encoded[index] = value
	}
	sort.Slice(encoded, func(i, j int) bool { return string(encoded[i]) < string(encoded[j]) })
	hash := sha256.New()
	for _, value := range encoded {
		_, _ = fmt.Fprintf(hash, "%d:", len(value))
		_, _ = hash.Write(value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func RoundSHA256(operation RoundIdentity) string {
	return SHA256String(strings.Join([]string{
		Version, "round", operation.CampaignID, operation.DeploymentID,
		operation.ExperimentID, operation.CellID, operation.SampleID,
		strconv.Itoa(operation.Iteration), strconv.Itoa(operation.ProcessReplicate),
		operation.PairID, operation.RootGroupID,
	}, "\x00"))
}

func ParticipantSHA256(roundSHA256 string, index int) string {
	if index < 1 {
		panic("concurrency participant index must be positive")
	}
	return SHA256String(Version + "\x00participant\x00" + roundSHA256 + "\x00" + strconv.Itoa(index))
}

func ParticipantSHA256s(roundSHA256 string, width int) []string {
	result := make([]string, width)
	for index := range result {
		result[index] = ParticipantSHA256(roundSHA256, index+1)
	}
	return result
}

func ParticipantSetSHA256(roundSHA256 string, width int) string {
	return gatewayapp.ConcurrencyParticipantSetSHA256(ParticipantSHA256s(roundSHA256, width))
}

func RequestID(operation RoundIdentity, phase string, index int) string {
	if strings.TrimSpace(phase) == "" || index < 0 {
		panic("invalid concurrency request identity")
	}
	return "final-v5-concurrency-" + SHA256String(strings.Join([]string{
		Version, operation.SampleID, operation.CellID, phase, strconv.Itoa(index),
	}, "\x00"))[:32]
}

func SHA256String(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func jsonSHA256(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return SHA256String(string(encoded))
}

func Validate() error {
	if len(FrozenCells) != 9 || len(PrefixSQL) != 3 {
		return fmt.Errorf("concurrency fixture matrix or prefix is incomplete")
	}
	seen := map[Cell]bool{}
	for _, cell := range FrozenCells {
		if seen[cell] || cell.Width < 1 || cell.Scale != strconv.Itoa(cell.Width) {
			return fmt.Errorf("invalid concurrency fixture cell: %+v", cell)
		}
		seen[cell] = true
	}
	seenSQL := map[string]bool{}
	for _, sqlText := range append(append([]string(nil), PrefixSQL...), ContenderSQL, OverflowSQL) {
		upper := strings.ToUpper(strings.TrimSpace(sqlText))
		if !strings.HasPrefix(upper, "SELECT ") || strings.Contains(sqlText, ";") || seenSQL[upper] {
			return fmt.Errorf("concurrency fixture contains non-read-only or duplicate canonical SQL")
		}
		seenSQL[upper] = true
	}
	if ExpectedContenderResultSHA256() == "" || FixtureSHA256() == "" || PlansSHA256() == "" {
		return fmt.Errorf("concurrency fixture digest is absent")
	}
	return nil
}

func SortedCells() []Cell {
	result := append([]Cell(nil), FrozenCells...)
	sort.Slice(result, func(i, j int) bool {
		left := result[i].WorkloadID + "\x00" + result[i].Scale + "\x00" + result[i].Mode
		right := result[j].WorkloadID + "\x00" + result[j].Scale + "\x00" + result[j].Mode
		return left < right
	})
	return result
}
