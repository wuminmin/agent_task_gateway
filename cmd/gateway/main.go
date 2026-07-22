package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	gatewayapp "taskbound.local/agent-data-gateway/internal/gateway"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logicalCatalog, err := catalog.Load(requiredEnv("CATALOG_PATH"))
	if err != nil {
		logger.Error("catalog validation failed", "error", err)
		os.Exit(1)
	}
	key, err := control.ParseAES256Key(requiredEnv("GATEWAY_DATA_KEY"))
	if err != nil {
		logger.Error("invalid result encryption key", "error", err)
		os.Exit(1)
	}
	cipher, err := control.NewAES256GCM(key)
	if err != nil {
		logger.Error("initialize result cipher", "error", err)
		os.Exit(1)
	}
	store, err := control.Open(ctx, requiredEnv("CONTROL_POSTGRES_DSN"), cipher)
	if err != nil {
		logger.Error("open control store", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	aliceToken := requiredEnv("TASKBOUND_ALICE_TOKEN")
	carolToken := requiredEnv("TASKBOUND_CAROL_TOKEN")
	if err := bootstrapPrincipal(ctx, store, control.Principal{ID: "principal_alice", Subject: "alice", Role: "query", TokenHash: tokenDigest(aliceToken)}); err != nil {
		logger.Error("bootstrap Alice", "error", err)
		os.Exit(1)
	}
	if err := bootstrapPrincipal(ctx, store, control.Principal{ID: "principal_carol", Subject: "carol", Role: "auditor", TokenHash: tokenDigest(carolToken)}); err != nil {
		logger.Error("bootstrap Carol", "error", err)
		os.Exit(1)
	}
	oaClient, err := approval.NewClient(requiredEnv("OA_BASE_URL"), requiredEnv("OA_SERVICE_TOKEN"), nil)
	if err != nil {
		logger.Error("initialize OA adapter", "error", err)
		os.Exit(1)
	}
	oaReceiptVerifier, err := approval.NewReceiptVerifierFromBase64(
		requiredEnv("OA_RECEIPT_KEY_ID"), requiredEnv("OA_RECEIPT_PUBLIC_KEY"),
	)
	if err != nil {
		logger.Error("initialize OA approval receipt verifier", "error", err)
		os.Exit(1)
	}
	expectedSchema, err := expectedReportingSchema(logicalCatalog)
	if err != nil {
		logger.Error("build reporting schema attestation", "error", err)
		os.Exit(1)
	}
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: requiredEnv("POSTGRES_DSN"), StatementTimeout: 5 * time.Second,
		ConnectTimeout: 10 * time.Second, MaxRows: 10000, MaxConnections: 4,
		ExpectedSchema: expectedSchema,
	})
	if err != nil {
		logger.Error("initialize PostgreSQL connector", "error", err)
		os.Exit(1)
	}
	defer connector.Close()
	queryReceiptSigner, err := queryreceipt.NewSignerFromBase64(
		requiredEnv("GATEWAY_RECEIPT_KEY_ID"), requiredEnv("GATEWAY_RECEIPT_PRIVATE_KEY"),
	)
	if err != nil {
		logger.Error("initialize query receipt signer", "error", err)
		os.Exit(1)
	}
	service, err := gatewayapp.New(gatewayapp.Config{
		Catalog: logicalCatalog, Store: store, Approval: oaClient,
		Connector: connector, CallbackSecret: requiredEnv("OA_CALLBACK_SECRET"), Logger: logger,
		ReceiptVerifier: oaReceiptVerifier, QueryReceiptSigner: queryReceiptSigner, Background: ctx,
	})
	if err != nil {
		logger.Error("initialize gateway service", "error", err)
		os.Exit(1)
	}
	authenticator := mcp.NewStaticAuthenticator([]mcp.TokenIdentity{
		{Token: aliceToken, Principal: mcp.Principal{ID: "principal_alice", Subject: "alice", Role: "query"}},
		{Token: carolToken, Principal: mcp.Principal{ID: "principal_carol", Subject: "carol", Role: "auditor"}},
	})
	mcpServer, err := mcp.NewServer(authenticator, service, logger)
	if err != nil {
		logger.Error("initialize MCP server", "error", err)
		os.Exit(1)
	}

	router := chi.NewRouter()
	router.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	router.Get("/health/ready", readiness(store, connector, service))
	router.Handle("/mcp", mcpServer)
	router.Handle("/api/v1/oa/callback", service.OACallbackHandler())

	go sweepExpired(ctx, store, logger)

	server := &http.Server{
		Addr: env("GATEWAY_ADDR", ":8082"), Handler: router, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 130 * time.Second, WriteTimeout: 130 * time.Second, IdleTimeout: 60 * time.Second,
	}
	go func() {
		logger.Info("gateway listening", "address", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("gateway stopped", "error", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

func expectedReportingSchema(logicalCatalog *catalog.Catalog) ([]dataconnector.ViewSchema, error) {
	result := make([]dataconnector.ViewSchema, 0, len(logicalCatalog.Products))
	for _, product := range logicalCatalog.Products {
		schema, view, ok := strings.Cut(product.ReportingView, ".")
		if !ok || schema == "" || view == "" {
			return nil, errors.New("validated catalog contains an invalid reporting view")
		}
		columns := make([]dataconnector.SchemaColumn, 0, len(product.Fields))
		for _, field := range product.Fields {
			columns = append(columns, dataconnector.SchemaColumn{Name: field.Name, PostgreSQLType: field.Type})
		}
		result = append(result, dataconnector.ViewSchema{Schema: schema, View: view, Columns: columns})
	}
	return result, nil
}

func requiredEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		slog.Error("required environment variable is missing", "name", key)
		os.Exit(1)
	}
	return value
}

func tokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func bootstrapPrincipal(ctx context.Context, store *control.Store, wanted control.Principal) error {
	existing, err := store.GetPrincipal(ctx, wanted.ID)
	if errors.Is(err, control.ErrNotFound) {
		wanted.CreatedAt = time.Now().UTC()
		return store.CreatePrincipal(ctx, wanted)
	}
	if err != nil {
		return err
	}
	if existing.Subject != wanted.Subject || existing.Role != wanted.Role || existing.TokenHash != wanted.TokenHash || existing.DisabledAt != nil {
		return errors.New("persisted principal does not match configured identity; reset the demo volume or restore the token")
	}
	return nil
}

func readiness(store *control.Store, connector *dataconnector.Connector, service *gatewayapp.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := store.DB().PingContext(ctx); err != nil {
			http.Error(w, "control store unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := connector.Ping(ctx); err != nil {
			http.Error(w, "data source unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := service.ReadyError(); err != nil {
			http.Error(w, "query settlement pending", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func sweepExpired(ctx context.Context, store *control.Store, logger *slog.Logger) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			afterID := ""
			for {
				tasks, err := store.ListTasks(ctx, control.TaskFilter{State: control.TaskActive, AfterID: afterID, Limit: 500})
				if err != nil {
					logger.Warn("list tasks for expiry sweep", "after_id", afterID, "error", err)
					break
				}
				for _, task := range tasks {
					if task.ExpiresAt == nil || now.Before(*task.ExpiresAt) {
						continue
					}
					_, err := store.TransitionTask(ctx, control.TaskTransition{
						TaskID: task.ID, ExpectedFrom: control.TaskActive, To: control.TaskArchived,
						Reason: control.TerminalExpired, Actor: "system",
					})
					if err != nil && !errors.Is(err, control.ErrInvalidStateChange) {
						logger.Warn("archive expired task", "task_id", task.ID, "error", err)
					}
				}
				if len(tasks) < 500 {
					break
				}
				afterID = tasks[len(tasks)-1].ID
			}
		}
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
