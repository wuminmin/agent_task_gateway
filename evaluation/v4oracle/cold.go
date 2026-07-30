package v4oracle

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
)

const (
	coldMagic                 = "TGCOLD1\x00"
	coldArtifactDomain        = "TASKGATE-ORDINAL-COLD-ARTIFACT-V1\x00"
	dictionaryDigestDomain    = "TASKGATE-ORDINAL-DICTIONARY-V1\x00"
	coldPayloadDigestDomain   = "TASKGATE-ORDINAL-COLD-PAYLOAD-V1\x00"
	segmentHashesDomain       = "TASKGATE-ORDINAL-SEGMENT-HASHES-V1\x00"
	segmentPayloadsDomain     = "TASKGATE-ORDINAL-SEGMENT-PAYLOADS-V1\x00"
	maximumManifestBytes      = uint64(4 << 20)
	maximumCanonicalFactBytes = uint64(1 << 20)
	maximumColdArtifactBytes  = int64(2 << 30)
)

type coldFact struct {
	Ref     ordinal.FactRef
	Fact    exposure.FactID
	Hash    [sha256.Size]byte
	Payload []byte
}

type coldScan struct {
	Manifest ordinal.DictionaryManifest
	SHA256   string
	Facts    uint64
}

// scanColdDictionary is an evaluation-owned parser, intentionally separate
// from ordinal.ParseColdDictionary and the Gateway registry. It expands one
// bounded FactID at a time, recomputes all semantic roots, verifies the sealed
// transport, and never materializes a dictionary-sized Go object graph.
func scanColdDictionary(path, expectedManifest, expectedDictionary, expectedFile string,
	yield func(coldFact) error) (coldScan, error) {
	var result coldScan
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= sha256.Size ||
		info.Size() > maximumColdArtifactBytes {
		return result, errors.New("COLD artifact is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	bodyBytes := info.Size() - sha256.Size
	limited := &io.LimitedReader{R: file, N: bodyBytes}
	bodyHash := sha256.New()
	_, _ = bodyHash.Write([]byte(coldArtifactDomain))
	fileHash := sha256.New()
	reader := &coldReader{source: io.TeeReader(limited, io.MultiWriter(bodyHash, fileHash)), remaining: bodyBytes}

	magic, err := reader.fixed(uint64(len(coldMagic)))
	if err != nil || string(magic) != coldMagic {
		return result, errors.New("invalid COLD magic")
	}
	manifestRaw, err := reader.bytes(maximumManifestBytes)
	if err != nil {
		return result, err
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestRaw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result.Manifest); err != nil || requireJSONEOF(decoder) != nil || result.Manifest.Validate() != nil {
		return result, errors.New("invalid COLD dictionary manifest")
	}
	storedManifest, err := reader.string(sha256.Size * 2)
	manifestDigest, digestErr := result.Manifest.Digest()
	if err != nil || digestErr != nil || storedManifest != expectedManifest || manifestDigest != expectedManifest ||
		result.Manifest.DictionaryDigest != expectedDictionary {
		return result, errors.New("COLD manifest/dictionary binding mismatch")
	}
	segmentCount, err := reader.uint64()
	if err != nil || segmentCount != uint64(len(result.Manifest.Segments)) {
		return result, errors.New("COLD segment count mismatch")
	}
	dictionaryHash := sha256.New()
	_, _ = dictionaryHash.Write([]byte(dictionaryDigestDomain))
	writeUint64Hash(dictionaryHash, segmentCount)
	coldHash := sha256.New()
	_, _ = coldHash.Write([]byte(coldPayloadDigestDomain))
	writeUint64Hash(coldHash, segmentCount)
	for _, segment := range result.Manifest.Segments {
		segmentID, idErr := reader.string(256)
		count, countErr := reader.uint64()
		if idErr != nil || countErr != nil || segmentID != segment.ID || count != segment.FactCount {
			return result, errors.New("COLD segment metadata mismatch")
		}
		segmentHash := sha256.New()
		_, _ = segmentHash.Write([]byte(segmentHashesDomain))
		writeUint64Hash(segmentHash, count)
		segmentPayload := sha256.New()
		_, _ = segmentPayload.Write([]byte(segmentPayloadsDomain))
		writeUint64Hash(segmentPayload, count)
		writeStringHash(dictionaryHash, segment.ID)
		writeStringHash(dictionaryHash, string(segment.Kind))
		writeStringHash(dictionaryHash, segment.Field)
		writeUint16Hash(dictionaryHash, segment.Shard)
		writeUint64Hash(dictionaryHash, count)
		writeStringHash(coldHash, segment.ID)
		writeUint64Hash(coldHash, count)
		for index := uint64(0); index < count; index++ {
			payload, payloadErr := reader.bytes(maximumCanonicalFactBytes)
			factJSON, factErr := reader.bytes(maximumCanonicalFactBytes)
			if payloadErr != nil || factErr != nil {
				return result, errors.New("truncated COLD fact")
			}
			var fact exposure.FactID
			factDecoder := json.NewDecoder(bytes.NewReader(factJSON))
			factDecoder.DisallowUnknownFields()
			if err := factDecoder.Decode(&fact); err != nil || requireJSONEOF(factDecoder) != nil {
				return result, errors.New("invalid COLD FactID JSON")
			}
			canonical, canonicalErr := fact.CanonicalPayload()
			hashText, hashErr := fact.Hash()
			hashBytes, decodeErr := hex.DecodeString(hashText)
			if canonicalErr != nil || hashErr != nil || decodeErr != nil || len(hashBytes) != sha256.Size || !bytes.Equal(canonical, payload) {
				return result, errors.New("COLD FactHash/canonical payload mismatch")
			}
			var factHash [sha256.Size]byte
			copy(factHash[:], hashBytes)
			_, _ = segmentHash.Write(factHash[:])
			writeBytesHash(segmentPayload, canonical)
			_, _ = dictionaryHash.Write(factHash[:])
			writeBytesHash(dictionaryHash, canonical)
			writeBytesHash(coldHash, canonical)
			if yield != nil {
				if index > uint64(^uint32(0)) {
					return result, errors.New("COLD ordinal exceeds uint32")
				}
				if err := yield(coldFact{Ref: ordinal.FactRef{DictionaryDigest: expectedDictionary,
					SegmentID: segment.ID, Ordinal: uint32(index)}, Fact: fact, Hash: factHash, Payload: canonical}); err != nil {
					return result, err
				}
			}
			result.Facts++
		}
		if hex.EncodeToString(segmentHash.Sum(nil)) != segment.HashesDigest ||
			hex.EncodeToString(segmentPayload.Sum(nil)) != segment.PayloadsDigest {
			return result, errors.New("COLD segment root mismatch")
		}
	}
	if reader.remaining != 0 || limited.N != 0 {
		return result, errors.New("trailing COLD body bytes")
	}
	var seal [sha256.Size]byte
	if _, err := io.ReadFull(file, seal[:]); err != nil || !bytes.Equal(seal[:], bodyHash.Sum(nil)) {
		return result, errors.New("COLD transport seal mismatch")
	}
	_, _ = fileHash.Write(seal[:])
	var extra [1]byte
	if count, err := file.Read(extra[:]); count != 0 || !errors.Is(err, io.EOF) {
		return result, errors.New("COLD artifact size changed")
	}
	result.SHA256 = hex.EncodeToString(fileHash.Sum(nil))
	if result.SHA256 != expectedFile || hex.EncodeToString(dictionaryHash.Sum(nil)) != result.Manifest.DictionaryDigest ||
		hex.EncodeToString(coldHash.Sum(nil)) != result.Manifest.ColdPayloadDigest {
		return result, errors.New("COLD artifact content digest mismatch")
	}
	return result, nil
}

type coldReader struct {
	source    io.Reader
	remaining int64
}

func (r *coldReader) fixed(length uint64) ([]byte, error) {
	if length > uint64(r.remaining) || length > uint64(^uint(0)>>1) {
		return nil, io.ErrUnexpectedEOF
	}
	value := make([]byte, int(length))
	if _, err := io.ReadFull(r.source, value); err != nil {
		return nil, err
	}
	r.remaining -= int64(length)
	return value, nil
}

func (r *coldReader) uint64() (uint64, error) {
	value, err := r.fixed(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}

func (r *coldReader) bytes(maximum uint64) ([]byte, error) {
	length, err := r.uint64()
	if err != nil || length > maximum {
		return nil, fmt.Errorf("invalid COLD length")
	}
	return r.fixed(length)
}

func (r *coldReader) string(maximum int) (string, error) {
	value, err := r.bytes(uint64(maximum))
	return string(value), err
}

func writeUint16Hash(target hash.Hash, value uint16) {
	var encoded [2]byte
	binary.BigEndian.PutUint16(encoded[:], value)
	_, _ = target.Write(encoded[:])
}

func writeUint64Hash(target hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = target.Write(encoded[:])
}

func writeBytesHash(target hash.Hash, value []byte) {
	writeUint64Hash(target, uint64(len(value)))
	_, _ = target.Write(value)
}

func writeStringHash(target hash.Hash, value string) { writeBytesHash(target, []byte(value)) }
