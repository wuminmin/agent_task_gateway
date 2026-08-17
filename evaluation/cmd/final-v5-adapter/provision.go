package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/domain"
)

const delegatedApprovalTTLHeadroomSeconds int64 = 30

type provisionedBudget struct {
	MaxQueries             int64                              `json:"max_queries"`
	MaxRows                int64                              `json:"max_rows"`
	MaxDBMS                int64                              `json:"max_db_ms"`
	QueryTimeoutMS         int64                              `json:"query_timeout_ms"`
	TaskTTLSeconds         int64                              `json:"task_ttl_seconds"`
	MaxReleaseFacts        int64                              `json:"max_release_facts"`
	MaxInfluenceFacts      int64                              `json:"max_influence_facts"`
	MaxOutcomeFacts        int64                              `json:"max_outcome_facts"`
	ExposureProfileVersion string                             `json:"exposure_profile_version"`
	PredicateFootprint     *domain.PredicateFootprintLimitsV1 `json:"predicate_footprint"`
}

type provisionedTask struct {
	TaskID        string            `json:"task_id"`
	OAURL         string            `json:"oa_url"`
	RootTaskID    string            `json:"root_task_id"`
	ParentTaskID  string            `json:"parent_task_id"`
	BudgetProfile string            `json:"budget_profile"`
	Budget        provisionedBudget `json:"budget"`
}

// provisionExpenseTask is the shared real OA path for source-controlled
// expense-publication workloads. It never writes Control directly and cannot
// widen the Catalog or signed grant.
func (adapter *realAdapter) provisionExpenseTask(ctx context.Context, operationObjective string, columns []string) (string, error) {
	created, err := adapter.provisionCatalogTask(ctx, operationObjective, "expense_detail", columns, "")
	if err != nil {
		return "", err
	}
	return created.TaskID, nil
}

// provisionCatalogTask exercises the real request/OA/Grant path and retains
// the server-selected product profile/root binding for evaluation evidence.
// parentTaskID creates an ordinary delegated task; Control remains the sole
// authority for its shared root and intersected budget.
func (adapter *realAdapter) provisionCatalogTask(ctx context.Context, operationObjective, product string,
	columns []string, parentTaskID string) (provisionedTask, error) {
	if strings.TrimSpace(operationObjective) == "" || len(columns) == 0 {
		return provisionedTask{}, errors.New("task objective and approved columns are required")
	}
	if strings.TrimSpace(product) == "" {
		return provisionedTask{}, errors.New("task product is required")
	}
	products := []string{product}
	approvedColumns := map[string][]string{product: append([]string(nil), columns...)}
	mandatoryScope := map[string]any{"department": []string{"销售部"}}
	var created provisionedTask
	arguments := map[string]any{
		"objective": operationObjective, "data_products": products,
		"columns": approvedColumns,
		"scopes":  mandatoryScope,
	}
	if parentTaskID != "" {
		arguments["parent_task_id"] = parentTaskID
	}
	if err := adapter.alice.call(ctx, "request_data_task", arguments, &created); err != nil {
		return provisionedTask{}, err
	}
	if created.TaskID == "" || created.OAURL == "" || created.RootTaskID == "" || created.BudgetProfile == "" ||
		created.Budget.MaxQueries <= 0 || created.Budget.MaxRows <= 0 || created.Budget.MaxDBMS <= 0 ||
		created.Budget.QueryTimeoutMS <= 0 || created.Budget.TaskTTLSeconds <= 0 || created.Budget.MaxReleaseFacts <= 0 ||
		created.Budget.MaxInfluenceFacts <= 0 || created.Budget.MaxOutcomeFacts <= 0 ||
		created.Budget.ExposureProfileVersion != "taskgate-exposure-v5" || created.Budget.PredicateFootprint == nil ||
		created.Budget.PredicateFootprint.Validate() != nil {
		return provisionedTask{}, errors.New("task request omitted identity, root, profile, or budget")
	}
	draftID := pathTail(created.OAURL)
	if err := oaAction(ctx, adapter.aliceOA, adapter.oaBase, draftID, "submit", ""); err != nil {
		return provisionedTask{}, err
	}
	if err := adapter.waitTask(ctx, created.TaskID, "AWAITING_APPROVAL"); err != nil {
		return provisionedTask{}, err
	}
	var narrowedTTLMS int64
	if parentTaskID == "" {
		if err := oaAction(ctx, adapter.bobOA, adapter.oaBase, draftID, "decision", "approved"); err != nil {
			return provisionedTask{}, err
		}
	} else {
		computedTTLMS, narrowErr := delegatedNarrowTTLMS(created.Budget.TaskTTLSeconds)
		if narrowErr != nil {
			return provisionedTask{}, narrowErr
		}
		narrowedTTLMS = computedTTLMS
		if err := oaNarrowAction(ctx, adapter.bobOA, adapter.oaBase, draftID, oaNarrowDecision{
			Products: products, Columns: approvedColumns, MandatoryScope: mandatoryScope,
			MaxQueries: created.Budget.MaxQueries, MaxResultRows: created.Budget.MaxRows,
			MaxDBMS: created.Budget.MaxDBMS, QueryTimeoutMS: created.Budget.QueryTimeoutMS,
			TaskTTLMS: narrowedTTLMS,
		}); err != nil {
			return provisionedTask{}, err
		}
	}
	if err := adapter.waitTask(ctx, created.TaskID, "ACTIVE"); err != nil {
		return provisionedTask{}, err
	}
	if parentTaskID != "" {
		if err := adapter.verifyDelegatedSignedGrant(ctx, parentTaskID, created.TaskID, created.Budget, narrowedTTLMS); err != nil {
			return provisionedTask{}, err
		}
	}
	return created, nil
}

// delegatedNarrowTTLMS reserves deterministic approval/callback headroom.
// request_data_task returns whole seconds, so flooring has already occurred
// before this calculation and cannot accidentally widen the selected TTL.
func delegatedNarrowTTLMS(taskTTLSeconds int64) (int64, error) {
	narrowedSeconds := taskTTLSeconds - delegatedApprovalTTLHeadroomSeconds
	if narrowedSeconds <= 0 || narrowedSeconds > math.MaxInt64/1000 {
		return 0, errors.New("delegated OA TTL cannot reserve 30 seconds of approval headroom")
	}
	return narrowedSeconds * 1000, nil
}

// verifyDelegatedSignedGrant reads only immutable persisted authorization
// evidence. Gateway callback handling cryptographically verifies the OA
// receipt before persistence; DecodeTaskGrantV1 rechecks its signed grant
// digest binding here before the Adapter lets the child execute a query.
func (adapter *realAdapter) verifyDelegatedSignedGrant(ctx context.Context, parentTaskID, childTaskID string,
	expected provisionedBudget, narrowedTTLMS int64) error {
	if adapter == nil || adapter.control == nil {
		return errors.New("delegated signed-grant verification requires Control")
	}
	var parentEncoded, childEncoded string
	if err := adapter.control.QueryRow(ctx, `SELECT approval_receipt FROM task_grants WHERE task_id=$1`, parentTaskID).Scan(&parentEncoded); err != nil {
		return fmt.Errorf("load parent signed grant: %w", err)
	}
	if err := adapter.control.QueryRow(ctx, `SELECT approval_receipt FROM task_grants WHERE task_id=$1`, childTaskID).Scan(&childEncoded); err != nil {
		return fmt.Errorf("load delegated signed grant: %w", err)
	}
	return validateDelegatedSignedGrant(parentEncoded, childEncoded, parentTaskID, childTaskID, expected, narrowedTTLMS)
}

func validateDelegatedSignedGrant(parentEncoded, childEncoded, parentTaskID, childTaskID string,
	expected provisionedBudget, narrowedTTLMS int64) error {
	parent, err := approval.DecodeTaskGrantV1(parentEncoded)
	if err != nil {
		return errors.New("parent persisted authorization is not a digest-bound signed grant")
	}
	child, err := approval.DecodeTaskGrantV1(childEncoded)
	if err != nil {
		return errors.New("child persisted authorization is not a digest-bound signed grant")
	}
	if parent.Core.TaskID != parentTaskID || child.Core.TaskID != childTaskID ||
		child.Core.ParentTaskID != parentTaskID || child.ApprovalReceipt.Decision != domain.ApprovalDecisionNarrow {
		return errors.New("delegated signed grant has unexpected lineage or OA decision")
	}
	if err := parent.Core.CheckDelegation(child.Core); err != nil {
		return fmt.Errorf("delegated signed grant expands parent authorization: %w", err)
	}
	budget := child.Core.Budget
	if budget.MaxQueries != expected.MaxQueries || budget.MaxResultRows != expected.MaxRows ||
		budget.MaxDBMS != expected.MaxDBMS || budget.PerQueryTimeoutMS != expected.QueryTimeoutMS ||
		budget.MaxReleaseFacts != expected.MaxReleaseFacts || budget.MaxInfluenceFacts != expected.MaxInfluenceFacts ||
		budget.MaxOutcomeFacts != expected.MaxOutcomeFacts || budget.ExposureProfileVersion != expected.ExposureProfileVersion ||
		!reflect.DeepEqual(budget.PredicateFootprint, expected.PredicateFootprint) || budget.TaskTTLMS != narrowedTTLMS {
		return errors.New("delegated signed grant differs from the server-selected budget outside the TTL headroom")
	}
	if !child.Core.ExpiresAt.Equal(child.ApprovalReceipt.IssuedAt.UTC().Add(time.Duration(narrowedTTLMS) * time.Millisecond)) {
		return errors.New("delegated signed grant expiry is not derived from its narrowed TTL")
	}
	if child.Core.ExpiresAt.After(parent.Core.ExpiresAt) {
		return errors.New("delegated signed grant expires after its parent")
	}
	return nil
}
