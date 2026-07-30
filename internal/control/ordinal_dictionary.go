package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/ordinal"
)

const (
	dictionarySegmentDigestDomain = "TASKGATE-V4-DICTIONARY-SEGMENT-V1\x00"
	v4CutoverAdvisoryLock         = int64(728194631046)
)

// OrdinalDictionaryChunk is one immutable cold dictionary artifact. SHA256 is
// the digest of Payload as stored (compressed when Compression is ZSTD).
type OrdinalDictionaryChunk struct {
	Index             int
	SHA256            string
	Compression       string
	Payload           []byte
	UncompressedBytes int64
	FirstOrdinal      uint64
	FactCount         uint64
}

// OrdinalDictionarySegment is one disjoint uint32 ordinal namespace.
type OrdinalDictionarySegment struct {
	ID           string
	FactKind     string
	FieldName    string
	OrdinalCount uint64
	Digest       string
	Chunks       []OrdinalDictionaryChunk
}

// OrdinalDictionaryManifest binds a frozen source snapshot to its exact FactID
// dictionary. ManifestJSON is retained as compiler/attestation evidence.
type OrdinalDictionaryManifest struct {
	Digest         string
	ManifestDigest string
	// PublicationDigest is the Catalog-approved publication manifest digest.
	// It must equal ManifestDigest; retaining the separately named column makes
	// the startup trust check explicit in durable evidence.
	PublicationDigest string
	DatasourceID      string
	SourceNamespace   string
	SnapshotID        string
	FactCount         uint64
	ManifestJSON      json.RawMessage
	Segments          []OrdinalDictionarySegment
	CreatedAt         time.Time
}

// OrdinalDictionarySet deliberately aliases the shared ordinal manifest so
// Gateway, Control PG, receipts and semantic-cache keys cannot drift onto
// different dictionary-set digest encodings.
type OrdinalDictionarySet = ordinal.DictionarySetManifest

type OrdinalArtifactChunk struct {
	Kind              string
	Index             int
	SHA256            string
	Compression       string
	Payload           []byte
	UncompressedBytes int64
}

// OrdinalDictionarySetDigest is a convenience wrapper around the one shared
// canonical implementation in internal/ordinal.
func OrdinalDictionarySetDigest(manifest ordinal.DictionarySetManifest) (string, error) {
	return manifest.Digest()
}

// ValidateV4Cutover reports whether a clean V4 activation is possible. It is
// read-only and never removes or rewrites legacy evidence.
func (s *Store) ValidateV4Cutover(ctx context.Context) error {
	const op = "validate V4 cutover"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	var legacy bool
	err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM exposure_ledgers)
    OR EXISTS (SELECT 1 FROM query_exposure_reservations)
    OR EXISTS (SELECT 1 FROM exposure_facts)
    OR EXISTS (SELECT 1 FROM query_exposure_facts)
    OR EXISTS (
        SELECT 1 FROM query_records q
        JOIN task_grants g ON g.task_id=q.task_id
        WHERE q.status='RESERVED'
          AND g.exposure_profile_version <> 'taskgate-exposure-v4'
    )`).Scan(&legacy)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	if legacy {
		return opErr(op, ErrConflict, fmt.Errorf("legacy exposure state or in-flight query exists; explicit drain/rebuild is required"))
	}
	return nil
}

// EnforceExposureDeploymentMode is the production startup boundary between
// the legacy per-Fact runtime and V4. A successful V4 call durably activates an
// immutable database marker in the same serialization domain as every guarded
// legacy insert. A later process presenting a legacy Catalog is refused even
// when the Catalog file and Gateway binary have both been replaced.
//
// This method never deletes or rewrites historical evidence. Catalog digests
// identify the activation attempt but are not used to prohibit later V4
// Catalog upgrades; the durable property is the one-way V4 deployment mode.
func (s *Store) EnforceExposureDeploymentMode(ctx context.Context, catalogDigest string, v4Enabled bool) error {
	const op = "enforce exposure deployment mode"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	if !validSHA256Hex(catalogDigest) {
		return opErr(op, ErrInvalid, fmt.Errorf("validated Catalog digest is required"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, v4CutoverAdvisoryLock); err != nil {
		return opErr(op, ErrConflict, err)
	}
	if v4Enabled {
		// The BEFORE INSERT trigger repeats the non-empty-ledger check while
		// holding this same lock. It also runs for ON CONFLICT DO NOTHING, so an
		// already activated database is rechecked fail-closed.
		_, err = tx.ExecContext(ctx, `INSERT INTO v4_cutover_state
(singleton,activated_catalog_digest,activated_at)
VALUES (TRUE,$1,$2) ON CONFLICT (singleton) DO NOTHING`, catalogDigest, dbTime(s.now()))
	} else {
		var activated bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS (
    SELECT 1 FROM v4_cutover_state WHERE singleton
)`).Scan(&activated)
		if err == nil && activated {
			err = fmt.Errorf("Control database is permanently activated for TaskGate V4; legacy Catalog refused")
		}
	}
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return opErr(op, ErrConflict, err)
	}
	return nil
}

// putOrdinalDictionary is the low-level persistence oracle used by Control PG
// tests. Production callers must use PutOrdinalSnapshotPublication so the
// ordinal package verifies the canonical manifest and dictionary digests
// before any metadata reaches this layer.
func (s *Store) putOrdinalDictionary(ctx context.Context, manifest OrdinalDictionaryManifest) error {
	const op = "put ordinal dictionary"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	normalized, err := normalizeOrdinalDictionary(manifest, s.now())
	if err != nil {
		return opErr(op, ErrInvalid, err)
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	if err := putOrdinalDictionaryTx(ctx, tx, normalized); err != nil {
		return opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return opErr(op, ErrConflict, err)
	}
	return nil
}

// PutOrdinalSnapshotPublication adapts the verified internal/ordinal registry
// contract to Control PG metadata. Optional HOT/COLD artifacts are stored as
// immutable content-addressed chunks; callers may omit them when the Catalog
// manifest points at an external mmap/cold artifact service.
func (s *Store) PutOrdinalSnapshotPublication(ctx context.Context, publicationManifestDigest string,
	index ordinal.SnapshotIndex, artifacts []OrdinalArtifactChunk) error {
	const op = "put ordinal snapshot publication"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	if index == nil || !validSHA256Hex(publicationManifestDigest) {
		return opErr(op, ErrInvalid, fmt.Errorf("verified snapshot index and publication digest are required"))
	}
	manifest := index.Manifest()
	if err := manifest.Validate(); err != nil {
		return opErr(op, ErrInvalid, err)
	}
	manifestDigest, err := manifest.Digest()
	if err != nil || manifestDigest != index.ManifestDigest() || manifest.DictionaryDigest != index.DictionaryDigest() {
		return opErr(op, ErrInvalid, fmt.Errorf("snapshot index manifest/dictionary digest mismatch"))
	}
	if publicationManifestDigest != manifestDigest {
		return opErr(op, ErrInvalid, fmt.Errorf("Catalog publication manifest digest does not match snapshot index"))
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return opErr(op, ErrInvalid, err)
	}
	converted := OrdinalDictionaryManifest{Digest: manifest.DictionaryDigest, ManifestDigest: manifestDigest, PublicationDigest: publicationManifestDigest,
		DatasourceID: manifest.SourceID, SourceNamespace: manifest.SourceNamespace, SnapshotID: manifest.Snapshot,
		ManifestJSON: raw, CreatedAt: s.now()}
	for _, source := range manifest.Segments {
		var factKind string
		switch source.Kind {
		case ordinal.SegmentBaseRow:
			factKind = "BASE_ROW"
		case ordinal.SegmentBaseCell:
			factKind = "BASE_CELL"
		default:
			return opErr(op, ErrInvalid, fmt.Errorf("snapshot publication contains dynamic segment %q", source.ID))
		}
		converted.FactCount += source.FactCount
		converted.Segments = append(converted.Segments, OrdinalDictionarySegment{ID: source.ID,
			FactKind: factKind, FieldName: source.Field, OrdinalCount: source.FactCount,
			Digest: controlSegmentManifestDigest(source)})
	}
	converted, err = normalizeOrdinalDictionary(converted, s.now())
	if err != nil {
		return opErr(op, ErrInvalid, err)
	}
	artifacts, err = normalizeOrdinalArtifacts(artifacts, manifestDigest)
	if err != nil {
		return opErr(op, ErrInvalid, err)
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	if err := putOrdinalDictionaryTx(ctx, tx, converted); err != nil {
		return opErr(op, ErrConflict, err)
	}
	if err := putOrdinalArtifactsTx(ctx, tx, converted.Digest, manifestDigest, artifacts, converted.CreatedAt); err != nil {
		return opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return opErr(op, ErrConflict, err)
	}
	return nil
}

func controlSegmentManifestDigest(segment ordinal.SegmentManifest) string {
	hash := sha256.New()
	hash.Write([]byte(dictionarySegmentDigestDomain))
	writeOrdinalDigestPart(hash, segment.ID)
	writeOrdinalDigestPart(hash, string(segment.Kind))
	writeOrdinalDigestPart(hash, segment.Field)
	var shard [2]byte
	binary.BigEndian.PutUint16(shard[:], segment.Shard)
	hash.Write(shard[:])
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], segment.FactCount)
	hash.Write(count[:])
	writeOrdinalDigestPart(hash, segment.HashesDigest)
	writeOrdinalDigestPart(hash, segment.PayloadsDigest)
	return hex.EncodeToString(hash.Sum(nil))
}

func normalizeOrdinalArtifacts(artifacts []OrdinalArtifactChunk, manifestDigest string) ([]OrdinalArtifactChunk, error) {
	seen := make(map[string]struct{}, len(artifacts))
	kindCounts := make(map[string]int, 3)
	for _, artifact := range artifacts {
		kindCounts[artifact.Kind]++
	}
	result := make([]OrdinalArtifactChunk, len(artifacts))
	for index, artifact := range artifacts {
		if (artifact.Kind != "HOT" && artifact.Kind != "COLD" && artifact.Kind != "SIDECAR") ||
			artifact.Index < 0 || (artifact.Compression != "" && artifact.Compression != "NONE" && artifact.Compression != "ZSTD") ||
			len(artifact.Payload) == 0 {
			return nil, fmt.Errorf("invalid ordinal artifact %d", index)
		}
		if artifact.Compression == "" {
			artifact.Compression = "NONE"
		}
		if artifact.UncompressedBytes == 0 && artifact.Compression == "NONE" {
			artifact.UncompressedBytes = int64(len(artifact.Payload))
		}
		if artifact.UncompressedBytes <= 0 {
			return nil, fmt.Errorf("ordinal artifact %d has invalid uncompressed size", index)
		}
		key := fmt.Sprintf("%s/%d", artifact.Kind, artifact.Index)
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("duplicate ordinal artifact %s", key)
		}
		seen[key] = struct{}{}
		digest := sha256.Sum256(artifact.Payload)
		computed := hex.EncodeToString(digest[:])
		if artifact.SHA256 != "" && artifact.SHA256 != computed {
			return nil, fmt.Errorf("ordinal artifact %s digest mismatch", key)
		}
		artifact.SHA256 = computed
		artifact.Payload = append([]byte(nil), artifact.Payload...)
		// A complete uncompressed prototype artifact is verified before it can
		// be bound to the manifest. Split/compressed production chunks retain
		// the manifest digest and are verified by their external loader.
		if artifact.Compression == "NONE" && artifact.Index == 0 && kindCounts[artifact.Kind] == 1 {
			switch artifact.Kind {
			case "HOT":
				if _, err := ordinal.ParseHotDictionary(artifact.Payload, manifestDigest); err != nil {
					return nil, fmt.Errorf("verify HOT artifact: %w", err)
				}
			case "COLD":
				if _, err := ordinal.ParseColdDictionary(artifact.Payload, manifestDigest); err != nil {
					return nil, fmt.Errorf("verify COLD artifact: %w", err)
				}
			}
		}
		result[index] = artifact
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		return result[i].Index < result[j].Index
	})
	return result, nil
}

func putOrdinalArtifactsTx(ctx context.Context, tx *sql.Tx, dictionaryDigest, manifestDigest string,
	artifacts []OrdinalArtifactChunk, now time.Time) error {
	for _, artifact := range artifacts {
		result, err := tx.ExecContext(ctx, `
INSERT INTO v4_dictionary_chunks(chunk_sha256,compression,payload,uncompressed_bytes,created_at)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (chunk_sha256) DO NOTHING`, artifact.SHA256,
			artifact.Compression, artifact.Payload, artifact.UncompressedBytes, dbTime(now))
		if err != nil {
			return err
		}
		inserted, _ := result.RowsAffected()
		if inserted == 0 {
			var compression string
			var payload []byte
			var size int64
			if err := tx.QueryRowContext(ctx, `SELECT compression,payload,uncompressed_bytes
FROM v4_dictionary_chunks WHERE chunk_sha256=$1`, artifact.SHA256).Scan(&compression, &payload, &size); err != nil {
				return err
			}
			if compression != artifact.Compression || size != artifact.UncompressedBytes || !bytes.Equal(payload, artifact.Payload) {
				return fmt.Errorf("dictionary artifact chunk collision for %s", artifact.SHA256)
			}
		}
		result, err = tx.ExecContext(ctx, `
INSERT INTO v4_dictionary_artifacts(dictionary_digest,artifact_kind,chunk_index,chunk_sha256,manifest_digest)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (dictionary_digest,artifact_kind,chunk_index) DO NOTHING`,
			dictionaryDigest, artifact.Kind, artifact.Index, artifact.SHA256, manifestDigest)
		if err != nil {
			return err
		}
		inserted, _ = result.RowsAffected()
		if inserted == 0 {
			var chunk, storedManifest string
			if err := tx.QueryRowContext(ctx, `SELECT chunk_sha256,manifest_digest FROM v4_dictionary_artifacts
WHERE dictionary_digest=$1 AND artifact_kind=$2 AND chunk_index=$3`, dictionaryDigest,
				artifact.Kind, artifact.Index).Scan(&chunk, &storedManifest); err != nil {
				return err
			}
			if chunk != artifact.SHA256 || storedManifest != manifestDigest {
				return fmt.Errorf("dictionary artifact binding collision for %s/%s/%d", dictionaryDigest, artifact.Kind, artifact.Index)
			}
		}
	}
	return nil
}

func normalizeOrdinalDictionary(manifest OrdinalDictionaryManifest, now time.Time) (OrdinalDictionaryManifest, error) {
	if !validSHA256Hex(manifest.Digest) || !validSHA256Hex(manifest.ManifestDigest) || !validSHA256Hex(manifest.PublicationDigest) ||
		strings.TrimSpace(manifest.DatasourceID) == "" || strings.TrimSpace(manifest.SourceNamespace) == "" ||
		strings.TrimSpace(manifest.SnapshotID) == "" || len(manifest.Segments) == 0 {
		return OrdinalDictionaryManifest{}, fmt.Errorf("dictionary identity, publication, snapshot and segments are required")
	}
	if manifest.PublicationDigest != manifest.ManifestDigest {
		return OrdinalDictionaryManifest{}, fmt.Errorf("publication digest must equal the Catalog-approved dictionary manifest digest")
	}
	normalizedJSON, err := normalizeJSON(manifest.ManifestJSON, `{}`)
	if err != nil {
		return OrdinalDictionaryManifest{}, fmt.Errorf("manifest JSON: %w", err)
	}
	manifest.ManifestJSON = normalizedJSON
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = now
	}
	manifest.CreatedAt = dbTime(manifest.CreatedAt)
	seenSegments := make(map[string]struct{}, len(manifest.Segments))
	var total uint64
	for segmentIndex := range manifest.Segments {
		segment := &manifest.Segments[segmentIndex]
		if strings.TrimSpace(segment.ID) == "" || segment.ID != strings.TrimSpace(segment.ID) ||
			(segment.FactKind != "BASE_ROW" && segment.FactKind != "BASE_CELL") || !validSHA256Hex(segment.Digest) ||
			segment.OrdinalCount > uint64(^uint32(0))+1 {
			return OrdinalDictionaryManifest{}, fmt.Errorf("invalid segment %d", segmentIndex)
		}
		if (segment.FactKind == "BASE_ROW" && segment.FieldName != "") ||
			(segment.FactKind == "BASE_CELL" && strings.TrimSpace(segment.FieldName) == "") {
			return OrdinalDictionaryManifest{}, fmt.Errorf("segment %q has invalid field binding", segment.ID)
		}
		if _, duplicate := seenSegments[segment.ID]; duplicate {
			return OrdinalDictionaryManifest{}, fmt.Errorf("duplicate segment %q", segment.ID)
		}
		seenSegments[segment.ID] = struct{}{}
		if ^uint64(0)-total < segment.OrdinalCount {
			return OrdinalDictionaryManifest{}, fmt.Errorf("dictionary fact count overflows")
		}
		total += segment.OrdinalCount
		seenChunks := make(map[int]struct{}, len(segment.Chunks))
		var covered uint64
		for chunkIndex := range segment.Chunks {
			chunk := &segment.Chunks[chunkIndex]
			if chunk.Index < 0 || !validSHA256Hex(chunk.SHA256) ||
				(chunk.Compression != "NONE" && chunk.Compression != "ZSTD") || len(chunk.Payload) == 0 ||
				chunk.UncompressedBytes <= 0 || chunk.FactCount == 0 ||
				chunk.FirstOrdinal+chunk.FactCount < chunk.FirstOrdinal ||
				chunk.FirstOrdinal+chunk.FactCount > segment.OrdinalCount {
				return OrdinalDictionaryManifest{}, fmt.Errorf("invalid chunk %d in segment %q", chunkIndex, segment.ID)
			}
			if _, duplicate := seenChunks[chunk.Index]; duplicate {
				return OrdinalDictionaryManifest{}, fmt.Errorf("duplicate chunk index %d in segment %q", chunk.Index, segment.ID)
			}
			seenChunks[chunk.Index] = struct{}{}
			digest := sha256.Sum256(chunk.Payload)
			if hex.EncodeToString(digest[:]) != chunk.SHA256 {
				return OrdinalDictionaryManifest{}, fmt.Errorf("chunk %s payload digest mismatch", chunk.SHA256)
			}
			covered += chunk.FactCount
		}
		if len(segment.Chunks) != 0 && covered != segment.OrdinalCount {
			return OrdinalDictionaryManifest{}, fmt.Errorf("segment %q chunks cover %d of %d ordinals", segment.ID, covered, segment.OrdinalCount)
		}
		sort.Slice(segment.Chunks, func(i, j int) bool { return segment.Chunks[i].Index < segment.Chunks[j].Index })
		var next uint64
		for _, chunk := range segment.Chunks {
			if chunk.FirstOrdinal != next {
				return OrdinalDictionaryManifest{}, fmt.Errorf("segment %q chunks are not contiguous", segment.ID)
			}
			next += chunk.FactCount
		}
	}
	if total != manifest.FactCount {
		return OrdinalDictionaryManifest{}, fmt.Errorf("manifest fact count %d differs from segment total %d", manifest.FactCount, total)
	}
	if total > uint64(math.MaxInt64) {
		return OrdinalDictionaryManifest{}, fmt.Errorf("dictionary fact count exceeds PostgreSQL BIGINT")
	}
	sort.Slice(manifest.Segments, func(i, j int) bool { return manifest.Segments[i].ID < manifest.Segments[j].ID })
	return manifest, nil
}

func putOrdinalDictionaryTx(ctx context.Context, tx *sql.Tx, manifest OrdinalDictionaryManifest) error {
	result, err := tx.ExecContext(ctx, `
INSERT INTO v4_dictionary_manifests(dictionary_digest,manifest_digest,publication_digest,datasource_id,
 source_namespace,snapshot_id,fact_count,segment_count,manifest_json,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (dictionary_digest) DO NOTHING`,
		manifest.Digest, manifest.ManifestDigest, manifest.PublicationDigest, manifest.DatasourceID, manifest.SourceNamespace,
		manifest.SnapshotID, manifest.FactCount, len(manifest.Segments), string(manifest.ManifestJSON), manifest.CreatedAt)
	if err != nil {
		return err
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		var publication, datasource, namespace, snapshot string
		var facts int64
		var segments int
		var raw []byte
		var storedManifest string
		if err := tx.QueryRowContext(ctx, `SELECT manifest_digest,publication_digest,datasource_id,source_namespace,snapshot_id,
 fact_count,segment_count,manifest_json FROM v4_dictionary_manifests WHERE dictionary_digest=$1`, manifest.Digest).
			Scan(&storedManifest, &publication, &datasource, &namespace, &snapshot, &facts, &segments, &raw); err != nil {
			return err
		}
		if storedManifest != manifest.ManifestDigest || publication != manifest.PublicationDigest || datasource != manifest.DatasourceID ||
			namespace != manifest.SourceNamespace || snapshot != manifest.SnapshotID || facts != int64(manifest.FactCount) ||
			segments != len(manifest.Segments) || !sameJSON(raw, manifest.ManifestJSON) {
			return fmt.Errorf("dictionary digest collision for %s", manifest.Digest)
		}
	}
	for _, segment := range manifest.Segments {
		result, err = tx.ExecContext(ctx, `
INSERT INTO v4_dictionary_segments(dictionary_digest,segment_id,fact_kind,field_name,ordinal_count,segment_digest,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (dictionary_digest,segment_id) DO NOTHING`,
			manifest.Digest, segment.ID, segment.FactKind, segment.FieldName, segment.OrdinalCount, segment.Digest, manifest.CreatedAt)
		if err != nil {
			return err
		}
		inserted, _ = result.RowsAffected()
		if inserted == 0 {
			var kind, field, digest string
			var count int64
			if err := tx.QueryRowContext(ctx, `SELECT fact_kind,field_name,ordinal_count,segment_digest
FROM v4_dictionary_segments WHERE dictionary_digest=$1 AND segment_id=$2`, manifest.Digest, segment.ID).
				Scan(&kind, &field, &count, &digest); err != nil {
				return err
			}
			if kind != segment.FactKind || field != segment.FieldName || count != int64(segment.OrdinalCount) || digest != segment.Digest {
				return fmt.Errorf("dictionary segment collision for %s/%s", manifest.Digest, segment.ID)
			}
		}
		for _, chunk := range segment.Chunks {
			result, err = tx.ExecContext(ctx, `
INSERT INTO v4_dictionary_chunks(chunk_sha256,compression,payload,uncompressed_bytes,created_at)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (chunk_sha256) DO NOTHING`, chunk.SHA256, chunk.Compression,
				chunk.Payload, chunk.UncompressedBytes, manifest.CreatedAt)
			if err != nil {
				return err
			}
			inserted, _ = result.RowsAffected()
			if inserted == 0 {
				var compression string
				var payload []byte
				var size int64
				if err := tx.QueryRowContext(ctx, `SELECT compression,payload,uncompressed_bytes
FROM v4_dictionary_chunks WHERE chunk_sha256=$1`, chunk.SHA256).Scan(&compression, &payload, &size); err != nil {
					return err
				}
				if compression != chunk.Compression || size != chunk.UncompressedBytes || !bytes.Equal(payload, chunk.Payload) {
					return fmt.Errorf("dictionary chunk digest collision for %s", chunk.SHA256)
				}
			}
			result, err = tx.ExecContext(ctx, `
INSERT INTO v4_dictionary_segment_chunks(dictionary_digest,segment_id,chunk_index,chunk_sha256,first_ordinal,fact_count)
VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (dictionary_digest,segment_id,chunk_index) DO NOTHING`,
				manifest.Digest, segment.ID, chunk.Index, chunk.SHA256, chunk.FirstOrdinal, chunk.FactCount)
			if err != nil {
				return err
			}
			inserted, _ = result.RowsAffected()
			if inserted == 0 {
				var hash string
				var first, count int64
				if err := tx.QueryRowContext(ctx, `SELECT chunk_sha256,first_ordinal,fact_count
FROM v4_dictionary_segment_chunks WHERE dictionary_digest=$1 AND segment_id=$2 AND chunk_index=$3`,
					manifest.Digest, segment.ID, chunk.Index).Scan(&hash, &first, &count); err != nil {
					return err
				}
				if hash != chunk.SHA256 || first != int64(chunk.FirstOrdinal) || count != int64(chunk.FactCount) {
					return fmt.Errorf("dictionary chunk mapping collision for %s/%s/%d", manifest.Digest, segment.ID, chunk.Index)
				}
			}
		}
	}
	return nil
}

// PutOrdinalDictionarySet publishes the exact canonical snapshot bundle used
// by an observation. The manifest and digest implementation are shared with
// Gateway and receipts; Control additionally proves that every member's
// dictionary/manifest pair was previously published.
func (s *Store) PutOrdinalDictionarySet(ctx context.Context, set OrdinalDictionarySet) error {
	const op = "put ordinal dictionary set"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	if err := set.Validate(); err != nil {
		return opErr(op, ErrInvalid, err)
	}
	computedDigest, err := set.Digest()
	if err != nil {
		return opErr(op, ErrInvalid, err)
	}
	raw, err := json.Marshal(set)
	if err != nil {
		return opErr(op, ErrInvalid, err)
	}
	now := dbTime(s.now())
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	result, err := tx.ExecContext(ctx, `INSERT INTO v4_dictionary_sets(dictionary_set_digest,catalog_digest,manifest_json,created_at)
VALUES ($1,$2,$3,$4) ON CONFLICT (dictionary_set_digest) DO NOTHING`, computedDigest, set.CatalogDigest, string(raw), now)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		var catalog string
		var stored []byte
		if err := tx.QueryRowContext(ctx, `SELECT catalog_digest,manifest_json FROM v4_dictionary_sets
WHERE dictionary_set_digest=$1`, computedDigest).Scan(&catalog, &stored); err != nil {
			return opErr(op, ErrConflict, err)
		}
		if catalog != set.CatalogDigest || !sameJSON(stored, raw) {
			return opErr(op, ErrConflict, fmt.Errorf("dictionary set digest collision for %s", computedDigest))
		}
	}
	for index, member := range set.Members {
		result, err = tx.ExecContext(ctx, `INSERT INTO v4_dictionary_set_members(dictionary_set_digest,member_index,
 publication_name,dictionary_digest,manifest_digest)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (dictionary_set_digest,member_index) DO NOTHING`, computedDigest,
			index, member.PublicationName, member.DictionaryDigest, member.ManifestDigest)
		if err != nil {
			return opErr(op, ErrConflict, err)
		}
		inserted, _ = result.RowsAffected()
		if inserted == 0 {
			var publication, dictionary, manifest string
			if err := tx.QueryRowContext(ctx, `SELECT publication_name,dictionary_digest,manifest_digest
FROM v4_dictionary_set_members WHERE dictionary_set_digest=$1 AND member_index=$2`, computedDigest, index).
				Scan(&publication, &dictionary, &manifest); err != nil {
				return opErr(op, ErrConflict, err)
			}
			if publication != member.PublicationName || dictionary != member.DictionaryDigest || manifest != member.ManifestDigest {
				return opErr(op, ErrConflict, fmt.Errorf("dictionary set member collision for %s", computedDigest))
			}
		}
	}
	var memberCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM v4_dictionary_set_members WHERE dictionary_set_digest=$1`, computedDigest).Scan(&memberCount); err != nil {
		return opErr(op, ErrConflict, err)
	}
	if memberCount != len(set.Members) {
		return opErr(op, ErrConflict, fmt.Errorf("dictionary set %s has different membership", computedDigest))
	}
	if err := tx.Commit(); err != nil {
		return opErr(op, ErrConflict, err)
	}
	return nil
}

func activateV4CutoverTx(ctx context.Context, tx *sql.Tx, taskID string, now time.Time) error {
	var existing bool
	err := tx.QueryRowContext(ctx, `SELECT singleton FROM v4_cutover_state WHERE singleton FOR UPDATE`).Scan(&existing)
	if err == nil {
		return nil
	}
	if !isNoRows(err) {
		return err
	}
	// The database trigger repeats this check, protecting callers that bypass
	// this Go API. No deletion is ever attempted here.
	_, err = tx.ExecContext(ctx, `INSERT INTO v4_cutover_state(singleton,activated_by_task_id,activated_at)
VALUES (TRUE,$1,$2) ON CONFLICT (singleton) DO NOTHING`, taskID, dbTime(now))
	return err
}
