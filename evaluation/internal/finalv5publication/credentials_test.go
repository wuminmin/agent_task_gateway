package finalv5publication

import (
	"strings"
	"testing"
)

func TestValidateCredentialFreeAcceptsOnlyThePublicDatabaseSecretReference(t *testing.T) {
	files := map[string][]byte{
		"catalog.yaml": []byte(`sources:
  - address: business-postgres
    secretRef: GATEWAY_DB_PASSWORD
metadata:
  credential_free: true
  endpoint: https://example.test/path@segment
`),
		"provenance.json": []byte(`{"secret_ref":"GATEWAY_DB_PASSWORD","status":"REVIEW_CANDIDATE"}`),
	}
	if err := ValidateCredentialFree(files, []string{"runtime-password", publicDatabaseSecretReference}); err != nil {
		t.Fatalf("public source-controlled secret reference was rejected: %v", err)
	}
}

func TestValidateCredentialFreeRejectsCredentialBearingKeysWithoutEcho(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		payload string
		secret  string
	}{
		{name: "password", file: "result.json", payload: `{"password":"PasswordValue_91f2"}`, secret: "PasswordValue_91f2"},
		{name: "credential", file: "result.yaml", payload: "credential: CredentialValue_70b6\n", secret: "CredentialValue_70b6"},
		{name: "token", file: "result.yaml", payload: "token: TokenValue_01c3\n", secret: "TokenValue_01c3"},
		{name: "DSN", file: "result.json", payload: `{"business_dsn":"postgres://reader@db/data"}`, secret: "postgres://reader@db/data"},
		{name: "object access key", file: "result.yaml", payload: "object_access_key: ObjectAccess_7ac4\n", secret: "ObjectAccess_7ac4"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateCredentialFree(map[string][]byte{test.file: []byte(test.payload)}, nil)
			assertRedactedCredentialError(t, err, test.secret)
		})
	}
}

func TestValidateCredentialFreeRejectsAllFourCredentialGates(t *testing.T) {
	tests := []struct {
		name      string
		file      string
		payload   string
		sensitive []string
		secret    string
		gate      string
	}{
		{name: "URL userinfo", file: "result.json",
			payload: `{"endpoint":"https://alice:UrlPassword_45a1@example.test/path"}`,
			secret:  "UrlPassword_45a1", gate: "url_userinfo"},
		{name: "PEM marker", file: "result.yaml",
			payload: "note: |\n  -----BEGIN PRIVATE KEY-----\n  PemBody_884f\n  -----END PRIVATE KEY-----\n",
			secret:  "PemBody_884f", gate: "pem_marker"},
		{name: "secret assignment", file: "result.json",
			payload: `{"note":"GATEWAY_DB_PASSWORD=AssignedValue_129e"}`,
			secret:  "AssignedValue_129e", gate: "secret_assignment"},
		{name: "exact sensitive scalar", file: "result.yaml",
			payload: "note: ExactSensitive_55db\n", sensitive: []string{"ExactSensitive_55db"},
			secret: "ExactSensitive_55db", gate: "sensitive_scalar"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateCredentialFree(map[string][]byte{test.file: []byte(test.payload)}, test.sensitive)
			assertRedactedCredentialError(t, err, test.secret)
			if !strings.Contains(err.Error(), test.gate) {
				t.Fatalf("error %q does not identify gate %q", err.Error(), test.gate)
			}
		})
	}
}

func TestValidateCredentialFreeEnforcesExactPublicReferenceContext(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		secret  string
	}{
		{name: "wrong reference value", payload: "secretRef: OtherDatabasePassword_47ce\n", secret: "OtherDatabasePassword_47ce"},
		{name: "wrong key", payload: "reference: GATEWAY_DB_PASSWORD\n", secret: publicDatabaseSecretReference},
		{name: "extended value", payload: "secret_ref: GATEWAY_DB_PASSWORD_extra\n", secret: "GATEWAY_DB_PASSWORD_extra"},
		{name: "wrong key spelling", payload: "SecretRef: GATEWAY_DB_PASSWORD\n", secret: publicDatabaseSecretReference},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateCredentialFree(map[string][]byte{"result.yaml": []byte(test.payload)}, nil)
			assertRedactedCredentialError(t, err, test.secret)
		})
	}
}

func TestValidateCredentialFreeRejectsEmbeddedSensitiveSubstring(t *testing.T) {
	const sensitive = "ExactSensitive_694a"
	files := map[string][]byte{
		"result.json": []byte(`{"note":"prefix-ExactSensitive_694a-suffix"}`),
		"result.yaml": []byte("count: 12\n"),
	}
	err := ValidateCredentialFree(files, []string{sensitive})
	assertRedactedCredentialError(t, err, sensitive)
	if !strings.Contains(err.Error(), "sensitive_substring") {
		t.Fatalf("error %q does not identify the sensitive-substring gate", err.Error())
	}
}

func TestValidateCredentialFreeRejectsMalformedOrOpenDocumentsWithoutEcho(t *testing.T) {
	const secret = "MalformedSecret_30d1"
	tests := map[string][]byte{
		"broken.json": []byte(`{"note":"MalformedSecret_30d1"`),
		"open.yaml":   []byte("note: safe\n---\nnote: MalformedSecret_30d1\n"),
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateCredentialFree(map[string][]byte{name: payload}, []string{secret})
			assertRedactedCredentialError(t, err, secret)
		})
	}
}

func assertRedactedCredentialError(t *testing.T, err error, secret string) {
	t.Helper()
	if err == nil {
		t.Fatal("credential-bearing artifact was accepted")
	}
	if secret != "" && strings.Contains(err.Error(), secret) {
		t.Fatal("credential rejection echoed the secret")
	}
}
