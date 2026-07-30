package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type createdTask struct {
	TaskID     string `json:"task_id"`
	RootTaskID string `json:"root_task_id"`
	OAURL      string `json:"oa_url"`
	Budget     struct {
		MaxQueries        int64  `json:"max_queries"`
		MaxRows           int64  `json:"max_rows"`
		MaxDBMS           int64  `json:"max_db_ms"`
		QueryTimeoutMS    int64  `json:"query_timeout_ms"`
		TaskTTLSeconds    int64  `json:"task_ttl_seconds"`
		MaxReleaseFacts   int64  `json:"max_release_facts"`
		MaxInfluenceFacts int64  `json:"max_influence_facts"`
		MaxOutcomeFacts   int64  `json:"max_outcome_facts"`
		ExposureProfile   string `json:"exposure_profile_version"`
	} `json:"budget"`
}

func provisionTaskFamilies(ctx context.Context, template concurrencyConfig) (concurrencyConfig, error) {
	if template.Provision == nil {
		return concurrencyConfig{}, errors.New("provision contract is required")
	}
	provision := *template.Provision
	token := strings.TrimSpace(os.Getenv(template.Gateway.TokenEnv))
	alicePassword := os.Getenv(provision.AlicePasswordEnv)
	bobPassword := os.Getenv(provision.BobPasswordEnv)
	if token == "" || alicePassword == "" || bobPassword == "" {
		return concurrencyConfig{}, errors.New("Gateway token or OA password environment variable is empty")
	}
	timeout := time.Duration(template.RequestTimeoutMS) * time.Millisecond
	alice, err := oaClient(provision.OAURL, "alice", alicePassword, timeout)
	if err != nil {
		return concurrencyConfig{}, fmt.Errorf("OA Alice login: %w", err)
	}
	bob, err := oaClient(provision.OAURL, "bob", bobPassword, timeout)
	if err != nil {
		return concurrencyConfig{}, fmt.Errorf("OA Bob login: %w", err)
	}
	mcp := &mcpClient{url: strings.TrimRight(template.Gateway.URL, "/") + "/mcp", token: token,
		http: &http.Client{Timeout: timeout}}
	prepared := template
	prepared.Cases = append([]concurrencyCase(nil), template.Cases...)
	for caseIndex := range prepared.Cases {
		one := &prepared.Cases[caseIndex]
		root, err := requestTask(ctx, mcp, provision, "V4 concurrency "+one.ID+" root", "")
		if err != nil {
			return concurrencyConfig{}, fmt.Errorf("case %s request root: %w", one.ID, err)
		}
		if err := validateCreatedBudget(root, one.AtBudget); err != nil {
			return concurrencyConfig{}, fmt.Errorf("case %s root Catalog budget: %w", one.ID, err)
		}
		if root.RootTaskID != root.TaskID {
			return concurrencyConfig{}, fmt.Errorf("case %s root response has root_task_id=%q", one.ID, root.RootTaskID)
		}
		if err := submitAndApprove(ctx, mcp, alice, bob, provision.OAURL, root, false, provision, timeout); err != nil {
			return concurrencyConfig{}, fmt.Errorf("case %s approve root: %w", one.ID, err)
		}
		one.RootTaskID = root.TaskID
		one.PrefixTaskID = root.TaskID
		one.ContenderTaskIDs = make([]string, 0, one.Concurrency)
		for index := 0; index < one.Concurrency+1; index++ {
			role := fmt.Sprintf("contender-%02d", index+1)
			if index == one.Concurrency {
				role = "overflow"
			}
			child, err := requestTask(ctx, mcp, provision,
				fmt.Sprintf("V4 concurrency %s %s", one.ID, role), root.TaskID)
			if err != nil {
				return concurrencyConfig{}, fmt.Errorf("case %s request %s: %w", one.ID, role, err)
			}
			if child.RootTaskID != root.TaskID {
				return concurrencyConfig{}, fmt.Errorf("case %s %s is not in the requested root family", one.ID, role)
			}
			if err := validateCreatedBudget(child, one.AtBudget); err != nil {
				return concurrencyConfig{}, fmt.Errorf("case %s %s inherited budget: %w", one.ID, role, err)
			}
			if err := submitAndApprove(ctx, mcp, alice, bob, provision.OAURL, child, true, provision, timeout); err != nil {
				return concurrencyConfig{}, fmt.Errorf("case %s approve %s: %w", one.ID, role, err)
			}
			if index == one.Concurrency {
				one.OverflowTaskID = child.TaskID
			} else {
				one.ContenderTaskIDs = append(one.ContenderTaskIDs, child.TaskID)
			}
		}
		fmt.Fprintf(os.Stderr, "provisioned V4 concurrency root family %d/%d (width=%d)\n",
			caseIndex+1, len(prepared.Cases), one.Concurrency)
	}
	return prepared, nil
}

func requestTask(ctx context.Context, mcp *mcpClient, provision provisionConfig,
	objective, parentTaskID string) (createdTask, error) {
	arguments := map[string]any{
		"objective": objective, "data_products": provision.DataProducts,
		"columns": provision.Columns, "scopes": provision.Scopes,
	}
	if parentTaskID != "" {
		arguments["parent_task_id"] = parentTaskID
	}
	var created createdTask
	if err := mcp.call(ctx, "request_data_task", arguments, &created); err != nil {
		return createdTask{}, err
	}
	if created.TaskID == "" || created.RootTaskID == "" || created.OAURL == "" {
		return createdTask{}, errors.New("request_data_task omitted task/root/OA identity")
	}
	return created, nil
}

func validateCreatedBudget(created createdTask, expected exposureCounts) error {
	actual := exposureCounts{Release: created.Budget.MaxReleaseFacts,
		Influence: created.Budget.MaxInfluenceFacts, Outcome: created.Budget.MaxOutcomeFacts}
	if created.Budget.ExposureProfile != "taskgate-exposure-v4" {
		return fmt.Errorf("profile=%q, want taskgate-exposure-v4", created.Budget.ExposureProfile)
	}
	if actual != expected {
		return fmt.Errorf("Catalog exposure budget=%+v, template at_budget=%+v", actual, expected)
	}
	if created.Budget.MaxQueries < 1 || created.Budget.MaxRows < 1 || created.Budget.MaxDBMS < 1 ||
		created.Budget.QueryTimeoutMS < 1 || created.Budget.TaskTTLSeconds < 2 {
		return errors.New("Catalog resource budget cannot support provisioning")
	}
	return nil
}

func submitAndApprove(ctx context.Context, mcp *mcpClient, alice, bob *http.Client, oaURL string,
	created createdTask, delegated bool, provision provisionConfig, timeout time.Duration) error {
	draftID := pathTail(created.OAURL)
	if draftID == "" {
		return errors.New("OA URL has no draft ID")
	}
	if err := oaAction(ctx, alice, oaURL, draftID, "submit", ""); err != nil {
		return err
	}
	if err := waitTask(ctx, mcp, created.TaskID, "AWAITING_APPROVAL", timeout); err != nil {
		return err
	}
	var err error
	if delegated {
		err = oaNarrow(ctx, bob, oaURL, draftID, created, provision)
	} else {
		err = oaAction(ctx, bob, oaURL, draftID, "decision", "approved")
	}
	if err != nil {
		return err
	}
	return waitTask(ctx, mcp, created.TaskID, "ACTIVE", timeout)
}

func waitTask(ctx context.Context, client *mcpClient, taskID, expected string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var status struct {
			State string `json:"state"`
		}
		if err := client.call(ctx, "get_task_status", map[string]any{"task_id": taskID}, &status); err != nil {
			return err
		}
		if status.State == expected {
			return nil
		}
		if status.State == "ARCHIVED" {
			return fmt.Errorf("task archived while waiting for %s", expected)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("task state=%s while waiting for %s", status.State, expected)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func oaClient(baseURL, username, password string, timeout time.Duration) (*http.Client, error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: timeout}
	page, err := httpGet(context.Background(), client, strings.TrimRight(baseURL, "/")+"/login")
	if err != nil {
		return nil, err
	}
	csrf, err := csrfToken(page)
	if err != nil {
		return nil, err
	}
	values := url.Values{"csrf": {csrf}, "username": {username}, "password": {password}}
	if _, err := httpPostForm(context.Background(), client, strings.TrimRight(baseURL, "/")+"/login", values); err != nil {
		return nil, err
	}
	return client, nil
}

func oaAction(ctx context.Context, client *http.Client, baseURL, draftID, action, decision string) error {
	taskURL := strings.TrimRight(baseURL, "/") + "/tasks/" + url.PathEscape(draftID)
	page, err := httpGet(ctx, client, taskURL)
	if err != nil {
		return err
	}
	csrf, err := csrfToken(page)
	if err != nil {
		return err
	}
	values := url.Values{"csrf": {csrf}}
	if decision != "" {
		values.Set("decision", decision)
	}
	_, err = httpPostForm(ctx, client, taskURL+"/"+action, values)
	return err
}

func oaNarrow(ctx context.Context, client *http.Client, baseURL, draftID string,
	created createdTask, provision provisionConfig) error {
	taskURL := strings.TrimRight(baseURL, "/") + "/tasks/" + url.PathEscape(draftID)
	page, err := httpGet(ctx, client, taskURL)
	if err != nil {
		return err
	}
	csrf, err := csrfToken(page)
	if err != nil {
		return err
	}
	products, _ := json.Marshal(provision.DataProducts)
	columns, _ := json.Marshal(provision.Columns)
	scopes, _ := json.Marshal(provision.Scopes)
	values := url.Values{
		"csrf": {csrf}, "decision": {"narrow"},
		"approved_products": {string(products)}, "approved_columns": {string(columns)},
		"mandatory_scope":      {string(scopes)},
		"max_queries":          {strconv.FormatInt(created.Budget.MaxQueries, 10)},
		"max_result_rows":      {strconv.FormatInt(created.Budget.MaxRows, 10)},
		"max_db_ms":            {strconv.FormatInt(created.Budget.MaxDBMS, 10)},
		"per_query_timeout_ms": {strconv.FormatInt(created.Budget.QueryTimeoutMS, 10)},
		"task_ttl_ms":          {strconv.FormatInt(created.Budget.TaskTTLSeconds*1000/2, 10)},
	}
	_, err = httpPostForm(ctx, client, taskURL+"/decision", values)
	return err
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func csrfToken(page []byte) (string, error) {
	match := csrfPattern.FindSubmatch(page)
	if len(match) != 2 {
		return "", errors.New("OA page omitted CSRF token")
	}
	return string(match[1]), nil
}

func httpGet(ctx context.Context, client *http.Client, target string) ([]byte, error) {
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
		return nil, fmt.Errorf("GET returned %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 2<<20))
}

func httpPostForm(ctx context.Context, client *http.Client, target string, values url.Values) ([]byte, error) {
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
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("POST returned %d", response.StatusCode)
	}
	return body, nil
}

func pathTail(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}
