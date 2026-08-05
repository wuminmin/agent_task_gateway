package physicalquery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// PreparedOperationV1Version identifies the shared preparation contract.
const PreparedOperationV1Version = "taskgate-prepared-operation-v1"

// preparedOperationDomain domain-separates the prepared bindings.
const preparedOperationDomain = "TASKGATE-PREPARED-OPERATION-V1"

// TargetRole names one of the two statements an operation prepares.
type TargetRole string

const (
	RoleVisible   TargetRole = "visible"
	RoleCompanion TargetRole = "companion"
)

// PreparedOperationV1 is the deterministic result of preparing one operation:
// the statements it will execute, the metadata execution needs, and the
// content-addressed identities that let two independent preparations be compared
// without either seeing the other's SQL.
//
// The SQL fields carry `json:"-"`. They are in-memory only and must never enter
// durable evidence, a sample, a log or a receipt; what travels is the bindings.
// Everything that does serialize is SQL-free by construction.
type PreparedOperationV1 struct {
	Version string `json:"version"`

	// VisibleSQL and CompanionSQL are pre-policy: they are what the policy
	// engine is asked to authorize, not yet what it renders. The row limits are
	// applied afterwards by physicalquery, from signed pre-state.
	VisibleSQL   string `json:"-"`
	CompanionSQL string `json:"-"`

	// HasCompanion distinguishes "no companion" from "a companion whose SQL
	// happens to be empty". A caller must never infer one from the other.
	HasCompanion bool `json:"has_companion"`

	// VisibleFields, FactFields and ProvenanceFields are the projections
	// execution and observation depend on.
	VisibleFields    []string `json:"visible_fields"`
	FactFields       []string `json:"fact_fields"`
	ProvenanceFields []string `json:"provenance_fields,omitempty"`

	// Grouped and ExpandedEvidence settle how the companion's row budget is
	// derived. They are prepared rather than inferred at execution time, because
	// physicalquery.DeriveLimits reads them and the finalizer must reach the same
	// limits from the same values.
	Grouped          bool `json:"grouped"`
	ExpandedEvidence bool `json:"expanded_evidence"`

	// The identities. Each is a digest over material that is already SQL-free,
	// or over SQL that is hashed rather than carried.
	PlanDigest                  string `json:"plan_digest"`
	CatalogDigest               string `json:"catalog_digest"`
	CompilerIdentity            string `json:"compiler_identity"`
	OrdinalProgramDigest        string `json:"ordinal_program_digest,omitempty"`
	DictionarySetDigest         string `json:"dictionary_set_digest,omitempty"`
	SidecarGrantsDigest         string `json:"sidecar_grants_digest,omitempty"`
	ViewBindingDigest           string `json:"view_binding_digest,omitempty"`
	ViewRegistryRevision        string `json:"view_registry_revision,omitempty"`
	PredicateFootprintIdentity  string `json:"predicate_footprint_identity,omitempty"`
	PreparedOperationSHA256     string `json:"prepared_operation_sha256"`
	PreparedVisibleTargetSHA256 string `json:"prepared_visible_target_sha256"`
	// PreparedCompanionTargetSHA256 is empty exactly when HasCompanion is false.
	PreparedCompanionTargetSHA256 string `json:"prepared_companion_target_sha256,omitempty"`
}

// Validate rejects a prepared operation that cannot be compared or executed.
func (prepared PreparedOperationV1) Validate() error {
	if prepared.Version != PreparedOperationV1Version {
		return fmt.Errorf("prepared operation version %q is unsupported", prepared.Version)
	}
	if strings.TrimSpace(prepared.VisibleSQL) == "" {
		return errors.New("prepared operation carries no visible statement")
	}
	// Presence-coupled in both directions, so a companion cannot be half present.
	if prepared.HasCompanion == (strings.TrimSpace(prepared.CompanionSQL) == "") {
		return errors.New("prepared operation's companion statement and its presence flag disagree")
	}
	if prepared.HasCompanion == (prepared.PreparedCompanionTargetSHA256 == "") {
		return errors.New("prepared operation's companion binding and its presence flag disagree")
	}
	if len(prepared.VisibleFields) == 0 {
		return errors.New("prepared operation projects no visible field")
	}
	for name, digest := range map[string]string{
		"plan digest":             prepared.PlanDigest,
		"catalog digest":          prepared.CatalogDigest,
		"compiler identity":       prepared.CompilerIdentity,
		"prepared operation":      prepared.PreparedOperationSHA256,
		"prepared visible target": prepared.PreparedVisibleTargetSHA256,
	} {
		if !validSHA256(digest) && name != "compiler identity" {
			return fmt.Errorf("prepared operation %s is not a lowercase SHA-256", name)
		}
		if strings.TrimSpace(digest) == "" {
			return fmt.Errorf("prepared operation carries no %s", name)
		}
	}
	// Expanded evidence is a property of a companion-bearing operation. Without
	// one there is no evidence row budget to expand.
	if prepared.ExpandedEvidence && !prepared.HasCompanion {
		return errors.New("prepared operation claims expanded evidence but prepares no companion statement")
	}
	return nil
}

// CarriesSQL reports whether any serialized member would leak statement text.
//
// It is a method rather than a test helper because it is the property durable
// evidence depends on, and the observer, the Sample and the receipt all assert
// it. A future member added to the wrong side of the `json:"-"` line is caught
// here rather than in whatever file it eventually reaches.
func (prepared PreparedOperationV1) CarriesSQL() (bool, error) {
	encoded, err := json.Marshal(prepared)
	if err != nil {
		return false, err
	}
	text := strings.ToLower(string(encoded))
	for _, token := range []string{"select ", " from ", " where ", " join ", "set_config", "pg_catalog"} {
		if strings.Contains(text, token) {
			return true, nil
		}
	}
	return false, nil
}

// TargetSHA256 is one prepared target's binding.
func (prepared PreparedOperationV1) TargetSHA256(role TargetRole) (string, error) {
	switch role {
	case RoleVisible:
		return prepared.PreparedVisibleTargetSHA256, nil
	case RoleCompanion:
		if !prepared.HasCompanion {
			return "", errors.New("this operation prepares no companion statement")
		}
		return prepared.PreparedCompanionTargetSHA256, nil
	default:
		return "", fmt.Errorf("%q is not a prepared target role", role)
	}
}

// RequireSame compares two independent preparations of what should be one
// operation, and names every member they disagree on.
//
// This is what the finalizer uses: it prepares from its own frozen inputs and
// requires the result to equal what the Gateway signed. The SQL is compared by
// digest rather than by text, so a rejection can be reported without carrying a
// statement into the failure message.
func (prepared PreparedOperationV1) RequireSame(other PreparedOperationV1) error {
	if err := prepared.Validate(); err != nil {
		return fmt.Errorf("this preparation: %w", err)
	}
	if err := other.Validate(); err != nil {
		return fmt.Errorf("the other preparation: %w", err)
	}
	var differences []string
	for name, pair := range map[string][2]string{
		"visible statement":            {sqlDigest(prepared.VisibleSQL), sqlDigest(other.VisibleSQL)},
		"companion statement":          {sqlDigest(prepared.CompanionSQL), sqlDigest(other.CompanionSQL)},
		"plan digest":                  {prepared.PlanDigest, other.PlanDigest},
		"catalog digest":               {prepared.CatalogDigest, other.CatalogDigest},
		"compiler identity":            {prepared.CompilerIdentity, other.CompilerIdentity},
		"ordinal program digest":       {prepared.OrdinalProgramDigest, other.OrdinalProgramDigest},
		"dictionary set digest":        {prepared.DictionarySetDigest, other.DictionarySetDigest},
		"sidecar grants digest":        {prepared.SidecarGrantsDigest, other.SidecarGrantsDigest},
		"view binding digest":          {prepared.ViewBindingDigest, other.ViewBindingDigest},
		"view registry revision":       {prepared.ViewRegistryRevision, other.ViewRegistryRevision},
		"predicate footprint identity": {prepared.PredicateFootprintIdentity, other.PredicateFootprintIdentity},
		"prepared operation binding":   {prepared.PreparedOperationSHA256, other.PreparedOperationSHA256},
		"prepared visible target":      {prepared.PreparedVisibleTargetSHA256, other.PreparedVisibleTargetSHA256},
		"prepared companion target":    {prepared.PreparedCompanionTargetSHA256, other.PreparedCompanionTargetSHA256},
	} {
		if pair[0] != pair[1] {
			differences = append(differences, name)
		}
	}
	if prepared.HasCompanion != other.HasCompanion {
		differences = append(differences, "companion presence")
	}
	if prepared.Grouped != other.Grouped {
		differences = append(differences, "grouped")
	}
	if prepared.ExpandedEvidence != other.ExpandedEvidence {
		differences = append(differences, "expanded evidence")
	}
	for name, pair := range map[string][2][]string{
		"visible fields":    {prepared.VisibleFields, other.VisibleFields},
		"fact fields":       {prepared.FactFields, other.FactFields},
		"provenance fields": {prepared.ProvenanceFields, other.ProvenanceFields},
	} {
		if !sameStrings(pair[0], pair[1]) {
			differences = append(differences, name)
		}
	}
	if len(differences) == 0 {
		return nil
	}
	sort.Strings(differences)
	return fmt.Errorf("two independent preparations of one operation disagree on %v", differences)
}

// sqlDigest hashes statement bytes so two preparations can be compared, and
// reported on, without either statement entering the comparison's output.
func sqlDigest(sql string) string {
	if sql == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'f':
		default:
			return false
		}
	}
	return true
}
