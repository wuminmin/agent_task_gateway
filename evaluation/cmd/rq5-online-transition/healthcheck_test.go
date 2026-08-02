package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type localOAHealthClientFunc func(*http.Request) (*http.Response, error)

func (function localOAHealthClientFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestProbeLocalOAReadinessAcceptsOnlyNoContent(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "ready", status: http.StatusNoContent},
		{name: "wrong-success-status", status: http.StatusOK, wantErr: true},
		{name: "unavailable", status: http.StatusServiceUnavailable, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(test.status)
			}))
			defer server.Close()

			err := probeLocalOAReadiness(t.Context(), server.Client(), server.URL)
			if (err != nil) != test.wantErr {
				t.Fatalf("probe status %d error = %v, wantErr %v", test.status, err, test.wantErr)
			}
		})
	}
}

func TestProbeLocalOAReadinessFailsClosedOnTransportError(t *testing.T) {
	want := errors.New("transport failed")
	client := localOAHealthClientFunc(func(*http.Request) (*http.Response, error) {
		return nil, want
	})
	if err := probeLocalOAReadiness(context.Background(), client, "http://oa.invalid/health/ready"); !errors.Is(err, want) {
		t.Fatalf("transport error = %v, want %v", err, want)
	}
}

func TestRunLocalOAHealthcheckRejectsNonLocalEndpoint(t *testing.T) {
	if err := runLocalOAHealthcheck("http://oa.invalid/health/ready"); err == nil {
		t.Fatal("non-local OA health endpoint was accepted")
	}
}
