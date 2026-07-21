package domain

import (
	"errors"
	"fmt"
)

// Sensitivity is an ordered data classification. The order is security
// significant: a grant may only move to an equal or lower classification.
type Sensitivity string

const (
	SensitivityPublic     Sensitivity = "public"
	SensitivityLow        Sensitivity = "low"
	SensitivityMedium     Sensitivity = "medium"
	SensitivityHigh       Sensitivity = "high"
	SensitivityRestricted Sensitivity = "restricted"
)

var ErrInvalidSensitivity = errors.New("invalid sensitivity")

var sensitivityRank = map[Sensitivity]int{
	SensitivityPublic:     0,
	SensitivityLow:        1,
	SensitivityMedium:     2,
	SensitivityHigh:       3,
	SensitivityRestricted: 4,
}

// Validate reports whether the sensitivity is one of the supported values.
func (s Sensitivity) Validate() error {
	if _, ok := sensitivityRank[s]; !ok {
		return fmt.Errorf("%w: %q", ErrInvalidSensitivity, s)
	}
	return nil
}

// Rank returns the security ordering for a sensitivity.
func (s Sensitivity) Rank() (int, error) {
	rank, ok := sensitivityRank[s]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrInvalidSensitivity, s)
	}
	return rank, nil
}

// AtMost reports whether s is no more sensitive than ceiling.
func (s Sensitivity) AtMost(ceiling Sensitivity) bool {
	sRank, sOK := sensitivityRank[s]
	cRank, cOK := sensitivityRank[ceiling]
	return sOK && cOK && sRank <= cRank
}

// HighestSensitivity returns the most sensitive classification in values.
func HighestSensitivity(values ...Sensitivity) (Sensitivity, error) {
	if len(values) == 0 {
		return "", fmt.Errorf("%w: no values", ErrInvalidSensitivity)
	}

	highest := values[0]
	highestRank, err := highest.Rank()
	if err != nil {
		return "", err
	}
	for _, value := range values[1:] {
		rank, rankErr := value.Rank()
		if rankErr != nil {
			return "", rankErr
		}
		if rank > highestRank {
			highest = value
			highestRank = rank
		}
	}
	return highest, nil
}

// ApprovalMode is selected deterministically from the catalog.
type ApprovalMode string

const (
	ApprovalModeAuto   ApprovalMode = "auto"
	ApprovalModeManual ApprovalMode = "manual"
)

var ErrInvalidApprovalMode = errors.New("invalid approval mode")

func (m ApprovalMode) Validate() error {
	switch m {
	case ApprovalModeAuto, ApprovalModeManual:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidApprovalMode, m)
	}
}
