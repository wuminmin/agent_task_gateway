package exposure

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ObservationDigest returns a stable digest of a normalized observation.
func ObservationDigest(observation Observation) (string, error) {
	normalized, err := observation.Normalize()
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
