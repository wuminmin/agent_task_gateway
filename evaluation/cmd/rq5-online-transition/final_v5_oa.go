package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/gateway"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// finalV5OAWorkflow drives the real oa-demo browser endpoints. Draft creation
// itself remains inside production Gateway through approval.Client; these two
// authenticated cookie/CSRF sessions perform the human submit and decision
// steps and then wait for oa-demo's signed HTTP callback to mutate Control.
type finalV5OAWorkflow struct {
	baseURL string
	alice   *http.Client
	bob     *http.Client
}

func newFinalV5OAWorkflow(ctx context.Context, baseURL, alicePassword,
	bobPassword string) (*finalV5OAWorkflow, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || alicePassword == "" || bobPassword == "" {
		return nil, errors.New("real RQ5 OA URL and deployment credentials are required")
	}
	alice, err := finalV5OALogin(ctx, baseURL, "alice", alicePassword)
	if err != nil {
		return nil, fmt.Errorf("real OA Alice login: %w", err)
	}
	bob, err := finalV5OALogin(ctx, baseURL, "bob", bobPassword)
	if err != nil {
		return nil, fmt.Errorf("real OA Bob login: %w", err)
	}
	return &finalV5OAWorkflow{baseURL: baseURL, alice: alice, bob: bob}, nil
}

func finalV5OALogin(ctx context.Context, baseURL, username, password string) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	page, err := finalV5OAGet(ctx, client, baseURL+"/login")
	if err != nil {
		return nil, err
	}
	values, err := finalV5OAFormValues(page)
	if err != nil || values.Get("csrf") == "" {
		return nil, errors.New("OA login page omitted its CSRF token")
	}
	values.Set("username", username)
	values.Set("password", password)
	if _, err := finalV5OAPost(ctx, client, baseURL+"/login", values); err != nil {
		return nil, err
	}
	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	authenticated := false
	for _, cookie := range jar.Cookies(parsedBaseURL) {
		if cookie.Name == "oa_session" && cookie.Value != "" {
			authenticated = true
		}
	}
	if !authenticated {
		return nil, errors.New("OA login did not issue an authenticated session cookie")
	}
	if _, err := finalV5OAGet(ctx, client, baseURL+"/tasks"); err != nil {
		return nil, errors.New("OA login did not establish an authenticated session")
	}
	return client, nil
}

func (workflow *finalV5OAWorkflow) submit(ctx context.Context, draftID string) error {
	return workflow.action(ctx, workflow.alice, draftID, "submit", "", nil)
}

func (workflow *finalV5OAWorkflow) decide(ctx context.Context, store *control.Store,
	draftID, parentTaskID string) error {
	if parentTaskID == "" {
		return workflow.action(ctx, workflow.bob, draftID, "decision", "approved", nil)
	}
	parentGrant, err := store.GetGrant(ctx, parentTaskID)
	if err != nil {
		return fmt.Errorf("load delegated parent grant before real OA narrowing: %w", err)
	}
	return workflow.action(ctx, workflow.bob, draftID, "decision", "narrow", &parentGrant.ExpiresAt)
}

func (workflow *finalV5OAWorkflow) action(ctx context.Context, client *http.Client,
	draftID, action, decision string, parentExpiry *time.Time) error {
	if workflow == nil || client == nil || draftID == "" {
		return errors.New("real OA action is incomplete")
	}
	taskURL := workflow.baseURL + "/tasks/" + url.PathEscape(draftID)
	page, err := finalV5OAGet(ctx, client, taskURL)
	if err != nil {
		return err
	}
	values, err := finalV5OAFormValues(page)
	if err != nil || values.Get("csrf") == "" {
		return errors.New("OA task page omitted its CSRF token")
	}
	if decision != "" {
		values.Set("decision", decision)
	}
	if parentExpiry != nil {
		required := []string{"approved_products", "approved_columns", "mandatory_scope", "max_queries",
			"max_result_rows", "max_db_ms", "per_query_timeout_ms", "task_ttl_ms"}
		for _, name := range required {
			if strings.TrimSpace(values.Get(name)) == "" {
				return fmt.Errorf("OA narrowing page omitted %s", name)
			}
		}
		requestedTTL, parseErr := strconv.ParseInt(values.Get("task_ttl_ms"), 10, 64)
		remainingTTL := time.Until(parentExpiry.UTC()).Milliseconds() - 5_000
		if parseErr != nil || requestedTTL <= 1 || remainingTTL <= 1 {
			return errors.New("delegated OA TTL cannot be safely narrowed below its parent")
		}
		if remainingTTL >= requestedTTL {
			remainingTTL = requestedTTL - 1
		}
		values.Set("task_ttl_ms", strconv.FormatInt(remainingTTL, 10))
	}
	_, err = finalV5OAPost(ctx, client, taskURL+"/"+action, values)
	return err
}

func finalV5OAGet(ctx context.Context, client *http.Client, target string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OA GET returned %d", response.StatusCode)
	}
	return finalV5ReadBounded(response.Body, 2<<20)
}

func finalV5OAPost(ctx context.Context, client *http.Client, target string,
	values url.Values) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := finalV5ReadBounded(response.Body, 2<<20)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return nil, fmt.Errorf("OA POST returned %d", response.StatusCode)
	}
	return body, nil
}

func finalV5ReadBounded(reader io.Reader, maximum int64) ([]byte, error) {
	limited := io.LimitReader(reader, maximum+1)
	value, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > maximum {
		return nil, errors.New("OA response exceeded its byte bound")
	}
	return value, nil
}

func finalV5OAFormValues(page []byte) (url.Values, error) {
	document, err := html.Parse(strings.NewReader(string(page)))
	if err != nil {
		return nil, err
	}
	values := make(url.Values)
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && (node.Data == "input" || node.Data == "textarea") {
			name, value := "", ""
			for _, attribute := range node.Attr {
				switch attribute.Key {
				case "name":
					name = attribute.Val
				case "value":
					value = attribute.Val
				}
			}
			if node.Data == "textarea" {
				var text strings.Builder
				for child := node.FirstChild; child != nil; child = child.NextSibling {
					if child.Type == html.TextNode {
						text.WriteString(child.Data)
					}
				}
				value = text.String()
			}
			if name != "" {
				values.Set(name, value)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	return values, nil
}

func requestAndApproveLive(ctx context.Context, service *gateway.Service, store *control.Store,
	workflow *finalV5OAWorkflow, principal mcp.Principal, parentTaskID string) (control.Task, error) {
	arguments := map[string]any{
		"objective":     "Verify immutable daily reporting publication routing",
		"data_products": []string{"daily_lineitem"},
		"columns": map[string][]string{"daily_lineitem": {
			"dataset_partition", "l_orderkey", "l_linenumber", "l_extendedprice",
		}},
		"scopes": map[string]any{"dataset_partition": "1"},
	}
	if parentTaskID != "" {
		arguments["parent_task_id"] = parentTaskID
	}
	result, err := callTool(ctx, service, principal, "request_data_task", arguments)
	if err != nil {
		return control.Task{}, err
	}
	taskID, ok := result["task_id"].(string)
	if !ok || taskID == "" {
		return control.Task{}, errors.New("request_data_task returned no task_id")
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		return control.Task{}, err
	}
	if task.ApprovalRef == "" || task.State != control.TaskAwaitingSubmission {
		return control.Task{}, errors.New("production approval.Client did not persist a real OA draft")
	}
	if err := workflow.submit(ctx, task.ApprovalRef); err != nil {
		return control.Task{}, fmt.Errorf("real OA submit: %w", err)
	}
	if _, err := waitFinalV5TaskState(ctx, store, task.ID, control.TaskAwaitingApproval); err != nil {
		return control.Task{}, fmt.Errorf("wait for submitted OA callback: %w", err)
	}
	if err := workflow.decide(ctx, store, task.ApprovalRef, parentTaskID); err != nil {
		return control.Task{}, fmt.Errorf("real OA decision: %w", err)
	}
	active, err := waitFinalV5TaskState(ctx, store, task.ID, control.TaskActive)
	if err != nil {
		return control.Task{}, fmt.Errorf("wait for decided OA callback: %w", err)
	}
	return active, nil
}

func waitFinalV5TaskState(ctx context.Context, store *control.Store, taskID string,
	want control.TaskState) (control.Task, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	for {
		task, err := store.GetTask(ctx, taskID)
		if err == nil && task.State == want {
			return task, nil
		}
		select {
		case <-ctx.Done():
			return control.Task{}, ctx.Err()
		case <-timeout.C:
			if err != nil {
				return control.Task{}, err
			}
			return control.Task{}, fmt.Errorf("task %s remained in %s, want %s", taskID, task.State, want)
		case <-ticker.C:
		}
	}
}
