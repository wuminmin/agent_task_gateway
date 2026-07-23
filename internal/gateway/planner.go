package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

func (s *Service) planExposure(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID     string               `json:"task_id"`
		Candidates []exposure.Candidate `json:"candidates"`
		Weights    *struct {
			AnswerCompleteness float64 `json:"answer_completeness"`
			QueryCoverage      float64 `json:"query_coverage"`
		} `json:"weights,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Candidates) == 0 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "candidates 不能为空"}
	}
	task, err := s.ownedTask(ctx, principal, args.TaskID)
	if err != nil {
		return nil, err
	}
	if task.State != control.TaskActive {
		return nil, toolError(control.ErrTaskNotActive)
	}
	grant, err := s.store.GetGrant(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if !grant.Exposure.Enabled() {
		return nil, toolError(control.ErrExposureEvidenceRequired)
	}
	approved := make(map[string]struct{}, len(grant.ApprovedProducts))
	for _, product := range grant.ApprovedProducts {
		approved[product] = struct{}{}
	}
	for _, candidate := range args.Candidates {
		if _, ok := approved[candidate.Product]; !ok {
			return nil, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "候选表示包含任务授权外的数据产品"}
		}
	}
	ledger, err := s.store.GetExposureLedger(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	weights := exposure.UtilityWeights{AnswerCompleteness: 0.5, QueryCoverage: 0.5}
	if args.Weights != nil {
		weights.AnswerCompleteness = args.Weights.AnswerCompleteness
		weights.QueryCoverage = args.Weights.QueryCoverage
	}
	remaining := ledger.Remaining()
	plan, err := exposure.Optimize(args.Candidates, remaining.ReleaseFacts, remaining.InfluenceFacts, weights)
	if err != nil {
		if errors.Is(err, exposure.ErrInvalid) {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "候选成本或可测量效用不符合规划契约"}
		}
		return nil, err
	}
	return map[string]any{
		"task_id": task.ID, "root_task_id": ledger.RootTaskID,
		"profile_version": ledger.ProfileVersion, "budget_remaining": remaining,
		"weights": weights, "plan": plan,
	}, nil
}
