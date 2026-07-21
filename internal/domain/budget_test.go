package domain

import (
	"errors"
	"testing"
	"time"
)

func TestBudgetRequestCannotExpandProfile(t *testing.T) {
	profile := Budget{
		MaxQueries:      10,
		MaxRows:         500,
		MaxDBTime:       30 * time.Second,
		PerQueryTimeout: 5 * time.Second,
		TaskTTL:         30 * time.Minute,
	}
	queries := int64(4)
	narrowed, err := (BudgetRequest{MaxQueries: &queries}).Apply(profile)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if narrowed.MaxQueries != queries || narrowed.MaxRows != profile.MaxRows {
		t.Fatalf("unexpected narrowed budget: %#v", narrowed)
	}

	tooMany := int64(11)
	if _, err := (BudgetRequest{MaxQueries: &tooMany}).Apply(profile); !errors.Is(err, ErrBudgetExpansion) {
		t.Fatalf("expansion error = %v, want ErrBudgetExpansion", err)
	}
}

func TestBudgetRejectsTimeoutAboveTotalDBTime(t *testing.T) {
	budget := Budget{
		MaxQueries:      1,
		MaxRows:         1,
		MaxDBTime:       time.Second,
		PerQueryTimeout: 2 * time.Second,
		TaskTTL:         time.Minute,
	}
	if err := budget.Validate(); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("Validate error = %v, want ErrInvalidBudget", err)
	}
}
