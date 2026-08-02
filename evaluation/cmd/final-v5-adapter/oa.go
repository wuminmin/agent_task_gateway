package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

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
