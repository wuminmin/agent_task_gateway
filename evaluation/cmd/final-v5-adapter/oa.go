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

// oaAccount holds one OA browser identity. Sessions are never reused across
// actions: the OA session token expires 8h after login while a campaign runs
// longer, and an expired session turns every authenticated POST into a silent
// redirect-to-/login with a final 200 (P69 root cause). Each action therefore
// opens a fresh session, verifies it is live, acts, and checks it never landed
// on the login page.
type oaAccount struct {
	baseURL  string
	username string
	password string
	timeout  time.Duration
}

func newOAAccount(baseURL, username, password string, timeout time.Duration) oaAccount {
	return oaAccount{baseURL: strings.TrimRight(baseURL, "/"), username: username, password: password, timeout: timeout}
}

// newSession logs in on a fresh cookie jar and proves the session is live
// with a redirect-free probe before returning the client. A wrong password
// surfaces here as an error instead of a silent redirect chain.
func (account oaAccount) newSession(ctx context.Context) (*http.Client, error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: account.timeout}
	page, _, err := httpGet(ctx, client, account.baseURL+"/login", 2<<20)
	if err != nil {
		return nil, err
	}
	csrf, err := csrfToken(page)
	if err != nil {
		return nil, err
	}
	values := url.Values{"csrf": {csrf}, "username": {account.username}, "password": {account.password}}
	if _, _, err := httpPostForm(ctx, client, account.baseURL+"/login", values); err != nil {
		return nil, err
	}
	if err := account.verifySessionAlive(ctx, client); err != nil {
		return nil, err
	}
	return client, nil
}

// verifySessionAlive asks the OA directly whether this client is logged in:
// with redirect-following disabled, GET /tasks answers 200 for a live session
// and 303 to /login for a dead or never-established one.
func (account oaAccount) verifySessionAlive(ctx context.Context, client *http.Client) error {
	probe := &http.Client{Jar: client.Jar, Timeout: account.timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, account.baseURL+"/tasks", nil)
	if err != nil {
		return err
	}
	response, err := probe.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("OA session for %s is not live: probe returned %d (location %q)",
			account.username, response.StatusCode, response.Header.Get("Location"))
	}
	return nil
}

func landedOnLogin(finalURL string) bool {
	parsed, err := url.Parse(finalURL)
	if err != nil {
		return false
	}
	return parsed.Path == "/login" || strings.HasSuffix(parsed.Path, "/login")
}

func oaAction(ctx context.Context, account oaAccount, draftID, action, decision string) error {
	values := url.Values{}
	if decision != "" {
		values.Set("decision", decision)
	}
	return account.perform(ctx, draftID, action, values)
}

// oaNarrowAction exercises the same authenticated Bob browser endpoint as a
// human "narrow" decision. It posts an explicit complete form instead of
// copying editable values out of HTML, so every resource ceiling remains
// bound to the Gateway's structured request_data_task response.
func oaNarrowAction(ctx context.Context, account oaAccount, draftID string,
	decision oaNarrowDecision) error {
	if strings.TrimSpace(account.baseURL) == "" || strings.TrimSpace(draftID) == "" {
		return errors.New("OA narrow action is incomplete")
	}
	values, err := decision.formValues()
	if err != nil {
		return err
	}
	return account.perform(ctx, draftID, "decision", values)
}

// perform runs one authenticated OA browser action on a freshly verified
// session. Landing on /login at any step is a hard error: the OA's silent
// answer for a dead session must never look like success again.
func (account oaAccount) perform(ctx context.Context, draftID, action string, values url.Values) error {
	client, err := account.newSession(ctx)
	if err != nil {
		return err
	}
	taskURL := account.baseURL + "/tasks/" + url.PathEscape(draftID)
	page, finalURL, err := httpGet(ctx, client, taskURL, 2<<20)
	if err != nil {
		return err
	}
	if landedOnLogin(finalURL) {
		return fmt.Errorf("OA dropped the %s session before %s on draft %s: landed on %s",
			account.username, action, draftID, finalURL)
	}
	csrf, err := csrfToken(page)
	if err != nil {
		return err
	}
	values.Set("csrf", csrf)
	_, finalURL, err = httpPostForm(ctx, client, taskURL+"/"+action, values)
	if err != nil {
		return err
	}
	if landedOnLogin(finalURL) {
		return fmt.Errorf("OA dropped the %s session during %s on draft %s: landed on %s",
			account.username, action, draftID, finalURL)
	}
	return nil
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

func httpGet(ctx context.Context, client *http.Client, target string, maximum int64) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	finalURL := response.Request.URL.String()
	if response.StatusCode != http.StatusOK {
		return nil, finalURL, fmt.Errorf("GET returned %d", response.StatusCode)
	}
	body, err := readExactlyBounded(response.Body, maximum)
	return body, finalURL, err
}

func httpPostForm(ctx context.Context, client *http.Client, target string, values url.Values) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	finalURL := response.Request.URL.String()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, finalURL, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return nil, finalURL, fmt.Errorf("POST returned %d", response.StatusCode)
	}
	return body, finalURL, nil
}
