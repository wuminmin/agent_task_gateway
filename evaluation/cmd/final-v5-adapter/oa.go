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
	"regexp"
	"strconv"
	"strings"
	"time"
)

// oaNarrowDecision is the complete set of authorization dimensions exposed
// by oa-demo's public narrow form. Exposure ceilings are deliberately absent:
// the OA preserves those signed manifest fields and does not make them
// editable. Callers must populate every field from the Gateway-selected task
// request; the Adapter is allowed to reduce only TaskTTLMS.
type oaNarrowDecision struct {
	Products       []string
	Columns        map[string][]string
	MandatoryScope map[string]any
	MaxQueries     int64
	MaxResultRows  int64
	MaxDBMS        int64
	QueryTimeoutMS int64
	TaskTTLMS      int64
}

func oaClient(baseURL, username, password string, timeout time.Duration) (*http.Client, error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: timeout}
	page, err := httpGet(context.Background(), client, strings.TrimRight(baseURL, "/")+"/login", 2<<20)
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
	page, err := httpGet(ctx, client, taskURL, 2<<20)
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

// oaNarrowAction exercises the same authenticated Bob browser endpoint as a
// human "narrow" decision. It posts an explicit complete form instead of
// copying editable values out of HTML, so every resource ceiling remains
// bound to the Gateway's structured request_data_task response.
func oaNarrowAction(ctx context.Context, client *http.Client, baseURL, draftID string,
	decision oaNarrowDecision) error {
	if client == nil || strings.TrimSpace(baseURL) == "" || strings.TrimSpace(draftID) == "" {
		return errors.New("OA narrow action is incomplete")
	}
	values, err := decision.formValues()
	if err != nil {
		return err
	}
	taskURL := strings.TrimRight(baseURL, "/") + "/tasks/" + url.PathEscape(draftID)
	page, err := httpGet(ctx, client, taskURL, 2<<20)
	if err != nil {
		return err
	}
	csrf, err := csrfToken(page)
	if err != nil {
		return err
	}
	values.Set("csrf", csrf)
	_, err = httpPostForm(ctx, client, taskURL+"/decision", values)
	return err
}

func (decision oaNarrowDecision) formValues() (url.Values, error) {
	if len(decision.Products) == 0 || len(decision.Columns) == 0 || len(decision.MandatoryScope) == 0 {
		return nil, errors.New("OA narrow authorization envelope is incomplete")
	}
	for _, product := range decision.Products {
		if strings.TrimSpace(product) == "" || len(decision.Columns[product]) == 0 {
			return nil, errors.New("OA narrow products and columns are incomplete")
		}
	}
	if decision.MaxQueries <= 0 || decision.MaxResultRows <= 0 || decision.MaxDBMS <= 0 ||
		decision.QueryTimeoutMS <= 0 || decision.TaskTTLMS <= 0 {
		return nil, errors.New("OA narrow resource budgets must be positive")
	}
	products, err := json.Marshal(decision.Products)
	if err != nil {
		return nil, err
	}
	columns, err := json.Marshal(decision.Columns)
	if err != nil {
		return nil, err
	}
	scope, err := json.Marshal(decision.MandatoryScope)
	if err != nil {
		return nil, err
	}
	return url.Values{
		"decision":             {"narrow"},
		"approved_products":    {string(products)},
		"approved_columns":     {string(columns)},
		"mandatory_scope":      {string(scope)},
		"max_queries":          {strconv.FormatInt(decision.MaxQueries, 10)},
		"max_result_rows":      {strconv.FormatInt(decision.MaxResultRows, 10)},
		"max_db_ms":            {strconv.FormatInt(decision.MaxDBMS, 10)},
		"per_query_timeout_ms": {strconv.FormatInt(decision.QueryTimeoutMS, 10)},
		"task_ttl_ms":          {strconv.FormatInt(decision.TaskTTLMS, 10)},
	}, nil
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func csrfToken(page []byte) (string, error) {
	match := csrfPattern.FindSubmatch(page)
	if len(match) != 2 {
		return "", errors.New("OA page omitted CSRF token")
	}
	return string(match[1]), nil
}

func httpGet(ctx context.Context, client *http.Client, target string, maximum int64) ([]byte, error) {
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
	return readExactlyBounded(response.Body, maximum)
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
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return nil, fmt.Errorf("POST returned %d", response.StatusCode)
	}
	return body, nil
}
