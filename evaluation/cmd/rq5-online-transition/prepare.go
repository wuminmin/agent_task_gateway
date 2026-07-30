package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

const preparationSchema = "taskgate-daily-publication-online-preparation-v1"

type preparationManifest struct {
	SchemaVersion string                `json:"schema_version"`
	Publications  []preparedPublication `json:"publications"`
}

type preparedPublication struct {
	Day              string `json:"day"`
	PublicationName  string `json:"publication_name"`
	Rows             uint64 `json:"rows"`
	SchemaDigest     string `json:"schema_digest"`
	ManifestDigest   string `json:"manifest_digest"`
	DictionaryDigest string `json:"dictionary_digest"`
	SidecarDigest    string `json:"sidecar_digest"`
	InputSHA256      string `json:"input_sha256"`
}

func preparePublications(options prepareOptions) error {
	if err := requireDirectory(options.InputDirectory, "-input-dir"); err != nil {
		return err
	}
	for _, target := range []struct {
		path string
		name string
	}{
		{options.ApprovedDirectory, "-approved-dir"},
		{options.ArtifactDirectory, "-artifact-dir"},
		{options.CalibrationDirectory, "-calibration-dir"},
	} {
		if err := createPrivateDirectory(target.path, target.name); err != nil {
			return err
		}
	}
	if options.ManifestPath == "" {
		return errors.New("-manifest is required")
	}
	if _, err := os.Lstat(options.ManifestPath); err == nil {
		return fmt.Errorf("refusing to overwrite %s", options.ManifestPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dsn := os.Getenv("SNAPSHOT_POSTGRES_DSN")
	if dsn == "" {
		return errors.New("SNAPSHOT_POSTGRES_DSN is required")
	}

	ctx := context.Background()
	manifest := preparationManifest{SchemaVersion: preparationSchema,
		Publications: make([]preparedPublication, 0, len(days))}
	for _, day := range days {
		candidatePath := filepath.Join(options.InputDirectory, day+".json")
		candidate, err := readCompilerInput(candidatePath)
		if err != nil {
			return fmt.Errorf("%s candidate: %w", day, err)
		}
		if err := validateCandidateIdentity(day, candidate); err != nil {
			return err
		}
		if !reflect.DeepEqual(candidate.ExpectedDigests, snapshotbundle.ExpectedDigests{}) ||
			len(candidate.Snapshot.Rows) != 0 {
			return fmt.Errorf("%s candidate must have no expected digests or caller-supplied rows", day)
		}

		connector, attestation, err := openPublicationConnector(ctx, dsn, candidate, "", businessDatabase)
		if err != nil {
			return fmt.Errorf("%s live connector attestation: %w", day, err)
		}
		connector.Close()
		candidate.Snapshot.SchemaDigest = attestation.SchemaDigest

		calibrationInput, err := snapshotbundle.ScanPostgresSnapshot(ctx, candidate, dsn)
		if err != nil {
			return fmt.Errorf("%s calibration scan: %w", day, err)
		}
		calibration, err := snapshotbundle.CompileOwnedToDirectory(&calibrationInput,
			options.CalibrationDirectory, snapshotbundle.DefaultPublicationLimits())
		if err != nil {
			return fmt.Errorf("%s calibration compile: %w", day, err)
		}
		candidate.ExpectedDigests = expectedDigests(calibration.Manifest)

		approvedForCompile, err := snapshotbundle.ScanPostgresSnapshot(ctx, candidate, dsn)
		if err != nil {
			return fmt.Errorf("%s approved scan: %w", day, err)
		}
		written, err := snapshotbundle.CompileOwnedToDirectory(&approvedForCompile,
			options.ArtifactDirectory, snapshotbundle.DefaultPublicationLimits())
		if err != nil {
			return fmt.Errorf("%s approved compile: %w", day, err)
		}
		if !reflect.DeepEqual(calibration.Manifest, written.Manifest) {
			return fmt.Errorf("%s calibration and approved rebuild differ", day)
		}

		approvedPath := filepath.Join(options.ApprovedDirectory, day+".json")
		if err := writeJSONAtomicExclusive(approvedPath, candidate); err != nil {
			return fmt.Errorf("write %s approved input: %w", day, err)
		}
		inputDigest, err := fileSHA256(approvedPath)
		if err != nil {
			return err
		}
		manifest.Publications = append(manifest.Publications, preparedPublication{
			Day: day, PublicationName: candidate.PublicationName, Rows: written.Manifest.RowCount,
			SchemaDigest: candidate.Snapshot.SchemaDigest, ManifestDigest: written.Manifest.ManifestDigest,
			DictionaryDigest: written.Manifest.DictionaryManifest.DictionaryDigest,
			SidecarDigest:    written.Manifest.DictionaryManifest.SidecarDigest, InputSHA256: inputDigest,
		})
	}
	if err := writeJSONAtomicExclusive(options.ManifestPath, manifest); err != nil {
		return fmt.Errorf("write preparation manifest: %w", err)
	}
	return nil
}

func readCompilerInput(path string) (snapshotbundle.CompilerInput, error) {
	file, err := os.Open(path)
	if err != nil {
		return snapshotbundle.CompilerInput{}, err
	}
	defer file.Close()
	return snapshotbundle.DecodeCompilerInput(file)
}

func validateCandidateIdentity(day string, input snapshotbundle.CompilerInput) error {
	expectedRelation := "reporting.daily_lineitem_" + day
	if input.Version != snapshotbundle.CompilerInputVersion || input.SourceRelation != expectedRelation ||
		input.CatalogSource != "daily_reporting" || input.Snapshot.SourceID != "taskgate-eval-daily-publication" ||
		input.Snapshot.SourceNamespace != "evaluation.daily_lineitem" {
		return fmt.Errorf("%s candidate identity does not match the deterministic daily fixture", day)
	}
	return nil
}

func expectedDigests(manifest snapshotbundle.BundleManifest) snapshotbundle.ExpectedDigests {
	return snapshotbundle.ExpectedDigests{
		SidecarDigest:     manifest.DictionaryManifest.SidecarDigest,
		DictionaryDigest:  manifest.DictionaryManifest.DictionaryDigest,
		ManifestDigest:    manifest.ManifestDigest,
		ColdPayloadDigest: manifest.DictionaryManifest.ColdPayloadDigest,
		HotIndexDigest:    manifest.DictionaryManifest.HotIndexDigest,
	}
}
