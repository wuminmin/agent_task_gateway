// Command final-v5-oracle exposes the independent Final-V5 oracle. Its Dataset
// adapters perform fixed, read-only PostgreSQL stream checks; no subcommand
// accepts credential-bearing flags, emits credentials, or writes campaign
// evidence.
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

const (
	maximumManifestBytes      = 1 << 20
	dependencyMemoryMembers   = 64 * 1024
	dependencyCapturedMembers = 8
	oracleCommandName         = "final-v5-oracle"
)

type artifactSpecHashes struct {
	dataset       string
	catalog       string
	query         string
	normalization string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: final-v5-oracle <artifact-manifest|scale-dataset-agreement|scale-manifests|verify-scale-manifests|provsql-dataset-agreement|provsql-manifests|verify-provsql-manifests|verify-manifest|dataset-fingerprint|dependency-report|outcome-schedule>")
		return 2
	}
	switch args[0] {
	case "artifact-manifest":
		return runArtifactManifest(args[1:], stdout, stderr)
	case "scale-dataset-agreement":
		return runScaleDatasetAgreement(args[1:], stdout, stderr)
	case "scale-manifests":
		return runScaleManifests(args[1:], stdout, stderr)
	case "verify-scale-manifests":
		return runVerifyScaleManifests(args[1:], stdout, stderr)
	case "provsql-dataset-agreement":
		return runProvSQLDatasetAgreement(args[1:], stdout, stderr)
	case "provsql-manifests":
		return runProvSQLManifests(args[1:], stdout, stderr)
	case "verify-provsql-manifests":
		return runVerifyProvSQLManifests(args[1:], stdout, stderr)
	case "verify-manifest":
		return runVerifyManifest(args[1:], stdin, stdout, stderr)
	case "dataset-fingerprint":
		return runDatasetFingerprint(args[1:], stdout, stderr)
	case "dependency-report":
		return runDependencyReport(args[1:], stdout, stderr)
	case "outcome-schedule":
		return runOutcomeSchedule(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		return 2
	}
}

func runDatasetFingerprint(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("dataset-fingerprint", stderr)
	if err := parseFlags(flags, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	summary, err := finalv5oracle.BenchmarkDatasetFingerprint()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeJSON(stdout, summary); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runArtifactManifest(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("artifact-manifest", stderr)
	rowCountText := flags.String("row-count", "", "formal artifact row count")
	columnCountText := flags.String("column-count", "", "formal artifact column count")
	datasetSHA256 := flags.String("dataset-spec-sha256", "", "reviewed Dataset Spec SHA-256")
	catalogSHA256 := flags.String("catalog-spec-sha256", "", "reviewed Catalog Spec SHA-256")
	querySHA256 := flags.String("query-spec-sha256", "", "reviewed Query Spec SHA-256")
	normalizationSHA256 := flags.String("normalization-spec-sha256", "", "reviewed normalization Spec SHA-256")
	if err := parseFlags(flags, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	rowCount, err := parseCanonicalInt64("row-count", *rowCountText)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	columnCount64, err := parseCanonicalInt64("column-count", *columnCountText)
	if err != nil || int64(int(columnCount64)) != columnCount64 {
		if err == nil {
			err = errors.New("column-count is outside the platform integer range")
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	specs := artifactSpecHashes{
		dataset: *datasetSHA256, catalog: *catalogSHA256,
		query: *querySHA256, normalization: *normalizationSHA256,
	}
	if err := specs.validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	columnCount := int(columnCount64)
	scale, err := artifactScale(rowCount, columnCount)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	summary, err := finalv5oracle.ArtifactResultSummary(rowCount, columnCount)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	manifest := artifactManifest(rowCount, columnCount, scale, specs, summary)
	canonical, err := finalv5oracle.CanonicalManifest(manifest)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeBytes(stdout, canonical); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runVerifyManifest(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := newFlagSet("verify-manifest", stderr)
	inputPath := flags.String("input", "", "canonical manifest path, or - for stdin")
	if err := parseFlags(flags, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *inputPath == "" {
		fmt.Fprintln(stderr, "input is required")
		return 2
	}
	value, err := readManifest(*inputPath, stdin)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	manifest, err := finalv5oracle.DecodeManifest(value)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := verifyGeneratedManifest(manifest); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	digest, err := finalv5oracle.ManifestSHA256(manifest)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := fmt.Fprintln(stdout, digest); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runDependencyReport(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("dependency-report", stderr)
	candidateText := flags.String("candidate-facts", "", "candidate dependency Fact cardinality")
	existingText := flags.String("existing-facts", "", "existing dependency Fact cardinality")
	overlapText := flags.String("overlap-facts", "", "overlap dependency Fact cardinality")
	if err := parseFlags(flags, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	candidate, err := parseCanonicalInt64("candidate-facts", *candidateText)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	existing, err := parseCanonicalInt64("existing-facts", *existingText)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	overlap, err := parseCanonicalInt64("overlap-facts", *overlapText)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	report, err := finalv5oracle.GenerateExposureScaleDependency(finalv5oracle.ExposureScaleDependencyRequest{
		CandidateFacts: candidate,
		ExistingFacts:  existing,
		OverlapFacts:   overlap,
		SetOptions: finalv5oracle.StreamSetOptions{
			MaxInMemoryMembers: dependencyMemoryMembers,
			CaptureMembers:     dependencyCapturedMembers,
		},
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeJSON(stdout, report); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runOutcomeSchedule(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("outcome-schedule", stderr)
	seedText := flags.String("seed", "", "deterministic schedule seed")
	candidateText := flags.String("candidate-cardinality", "", "candidate members per measured sample")
	targetText := flags.String("target-percent", "", "exact cell-level overlap percent")
	if err := parseFlags(flags, args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	seed, err := parseCanonicalInt64("seed", *seedText)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	candidate, err := parseCanonicalInt64("candidate-cardinality", *candidateText)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	target64, err := parseCanonicalInt64("target-percent", *targetText)
	if err != nil || int64(int(target64)) != target64 {
		if err == nil {
			err = errors.New("target-percent is outside the platform integer range")
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	schedule, err := finalv5oracle.BuildOutcomeOverlapSchedule(seed, candidate, int(target64))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeJSON(stdout, schedule); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func artifactManifest(rowCount int64, columnCount int, scale string, specs artifactSpecHashes, summary finalv5oracle.ResultSummary) finalv5oracle.OracleManifest {
	return finalv5oracle.OracleManifest{
		SchemaVersion:           finalv5oracle.ManifestSchemaVersion,
		OracleVersion:           finalv5oracle.ManifestOracleVersion,
		ContractVersion:         finalv5oracle.ManifestContractVersion,
		ExperimentID:            "artifact",
		WorkloadID:              "result-heavy",
		Scale:                   scale,
		Mode:                    "novel",
		DatasetSpecSHA256:       specs.dataset,
		CatalogSpecSHA256:       specs.catalog,
		QuerySpecSHA256:         specs.query,
		NormalizationSpecSHA256: specs.normalization,
		Expected: finalv5oracle.ManifestExpected{
			RowCount:               finalv5oracle.Int64(summary.RowCount),
			ColumnCount:            finalv5oracle.Int(summary.ColumnCount),
			NormalizedSchemaSHA256: summary.NormalizedSchemaSHA256,
			CanonicalResultSHA256:  summary.CanonicalResultSHA256,
		},
		Generation: finalv5oracle.ManifestGeneration{
			Seed:             finalv5oracle.ArtifactGeneratorSeed,
			GeneratorVersion: finalv5oracle.ArtifactGeneratorVersion,
			Command:          artifactGenerationCommand(rowCount, columnCount, specs),
		},
	}
}

func verifyGeneratedManifest(manifest finalv5oracle.OracleManifest) error {
	switch {
	case manifest.ExperimentID == "scale" && manifest.WorkloadID == "dependency-e2e":
		return finalv5oracle.VerifyExposureScaleDependencyManifest(manifest, finalv5oracle.StreamSetOptions{
			MaxInMemoryMembers: dependencyMemoryMembers, CaptureMembers: dependencyCapturedMembers,
		})
	case manifest.ExperimentID == "provsql" && manifest.WorkloadID == "nonce-join-group":
		return finalv5oracle.VerifyProvSQLNonceJoinManifest(manifest, finalv5oracle.StreamSetOptions{
			MaxInMemoryMembers: dependencyMemoryMembers, CaptureMembers: dependencyCapturedMembers,
		})
	case manifest.ExperimentID != "artifact" || manifest.WorkloadID != "result-heavy":
		return errors.New("manifest workload has no implemented independent semantic verifier")
	}
	rowCount, columnCount, err := parseArtifactScale(manifest.Scale)
	if err != nil {
		return err
	}
	if manifest.Mode != "novel" || manifest.Generation.Seed != finalv5oracle.ArtifactGeneratorSeed ||
		manifest.Generation.GeneratorVersion != finalv5oracle.ArtifactGeneratorVersion {
		return errors.New("artifact manifest mode or frozen generator binding was mutated")
	}
	summary, err := finalv5oracle.ArtifactResultSummary(rowCount, columnCount)
	if err != nil {
		return err
	}
	wantExpected := finalv5oracle.ManifestExpected{
		RowCount: finalv5oracle.Int64(summary.RowCount), ColumnCount: finalv5oracle.Int(summary.ColumnCount),
		NormalizedSchemaSHA256: summary.NormalizedSchemaSHA256, CanonicalResultSHA256: summary.CanonicalResultSHA256,
	}
	if !reflect.DeepEqual(manifest.Expected, wantExpected) {
		return errors.New("artifact manifest logical-result members were mutated")
	}
	specs := artifactSpecHashes{
		dataset: manifest.DatasetSpecSHA256, catalog: manifest.CatalogSpecSHA256,
		query: manifest.QuerySpecSHA256, normalization: manifest.NormalizationSpecSHA256,
	}
	if manifest.Generation.Command != artifactGenerationCommand(rowCount, columnCount, specs) {
		return errors.New("artifact manifest generation command or Spec binding was mutated")
	}
	return nil
}

func artifactGenerationCommand(rowCount int64, columnCount int, specs artifactSpecHashes) string {
	return fmt.Sprintf(
		"%s artifact-manifest --row-count %d --column-count %d --dataset-spec-sha256 %s --catalog-spec-sha256 %s --query-spec-sha256 %s --normalization-spec-sha256 %s",
		oracleCommandName, rowCount, columnCount, specs.dataset, specs.catalog, specs.query, specs.normalization,
	)
}

func artifactScale(rowCount int64, columnCount int) (string, error) {
	suffix := ""
	switch rowCount {
	case 100:
		suffix = "100x"
	case 10_000:
		suffix = "10k-x"
	case 100_000:
		suffix = "100k-x"
	default:
		return "", errors.New("row-count must be 100, 10000, or 100000")
	}
	if columnCount != 4 && columnCount != 16 {
		return "", errors.New("column-count must be 4 or 16")
	}
	return suffix + strconv.Itoa(columnCount), nil
}

func parseArtifactScale(scale string) (int64, int, error) {
	switch scale {
	case "100x4":
		return 100, 4, nil
	case "10k-x4":
		return 10_000, 4, nil
	case "100k-x4":
		return 100_000, 4, nil
	case "100x16":
		return 100, 16, nil
	case "10k-x16":
		return 10_000, 16, nil
	case "100k-x16":
		return 100_000, 16, nil
	default:
		return 0, 0, fmt.Errorf("artifact manifest scale %q is not formal", scale)
	}
}

func (specs artifactSpecHashes) validate() error {
	for name, value := range map[string]string{
		"dataset-spec-sha256": specs.dataset, "catalog-spec-sha256": specs.catalog,
		"query-spec-sha256": specs.query, "normalization-spec-sha256": specs.normalization,
	} {
		if !isCanonicalSHA256(value) {
			return fmt.Errorf("%s must be an explicit canonical SHA-256", name)
		}
	}
	return nil
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	result := flag.NewFlagSet(name, flag.ContinueOnError)
	result.SetOutput(stderr)
	result.Usage = func() {}
	return result
}

func parseFlags(flags *flag.FlagSet, args []string) error {
	if err := rejectDuplicateFlags(args); err != nil {
		return err
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("positional arguments are not accepted")
	}
	return nil
}

// Every flag in this command consumes a value, so a small pre-scan can reject
// ambiguous duplicate flags before flag.FlagSet applies last-value-wins.
func rejectDuplicateFlags(args []string) error {
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" || !strings.HasPrefix(argument, "-") || argument == "-" {
			continue
		}
		trimmed := strings.TrimLeft(argument, "-")
		name, _, inline := strings.Cut(trimmed, "=")
		if name == "" {
			continue
		}
		if seen[name] {
			return fmt.Errorf("flag %q was provided more than once", name)
		}
		seen[name] = true
		if !inline {
			index++
		}
	}
	return nil
}

func parseCanonicalInt64(name, value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || strconv.FormatInt(parsed, 10) != value {
		return 0, fmt.Errorf("%s must be a canonical base-10 int64", name)
	}
	return parsed, nil
}

func isCanonicalSHA256(value string) bool {
	if len(value) != 2*32 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func readManifest(path string, stdin io.Reader) ([]byte, error) {
	if path == "-" {
		return readBounded(stdin)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()
	return readBounded(file)
}

func readBounded(input io.Reader) ([]byte, error) {
	if input == nil {
		return nil, errors.New("manifest input is nil")
	}
	value, err := io.ReadAll(io.LimitReader(input, maximumManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(value) > maximumManifestBytes {
		return nil, errors.New("manifest exceeds the one-MiB verification limit")
	}
	return value, nil
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write JSON output: %w", err)
	}
	return nil
}

func writeBytes(output io.Writer, value []byte) error {
	written, err := io.Copy(output, bytes.NewReader(value))
	if err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if written != int64(len(value)) {
		return io.ErrShortWrite
	}
	return nil
}
