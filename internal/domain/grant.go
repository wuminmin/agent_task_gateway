package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	ErrInvalidGrant   = errors.New("invalid task grant")
	ErrGrantExpired   = errors.New("task grant expired")
	ErrGrantExpansion = errors.New("task grant expansion")
)

// TaskGrant is the immutable authorization envelope produced by approval.
// Callers should persist it as a value and derive narrower values for queries;
// CheckNarrowing rejects every form of expansion described by the contract.
type TaskGrant struct {
	TaskID             string              `json:"task_id"`
	Subject            string              `json:"subject"`
	Purpose            string              `json:"purpose"`
	ApprovedProducts   []string            `json:"approved_products"`
	ApprovedColumns    map[string][]string `json:"approved_columns"`
	MandatoryScope     map[string]any      `json:"mandatory_scope"`
	SensitivityCeiling Sensitivity         `json:"sensitivity_ceiling"`
	Budget             Budget              `json:"budget"`
	ExpiresAt          time.Time           `json:"expires_at"`
	CatalogVersion     string              `json:"catalog_version"`
	ApprovalReceipt    string              `json:"approval_receipt"`
}

// Validate checks the grant's structure without treating an already expired
// persisted grant as malformed.
func (g TaskGrant) Validate() error {
	if strings.TrimSpace(g.TaskID) == "" {
		return fmt.Errorf("%w: task_id is required", ErrInvalidGrant)
	}
	if strings.TrimSpace(g.Subject) == "" {
		return fmt.Errorf("%w: subject is required", ErrInvalidGrant)
	}
	if strings.TrimSpace(g.Purpose) == "" {
		return fmt.Errorf("%w: purpose is required", ErrInvalidGrant)
	}
	if len(g.ApprovedProducts) == 0 {
		return fmt.Errorf("%w: approved_products is required", ErrInvalidGrant)
	}
	if duplicate := firstDuplicate(g.ApprovedProducts); duplicate != "" {
		return fmt.Errorf("%w: duplicate approved product %q", ErrInvalidGrant, duplicate)
	}
	for _, product := range g.ApprovedProducts {
		if strings.TrimSpace(product) == "" {
			return fmt.Errorf("%w: approved product is empty", ErrInvalidGrant)
		}
		columns, ok := g.ApprovedColumns[product]
		if !ok || len(columns) == 0 {
			return fmt.Errorf("%w: approved columns missing for %q", ErrInvalidGrant, product)
		}
		if duplicate := firstDuplicate(columns); duplicate != "" {
			return fmt.Errorf("%w: duplicate approved column %q.%q", ErrInvalidGrant, product, duplicate)
		}
		for _, column := range columns {
			if strings.TrimSpace(column) == "" {
				return fmt.Errorf("%w: approved column is empty for %q", ErrInvalidGrant, product)
			}
		}
	}
	for product := range g.ApprovedColumns {
		if !contains(g.ApprovedProducts, product) {
			return fmt.Errorf("%w: columns supplied for unapproved product %q", ErrInvalidGrant, product)
		}
	}
	if g.MandatoryScope == nil {
		return fmt.Errorf("%w: mandatory_scope is required", ErrInvalidGrant)
	}
	if err := g.SensitivityCeiling.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidGrant, err)
	}
	if err := g.Budget.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidGrant, err)
	}
	if g.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: expires_at is required", ErrInvalidGrant)
	}
	if strings.TrimSpace(g.CatalogVersion) == "" {
		return fmt.Errorf("%w: catalog_version is required", ErrInvalidGrant)
	}
	if strings.TrimSpace(g.ApprovalReceipt) == "" {
		return fmt.Errorf("%w: approval_receipt is required", ErrInvalidGrant)
	}
	return nil
}

// ValidateAt additionally verifies that the grant is currently usable.
func (g TaskGrant) ValidateAt(now time.Time) error {
	if err := g.Validate(); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("%w: current time is required", ErrInvalidGrant)
	}
	if !now.Before(g.ExpiresAt) {
		return ErrGrantExpired
	}
	return nil
}

func (g TaskGrant) IsExpired(now time.Time) bool {
	return !g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt)
}

func (g TaskGrant) AuthorizesProduct(product string) bool {
	return contains(g.ApprovedProducts, product)
}

func (g TaskGrant) AuthorizesColumn(product, column string) bool {
	return g.AuthorizesProduct(product) && contains(g.ApprovedColumns[product], column)
}

// CheckNarrowing verifies that candidate preserves identity and approval
// provenance while reducing (or retaining) every authorization dimension.
func (g TaskGrant) CheckNarrowing(candidate TaskGrant) error {
	if err := g.Validate(); err != nil {
		return fmt.Errorf("parent: %w", err)
	}
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("candidate: %w", err)
	}

	if candidate.TaskID != g.TaskID || candidate.Subject != g.Subject ||
		candidate.Purpose != g.Purpose || candidate.CatalogVersion != g.CatalogVersion ||
		candidate.ApprovalReceipt != g.ApprovalReceipt {
		return grantExpansion("identity or approval provenance changed")
	}
	if !candidate.SensitivityCeiling.AtMost(g.SensitivityCeiling) {
		return grantExpansion("sensitivity ceiling increased")
	}
	if candidate.ExpiresAt.After(g.ExpiresAt) {
		return grantExpansion("expiry extended")
	}
	if err := candidate.Budget.EnsureWithin(g.Budget); err != nil {
		return grantExpansion("budget increased")
	}

	for _, product := range candidate.ApprovedProducts {
		if !g.AuthorizesProduct(product) {
			return grantExpansion("product added")
		}
		for _, column := range candidate.ApprovedColumns[product] {
			if !g.AuthorizesColumn(product, column) {
				return grantExpansion("column added")
			}
		}
	}

	if !scopeMapNarrower(g.MandatoryScope, candidate.MandatoryScope) {
		return grantExpansion("mandatory scope weakened or changed incompatibly")
	}
	return nil
}

func grantExpansion(message string) error {
	return fmt.Errorf("%w: %s", ErrGrantExpansion, message)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func firstDuplicate(values []string) string {
	copyValues := append([]string(nil), values...)
	sort.Strings(copyValues)
	for index := 1; index < len(copyValues); index++ {
		if copyValues[index] == copyValues[index-1] {
			return copyValues[index]
		}
	}
	return ""
}
