package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

type DataConnector interface {
	Query(context.Context, dataconnector.QueryRequest) (dataconnector.Result, error)
	Ping(context.Context) error
	Attestation(context.Context) (dataconnector.Attestation, error)
}

type Config struct {
	Catalog            *catalog.Catalog
	Store              *control.Store
	Approval           approval.ApprovalAdapter
	ReceiptVerifier    approval.ReceiptVerifier
	QueryReceiptSigner *queryreceipt.Signer
	Connector          DataConnector
	CallbackSecret     string
	Logger             *slog.Logger
	Clock              func() time.Time
	Background         context.Context
}

type Service struct {
	catalog            *catalog.Catalog
	store              *control.Store
	approval           approval.ApprovalAdapter
	receiptVerifier    approval.ReceiptVerifier
	queryReceiptSigner *queryreceipt.Signer
	connector          DataConnector
	callbackSecret     []byte
	logger             *slog.Logger
	clock              func() time.Time
	background         context.Context
	pendingSettles     atomic.Int64
}

type pendingContext struct {
	Products        []string            `json:"products"`
	Columns         map[string][]string `json:"columns"`
	MandatoryScope  map[string]any      `json:"mandatory_scope"`
	Budget          domain.Budget       `json:"budget"`
	Sensitivity     domain.Sensitivity  `json:"sensitivity"`
	DatasourceID    string              `json:"datasource_id"`
	SchemaDigest    string              `json:"schema_digest"`
	ApprovalMode    domain.ApprovalMode `json:"approval_mode"`
	Approver        string              `json:"approver,omitempty"`
	CallbackContext string              `json:"callback_context"`
}

func New(config Config) (*Service, error) {
	if config.Catalog == nil || config.Store == nil || config.Approval == nil || config.Connector == nil || config.CallbackSecret == "" {
		return nil, errors.New("gateway catalog, store, approval, connector, and callback secret are required")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Background == nil {
		config.Background = context.Background()
	}
	if config.ReceiptVerifier == nil {
		// The demo derives a stable Ed25519 key from its existing secret so old
		// compose environments keep working. Deployments can supply a public-key
		// verifier independently of the HMAC transport secret.
		config.ReceiptVerifier = approval.DemoReceiptVerifier([]byte(config.CallbackSecret))
	}
	if config.QueryReceiptSigner == nil {
		// Tests and the self-contained demo get a deterministic fallback. The
		// production entry point supplies an independently configured key.
		config.QueryReceiptSigner = queryreceipt.DemoSigner([]byte(config.CallbackSecret))
	}
	return &Service{
		catalog: config.Catalog, store: config.Store, approval: config.Approval,
		receiptVerifier:    config.ReceiptVerifier,
		queryReceiptSigner: config.QueryReceiptSigner, connector: config.Connector,
		callbackSecret: []byte(config.CallbackSecret), logger: config.Logger,
		clock: config.Clock, background: config.Background,
	}, nil
}

// ReadyError reports durable query-budget settlements that have not yet been
// persisted. While this is non-nil, accepting more queries could hide an
// outstanding reservation or temporarily overstate the remaining budget.
func (s *Service) ReadyError() error {
	if s == nil {
		return errors.New("gateway service is nil")
	}
	if pending := s.pendingSettles.Load(); pending > 0 {
		return fmt.Errorf("%d query budget settlement(s) pending", pending)
	}
	return nil
}

func randomID(prefix string) string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(buffer)
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func decodeArgs(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "工具参数不符合契约"}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "工具参数包含多余内容"}
	}
	return nil
}

func toolError(err error) error {
	if err == nil {
		return nil
	}
	var already *mcp.ToolError
	if errors.As(err, &already) {
		return already
	}
	var policyErr *sqlpolicy.PolicyError
	if errors.As(err, &policyErr) {
		return &mcp.ToolError{Code: string(policyErr.Code), Message: policyErr.Error()}
	}
	var connectorErr *dataconnector.Error
	if errors.As(err, &connectorErr) {
		return &mcp.ToolError{Code: string(connectorErr.Code), Message: connectorErr.Error()}
	}
	var appErr *apierr.Error
	if errors.As(err, &appErr) {
		return &mcp.ToolError{Code: appErr.Code, Message: appErr.Message}
	}
	switch control.CodeOf(err) {
	case control.CodeNotFound:
		return &mcp.ToolError{Code: apierr.CodeNotFound, Message: "请求的任务或凭证不存在"}
	case control.CodeTaskNotActive, control.CodeTaskExpired:
		return &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务当前不可查询；请检查审批、期限或归档状态"}
	case control.CodeBudgetExhausted:
		return &mcp.ToolError{Code: apierr.CodeBudgetExhausted, Message: "任务预算已耗尽并已归档"}
	case control.CodeQueryInProgress:
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "同一任务已有查询正在执行"}
	case control.CodeInvalid, control.CodeInvalidStateChange:
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "请求不符合任务状态或数据契约"}
	case control.CodeIdempotencyConflict:
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "幂等键已用于不同请求"}
	case control.CodeConflict, control.CodeCallbackInProgress:
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "请求与当前状态冲突；请重试或刷新状态"}
	}
	return &mcp.ToolError{Code: apierr.CodeInternal, Message: "请求处理失败；请使用 trace_id 联系管理员"}
}

func forbidden() error {
	return &mcp.ToolError{Code: apierr.CodeForbidden, Message: "当前身份无权执行此工具"}
}

func notFound() error {
	return &mcp.ToolError{Code: apierr.CodeNotFound, Message: "请求的任务或凭证不存在"}
}

func (s *Service) ownedTask(ctx context.Context, principal mcp.Principal, taskID string) (control.Task, error) {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return control.Task{}, toolError(err)
	}
	if principal.Role != "query" || task.PrincipalID != principal.ID {
		return control.Task{}, notFound()
	}
	return task, nil
}

func publicTask(task control.Task) map[string]any {
	return map[string]any{
		"task_id": task.ID, "objective": task.Objective, "state": task.State,
		"terminal_reason": task.TerminalReason, "catalog_version": task.CatalogVersion,
		"sensitivity": task.Sensitivity, "created_at": task.CreatedAt,
		"updated_at": task.UpdatedAt, "expires_at": task.ExpiresAt,
	}
}

func publicBudget(snapshot control.BudgetSnapshot) map[string]any {
	return map[string]any{
		"limits":     map[string]any{"queries": snapshot.Limits.Queries, "rows": snapshot.Limits.Rows, "db_ms": snapshot.Limits.DBMS},
		"used":       map[string]any{"queries": snapshot.Usage.UsedQueries, "rows": snapshot.Usage.UsedRows, "db_ms": snapshot.Usage.UsedDBMS},
		"reserved":   map[string]any{"queries": snapshot.Usage.ReservedQueries, "rows": snapshot.Usage.ReservedRows, "db_ms": snapshot.Usage.ReservedDBMS},
		"remaining":  map[string]any{"queries": snapshot.Remaining().Queries, "rows": snapshot.Remaining().Rows, "db_ms": snapshot.Remaining().DBMS},
		"updated_at": snapshot.UpdatedAt,
	}
}
