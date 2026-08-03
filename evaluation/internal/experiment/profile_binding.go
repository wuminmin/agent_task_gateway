package experiment

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ProfileBindingVersion identifies the matched-pair profile rule.
const ProfileBindingVersion = "taskgate-final-v5-profile-binding-v1"

var profileIDPattern = regexp.MustCompile(`^profile-[0-9a-f]{16}$`)

// ProfileBinding is the deployment profile one arm of one workload cell ran
// against. A Direct arm never reaches the Gateway, but it still carries this
// identity: it must read the same Dataset, Catalog and Publication as its
// paired BDG arm, and an unbound Direct arm would make that unprovable.
type ProfileBinding struct {
	Version              string `json:"version"`
	ProfileID            string `json:"profile_id"`
	ClosureSHA256        string `json:"closure_sha256"`
	CatalogSHA256        string `json:"catalog_sha256"`
	DatasetBindingSHA256 string `json:"dataset_binding_sha256"`
	PublicationIdentity  string `json:"publication_identity"`
}

// Validate rejects a structurally incomplete binding.
func (binding ProfileBinding) Validate() error {
	if binding.Version != ProfileBindingVersion {
		return fmt.Errorf("profile binding version %q is unsupported", binding.Version)
	}
	if !profileIDPattern.MatchString(binding.ProfileID) {
		return fmt.Errorf("profile_id %q is not derived from a closure digest", binding.ProfileID)
	}
	for name, digest := range map[string]string{
		"closure_sha256": binding.ClosureSHA256, "catalog_sha256": binding.CatalogSHA256,
		"dataset_binding_sha256": binding.DatasetBindingSHA256,
	} {
		if !validSHA256(digest) {
			return fmt.Errorf("profile binding %s is not SHA-256", name)
		}
	}
	if strings.TrimSpace(binding.PublicationIdentity) == "" {
		return errors.New("profile binding publication identity is empty")
	}
	return nil
}

// MatchedPairProfileFailure names one cell whose arms disagree.
type MatchedPairProfileFailure struct {
	CellID string   `json:"cell_id"`
	PairID string   `json:"pair_id"`
	Field  string   `json:"field"`
	Values []string `json:"values"`
}

func (failure MatchedPairProfileFailure) Error() string {
	return fmt.Sprintf("cell %s pair %s: arms disagree on %s (%s)",
		failure.CellID, failure.PairID, failure.Field, strings.Join(failure.Values, " vs "))
}

// ValidateMatchedPairProfiles proves that every arm and repetition of one
// workload cell ran against the same deployment profile.
//
// A mismatch invalidates the whole cell rather than the offending arm: deleting
// one arm would leave a matched pair that silently compared two different
// Datasets, Catalogs or Publications. The failing samples are always retained.
func ValidateMatchedPairProfiles(samples []Sample) []MatchedPairProfileFailure {
	type armKey struct{ cell, pair string }
	grouped := map[armKey][]Sample{}
	for _, sample := range samples {
		if sample.ProfileBinding == nil {
			continue
		}
		grouped[armKey{sample.CellID, sample.PairID}] = append(grouped[armKey{sample.CellID, sample.PairID}], sample)
	}
	var failures []MatchedPairProfileFailure
	for key, group := range grouped {
		if len(group) < 2 {
			continue
		}
		first := *group[0].ProfileBinding
		for _, sample := range group[1:] {
			other := *sample.ProfileBinding
			for _, field := range []struct {
				name        string
				left, right string
			}{
				{"profile_id", first.ProfileID, other.ProfileID},
				{"closure_sha256", first.ClosureSHA256, other.ClosureSHA256},
				{"catalog_sha256", first.CatalogSHA256, other.CatalogSHA256},
				{"dataset_binding_sha256", first.DatasetBindingSHA256, other.DatasetBindingSHA256},
				{"publication_identity", first.PublicationIdentity, other.PublicationIdentity},
			} {
				if field.left == field.right {
					continue
				}
				failures = append(failures, MatchedPairProfileFailure{CellID: key.cell, PairID: key.pair,
					Field: field.name, Values: []string{field.left, field.right}})
			}
		}
	}
	sort.Slice(failures, func(left, right int) bool {
		if failures[left].CellID != failures[right].CellID {
			return failures[left].CellID < failures[right].CellID
		}
		if failures[left].PairID != failures[right].PairID {
			return failures[left].PairID < failures[right].PairID
		}
		return failures[left].Field < failures[right].Field
	})
	return failures
}

// InvalidateMismatchedProfileCells marks every sample of a disagreeing cell
// invalid and returns the affected cell IDs. Nothing is dropped: an invalid
// sample stays in the retained evidence with its reason.
func InvalidateMismatchedProfileCells(samples []Sample) ([]Sample, []string) {
	failures := ValidateMatchedPairProfiles(samples)
	if len(failures) == 0 {
		return samples, nil
	}
	invalid := map[string]bool{}
	for _, failure := range failures {
		invalid[failure.CellID] = true
	}
	for index := range samples {
		if !invalid[samples[index].CellID] {
			continue
		}
		samples[index].Status = "invalid"
		samples[index].ErrorCode = "matched_pair_profile_mismatch"
		samples[index].Reason = "the arms of this cell ran against different deployment profiles; " +
			"the whole cell is invalid and its evidence is retained"
	}
	cells := make([]string, 0, len(invalid))
	for cell := range invalid {
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
