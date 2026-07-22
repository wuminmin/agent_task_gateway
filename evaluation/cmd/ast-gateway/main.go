// Command ast-gateway is the evaluation-only AST baseline. It deliberately
// includes SQL authorization, rewrite, and the read-only connector, but no
// task state, approval, budget ledger, control database, or receipts.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

type config struct {
	ListenAddress    string                   `json:"listen_address"`
	DSNEnv           string                   `json:"dsn_env"`
	TokenEnv         string                   `json:"token_env"`
	StatementTimeout string                   `json:"statement_timeout"`
	MaxRows          int64                    `json:"max_rows"`
	MaxConnections   int32                    `json:"max_connections"`
	AllowedFunctions []string                 `json:"allowed_functions"`
	AllowedOperators []string                 `json:"allowed_operators"`
	Products         []sqlpolicy.ProductGrant `json:"products"`
}

type server struct {
	engine    *sqlpolicy.Engine
	connector *dataconnector.Connector
	grant     sqlpolicy.Grant
	token     string
	timeout   time.Duration
	maxRows   int64
}

type queryRequest struct {
	SQL        string `json:"sql"`
	RequestID  string `json:"request_id"`
	Experiment string `json:"experiment"`
}

type queryResponse struct {
	RequestID  string                 `json:"request_id"`
	RowCount   int64                  `json:"row_count"`
	Rows       [][]any                `json:"rows"`
	DatabaseMS float64                `json:"database_ms"`
	Component  map[string]float64     `json:"component_ms"`
	Metadata   map[string]interface{} `json:"metadata"`
}

func main() {
	configPath := flag.String("config", "", "path to the AST baseline JSON configuration")
	flag.Parse()
	if *configPath == "" {
		log.Fatal("ast-gateway: -config is required")
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	dsn := os.Getenv(cfg.DSNEnv)
	if dsn == "" {
		log.Fatalf("ast-gateway: environment variable %s is required", cfg.DSNEnv)
	}
	token := ""
	if cfg.TokenEnv != "" {
		token = os.Getenv(cfg.TokenEnv)
		if token == "" {
			log.Fatalf("ast-gateway: environment variable %s is required", cfg.TokenEnv)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN:              dsn,
		StatementTimeout: cfg.timeout(),
		ConnectTimeout:   10 * time.Second,
		MaxRows:          cfg.MaxRows,
		MaxConnections:   cfg.MaxConnections,
		ApplicationName:  "taskgate-evaluation-ast-only",
	})
	cancel()
	if err != nil {
		log.Fatalf("ast-gateway: connect to PostgreSQL: %v", err)
	}
	defer connector.Close()

	handler := &server{
		engine: sqlpolicy.New(sqlpolicy.Config{
			AllowedFunctions: cfg.AllowedFunctions,
			AllowedOperators: cfg.AllowedOperators,
		}),
		connector: connector,
		grant:     sqlpolicy.Grant{Products: cfg.Products},
		token:     token,
		timeout:   cfg.timeout(),
		maxRows:   cfg.MaxRows,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/ready", handler.ready)
	mux.HandleFunc("POST /query", handler.query)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.timeout() + 2*time.Second,
		WriteTimeout:      cfg.timeout() + 2*time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()

	log.Printf("ast-gateway: listening on %s", cfg.ListenAddress)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("ast-gateway: read config: %w", err)
	}
	var cfg config
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return config{}, fmt.Errorf("ast-gateway: decode config: %w", err)
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = ":8088"
	}
	if cfg.DSNEnv == "" || cfg.MaxRows <= 0 || cfg.MaxConnections <= 0 || len(cfg.Products) == 0 {
		return config{}, errors.New("ast-gateway: dsn_env, positive max_rows/max_connections, and products are required")
	}
	if _, err := time.ParseDuration(cfg.StatementTimeout); err != nil {
		return config{}, fmt.Errorf("ast-gateway: invalid statement_timeout: %w", err)
	}
	return cfg, nil
}

func (cfg config) timeout() time.Duration {
	timeout, _ := time.ParseDuration(cfg.StatementTimeout)
	return timeout
}

func (s *server) authenticate(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && len(presented) == len(s.token) && subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1
}

func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.connector.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true})
}

func (s *server) query(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var request queryRequest
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.SQL) == "" || request.RequestID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}

	policyStarted := time.Now()
	decision, err := s.engine.Authorize(sqlpolicy.Request{
		SQL: request.SQL, Grant: s.grant, RowLimit: s.maxRows,
	})
	policyDuration := time.Since(policyStarted)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "policy_denied"})
		return
	}

	result, err := s.connector.Query(r.Context(), dataconnector.QueryRequest{
		SQL: decision.SQL, StatementTimeout: s.timeout, MaxRows: s.maxRows,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, queryResponse{
		RequestID:  request.RequestID,
		RowCount:   result.RowCount,
		Rows:       result.Rows,
		DatabaseMS: milliseconds(result.DatabaseTime),
		Component: map[string]float64{
			"policy":   milliseconds(policyDuration),
			"database": milliseconds(result.DatabaseTime),
		},
		Metadata: map[string]interface{}{
			"fingerprint": decision.Fingerprint,
			"truncated":   result.Truncated,
		},
	})
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration.Nanoseconds()) / float64(time.Millisecond)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
