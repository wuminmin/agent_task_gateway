package finalv5publication

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const AdapterStderrCredentialScanVersion = "taskgate-adapter-stderr-credential-scan-v1"

// AdapterStderrCredentialScan is safe to retain beside the private stderr. It
// records only its digest, size, and zero-hit gate counts; credential values are
// never copied into the report or an error.
type AdapterStderrCredentialScan struct {
	SchemaVersion           int    `json:"schema_version"`
	Record                  string `json:"record"`
	Status                  string `json:"status"`
	InputSHA256             string `json:"input_sha256"`
	InputBytes              int64  `json:"input_bytes"`
	SensitiveValuesChecked  int    `json:"sensitive_values_checked"`
	URLUserinfoHits         int    `json:"url_userinfo_hits"`
	PEMMarkerHits           int    `json:"pem_marker_hits"`
	SecretAssignmentHits    int    `json:"secret_assignment_hits"`
	JSONScalarExactHits     int    `json:"json_scalar_exact_hits"`
	ExactValueSubstringHits int    `json:"exact_value_substring_hits"`
}

// ValidateAdapterStderr applies the existing publication credential scanner to
// arbitrary text. The wrapper makes every byte one structured JSON scalar, and
// each line that is itself JSON is additionally scanned as a document so nested
// scalar equality cannot hide inside serialized diagnostics.
func ValidateAdapterStderr(value []byte, sensitiveValues []string) (AdapterStderrCredentialScan, error) {
	digest := sha256.Sum256(value)
	report := AdapterStderrCredentialScan{SchemaVersion: 1, Record: AdapterStderrCredentialScanVersion,
		Status: "pass", InputSHA256: hex.EncodeToString(digest[:]), InputBytes: int64(len(value))}
	uniqueSensitive := make(map[string]bool, len(sensitiveValues))
	for _, sensitive := range sensitiveValues {
		if sensitive != "" {
			uniqueSensitive[sensitive] = true
		}
	}
	report.SensitiveValuesChecked = len(uniqueSensitive)
	checked := make([]string, 0, len(uniqueSensitive))
	for sensitive := range uniqueSensitive {
		checked = append(checked, sensitive)
	}
	wrapper, err := json.Marshal(map[string]string{"adapter_stderr": string(value)})
	if err != nil {
		return AdapterStderrCredentialScan{}, fmt.Errorf("encode adapter stderr credential wrapper: %w", err)
	}
	files := map[string][]byte{"adapter-stderr-wrapper.json": wrapper}
	scanner := bufio.NewScanner(bytes.NewReader(value))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		payload := bytes.TrimSpace(scanner.Bytes())
		if len(payload) != 0 && json.Valid(payload) {
			files[fmt.Sprintf("adapter-stderr-line-%06d.json", line)] = append([]byte(nil), payload...)
		}
	}
	if err := scanner.Err(); err != nil {
		return AdapterStderrCredentialScan{}, fmt.Errorf("scan adapter stderr lines: %w", err)
	}
	if err := ValidateCredentialFree(files, checked); err != nil {
		return AdapterStderrCredentialScan{}, err
	}
	return report, nil
}

// ValidateAdapterStderrCredentialScan proves that a retained scan report binds
// the exact stderr bytes and that every credential gate stayed closed. The
// launcher supplies the sensitive values; this verifier consumes only the safe
// report and retained diagnostic bytes.
func ValidateAdapterStderrCredentialScan(value []byte, report AdapterStderrCredentialScan) error {
	digest := sha256.Sum256(value)
	if report.SchemaVersion != 1 || report.Record != AdapterStderrCredentialScanVersion ||
		report.Status != "pass" || report.InputSHA256 != hex.EncodeToString(digest[:]) ||
		report.InputBytes != int64(len(value)) || report.SensitiveValuesChecked < 1 ||
		report.URLUserinfoHits != 0 || report.PEMMarkerHits != 0 ||
		report.SecretAssignmentHits != 0 || report.JSONScalarExactHits != 0 ||
		report.ExactValueSubstringHits != 0 {
		return fmt.Errorf("adapter stderr credential scan is incomplete or differs from the retained stderr")
	}
	return nil
}
