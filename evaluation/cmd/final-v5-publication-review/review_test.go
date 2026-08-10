package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/finalv5linker"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

func TestTrackedExposureScaleReviewDirectoryIsClosed(t *testing.T) {
	report, err := validateReviewDirectory(trackedReviewDirectory(t))
	if err != nil {
		t.Fatalf("validate tracked review directory: %v", err)
	}
	if report.ProvSQLManifestSet.AggregateSHA256 != provSQLManifestSetSHA ||
		report.ScaleManifestSet.AggregateSHA256 != scaleManifestSetSHA ||
		report.ScaleUnion.Role != reviewSemanticRole || report.ScaleUnion.SetSHA256 != reviewScaleUnionSHA ||
		report.SemanticOrdinalLink.ActualOrdinalSource !=
			finalv5linker.ActualSetSourceReviewedPublicationUniverse ||
		report.SemanticOrdinalLink.ExpectedOrdinalCardinality !=
			uint64(finalv5oracle.ExposureScaleMaximumDatasetFacts) {
		t.Fatalf("tracked review closure is incomplete: %+v", report)
	}
}

func TestReviewDirectoryRejectsCompanionDriftAndExtraFiles(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "descriptor-bound companion drift",
			mutate: func(t *testing.T, directory string) {
				t.Helper()
				path := filepath.Join(directory, "catalog.yaml")
				value, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(value, []byte("# drift\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "extra file",
			mutate: func(t *testing.T, directory string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(directory, "unreviewed.json"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "review")
			if err := copyReviewDirectory(trackedReviewDirectory(t), directory); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, directory)
			if _, err := validateReviewDirectory(directory); err == nil {
				t.Fatal("validateReviewDirectory accepted a mutated closed set")
			}
		})
	}
}

func TestReviewReportRejectsUnknownFieldsAndPrematureApproval(t *testing.T) {
	value, err := os.ReadFile(filepath.Join(trackedReviewDirectory(t), "review.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("unknown field", func(t *testing.T) {
		mutated := bytes.Replace(value, []byte("{\n"), []byte("{\n  \"unreviewed\": true,\n"), 1)
		if _, err := decodeReviewReport(bytes.NewReader(mutated)); err == nil {
			t.Fatal("decodeReviewReport accepted an unknown field")
		}
	})
	t.Run("author approval", func(t *testing.T) {
		var document map[string]any
		if err := json.Unmarshal(value, &document); err != nil {
			t.Fatal(err)
		}
		document["author_approved"] = true
		mutated, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeReviewReport(bytes.NewReader(mutated)); err == nil {
			t.Fatal("decodeReviewReport accepted premature author approval")
		}
	})
}

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

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate publication-review test source")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func trackedReviewDirectory(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), "evaluation", "final-v5-wsl2", "publication-review", "exposure-scale-v1")
}

func copyReviewDirectory(source, destination string) error {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		value, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), value, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func compareReviewDirectories(left, right string) error {
	leftEntries, err := os.ReadDir(left)
	if err != nil {
		return err
	}
	rightEntries, err := os.ReadDir(right)
	if err != nil {
		return err
	}
	if len(leftEntries) != len(rightEntries) {
		return os.ErrInvalid
	}
	for index := range leftEntries {
		if leftEntries[index].Name() != rightEntries[index].Name() {
			return os.ErrInvalid
		}
		leftValue, err := os.ReadFile(filepath.Join(left, leftEntries[index].Name()))
		if err != nil {
			return err
		}
		rightValue, err := os.ReadFile(filepath.Join(right, rightEntries[index].Name()))
		if err != nil {
			return err
		}
		if !bytes.Equal(leftValue, rightValue) {
			return os.ErrInvalid
		}
	}
	return nil
}
