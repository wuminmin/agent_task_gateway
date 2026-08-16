package main

import (
	"fmt"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// The Contract Index is one runtime shared by every family. Baseline reads the
// same loaded instance the Artifact gate already revalidated rather than
// opening a second one that could drift from it.
var (
	baselineContractRuntime    = artifactContractRuntime
	baselineContractRuntimeErr = artifactContractRuntimeErr
)

// baselineImplementedWorkloads names the Baseline workloads whose execution
// path exists. It is deliberately not the whole contract: S3, S4 and S5 have
// frozen cells but no implementation, and advertising them would turn an unrun
// cell into a claimed capability. S6 joined the list because its Product, route
// and templates are the ones the Artifact cells already execute end to end.
var baselineImplementedWorkloads = map[string]bool{"S1": true, "S2": true, "S6": true}

// baselineProductBinding is how one frozen Product reaches a real deployment:
// the columns the OA approves, and the two relations the Observer counts calls
// against. The approved column list is the Product's complete field list rather
// than only the fields the frozen template names, because the ordinal companion
// is keyed on the Product's entity key and the mandatory partition scope is
// what bounds the exposure, not the projection.
type baselineProductBinding struct {
	Columns           []string
	VisibleRelation   string
	CompanionRelation string
}

var baselineProductBindings = map[string]baselineProductBinding{
	"provsql_orders": {
		Columns:           []string{"orderkey", "status", "partition_key"},
		VisibleRelation:   "reporting.provsql_orders",
		CompanionRelation: "taskgate_ordinal.provsql_orders_v1",
	},
	"provsql_lineitem": {
		Columns:           []string{"orderkey", "linenumber", "extendedprice", "partition_key"},
		VisibleRelation:   "reporting.provsql_lineitem",
		CompanionRelation: "taskgate_ordinal.provsql_lineitem_v1",
	},
	// S6 shares this Product and these templates with the Artifact cells, which
	// have already executed them end to end. Its sixteen fields are the frozen
	// x16 projection; the x4 cells select the first four of them.
	"final_v5_result_heavy": {
		Columns: []string{
			"row_id", "category", "amount", "event_date", "sequence_no", "approved",
			"event_timestamp", "description", "quantity", "unit_price", "tax_amount",
			"settled_date", "processed_at", "region", "revision", "active",
		},
		VisibleRelation:   "reporting.final_v5_result_heavy",
		CompanionRelation: "taskgate_ordinal.final_v5_result_heavy_v1",
	},
}

// baselinePartitionScope is the mandatory scope every frozen Baseline cell runs
// under. config/catalog.yaml declares partition_key as an enum whose only
// allowed value is "1", and both arm templates filter on it, so a task that
// omitted it could not execute the frozen query at all.
var baselinePartitionScope = map[string][]string{"partition_key": {"1"}}

// baselineScopesFor returns the mandatory scope a cell's Products declare. S6's
// result-heavy Product is scoped by category over the complete frozen domain,
// not by partition key, and its templates filter on exactly those four values.
func baselineScopesFor(productIDs []string) map[string][]string {
	for _, product := range productIDs {
		if product == "final_v5_result_heavy" {
			return map[string][]string{"category": {"alpha", "beta", "gamma", "delta"}}
		}
	}
	return baselinePartitionScope
}

// baselineExecutionCell is one frozen cell resolved all the way to the bytes a
// measured sample runs: the contract coordinate, the approved task, and the
// three rendered SQL strings the modes need.
type baselineExecutionCell struct {
	Contract   finalv5contracts.BaselineCell
	Task       boundTaskRequest
	DirectSQL  string
	BDGSQL     string
	RewriteSQL string
}

// resolveBaselineExecutionCell turns one AdapterOperation into the frozen cell
// it names. It fails closed: an unknown workload, an unrenderable parameter or
// an unbound Product all produce an error rather than a substituted default.
func resolveBaselineExecutionCell(operation experiment.AdapterOperation) (baselineExecutionCell, error) {
	if baselineContractRuntimeErr != nil {
		return baselineExecutionCell{}, fmt.Errorf("contract runtime: %w", baselineContractRuntimeErr)
	}
	if !baselineImplementedWorkloads[operation.WorkloadID] {
		return baselineExecutionCell{}, fmt.Errorf("baseline workload %q has no implementation", operation.WorkloadID)
	}
	contract, err := baselineContractRuntime.BaselineCell(operation.WorkloadID, operation.Scale, operation.Mode)
	if err != nil {
		return baselineExecutionCell{}, err
	}
	rendered, err := baselineContractRuntime.BaselineQueryContract(contract)
	if err != nil {
		return baselineExecutionCell{}, err
	}
	task := boundTaskRequest{
		Objective:    "Final V5 baseline " + contract.Identity.String(),
		DataProducts: append([]string(nil), contract.ProductIDs...),
		Columns:      map[string][]string{},
		Scopes:       baselineScopesFor(contract.ProductIDs),
	}
	for _, product := range contract.ProductIDs {
		binding, bound := baselineProductBindings[product]
		if !bound {
			return baselineExecutionCell{}, fmt.Errorf("baseline cell %s binds unbound Product %q", contract.Identity, product)
		}
		task.Columns[product] = append([]string(nil), binding.Columns...)
	}
	// The Observer counts calls against one visible relation and one companion.
	// A multi-Product cell binds the relation its query drives from -- S2 filters
	// and groups on orders and joins lineitem to it -- so a replay that touched
	// the database at all would still register here. It is a detector of
	// database contact, not a complete call census across both relations.
	driving := baselineProductBindings[contract.ProductIDs[0]]
	task.VisibleRelation, task.CompanionRelation = driving.VisibleRelation, driving.CompanionRelation
	if err := validateBoundTask(task); err != nil {
		return baselineExecutionCell{}, fmt.Errorf("baseline cell %s task: %w", contract.Identity, err)
	}
	return baselineExecutionCell{
		Contract: contract, Task: task,
		DirectSQL:  rendered.Direct.SQL,
		BDGSQL:     rendered.BDG.SQL,
		RewriteSQL: normalizedRewriteOf(rendered.BDG.SQL),
	}, nil
}

// normalizedRewriteOf produces a semantically identical rewrite of a frozen
// query whose bytes differ. The normalized_rewrite_replay mode exists to show
// that TaskGate recognises an equivalent query it has already settled, so the
// rewrite must change only layout: it collapses the template's line breaks and
// indentation into single spaces and pads the statement, touching no
// identifier, literal, operator or clause order.
func normalizedRewriteOf(sql string) string {
	return "  " + strings.Join(strings.Fields(sql), " ") + "  "
}

// baselineImplementedPublicationCellsFromContract lists the Baseline cells the
// Adapter can actually execute, derived from the frozen contract rather than
// from a second hand-kept table. A workload without an execution path
// contributes nothing, so the coverage gate keeps reporting Baseline
// incomplete until every frozen cell is real.
func baselineImplementedPublicationCellsFromContract() []publicationCell {
	if baselineContractRuntimeErr != nil {
		return nil
	}
	identities, err := baselineContractRuntime.BaselineRequirements()
	if err != nil {
		return nil
	}
	cells := make([]publicationCell, 0, len(identities))
	for _, identity := range identities {
		if !baselineImplementedWorkloads[identity.WorkloadID] {
			continue
		}
		operation := experiment.AdapterOperation{
			ExperimentID: finalv5contracts.BaselineExperimentID,
			WorkloadID:   identity.WorkloadID, Scale: identity.Scale, Mode: identity.Mode,
		}
		if _, err := resolveBaselineExecutionCell(operation); err != nil {
			continue
		}
		cells = append(cells, publicationCell{
			WorkloadID: identity.WorkloadID, Scale: identity.Scale, Mode: identity.Mode})
	}
	return cells
}
