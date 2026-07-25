package domain

import (
	"errors"
	"testing"
)

func TestHighestSensitivity(t *testing.T) {
	highest, err := HighestSensitivity(SensitivityLow, SensitivityHigh, SensitivityMedium)
	if err != nil {
		t.Fatalf("HighestSensitivity returned error: %v", err)
	}
	if highest != SensitivityHigh {
		t.Fatalf("HighestSensitivity = %q, want %q", highest, SensitivityHigh)
	}
	if _, err := HighestSensitivity(Sensitivity("secret")); !errors.Is(err, ErrInvalidSensitivity) {
		t.Fatalf("invalid sensitivity error = %v", err)
	}
}

func TestApprovalModeRejectsAutomaticApproval(t *testing.T) {
	if err := ApprovalMode("auto").Validate(); !errors.Is(err, ErrInvalidApprovalMode) {
		t.Fatalf("automatic approval error = %v, want ErrInvalidApprovalMode", err)
	}
	if err := ApprovalModeManual.Validate(); err != nil {
		t.Fatalf("manual approval = %v", err)
	}
}
