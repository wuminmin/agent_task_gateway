package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	exposureeval "taskbound.local/agent-data-gateway/evaluation/exposure"
)

func main() {
	dsn := strings.TrimSpace(os.Getenv("EXPOSURE_TEST_POSTGRES_DSN"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "exposure evaluation failed: EXPOSURE_TEST_POSTGRES_DSN is required for the PostgreSQL oracle campaign")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := exposureeval.RunPostgreSQL(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "exposure evaluation failed:", err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, "encode exposure report:", err)
		os.Exit(1)
	}
}
