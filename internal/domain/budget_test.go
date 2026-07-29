package domain

import (
	"errors"
	"testing"
	"time"
)

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
