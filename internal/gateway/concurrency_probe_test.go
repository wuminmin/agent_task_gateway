package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const concurrencyProbeTestToken = "0123456789abcdef0123456789abcdef"

func TestConcurrencyProbeObservesExactServiceArrivalsAndBoundedQueue(t *testing.T) {
	var poolMu sync.Mutex
	poolInUse := 0
	probe, err := NewConcurrencyProbe(ConcurrencyProbeConfig{
		Token: concurrencyProbeTestToken, MaxActive: 3, MaxQueued: 16,
		ConnectorMaxConnections: 4,
		PoolStats: func() sql.DBStats {
			poolMu.Lock()
			defer poolMu.Unlock()
			return sql.DBStats{MaxOpenConnections: 8, InUse: poolInUse}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handlerRelease := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get(ConcurrencyAuthorizationHeader) != "" ||
			request.Header.Get(ConcurrencyRoundHeader) != "" || request.Header.Get(ConcurrencyParticipantHeader) != "" {
			t.Error("probe-private headers reached the real MCP handler")
		}
		poolMu.Lock()
		poolInUse++
		poolMu.Unlock()
		defer func() {
			poolMu.Lock()
			poolInUse--
			poolMu.Unlock()
		}()
		<-handlerRelease
		w.WriteHeader(http.StatusNoContent)
	})
	router := http.NewServeMux()
	router.Handle(ConcurrencyProbeAdminPath+"/", http.StripPrefix(ConcurrencyProbeAdminPath, probe.AdminHandler()))
	router.Handle("/mcp", probe.Middleware(next))
	server := httptest.NewServer(router)
	defer server.Close()

	roundSHA := probeTestSHA("round")
	createProbeRound(t, server.URL, roundSHA, "natural_contention", 10)
	errs := make(chan error, 10)
	for index := 0; index < 10; index++ {
		go func(index int) {
			request, requestErr := http.NewRequest(http.MethodPost, server.URL+"/mcp", nil)
			if requestErr == nil {
				request.Header.Set(ConcurrencyRoundHeader, roundSHA)
				request.Header.Set(ConcurrencyParticipantHeader, probeTestSHA(string(rune('a'+index))))
				request.Header.Set(ConcurrencyAuthorizationHeader, concurrencyProbeTestToken)
				var response *http.Response
				response, requestErr = http.DefaultClient.Do(request)
				if response != nil {
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
					if response.StatusCode != http.StatusNoContent && requestErr == nil {
						requestErr = io.ErrUnexpectedEOF
					}
				}
			}
			errs <- requestErr
		}(index)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshot, err := WaitForConcurrencyProbeSnapshot(ctx, time.Millisecond, func(context.Context) (ConcurrencyProbeSnapshot, error) {
		return getProbeSnapshot(server.URL, roundSHA)
	}, func(value ConcurrencyProbeSnapshot) bool {
		return value.Arrived == 10 && value.PeakBarrierWaiting == 10 &&
			value.PeakActive == 3 && value.PeakQueued == 7
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.UniqueParticipants != 10 || !snapshot.Released || snapshot.GatewayInstanceSHA256 == "" ||
		snapshot.HTTPActiveCapacity != 3 || snapshot.HTTPQueueCapacity != 16 ||
		snapshot.ControlPoolCapacity != 8 || snapshot.ConnectorPoolCapacity != 4 {
		t.Fatalf("service arrival snapshot = %+v", snapshot)
	}
	close(handlerRelease)
	for index := 0; index < 10; index++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	final, err := getProbeSnapshot(server.URL, roundSHA)
	if err != nil {
		t.Fatal(err)
	}
	participants := make([]string, 0, 10)
	for index := 0; index < 10; index++ {
		participants = append(participants, probeTestSHA(string(rune('a'+index))))
	}
	if final.Completed != 10 || final.Active != 0 || final.Queued != 0 || final.BarrierWaiting != 0 ||
		final.Canceled != 0 || final.Rejected != 0 ||
		final.ParticipantSetSHA256 != ConcurrencyParticipantSetSHA256(participants) {
		t.Fatalf("final service snapshot = %+v", final)
	}
	deleteProbeRound(t, server.URL, roundSHA)
}

func TestConcurrencyProbeRejectsUnauthenticatedAndDuplicateParticipants(t *testing.T) {
	probe, err := NewConcurrencyProbe(ConcurrencyProbeConfig{
		Token: concurrencyProbeTestToken, MaxActive: 2, MaxQueued: 2, ConnectorMaxConnections: 1,
		PoolStats: func() sql.DBStats { return sql.DBStats{MaxOpenConnections: 2} },
	})
	if err != nil {
		t.Fatal(err)
	}
	roundSHA := probeTestSHA("duplicate-round")
	probe.rounds[roundSHA] = &concurrencyProbeRound{
		mode: "natural_contention", expected: 2, participants: map[string]struct{}{},
		release: make(chan struct{}),
	}
	participant := probeTestSHA("participant")
	if _, _, err := probe.arrive(roundSHA, participant); err != nil {
		t.Fatal(err)
	}
	if _, _, err := probe.arrive(roundSHA, participant); err == nil {
		t.Fatal("duplicate participant was accepted")
	}
	if probe.authorized("wrong") {
		t.Fatal("wrong probe token was accepted")
	}
}

func TestConcurrencyProbeCapacityFailsClosed(t *testing.T) {
	valid := ConcurrencyProbeConfig{
		Token: concurrencyProbeTestToken, MaxActive: 1, MaxQueued: 1, ConnectorMaxConnections: 1,
		PoolStats: func() sql.DBStats { return sql.DBStats{MaxOpenConnections: 1} },
	}
	for _, mutate := range []func(*ConcurrencyProbeConfig){
		func(value *ConcurrencyProbeConfig) { value.Token = "short" },
		func(value *ConcurrencyProbeConfig) { value.MaxActive = 0 },
		func(value *ConcurrencyProbeConfig) { value.MaxQueued = 0 },
		func(value *ConcurrencyProbeConfig) { value.MaxQueued = -1 },
		func(value *ConcurrencyProbeConfig) { value.ConnectorMaxConnections = 0 },
		func(value *ConcurrencyProbeConfig) { value.PoolStats = func() sql.DBStats { return sql.DBStats{} } },
	} {
		config := valid
		mutate(&config)
		if _, err := NewConcurrencyProbe(config); err == nil {
			t.Fatalf("invalid probe config was accepted: %+v", config)
		}
	}
}

func createProbeRound(t *testing.T, baseURL, roundSHA, mode string, width int) {
	t.Helper()
	body, _ := json.Marshal(concurrencyRoundRequest{RoundSHA256: roundSHA, Mode: mode, ExpectedWidth: width})
	request, _ := http.NewRequest(http.MethodPost, baseURL+ConcurrencyProbeAdminPath+"/rounds", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+concurrencyProbeTestToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create round status = %d", response.StatusCode)
	}
}

func getProbeSnapshot(baseURL, roundSHA string) (ConcurrencyProbeSnapshot, error) {
	request, _ := http.NewRequest(http.MethodGet, baseURL+ConcurrencyProbeAdminPath+"/rounds/"+roundSHA, nil)
	request.Header.Set("Authorization", "Bearer "+concurrencyProbeTestToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return ConcurrencyProbeSnapshot{}, err
	}
	defer response.Body.Close()
	var snapshot ConcurrencyProbeSnapshot
	if response.StatusCode != http.StatusOK {
		return snapshot, io.ErrUnexpectedEOF
	}
	err = json.NewDecoder(response.Body).Decode(&snapshot)
	return snapshot, err
}

func deleteProbeRound(t *testing.T, baseURL, roundSHA string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodDelete, baseURL+ConcurrencyProbeAdminPath+"/rounds/"+roundSHA, nil)
	request.Header.Set("Authorization", "Bearer "+concurrencyProbeTestToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete round status = %d", response.StatusCode)
	}
}

func probeTestSHA(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
