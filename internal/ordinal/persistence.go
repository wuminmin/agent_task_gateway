package ordinal

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

const (
	hotArtifactMagic          = "TGHOTV1\x00"
	coldArtifactMagic         = "TGCOLD1\x00"
	hotArtifactDomain         = "TASKGATE-ORDINAL-HOT-ARTIFACT-V1\x00"
	coldArtifactDomain        = "TASKGATE-ORDINAL-COLD-ARTIFACT-V1\x00"
	maxManifestBytes          = 4 << 20
	maxArtifactString         = 1 << 20
	artifactStreamBufferBytes = 64 << 10
)

// MarshalBinary emits the prototype deterministic hot artifact with the
// entity-key lookup table enabled. Hash arrays and ordinals are fixed-width;
// only the small manifest uses JSON.
func (d *HotDictionary) MarshalBinary() ([]byte, error) {
	return d.marshalBinary(true)
}

// MarshalBinaryWithoutEntityKeys omits entity->handle strings when the
// business sidecar already returns trusted row handles. Handle->FactRefs and
// all hash arrays remain available and manifest-bound.
func (d *HotDictionary) MarshalBinaryWithoutEntityKeys() ([]byte, error) {
	return d.marshalBinary(false)
}

func (d *HotDictionary) marshalBinary(includeEntityKeys bool) ([]byte, error) {
	if d == nil || d.ManifestDigest() == "" || d.hotIndexDigest() != d.manifest.HotIndexDigest {
		return nil, fmt.Errorf("%w: inconsistent hot dictionary", ErrInvalid)
	}
	manifestDigest, err := d.manifest.Digest()
	if err != nil || manifestDigest != d.ManifestDigest() {
		return nil, fmt.Errorf("%w: hot manifest digest", ErrDigestMismatch)
	}
	manifestJSON, err := json.Marshal(d.manifest)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal hot manifest", ErrInvalid)
	}
	var body bytes.Buffer
	body.WriteString(hotArtifactMagic)
	writeBytes(&body, manifestJSON)
	writeString(&body, d.manifestDigest)
	if includeEntityKeys {
		body.WriteByte(1)
	} else {
		body.WriteByte(0)
	}
	writeUint64(&body, uint64(len(d.manifest.Segments)))
	for _, segment := range d.manifest.Segments {
		writeString(&body, segment.ID)
		hashes := d.hashes[segment.ID]
		writeUint64(&body, uint64(len(hashes)))
		for _, hash := range hashes {
			body.Write(hash[:])
		}
	}
	writeString(&body, d.rowSegment)
	writeUint64(&body, uint64(len(d.fields)))
	for _, field := range d.fields {
		writeString(&body, field.name)
		writeString(&body, field.segmentID)
	}
	writeUint64(&body, uint64(len(d.rows)))
	for _, row := range d.rows {
		if includeEntityKeys {
			writeString(&body, row.entityKey)
		}
		if d.rowSegment == "" {
			writeString(&body, row.rowSegmentID)
		}
		writeUint32(&body, row.rowOrdinal)
		writeUint64(&body, uint64(len(row.cellOrdinals)))
		for index, ordinal := range row.cellOrdinals {
			if d.fields[index].segmentID == "" {
				writeString(&body, row.cellSegmentIDs[index])
			}
			writeUint32(&body, ordinal)
		}
	}
	return sealArtifact(hotArtifactDomain, body.Bytes()), nil
}

// ParseHotDictionary verifies the artifact checksum, externally expected
// manifest digest, per-segment hash roots, sidecar digest (when present), all
// bounds, and the manifest-bound hot-index digest before activation.
func ParseHotDictionary(encoded []byte, expectedManifestDigest string) (*HotDictionary, error) {
	body, err := openArtifact(hotArtifactDomain, encoded)
	if err != nil {
		return nil, err
	}
	reader := artifactReader{Reader: bytes.NewReader(body)}
	magic, err := reader.readFixed(len(hotArtifactMagic))
	if err != nil || string(magic) != hotArtifactMagic {
		return nil, fmt.Errorf("%w: hot artifact magic", ErrInvalid)
	}
	manifestJSON, err := reader.readBytes(maxManifestBytes)
	if err != nil {
		return nil, err
	}
	manifest, err := decodeManifest(manifestJSON)
	if err != nil {
		return nil, err
	}
	storedManifestDigest, err := reader.readString(sha256.Size * 2)
	if err != nil || !validDigest(expectedManifestDigest) || storedManifestDigest != expectedManifestDigest {
		return nil, fmt.Errorf("%w: expected hot manifest", ErrDigestMismatch)
	}
	actualManifestDigest, err := manifest.Digest()
	if err != nil || actualManifestDigest != expectedManifestDigest {
		return nil, fmt.Errorf("%w: hot manifest content", ErrDigestMismatch)
	}
	includeEntityByte, err := reader.ReadByte()
	if err != nil || includeEntityByte > 1 {
		return nil, fmt.Errorf("%w: hot entity-key flag", ErrInvalid)
	}
	includeEntityKeys := includeEntityByte == 1
	hot := &HotDictionary{manifest: manifest, manifestDigest: actualManifestDigest,
		hashes: make(map[string][][sha256.Size]byte, len(manifest.Segments))}
	segmentCount, err := reader.readUint64()
	if err != nil || segmentCount != uint64(len(manifest.Segments)) {
		return nil, fmt.Errorf("%w: hot segment count", ErrInvalid)
	}
	for _, segment := range manifest.Segments {
		segmentID, readErr := reader.readString(maxArtifactString)
		count, countErr := reader.readUint64()
		if readErr != nil || countErr != nil || segmentID != segment.ID || count != segment.FactCount || count > uint64(maxInt()) {
			return nil, fmt.Errorf("%w: hot segment metadata", ErrInvalid)
		}
		hashes := make([][sha256.Size]byte, int(count))
		for index := range hashes {
			material, readHashErr := reader.readFixed(sha256.Size)
			if readHashErr != nil {
				return nil, readHashErr
			}
			copy(hashes[index][:], material)
		}
		if digestHashArray(hashes) != segment.HashesDigest {
			return nil, fmt.Errorf("%w: segment hash root", ErrDigestMismatch)
		}
		hot.hashes[segment.ID] = hashes
	}
	hot.rowSegment, err = reader.readString(maxArtifactString)
	if err != nil {
		return nil, err
	}
	fieldCount, err := reader.readUint64()
	if err != nil || fieldCount > uint64(maxInt()) {
		return nil, fmt.Errorf("%w: hot field count", ErrInvalid)
	}
	hot.fields = make([]hotField, int(fieldCount))
	for index := range hot.fields {
		hot.fields[index].name, err = reader.readString(maxArtifactString)
		if err != nil {
			return nil, err
		}
		hot.fields[index].segmentID, err = reader.readString(maxArtifactString)
		if err != nil {
			return nil, err
		}
		if (hot.fields[index].segmentID != "" && !hot.segmentMatches(hot.fields[index].segmentID, SegmentBaseCell, hot.fields[index].name)) ||
			(index > 0 && hot.fields[index-1].name >= hot.fields[index].name) {
			return nil, fmt.Errorf("%w: hot field layout", ErrInvalid)
		}
	}
	rowCount, err := reader.readUint64()
	if err != nil || rowCount > uint64(maxInt()) {
		return nil, fmt.Errorf("%w: hot row count", ErrInvalid)
	}
	hot.rows = make([]hotRow, int(rowCount))
	if includeEntityKeys {
		hot.entityToHandle = make(map[string]RowHandle, int(rowCount))
	}
	if rowCount > 0 && hot.rowSegment != "" && !hot.segmentMatches(hot.rowSegment, SegmentBaseRow, "") {
		return nil, fmt.Errorf("%w: hot row segment", ErrInvalid)
	}
	for index := range hot.rows {
		if includeEntityKeys {
			hot.rows[index].entityKey, err = reader.readString(maxArtifactString)
			if err != nil || !validID(hot.rows[index].entityKey) {
				return nil, fmt.Errorf("%w: hot entity key", ErrInvalid)
			}
			if _, duplicate := hot.entityToHandle[hot.rows[index].entityKey]; duplicate {
				return nil, fmt.Errorf("%w: duplicate hot entity key", ErrInvalid)
			}
			hot.entityToHandle[hot.rows[index].entityKey] = RowHandle(index + 1)
		}
		if hot.rowSegment == "" {
			hot.rows[index].rowSegmentID, err = reader.readString(maxArtifactString)
			if err != nil || !hot.segmentMatches(hot.rows[index].rowSegmentID, SegmentBaseRow, "") {
				return nil, fmt.Errorf("%w: hot row shard", ErrInvalid)
			}
		}
		hot.rows[index].rowOrdinal, err = reader.readUint32()
		if err != nil {
			return nil, err
		}
		cellCount, countErr := reader.readUint64()
		if countErr != nil || cellCount != uint64(len(hot.fields)) {
			return nil, fmt.Errorf("%w: hot row cell count", ErrInvalid)
		}
		hot.rows[index].cellOrdinals = make([]uint32, len(hot.fields))
		hot.rows[index].cellSegmentIDs = make([]string, len(hot.fields))
		for cell := range hot.rows[index].cellOrdinals {
			segmentID := hot.fields[cell].segmentID
			if segmentID == "" {
				hot.rows[index].cellSegmentIDs[cell], err = reader.readString(maxArtifactString)
				segmentID = hot.rows[index].cellSegmentIDs[cell]
				if err != nil || !hot.segmentMatches(segmentID, SegmentBaseCell, hot.fields[cell].name) {
					return nil, fmt.Errorf("%w: hot cell shard", ErrInvalid)
				}
			}
			hot.rows[index].cellOrdinals[cell], err = reader.readUint32()
			if err != nil || uint64(hot.rows[index].cellOrdinals[cell]) >= uint64(len(hot.hashes[segmentID])) {
				return nil, fmt.Errorf("%w: hot cell ordinal", ErrInvalid)
			}
		}
		rowSegmentID := hot.rowSegment
		if rowSegmentID == "" {
			rowSegmentID = hot.rows[index].rowSegmentID
		}
		if rowSegmentID == "" || uint64(hot.rows[index].rowOrdinal) >= uint64(len(hot.hashes[rowSegmentID])) {
			return nil, fmt.Errorf("%w: hot row ordinal", ErrInvalid)
		}
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("%w: trailing hot artifact bytes", ErrNonCanonical)
	}
	if hot.hotIndexDigest() != manifest.HotIndexDigest {
		return nil, fmt.Errorf("%w: hot index content", ErrDigestMismatch)
	}
	if includeEntityKeys && hot.sidecarDigest() != manifest.SidecarDigest {
		return nil, fmt.Errorf("%w: hot sidecar content", ErrDigestMismatch)
	}
	return hot, nil
}

func (d *ColdDictionary) MarshalBinary() ([]byte, error) {
	if d == nil {
		return nil, fmt.Errorf("%w: nil cold dictionary", ErrInvalid)
	}
	actualManifestDigest, err := d.manifest.Digest()
	if err != nil || actualManifestDigest != d.manifestDigest || d.dictionaryDigest != d.manifest.DictionaryDigest {
		return nil, fmt.Errorf("%w: cold manifest", ErrDigestMismatch)
	}
	manifestJSON, _ := json.Marshal(d.manifest)
	var body bytes.Buffer
	body.WriteString(coldArtifactMagic)
	writeBytes(&body, manifestJSON)
	writeString(&body, d.manifestDigest)
	writeUint64(&body, uint64(len(d.manifest.Segments)))
	for _, segment := range d.manifest.Segments {
		entries := d.segments[segment.ID]
		writeString(&body, segment.ID)
		writeUint64(&body, uint64(len(entries)))
		for _, entry := range entries {
			writeBytes(&body, entry.payload)
			encodedFact, marshalErr := json.Marshal(entry.fact)
			if marshalErr != nil {
				return nil, marshalErr
			}
			writeBytes(&body, encodedFact)
		}
	}
	return sealArtifact(coldArtifactDomain, body.Bytes()), nil
}

func ParseColdDictionary(encoded []byte, expectedManifestDigest string) (*ColdDictionary, error) {
	return parseColdDictionary(encoded, expectedManifestDigest, true)
}

// VerifyColdDictionary performs the same canonical-payload and digest checks
// as ParseColdDictionary without retaining a second in-memory audit index.
// Offline publication uses this path because it already owns the sealed COLD
// bytes; materializing another million FactID objects solely to discard them
// would make verification needlessly scale with object overhead.
func VerifyColdDictionary(encoded []byte, expectedManifestDigest string) error {
	_, err := parseColdDictionary(encoded, expectedManifestDigest, false)
	return err
}

// VerifyColdDictionaryReader verifies a sealed COLD artifact without loading
// it into memory. artifactBytes must be the size of the opened regular file.
// The parser holds at most one bounded FactID JSON document and canonical
// payload at a time; the artifact body and payload stream use fixed 64 KiB
// reads. Besides the transport seal, every semantic root is recomputed from
// canonical FactIDs and matched to the Catalog-bound dictionary manifest.
//
// A raw payload is compared with the reconstructed canonical payload by
// length and SHA-256. This is the same collision-resistance assumption used
// by FactID, the immutable segment roots, and the publication manifest.
func VerifyColdDictionaryReader(reader io.Reader, artifactBytes int64, expectedManifestDigest string) error {
	if reader == nil || artifactBytes <= sha256.Size || !validDigest(expectedManifestDigest) {
		return fmt.Errorf("%w: invalid cold artifact stream", ErrInvalid)
	}
	bodyBytes := artifactBytes - sha256.Size
	bodyLimit := &io.LimitedReader{R: reader, N: bodyBytes}
	bodyHash := sha256.New()
	_, _ = bodyHash.Write([]byte(coldArtifactDomain))
	stream := &streamArtifactReader{reader: io.TeeReader(bodyLimit, bodyHash), remaining: bodyBytes}
	if err := verifyColdDictionaryBody(stream, expectedManifestDigest); err != nil {
		return err
	}
	if stream.remaining != 0 || bodyLimit.N != 0 {
		return fmt.Errorf("%w: trailing cold artifact bytes", ErrNonCanonical)
	}
	var seal [sha256.Size]byte
	if _, err := io.ReadFull(reader, seal[:]); err != nil {
		return fmt.Errorf("%w: truncated cold artifact seal", ErrInvalid)
	}
	if !bytes.Equal(seal[:], bodyHash.Sum(nil)) {
		return fmt.Errorf("%w: cold artifact seal", ErrDigestMismatch)
	}
	var extra [1]byte
	if read, err := io.ReadFull(reader, extra[:]); read != 0 || err != io.EOF {
		return fmt.Errorf("%w: cold artifact size", ErrNonCanonical)
	}
	return nil
}

// VerifyColdDictionaryEnvelopeReader is the bounded activation check for an
// already compiler-verified, immutable publication. It streams the complete
// file to verify its domain-separated seal and returns its transport SHA-256,
// while also binding the embedded dictionary manifest to the Catalog digest.
// It intentionally does not redo per-Fact canonical verification; callers
// must only use it for a read-only publication produced by a compiler that ran
// VerifyColdDictionary (or VerifyColdDictionaryReader) before publication.
func VerifyColdDictionaryEnvelopeReader(reader io.Reader, artifactBytes int64,
	expectedManifestDigest string) ([sha256.Size]byte, error) {
	var artifactDigest [sha256.Size]byte
	if reader == nil || artifactBytes <= sha256.Size || !validDigest(expectedManifestDigest) {
		return artifactDigest, fmt.Errorf("%w: invalid cold artifact stream", ErrInvalid)
	}
	bodyBytes := artifactBytes - sha256.Size
	bodyLimit := &io.LimitedReader{R: reader, N: bodyBytes}
	bodyHash := sha256.New()
	_, _ = bodyHash.Write([]byte(coldArtifactDomain))
	fileHash := sha256.New()
	stream := &streamArtifactReader{reader: io.TeeReader(bodyLimit, io.MultiWriter(bodyHash, fileHash)), remaining: bodyBytes}
	magic, err := stream.readFixed(len(coldArtifactMagic))
	if err != nil || string(magic) != coldArtifactMagic {
		return artifactDigest, fmt.Errorf("%w: cold artifact magic", ErrInvalid)
	}
	manifestJSON, err := stream.readBytes(maxManifestBytes)
	if err != nil {
		return artifactDigest, err
	}
	manifest, err := decodeManifest(manifestJSON)
	if err != nil {
		return artifactDigest, err
	}
	storedDigest, err := stream.readString(sha256.Size * 2)
	actualDigest, digestErr := manifest.Digest()
	if err != nil || digestErr != nil || storedDigest != expectedManifestDigest || actualDigest != expectedManifestDigest {
		return artifactDigest, fmt.Errorf("%w: cold manifest", ErrDigestMismatch)
	}
	if err := stream.discardRemaining(); err != nil || bodyLimit.N != 0 {
		return artifactDigest, fmt.Errorf("%w: truncated cold artifact body", ErrInvalid)
	}
	var seal [sha256.Size]byte
	if _, err := io.ReadFull(reader, seal[:]); err != nil {
		return artifactDigest, fmt.Errorf("%w: truncated cold artifact seal", ErrInvalid)
	}
	if !bytes.Equal(seal[:], bodyHash.Sum(nil)) {
		return artifactDigest, fmt.Errorf("%w: cold artifact seal", ErrDigestMismatch)
	}
	_, _ = fileHash.Write(seal[:])
	var extra [1]byte
	if read, err := io.ReadFull(reader, extra[:]); read != 0 || err != io.EOF {
		return artifactDigest, fmt.Errorf("%w: cold artifact size", ErrNonCanonical)
	}
	copy(artifactDigest[:], fileHash.Sum(nil))
	return artifactDigest, nil
}

func verifyColdDictionaryBody(reader *streamArtifactReader, expectedManifestDigest string) error {
	magic, err := reader.readFixed(len(coldArtifactMagic))
	if err != nil || string(magic) != coldArtifactMagic {
		return fmt.Errorf("%w: cold artifact magic", ErrInvalid)
	}
	manifestJSON, err := reader.readBytes(maxManifestBytes)
	if err != nil {
		return err
	}
	manifest, err := decodeManifest(manifestJSON)
	if err != nil {
		return err
	}
	storedDigest, err := reader.readString(sha256.Size * 2)
	actualDigest, digestErr := manifest.Digest()
	if err != nil || digestErr != nil || storedDigest != expectedManifestDigest || actualDigest != expectedManifestDigest {
		return fmt.Errorf("%w: cold manifest", ErrDigestMismatch)
	}
	segmentCount, err := reader.readUint64()
	if err != nil || segmentCount != uint64(len(manifest.Segments)) {
		return fmt.Errorf("%w: cold segment count", ErrInvalid)
	}

	dictionaryHash := sha256.New()
	_, _ = dictionaryHash.Write([]byte(dictionaryDigestDomain))
	writeUint64(dictionaryHash, uint64(len(manifest.Segments)))
	coldHash := sha256.New()
	_, _ = coldHash.Write([]byte(coldPayloadDigestDomain))
	writeUint64(coldHash, uint64(len(manifest.Segments)))
	for _, segment := range manifest.Segments {
		segmentID, idErr := reader.readString(maxArtifactString)
		count, countErr := reader.readUint64()
		if idErr != nil || countErr != nil || segmentID != segment.ID || count != segment.FactCount || count > uint64(maxInt()) {
			return fmt.Errorf("%w: cold segment metadata", ErrInvalid)
		}
		segmentHashes := sha256.New()
		_, _ = segmentHashes.Write([]byte(segmentHashesDomain))
		writeUint64(segmentHashes, count)
		segmentPayloads := sha256.New()
		_, _ = segmentPayloads.Write([]byte(segmentPayloadsDomain))
		writeUint64(segmentPayloads, count)
		writeString(dictionaryHash, segment.ID)
		writeString(dictionaryHash, string(segment.Kind))
		writeString(dictionaryHash, segment.Field)
		writeUint16(dictionaryHash, segment.Shard)
		writeUint64(dictionaryHash, count)
		writeString(coldHash, segment.ID)
		writeUint64(coldHash, count)

		for index := uint64(0); index < count; index++ {
			payloadLength, payloadDigest, payloadErr := reader.readHashedBytes(maxArtifactString * 64)
			factJSON, factErr := reader.readBytes(maxArtifactString * 4)
			if payloadErr != nil || factErr != nil {
				return fmt.Errorf("%w: cold fact bytes", ErrInvalid)
			}
			fact, decodeErr := decodeFact(factJSON)
			canonical, canonicalErr := fact.CanonicalPayload()
			hashText, hashErr := fact.Hash()
			canonicalDigest := sha256.Sum256(canonical)
			if decodeErr != nil || canonicalErr != nil || hashErr != nil || uint64(len(canonical)) != payloadLength ||
				!bytes.Equal(payloadDigest[:], canonicalDigest[:]) {
				return fmt.Errorf("%w: cold canonical fact", ErrInvalid)
			}
			hashBytes, decodeHashErr := hex.DecodeString(hashText)
			if decodeHashErr != nil || len(hashBytes) != sha256.Size {
				return fmt.Errorf("%w: cold FactID hash", ErrInvalid)
			}
			_, _ = segmentHashes.Write(hashBytes)
			writeBytes(segmentPayloads, canonical)
			_, _ = dictionaryHash.Write(hashBytes)
			writeBytes(dictionaryHash, canonical)
			writeBytes(coldHash, canonical)
		}
		if hex.EncodeToString(segmentHashes.Sum(nil)) != segment.HashesDigest ||
			hex.EncodeToString(segmentPayloads.Sum(nil)) != segment.PayloadsDigest {
			return fmt.Errorf("%w: cold segment roots", ErrDigestMismatch)
		}
	}
	if reader.remaining != 0 || hex.EncodeToString(dictionaryHash.Sum(nil)) != manifest.DictionaryDigest ||
		hex.EncodeToString(coldHash.Sum(nil)) != manifest.ColdPayloadDigest {
		return fmt.Errorf("%w: cold dictionary content", ErrDigestMismatch)
	}
	return nil
}

func parseColdDictionary(encoded []byte, expectedManifestDigest string, retain bool) (*ColdDictionary, error) {
	body, err := openArtifact(coldArtifactDomain, encoded)
	if err != nil {
		return nil, err
	}
	reader := artifactReader{Reader: bytes.NewReader(body)}
	magic, err := reader.readFixed(len(coldArtifactMagic))
	if err != nil || string(magic) != coldArtifactMagic {
		return nil, fmt.Errorf("%w: cold artifact magic", ErrInvalid)
	}
	manifestJSON, err := reader.readBytes(maxManifestBytes)
	if err != nil {
		return nil, err
	}
	manifest, err := decodeManifest(manifestJSON)
	if err != nil {
		return nil, err
	}
	storedDigest, err := reader.readString(sha256.Size * 2)
	actualDigest, digestErr := manifest.Digest()
	if err != nil || digestErr != nil || !validDigest(expectedManifestDigest) || storedDigest != expectedManifestDigest || actualDigest != expectedManifestDigest {
		return nil, fmt.Errorf("%w: cold manifest", ErrDigestMismatch)
	}
	segmentCount, err := reader.readUint64()
	if err != nil || segmentCount != uint64(len(manifest.Segments)) {
		return nil, fmt.Errorf("%w: cold segment count", ErrInvalid)
	}
	var cold *ColdDictionary
	if retain {
		cold = &ColdDictionary{manifest: manifest, manifestDigest: actualDigest, dictionaryDigest: manifest.DictionaryDigest,
			segments: make(map[string][]coldEntry, len(manifest.Segments))}
	}
	dictionaryHash := sha256.New()
	dictionaryHash.Write([]byte(dictionaryDigestDomain))
	writeUint64(dictionaryHash, uint64(len(manifest.Segments)))
	coldHash := sha256.New()
	coldHash.Write([]byte(coldPayloadDigestDomain))
	writeUint64(coldHash, uint64(len(manifest.Segments)))
	for _, segment := range manifest.Segments {
		segmentID, idErr := reader.readString(maxArtifactString)
		count, countErr := reader.readUint64()
		if idErr != nil || countErr != nil || segmentID != segment.ID || count != segment.FactCount || count > uint64(maxInt()) {
			return nil, fmt.Errorf("%w: cold segment metadata", ErrInvalid)
		}
		var coldEntries []coldEntry
		if retain {
			coldEntries = make([]coldEntry, int(count))
		}
		segmentHashes := sha256.New()
		segmentHashes.Write([]byte(segmentHashesDomain))
		writeUint64(segmentHashes, count)
		segmentPayloads := sha256.New()
		segmentPayloads.Write([]byte(segmentPayloadsDomain))
		writeUint64(segmentPayloads, count)
		writeString(dictionaryHash, segment.ID)
		writeString(dictionaryHash, string(segment.Kind))
		writeString(dictionaryHash, segment.Field)
		writeUint16(dictionaryHash, segment.Shard)
		writeUint64(dictionaryHash, count)
		writeString(coldHash, segment.ID)
		writeUint64(coldHash, count)
		for index := 0; index < int(count); index++ {
			payload, payloadErr := reader.readBytes(maxArtifactString * 64)
			factJSON, factErr := reader.readBytes(maxArtifactString * 4)
			if payloadErr != nil || factErr != nil {
				return nil, fmt.Errorf("%w: cold fact bytes", ErrInvalid)
			}
			fact, decodeErr := decodeFact(factJSON)
			canonical, canonicalErr := fact.CanonicalPayload()
			hashText, hashErr := fact.Hash()
			if decodeErr != nil || canonicalErr != nil || hashErr != nil || !bytes.Equal(payload, canonical) {
				return nil, fmt.Errorf("%w: cold canonical fact", ErrInvalid)
			}
			hashBytes, _ := hex.DecodeString(hashText)
			var factHash [sha256.Size]byte
			copy(factHash[:], hashBytes)
			_, _ = segmentHashes.Write(factHash[:])
			writeBytes(segmentPayloads, payload)
			_, _ = dictionaryHash.Write(factHash[:])
			writeBytes(dictionaryHash, payload)
			writeBytes(coldHash, payload)
			if retain {
				coldEntries[index] = coldEntry{payload: payload, fact: fact}
			}
		}
		hashesDigest := hex.EncodeToString(segmentHashes.Sum(nil))
		payloadsDigest := hex.EncodeToString(segmentPayloads.Sum(nil))
		if hashesDigest != segment.HashesDigest || payloadsDigest != segment.PayloadsDigest {
			return nil, fmt.Errorf("%w: cold segment roots", ErrDigestMismatch)
		}
		if retain {
			cold.segments[segment.ID] = coldEntries
		}
	}
	if reader.Len() != 0 || hex.EncodeToString(dictionaryHash.Sum(nil)) != manifest.DictionaryDigest ||
		hex.EncodeToString(coldHash.Sum(nil)) != manifest.ColdPayloadDigest {
		return nil, fmt.Errorf("%w: cold dictionary content", ErrDigestMismatch)
	}
	return cold, nil
}

func (d *HotDictionary) hotIndexDigest() string {
	hash := sha256.New()
	hash.Write([]byte(hotIndexDigestDomain))
	writeString(hash, d.DictionaryDigest())
	writeUint64(hash, uint64(len(d.manifest.Segments)))
	for _, segment := range d.manifest.Segments {
		writeString(hash, segment.ID)
		writeUint64(hash, uint64(len(d.hashes[segment.ID])))
		for _, value := range d.hashes[segment.ID] {
			_, _ = hash.Write(value[:])
		}
	}
	writeUint64(hash, uint64(len(d.rows)))
	for index, row := range d.rows {
		writeUint64(hash, uint64(index+1))
		rowSegmentID := d.rowSegment
		if rowSegmentID == "" {
			rowSegmentID = row.rowSegmentID
		}
		writeString(hash, rowSegmentID)
		writeUint32(hash, row.rowOrdinal)
		writeUint64(hash, uint64(len(d.fields)))
		for fieldIndex, field := range d.fields {
			writeString(hash, field.name)
			segmentID := field.segmentID
			if segmentID == "" {
				segmentID = row.cellSegmentIDs[fieldIndex]
			}
			writeString(hash, segmentID)
			writeUint32(hash, row.cellOrdinals[fieldIndex])
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (d *HotDictionary) segmentMatches(segmentID string, kind SegmentKind, field string) bool {
	if d == nil || segmentID == "" {
		return false
	}
	for _, segment := range d.manifest.Segments {
		if segment.ID == segmentID {
			return segment.Kind == kind && segment.Field == field
		}
	}
	return false
}

func (d *HotDictionary) sidecarDigest() string {
	hash := sha256.New()
	hash.Write([]byte(sidecarDigestDomain))
	writeUint64(hash, uint64(len(d.rows)))
	for index, row := range d.rows {
		writeUint64(hash, uint64(index+1))
		writeString(hash, row.entityKey)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func digestHashArray(hashes [][sha256.Size]byte) string {
	hash := sha256.New()
	hash.Write([]byte(segmentHashesDomain))
	writeUint64(hash, uint64(len(hashes)))
	for _, value := range hashes {
		_, _ = hash.Write(value[:])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func sealArtifact(domain string, body []byte) []byte {
	hash := sha256.New()
	hash.Write([]byte(domain))
	hash.Write(body)
	// bytes.Buffer owns body exclusively at both call sites. Appending the seal
	// in place avoids a second artifact-sized copy during offline publication;
	// append still allocates safely when the buffer has no spare capacity.
	return append(body, hash.Sum(nil)...)
}

func openArtifact(domain string, encoded []byte) ([]byte, error) {
	if len(encoded) < sha256.Size {
		return nil, fmt.Errorf("%w: truncated artifact", ErrInvalid)
	}
	body := encoded[:len(encoded)-sha256.Size]
	expected := encoded[len(encoded)-sha256.Size:]
	hash := sha256.New()
	hash.Write([]byte(domain))
	hash.Write(body)
	if !bytes.Equal(expected, hash.Sum(nil)) {
		return nil, ErrDigestMismatch
	}
	return body, nil
}

func decodeManifest(encoded []byte) (DictionaryManifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var manifest DictionaryManifest
	if err := decoder.Decode(&manifest); err != nil {
		return DictionaryManifest{}, fmt.Errorf("%w: decode manifest", ErrInvalid)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return DictionaryManifest{}, err
	}
	if err := manifest.Validate(); err != nil {
		return DictionaryManifest{}, err
	}
	return manifest, nil
}

func decodeFact(encoded []byte) (exposure.FactID, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var fact exposure.FactID
	if err := decoder.Decode(&fact); err != nil {
		return exposure.FactID{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return exposure.FactID{}, err
	}
	return fact, fact.Validate()
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON", ErrNonCanonical)
	}
	return nil
}

type artifactReader struct{ *bytes.Reader }

func (r artifactReader) readFixed(length int) ([]byte, error) {
	if length < 0 || length > r.Len() {
		return nil, fmt.Errorf("%w: truncated artifact field", ErrInvalid)
	}
	result := make([]byte, length)
	if _, err := io.ReadFull(r, result); err != nil {
		return nil, fmt.Errorf("%w: artifact field", ErrInvalid)
	}
	return result, nil
}

func (r artifactReader) readUint32() (uint32, error) {
	material, err := r.readFixed(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(material), nil
}

func (r artifactReader) readUint64() (uint64, error) {
	material, err := r.readFixed(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(material), nil
}

func (r artifactReader) readBytes(maximum int) ([]byte, error) {
	length, err := r.readUint64()
	if err != nil || length > uint64(maximum) || length > uint64(r.Len()) {
		return nil, fmt.Errorf("%w: artifact byte length", ErrInvalid)
	}
	return r.readFixed(int(length))
}

func (r artifactReader) readString(maximum int) (string, error) {
	material, err := r.readBytes(maximum)
	if err != nil {
		return "", err
	}
	return string(material), nil
}

// streamArtifactReader is the activation-path counterpart to artifactReader.
// It never creates a slice proportional to the complete artifact and limits
// every underlying Read request to artifactStreamBufferBytes.
type streamArtifactReader struct {
	reader    io.Reader
	remaining int64
	buffer    [artifactStreamBufferBytes]byte
}

func (r *streamArtifactReader) readFixed(length int) ([]byte, error) {
	if length < 0 || int64(length) > r.remaining {
		return nil, fmt.Errorf("%w: truncated artifact field", ErrInvalid)
	}
	result := make([]byte, length)
	for offset := 0; offset < length; {
		end := offset + artifactStreamBufferBytes
		if end > length {
			end = length
		}
		read, err := io.ReadFull(r.reader, result[offset:end])
		r.remaining -= int64(read)
		offset += read
		if err != nil {
			return nil, fmt.Errorf("%w: artifact field", ErrInvalid)
		}
	}
	return result, nil
}

func (r *streamArtifactReader) readUint64() (uint64, error) {
	material, err := r.readFixed(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(material), nil
}

func (r *streamArtifactReader) readBytes(maximum int) ([]byte, error) {
	length, err := r.readUint64()
	if err != nil || length > uint64(maximum) || length > uint64(r.remaining) || length > uint64(maxInt()) {
		return nil, fmt.Errorf("%w: artifact byte length", ErrInvalid)
	}
	return r.readFixed(int(length))
}

func (r *streamArtifactReader) readString(maximum int) (string, error) {
	material, err := r.readBytes(maximum)
	if err != nil {
		return "", err
	}
	return string(material), nil
}

func (r *streamArtifactReader) readHashedBytes(maximum int) (uint64, [sha256.Size]byte, error) {
	length, err := r.readUint64()
	if err != nil || length > uint64(maximum) || length > uint64(r.remaining) {
		return 0, [sha256.Size]byte{}, fmt.Errorf("%w: artifact byte length", ErrInvalid)
	}
	hash := sha256.New()
	left := length
	for left != 0 {
		chunk := uint64(len(r.buffer))
		if left < chunk {
			chunk = left
		}
		read, readErr := io.ReadFull(r.reader, r.buffer[:int(chunk)])
		r.remaining -= int64(read)
		left -= uint64(read)
		if read != 0 {
			_, _ = hash.Write(r.buffer[:read])
		}
		if readErr != nil {
			return 0, [sha256.Size]byte{}, fmt.Errorf("%w: artifact field", ErrInvalid)
		}
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return length, digest, nil
}

func (r *streamArtifactReader) discardRemaining() error {
	for r.remaining != 0 {
		chunk := int64(len(r.buffer))
		if r.remaining < chunk {
			chunk = r.remaining
		}
		read, err := io.ReadFull(r.reader, r.buffer[:int(chunk)])
		r.remaining -= int64(read)
		if err != nil {
			return fmt.Errorf("%w: artifact field", ErrInvalid)
		}
	}
	return nil
}

func maxInt() int { return int(^uint(0) >> 1) }
