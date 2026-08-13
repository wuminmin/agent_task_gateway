package exposure

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
)

const outcomeDigestDomainV1 = "TASKGATE-OUTCOME-DIGEST-V1\x00"

// AttachOutcomeV3 upgrades a normalized V2 observation into V3 by adding one
// proposition-bearing fact. The result digest is deliberately independent of
// row order and provenance multiplicity because it is computed from the
// normalized release fact set plus the visible row count.
func AttachOutcomeV3(observation Observation, normalFormVersion, normalFormSHA256 string, visibleRows int64) (Observation, error) {
	return attachOutcome(observation, ProfileV3, normalFormVersion, normalFormSHA256, visibleRows)
}

// AttachOutcomeV4 produces the same proposition-bearing V3 FactID while
// marking the enclosing observation for the V4 ordinal/bitmap settlement
// backend. Keeping the fact payload unchanged is what makes Decode(V4)
// directly comparable with the V3 FactSet oracle.
func AttachOutcomeV4(observation Observation, normalFormVersion, normalFormSHA256 string, visibleRows int64) (Observation, error) {
	return attachOutcome(observation, ProfileV4, normalFormVersion, normalFormSHA256, visibleRows)
}

func attachOutcome(observation Observation, targetProfile, normalFormVersion, normalFormSHA256 string, visibleRows int64) (Observation, error) {
	if observation.ProfileVersion != ProfileV2 {
		return Observation{}, fmt.Errorf("%w: outcome attachment requires a V2 observation", ErrInvalid)
	}
	if targetProfile != ProfileV3 && targetProfile != ProfileV4 {
		return Observation{}, fmt.Errorf("%w: unsupported outcome observation profile %q", ErrInvalid, targetProfile)
	}
	if visibleRows < 0 {
		return Observation{}, fmt.Errorf("%w: visible row count cannot be negative", ErrInvalid)
	}
	normalized, err := observation.Normalize()
	if err != nil {
		return Observation{}, err
	}
	digest, err := ReleaseOutcomeDigest(normalized.Release, visibleRows)
	if err != nil {
		return Observation{}, err
	}
	fact, err := NewOutcomeFactV3(normalFormVersion, normalFormSHA256, digest, visibleRows)
	if err != nil {
		return Observation{}, err
	}
	return (Observation{
		ProfileVersion: targetProfile,
		Release:        normalized.Release,
		Influence:      normalized.Influence,
		Outcome:        []FactID{fact},
	}).Normalize()
}

// ReleaseOutcomeDigest commits to the exact normalized release fact set and
// visible cardinality. The separate normal-form commitment in FactOutcome
// prevents different predicates with the same empty or zero result from
// collapsing to the same charged fact.
//
// The committed byte sequence is unchanged, and so is every digest this has
// ever produced: domain, visible rows, unique fact count, then each fact's
// length-prefixed canonical payload in ascending hash order.
//
// Only the cost of producing it changed. This used to rebuild a whole FactSet
// from a slice that the hot caller had already drained from one, materialize a
// second sorted []FactID, and concatenate every canonical payload into one
// bytes.Buffer before hashing -- so the drain transient measured about 3.9x the
// resident set, which made it the dominant cost of wide-result accounting
// rather than the accumulation everyone assumed. It now sorts a compact index
// and streams into sha256, holding no duplicate of the facts and no whole
// payload at all.
//
// Dedup semantics are deliberately identical, because callers do pass slices
// that are not already sets: repeated hashes still collapse to one entry and
// still fail closed when two facts share a hash but not a canonical payload.
func ReleaseOutcomeDigest(release []FactID, visibleRows int64) (string, error) {
	if visibleRows < 0 {
		return "", fmt.Errorf("%w: visible row count cannot be negative", ErrInvalid)
	}
	// Sorting the decoded digests orders identically to sorting their lowercase
	// hex, because hex is monotonic in nibble value; FactSet.Values sorts the
	// hex, and the differential test pins the two orders together.
	type indexedFact struct {
		hash  [sha256.Size]byte
		index int
	}
	indexed := make([]indexedFact, 0, len(release))
	for position, fact := range release {
		hashText, err := fact.Hash()
		if err != nil {
			return "", err
		}
		raw, err := hex.DecodeString(hashText)
		if err != nil || len(raw) != sha256.Size {
			return "", fmt.Errorf("%w: fact hash is not a SHA-256 digest", ErrInvalid)
		}
		entry := indexedFact{index: position}
		copy(entry.hash[:], raw)
		indexed = append(indexed, entry)
	}
	// Ties keep the earliest occurrence, matching FactSet.Add, which never
	// overwrites an already-present hash.
	sort.Slice(indexed, func(i, j int) bool {
		if order := bytes.Compare(indexed[i].hash[:], indexed[j].hash[:]); order != 0 {
			return order < 0
		}
		return indexed[i].index < indexed[j].index
	})

	unique := 0
	for position := range indexed {
		if position > 0 && indexed[position].hash == indexed[position-1].hash {
			continue
		}
		unique++
	}

	digestState := sha256.New()
	digestState.Write([]byte(outcomeDigestDomainV1))
	writeCanonicalUint64Stream(digestState, uint64(visibleRows))
	writeCanonicalUint64Stream(digestState, uint64(unique))
	var previousPayload []byte
	for position, entry := range indexed {
		fact := release[entry.index]
		if !fact.IsV2() {
			return "", fmt.Errorf("%w: outcome release set contains a non-V2 fact", ErrInvalid)
		}
		payload, err := fact.CanonicalPayload()
		if err != nil {
			return "", err
		}
		if position > 0 && entry.hash == indexed[position-1].hash {
			// A repeated hash is only a duplicate if the canonical payload also
			// matches; otherwise it is a collision and must fail closed.
			if !bytes.Equal(previousPayload, payload) {
				return "", fmt.Errorf("%w: fact hash collision for %s", ErrInvalid,
					hex.EncodeToString(entry.hash[:]))
			}
			continue
		}
		writeCanonicalUint64Stream(digestState, uint64(len(payload)))
		digestState.Write(payload)
		previousPayload = payload
	}
	return hex.EncodeToString(digestState.Sum(nil)), nil
}

func writeCanonicalUint64Stream(w io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = w.Write(encoded[:])
}
