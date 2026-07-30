package exposure

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
func ReleaseOutcomeDigest(release []FactID, visibleRows int64) (string, error) {
	if visibleRows < 0 {
		return "", fmt.Errorf("%w: visible row count cannot be negative", ErrInvalid)
	}
	set, err := NewFactSet(release...)
	if err != nil {
		return "", err
	}
	var payload bytes.Buffer
	payload.WriteString(outcomeDigestDomainV1)
	writeCanonicalUint64(&payload, uint64(visibleRows))
	values := set.Values()
	writeCanonicalUint64(&payload, uint64(len(values)))
	for _, fact := range values {
		if !fact.IsV2() {
			return "", fmt.Errorf("%w: outcome release set contains a non-V2 fact", ErrInvalid)
		}
		factPayload, err := fact.CanonicalPayload()
		if err != nil {
			return "", err
		}
		writeCanonicalBytes(&payload, factPayload)
	}
	digest := sha256.Sum256(payload.Bytes())
	return hex.EncodeToString(digest[:]), nil
}

func writeCanonicalBytes(w *bytes.Buffer, value []byte) {
	writeCanonicalUint64(w, uint64(len(value)))
	w.Write(value)
}
