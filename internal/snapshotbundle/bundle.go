// Package snapshotbundle defines the operational, deterministic envelope used
// to move one offline ordinal snapshot publication into a Gateway deployment.
// The ordinal package remains the semantic authority for FactID and manifest
// digests; this package only validates compiler input and binds files to it.
package snapshotbundle

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
)

const (
	CompilerInputVersion = "taskgate-snapshot-index-input-v1"
	BundleVersion        = "taskgate-snapshot-index-bundle-v1"
	SidecarVersion       = "taskgate-ordinal-sidecar-ndjson-v1"
	maxJSONDocumentBytes = 4 << 20
	maxSidecarLineBytes  = 64 << 20
	// Raised from 2 GiB for the P9.E scale publication: the 750k-row COLD
	// artifact carries ~1.2e7 canonical values. Compiler and loader share
	// this constant, so both sides move together.
	maxPublishedBytes    = uint64(16 << 30)
	maxHotPublishedBytes = uint64(1024 << 20)
)

var (
	configNamePattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	identifierPattern     = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	versionPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	sourceRelationPattern = regexp.MustCompile(`^reporting\.[a-z_][a-z0-9_]*$`)
	sidecarNamePattern    = regexp.MustCompile(`^taskgate_ordinal\.[a-z_][a-z0-9_]*$`)
	digestPattern         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errPublishedSizeLimit = errors.New("snapshot publication size limit exceeded")
)

// CompilerInput is deliberately independent of Catalog YAML so a candidate
// publication can be built and its computed digests reviewed before approval.
type CompilerInput struct {
	Version         string          `json:"version"`
	PublicationName string          `json:"publication_name"`
	CatalogSource   string          `json:"catalog_source"`
	SourceRelation  string          `json:"source_relation,omitempty"`
	OrdinalSidecar  string          `json:"ordinal_sidecar"`
	EntityKeyFields []string        `json:"entity_key_fields"`
	Snapshot        SnapshotInput   `json:"snapshot"`
	ExpectedDigests ExpectedDigests `json:"expected_digests,omitempty"`
}

type SnapshotInput struct {
	SourceID        string          `json:"source_id"`
	SourceNamespace string          `json:"source_namespace"`
	Snapshot        string          `json:"snapshot"`
	SchemaDigest    string          `json:"schema_digest"`
	Fields          []SnapshotField `json:"fields"`
	Rows            []SnapshotRow   `json:"rows"`
}

type SnapshotField struct {
	Name             string `json:"name"`
	CanonicalFieldID string `json:"canonical_field_id,omitempty"`
	SQLType          string `json:"sql_type"`
	Collation        string `json:"collation,omitempty"`
	CollationVersion string `json:"collation_version,omitempty"`
}

type SnapshotRow struct {
	// EntityKey is optional. When present it is treated as an assertion and
	// must equal the key recomputed from EntityKeyFields and typed Values.
	EntityKey string         `json:"entity_key,omitempty"`
	Values    map[string]any `json:"values"`
}

type ExpectedDigests struct {
	SidecarDigest     string `json:"sidecar_digest,omitempty"`
	DictionaryDigest  string `json:"dictionary_digest,omitempty"`
	ManifestDigest    string `json:"manifest_digest,omitempty"`
	ColdPayloadDigest string `json:"cold_payload_digest,omitempty"`
	HotIndexDigest    string `json:"hot_index_digest,omitempty"`
}

type FileDescriptor struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// BundleManifest is the small startup envelope. DictionaryManifest is copied
// from the verified HOT artifact and its own domain-separated digest remains
// the Catalog trust anchor; the file hashes are transport-integrity metadata.
type BundleManifest struct {
	Version            string                     `json:"version"`
	PublicationName    string                     `json:"publication_name"`
	CatalogSource      string                     `json:"catalog_source"`
	OrdinalSidecar     string                     `json:"ordinal_sidecar"`
	ManifestDigest     string                     `json:"manifest_digest"`
	DictionaryManifest ordinal.DictionaryManifest `json:"dictionary_manifest"`
	RowCount           uint64                     `json:"row_count"`
	Hot                FileDescriptor             `json:"hot"`
	Cold               FileDescriptor             `json:"cold"`
	Sidecar            FileDescriptor             `json:"sidecar"`
}

type CompiledBundle struct {
	Manifest BundleManifest
	Hot      []byte
	Cold     []byte
	Sidecar  []byte
}

// WrittenBundle describes one compiler-verified immutable publication. The
// production builder uses this result instead of retaining all artifact byte
// slices until publication.
type WrittenBundle struct {
	Manifest  BundleManifest
	Directory string
	Bytes     int64
}

type PublicationLimits struct {
	MaxBytes               int64
	MaxHotBytes            int64
	AllowExistingIdentical bool
}

func DefaultPublicationLimits() PublicationLimits {
	return PublicationLimits{MaxBytes: int64(maxPublishedBytes), MaxHotBytes: int64(maxHotPublishedBytes)}
}

type SidecarKeyField struct {
	Name    string `json:"name"`
	SQLType string `json:"sql_type"`
}

type SidecarHeader struct {
	Type            string            `json:"type"`
	Version         string            `json:"version"`
	PublicationName string            `json:"publication_name"`
	OrdinalSidecar  string            `json:"ordinal_sidecar"`
	SourceNamespace string            `json:"source_namespace"`
	ManifestDigest  string            `json:"manifest_digest"`
	SidecarDigest   string            `json:"sidecar_digest"`
	EntityKeyFields []SidecarKeyField `json:"entity_key_fields"`
}

type SidecarRow struct {
	Type      string `json:"type"`
	RowHandle uint64 `json:"row_handle"`
	EntityKey string `json:"entity_key"`
	KeyValues []any  `json:"key_values"`
}

type SidecarFooter struct {
	Type          string `json:"type"`
	RowCount      uint64 `json:"row_count"`
	SidecarDigest string `json:"sidecar_digest"`
}

type SidecarExpectation struct {
	PublicationName string
	OrdinalSidecar  string
	SourceNamespace string
	ManifestDigest  string
	SidecarDigest   string
}

func DecodeCompilerInput(reader io.Reader) (CompilerInput, error) {
	var input CompilerInput
	if err := decodeStrictJSON(reader, &input); err != nil {
		return CompilerInput{}, fmt.Errorf("decode snapshot compiler input: %w", err)
	}
	return input, nil
}

func DecodeBundleManifest(reader io.Reader) (BundleManifest, error) {
	var manifest BundleManifest
	if err := decodeStrictJSON(io.LimitReader(reader, maxJSONDocumentBytes+1), &manifest); err != nil {
		return BundleManifest{}, fmt.Errorf("decode snapshot bundle manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return BundleManifest{}, err
	}
	return manifest, nil
}

// Compile creates and immediately independently verifies both artifacts before
// returning. HOT is retained for sidecar verification; COLD is streamed
// through the same canonical and digest checks without building a duplicate
// audit dictionary. This makes the CLI a verifier, not merely a serializer.
func Compile(input CompilerInput) (CompiledBundle, error) {
	return compile(input)
}

// CompileOwned is the publication-builder entrypoint. It transfers ownership
// of the million-scale row slice from input and clears the caller's reference
// before artifact encoding, so scanned row maps cannot overlap the complete
// HOT/COLD byte buffers. Compile remains non-destructive for tests and callers
// that need to retain their input.
func CompileOwned(input *CompilerInput) (CompiledBundle, error) {
	if input == nil {
		return CompiledBundle{}, errors.New("snapshot compiler input is required")
	}
	owned := *input
	input.Snapshot.Rows = nil
	return compile(owned)
}

// CompileOwnedToDirectory is the bounded-memory production publication path.
// It transfers ownership of input rows, streams COLD and sidecar bytes into a
// private staging directory, reopens every artifact for independent
// verification, writes the activation manifest last, and publishes with an
// atomic no-replace directory rename. The artifact root must be owned by the
// dedicated builder euid and not group/world writable. A process running as
// that same uid is inside the builder-integrity trust boundary; protecting a
// compromised peer with identical filesystem authority is out of scope.
func CompileOwnedToDirectory(input *CompilerInput, baseDirectory string, limits PublicationLimits) (WrittenBundle, error) {
	if input == nil {
		return WrittenBundle{}, errors.New("snapshot compiler input is required")
	}
	owned := *input
	input.Snapshot.Rows = nil
	return compileOwnedToDirectory(owned, baseDirectory, limits, nil)
}

type publishFault func(stage string) error

func compileOwnedToDirectory(input CompilerInput, baseDirectory string, limits PublicationLimits,
	inject publishFault) (result WrittenBundle, resultErr error) {
	if strings.TrimSpace(baseDirectory) == "" {
		return WrittenBundle{}, errors.New("snapshot artifact base directory is required")
	}
	if limits.MaxBytes <= 0 || uint64(limits.MaxBytes) > maxPublishedBytes || limits.MaxHotBytes <= 0 ||
		uint64(limits.MaxHotBytes) > maxHotPublishedBytes {
		return WrittenBundle{}, errors.New("snapshot publication limits are invalid")
	}
	spec, keyFields, rowsByEntity, err := prepareCompilerInput(input)
	if err != nil {
		return WrittenBundle{}, err
	}
	if err := os.MkdirAll(baseDirectory, 0o750); err != nil {
		return WrittenBundle{}, fmt.Errorf("create snapshot artifact base directory: %w", err)
	}
	base, err := openRegularDirectory(baseDirectory)
	if err != nil {
		return WrittenBundle{}, fmt.Errorf("open snapshot artifact base directory: %w", err)
	}
	defer base.Close()
	finalDirectory := filepath.Join(baseDirectory, input.PublicationName)
	if _, err := os.Lstat(finalDirectory); err == nil {
		if !limits.AllowExistingIdentical {
			return WrittenBundle{}, fmt.Errorf("snapshot publication directory already exists: %s", finalDirectory)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return WrittenBundle{}, err
	}

	artifact, err := ordinal.CompileSnapshotArtifact(spec)
	if err != nil {
		return WrittenBundle{}, fmt.Errorf("compile ordinal snapshot: %w", err)
	}
	// rowsByEntity contains only sidecar key values. Release all other scanned
	// values before writing or reparsing any complete artifact.
	spec.Rows = nil
	input.Snapshot.Rows = nil
	runtime.GC()
	manifest := artifact.Hot.Manifest()
	manifestDigest := artifact.Hot.ManifestDigest()
	if err := validateExpectedDigests(input.ExpectedDigests, manifest, manifestDigest); err != nil {
		return WrittenBundle{}, err
	}

	temporary, err := os.MkdirTemp(baseDirectory, "."+input.PublicationName+".tmp-")
	if err != nil {
		return WrittenBundle{}, err
	}
	cleanupTemporary := true
	defer func() {
		if cleanupTemporary {
			if cleanupErr := os.RemoveAll(temporary); cleanupErr != nil {
				result = WrittenBundle{}
				resultErr = errors.Join(resultErr, fmt.Errorf("clean staged publication: %w", cleanupErr))
			}
		}
	}()
	staging, err := openRegularDirectory(temporary)
	if err != nil {
		return WrittenBundle{}, fmt.Errorf("open staged publication directory: %w", err)
	}
	defer staging.Close()
	if err := verifyDirectoryEntry(base, staging, filepath.Base(temporary)); err != nil {
		return WrittenBundle{}, fmt.Errorf("bind staged publication to base directory: %w", err)
	}

	hotName := input.PublicationName + ".hot.tgord"
	coldName := input.PublicationName + ".cold.tgord"
	sidecarName := input.PublicationName + ".sidecar.ndjson"
	hotBytes, err := artifact.Hot.MarshalBinary()
	if err != nil {
		return WrittenBundle{}, fmt.Errorf("encode HOT artifact: %w", err)
	}
	hotLimit := limits.MaxHotBytes
	if limits.MaxBytes < hotLimit {
		hotLimit = limits.MaxBytes
	}
	hotDescriptor, err := writeStreamedFile(staging, hotName, hotLimit, func(writer io.Writer) error {
		_, writeErr := writeFull(writer, hotBytes)
		return writeErr
	})
	if err != nil {
		return WrittenBundle{}, fmt.Errorf("write HOT artifact: %w", err)
	}
	if err := runPublishFault(inject, "hot"); err != nil {
		return WrittenBundle{}, err
	}
	// The encoded file is now durable in staging. Drop the compiler HOT graph,
	// then reopen and parse the file so verification covers the actual bytes.
	artifact.Hot = nil
	hotBytes = nil
	runtime.GC()
	verifiedHotBytes, err := readVerifiedArtifact(staging, hotName, hotDescriptor)
	if err != nil {
		return WrittenBundle{}, fmt.Errorf("reopen HOT artifact: %w", err)
	}
	verifiedHot, err := ordinal.ParseHotDictionary(verifiedHotBytes, manifestDigest)
	if err != nil {
		return WrittenBundle{}, fmt.Errorf("verify encoded HOT artifact: %w", err)
	}
	verifiedHotBytes = nil
	runtime.GC()

	coldLimit, err := remainingPublishedBytes(limits.MaxBytes, hotDescriptor.Bytes)
	if err != nil {
		return WrittenBundle{}, err
	}
	coldDescriptor, err := writeStreamedFile(staging, coldName, coldLimit, func(writer io.Writer) error {
		_, writeErr := artifact.Cold.WriteBinary(writer)
		return writeErr
	})
	if err != nil {
		return WrittenBundle{}, fmt.Errorf("write COLD artifact: %w", err)
	}
	if err := runPublishFault(inject, "cold"); err != nil {
		return WrittenBundle{}, err
	}
	// Verification streams from a separately opened file after the compiler
	// graph is released; no artifact-sized byte slice is retained.
	artifact.Cold = nil
	runtime.GC()
	if err := verifyColdArtifactFile(staging, coldName, coldDescriptor, manifestDigest); err != nil {
		return WrittenBundle{}, fmt.Errorf("verify encoded COLD artifact: %w", err)
	}

	sidecarLimit, err := remainingPublishedBytes(limits.MaxBytes, hotDescriptor.Bytes, coldDescriptor.Bytes)
	if err != nil {
		return WrittenBundle{}, err
	}
	sidecarDescriptor, err := writeStreamedFile(staging, sidecarName, sidecarLimit, func(writer io.Writer) error {
		return writeSidecar(writer, input, keyFields, rowsByEntity, verifiedHot)
	})
	if err != nil {
		return WrittenBundle{}, fmt.Errorf("write sidecar export: %w", err)
	}
	if err := runPublishFault(inject, "sidecar"); err != nil {
		return WrittenBundle{}, err
	}
	rowsByEntity = nil
	runtime.GC()
	expectation := SidecarExpectation{PublicationName: input.PublicationName, OrdinalSidecar: input.OrdinalSidecar,
		SourceNamespace: spec.SourceNamespace, ManifestDigest: manifestDigest, SidecarDigest: manifest.SidecarDigest}
	if err := verifySidecarArtifactFile(staging, sidecarName, sidecarDescriptor, verifiedHot, expectation); err != nil {
		return WrittenBundle{}, fmt.Errorf("verify sidecar export: %w", err)
	}

	bundleManifest := BundleManifest{Version: BundleVersion, PublicationName: input.PublicationName,
		CatalogSource: input.CatalogSource, OrdinalSidecar: input.OrdinalSidecar, ManifestDigest: manifestDigest,
		DictionaryManifest: manifest, RowCount: verifiedHot.RowCount(), Hot: hotDescriptor,
		Cold: coldDescriptor, Sidecar: sidecarDescriptor}
	if err := bundleManifest.Validate(); err != nil {
		return WrittenBundle{}, err
	}
	manifestJSON, err := manifestJSON(bundleManifest)
	if err != nil {
		return WrittenBundle{}, err
	}
	totalBytes, err := sumPublishedBytes(limits.MaxBytes, hotDescriptor.Bytes, coldDescriptor.Bytes,
		sidecarDescriptor.Bytes, int64(len(manifestJSON)))
	if err != nil {
		return WrittenBundle{}, err
	}
	manifestName := input.PublicationName + ".bundle.json"
	manifestLimit, err := remainingPublishedBytes(limits.MaxBytes, hotDescriptor.Bytes, coldDescriptor.Bytes,
		sidecarDescriptor.Bytes)
	if err != nil {
		return WrittenBundle{}, err
	}
	manifestDescriptor, err := writeStreamedFile(staging, manifestName, manifestLimit, func(writer io.Writer) error {
		_, writeErr := writeFull(writer, manifestJSON)
		return writeErr
	})
	if err != nil {
		return WrittenBundle{}, fmt.Errorf("write bundle activation manifest: %w", err)
	}
	if err := runPublishFault(inject, "manifest"); err != nil {
		return WrittenBundle{}, err
	}
	verifiedManifestJSON, err := readVerifiedArtifact(staging, manifestName, manifestDescriptor)
	if err != nil {
		return WrittenBundle{}, fmt.Errorf("reopen bundle activation manifest: %w", err)
	}
	verifiedManifest, err := DecodeBundleManifest(bytes.NewReader(verifiedManifestJSON))
	if err != nil || !reflect.DeepEqual(verifiedManifest, bundleManifest) {
		return WrittenBundle{}, fmt.Errorf("verify bundle activation manifest: %w", errors.Join(err, errors.New("manifest differs from compiled bundle")))
	}
	expectedFiles := map[string]FileDescriptor{
		hotName: hotDescriptor, coldName: coldDescriptor, sidecarName: sidecarDescriptor,
		manifestName: manifestDescriptor,
	}
	if err := verifyStagedPublication(staging, expectedFiles); err != nil {
		return WrittenBundle{}, fmt.Errorf("verify staged publication directory: %w", err)
	}
	if err := staging.Sync(); err != nil {
		return WrittenBundle{}, fmt.Errorf("sync staged publication directory: %w", err)
	}
	if err := verifyOpenDirectoryIdentity(baseDirectory, base); err != nil {
		return WrittenBundle{}, fmt.Errorf("publication base identity before commit: %w", err)
	}
	if err := verifyDirectoryEntry(base, staging, filepath.Base(temporary)); err != nil {
		return WrittenBundle{}, fmt.Errorf("staged publication identity before commit: %w", err)
	}
	if err := renameDirectoryNoReplace(base, filepath.Base(temporary), input.PublicationName); err != nil {
		if !limits.AllowExistingIdentical || !errors.Is(err, os.ErrExist) {
			return WrittenBundle{}, fmt.Errorf("publish snapshot artifact directory: %w", err)
		}
		if err := verifyExistingStreamedPublication(base, input.PublicationName, expectedFiles,
			bundleManifest, manifestDescriptor); err != nil {
			return WrittenBundle{}, fmt.Errorf("existing snapshot publication is not identical: %w", err)
		}
		if err := verifyOpenDirectoryIdentity(baseDirectory, base); err != nil {
			return WrittenBundle{}, fmt.Errorf("publication base identity after identical verification: %w", err)
		}
		return WrittenBundle{Manifest: bundleManifest, Directory: finalDirectory, Bytes: totalBytes}, nil
	}
	cleanupTemporary = false
	rollbackCommit := func(cause error) (WrittenBundle, error) {
		rollbackErr := renameDirectoryNoReplace(base, input.PublicationName, filepath.Base(temporary))
		if rollbackErr == nil {
			cleanupTemporary = true
			return WrittenBundle{}, errors.Join(cause, base.Sync())
		}
		disableErr := disablePublishedDirectory(base, staging, input.PublicationName, manifestName)
		return WrittenBundle{}, errors.Join(cause,
			fmt.Errorf("rollback committed publication: %w", rollbackErr), disableErr)
	}
	if err := verifyPublishedDirectory(base, staging, input.PublicationName); err != nil {
		return rollbackCommit(fmt.Errorf("published directory identity: %w", err))
	}
	if err := base.Sync(); err != nil {
		return rollbackCommit(fmt.Errorf("sync published snapshot directory: %w", err))
	}
	if err := verifyOpenDirectoryIdentity(baseDirectory, base); err != nil {
		return rollbackCommit(fmt.Errorf("publication base identity after commit: %w", err))
	}
	return WrittenBundle{Manifest: bundleManifest, Directory: finalDirectory, Bytes: totalBytes}, nil
}

func compile(input CompilerInput) (CompiledBundle, error) {
	spec, keyFields, rowsByEntity, err := prepareCompilerInput(input)
	if err != nil {
		return CompiledBundle{}, err
	}
	artifact, err := ordinal.CompileSnapshotArtifact(spec)
	if err != nil {
		return CompiledBundle{}, fmt.Errorf("compile ordinal snapshot: %w", err)
	}
	// rowsByEntity retains only the entity-key values needed by the sidecar.
	// Everything else from the PostgreSQL scan can be reclaimed before the
	// roughly gigabyte-scale artifact buffers are allocated.
	spec.Rows = nil
	input.Snapshot.Rows = nil
	runtime.GC()
	manifest := artifact.Hot.Manifest()
	manifestDigest := artifact.Hot.ManifestDigest()
	if err := validateExpectedDigests(input.ExpectedDigests, manifest, manifestDigest); err != nil {
		return CompiledBundle{}, err
	}
	hotBytes, err := artifact.Hot.MarshalBinary()
	if err != nil {
		return CompiledBundle{}, fmt.Errorf("encode HOT artifact: %w", err)
	}
	coldBytes, err := artifact.Cold.MarshalBinary()
	if err != nil {
		return CompiledBundle{}, fmt.Errorf("encode COLD artifact: %w", err)
	}
	verifiedHot, err := ordinal.ParseHotDictionary(hotBytes, manifestDigest)
	if err != nil {
		return CompiledBundle{}, fmt.Errorf("verify encoded HOT artifact: %w", err)
	}
	if err := ordinal.VerifyColdDictionary(coldBytes, manifestDigest); err != nil {
		return CompiledBundle{}, fmt.Errorf("verify encoded COLD artifact: %w", err)
	}

	sidecarBytes, err := marshalSidecar(input, keyFields, rowsByEntity, verifiedHot)
	if err != nil {
		return CompiledBundle{}, err
	}
	artifactBytes := uint64(len(hotBytes)) + uint64(len(coldBytes)) + uint64(len(sidecarBytes))
	if artifactBytes > maxPublishedBytes {
		return CompiledBundle{}, fmt.Errorf("snapshot publication artifacts use %d bytes; limit is %d", artifactBytes, maxPublishedBytes)
	}
	expectation := SidecarExpectation{PublicationName: input.PublicationName, OrdinalSidecar: input.OrdinalSidecar,
		SourceNamespace: spec.SourceNamespace, ManifestDigest: manifestDigest, SidecarDigest: manifest.SidecarDigest}
	if err := VerifySidecarNDJSON(bytes.NewReader(sidecarBytes), verifiedHot, expectation); err != nil {
		return CompiledBundle{}, fmt.Errorf("verify sidecar export: %w", err)
	}

	hotName := input.PublicationName + ".hot.tgord"
	coldName := input.PublicationName + ".cold.tgord"
	sidecarName := input.PublicationName + ".sidecar.ndjson"
	bundle := CompiledBundle{Hot: hotBytes, Cold: coldBytes, Sidecar: sidecarBytes}
	bundle.Manifest = BundleManifest{Version: BundleVersion, PublicationName: input.PublicationName,
		CatalogSource: input.CatalogSource, OrdinalSidecar: input.OrdinalSidecar, ManifestDigest: manifestDigest,
		DictionaryManifest: manifest, RowCount: verifiedHot.RowCount(),
		Hot: fileDescriptor(hotName, hotBytes), Cold: fileDescriptor(coldName, coldBytes),
		Sidecar: fileDescriptor(sidecarName, sidecarBytes)}
	if err := bundle.Manifest.Validate(); err != nil {
		return CompiledBundle{}, err
	}
	manifestJSON, err := bundle.ManifestJSON()
	if err != nil {
		return CompiledBundle{}, err
	}
	// The published-size SLO covers the complete directory, including its
	// activation marker, rather than only the three data artifacts.
	totalPublishedBytes := artifactBytes + uint64(len(manifestJSON))
	if totalPublishedBytes < artifactBytes || totalPublishedBytes > maxPublishedBytes {
		return CompiledBundle{}, fmt.Errorf("snapshot publication uses %d bytes; limit is %d", totalPublishedBytes, maxPublishedBytes)
	}
	return bundle, nil
}

func (m BundleManifest) Validate() error {
	if m.Version != BundleVersion || !configNamePattern.MatchString(m.PublicationName) ||
		!identifierPattern.MatchString(m.CatalogSource) || !sidecarNamePattern.MatchString(m.OrdinalSidecar) ||
		!digestPattern.MatchString(m.ManifestDigest) || m.RowCount == 0 {
		return errors.New("invalid snapshot bundle identity")
	}
	if err := m.DictionaryManifest.Validate(); err != nil {
		return fmt.Errorf("invalid bundled dictionary manifest: %w", err)
	}
	digest, err := m.DictionaryManifest.Digest()
	if err != nil || digest != m.ManifestDigest {
		return errors.New("bundled dictionary manifest digest mismatch")
	}
	expectedNames := map[string]string{
		"HOT": m.PublicationName + ".hot.tgord", "COLD": m.PublicationName + ".cold.tgord",
		"SIDECAR": m.PublicationName + ".sidecar.ndjson",
	}
	for kind, descriptor := range map[string]FileDescriptor{"HOT": m.Hot, "COLD": m.Cold, "SIDECAR": m.Sidecar} {
		if err := descriptor.validate(expectedNames[kind]); err != nil {
			return fmt.Errorf("invalid %s descriptor: %w", kind, err)
		}
	}
	return nil
}

func (f FileDescriptor) validate(expectedName string) error {
	if f.Name != expectedName || filepath.Base(f.Name) != f.Name || !digestPattern.MatchString(f.SHA256) || f.Bytes <= 0 {
		return errors.New("non-canonical artifact file descriptor")
	}
	return nil
}

// ManifestJSON returns the deterministic publication manifest written last by
// Write. No timestamps are included, so identical snapshot input is bytewise
// reproducible.
func (b CompiledBundle) ManifestJSON() ([]byte, error) {
	if err := b.Manifest.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(b.Manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// Write atomically publishes a new <base>/<publication> directory. Existing
// publications are never overwritten; rebuilding requires a new publication
// name or an explicit operator-managed removal.
func (b CompiledBundle) Write(baseDirectory string) (string, error) {
	return b.write(baseDirectory, false)
}

// WriteIdempotent has the same immutable publication semantics as Write, but
// treats an already-published, byte-identical bundle as success. It exists for
// declarative deployment systems that rerun one-shot compiler containers on
// every start. Any missing, additional, non-regular, or different artifact is
// rejected; this method never repairs or overwrites an existing publication.
func (b CompiledBundle) WriteIdempotent(baseDirectory string) (string, error) {
	return b.write(baseDirectory, true)
}

func (b CompiledBundle) write(baseDirectory string, allowIdentical bool) (string, error) {
	if strings.TrimSpace(baseDirectory) == "" {
		return "", errors.New("snapshot artifact base directory is required")
	}
	manifestJSON, err := b.ManifestJSON()
	if err != nil {
		return "", err
	}
	if err := verifyPayloadDescriptor(b.Manifest.Hot, b.Hot); err != nil {
		return "", err
	}
	if err := verifyPayloadDescriptor(b.Manifest.Cold, b.Cold); err != nil {
		return "", err
	}
	if err := verifyPayloadDescriptor(b.Manifest.Sidecar, b.Sidecar); err != nil {
		return "", err
	}
	if err := os.MkdirAll(baseDirectory, 0o750); err != nil {
		return "", fmt.Errorf("create snapshot artifact base directory: %w", err)
	}
	finalDirectory := filepath.Join(baseDirectory, b.Manifest.PublicationName)
	if _, err := os.Lstat(finalDirectory); err == nil {
		if allowIdentical {
			if err := verifyExistingPublication(finalDirectory, filesForBundle(b, manifestJSON)); err != nil {
				return "", fmt.Errorf("existing snapshot publication is not identical: %w", err)
			}
			return finalDirectory, nil
		}
		return "", fmt.Errorf("snapshot publication directory already exists: %s", finalDirectory)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	temporary, err := os.MkdirTemp(baseDirectory, "."+b.Manifest.PublicationName+".tmp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	files := filesForBundle(b, manifestJSON)
	for _, file := range files {
		if err := writeNewFile(filepath.Join(temporary, file.name), file.data); err != nil {
			return "", err
		}
	}
	if err := os.Rename(temporary, finalDirectory); err != nil {
		if allowIdentical {
			if verifyErr := verifyExistingPublication(finalDirectory, files); verifyErr == nil {
				return finalDirectory, nil
			}
		}
		return "", fmt.Errorf("publish snapshot artifact directory: %w", err)
	}
	return finalDirectory, nil
}

type bundleFile struct {
	name string
	data []byte
}

func filesForBundle(bundle CompiledBundle, manifestJSON []byte) []bundleFile {
	return []bundleFile{
		{bundle.Manifest.Hot.Name, bundle.Hot},
		{bundle.Manifest.Cold.Name, bundle.Cold},
		{bundle.Manifest.Sidecar.Name, bundle.Sidecar},
		// The bundle manifest is the activation marker and is always written last.
		{bundle.Manifest.PublicationName + ".bundle.json", manifestJSON},
	}
}

func verifyExistingPublication(directory string, expected []bundleFile) error {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("publication path is not a regular directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("publication contains %d files; expected %d", len(entries), len(expected))
	}
	byName := make(map[string][]byte, len(expected))
	for _, file := range expected {
		byName[file.name] = file.data
	}
	for _, entry := range entries {
		payload, found := byName[entry.Name()]
		if !found || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected or non-regular artifact %q", entry.Name())
		}
		entryInfo, err := entry.Info()
		if err != nil || !entryInfo.Mode().IsRegular() {
			return fmt.Errorf("artifact %q is not a regular file", entry.Name())
		}
		if entryInfo.Size() != int64(len(payload)) {
			return fmt.Errorf("artifact %q size differs", entry.Name())
		}
		file, err := os.Open(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return fmt.Errorf("read artifact %q: %v", entry.Name(), errors.Join(copyErr, closeErr))
		}
		expectedHash := sha256.Sum256(payload)
		if !bytes.Equal(hash.Sum(nil), expectedHash[:]) {
			return fmt.Errorf("artifact %q digest differs", entry.Name())
		}
	}
	return nil
}

func VerifySidecarNDJSON(reader io.Reader, index ordinal.SnapshotIndex, expected SidecarExpectation) error {
	if index == nil || !configNamePattern.MatchString(expected.PublicationName) ||
		!sidecarNamePattern.MatchString(expected.OrdinalSidecar) || !digestPattern.MatchString(expected.ManifestDigest) ||
		!digestPattern.MatchString(expected.SidecarDigest) || expected.ManifestDigest != index.ManifestDigest() ||
		expected.SidecarDigest != index.Manifest().SidecarDigest {
		return errors.New("invalid sidecar verification binding")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxSidecarLineBytes)
	var header SidecarHeader
	var rows uint64
	footerSeen := false
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := append([]byte(nil), scanner.Bytes()...)
		if len(line) == 0 || footerSeen {
			return fmt.Errorf("non-canonical sidecar line %d", lineNumber)
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			return fmt.Errorf("decode sidecar line %d: %w", lineNumber, err)
		}
		switch envelope.Type {
		case "header":
			if lineNumber != 1 || decodeCanonicalLine(line, &header) != nil || header.Type != "header" ||
				header.Version != SidecarVersion || header.PublicationName != expected.PublicationName ||
				header.OrdinalSidecar != expected.OrdinalSidecar || header.SourceNamespace != expected.SourceNamespace ||
				header.ManifestDigest != expected.ManifestDigest || header.SidecarDigest != expected.SidecarDigest ||
				len(header.EntityKeyFields) == 0 {
				return errors.New("sidecar header does not match its publication")
			}
			seen := make(map[string]struct{}, len(header.EntityKeyFields))
			for fieldIndex := range header.EntityKeyFields {
				field := &header.EntityKeyFields[fieldIndex]
				canonical, err := exposure.CanonicalSQLTypeV2(field.SQLType)
				if err != nil || !identifierPattern.MatchString(field.Name) {
					return errors.New("sidecar header has an invalid entity key field")
				}
				if _, duplicate := seen[field.Name]; duplicate {
					return errors.New("sidecar header has duplicate entity key fields")
				}
				seen[field.Name] = struct{}{}
				field.SQLType = canonical
			}
		case "row":
			if lineNumber == 1 || footerSeen {
				return errors.New("sidecar row appears outside header/footer")
			}
			var row SidecarRow
			if err := decodeCanonicalLine(line, &row); err != nil {
				return fmt.Errorf("decode canonical sidecar row %d: %w", rows+1, err)
			}
			rows++
			if row.RowHandle != rows || len(row.KeyValues) != len(header.EntityKeyFields) {
				return errors.New("sidecar row handles or key values are not dense")
			}
			components := []string{expected.SourceNamespace}
			for fieldIndex, field := range header.EntityKeyFields {
				value, err := normalizeJSONValue(field.SQLType, row.KeyValues[fieldIndex])
				if err != nil {
					return fmt.Errorf("sidecar row %d key value: %w", rows, err)
				}
				canonical, err := exposure.CanonicalSQLValue(field.SQLType, value)
				if err != nil {
					return fmt.Errorf("sidecar row %d canonical key: %w", rows, err)
				}
				components = append(components, field.Name, field.SQLType, canonical)
			}
			entityKey, err := exposure.ComposeCanonicalKeyV2("base-entity", components...)
			indexedEntity, found := sidecarRowIdentity(index, ordinal.RowHandle(rows))
			if err != nil || !found || row.EntityKey != entityKey || indexedEntity != entityKey {
				return fmt.Errorf("sidecar row %d does not match the HOT index", rows)
			}
		case "footer":
			var footer SidecarFooter
			if err := decodeCanonicalLine(line, &footer); err != nil || footer.Type != "footer" ||
				footer.RowCount != rows || footer.RowCount != index.RowCount() || footer.SidecarDigest != expected.SidecarDigest {
				return errors.New("sidecar footer does not match the HOT index")
			}
			footerSeen = true
		default:
			return fmt.Errorf("unknown sidecar record type on line %d", lineNumber)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan sidecar export: %w", err)
	}
	if !footerSeen || rows == 0 {
		return errors.New("sidecar export is incomplete")
	}
	return nil
}

func prepareCompilerInput(input CompilerInput) (ordinal.SnapshotSpec, []SidecarKeyField, map[string][]any, error) {
	if input.Version != CompilerInputVersion || !configNamePattern.MatchString(input.PublicationName) ||
		!identifierPattern.MatchString(input.CatalogSource) || !sidecarNamePattern.MatchString(input.OrdinalSidecar) ||
		(input.SourceRelation != "" && !sourceRelationPattern.MatchString(input.SourceRelation)) ||
		!configNamePattern.MatchString(input.Snapshot.SourceID) || strings.TrimSpace(input.Snapshot.SourceNamespace) == "" ||
		strings.ContainsAny(input.Snapshot.SourceNamespace, "\x00\r\n\t") || !versionPattern.MatchString(input.Snapshot.Snapshot) ||
		!digestPattern.MatchString(input.Snapshot.SchemaDigest) || len(input.Snapshot.Fields) == 0 ||
		len(input.Snapshot.Rows) == 0 || len(input.EntityKeyFields) == 0 {
		return ordinal.SnapshotSpec{}, nil, nil, errors.New("snapshot compiler input identity is incomplete")
	}
	fields, _, fieldTypes, err := prepareSnapshotFields(input.Snapshot.Fields)
	if err != nil {
		return ordinal.SnapshotSpec{}, nil, nil, err
	}
	keyFields, err := prepareSidecarKeyFields(input.EntityKeyFields, fieldTypes)
	if err != nil {
		return ordinal.SnapshotSpec{}, nil, nil, err
	}
	rows := make([]ordinal.SnapshotRow, 0, len(input.Snapshot.Rows))
	rowsByEntity := make(map[string][]any, len(input.Snapshot.Rows))
	for rowIndex, source := range input.Snapshot.Rows {
		if len(source.Values) != len(fieldTypes) {
			return ordinal.SnapshotSpec{}, nil, nil, fmt.Errorf("snapshot row %d field set differs from schema", rowIndex)
		}
		// Most decoded SQL values are already in their canonical Go carrier
		// type. Reuse that immutable row map and copy lazily only for bytea,
		// whose explicit base64 transport form must be decoded.
		values := source.Values
		valuesCopied := false
		for field, sqlType := range fieldTypes {
			raw, found := source.Values[field]
			if !found {
				return ordinal.SnapshotSpec{}, nil, nil, fmt.Errorf("snapshot row %d misses field %q", rowIndex, field)
			}
			value, err := normalizeJSONValue(sqlType, raw)
			if err != nil {
				return ordinal.SnapshotSpec{}, nil, nil, fmt.Errorf("snapshot row %d field %q: %w", rowIndex, field, err)
			}
			if _, err := exposure.CanonicalSQLValue(sqlType, value); err != nil {
				return ordinal.SnapshotSpec{}, nil, nil, fmt.Errorf("snapshot row %d field %q: %w", rowIndex, field, err)
			}
			if sqlType == "bytea" {
				if !valuesCopied {
					values = make(map[string]any, len(source.Values))
					for name, original := range source.Values {
						values[name] = original
					}
					valuesCopied = true
				}
				values[field] = value
			}
		}
		components := []string{input.Snapshot.SourceNamespace}
		for _, field := range keyFields {
			canonical, _ := exposure.CanonicalSQLValue(field.SQLType, values[field.Name])
			components = append(components, field.Name, field.SQLType, canonical)
		}
		entityKey, err := exposure.ComposeCanonicalKeyV2("base-entity", components...)
		if err != nil || (source.EntityKey != "" && source.EntityKey != entityKey) {
			return ordinal.SnapshotSpec{}, nil, nil, fmt.Errorf("snapshot row %d entity key assertion failed", rowIndex)
		}
		if _, duplicate := rowsByEntity[entityKey]; duplicate {
			return ordinal.SnapshotSpec{}, nil, nil, fmt.Errorf("duplicate canonical entity key in row %d", rowIndex)
		}
		keyValues := make([]any, len(keyFields))
		for index, field := range keyFields {
			keyValues[index] = exportJSONValue(field.SQLType, values[field.Name])
		}
		rowsByEntity[entityKey] = keyValues
		rows = append(rows, ordinal.SnapshotRow{EntityKey: entityKey, Values: values})
	}
	return ordinal.SnapshotSpec{SourceID: input.Snapshot.SourceID, SourceNamespace: input.Snapshot.SourceNamespace,
		Snapshot: input.Snapshot.Snapshot, SchemaDigest: input.Snapshot.SchemaDigest, Fields: fields, Rows: rows}, keyFields, rowsByEntity, nil
}

type physicalSnapshotField struct {
	Name             string
	SQLType          string
	Collation        string
	CollationVersion string
}

// prepareSnapshotFields is shared by the offline database scanner and the
// compiler so a physical column cannot be scanned under weaker type or
// collation rules than the rules that define its canonical facts.
func prepareSnapshotFields(source []SnapshotField) ([]ordinal.SnapshotField, []physicalSnapshotField, map[string]string, error) {
	fields := make([]ordinal.SnapshotField, 0, len(source))
	physical := make([]physicalSnapshotField, 0, len(source))
	fieldTypes := make(map[string]string, len(source))
	fieldCollations := make(map[string][2]string, len(source))
	seenCanonical := make(map[string]struct{}, len(source))
	for _, field := range source {
		canonicalType, err := exposure.CanonicalSQLTypeV2(field.SQLType)
		canonicalID := field.CanonicalFieldID
		if canonicalID == "" {
			canonicalID = field.Name
		}
		if err != nil || !identifierPattern.MatchString(field.Name) || strings.TrimSpace(canonicalID) == "" ||
			strings.ContainsAny(canonicalID, "\x00\r\n\t") {
			return nil, nil, nil, fmt.Errorf("invalid snapshot field %q", field.Name)
		}
		collatable := canonicalType == "text" || canonicalType == "character" || canonicalType == "character varying"
		if (collatable && (field.Collation == "" || field.CollationVersion == "")) ||
			(!collatable && (field.Collation != "" || field.CollationVersion != "")) ||
			strings.TrimSpace(field.Collation) != field.Collation || strings.TrimSpace(field.CollationVersion) != field.CollationVersion {
			return nil, nil, nil, fmt.Errorf("field %q has incomplete or inapplicable collation metadata", field.Name)
		}
		if existingType, duplicate := fieldTypes[field.Name]; duplicate {
			if existingType != canonicalType || fieldCollations[field.Name] != ([2]string{field.Collation, field.CollationVersion}) {
				return nil, nil, nil, fmt.Errorf("snapshot field %q has conflicting physical semantics", field.Name)
			}
		} else {
			fieldTypes[field.Name] = canonicalType
			fieldCollations[field.Name] = [2]string{field.Collation, field.CollationVersion}
			physical = append(physical, physicalSnapshotField{Name: field.Name, SQLType: canonicalType,
				Collation: field.Collation, CollationVersion: field.CollationVersion})
		}
		if _, duplicate := seenCanonical[canonicalID]; duplicate {
			return nil, nil, nil, fmt.Errorf("duplicate canonical snapshot field %q", canonicalID)
		}
		seenCanonical[canonicalID] = struct{}{}
		fields = append(fields, ordinal.SnapshotField{Name: field.Name, CanonicalFieldID: canonicalID, SQLType: canonicalType})
	}
	return fields, physical, fieldTypes, nil
}

func prepareSidecarKeyFields(names []string, fieldTypes map[string]string) ([]SidecarKeyField, error) {
	keyFields := make([]SidecarKeyField, 0, len(names))
	seenKeys := make(map[string]struct{}, len(names))
	for _, name := range names {
		sqlType, found := fieldTypes[name]
		if !found || !identifierPattern.MatchString(name) {
			return nil, fmt.Errorf("entity key field %q is absent", name)
		}
		if _, duplicate := seenKeys[name]; duplicate {
			return nil, fmt.Errorf("duplicate entity key field %q", name)
		}
		seenKeys[name] = struct{}{}
		keyFields = append(keyFields, SidecarKeyField{Name: name, SQLType: sqlType})
	}
	return keyFields, nil
}

func validateExpectedDigests(expected ExpectedDigests, manifest ordinal.DictionaryManifest, manifestDigest string) error {
	values := map[string][2]string{
		"sidecar": {expected.SidecarDigest, manifest.SidecarDigest}, "dictionary": {expected.DictionaryDigest, manifest.DictionaryDigest},
		"manifest": {expected.ManifestDigest, manifestDigest}, "cold payload": {expected.ColdPayloadDigest, manifest.ColdPayloadDigest},
		"hot index": {expected.HotIndexDigest, manifest.HotIndexDigest},
	}
	for name, pair := range values {
		if pair[0] != "" && (!digestPattern.MatchString(pair[0]) || pair[0] != pair[1]) {
			return fmt.Errorf("expected %s digest does not match compiled snapshot", name)
		}
	}
	return nil
}

func marshalSidecar(input CompilerInput, keyFields []SidecarKeyField, rows map[string][]any, index ordinal.SnapshotIndex) ([]byte, error) {
	var output bytes.Buffer
	if err := writeSidecar(&output, input, keyFields, rows, index); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func writeSidecar(writer io.Writer, input CompilerInput, keyFields []SidecarKeyField,
	rows map[string][]any, index ordinal.SnapshotIndex) error {
	header := SidecarHeader{Type: "header", Version: SidecarVersion, PublicationName: input.PublicationName,
		OrdinalSidecar: input.OrdinalSidecar, SourceNamespace: input.Snapshot.SourceNamespace,
		ManifestDigest: index.ManifestDigest(), SidecarDigest: index.Manifest().SidecarDigest,
		EntityKeyFields: append([]SidecarKeyField(nil), keyFields...)}
	if err := writeJSONLine(writer, header); err != nil {
		return err
	}
	for handle := uint64(1); handle <= index.RowCount(); handle++ {
		entityKey, found := sidecarRowIdentity(index, ordinal.RowHandle(handle))
		keyValues, inputFound := rows[entityKey]
		if !found || !inputFound {
			return fmt.Errorf("compiled HOT index misses dense row handle %d", handle)
		}
		if len(keyValues) != len(keyFields) {
			return fmt.Errorf("compiled sidecar key width differs at row handle %d", handle)
		}
		if err := writeJSONLine(writer, SidecarRow{Type: "row", RowHandle: handle, EntityKey: entityKey, KeyValues: keyValues}); err != nil {
			return err
		}
	}
	if err := writeJSONLine(writer, SidecarFooter{Type: "footer", RowCount: index.RowCount(), SidecarDigest: index.Manifest().SidecarDigest}); err != nil {
		return err
	}
	return nil
}

func sidecarRowIdentity(index ordinal.SnapshotIndex, handle ordinal.RowHandle) (string, bool) {
	// Use an exact concrete-type allowlist. A decorator may acquire projected
	// methods through Go method promotion while overriding LookupRow for fault
	// injection or validation; a bare interface assertion would bypass it.
	switch projected := index.(type) {
	case *ordinal.HotDictionary:
		entityKey, _, found := projected.LookupRowIdentity(handle)
		return entityKey, found
	case *ordinal.Dictionary:
		entityKey, _, found := projected.LookupRowIdentity(handle)
		return entityKey, found
	}
	indexed, found := index.LookupRow(handle)
	if !found || indexed.Handle != handle {
		return "", false
	}
	return indexed.EntityKey, true
}

func normalizeJSONValue(sqlType string, value any) (any, error) {
	canonical, err := exposure.CanonicalSQLTypeV2(sqlType)
	if err != nil {
		return nil, err
	}
	if value == nil || canonical != "bytea" {
		return value, nil
	}
	text, ok := value.(string)
	if !ok || !strings.HasPrefix(text, "base64:") {
		return nil, errors.New("bytea JSON values must use the base64:<data> form")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(text, "base64:"))
	if err != nil {
		return nil, errors.New("invalid base64 bytea value")
	}
	return decoded, nil
}

func exportJSONValue(sqlType string, value any) any {
	canonical, _ := exposure.CanonicalSQLTypeV2(sqlType)
	if canonical == "bytea" {
		if encoded, ok := value.([]byte); ok {
			return "base64:" + base64.StdEncoding.EncodeToString(encoded)
		}
	}
	return value
}

func writeJSONLine(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := writer.Write(encoded); err != nil {
		return err
	}
	_, err = writer.Write([]byte{'\n'})
	return err
}

func decodeCanonicalLine(line []byte, target any) error {
	if err := decodeStrictJSON(bytes.NewReader(line), target); err != nil {
		return err
	}
	canonical, err := json.Marshal(target)
	if err != nil || !bytes.Equal(canonical, line) {
		return errors.New("sidecar JSON line is not canonical")
	}
	return nil
}

func decodeStrictJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON document has trailing content")
	}
	return nil
}

func fileDescriptor(name string, payload []byte) FileDescriptor {
	digest := sha256.Sum256(payload)
	return FileDescriptor{Name: name, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(payload))}
}

func verifyPayloadDescriptor(descriptor FileDescriptor, payload []byte) error {
	actual := fileDescriptor(descriptor.Name, payload)
	if actual != descriptor {
		return fmt.Errorf("artifact payload does not match descriptor %q", descriptor.Name)
	}
	return nil
}

func writeNewFile(path string, payload []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func manifestJSON(manifest BundleManifest) ([]byte, error) {
	return (CompiledBundle{Manifest: manifest}).ManifestJSON()
}

func runPublishFault(inject publishFault, stage string) error {
	if inject == nil {
		return nil
	}
	if err := inject(stage); err != nil {
		return fmt.Errorf("injected publication failure after %s: %w", stage, err)
	}
	return nil
}

type countingWriter struct {
	writer  io.Writer
	written int64
	limit   int64
}

func (w *countingWriter) Write(payload []byte) (int, error) {
	if w.limit <= 0 || int64(len(payload)) > w.limit-w.written {
		return 0, fmt.Errorf("%w: %d bytes written, %d requested, limit %d",
			errPublishedSizeLimit, w.written, len(payload), w.limit)
	}
	written, err := w.writer.Write(payload)
	if written < 0 || written > len(payload) {
		return 0, io.ErrShortWrite
	}
	w.written += int64(written)
	return written, err
}

func writeFull(writer io.Writer, payload []byte) (int, error) {
	written := 0
	for written < len(payload) {
		count, err := writer.Write(payload[written:])
		if count < 0 || count > len(payload)-written {
			return written, io.ErrShortWrite
		}
		written += count
		if err != nil {
			return written, err
		}
		if count == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func writeStreamedFile(directory *os.File, name string, maximumBytes int64,
	emit func(io.Writer) error) (FileDescriptor, error) {
	if emit == nil || filepath.Base(name) != name || maximumBytes <= 0 {
		return FileDescriptor{}, errors.New("invalid streamed artifact writer")
	}
	file, err := createArtifactFile(directory, name)
	if err != nil {
		return FileDescriptor{}, err
	}
	digest := sha256.New()
	counter := &countingWriter{writer: io.MultiWriter(file, digest), limit: maximumBytes}
	emitErr := emit(counter)
	var syncErr error
	if emitErr == nil {
		syncErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(emitErr, syncErr, closeErr); err != nil {
		return FileDescriptor{}, err
	}
	if counter.written <= 0 {
		return FileDescriptor{}, errors.New("streamed artifact is empty")
	}
	return FileDescriptor{Name: name, SHA256: hex.EncodeToString(digest.Sum(nil)), Bytes: counter.written}, nil
}

func openRegularDirectory(path string) (*os.File, error) {
	directory, err := openDirectoryNoFollow(path)
	if err != nil {
		return nil, errors.New("publication base is not a regular directory")
	}
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		_ = directory.Close()
		return nil, errors.New("publication base is not a regular directory")
	}
	if err := validateDirectorySecurity(directory); err != nil {
		_ = directory.Close()
		return nil, fmt.Errorf("unsafe publication directory: %w", err)
	}
	return directory, nil
}

func verifyOpenDirectoryIdentity(path string, directory *os.File) error {
	if directory == nil {
		return errors.New("open directory is required")
	}
	actual, err := openDirectoryNoFollow(path)
	if err != nil {
		return errors.New("directory path is no longer a regular directory")
	}
	defer actual.Close()
	pathInfo, pathErr := actual.Stat()
	openInfo, openErr := directory.Stat()
	if pathErr != nil || openErr != nil || !pathInfo.IsDir() || !openInfo.IsDir() || !os.SameFile(pathInfo, openInfo) {
		return errors.New("directory path identity changed")
	}
	return nil
}

func validRelativeName(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\x00")
}

func openVerifiedArtifact(directory *os.File, name string, descriptor FileDescriptor) (*os.File, error) {
	if descriptor.Name != name || !validRelativeName(name) {
		return nil, errors.New("artifact descriptor name mismatch")
	}
	file, err := openArtifactFile(directory, name)
	if err != nil {
		return nil, errors.New("artifact is not the expected regular file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != descriptor.Bytes {
		_ = file.Close()
		return nil, errors.New("artifact is not the expected regular file")
	}
	if err := validateArtifactSecurity(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("unsafe artifact permissions: %w", err)
	}
	return file, nil
}

func readVerifiedArtifact(directory *os.File, name string, descriptor FileDescriptor) ([]byte, error) {
	if descriptor.Bytes <= 0 || uint64(descriptor.Bytes) > uint64(maxPublishedBytes) || uint64(descriptor.Bytes) > uint64(^uint(0)>>1) {
		return nil, errors.New("artifact size is outside the publication bound")
	}
	file, err := openVerifiedArtifact(directory, name, descriptor)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, int(descriptor.Bytes))
	_, readErr := io.ReadFull(file, payload)
	var extra [1]byte
	read, trailingErr := file.Read(extra[:])
	closeErr := file.Close()
	if readErr != nil || read != 0 || trailingErr != io.EOF || closeErr != nil {
		return nil, fmt.Errorf("read complete artifact: %v", errors.Join(readErr, trailingErr, closeErr))
	}
	if actual := fileDescriptor(descriptor.Name, payload); actual != descriptor {
		return nil, errors.New("artifact transport digest mismatch")
	}
	return payload, nil
}

func verifyColdArtifactFile(directory *os.File, name string, descriptor FileDescriptor, manifestDigest string) error {
	file, err := openVerifiedArtifact(directory, name, descriptor)
	if err != nil {
		return err
	}
	digest := sha256.New()
	verifyErr := ordinal.VerifyColdDictionaryReader(io.TeeReader(file, digest), descriptor.Bytes, manifestDigest)
	closeErr := file.Close()
	if err := errors.Join(verifyErr, closeErr); err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != descriptor.SHA256 {
		return errors.New("COLD artifact transport digest mismatch")
	}
	return nil
}

func verifySidecarArtifactFile(directory *os.File, name string, descriptor FileDescriptor, index ordinal.SnapshotIndex,
	expectation SidecarExpectation) error {
	file, err := openVerifiedArtifact(directory, name, descriptor)
	if err != nil {
		return err
	}
	digest := sha256.New()
	verifyErr := VerifySidecarNDJSON(io.TeeReader(file, digest), index, expectation)
	closeErr := file.Close()
	if err := errors.Join(verifyErr, closeErr); err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != descriptor.SHA256 {
		return errors.New("sidecar artifact transport digest mismatch")
	}
	return nil
}

func verifyStagedPublication(directory *os.File, expected map[string]FileDescriptor) error {
	entries, err := readDirectoryEntries(directory)
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("staged publication contains %d files; expected %d", len(entries), len(expected))
	}
	for _, entry := range entries {
		descriptor, found := expected[entry.Name()]
		if !found || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected or non-regular staged artifact %q", entry.Name())
		}
		if err := verifyArtifactTransport(directory, entry.Name(), descriptor); err != nil {
			return fmt.Errorf("staged artifact %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func verifyExistingStreamedPublication(base *os.File, publicationName string,
	expectedFiles map[string]FileDescriptor, expectedManifest BundleManifest,
	manifestDescriptor FileDescriptor) error {
	existing, err := openDirectoryAt(base, publicationName)
	if err != nil {
		return err
	}
	defer existing.Close()
	if err := validateDirectorySecurity(existing); err != nil {
		return fmt.Errorf("unsafe existing publication directory: %w", err)
	}
	if err := verifyStagedPublication(existing, expectedFiles); err != nil {
		return err
	}
	encoded, err := readVerifiedArtifact(existing, manifestDescriptor.Name, manifestDescriptor)
	if err != nil {
		return err
	}
	manifest, err := DecodeBundleManifest(bytes.NewReader(encoded))
	if err != nil || !reflect.DeepEqual(manifest, expectedManifest) {
		return errors.Join(err, errors.New("existing activation manifest differs"))
	}
	return nil
}

func verifyArtifactTransport(directory *os.File, name string, descriptor FileDescriptor) error {
	file, err := openVerifiedArtifact(directory, name, descriptor)
	if err != nil {
		return err
	}
	digest := sha256.New()
	buffer := make([]byte, 64<<10)
	written, copyErr := io.CopyBuffer(digest, file, buffer)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if written != descriptor.Bytes || hex.EncodeToString(digest.Sum(nil)) != descriptor.SHA256 {
		return errors.New("artifact transport descriptor mismatch")
	}
	return nil
}

func sumPublishedBytes(limit int64, values ...int64) (int64, error) {
	if limit <= 0 || uint64(limit) > maxPublishedBytes {
		return 0, errors.New("snapshot publication limit is invalid")
	}
	var total uint64
	for _, value := range values {
		if value < 0 || uint64(value) > ^uint64(0)-total {
			return 0, errors.New("snapshot publication size overflow")
		}
		total += uint64(value)
	}
	if total > uint64(limit) {
		return 0, fmt.Errorf("%w: publication uses %d bytes; remaining limit is %d",
			errPublishedSizeLimit, total, limit)
	}
	return int64(total), nil
}

func remainingPublishedBytes(limit int64, used ...int64) (int64, error) {
	total, err := sumPublishedBytes(limit, used...)
	if err != nil {
		return 0, err
	}
	remaining := limit - total
	if remaining <= 0 {
		return 0, errPublishedSizeLimit
	}
	return remaining, nil
}
