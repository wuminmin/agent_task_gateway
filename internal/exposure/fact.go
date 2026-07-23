// Package exposure defines TaskGate's task-bound data exposure semantics.
package exposure

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrInvalid = errors.New("invalid exposure value")

const RowExistenceField = "$row"

// FactID names one versioned fact independently of the query that revealed it.
// Stable identity is what makes retries, pagination, and equivalent rewrites
// consume a fact at most once within a root task family.
type FactID struct {
	Product      string `json:"product"`
	Snapshot     string `json:"snapshot"`
	EntityKey    string `json:"entity_key"`
	Field        string `json:"field"`
	ValueVersion string `json:"value_version"`
}

func (f FactID) Validate() error {
	for name, value := range map[string]string{
		"product": f.Product, "snapshot": f.Snapshot, "entity_key": f.EntityKey,
		"field": f.Field, "value_version": f.ValueVersion,
	} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("%w: %s is required without boundary whitespace", ErrInvalid, name)
		}
	}
	if len(f.ValueVersion) != sha256.Size*2 {
		return fmt.Errorf("%w: value_version must be a SHA-256 digest", ErrInvalid)
	}
	if _, err := hex.DecodeString(f.ValueVersion); err != nil {
		return fmt.Errorf("%w: value_version must be lowercase hexadecimal", ErrInvalid)
	}
	if strings.ToLower(f.ValueVersion) != f.ValueVersion {
		return fmt.Errorf("%w: value_version must be lowercase hexadecimal", ErrInvalid)
	}
	return nil
}

// Hash returns the durable identity used by the PostgreSQL task-family ledger.
func (f FactID) Hash() (string, error) {
	if err := f.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("%w: encode fact: %v", ErrInvalid, err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ValueVersion hashes the typed JSON representation of a released value.
func ValueVersion(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("%w: value is not canonical JSON: %v", ErrInvalid, err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ComposeKey creates a stable, typed entity key from one or more key values.
func ComposeKey(values ...any) (string, error) {
	if len(values) == 0 {
		return "", fmt.Errorf("%w: entity key requires at least one value", ErrInvalid)
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("%w: encode entity key: %v", ErrInvalid, err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func NewFact(product, snapshot, entityKey, field string, value any) (FactID, error) {
	version, err := ValueVersion(value)
	if err != nil {
		return FactID{}, err
	}
	fact := FactID{Product: product, Snapshot: snapshot, EntityKey: entityKey, Field: field, ValueVersion: version}
	if err := fact.Validate(); err != nil {
		return FactID{}, err
	}
	return fact, nil
}

// FactSet is a set keyed by canonical fact hash. It deliberately retains the
// full identity so settlements remain independently auditable.
type FactSet map[string]FactID

func NewFactSet(facts ...FactID) (FactSet, error) {
	result := make(FactSet, len(facts))
	for _, fact := range facts {
		if err := result.Add(fact); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s FactSet) Add(fact FactID) error {
	if s == nil {
		return fmt.Errorf("%w: nil fact set", ErrInvalid)
	}
	hash, err := fact.Hash()
	if err != nil {
		return err
	}
	s[hash] = fact
	return nil
}

func (s FactSet) Clone() FactSet {
	result := make(FactSet, len(s))
	for hash, fact := range s {
		result[hash] = fact
	}
	return result
}

func (s FactSet) Merge(other FactSet) {
	for hash, fact := range other {
		s[hash] = fact
	}
}

func (s FactSet) Values() []FactID {
	hashes := make([]string, 0, len(s))
	for hash := range s {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	result := make([]FactID, 0, len(hashes))
	for _, hash := range hashes {
		result = append(result, s[hash])
	}
	return result
}

// Observation is the dual ledger effect of one buffered query result.
type Observation struct {
	ProfileVersion string   `json:"profile_version"`
	Release        []FactID `json:"release"`
	Influence      []FactID `json:"influence"`
}

func (o Observation) Normalize() (Observation, error) {
	if strings.TrimSpace(o.ProfileVersion) == "" || o.ProfileVersion != strings.TrimSpace(o.ProfileVersion) {
		return Observation{}, fmt.Errorf("%w: profile_version is required", ErrInvalid)
	}
	release, err := NewFactSet(o.Release...)
	if err != nil {
		return Observation{}, err
	}
	influence, err := NewFactSet(o.Influence...)
	if err != nil {
		return Observation{}, err
	}
	return Observation{ProfileVersion: o.ProfileVersion, Release: release.Values(), Influence: influence.Values()}, nil
}

func MergeObservations(profile string, observations ...Observation) (Observation, error) {
	release := make(FactSet)
	influence := make(FactSet)
	for _, observation := range observations {
		normalized, err := observation.Normalize()
		if err != nil {
			return Observation{}, err
		}
		if normalized.ProfileVersion != profile {
			return Observation{}, fmt.Errorf("%w: mixed exposure profiles", ErrInvalid)
		}
		set, _ := NewFactSet(normalized.Release...)
		release.Merge(set)
		set, _ = NewFactSet(normalized.Influence...)
		influence.Merge(set)
	}
	return Observation{ProfileVersion: profile, Release: release.Values(), Influence: influence.Values()}, nil
}
