package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ProfileBindingVersion identifies the matched-pair profile rule.
const ProfileBindingVersion = "taskgate-final-v5-profile-binding-v1"

var profileIDPattern = regexp.MustCompile(`^profile-[0-9a-f]{16}$`)

// PublicationSetVersion domain-separates the canonical Publication-set digest.
const PublicationSetVersion = "taskgate-final-v5-publication-set-v1"

// CanonicalPublicationSetSHA256 is the identity of a whole Publication closure.
// A profile routinely activates several Publications -- orders plus lineitem,
// for example -- so this is deliberately a digest over the canonically sorted,
// length-delimited set, never one Publication name. Reordering the input cannot
// change it; adding, dropping or renaming a member always does.
func CanonicalPublicationSetSHA256(publications []string) (string, error) {
	if len(publications) == 0 {
		return "", errors.New("a Publication set must contain at least one Publication")
	}
	sorted := append([]string(nil), publications...)
	sort.Strings(sorted)
	hash := sha256.New()
	hash.Write([]byte(PublicationSetVersion + "\x00"))
	fmt.Fprintf(hash, "%d\x00", len(sorted))
	previous := ""
	for index, name := range sorted {
		if strings.TrimSpace(name) == "" {
			return "", errors.New("a Publication set member is empty")
		}
		if index > 0 && name == previous {
			return "", fmt.Errorf("Publication %q appears twice in the set", name)
		}
		previous = name
		fmt.Fprintf(hash, "%d\x00%s\x00", len(name), name)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ProfileBinding is the deployment profile one arm of one workload cell ran
// against. A Direct arm never reaches the Gateway, but it still carries this
// identity: it must read the same Dataset, Catalog and Publication set as its
// paired BDG arm, and an unbound Direct arm would make that unprovable.
type ProfileBinding struct {
	Version       string `json:"version"`
	ProfileID     string `json:"profile_id"`
	ClosureSHA256 string `json:"closure_sha256"`
	CatalogSHA256 string `json:"catalog_sha256"`
	// DatasetBindingSHA256 identifies the external deployment Dataset Binding.
	DatasetBindingSHA256 string `json:"dataset_binding_sha256"`
	// PublicationIdentity is the canonical Publication-set SHA-256 produced by
	// CanonicalPublicationSetSHA256, not a Publication name. The JSON field name
	// is retained for compatibility with the v1.2 evidence schema.
	PublicationIdentity string `json:"publication_identity"`
}

// Equal compares every member of the binding. A partial comparison -- profile
// ID only, say -- would let two arms silently disagree on the Catalog or the
// Dataset they actually read.
func (binding ProfileBinding) Equal(other ProfileBinding) bool {
	return binding == other
}

// MismatchedFields lists exactly which members two bindings disagree on.
func (binding ProfileBinding) MismatchedFields(other ProfileBinding) []string {
	var fields []string
	for _, field := range []struct {
		name        string
		left, right string
	}{
		{"version", binding.Version, other.Version},
		{"profile_id", binding.ProfileID, other.ProfileID},
		{"closure_sha256", binding.ClosureSHA256, other.ClosureSHA256},
		{"catalog_sha256", binding.CatalogSHA256, other.CatalogSHA256},
		{"dataset_binding_sha256", binding.DatasetBindingSHA256, other.DatasetBindingSHA256},
		{"publication_identity", binding.PublicationIdentity, other.PublicationIdentity},
	} {
		if field.left != field.right {
			fields = append(fields, field.name)
		}
	}
	return fields
}

// Validate rejects a structurally incomplete binding.
func (binding ProfileBinding) Validate() error {
	if binding.Version != ProfileBindingVersion {
		return fmt.Errorf("profile binding version %q is unsupported", binding.Version)
	}
	if !profileIDPattern.MatchString(binding.ProfileID) {
		return fmt.Errorf("profile_id %q is not derived from a closure digest", binding.ProfileID)
	}
	// publication_identity is a canonical Publication-set digest, so it is held
	// to the same format as every other digest rather than to "non-empty".
	for name, digest := range map[string]string{
		"closure_sha256": binding.ClosureSHA256, "catalog_sha256": binding.CatalogSHA256,
		"dataset_binding_sha256": binding.DatasetBindingSHA256,
		"publication_identity":   binding.PublicationIdentity,
	} {
		if !validSHA256(digest) {
			return fmt.Errorf("profile binding %s is not SHA-256", name)
		}
	}
	return nil
}

// MatchedPairProfileFailure names one cell whose arms disagree.
type MatchedPairProfileFailure struct {
	CellID       string   `json:"cell_id"`
	PairedCellID string   `json:"paired_cell_id"`
	PairID       string   `json:"pair_id"`
	Field        string   `json:"field"`
	Values       []string `json:"values"`
}

func (failure MatchedPairProfileFailure) Error() string {
	return fmt.Sprintf("cells %s and %s in pair %s disagree on %s (%s)",
		failure.CellID, failure.PairedCellID, failure.PairID, failure.Field, strings.Join(failure.Values, " vs "))
}

// ValidateMatchedPairProfiles proves that every arm and repetition of one
// workload cell ran against the same deployment profile.
//
// A mismatch invalidates the whole cell rather than the offending arm: deleting
// one arm would leave a matched pair that silently compared two different
// Datasets, Catalogs or Publications. The failing samples are always retained.
func ValidateMatchedPairProfiles(samples []Sample) []MatchedPairProfileFailure {
	// A matched pair is identified by PairID and routinely spans two cells: a
	// Direct cell and its BDG cell. Grouping by cell would therefore compare an
	// arm only with itself and never detect the split it exists to catch.
	grouped := map[string][]Sample{}
	for _, sample := range samples {
		if sample.ProfileBinding == nil || strings.TrimSpace(sample.PairID) == "" {
			continue
		}
		grouped[sample.PairID] = append(grouped[sample.PairID], sample)
	}
	var failures []MatchedPairProfileFailure
	for _, pair := range sortedStringKeys(grouped) {
		group := grouped[pair]
		if len(group) < 2 {
			continue
		}
		first := *group[0].ProfileBinding
		for _, sample := range group[1:] {
			other := *sample.ProfileBinding
			for _, field := range first.MismatchedFields(other) {
				failures = append(failures, MatchedPairProfileFailure{
					CellID: group[0].CellID, PairedCellID: sample.CellID, PairID: pair,
					Field: field, Values: []string{fieldValue(first, field), fieldValue(other, field)}})
			}
		}
	}
	sort.Slice(failures, func(left, right int) bool {
		if failures[left].PairID != failures[right].PairID {
			return failures[left].PairID < failures[right].PairID
		}
		return failures[left].Field < failures[right].Field
	})
	return failures
}

func sortedStringKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// InvalidateMismatchedProfileCells marks every sample of a disagreeing pair
// invalid, including every cell that pair touches, and returns the affected
// cell IDs. Nothing is dropped: an invalid sample stays in the retained
// evidence with its reason, because deleting the offending arm would leave a
// pair that silently compared two different deployments.
func InvalidateMismatchedProfileCells(samples []Sample) ([]Sample, []string) {
	failures := ValidateMatchedPairProfiles(samples)
	if len(failures) == 0 {
		return samples, nil
	}
	invalidPairs := map[string]bool{}
	invalidCells := map[string]bool{}
	for _, failure := range failures {
		invalidPairs[failure.PairID] = true
		invalidCells[failure.CellID] = true
		invalidCells[failure.PairedCellID] = true
	}
	for index := range samples {
		if !invalidPairs[samples[index].PairID] && !invalidCells[samples[index].CellID] {
			continue
		}
		invalidCells[samples[index].CellID] = true
		samples[index].Status = "invalid"
		samples[index].ErrorCode = "matched_pair_profile_mismatch"
		samples[index].Reason = "the arms of this cell ran against different deployment profiles; " +
			"the whole cell is invalid and its evidence is retained"
	}
	cells := make([]string, 0, len(invalidCells))
	for cell := range invalidCells {
		cells = append(cells, cell)
	}
	sort.Strings(cells)
	return samples, cells
}

// RequireProfileBinding is the adapter-side gate: a sample that claims to have
// executed against a governed deployment must say which profile it used.
func RequireProfileBinding(sample Sample) error {
	if sample.ProfileBinding == nil {
		return errors.New("sample carries no deployment profile binding")
	}
	return sample.ProfileBinding.Validate()
}

// fieldValue reads one named member so a mismatch report can show both values
// without duplicating the field table.
func fieldValue(binding ProfileBinding, field string) string {
	switch field {
	case "version":
		return binding.Version
	case "profile_id":
		return binding.ProfileID
	case "closure_sha256":
		return binding.ClosureSHA256
	case "catalog_sha256":
		return binding.CatalogSHA256
	case "dataset_binding_sha256":
		return binding.DatasetBindingSHA256
	case "publication_identity":
		return binding.PublicationIdentity
	default:
		return ""
	}
}
