//go:build taskgate_scale

// This case regenerates the tracked exposure-scale review against live PostgreSQL.
// It carries a multi-minute budget and never finished inside the acceptance
// run's per-package limit, so it belongs on the taskgate_scale lane the
// repository already reserves for costly scale work.

package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTrackedExposureScaleReviewRegeneratesAgainstPostgreSQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN"))
	}
	if dsn == "" {
		t.Fatal("BUSINESS_TEST_POSTGRES_DSN or TASKGATE_FINAL_V5_BUSINESS_DSN is required; this live review test must not skip")
	}
	root := repositoryRoot(t)
	temporary := t.TempDir()
	artifactRoot := filepath.Join(temporary, "artifacts")
	if err := os.Mkdir(artifactRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	outputDirectory := filepath.Join(temporary, "review")
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
	defer cancel()
	generated, err := generateReview(ctx, generateOptions{
		RepositoryRoot: root, OutputDirectory: outputDirectory, ArtifactRoot: artifactRoot, DSN: dsn,
	})
	if err != nil {
		t.Fatalf("generate live exposure-scale review: %v", err)
	}
	tracked, err := validateReviewDirectory(trackedReviewDirectory(t))
	if err != nil {
		t.Fatalf("validate tracked review directory: %v", err)
	}
	if !reflect.DeepEqual(generated, tracked) {
		t.Fatal("live-generated review report differs from the tracked review candidate")
	}
	if err := compareReviewDirectories(trackedReviewDirectory(t), outputDirectory); err != nil {
		t.Fatalf("live-generated review bytes differ from tracked bytes: %v", err)
	}
	if generated.Database.MaximumPreparedStatementCount != 0 ||
		generated.Database.QueryExecMode != "simple_protocol" {
		t.Fatalf("live review database protocol evidence is incomplete: %+v", generated.Database)
	}
	t.Logf("regenerated %d-row/%d-Fact review; semantic=%s ordinal=%s prepared_statement_max=%d",
		generated.Publication.RowCount, generated.SemanticOrdinalLink.ExpectedOrdinalCardinality,
		generated.SemanticOrdinalLink.OracleSemantic.SetSHA256,
		generated.SemanticOrdinalLink.ExpectedOrdinalSetSHA256,
		generated.Database.MaximumPreparedStatementCount)
}
