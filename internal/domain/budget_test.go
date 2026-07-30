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

func TestBudgetAcceptsV4OnlyWithOutcomeLimit(t *testing.T) {
	budget := Budget{
		MaxQueries: 1, MaxRows: 1, MaxDBTime: time.Second,
		PerQueryTimeout: time.Second, TaskTTL: time.Minute,
		MaxReleaseFacts: 10, MaxInfluenceFacts: 20, MaxOutcomeFacts: 1,
		ExposureProfileVersion: "taskgate-exposure-v4",
	}
	if err := budget.Validate(); err != nil {
		t.Fatalf("valid V4 budget: %v", err)
	}
	budget.MaxOutcomeFacts = 0
	if err := budget.Validate(); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("V4 budget without outcome limit error = %v, want ErrInvalidBudget", err)
	}
}
