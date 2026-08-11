package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5dataset"
)

const provSQLDatasetAgreementVersion = "taskgate-final-v5-provsql-dataset-agreement-v1"

type provSQLDatasetStreamShape struct {
	ProductID string                              `json:"product_id"`
	Columns   []finalv5oracle.DatasetStreamColumn `json:"columns"`
}

// provSQLDatasetAgreement is credential-free evidence from one fixed,
// read-only PostgreSQL transaction. Reference and Observed contain only typed
// Product commitments, never rows or connection material.
type provSQLDatasetAgreement struct {
	Version                string                                  `json:"version"`
	Columns                []provSQLDatasetStreamShape             `json:"columns"`
	Reference              finalv5oracle.DatasetFingerprintSummary `json:"reference"`
	Observed               finalv5oracle.DatasetFingerprintSummary `json:"observed"`
	PreparedStatementCount int64                                   `json:"prepared_statement_count"`
	Agreed                 bool                                    `json:"agreed"`
}

type provSQLManifestBatchOutput struct {
	DatasetAgreement provSQLDatasetAgreement                 `json:"dataset_agreement"`
	ManifestCount    int                                     `json:"manifest_count"`
	Manifests        []finalv5oracle.ProvSQLManifestArtifact `json:"manifests"`
}

func runProvSQLDatasetAgreement(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("provsql-dataset-agreement", stderr)
	if err := parseFlags(flags, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	agreement, err := liveProvSQLDatasetAgreement(context.Background())
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

func runProvSQLManifests(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("provsql-manifests", stderr)
	outputRoot := flags.String("output-root", filepath.FromSlash("evaluation/final-v5-wsl2/oracle-manifests"),
		"existing oracle-manifests output root")
	specs := provSQLManifestSpecFlags(flags)
	if err := parseFlags(flags, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := specs.validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	agreement, err := liveProvSQLDatasetAgreement(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	artifacts, err := finalv5oracle.GenerateProvSQLNonceJoinManifests(specs.value(), finalv5oracle.StreamSetOptions{
		MaxInMemoryMembers: dependencyMemoryMembers, CaptureMembers: dependencyCapturedMembers,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := installProvSQLManifests(*outputRoot, artifacts); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeJSON(stdout, provSQLManifestBatchOutput{
		DatasetAgreement: agreement, ManifestCount: len(artifacts), Manifests: artifacts,
	}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runVerifyProvSQLManifests(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("verify-provsql-manifests", stderr)
	inputRoot := flags.String("input-root", "", "oracle-manifests input root")
	if err := parseFlags(flags, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *inputRoot == "" {
		fmt.Fprintln(stderr, "input-root is required")
		return 2
	}
	agreement, err := liveProvSQLDatasetAgreement(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	values, err := readProvSQLManifestSet(*inputRoot)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	artifacts, err := finalv5oracle.VerifyProvSQLNonceJoinManifestSet(values, finalv5oracle.StreamSetOptions{
		MaxInMemoryMembers: dependencyMemoryMembers, CaptureMembers: dependencyCapturedMembers,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeJSON(stdout, provSQLManifestBatchOutput{
		DatasetAgreement: agreement, ManifestCount: len(artifacts), Manifests: artifacts,
	}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

type provSQLManifestSpecFlagValues struct {
	dataset, catalog, query, normalization *string
}

func provSQLManifestSpecFlags(flags interface {
	String(string, string, string) *string
}) provSQLManifestSpecFlagValues {
	return provSQLManifestSpecFlagValues{
		dataset:       flags.String("dataset-spec-sha256", "", "reviewed Dataset Spec SHA-256"),
		catalog:       flags.String("catalog-spec-sha256", "", "reviewed Catalog Spec SHA-256"),
		query:         flags.String("query-spec-sha256", "", "reviewed ProvSQL query Spec SHA-256"),
		normalization: flags.String("normalization-spec-sha256", "", "reviewed normalization Spec SHA-256"),
	}
}

func (values provSQLManifestSpecFlagValues) value() finalv5oracle.ProvSQLManifestSpecHashes {
	return finalv5oracle.ProvSQLManifestSpecHashes{
		Dataset: *values.dataset, Catalog: *values.catalog, Query: *values.query, Normalization: *values.normalization,
	}
}

func (values provSQLManifestSpecFlagValues) validate() error {
	for name, value := range map[string]string{
		"dataset-spec-sha256": *values.dataset, "catalog-spec-sha256": *values.catalog,
		"query-spec-sha256": *values.query, "normalization-spec-sha256": *values.normalization,
	} {
		if !isCanonicalSHA256(value) {
			return fmt.Errorf("%s must be an explicit canonical SHA-256", name)
		}
	}
	if got, want := values.value(), finalv5oracle.FrozenProvSQLManifestSpecHashes(); got != want {
		return fmt.Errorf("ProvSQL manifest Spec hashes are %+v; expected the reviewed fixed binding %+v", got, want)
	}
	return nil
}

func liveProvSQLDatasetAgreement(ctx context.Context) (provSQLDatasetAgreement, error) {
	shapes, err := provSQLDatasetStreamShapes()
	if err != nil {
		return provSQLDatasetAgreement{}, err
	}
	agreement := provSQLDatasetAgreement{Version: provSQLDatasetAgreementVersion, Columns: shapes}
	if err := validateProvSQLDatasetStreamShapes(agreement.Columns); err != nil {
		return agreement, err
	}
	reference, err := finalv5oracle.ProvSQLDatasetFingerprint()
	if err != nil {
		return agreement, fmt.Errorf("regenerate frozen ProvSQL typed Products: %w", err)
	}
	agreement.Reference = reference
	dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN"))
	}
	if dsn == "" {
		return agreement, errors.New("BUSINESS_TEST_POSTGRES_DSN or TASKGATE_FINAL_V5_BUSINESS_DSN is required")
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return agreement, errors.New("connect fixed ProvSQL Dataset database failed")
	}
	defer connection.Close(ctx)
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return agreement, errors.New("begin read-only ProvSQL Dataset transaction failed")
	}
	defer tx.Rollback(ctx)

	streams := make(map[string]finalv5oracle.DatasetRowStream, len(shapes))
	for _, shape := range shapes {
		stream, streamErr := finalv5dataset.ProductStream(ctx, tx, shape.ProductID)
		if streamErr != nil {
			return agreement, streamErr
		}
		streams[shape.ProductID] = stream
	}
	observed, err := finalv5oracle.ProvSQLDatasetFingerprintFromStreams(streams)
	if err != nil {
		return agreement, fmt.Errorf("fingerprint fixed live ProvSQL typed Products: %w", err)
	}
	agreement.Observed = observed
	agreement.Agreed = reflect.DeepEqual(agreement.Reference, agreement.Observed)
	if !agreement.Agreed {
		return agreement, errors.New("live ProvSQL typed Products disagree with the frozen formulas")
	}
	agreement.PreparedStatementCount, err = finalv5dataset.PreparedStatementCount(ctx, tx)
	if err != nil {
		return agreement, errors.New("verify ProvSQL session prepared-statement state failed")
	}
	if agreement.PreparedStatementCount != 0 {
		return agreement, fmt.Errorf("ProvSQL Dataset session contains %d prepared statements; expected zero",
			agreement.PreparedStatementCount)
	}
	if err := tx.Commit(ctx); err != nil {
		return agreement, errors.New("commit read-only ProvSQL Dataset transaction failed")
	}
	return agreement, nil
}

func provSQLDatasetStreamShapes() ([]provSQLDatasetStreamShape, error) {
	result := make([]provSQLDatasetStreamShape, 0, 3)
	for _, productID := range []string{
		finalv5oracle.ProvSQLOrdersProductID,
		finalv5oracle.ProvSQLLineitemProductID,
		finalv5oracle.ProvSQLNonceProductID,
	} {
		columns, err := finalv5dataset.DatasetStreamColumns(productID)
		if err != nil {
			return nil, err
		}
		result = append(result, provSQLDatasetStreamShape{ProductID: productID, Columns: columns})
	}
	return result, nil
}

func validateProvSQLDatasetStreamShapes(shapes []provSQLDatasetStreamShape) error {
	products := finalv5oracle.ProvSQLDatasetProductSpecs()
	if len(products) != 3 || len(shapes) != 3 || len(products) != len(shapes) {
		return fmt.Errorf("fixed ProvSQL Dataset has %d Product specifications and %d query shapes; expected three each",
			len(products), len(shapes))
	}
	for productIndex := range products {
		product, shape := products[productIndex], shapes[productIndex]
		if product.ProductID != shape.ProductID || len(product.Fields) != len(shape.Columns) {
			return fmt.Errorf("fixed ProvSQL Dataset Product %d does not match its query shape", productIndex+1)
		}
		for fieldIndex := range product.Fields {
			field, column := product.Fields[fieldIndex], shape.Columns[fieldIndex]
			resolved, err := finalv5oracle.SQLTypeFromPostgresOID(column.PostgreSQLOID)
			if err != nil || field.Name != column.Name || field.SQLType != column.SQLType || resolved != field.SQLType {
				return fmt.Errorf("fixed ProvSQL Dataset Product %s field %d does not match its query shape",
					product.ProductID, fieldIndex+1)
			}
		}
	}
	return nil
}

func installProvSQLManifests(outputRoot string, artifacts []finalv5oracle.ProvSQLManifestArtifact) error {
	canonical, err := validateProvSQLManifestArtifacts(artifacts)
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(outputRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("ProvSQL manifest output-root must be an existing real directory")
	}
	provSQLRoot := filepath.Join(outputRoot, "provsql")
	if info, err := os.Lstat(provSQLRoot); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("ProvSQL manifest output parent must be a real directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect ProvSQL manifest output parent: %w", err)
	}
	target := filepath.Join(provSQLRoot, "nonce-join-group")
	if _, err := os.Lstat(target); err == nil {
		existing, readErr := readProvSQLManifestSet(outputRoot)
		if readErr != nil {
			return readErr
		}
		if len(existing) != len(canonical) {
			return errors.New("existing ProvSQL manifest tree differs; refusing overwrite")
		}
		for relative, want := range canonical {
			if !bytes.Equal(existing[relative], want) {
				return fmt.Errorf("existing ProvSQL manifest tree differs at %q; refusing overwrite", relative)
			}
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect ProvSQL manifest target: %w", err)
	}
	stage, err := os.MkdirTemp(outputRoot, ".final-v5-oracle-provsql-")
	if err != nil {
		return fmt.Errorf("create ProvSQL manifest staging directory: %w", err)
	}
	defer os.RemoveAll(stage)
	for relative, value := range canonical {
		stagedPath := filepath.Join(stage, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(stagedPath), 0o755); err != nil {
			return fmt.Errorf("create ProvSQL manifest directory: %w", err)
		}
		file, err := os.OpenFile(stagedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("create ProvSQL manifest: %w", err)
		}
		if _, err := file.Write(value); err != nil {
			_ = file.Close()
			return fmt.Errorf("write ProvSQL manifest: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close ProvSQL manifest: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create ProvSQL manifest workload parent: %w", err)
	}
	if err := os.Rename(filepath.Join(stage, "provsql", "nonce-join-group"), target); err != nil {
		return fmt.Errorf("install closed ProvSQL manifest tree: %w", err)
	}
	return nil
}

func validateProvSQLManifestArtifacts(artifacts []finalv5oracle.ProvSQLManifestArtifact) (map[string][]byte, error) {
	want, _, err := provSQLExpectedManifestTree()
	if err != nil {
		return nil, err
	}
	if len(artifacts) != len(want) {
		return nil, fmt.Errorf("ProvSQL manifest generator supplied %d artifacts; expected exactly %d", len(artifacts), len(want))
	}
	canonical := make(map[string][]byte, len(want))
	for _, artifact := range artifacts {
		if !want[artifact.RelativePath] {
			return nil, fmt.Errorf("ProvSQL manifest generator supplied unexpected path %q", artifact.RelativePath)
		}
		if _, duplicate := canonical[artifact.RelativePath]; duplicate {
			return nil, fmt.Errorf("ProvSQL manifest generator supplied duplicate path %q", artifact.RelativePath)
		}
		cell, err := finalv5oracle.ParseProvSQLBindingKey(artifact.Manifest.BindingKey)
		if err != nil {
			return nil, fmt.Errorf("ProvSQL manifest path %q has invalid binding identity: %w", artifact.RelativePath, err)
		}
		relative, err := finalv5oracle.ProvSQLNonceJoinManifestPath(artifact.Manifest.Scale, cell.Nonce)
		if err != nil || relative != artifact.RelativePath {
			return nil, fmt.Errorf("ProvSQL manifest path %q does not match its semantic identity", artifact.RelativePath)
		}
		value, err := finalv5oracle.CanonicalManifest(artifact.Manifest)
		if err != nil {
			return nil, fmt.Errorf("validate ProvSQL manifest %q: %w", artifact.RelativePath, err)
		}
		digest, err := finalv5oracle.ManifestSHA256(artifact.Manifest)
		if err != nil {
			return nil, fmt.Errorf("validate ProvSQL manifest %q: %w", artifact.RelativePath, err)
		}
		if digest != artifact.SHA256 {
			return nil, fmt.Errorf("ProvSQL manifest %q SHA-256 is %q; expected %q", artifact.RelativePath, artifact.SHA256, digest)
		}
		canonical[artifact.RelativePath] = value
	}
	return canonical, nil
}

func readProvSQLManifestSet(inputRoot string) (map[string][]byte, error) {
	wantFiles, wantDirectories, err := provSQLExpectedManifestTree()
	if err != nil {
		return nil, err
	}
	for _, directory := range []string{inputRoot, filepath.Join(inputRoot, "provsql")} {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("ProvSQL manifest input root and parent must be existing real directories")
		}
	}
	root := filepath.Join(inputRoot, "provsql", "nonce-join-group")
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("ProvSQL manifest tree must be an existing real directory")
	}
	values := make(map[string][]byte, len(wantFiles))
	err = filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(inputRoot, current)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if current == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("ProvSQL manifest tree contains symlink %q", current)
		}
		if entry.IsDir() {
			if !wantDirectories[key] {
				return fmt.Errorf("ProvSQL manifest tree contains unexpected directory %q", current)
			}
			return nil
		}
		if !entry.Type().IsRegular() || filepath.Ext(current) != ".json" || !wantFiles[key] {
			return fmt.Errorf("ProvSQL manifest tree contains non-canonical entry %q", current)
		}
		value, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		values[key] = value
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read ProvSQL manifest tree: %w", err)
	}
	if len(values) != len(wantFiles) {
		return nil, fmt.Errorf("ProvSQL manifest tree contains %d files; expected exactly %d", len(values), len(wantFiles))
	}
	return values, nil
}

func provSQLExpectedManifestTree() (map[string]bool, map[string]bool, error) {
	files := make(map[string]bool, 105)
	directories := make(map[string]bool)
	for _, cell := range finalv5oracle.ProvSQLNonceJoinCells() {
		relative, err := finalv5oracle.ProvSQLNonceJoinManifestPath(cell.Scale, cell.Nonce)
		if err != nil {
			return nil, nil, err
		}
		if files[relative] {
			return nil, nil, fmt.Errorf("frozen ProvSQL cells duplicate manifest path %q", relative)
		}
		files[relative] = true
		for parent := filepath.ToSlash(filepath.Dir(relative)); parent != "." && parent != "provsql/nonce-join-group"; parent = filepath.ToSlash(filepath.Dir(parent)) {
			directories[parent] = true
		}
	}
	if len(files) != 105 {
		return nil, nil, fmt.Errorf("frozen ProvSQL grid contains %d manifest paths; expected 105", len(files))
	}
	return files, directories, nil
}
