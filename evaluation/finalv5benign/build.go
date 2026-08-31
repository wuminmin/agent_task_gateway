package finalv5benign

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// lowerabilityReport is the frozen agent-workload lowerability artifact; the
// corpus takes exactly its 27 lowerable statements, in question order.
type lowerabilityReport struct {
	Passes []struct {
		Variant string `json:"variant"`
		Results []struct {
			Query     string `json:"query"`
			SQLSHA256 string `json:"sql_sha256"`
			Lowerable bool   `json:"lowerable"`
		} `json:"results"`
	} `json:"passes"`
}

// BuildInput names the source-controlled inputs of the corpus build.
type BuildInput struct {
	// AgentWorkloadDir holds queries/qNN.sql and results.json.
	AgentWorkloadDir string
	// LiveCatalogPath is config/catalog.yaml: the classification authority.
	LiveCatalogPath string
}

// BuildManifest derives the frozen benign-trace corpus: per statement the
// production-chain classification and, for authorized statements, the exact
// a-priori footprints from the closed-form dataset models.
func BuildManifest(input BuildInput) (Manifest, error) {
	manifest := Manifest{SchemaVersion: SchemaVersion, CorpusID: CorpusID,
		WorkloadID: TraceWorkloadID, SourceDirectory: input.AgentWorkloadDir}
	reportBytes, err := os.ReadFile(filepath.Join(input.AgentWorkloadDir, "results.json"))
	if err != nil {
		return Manifest{}, err
	}
	var report lowerabilityReport
	if err := json.Unmarshal(reportBytes, &report); err != nil {
		return Manifest{}, err
	}
	if len(report.Passes) == 0 || report.Passes[0].Variant != "as-written" {
		return Manifest{}, errors.New("agent-workload lowerability report lacks the as-written pass")
	}
	live, err := catalog.Load(input.LiveCatalogPath)
	if err != nil {
		return Manifest{}, err
	}
	unionStream := newFactCollector()
	index := 0
	for _, entry := range report.Passes[0].Results {
		if !entry.Lowerable {
			continue
		}
		index++
		raw, err := os.ReadFile(filepath.Join(input.AgentWorkloadDir, "queries", entry.Query+".sql"))
		if err != nil {
			return Manifest{}, err
		}
		digest := sha256.Sum256(raw)
		if hex.EncodeToString(digest[:]) != entry.SQLSHA256 {
			return Manifest{}, fmt.Errorf("statement %s drifted from the frozen lowerability report", entry.Query)
		}
		statement := Statement{Index: index, ID: entry.Query, SQL: string(raw), SQLSHA256: entry.SQLSHA256}
		if err := classifyAndEvaluate(&statement, live, unionStream); err != nil {
			return Manifest{}, fmt.Errorf("statement %s: %w", entry.Query, err)
		}
		manifest.Statements = append(manifest.Statements, statement)
	}
	if index == 0 {
		return Manifest{}, errors.New("no lowerable statements found")
	}
	return finishManifest(manifest, unionStream)
}

func finishManifest(manifest Manifest, union *factCollector) (Manifest, error) {
	var atoms int64
	for _, statement := range manifest.Statements {
		switch statement.Classification {
		case ClassPolicyRefused:
			manifest.PolicyRefused++
			continue
		case ClassZeroRelease:
			manifest.ZeroRelease++
		}
		manifest.AuthorizedStatements++
		manifest.TotalReleaseFacts += statement.ReleaseFacts
		if statement.Dependency.Cardinality > manifest.MaxDependencyFacts {
			manifest.MaxDependencyFacts = statement.Dependency.Cardinality
		}
		atoms += statement.PredicateAtoms
	}
	// The recipe's Dependency budget is the maximum single-statement
	// footprint. Cumulative accounting is set-valued, so the trace union is
	// the true requirement; the corpus records both, and a recipe run that
	// refuses is exactly the recipe omission the study reports.
	unionSummary, err := union.summarize("dependency-union")
	if err != nil {
		return Manifest{}, err
	}
	outcome := manifest.AuthorizedStatements + atoms
	recipe := RecipeBudget{Name: "benign-recipe", Multiplier: 1,
		MaxReleaseFacts: maxInt64(manifest.TotalReleaseFacts, 1),
		MaxInfluence:    maxInt64(manifest.MaxDependencyFacts, 1),
		MaxOutcome:      maxInt64(outcome, 1),
		MaxQueries:      maxInt64(4*outcome, int64(len(manifest.Statements)))}
	manifest.Budgets = []RecipeBudget{recipe,
		scaledBudget(recipe, "benign-x2", 2), scaledBudget(recipe, "benign-x4", 4)}
	manifest.Budgets[0].Name = "benign-recipe"
	manifest.TraceUnionDependencyFacts = unionSummary.Cardinality
	manifest.TraceUnionDependencySHA256 = unionSummary.SetSHA256
	return manifest, nil
}

func scaledBudget(base RecipeBudget, name string, multiplier int64) RecipeBudget {
	return RecipeBudget{Name: name, Multiplier: multiplier,
		MaxReleaseFacts: base.MaxReleaseFacts * multiplier,
		MaxInfluence:    base.MaxInfluence * multiplier,
		MaxOutcome:      base.MaxOutcome * multiplier,
		MaxQueries:      base.MaxQueries * multiplier}
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

// classifyAndEvaluate runs the production admission chain and, when the
// statement authorizes, the statement's closed-form evaluation.
func classifyAndEvaluate(statement *Statement, live *catalog.Catalog, union *factCollector) error {
	specification, known := statementSpecifications[statement.ID]
	if !known {
		return errors.New("statement has no closed-form specification")
	}
	statement.Products = append([]string(nil), specification.products...)
	products := map[string]queryplan.Product{}
	grant := sqlpolicy.Grant{}
	for _, name := range specification.products {
		product, found := live.LookupProduct(name)
		if !found {
			return fmt.Errorf("live catalog lacks product %q", name)
		}
		approved := map[string]struct{}{}
		var columns []string
		for _, field := range product.Fields {
			approved[field.Name] = struct{}{}
			columns = append(columns, field.Name)
		}
		products[name] = physicalquery.QueryProductFromCatalog(product, approved)
		parts := strings.Split(product.ReportingView, ".")
		if len(parts) != 2 {
			return fmt.Errorf("product %q reporting view is not schema-qualified", name)
		}
		grant.Products = append(grant.Products, sqlpolicy.ProductGrant{
			LogicalName: name, PhysicalSchema: parts[0], PhysicalView: parts[1],
			ApprovedColumns: columns, AllowedFunctions: append([]string(nil), product.AllowedFunctions...),
			AllowedAggregates: append([]string(nil), product.AllowedAggregates...),
			AllowedOperators:  append([]string(nil), product.AllowedOperators...),
		})
	}
	lowered, err := sqllowering.Lower(statement.SQL, products)
	if err != nil {
		var typed *sqllowering.Error
		if errors.As(err, &typed) {
			statement.Classification = ClassPolicyRefused
			statement.PolicyCode, statement.PolicyReason = typed.Code, typed.Reason
			return nil
		}
		return err
	}
	var visibleSQL, provenanceSQL string
	var evidenceFields []string
	var compilationForEval queryplan.RelationalCompilation
	if lowered.Plan.From == nil {
		product, found := products[lowered.Plan.Product]
		if !found {
			return fmt.Errorf("lowered to unapproved product %q", lowered.Plan.Product)
		}
		visibleSQL, err = queryplan.Compile(lowered.Plan, product)
	} else {
		var compiled queryplan.RelationalCompilation
		compiled, err = queryplan.CompileRelational(lowered.Plan, products)
		if err == nil {
			visibleSQL, provenanceSQL = compiled.VisibleSQL, compiled.ProvenanceSQL
			evidenceFields = append([]string(nil), compiled.ProvenanceFields...)
			compilationForEval = compiled
			if specErr := specification.checkCompilation(compiled); specErr != nil {
				return specErr
			}
		}
	}
	if err != nil {
		statement.Classification = ClassPolicyRefused
		statement.PolicyCode = "QUERY_PLAN_COMPILE_REJECTED"
		statement.PolicyReason = firstErrorToken(err)
		return nil
	}
	engine := sqlpolicy.New(sqlpolicy.Config{})
	if _, err := engine.Authorize(sqlpolicy.Request{SQL: visibleSQL, Grant: grant, RowLimit: 1_000_000}); err != nil {
		statement.Classification = ClassPolicyRefused
		statement.PolicyCode, statement.PolicyReason = policyCode(err), ""
		return nil
	}
	if provenanceSQL != "" {
		if _, err := engine.Authorize(sqlpolicy.Request{SQL: provenanceSQL, Grant: grant, RowLimit: 1_000_000}); err != nil {
			statement.Classification = ClassPolicyRefused
			statement.PolicyCode, statement.PolicyReason = policyCode(err), "companion"
			return nil
		}
	}
	statement.EvidenceFields = evidenceFields
	evaluation, err := specification.evaluate(evaluationContext{live: live, compiled: compilationForEval})
	if err != nil {
		return err
	}
	statement.ReleasedRows = evaluation.releasedRows
	statement.ReleaseFacts = evaluation.releaseFacts
	statement.EvidenceRows = evaluation.evidenceRows
	statement.PredicateAtoms = specification.predicateAtoms
	if evaluation.evidenceRows == 0 {
		statement.Classification = ClassZeroRelease
		statement.Dependency = SetCommitment{}
		return nil
	}
	statement.Classification = ClassReleased
	collector := newFactCollector()
	if err := evaluation.streamFacts(func(hash string) error {
		if err := collector.add(hash); err != nil {
			return err
		}
		return union.add(hash)
	}); err != nil {
		return err
	}
	summary, err := collector.summarize("dependency")
	if err != nil {
		return err
	}
	statement.Dependency = SetCommitment{Cardinality: summary.Cardinality, SetSHA256: summary.SetSHA256}
	return nil
}

func policyCode(err error) string {
	var typed *sqlpolicy.PolicyError
	if errors.As(err, &typed) {
		return string(typed.Code)
	}
	return "SQL_POLICY_REJECTED"
}

func firstErrorToken(err error) string {
	text := err.Error()
	if len(text) > 120 {
		text = text[:120]
	}
	return text
}

// factCollector buffers dependency fact hashes and summarizes them as an
// exact semantic set through the oracle's bounded external sorter.
type factCollector struct{ hashes []string }

func newFactCollector() *factCollector { return &factCollector{} }

func (collector *factCollector) add(hash string) error {
	if len(hash) != 64 {
		return errors.New("dependency fact hash is malformed")
	}
	collector.hashes = append(collector.hashes, hash)
	return nil
}

func (collector *factCollector) summarize(role string) (finalv5oracle.StreamSetSummary, error) {
	stream := func(yield func(string) error) error {
		for _, hash := range collector.hashes {
			if err := yield(hash); err != nil {
				return err
			}
		}
		return nil
	}
	return finalv5oracle.SummarizeSemanticSet(role, stream, finalv5oracle.StreamSetOptions{})
}

// checkCompilation lets a specification pin structural expectations (for
// review; footprints never come from the compilation).
func (specification statementSpecification) checkCompilation(compiled queryplan.RelationalCompilation) error {
	if specification.expectKind != "" && compiled.Kind != specification.expectKind {
		return fmt.Errorf("compiled kind %q, specification expects %q", compiled.Kind, specification.expectKind)
	}
	return nil
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
