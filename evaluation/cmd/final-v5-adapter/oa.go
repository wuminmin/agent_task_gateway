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
	"sync"
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

// oaSession is the single live browser session shared by every copy of an
// oaAccount value. gen advances on each successful login so that a burst of
// concurrent operations that all notice the same dead session causes exactly
// one re-login instead of a stampede of bcrypt logins.
type oaSession struct {
	mu     sync.Mutex
	client *http.Client
	gen    uint64
}

// oaAccount holds one OA browser identity. The OA session token expires 8h
// after login while a campaign runs longer, and an expired session turns every
// authenticated POST into a silent redirect-to-/login with a final 200 (P69
// root cause). One session is therefore kept and reused, every response is
// checked for having landed on the login page, and only that signal triggers a
// re-login plus a single replay of the action. The session is deliberately not
// proved live by a separate probe: the action's own GET already carries that
// signal, while GET /tasks costs the OA a full scan of every draft it holds,
// which made a campaign quadratic in the number of tasks (P72).
type oaAccount struct {
	baseURL  string
	username string
	password string
	timeout  time.Duration
	session  *oaSession
}

func newOAAccount(baseURL, username, password string, timeout time.Duration) oaAccount {
	return oaAccount{baseURL: strings.TrimRight(baseURL, "/"), username: username,
		password: password, timeout: timeout, session: &oaSession{}}
}

// login performs the credential exchange on a fresh cookie jar. It does not
// probe /tasks: that probe is O(drafts held by the OA) and, run once per
// action, made a whole campaign quadratic (P72).
func (account oaAccount) login(ctx context.Context) (*http.Client, error) {
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
	return client, nil
}

// newSession logs in and proves the session is live. It is used for start-up
// credential verification, where one extra probe is free and a wrong password
// must fail the run immediately instead of measuring no-ops.
func (account oaAccount) newSession(ctx context.Context) (*http.Client, error) {
	client, err := account.login(ctx)
	if err != nil {
		return nil, err
	}
	if err := account.verifySessionAlive(ctx, client); err != nil {
		return nil, err
	}
	return client, nil
}

// current returns the shared session, logging in on first use. The returned
// generation identifies that session, so a caller that finds it dead can ask
// for a replacement without racing every other caller into a fresh login.
func (account oaAccount) current(ctx context.Context) (*http.Client, uint64, error) {
	account.session.mu.Lock()
	defer account.session.mu.Unlock()
	if account.session.client == nil {
		client, err := account.login(ctx)
		if err != nil {
			return nil, 0, err
		}
		account.session.client = client
		account.session.gen++
	}
	return account.session.client, account.session.gen, nil
}

// refresh replaces the session only if nobody else already replaced the
// generation the caller found dead.
func (account oaAccount) refresh(ctx context.Context, stale uint64) (*http.Client, uint64, error) {
	account.session.mu.Lock()
	defer account.session.mu.Unlock()
	if account.session.client != nil && account.session.gen != stale {
		return account.session.client, account.session.gen, nil
	}
	client, err := account.login(ctx)
	if err != nil {
		return nil, 0, err
	}
	account.session.client = client
	account.session.gen++
	return account.session.client, account.session.gen, nil
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

// perform runs one authenticated OA browser action on the shared session.
// Landing on /login is never accepted as success: it means the session died,
// and it is answered with exactly one re-login and one replay. The OA rejects
// an unauthenticated request inside requireSession before the handler runs
// (internal/oademo/server.go), so the attempt that was redirected cannot have
// applied the action, and replaying it cannot apply it twice.
func (account oaAccount) perform(ctx context.Context, draftID, action string, values url.Values) error {
	client, gen, err := account.current(ctx)
	if err != nil {
		return err
	}
	dead, err := account.attempt(ctx, client, draftID, action, values)
	if err != nil {
		return err
	}
	if !dead {
		return nil
	}
	client, _, err = account.refresh(ctx, gen)
	if err != nil {
		return fmt.Errorf("OA re-login for %s before %s on draft %s: %w",
			account.username, action, draftID, err)
	}
	dead, err = account.attempt(ctx, client, draftID, action, values)
	if err != nil {
		return err
	}
	if dead {
		return fmt.Errorf("OA dropped the %s session twice during %s on draft %s",
			account.username, action, draftID)
	}
	return nil
}

// attempt runs the GET/POST pair once. It reports dead=true when either
// response landed on the login page, which is the OA's silent answer for an
// expired session and must never be read as success.
func (account oaAccount) attempt(ctx context.Context, client *http.Client,
	draftID, action string, values url.Values) (bool, error) {
	taskURL := account.baseURL + "/tasks/" + url.PathEscape(draftID)
	page, finalURL, err := httpGet(ctx, client, taskURL, 2<<20)
	if err != nil {
		return false, err
	}
	if landedOnLogin(finalURL) {
		return true, nil
	}
	csrf, err := csrfToken(page)
	if err != nil {
		return false, err
	}
	values.Set("csrf", csrf)
	_, finalURL, err = httpPostForm(ctx, client, taskURL+"/"+action, values)
	if err != nil {
		return false, err
	}
	if landedOnLogin(finalURL) {
		return true, nil
	}
	return false, nil
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
