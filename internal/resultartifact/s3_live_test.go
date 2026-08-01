//go:build taskgate_integration

package resultartifact

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestS3BackendLive is opt-in so ordinary unit runs need no object store. The
// Compose smoke/acceptance environment supplies bucket-scoped credentials.
func TestS3BackendLive(t *testing.T) {
	endpoint := os.Getenv("RESULT_ARTIFACT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("RESULT_ARTIFACT_TEST_S3_ENDPOINT is required for the S3 artifact test")
	}
	backend, err := NewS3Backend(S3Config{
		Endpoint: endpoint, Region: envOr("RESULT_ARTIFACT_TEST_S3_REGION", "us-east-1"),
		Bucket:    os.Getenv("RESULT_ARTIFACT_TEST_S3_BUCKET"),
		AccessKey: os.Getenv("RESULT_ARTIFACT_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("RESULT_ARTIFACT_TEST_S3_SECRET_KEY"), ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}
	if err := backend.Ready(t.Context()); err != nil {
		t.Fatalf("S3 readiness: %v", err)
	}
	tempDir := t.TempDir()
	if err := os.Chmod(tempDir, 0o700); err != nil {
		t.Fatalf("restrict temp directory: %v", err)
	}
	manager, err := NewManager(backend, newArtifactTestCipher(t), tempDir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	resultID := fmt.Sprintf("res_s3_live_%d", time.Now().UnixNano())
	staged, err := manager.Stage(t.Context(), StageRequest{
		ResultID: resultID, TaskID: "task_s3_live",
		StagingKey: "staging/live/" + resultID + ".parquet.enc",
		ObjectKey:  "results/live/" + resultID + ".parquet.enc",
		Columns:    []Column{{Name: "label", DataTypeOID: 25}, {Name: "amount", DataTypeOID: 20}},
		Rows:       [][]any{{"对象存储", int64(42)}},
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	t.Cleanup(func() {
		_ = manager.Delete(context.Background(), staged.StagingKey)
		_ = manager.Delete(context.Background(), staged.ObjectKey)
	})
	if _, err := manager.Promote(t.Context(), staged); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	// Recovery must accept the authenticated canonical object even after the
	// private staging upload has already been deleted.
	if _, err := manager.Promote(t.Context(), staged); err != nil {
		t.Fatalf("idempotent Promote: %v", err)
	}
	encoded, err := manager.ReadParquet(t.Context(), artifactReference(staged), staged.ParquetSize)
	if err != nil {
		t.Fatalf("ReadParquet: %v", err)
	}
	rows, err := ReadParquet(encoded, staged.ResultID, staged.Schema, 0, 1)
	if err != nil {
		t.Fatalf("decode Parquet: %v", err)
	}
	if len(rows) != 1 || len(rows[0]) != 2 || rows[0][0] != "对象存储" || rows[0][1] != int64(42) {
		t.Fatalf("S3 Parquet round trip = %#v", rows)
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
