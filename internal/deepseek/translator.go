package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
)

const defaultModel = "deepseek-v4-flash"

type RequestedBudget struct {
	MaxQueries int `json:"max_queries,omitempty"`
	MaxRows    int `json:"max_rows,omitempty"`
}

type TaskIntent struct {
	Objective       string              `json:"objective"`
	DataProducts    []string            `json:"data_products"`
	Columns         map[string][]string `json:"columns,omitempty"`
	Scopes          map[string]any      `json:"scopes,omitempty"`
	RequestedBudget *RequestedBudget    `json:"requested_budget,omitempty"`
}

type Filter struct {
	Column string `json:"column"`
	Op     string `json:"op"`
	Value  any    `json:"value"`
}

type Order struct {
	Column    string `json:"column"`
	Direction string `json:"direction"`
}

// QueryPlan is declarative input to deterministic Go compilation. It is never
// treated as executable SQL from the model.
type QueryPlan struct {
	Product    string      `json:"product"`
	Columns    []string    `json:"columns"`
	Aggregates []Aggregate `json:"aggregates,omitempty"`
	Filters    []Filter    `json:"filters,omitempty"`
	GroupBy    []string    `json:"group_by,omitempty"`
	OrderBy    []Order     `json:"order_by,omitempty"`
	Limit      int         `json:"limit,omitempty"`
}

type Aggregate struct {
	Function string `json:"function"`
	Column   string `json:"column"`
	Alias    string `json:"alias"`
}

type Translator interface {
	TranslateIntent(context.Context, string, string) (TaskIntent, error)
	TranslateQuery(context.Context, string, string) (QueryPlan, error)
}

type Client struct {
	apiKey  string
	baseURL string
	model   string
	http    *http.Client
}

func New(apiKey, baseURL, model string, httpClient *http.Client) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.deepseek.com"
	}
	if strings.TrimSpace(model) == "" {
		model = defaultModel
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{apiKey: apiKey, baseURL: strings.TrimRight(baseURL, "/"), model: model, http: httpClient}
}

func (c *Client) TranslateIntent(ctx context.Context, question, logicalCatalog string) (TaskIntent, error) {
	var intent TaskIntent
	schema := `Return exactly one JSON object with objective (string), data_products (string array), optional columns (object of string arrays), optional scopes (object), and optional requested_budget {max_queries,max_rows}. Never invent a product, field, or scope.`
	content, err := c.structured(ctx, schema, question, logicalCatalog)
	if err != nil {
		return intent, err
	}
	if err := decodeStrict(content, &intent); err == nil && validateIntent(intent) == nil {
		return intent, nil
	}
	content, err = c.repair(ctx, schema, content)
	if err != nil {
		return intent, err
	}
	if err := decodeStrict(content, &intent); err != nil {
		return TaskIntent{}, apierr.Wrap(apierr.CodeInvalidModelOutput, "模型输出不符合 TaskIntent 契约", err)
	}
	if err := validateIntent(intent); err != nil {
		return TaskIntent{}, apierr.Wrap(apierr.CodeInvalidModelOutput, "模型输出不符合 TaskIntent 契约", err)
	}
	return intent, nil
}

func (c *Client) TranslateQuery(ctx context.Context, question, logicalCatalog string) (QueryPlan, error) {
	var plan QueryPlan
	schema := `Return exactly one JSON object: product, columns, optional aggregates [{function,column,alias}], filters [{column,op,value}], group_by, order_by [{column,direction}], and limit. This is a declarative query plan, never SQL. Use only catalog names.`
	content, err := c.structured(ctx, schema, question, logicalCatalog)
	if err != nil {
		return plan, err
	}
	if err := decodeStrict(content, &plan); err == nil && validateQueryPlan(plan) == nil {
		return plan, nil
	}
	content, err = c.repair(ctx, schema, content)
	if err != nil {
		return plan, err
	}
	if err := decodeStrict(content, &plan); err != nil {
		return QueryPlan{}, apierr.Wrap(apierr.CodeInvalidModelOutput, "模型输出不符合 QueryPlan 契约", err)
	}
	if err := validateQueryPlan(plan); err != nil {
		return QueryPlan{}, apierr.Wrap(apierr.CodeInvalidModelOutput, "模型输出不符合 QueryPlan 契约", err)
	}
	return plan, nil
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	Temperature    float64       `json:"temperature"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) structured(ctx context.Context, contract, question, catalog string) ([]byte, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, apierr.New(apierr.CodeModelUnavailable, "DeepSeek 未配置；直接 SQL 与确定性工具仍可使用")
	}
	system := "You translate natural-language data requests into strict JSON. " + contract + " The catalog below contains logical metadata only.\nCATALOG:\n" + catalog
	return c.complete(ctx, []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: question}})
}

func (c *Client) repair(ctx context.Context, contract string, invalid []byte) ([]byte, error) {
	messages := []chatMessage{
		{Role: "system", Content: "Repair JSON once. " + contract + " Return JSON only."},
		{Role: "user", Content: "Invalid output:\n" + string(invalid)},
	}
	return c.complete(ctx, messages)
}

func (c *Client) complete(ctx context.Context, messages []chatMessage) ([]byte, error) {
	reqBody := chatRequest{Model: c.model, Messages: messages, Temperature: 0}
	reqBody.ResponseFormat.Type = "json_object"
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, "无法构造模型请求", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, "无法构造模型请求", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeModelUnavailable, "DeepSeek 暂不可用；请稍后重试", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeModelUnavailable, "DeepSeek 响应读取失败", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apierr.Wrap(apierr.CodeModelUnavailable, "DeepSeek 暂不可用；请稍后重试", fmt.Errorf("status %d", resp.StatusCode))
	}
	var decoded chatResponse
	if err := json.Unmarshal(body, &decoded); err != nil || len(decoded.Choices) != 1 {
		return nil, apierr.Wrap(apierr.CodeModelUnavailable, "DeepSeek 返回了无效响应", err)
	}
	return []byte(strings.TrimSpace(decoded.Choices[0].Message.Content)), nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON contains trailing data")
	}
	return nil
}

func validateIntent(intent TaskIntent) error {
	if strings.TrimSpace(intent.Objective) == "" {
		return errors.New("objective is required")
	}
	if len(intent.DataProducts) == 0 {
		return errors.New("at least one data product is required")
	}
	for _, product := range intent.DataProducts {
		if strings.TrimSpace(product) == "" {
			return errors.New("data product is empty")
		}
	}
	if intent.RequestedBudget != nil && (intent.RequestedBudget.MaxQueries < 0 || intent.RequestedBudget.MaxRows < 0) {
		return errors.New("requested budget cannot be negative")
	}
	return nil
}

func validateQueryPlan(plan QueryPlan) error {
	if strings.TrimSpace(plan.Product) == "" {
		return errors.New("product is required")
	}
	if len(plan.Columns)+len(plan.Aggregates) == 0 {
		return errors.New("columns or aggregates are required")
	}
	if plan.Limit < 0 {
		return errors.New("limit cannot be negative")
	}
	for _, filter := range plan.Filters {
		switch strings.ToLower(strings.TrimSpace(filter.Op)) {
		case "=", "!=", "<>", "<", "<=", ">", ">=", "in", "not in", "like":
		default:
			return fmt.Errorf("unsupported filter operator %q", filter.Op)
		}
	}
	for _, order := range plan.OrderBy {
		switch strings.ToLower(order.Direction) {
		case "", "asc", "desc":
		default:
			return fmt.Errorf("unsupported order direction %q", order.Direction)
		}
	}
	return nil
}
