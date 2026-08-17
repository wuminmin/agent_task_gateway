package finalv5publication

import (
	"strings"
	"testing"
)

func TestAdapterStderrCredentialGateAcceptsEmptyAndPublicDiagnostics(t *testing.T) {
	for _, value := range []string{"", "authenticated Gateway concurrency capacity cannot execute the frozen width-500 matrix\n"} {
		report, err := ValidateAdapterStderr([]byte(value), []string{"PrivateValue_2039"})
		if err != nil {
			t.Fatalf("value %q: %v", value, err)
		}
		if report.Status != "pass" || report.InputBytes != int64(len(value)) || report.SensitiveValuesChecked != 1 ||
			report.URLUserinfoHits != 0 || report.PEMMarkerHits != 0 || report.SecretAssignmentHits != 0 ||
			report.JSONScalarExactHits != 0 || report.ExactValueSubstringHits != 0 {
			t.Fatalf("unexpected report: %#v", report)
		}
	}
}

func TestAdapterStderrCredentialGateRejectsEveryStrictGate(t *testing.T) {
	sensitive := "PrivateValue_2039"
	tests := []struct {
		name, value, gate string
	}{
		{"URL userinfo", "connect postgres://alice:password@example.invalid/db", "url_userinfo"},
		{"PEM", "-----BEGIN PRIVATE KEY-----", "pem_marker"},
		{"assignment", "database_password=AssignedValue", "secret_assignment"},
		{"JSON scalar", `{"message":"PrivateValue_2039"}`, "sensitive_scalar"},
		{"exact substring", "prefix-PrivateValue_2039-suffix", "sensitive_substring"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ValidateAdapterStderr([]byte(test.value), []string{sensitive}); err == nil || !strings.Contains(err.Error(), test.gate) {
				t.Fatalf("error = %v, want gate %s", err, test.gate)
			}
		})
	}
}
