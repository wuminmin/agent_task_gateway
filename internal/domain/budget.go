package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidBudget   = errors.New("invalid budget")
	ErrBudgetExpansion = errors.New("budget expansion")
)

// Budget contains all hard limits attached to a task grant.
type Budget struct {
	MaxQueries             int64         `json:"max_queries" yaml:"max_queries"`
	MaxRows                int64         `json:"max_rows" yaml:"max_rows"`
	MaxDBTime              time.Duration `json:"max_db_time" yaml:"max_db_time"`
	PerQueryTimeout        time.Duration `json:"per_query_timeout" yaml:"per_query_timeout"`
	TaskTTL                time.Duration `json:"task_ttl" yaml:"task_ttl"`
	MaxReleaseFacts        int64         `json:"max_release_facts,omitempty" yaml:"max_release_facts,omitempty"`
	MaxInfluenceFacts      int64         `json:"max_influence_facts,omitempty" yaml:"max_influence_facts,omitempty"`
	MaxOutcomeFacts        int64         `json:"max_outcome_facts,omitempty" yaml:"max_outcome_facts,omitempty"`
	ExposureProfileVersion string        `json:"exposure_profile_version,omitempty" yaml:"exposure_profile_version,omitempty"`
}

func (b Budget) Validate() error {
	switch {
	case b.MaxQueries <= 0:
		return fmt.Errorf("%w: max_queries must be positive", ErrInvalidBudget)
	case b.MaxRows <= 0:
		return fmt.Errorf("%w: max_rows must be positive", ErrInvalidBudget)
	case b.MaxDBTime <= 0:
		return fmt.Errorf("%w: max_db_time must be positive", ErrInvalidBudget)
	case b.PerQueryTimeout <= 0:
		return fmt.Errorf("%w: per_query_timeout must be positive", ErrInvalidBudget)
	case b.PerQueryTimeout > b.MaxDBTime:
		return fmt.Errorf("%w: per_query_timeout exceeds max_db_time", ErrInvalidBudget)
	case b.TaskTTL <= 0:
		return fmt.Errorf("%w: task_ttl must be positive", ErrInvalidBudget)
	case b.MaxReleaseFacts < 0 || b.MaxInfluenceFacts < 0 || b.MaxOutcomeFacts < 0:
		return fmt.Errorf("%w: exposure limits cannot be negative", ErrInvalidBudget)
	case (b.MaxReleaseFacts == 0) != (b.MaxInfluenceFacts == 0):
		return fmt.Errorf("%w: release and influence limits must both be enabled or disabled", ErrInvalidBudget)
	case b.MaxReleaseFacts > 0 && strings.TrimSpace(b.ExposureProfileVersion) == "":
		return fmt.Errorf("%w: exposure_profile_version is required", ErrInvalidBudget)
	case b.MaxReleaseFacts == 0 && b.ExposureProfileVersion != "":
		return fmt.Errorf("%w: exposure_profile_version requires exposure limits", ErrInvalidBudget)
	case isOutcomeExposureProfile(b.ExposureProfileVersion) && b.MaxOutcomeFacts <= 0:
		return fmt.Errorf("%w: V3/V4 requires a positive outcome limit", ErrInvalidBudget)
	case !isOutcomeExposureProfile(b.ExposureProfileVersion) && b.MaxOutcomeFacts != 0:
		return fmt.Errorf("%w: outcome limit requires V3/V4", ErrInvalidBudget)
	default:
		return nil
	}
}

func isOutcomeExposureProfile(profile string) bool {
	return profile == "taskgate-exposure-v3" || profile == "taskgate-exposure-v4"
}

// Within reports whether every limit is no greater than its parent limit.
func (b Budget) Within(parent Budget) bool {
	return b.MaxQueries <= parent.MaxQueries &&
		b.MaxRows <= parent.MaxRows &&
		b.MaxDBTime <= parent.MaxDBTime &&
		b.PerQueryTimeout <= parent.PerQueryTimeout &&
		b.TaskTTL <= parent.TaskTTL &&
		b.MaxReleaseFacts <= parent.MaxReleaseFacts &&
		b.MaxInfluenceFacts <= parent.MaxInfluenceFacts &&
		b.MaxOutcomeFacts <= parent.MaxOutcomeFacts &&
		b.ExposureProfileVersion == parent.ExposureProfileVersion
}

// EnsureWithin returns a stable expansion error when a requested budget would
// exceed an approved budget.
func (b Budget) EnsureWithin(parent Budget) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := parent.Validate(); err != nil {
		return fmt.Errorf("parent: %w", err)
	}
	if !b.Within(parent) {
		return ErrBudgetExpansion
	}
	return nil
}
