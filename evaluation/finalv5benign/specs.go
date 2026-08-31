package finalv5benign

import (
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// evaluationContext hands a specification the live catalog (fact namespaces
// and snapshots) plus the production compilation (per-source evidence
// fields). Footprint logic never parses SQL; it re-derives survivors from the
// closed-form dataset models.
type evaluationContext struct {
	live     *catalog.Catalog
	compiled queryplan.RelationalCompilation
}

// statementEvaluation is a statement's closed-form footprint.
type statementEvaluation struct {
	releasedRows int64
	releaseFacts int64
	evidenceRows int64
	streamFacts  func(yield func(string) error) error
}

type statementSpecification struct {
	products       []string
	expectKind     string
	predicateAtoms int64
	evaluate       func(evaluationContext) (statementEvaluation, error)
}

// sourceBinding resolves a product's fact identity and evidence fields.
type sourceBinding struct {
	product   string
	namespace string
	snapshot  string
	fields    []string
}

func bindSource(context evaluationContext, product string) (sourceBinding, error) {
	entry, found := context.live.LookupProduct(product)
	if !found {
		return sourceBinding{}, fmt.Errorf("live catalog lacks product %q", product)
	}
	binding := sourceBinding{product: product, namespace: entry.FactNamespace, snapshot: entry.Snapshot}
	for _, source := range context.compiled.Sources {
		if source.Product == product {
			binding.fields = append([]string(nil), source.EvidenceFields...)
		}
	}
	if len(binding.fields) == 0 {
		return sourceBinding{}, fmt.Errorf("compilation carries no evidence fields for %q", product)
	}
	return binding, nil
}

// column value lookup per product family.
type columnValueFunc func(column string) (sqlType, canonical string, err error)

// emitRowFacts yields the base-row fact plus one cell fact per evidence field.
func emitRowFacts(binding sourceBinding, entityKey string, value columnValueFunc,
	yield func(string) error) error {
	rowFact, err := finalv5oracle.BuildV2BaseRowFact(finalv5oracle.V2BaseRowInput{
		SourceNamespace: binding.namespace, Snapshot: binding.snapshot, EntityKey: entityKey})
	if err != nil {
		return err
	}
	if err := yield(rowFact.SHA256); err != nil {
		return err
	}
	for _, field := range binding.fields {
		sqlType, canonical, err := value(field)
		if err != nil {
			return err
		}
		fact, err := finalv5oracle.BuildV2BaseCellFact(finalv5oracle.V2BaseCellInput{
			SourceNamespace: binding.namespace, Snapshot: binding.snapshot, EntityKey: entityKey,
			Field: field, SQLType: sqlType, CanonicalValue: canonical})
		if err != nil {
			return err
		}
		if err := yield(fact.SHA256); err != nil {
			return err
		}
	}
	return nil
}

func entityKey(binding sourceBinding, components ...string) (string, error) {
	return finalv5oracle.ComposeOracleCanonicalKeyV2("base-entity",
		append([]string{binding.namespace}, components...)...)
}

// ---------------------------------------------------------------------------
// Expense family evaluation helpers.
// ---------------------------------------------------------------------------

func expenseEntityKey(binding sourceBinding, row ExpenseRow) (string, error) {
	return entityKey(binding, "receipt_no", "text", "s:"+row.ReceiptNo)
}

func summaryEntityKey(binding sourceBinding, row SummaryRow) (string, error) {
	return entityKey(binding,
		"month", "text", "s:"+row.Month,
		"department", "text", "s:"+row.Department,
		"expense_type", "text", "s:"+row.ExpenseType)
}

// expenseSpec evaluates a single-source expense_detail statement.
func expenseSpec(atoms int64, survives func(ExpenseRow) bool,
	released func(rows []ExpenseRow) (releasedRows, releaseFacts int64)) statementSpecification {
	return statementSpecification{products: []string{"expense_detail"}, expectKind: "scan", predicateAtoms: atoms,
		evaluate: func(context evaluationContext) (statementEvaluation, error) {
			binding, err := bindSource(context, "expense_detail")
			if err != nil {
				return statementEvaluation{}, err
			}
			var survivors []ExpenseRow
			for _, row := range ExpenseRows() {
				if survives(row) {
					survivors = append(survivors, row)
				}
			}
			releasedRows, releaseFacts := released(survivors)
			return statementEvaluation{releasedRows: releasedRows, releaseFacts: releaseFacts,
				evidenceRows: int64(len(survivors)),
				streamFacts: func(yield func(string) error) error {
					for _, row := range survivors {
						key, err := expenseEntityKey(binding, row)
						if err != nil {
							return err
						}
						local := row
						if err := emitRowFacts(binding, key, func(column string) (string, string, error) {
							return ExpenseColumnValue(local, column)
						}, yield); err != nil {
							return err
						}
					}
					return nil
				}}, nil
		}}
}

// summarySpec evaluates a single-source expense_summary statement.
func summarySpec(atoms int64, survives func(SummaryRow) bool,
	released func(rows []SummaryRow) (releasedRows, releaseFacts int64)) statementSpecification {
	return statementSpecification{products: []string{"expense_summary"}, expectKind: "scan", predicateAtoms: atoms,
		evaluate: func(context evaluationContext) (statementEvaluation, error) {
			binding, err := bindSource(context, "expense_summary")
			if err != nil {
				return statementEvaluation{}, err
			}
			var survivors []SummaryRow
			for _, row := range SummaryRows() {
				if survives(row) {
					survivors = append(survivors, row)
				}
			}
			releasedRows, releaseFacts := released(survivors)
			return statementEvaluation{releasedRows: releasedRows, releaseFacts: releaseFacts,
				evidenceRows: int64(len(survivors)),
				streamFacts: func(yield func(string) error) error {
					for _, row := range survivors {
						key, err := summaryEntityKey(binding, row)
						if err != nil {
							return err
						}
						local := row
						if err := emitRowFacts(binding, key, func(column string) (string, string, error) {
							return SummaryColumnValue(local, column)
						}, yield); err != nil {
							return err
						}
					}
					return nil
				}}, nil
		}}
}

// groupCount groups expense survivors by a key function.
func groupCount[Row any](rows []Row, key func(Row) string) int64 {
	seen := map[string]bool{}
	for _, row := range rows {
		seen[key(row)] = true
	}
	return int64(len(seen))
}

var errUnreachable = errors.New("specification evaluated for a statement the chain refused")
