// Command v4-million-oracle audits the committed maximum-point V4 effect
// without invoking the Gateway ordinal derivation hot path.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"taskbound.local/agent-data-gateway/evaluation/v4oracle"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "V4 million-Fact oracle failed:", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg v4oracle.Config
	var output, businessEnv, controlEnv string
	var timeout time.Duration
	flag.StringVar(&cfg.ResultsPath, "results", "", "digest-bound V4 full results.json")
	flag.StringVar(&cfg.ArtifactDir, "artifact-dir", "", "verified snapshot publication artifact root")
	flag.StringVar(&cfg.SpoolParent, "spool-parent", "", "private disk-backed parent for bounded external-sort runs")
	flag.StringVar(&cfg.RepositoryRoot, "repository-root", "", "read-only repository root used to bind the complete oracle dependency source scope")
	flag.Int64Var(&cfg.SortMemoryBytes, "sort-memory-bytes", 16<<20, "maximum in-memory record buffer per active external sorter")
	flag.StringVar(&output, "output", "", "exclusive machine-verifiable oracle report path")
	flag.StringVar(&businessEnv, "business-dsn-env", "V4_EVAL_BUSINESS_DSN", "environment variable containing the Business PostgreSQL DSN")
	flag.StringVar(&controlEnv, "control-dsn-env", "V4_EVAL_CONTROL_DSN", "environment variable containing the Control PostgreSQL DSN")
	flag.DurationVar(&timeout, "timeout", 30*time.Minute, "offline oracle timeout")
	flag.Parse()
	if flag.NArg() != 0 || strings.TrimSpace(output) == "" || cfg.ResultsPath == "" || cfg.ArtifactDir == "" || cfg.SpoolParent == "" || cfg.RepositoryRoot == "" {
		return errors.New("-results, -artifact-dir, -spool-parent, -repository-root, and -output are required")
	}
	if _, err := os.Lstat(output); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("refusing to overwrite an existing oracle report")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return err
	}
	businessDSN, controlDSN := os.Getenv(businessEnv), os.Getenv(controlEnv)
	if strings.TrimSpace(businessDSN) == "" || strings.TrimSpace(controlDSN) == "" {
		return fmt.Errorf("%s and %s must be set", businessEnv, controlEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	business, err := pgxpool.New(ctx, businessDSN)
	if err != nil {
		return err
	}
	defer business.Close()
	control, err := pgxpool.New(ctx, controlDSN)
	if err != nil {
		return err
	}
	defer control.Close()
	report, verifyErr := v4oracle.Verify(ctx, business, control, cfg)
	raw, marshalErr := json.MarshalIndent(report, "", "  ")
	if marshalErr != nil {
		return marshalErr
	}
	raw = append(raw, '\n')
	if err := writeExclusiveAtomic(output, raw); err != nil {
		return err
	}
	return verifyErr
}

func writeExclusiveAtomic(path string, raw []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".v4-million-oracle-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Link(temporaryPath, path)
}
