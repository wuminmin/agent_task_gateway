package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

const (
	reportSchemaVersion         = 1
	receiptSchemaVersion        = "taskgate-snapshot-verification-receipt-v1"
	receiptDigestDomain         = "taskgate/snapshot-verification-receipt/v1\x00"
	maxBundleManifestBytes      = int64(4 << 20)
	maxVerificationReceiptBytes = int64(4 << 20)
	maxHotArtifactBytes         = int64(1024 << 20)
	maxPublishedBytes           = int64(2 << 30)
)

var (
	publicationNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	identifierPattern      = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	sidecarNamePattern     = regexp.MustCompile(`^taskgate_ordinal\.[a-z_][a-z0-9_]*$`)
	digestPattern          = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errUsage               = errors.New("invalid command line")
)

type stringList []string

func (values *stringList) String() string {
	return strings.Join(*values, ",")
}

func (values *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

type commandOptions struct {
	mode              string
	inputs            []string
	outputDirectory   string
	artifactDirectory string
	receiptPath       string
	receiptSHA256     string
}

type buildDependencies struct {
	scan             func(context.Context, snapshotbundle.CompilerInput, string) (snapshotbundle.CompilerInput, error)
	requireImmutable func(string) error
}

type artifactIdentity struct {
	Device           uint64 `json:"device"`
	Inode            uint64 `json:"inode"`
	Mode             uint32 `json:"mode"`
	Size             int64  `json:"size"`
	ModifiedUnixNano int64  `json:"modified_unix_nano"`
	ChangedUnixNano  int64  `json:"changed_unix_nano"`
}

type verifiedArtifact struct {
	Name     string           `json:"name"`
	SHA256   string           `json:"sha256"`
	Bytes    int64            `json:"bytes"`
	Identity artifactIdentity `json:"identity"`
}

type verifiedPublication struct {
	PublicationName string                 `json:"publication_name"`
	Directory       artifactIdentity       `json:"directory"`
	BundleSHA256    string                 `json:"bundle_sha256"`
	Measurement     publicationMeasurement `json:"measurement"`
	Artifacts       []verifiedArtifact     `json:"artifacts"`
}

type verificationReceipt struct {
	SchemaVersion     string                `json:"schema_version"`
	VerifiedAt        string                `json:"verified_at"`
	ArtifactRoot      artifactIdentity      `json:"artifact_root"`
	Publications      []verifiedPublication `json:"publications"`
	ReceiptBodySHA256 string                `json:"receipt_body_sha256"`
}

type publicationMeasurement struct {
	PublicationName   string `json:"publication_name"`
	RowCount          uint64 `json:"row_count"`
	ManifestDigest    string `json:"manifest_digest"`
	DictionaryDigest  string `json:"dictionary_digest"`
	SidecarDigest     string `json:"sidecar_digest"`
	ColdPayloadDigest string `json:"cold_payload_digest"`
	HotIndexDigest    string `json:"hot_index_digest"`
	ArtifactBytes     int64  `json:"artifact_bytes"`
	HotArtifactBytes  int64  `json:"hot_artifact_bytes"`
}

type commandReport struct {
	SchemaVersion             int                      `json:"schema_version"`
	Mode                      string                   `json:"mode"`
	Publications              []publicationMeasurement `json:"publications"`
	TotalArtifactBytes        int64                    `json:"total_artifact_bytes"`
	HotArtifactBytes          int64                    `json:"hot_artifact_bytes"`
	VerificationReceiptSHA256 string                   `json:"verification_receipt_sha256,omitempty"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, buildDependencies{
		scan: snapshotbundle.ScanPostgresSnapshot, requireImmutable: requireReadOnlyArtifactRoot,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "v4-offline:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer,
	dependencies buildDependencies) error {
	if ctx == nil || getenv == nil || stdout == nil {
		return errors.New("command context, environment, and output are required")
	}
	options, err := parseCommand(args)
	if err != nil {
		return err
	}
	var report commandReport
	switch options.mode {
	case "build":
		if dependencies.scan == nil {
			return errors.New("snapshot scanner is required")
		}
		report, err = build(ctx, options, strings.TrimSpace(getenv("SNAPSHOT_POSTGRES_DSN")), dependencies)
	case "verify":
		report, err = verify(options, dependencies)
	case "activate":
		report, err = activate(options, dependencies)
	default:
		return fmt.Errorf("%w: expected build, verify, or activate", errUsage)
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(report)
}

func parseCommand(args []string) (commandOptions, error) {
	if len(args) == 0 {
		return commandOptions{}, fmt.Errorf("%w: expected build, verify, or activate", errUsage)
	}
	mode := args[0]
	flags := flag.NewFlagSet("v4-offline "+mode, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var inputs stringList
	flags.Var(&inputs, "input", "path to taskgate-snapshot-index-input-v1 JSON (repeatable)")
	outputDirectory := flags.String("output-dir", "", "base directory for newly built publications")
	artifactDirectory := flags.String("artifact-dir", "", "directory containing immutable publication bundles")
	receiptPath := flags.String("receipt", "", "verification receipt input (activate) or output (verify)")
	receiptSHA256 := flags.String("receipt-sha256", "", "expected SHA-256 of the canonical verification receipt")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || len(inputs) == 0 {
		return commandOptions{}, fmt.Errorf("%w: %s requires one or more -input flags", errUsage, mode)
	}
	options := commandOptions{mode: mode, inputs: append([]string(nil), inputs...),
		outputDirectory: strings.TrimSpace(*outputDirectory), artifactDirectory: strings.TrimSpace(*artifactDirectory),
		receiptPath: strings.TrimSpace(*receiptPath), receiptSHA256: strings.TrimSpace(*receiptSHA256)}
	switch mode {
	case "build":
		if options.outputDirectory == "" || options.artifactDirectory != "" || options.receiptPath != "" || options.receiptSHA256 != "" {
			return commandOptions{}, fmt.Errorf("%w: build requires only -output-dir", errUsage)
		}
	case "verify":
		if options.artifactDirectory == "" || options.outputDirectory != "" || options.receiptPath == "" || options.receiptSHA256 != "" {
			return commandOptions{}, fmt.Errorf("%w: verify requires -artifact-dir and -receipt", errUsage)
		}
	case "activate":
		if options.artifactDirectory == "" || options.outputDirectory != "" || options.receiptPath == "" ||
			!digestPattern.MatchString(options.receiptSHA256) {
			return commandOptions{}, fmt.Errorf("%w: activate requires -artifact-dir, -receipt, and lowercase -receipt-sha256", errUsage)
		}
	default:
		return commandOptions{}, fmt.Errorf("%w: unknown mode %q", errUsage, mode)
	}
	return options, nil
}

func build(ctx context.Context, options commandOptions, dsn string, dependencies buildDependencies) (commandReport, error) {
	if dsn == "" {
		return commandReport{}, errors.New("SNAPSHOT_POSTGRES_DSN is required for build")
	}
	inputs, err := loadInputs(options.inputs, false)
	if err != nil {
		return commandReport{}, err
	}
	report := commandReport{SchemaVersion: reportSchemaVersion, Mode: "build",
		Publications: make([]publicationMeasurement, 0, len(inputs))}
	for index, input := range inputs {
		if err := ctx.Err(); err != nil {
			return commandReport{}, err
		}
		measurement, err := buildPublication(ctx, input, dsn, options.outputDirectory,
			maxPublishedBytes-report.TotalArtifactBytes, maxHotArtifactBytes-report.HotArtifactBytes, dependencies)
		if err != nil {
			return commandReport{}, fmt.Errorf("build input %d publication %q: %w", index+1, input.PublicationName, err)
		}
		if err := addMeasurement(&report, measurement); err != nil {
			return commandReport{}, err
		}
		report.Publications = append(report.Publications, measurement)
		// Production normally uses one builder process per publication. Explicitly
		// release the completed bundle before scanning the next input so this
		// single-PID measurement preserves the same serialized working-set boundary.
		runtime.GC()
	}
	return report, nil
}

func buildPublication(ctx context.Context, input snapshotbundle.CompilerInput, dsn, outputDirectory string,
	remainingArtifactBytes, remainingHotBytes int64, dependencies buildDependencies) (publicationMeasurement, error) {
	if strings.TrimSpace(input.SourceRelation) == "" {
		return publicationMeasurement{}, errors.New("source_relation is required")
	}
	scanned, err := dependencies.scan(ctx, input, dsn)
	if err != nil {
		return publicationMeasurement{}, fmt.Errorf("scan PostgreSQL snapshot: %w", err)
	}
	if remainingArtifactBytes <= 0 || remainingHotBytes <= 0 {
		return publicationMeasurement{}, errors.New("combined snapshot artifact limits are exhausted")
	}
	written, err := snapshotbundle.CompileOwnedToDirectory(&scanned, outputDirectory, snapshotbundle.PublicationLimits{
		MaxBytes: remainingArtifactBytes, MaxHotBytes: remainingHotBytes,
	})
	if err != nil {
		return publicationMeasurement{}, fmt.Errorf("compile and publish snapshot bundle: %w", err)
	}
	return measurementFromManifest(written.Manifest, written.Bytes), nil
}

func verify(options commandOptions, dependencies buildDependencies) (commandReport, error) {
	inputs, err := loadInputs(options.inputs, true)
	if err != nil {
		return commandReport{}, err
	}
	if err := verifyArtifactRoot(options.artifactDirectory, inputs); err != nil {
		return commandReport{}, err
	}
	if dependencies.requireImmutable == nil {
		return commandReport{}, errors.New("immutable artifact-root verifier is required")
	}
	if err := dependencies.requireImmutable(options.artifactDirectory); err != nil {
		return commandReport{}, fmt.Errorf("immutable artifact root: %w", err)
	}
	rootIdentity, err := readPathIdentity(options.artifactDirectory, true)
	if err != nil {
		return commandReport{}, fmt.Errorf("artifact root identity: %w", err)
	}
	report := commandReport{SchemaVersion: reportSchemaVersion, Mode: "verify",
		Publications: make([]publicationMeasurement, 0, len(inputs))}
	receipt := verificationReceipt{SchemaVersion: receiptSchemaVersion,
		VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano), ArtifactRoot: rootIdentity,
		Publications: make([]verifiedPublication, 0, len(inputs))}
	indexes := make([]*ordinal.HotDictionary, 0, len(inputs))
	for index, input := range inputs {
		measurement, hot, publication, err := verifyPublication(options.artifactDirectory, input,
			maxPublishedBytes-report.TotalArtifactBytes, maxHotArtifactBytes-report.HotArtifactBytes)
		if err != nil {
			return commandReport{}, fmt.Errorf("verify input %d publication %q: %w", index+1, input.PublicationName, err)
		}
		if err := addMeasurement(&report, measurement); err != nil {
			return commandReport{}, err
		}
		report.Publications = append(report.Publications, measurement)
		receipt.Publications = append(receipt.Publications, publication)
		indexes = append(indexes, hot)
	}
	rootIdentityAfter, err := readPathIdentity(options.artifactDirectory, true)
	if err != nil || !reflect.DeepEqual(rootIdentityAfter, rootIdentity) {
		return commandReport{}, errors.New("artifact root identity changed during strict verification")
	}
	receiptSHA256, err := writeVerificationReceipt(options.receiptPath, &receipt)
	if err != nil {
		return commandReport{}, err
	}
	report.VerificationReceiptSHA256 = receiptSHA256
	runtime.KeepAlive(indexes)
	return report, nil
}

func activate(options commandOptions, dependencies buildDependencies) (commandReport, error) {
	inputs, err := loadInputs(options.inputs, true)
	if err != nil {
		return commandReport{}, err
	}
	if err := verifyArtifactRoot(options.artifactDirectory, inputs); err != nil {
		return commandReport{}, err
	}
	if dependencies.requireImmutable == nil {
		return commandReport{}, errors.New("immutable artifact-root verifier is required")
	}
	if err := dependencies.requireImmutable(options.artifactDirectory); err != nil {
		return commandReport{}, fmt.Errorf("immutable artifact root: %w", err)
	}
	receipt, receiptSHA256, err := readVerificationReceipt(options.receiptPath, options.receiptSHA256)
	if err != nil {
		return commandReport{}, err
	}
	rootIdentity, err := readPathIdentity(options.artifactDirectory, true)
	if err != nil || !reflect.DeepEqual(rootIdentity, receipt.ArtifactRoot) {
		return commandReport{}, errors.New("artifact root identity differs from strict verification receipt")
	}
	if len(receipt.Publications) != len(inputs) {
		return commandReport{}, errors.New("verification receipt publication count differs from approved inputs")
	}
	report := commandReport{SchemaVersion: reportSchemaVersion, Mode: "activate",
		Publications: make([]publicationMeasurement, 0, len(inputs)), VerificationReceiptSHA256: receiptSHA256}
	indexes := make([]*ordinal.HotDictionary, 0, len(inputs))
	for index, input := range inputs {
		measurement, hot, err := activateVerifiedPublication(options.artifactDirectory, input,
			receipt.Publications[index], maxPublishedBytes-report.TotalArtifactBytes,
			maxHotArtifactBytes-report.HotArtifactBytes)
		if err != nil {
			return commandReport{}, fmt.Errorf("activate input %d publication %q: %w", index+1, input.PublicationName, err)
		}
		if err := addMeasurement(&report, measurement); err != nil {
			return commandReport{}, err
		}
		report.Publications = append(report.Publications, measurement)
		indexes = append(indexes, hot)
	}
	rootIdentityAfter, err := readPathIdentity(options.artifactDirectory, true)
	if err != nil || !reflect.DeepEqual(rootIdentityAfter, receipt.ArtifactRoot) {
		return commandReport{}, errors.New("artifact root identity changed during warm activation")
	}
	// The strict phase already verified COLD and sidecar contents. Keep all HOT
	// dictionaries resident here so this remains an end-to-end activation
	// measurement rather than a per-file parse microbenchmark.
	runtime.KeepAlive(indexes)
	return report, nil
}

func verifyPublication(baseDirectory string, input snapshotbundle.CompilerInput,
	remainingArtifactBytes, remainingHotBytes int64) (publicationMeasurement, *ordinal.HotDictionary,
	verifiedPublication, error) {
	directory := filepath.Join(baseDirectory, input.PublicationName)
	if err := verifyPublicationDirectory(directory, input.PublicationName); err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, err
	}
	directoryIdentity, err := readPathIdentity(directory, true)
	if err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, err
	}
	manifestPath := filepath.Join(directory, input.PublicationName+".bundle.json")
	manifestIdentity, err := readPathIdentity(manifestPath, false)
	if err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, err
	}
	manifestBytes, err := readRegularFile(manifestPath, maxBundleManifestBytes)
	if err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, err
	}
	manifest, err := snapshotbundle.DecodeBundleManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, err
	}
	if err := matchManifestToInput(manifest, input); err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, err
	}
	manifestIdentityAfter, err := readPathIdentity(manifestPath, false)
	if err != nil || !reflect.DeepEqual(manifestIdentityAfter, manifestIdentity) {
		return publicationMeasurement{}, nil, verifiedPublication{}, errors.New("bundle manifest identity changed during strict verification")
	}
	artifactBytes, err := sumBytes(int64(len(manifestBytes)), manifest.Hot.Bytes, manifest.Cold.Bytes, manifest.Sidecar.Bytes)
	if err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, err
	}
	if remainingArtifactBytes < 0 || artifactBytes > remainingArtifactBytes {
		return publicationMeasurement{}, nil, verifiedPublication{}, errors.New("combined snapshot artifacts exceed 2 GiB")
	}
	if manifest.Hot.Bytes > remainingHotBytes || remainingHotBytes < 0 {
		return publicationMeasurement{}, nil, verifiedPublication{}, errors.New("combined HOT artifacts exceed 1024 MiB")
	}
	descriptors := []snapshotbundle.FileDescriptor{manifest.Hot, manifest.Cold, manifest.Sidecar}
	identities := make([]artifactIdentity, len(descriptors))
	for index, descriptor := range descriptors {
		identity, identityErr := readPathIdentity(filepath.Join(directory, descriptor.Name), false)
		if identityErr != nil || identity.Size != descriptor.Bytes {
			return publicationMeasurement{}, nil, verifiedPublication{}, fmt.Errorf("capture artifact %q identity before verification", descriptor.Name)
		}
		identities[index] = identity
	}
	hotBytes, err := readRegularFile(filepath.Join(directory, manifest.Hot.Name), remainingHotBytes)
	if err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, err
	}
	if err := verifyDescriptor(manifest.Hot, hotBytes); err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, err
	}
	hot, err := ordinal.ParseHotDictionary(hotBytes, manifest.ManifestDigest)
	if err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, fmt.Errorf("parse HOT artifact: %w", err)
	}
	if hot.RowCount() != manifest.RowCount || hot.ManifestDigest() != manifest.ManifestDigest ||
		hot.DictionaryDigest() != manifest.DictionaryManifest.DictionaryDigest ||
		!reflect.DeepEqual(hot.Manifest(), manifest.DictionaryManifest) {
		return publicationMeasurement{}, nil, verifiedPublication{}, errors.New("HOT artifact does not match bundle manifest")
	}
	if err := verifyCold(filepath.Join(directory, manifest.Cold.Name), manifest.Cold, manifest.ManifestDigest); err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, fmt.Errorf("verify COLD envelope: %w", err)
	}
	expectation := snapshotbundle.SidecarExpectation{PublicationName: input.PublicationName,
		OrdinalSidecar: input.OrdinalSidecar, SourceNamespace: input.Snapshot.SourceNamespace,
		ManifestDigest: manifest.ManifestDigest, SidecarDigest: manifest.DictionaryManifest.SidecarDigest}
	if err := verifySidecar(filepath.Join(directory, manifest.Sidecar.Name), manifest.Sidecar, hot, expectation); err != nil {
		return publicationMeasurement{}, nil, verifiedPublication{}, fmt.Errorf("verify sidecar: %w", err)
	}
	measurement := measurementFromManifest(manifest, artifactBytes)
	directoryIdentityAfter, err := readPathIdentity(directory, true)
	if err != nil || !reflect.DeepEqual(directoryIdentityAfter, directoryIdentity) {
		return publicationMeasurement{}, nil, verifiedPublication{}, errors.New("publication directory identity changed during strict verification")
	}
	bundleDigest := sha256.Sum256(manifestBytes)
	verified := verifiedPublication{PublicationName: input.PublicationName, Directory: directoryIdentity,
		BundleSHA256: hex.EncodeToString(bundleDigest[:]), Measurement: measurement,
		Artifacts: make([]verifiedArtifact, 0, 4)}
	allDescriptors := append([]snapshotbundle.FileDescriptor{{Name: filepath.Base(manifestPath),
		SHA256: verified.BundleSHA256, Bytes: int64(len(manifestBytes))}}, descriptors...)
	allIdentities := append([]artifactIdentity{manifestIdentityAfter}, identities...)
	for index, descriptor := range allDescriptors {
		identity, identityErr := readPathIdentity(filepath.Join(directory, descriptor.Name), false)
		if identityErr != nil || !reflect.DeepEqual(identity, allIdentities[index]) {
			return publicationMeasurement{}, nil, verifiedPublication{}, fmt.Errorf("artifact %q identity changed during strict verification", descriptor.Name)
		}
		verified.Artifacts = append(verified.Artifacts, verifiedArtifact{Name: descriptor.Name,
			SHA256: descriptor.SHA256, Bytes: descriptor.Bytes, Identity: identity})
	}
	return measurement, hot, verified, nil
}

func activateVerifiedPublication(baseDirectory string, input snapshotbundle.CompilerInput,
	verified verifiedPublication, remainingArtifactBytes, remainingHotBytes int64) (
	publicationMeasurement, *ordinal.HotDictionary, error) {
	if verified.PublicationName != input.PublicationName || len(verified.Artifacts) != 4 {
		return publicationMeasurement{}, nil, errors.New("verification receipt does not match approved publication")
	}
	directory := filepath.Join(baseDirectory, input.PublicationName)
	if err := verifyPublicationDirectory(directory, input.PublicationName); err != nil {
		return publicationMeasurement{}, nil, err
	}
	directoryIdentity, err := readPathIdentity(directory, true)
	if err != nil || !reflect.DeepEqual(directoryIdentity, verified.Directory) {
		return publicationMeasurement{}, nil, errors.New("publication directory identity differs from strict verification receipt")
	}
	manifestPath := filepath.Join(directory, input.PublicationName+".bundle.json")
	manifestBytes, err := readRegularFile(manifestPath, maxBundleManifestBytes)
	if err != nil {
		return publicationMeasurement{}, nil, err
	}
	bundleDigest := sha256.Sum256(manifestBytes)
	if hex.EncodeToString(bundleDigest[:]) != verified.BundleSHA256 {
		return publicationMeasurement{}, nil, errors.New("bundle manifest digest differs from strict verification receipt")
	}
	manifest, err := snapshotbundle.DecodeBundleManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		return publicationMeasurement{}, nil, err
	}
	if err := matchManifestToInput(manifest, input); err != nil {
		return publicationMeasurement{}, nil, err
	}
	artifactBytes, err := sumBytes(int64(len(manifestBytes)), manifest.Hot.Bytes, manifest.Cold.Bytes, manifest.Sidecar.Bytes)
	if err != nil || remainingArtifactBytes < 0 || artifactBytes > remainingArtifactBytes {
		return publicationMeasurement{}, nil, errors.New("combined snapshot artifacts exceed 2 GiB")
	}
	if manifest.Hot.Bytes > remainingHotBytes || remainingHotBytes < 0 {
		return publicationMeasurement{}, nil, errors.New("combined HOT artifacts exceed 1024 MiB")
	}
	measurement := measurementFromManifest(manifest, artifactBytes)
	if !reflect.DeepEqual(measurement, verified.Measurement) {
		return publicationMeasurement{}, nil, errors.New("bundle measurement differs from strict verification receipt")
	}
	descriptors := []snapshotbundle.FileDescriptor{
		{Name: filepath.Base(manifestPath), SHA256: verified.BundleSHA256, Bytes: int64(len(manifestBytes))},
		manifest.Hot, manifest.Cold, manifest.Sidecar,
	}
	for index, descriptor := range descriptors {
		artifact := verified.Artifacts[index]
		if artifact.Name != descriptor.Name || artifact.SHA256 != descriptor.SHA256 || artifact.Bytes != descriptor.Bytes {
			return publicationMeasurement{}, nil, fmt.Errorf("artifact %q descriptor differs from strict verification receipt", descriptor.Name)
		}
		identity, identityErr := readPathIdentity(filepath.Join(directory, descriptor.Name), false)
		if identityErr != nil || !reflect.DeepEqual(identity, artifact.Identity) {
			return publicationMeasurement{}, nil, fmt.Errorf("artifact %q identity differs from strict verification receipt", descriptor.Name)
		}
	}

	// The receipt and immutable inode checks above bind COLD and sidecar to the
	// strict phase. Warm activation reads and parses only the manifest and HOT
	// index, while still rechecking HOT transport and semantic digests.
	hotBytes, err := readRegularFile(filepath.Join(directory, manifest.Hot.Name), remainingHotBytes)
	if err != nil {
		return publicationMeasurement{}, nil, err
	}
	if err := verifyDescriptor(manifest.Hot, hotBytes); err != nil {
		return publicationMeasurement{}, nil, err
	}
	hot, err := ordinal.ParseHotDictionary(hotBytes, manifest.ManifestDigest)
	if err != nil {
		return publicationMeasurement{}, nil, fmt.Errorf("parse HOT artifact: %w", err)
	}
	if hot.RowCount() != manifest.RowCount || hot.ManifestDigest() != manifest.ManifestDigest ||
		hot.DictionaryDigest() != manifest.DictionaryManifest.DictionaryDigest ||
		!reflect.DeepEqual(hot.Manifest(), manifest.DictionaryManifest) {
		return publicationMeasurement{}, nil, errors.New("HOT artifact does not match bundle manifest")
	}
	for index, descriptor := range descriptors {
		identity, identityErr := readPathIdentity(filepath.Join(directory, descriptor.Name), false)
		if identityErr != nil || !reflect.DeepEqual(identity, verified.Artifacts[index].Identity) {
			return publicationMeasurement{}, nil, fmt.Errorf("artifact %q identity changed during warm activation", descriptor.Name)
		}
	}
	directoryIdentityAfter, err := readPathIdentity(directory, true)
	if err != nil || !reflect.DeepEqual(directoryIdentityAfter, verified.Directory) {
		return publicationMeasurement{}, nil, errors.New("publication directory identity changed during warm activation")
	}
	return measurement, hot, nil
}

func readPathIdentity(path string, directory bool) (artifactIdentity, error) {
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || before.IsDir() != directory || (!directory && !before.Mode().IsRegular()) {
		return artifactIdentity{}, fmt.Errorf("artifact %q has an invalid file type", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return artifactIdentity{}, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || after.IsDir() != directory || (!directory && !after.Mode().IsRegular()) {
		return artifactIdentity{}, fmt.Errorf("artifact %q changed while opening", filepath.Base(path))
	}
	identity, err := identityFromFile(file)
	if err != nil {
		return artifactIdentity{}, err
	}
	if identity.Device == 0 || identity.Inode == 0 || identity.Size != after.Size() || identity.Size < 0 {
		return artifactIdentity{}, fmt.Errorf("artifact %q has an invalid stable identity", filepath.Base(path))
	}
	return identity, nil
}

func writeVerificationReceipt(path string, receipt *verificationReceipt) (string, error) {
	if receipt == nil || strings.TrimSpace(path) == "" {
		return "", errors.New("verification receipt path and body are required")
	}
	receipt.ReceiptBodySHA256 = verificationReceiptBodyDigest(*receipt)
	if err := validateVerificationReceipt(*receipt); err != nil {
		return "", err
	}
	encoded, err := canonicalVerificationReceipt(*receipt)
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create verification receipt: %w", err)
	}
	_, writeErr := file.Write(encoded)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return "", fmt.Errorf("write verification receipt: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func readVerificationReceipt(path, expectedSHA256 string) (verificationReceipt, string, error) {
	if !digestPattern.MatchString(expectedSHA256) {
		return verificationReceipt{}, "", errors.New("expected verification receipt SHA-256 is invalid")
	}
	encoded, err := readRegularFile(path, maxVerificationReceiptBytes)
	if err != nil {
		return verificationReceipt{}, "", fmt.Errorf("read verification receipt: %w", err)
	}
	digest := sha256.Sum256(encoded)
	actualSHA256 := hex.EncodeToString(digest[:])
	if actualSHA256 != expectedSHA256 {
		return verificationReceipt{}, "", errors.New("verification receipt SHA-256 differs from measured strict-verification output")
	}
	var receipt verificationReceipt
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return verificationReceipt{}, "", fmt.Errorf("decode verification receipt: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return verificationReceipt{}, "", errors.New("verification receipt contains trailing JSON")
	}
	canonical, err := canonicalVerificationReceipt(receipt)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return verificationReceipt{}, "", errors.New("verification receipt is not canonical")
	}
	if err := validateVerificationReceipt(receipt); err != nil {
		return verificationReceipt{}, "", err
	}
	return receipt, actualSHA256, nil
}

func canonicalVerificationReceipt(receipt verificationReceipt) ([]byte, error) {
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func verificationReceiptBodyDigest(receipt verificationReceipt) string {
	receipt.ReceiptBodySHA256 = ""
	encoded, _ := json.Marshal(receipt)
	digest := sha256.New()
	_, _ = digest.Write([]byte(receiptDigestDomain))
	_, _ = digest.Write(encoded)
	return hex.EncodeToString(digest.Sum(nil))
}

func validateVerificationReceipt(receipt verificationReceipt) error {
	if receipt.SchemaVersion != receiptSchemaVersion || !digestPattern.MatchString(receipt.ReceiptBodySHA256) ||
		receipt.ReceiptBodySHA256 != verificationReceiptBodyDigest(receipt) || len(receipt.Publications) == 0 {
		return errors.New("verification receipt schema or body digest is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, receipt.VerifiedAt); err != nil {
		return errors.New("verification receipt timestamp is invalid")
	}
	if receipt.ArtifactRoot.Device == 0 || receipt.ArtifactRoot.Inode == 0 {
		return errors.New("verification receipt artifact-root identity is invalid")
	}
	seen := make(map[string]struct{}, len(receipt.Publications))
	for _, publication := range receipt.Publications {
		if !publicationNamePattern.MatchString(publication.PublicationName) ||
			!digestPattern.MatchString(publication.BundleSHA256) || publication.Directory.Device == 0 ||
			publication.Directory.Inode == 0 || len(publication.Artifacts) != 4 ||
			publication.Measurement.PublicationName != publication.PublicationName {
			return errors.New("verification receipt publication is invalid")
		}
		if _, duplicate := seen[publication.PublicationName]; duplicate {
			return errors.New("verification receipt repeats a publication")
		}
		seen[publication.PublicationName] = struct{}{}
		artifactNames := make(map[string]struct{}, len(publication.Artifacts))
		for _, artifact := range publication.Artifacts {
			if strings.TrimSpace(artifact.Name) == "" || filepath.Base(artifact.Name) != artifact.Name ||
				!digestPattern.MatchString(artifact.SHA256) || artifact.Bytes <= 0 ||
				artifact.Identity.Device == 0 || artifact.Identity.Inode == 0 || artifact.Identity.Size != artifact.Bytes {
				return errors.New("verification receipt artifact is invalid")
			}
			if _, duplicate := artifactNames[artifact.Name]; duplicate {
				return errors.New("verification receipt repeats an artifact")
			}
			artifactNames[artifact.Name] = struct{}{}
		}
	}
	return nil
}

func loadInputs(paths []string, requireExpectedDigests bool) ([]snapshotbundle.CompilerInput, error) {
	inputs := make([]snapshotbundle.CompilerInput, 0, len(paths))
	seenPaths := make(map[string]struct{}, len(paths))
	seenPublications := make(map[string]struct{}, len(paths))
	for index, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", index+1, err)
		}
		if _, duplicate := seenPaths[absolute]; duplicate {
			return nil, fmt.Errorf("input %d duplicates %q", index+1, path)
		}
		seenPaths[absolute] = struct{}{}
		file, err := os.Open(absolute)
		if err != nil {
			return nil, fmt.Errorf("open input %d: %w", index+1, err)
		}
		input, decodeErr := snapshotbundle.DecodeCompilerInput(file)
		closeErr := file.Close()
		if decodeErr != nil || closeErr != nil {
			return nil, fmt.Errorf("read input %d: %w", index+1, errors.Join(decodeErr, closeErr))
		}
		if err := validateInputIdentity(input, requireExpectedDigests); err != nil {
			return nil, fmt.Errorf("input %d: %w", index+1, err)
		}
		if _, duplicate := seenPublications[input.PublicationName]; duplicate {
			return nil, fmt.Errorf("input %d duplicates publication %q", index+1, input.PublicationName)
		}
		seenPublications[input.PublicationName] = struct{}{}
		inputs = append(inputs, input)
	}
	return inputs, nil
}

func validateInputIdentity(input snapshotbundle.CompilerInput, requireExpectedDigests bool) error {
	if input.Version != snapshotbundle.CompilerInputVersion || !publicationNamePattern.MatchString(input.PublicationName) ||
		!identifierPattern.MatchString(input.CatalogSource) || !sidecarNamePattern.MatchString(input.OrdinalSidecar) ||
		strings.TrimSpace(input.Snapshot.SourceID) == "" || strings.TrimSpace(input.Snapshot.SourceNamespace) == "" ||
		strings.TrimSpace(input.Snapshot.Snapshot) == "" || !digestPattern.MatchString(input.Snapshot.SchemaDigest) ||
		len(input.Snapshot.Fields) == 0 || len(input.EntityKeyFields) == 0 {
		return errors.New("snapshot compiler input identity is incomplete")
	}
	if requireExpectedDigests {
		for _, expected := range []struct {
			name   string
			digest string
		}{
			{name: "manifest", digest: input.ExpectedDigests.ManifestDigest},
			{name: "dictionary", digest: input.ExpectedDigests.DictionaryDigest},
			{name: "sidecar", digest: input.ExpectedDigests.SidecarDigest},
			{name: "cold payload", digest: input.ExpectedDigests.ColdPayloadDigest},
			{name: "hot index", digest: input.ExpectedDigests.HotIndexDigest},
		} {
			if !digestPattern.MatchString(expected.digest) {
				return fmt.Errorf("approved %s digest is required", expected.name)
			}
		}
	}
	return nil
}

func verifyArtifactRoot(baseDirectory string, inputs []snapshotbundle.CompilerInput) error {
	info, err := os.Lstat(baseDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("artifact root must be a real directory")
	}
	expected := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		expected[input.PublicationName] = struct{}{}
	}
	entries, err := os.ReadDir(baseDirectory)
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return errors.New("artifact root does not contain exactly the requested publications")
	}
	for _, entry := range entries {
		if _, found := expected[entry.Name()]; !found {
			return fmt.Errorf("artifact root contains unexpected publication %q", entry.Name())
		}
		entryInfo, err := entry.Info()
		if err != nil || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || entryInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("publication %q is not a real directory", entry.Name())
		}
	}
	return nil
}

func verifyPublicationDirectory(directory, publicationName string) error {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("publication path must be a real directory")
	}
	expected := map[string]struct{}{
		publicationName + ".bundle.json": {}, publicationName + ".hot.tgord": {},
		publicationName + ".cold.tgord": {}, publicationName + ".sidecar.ndjson": {},
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return errors.New("publication directory is incomplete or contains unknown files")
	}
	for _, entry := range entries {
		if _, found := expected[entry.Name()]; !found {
			return fmt.Errorf("unexpected publication artifact %q", entry.Name())
		}
		entryInfo, err := entry.Info()
		if err != nil || entry.Type()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() ||
			entryInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("publication artifact %q is not a regular file", entry.Name())
		}
	}
	return nil
}

func matchManifestToInput(manifest snapshotbundle.BundleManifest, input snapshotbundle.CompilerInput) error {
	expected := input.ExpectedDigests
	dictionary := manifest.DictionaryManifest
	if manifest.PublicationName != input.PublicationName || manifest.CatalogSource != input.CatalogSource ||
		manifest.OrdinalSidecar != input.OrdinalSidecar || manifest.ManifestDigest != expected.ManifestDigest ||
		dictionary.SourceID != input.Snapshot.SourceID || dictionary.SourceNamespace != input.Snapshot.SourceNamespace ||
		dictionary.Snapshot != input.Snapshot.Snapshot || dictionary.SchemaDigest != input.Snapshot.SchemaDigest ||
		dictionary.DictionaryDigest != expected.DictionaryDigest || dictionary.SidecarDigest != expected.SidecarDigest ||
		dictionary.ColdPayloadDigest != expected.ColdPayloadDigest || dictionary.HotIndexDigest != expected.HotIndexDigest {
		return errors.New("bundle manifest does not match approved compiler input")
	}
	return nil
}

func readRegularFile(path string, maximum int64) ([]byte, error) {
	file, size, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if size <= 0 || size > maximum || size > int64(^uint(0)>>1) {
		return nil, fmt.Errorf("artifact %q has invalid or excessive size", filepath.Base(path))
	}
	payload, err := io.ReadAll(io.LimitReader(file, size+1))
	if err != nil || int64(len(payload)) != size {
		return nil, fmt.Errorf("read artifact %q: size changed", filepath.Base(path))
	}
	return payload, nil
}

func openRegularFile(path string) (*os.File, int64, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("artifact %q is not a regular file", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() {
		_ = file.Close()
		return nil, 0, fmt.Errorf("artifact %q changed while opening", filepath.Base(path))
	}
	return file, after.Size(), nil
}

func verifyDescriptor(descriptor snapshotbundle.FileDescriptor, payload []byte) error {
	digest := sha256.Sum256(payload)
	if descriptor.Bytes != int64(len(payload)) || descriptor.SHA256 != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("artifact %q digest or size differs from bundle", descriptor.Name)
	}
	return nil
}

func verifyCold(path string, descriptor snapshotbundle.FileDescriptor, manifestDigest string) error {
	file, size, err := openRegularFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if size != descriptor.Bytes {
		return errors.New("COLD artifact size differs from bundle")
	}
	digest, err := ordinal.VerifyColdDictionaryEnvelopeReader(file, size, manifestDigest)
	if err != nil {
		return err
	}
	if descriptor.SHA256 != hex.EncodeToString(digest[:]) {
		return errors.New("COLD artifact digest differs from bundle")
	}
	return nil
}

func verifySidecar(path string, descriptor snapshotbundle.FileDescriptor, index ordinal.SnapshotIndex,
	expected snapshotbundle.SidecarExpectation) error {
	file, size, err := openRegularFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if size != descriptor.Bytes {
		return errors.New("sidecar artifact size differs from bundle")
	}
	hash := sha256.New()
	if err := snapshotbundle.VerifySidecarNDJSON(io.TeeReader(file, hash), index, expected); err != nil {
		return err
	}
	if descriptor.SHA256 != hex.EncodeToString(hash.Sum(nil)) {
		return errors.New("sidecar artifact digest differs from bundle")
	}
	return nil
}

func measurementFromManifest(manifest snapshotbundle.BundleManifest, artifactBytes int64) publicationMeasurement {
	dictionary := manifest.DictionaryManifest
	return publicationMeasurement{PublicationName: manifest.PublicationName, RowCount: manifest.RowCount,
		ManifestDigest: manifest.ManifestDigest, DictionaryDigest: dictionary.DictionaryDigest,
		SidecarDigest: dictionary.SidecarDigest, ColdPayloadDigest: dictionary.ColdPayloadDigest,
		HotIndexDigest: dictionary.HotIndexDigest, ArtifactBytes: artifactBytes,
		HotArtifactBytes: manifest.Hot.Bytes}
}

func addMeasurement(report *commandReport, measurement publicationMeasurement) error {
	var err error
	report.TotalArtifactBytes, err = sumBytes(report.TotalArtifactBytes, measurement.ArtifactBytes)
	if err != nil || report.TotalArtifactBytes > maxPublishedBytes {
		return errors.New("combined snapshot artifacts exceed 2 GiB")
	}
	report.HotArtifactBytes, err = sumBytes(report.HotArtifactBytes, measurement.HotArtifactBytes)
	if err != nil || report.HotArtifactBytes > maxHotArtifactBytes {
		return errors.New("combined HOT artifacts exceed 1024 MiB")
	}
	return nil
}

func sumBytes(values ...int64) (int64, error) {
	var total int64
	for _, value := range values {
		if value < 0 || value > int64(^uint64(0)>>1)-total {
			return 0, errors.New("artifact byte count overflow")
		}
		total += value
	}
	return total, nil
}
