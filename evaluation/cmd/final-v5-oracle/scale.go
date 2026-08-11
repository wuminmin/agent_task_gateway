package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5dataset"
)

type scaleManifestBatchOutput struct {
	DatasetAgreement finalv5oracle.ExposureScaleDatasetAgreement   `json:"dataset_agreement"`
	ManifestCount    int                                           `json:"manifest_count"`
	Manifests        []finalv5oracle.ExposureScaleManifestArtifact `json:"manifests"`
}

func runScaleDatasetAgreement(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("scale-dataset-agreement", stderr)
	if err := parseFlags(flags, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	agreement, err := liveExposureScaleDatasetAgreement(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeJSON(stdout, agreement); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runScaleManifests(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("scale-manifests", stderr)
	outputRoot := flags.String("output-root", filepath.FromSlash("evaluation/final-v5-wsl2/oracle-manifests"),
		"existing oracle-manifests output root")
	specs := scaleManifestSpecFlags(flags)
	if err := parseFlags(flags, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := specs.validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	agreement, err := liveExposureScaleDatasetAgreement(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	artifacts, err := finalv5oracle.GenerateExposureScaleDependencyManifests(specs.value(), finalv5oracle.StreamSetOptions{
		MaxInMemoryMembers: dependencyMemoryMembers, CaptureMembers: dependencyCapturedMembers,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := installScaleManifests(*outputRoot, artifacts); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeJSON(stdout, scaleManifestBatchOutput{
		DatasetAgreement: agreement, ManifestCount: len(artifacts), Manifests: artifacts,
	}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runVerifyScaleManifests(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("verify-scale-manifests", stderr)
	inputRoot := flags.String("input-root", "", "oracle-manifests input root")
	if err := parseFlags(flags, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *inputRoot == "" {
		fmt.Fprintln(stderr, "input-root is required")
		return 2
	}
	agreement, err := liveExposureScaleDatasetAgreement(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	values, err := readScaleManifestSet(*inputRoot)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	artifacts, err := finalv5oracle.VerifyExposureScaleDependencyManifestSet(values, finalv5oracle.StreamSetOptions{
		MaxInMemoryMembers: dependencyMemoryMembers, CaptureMembers: dependencyCapturedMembers,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeJSON(stdout, scaleManifestBatchOutput{
		DatasetAgreement: agreement, ManifestCount: len(artifacts), Manifests: artifacts,
	}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

type scaleManifestSpecFlagValues struct {
	dataset, catalog, query, normalization *string
}

func scaleManifestSpecFlags(flags interface {
	String(string, string, string) *string
}) scaleManifestSpecFlagValues {
	return scaleManifestSpecFlagValues{
		dataset:       flags.String("dataset-spec-sha256", "", "reviewed Dataset Spec SHA-256"),
		catalog:       flags.String("catalog-spec-sha256", "", "reviewed Catalog Spec SHA-256"),
		query:         flags.String("query-spec-sha256", "", "reviewed four-query Scale Spec SHA-256"),
		normalization: flags.String("normalization-spec-sha256", "", "reviewed normalization Spec SHA-256"),
	}
}

func (values scaleManifestSpecFlagValues) value() finalv5oracle.ExposureScaleManifestSpecHashes {
	return finalv5oracle.ExposureScaleManifestSpecHashes{
		Dataset: *values.dataset, Catalog: *values.catalog, Query: *values.query, Normalization: *values.normalization,
	}
}

func (values scaleManifestSpecFlagValues) validate() error {
	for name, value := range map[string]string{
		"dataset-spec-sha256": *values.dataset, "catalog-spec-sha256": *values.catalog,
		"query-spec-sha256": *values.query, "normalization-spec-sha256": *values.normalization,
	} {
		if !isCanonicalSHA256(value) {
			return fmt.Errorf("%s must be an explicit canonical SHA-256", name)
		}
	}
	return values.value().Validate()
}

func liveExposureScaleDatasetAgreement(ctx context.Context) (finalv5oracle.ExposureScaleDatasetAgreement, error) {
	dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN"))
	}
	if dsn == "" {
		return finalv5oracle.ExposureScaleDatasetAgreement{},
			errors.New("BUSINESS_TEST_POSTGRES_DSN or TASKGATE_FINAL_V5_BUSINESS_DSN is required")
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return finalv5oracle.ExposureScaleDatasetAgreement{}, errors.New("connect fixed exposure-scale Dataset database failed")
	}
	defer connection.Close(ctx)
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return finalv5oracle.ExposureScaleDatasetAgreement{}, errors.New("begin read-only exposure-scale Dataset transaction failed")
	}
	defer tx.Rollback(ctx)
	columns, err := finalv5dataset.DatasetStreamColumns(finalv5oracle.ExposureScaleProductID)
	if err != nil {
		return finalv5oracle.ExposureScaleDatasetAgreement{}, err
	}
	stream, err := finalv5dataset.ProductStream(ctx, tx, finalv5oracle.ExposureScaleProductID)
	if err != nil {
		return finalv5oracle.ExposureScaleDatasetAgreement{}, err
	}
	agreement, err := finalv5oracle.AgreeExposureScaleDatasetStream(columns, stream)
	if err != nil {
		return agreement, err
	}
	preparedStatements, err := finalv5dataset.PreparedStatementCount(ctx, tx)
	if err != nil {
		return agreement, errors.New("verify exposure-scale session prepared-statement state failed")
	}
	if preparedStatements != 0 {
		return agreement, fmt.Errorf("exposure-scale Dataset session contains %d prepared statements; expected zero", preparedStatements)
	}
	if err := tx.Commit(ctx); err != nil {
		return agreement, errors.New("commit read-only exposure-scale Dataset transaction failed")
	}
	return agreement, nil
}

func installScaleManifests(outputRoot string, artifacts []finalv5oracle.ExposureScaleManifestArtifact) error {
	if err := validateScaleManifestArtifacts(artifacts); err != nil {
		return err
	}
	info, err := os.Stat(outputRoot)
	if err != nil || !info.IsDir() {
		return errors.New("scale manifest output-root must be an existing directory")
	}
	target := filepath.Join(outputRoot, "scale", "dependency-e2e")
	if _, err := os.Stat(target); err == nil {
		values, readErr := readScaleManifestSet(outputRoot)
		if readErr != nil {
			return readErr
		}
		if _, verifyErr := finalv5oracle.VerifyExposureScaleDependencyManifestSet(values, finalv5oracle.StreamSetOptions{
			MaxInMemoryMembers: dependencyMemoryMembers, CaptureMembers: dependencyCapturedMembers,
		}); verifyErr != nil {
			return fmt.Errorf("existing scale manifest tree differs; refusing overwrite: %w", verifyErr)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect scale manifest target: %w", err)
	}
	stage, err := os.MkdirTemp(outputRoot, ".final-v5-oracle-scale-")
	if err != nil {
		return fmt.Errorf("create scale manifest staging directory: %w", err)
	}
	defer os.RemoveAll(stage)
	for _, artifact := range artifacts {
		canonical, err := finalv5oracle.CanonicalManifest(artifact.Manifest)
		if err != nil {
			return err
		}
		stagedPath := filepath.Join(stage, filepath.FromSlash(artifact.RelativePath))
		if err := os.MkdirAll(filepath.Dir(stagedPath), 0o755); err != nil {
			return fmt.Errorf("create scale manifest directory: %w", err)
		}
		file, err := os.OpenFile(stagedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("create scale manifest: %w", err)
		}
		if _, err := file.Write(canonical); err != nil {
			_ = file.Close()
			return fmt.Errorf("write scale manifest: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close scale manifest: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create Scale manifest workload parent: %w", err)
	}
	if err := os.Rename(filepath.Join(stage, "scale", "dependency-e2e"), target); err != nil {
		return fmt.Errorf("install closed scale manifest tree: %w", err)
	}
	return nil
}

func validateScaleManifestArtifacts(artifacts []finalv5oracle.ExposureScaleManifestArtifact) error {
	if len(artifacts) != 24 {
		return fmt.Errorf("Scale manifest generator supplied %d artifacts; expected exactly 24", len(artifacts))
	}
	want := make(map[string]bool, 24)
	for _, cell := range finalv5oracle.ExposureScaleDependencyCells() {
		for _, mode := range []string{finalv5oracle.ExposureScaleModeNovel, finalv5oracle.ExposureScaleModeSemanticReplay} {
			relative, err := finalv5oracle.ExposureScaleDependencyManifestPath(cell.Scale, mode)
			if err != nil {
				return err
			}
			want[relative] = true
		}
	}
	seen := make(map[string]bool, 24)
	for _, artifact := range artifacts {
		if !want[artifact.RelativePath] || seen[artifact.RelativePath] {
			return fmt.Errorf("Scale manifest generator supplied unexpected or duplicate path %q", artifact.RelativePath)
		}
		relative, err := finalv5oracle.ExposureScaleDependencyManifestPath(artifact.Manifest.Scale, artifact.Manifest.Mode)
		if err != nil || relative != artifact.RelativePath {
			return fmt.Errorf("Scale manifest path %q does not match its semantic identity", artifact.RelativePath)
		}
		digest, err := finalv5oracle.ManifestSHA256(artifact.Manifest)
		if err != nil {
			return fmt.Errorf("validate Scale manifest %q: %w", artifact.RelativePath, err)
		}
		if digest != artifact.SHA256 {
			return fmt.Errorf("Scale manifest %q SHA-256 is %q; expected %q", artifact.RelativePath, artifact.SHA256, digest)
		}
		seen[artifact.RelativePath] = true
	}
	if len(seen) != len(want) {
		return errors.New("Scale manifest generator omitted a fixed dependency cell")
	}
	return nil
}

func readScaleManifestSet(inputRoot string) (map[string][]byte, error) {
	root := filepath.Join(inputRoot, "scale", "dependency-e2e")
	values := make(map[string][]byte, 24)
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() || filepath.Ext(current) != ".json" {
			return fmt.Errorf("scale manifest tree contains non-canonical entry %q", current)
		}
		relative, err := filepath.Rel(inputRoot, current)
		if err != nil {
			return err
		}
		value, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if _, duplicate := values[key]; duplicate {
			return fmt.Errorf("scale manifest path %q is duplicated", key)
		}
		values[key] = value
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read scale manifest tree: %w", err)
	}
	return values, nil
}
