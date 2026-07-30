package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/gateway"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// experimentRouter is deliberately local to this evaluation command. It is
// not production routing and cannot mutate a Gateway Service or its Catalog.
// Switching changes only which already-constructed retained Service receives
// a subsequent root approval workflow; execution always resolves the task's
// persisted Catalog version back to its retained Service.
type experimentRouter struct {
	store       *control.Store
	approval    *capturingApproval
	principal   mcp.Principal
	secret      string
	deployments []*retainedDeployment
	byVersion   map[string]*retainedDeployment
	active      atomic.Int32
}

type retainedDeployment struct {
	Day         string
	Catalog     *catalog.Catalog
	Publication loadedPublication
	Connector   *dataconnector.Connector
	Service     *gateway.Service
}

func newExperimentRouter(store *control.Store, adapter *capturingApproval, principal mcp.Principal,
	secret string, deployments []*retainedDeployment) (*experimentRouter, error) {
	if store == nil || adapter == nil || secret == "" || len(deployments) != len(days) {
		return nil, errors.New("complete retained deployment set is required")
	}
	result := &experimentRouter{store: store, approval: adapter, principal: principal,
		secret: secret, deployments: deployments, byVersion: make(map[string]*retainedDeployment, len(deployments))}
	for index, deployment := range deployments {
		if deployment == nil || deployment.Service == nil || deployment.Catalog == nil || deployment.Day != days[index] {
			return nil, errors.New("retained deployments are missing or out of order")
		}
		if _, duplicate := result.byVersion[deployment.Catalog.CatalogVersion]; duplicate {
			return nil, errors.New("retained Catalog version is duplicated")
		}
		result.byVersion[deployment.Catalog.CatalogVersion] = deployment
	}
	result.active.Store(0)
	return result, nil
}

func (router *experimentRouter) switchTo(index int) error {
	if index <= 0 || index >= len(router.deployments) {
		return errors.New("switch target is outside retained deployment set")
	}
	current := int(router.active.Load())
	if index != current+1 {
		return fmt.Errorf("switch %d->%d is not the next daily publication", current, index)
	}
	router.active.Store(int32(index))
	if int(router.active.Load()) != index {
		return errors.New("active deployment pointer did not commit")
	}
	return nil
}

func (router *experimentRouter) approveRoot(ctx context.Context) (control.Task, error) {
	index := int(router.active.Load())
	if index < 0 || index >= len(router.deployments) {
		return control.Task{}, errors.New("active deployment pointer is invalid")
	}
	return requestAndApprove(ctx, router.deployments[index].Service, router.store,
		router.approval, router.principal, router.secret, "")
}

func (router *experimentRouter) approveChild(ctx context.Context, parentTaskID string) (control.Task, error) {
	deployment, err := router.deploymentForTask(ctx, parentTaskID)
	if err != nil {
		return control.Task{}, err
	}
	return requestAndApprove(ctx, deployment.Service, router.store,
		router.approval, router.principal, router.secret, parentTaskID)
}

func (router *experimentRouter) executePlan(ctx context.Context, taskID, requestID string) (map[string]any, error) {
	deployment, err := router.deploymentForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return callTool(ctx, deployment.Service, router.principal, "execute_plan", map[string]any{
		"task_id": taskID, "request_id": requestID,
		"plan": map[string]any{
			"product":  "daily_lineitem",
			"columns":  []string{"l_orderkey", "l_linenumber", "l_extendedprice"},
			"filters":  []map[string]any{{"column": "l_orderkey", "op": "=", "value": 1}},
			"order_by": []map[string]any{{"column": "l_linenumber", "direction": "asc"}},
			"limit":    10,
		},
	})
}

func (router *experimentRouter) deploymentForTask(ctx context.Context, taskID string) (*retainedDeployment, error) {
	task, err := router.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	deployment := router.byVersion[task.CatalogVersion]
	if deployment == nil {
		return nil, errors.New("task Catalog version has no retained Service")
	}
	return deployment, nil
}

func (router *experimentRouter) persistedPublicationDigest(ctx context.Context, taskID string) (string, error) {
	task, err := router.store.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	grant, err := router.store.GetGrant(ctx, taskID)
	if err != nil {
		return "", err
	}
	protocol, err := approval.DecodeTaskGrantV1(grant.ApprovalReceipt)
	if err != nil || approval.VerifyTaskGrantV1(approval.DemoReceiptVerifier([]byte(router.secret)), protocol) != nil {
		return "", errors.New("persisted signed grant is invalid")
	}
	deployment := router.byVersion[task.CatalogVersion]
	if deployment == nil || protocol.Core.CatalogVersion != task.CatalogVersion ||
		protocol.Core.CatalogSHA256 != deployment.Catalog.SHA256 || grant.CatalogDigest != deployment.Catalog.SHA256 {
		return "", errors.New("persisted signed grant does not bind its retained Catalog")
	}
	return deployment.Publication.Bundle.ManifestDigest, nil
}
