package approval

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

type DraftResponse struct {
	DraftID string `json:"draft_id"`
	State   string `json:"state"`
	URL     string `json:"url"`
}

type ApprovalAdapter interface {
	CreateDraft(context.Context, DraftRequest) (DraftResponse, error)
}

type Client struct {
	baseURL      string
	serviceToken string
	http         *http.Client
}

func NewClient(baseURL, serviceToken string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" || serviceToken == "" {
		return nil, errors.New("OA base URL and service token are required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), serviceToken: serviceToken, http: httpClient}, nil
}

func (c *Client) CreateDraft(ctx context.Context, draft DraftRequest) (DraftResponse, error) {
	var result DraftResponse
	body, err := json.Marshal(draft)
	if err != nil {
		return result, apierr.Wrap(apierr.CodeInternal, "无法构造 OA 草稿", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/drafts", bytes.NewReader(body))
	if err != nil {
		return result, apierr.Wrap(apierr.CodeInternal, "无法构造 OA 请求", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.serviceToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return result, apierr.Wrap(apierr.CodeApprovalUnavailable, "OA 暂不可用；任务未创建", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return result, apierr.Wrap(apierr.CodeApprovalUnavailable, "OA 响应读取失败；任务未创建", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return result, apierr.Wrap(apierr.CodeApprovalUnavailable, "OA 拒绝创建草稿；任务未创建", fmt.Errorf("status %d", resp.StatusCode))
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result.DraftID == "" || result.URL == "" {
		return DraftResponse{}, apierr.Wrap(apierr.CodeApprovalUnavailable, "OA 返回了无效草稿", err)
	}
	return result, nil
}
