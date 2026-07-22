package security

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

type corpus struct {
	SchemaVersion int `json:"schema_version"`
	Cases         []struct {
		ID       string `json:"id"`
		File     string `json:"file"`
		Expected string `json:"expected"`
	} `json:"cases"`
}

type astConfig struct {
	Products []sqlpolicy.ProductGrant `json:"products"`
}

type promptCorpus struct {
	SchemaVersion int `json:"schema_version"`
	Cases         []struct {
		ID            string `json:"id"`
		UntrustedText string `json:"untrusted_text"`
		Expected      string `json:"expected"`
		Attempts      []struct {
			ID           string `json:"id"`
			SQL          string `json:"sql"`
			ExpectedCode string `json:"expected_code"`
		} `json:"representative_attempts"`
	} `json:"cases"`
}

func TestAttackCorpus(t *testing.T) {
	corpusPath := filepath.Join("..", "attacks", "corpus.json")
	encoded, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	var cases corpus
	if err := json.Unmarshal(encoded, &cases); err != nil || cases.SchemaVersion != 1 || len(cases.Cases) == 0 {
		t.Fatalf("invalid attack corpus: %v", err)
	}
	configBytes, err := os.ReadFile(filepath.Join("..", "ast-gateway", "tpch.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config astConfig
	if err := json.Unmarshal(configBytes, &config); err != nil || len(config.Products) == 0 {
		t.Fatalf("invalid AST grant: %v", err)
	}
	engine := sqlpolicy.New(sqlpolicy.Config{})
	grant := sqlpolicy.Grant{Products: config.Products}
	seen := make(map[string]bool)
	for _, attack := range cases.Cases {
		attack := attack
		t.Run(attack.ID, func(t *testing.T) {
			if attack.ID == "" || seen[attack.ID] {
				t.Fatal("empty or duplicate attack ID")
			}
			seen[attack.ID] = true
			sqlBytes, err := os.ReadFile(filepath.Join("..", "attacks", attack.File))
			if err != nil {
				t.Fatal(err)
			}
			decision, decisionErr := engine.Authorize(sqlpolicy.Request{
				SQL: string(sqlBytes), Grant: grant, RowLimit: 10_000,
			})
			if attack.Expected == "ALLOW_WITH_MANDATORY_SCOPE" {
				if decisionErr != nil {
					t.Fatalf("expected allow, got %v", decisionErr)
				}
				if !strings.Contains(decision.SQL, `"eval_scope" = E'all'`) {
					t.Fatalf("authorized SQL omitted mandatory scope: %s", decision.SQL)
				}
				return
			}
			var policyErr *sqlpolicy.PolicyError
			if !errors.As(decisionErr, &policyErr) {
				t.Fatalf("expected policy code %s, got %v", attack.Expected, decisionErr)
			}
			if string(policyErr.Code) != attack.Expected {
				t.Fatalf("expected %s, got %s", attack.Expected, policyErr.Code)
			}
		})
	}
}

// TestPromptInjectionBoundaryCases validates only the deterministic SQL-policy
// boundary associated with each untrusted-text case. It does not invoke a
// model and must not be interpreted as a claim of prompt-injection robustness.
func TestPromptInjectionBoundaryCases(t *testing.T) {
	encoded, err := os.ReadFile(filepath.Join("..", "attacks", "prompt-injection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var promptCases promptCorpus
	if err := json.Unmarshal(encoded, &promptCases); err != nil || promptCases.SchemaVersion != 1 || len(promptCases.Cases) == 0 {
		t.Fatalf("invalid prompt-injection corpus: %v", err)
	}
	configBytes, err := os.ReadFile(filepath.Join("..", "ast-gateway", "tpch.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config astConfig
	if err := json.Unmarshal(configBytes, &config); err != nil || len(config.Products) == 0 {
		t.Fatalf("invalid AST grant: %v", err)
	}
	engine := sqlpolicy.New(sqlpolicy.Config{})
	grant := sqlpolicy.Grant{Products: config.Products}
	seen := make(map[string]bool)
	for _, promptCase := range promptCases.Cases {
		promptCase := promptCase
		t.Run(promptCase.ID, func(t *testing.T) {
			if promptCase.ID == "" || promptCase.UntrustedText == "" || promptCase.Expected == "" || seen[promptCase.ID] {
				t.Fatal("prompt case must have unique ID, untrusted text, and expected boundary")
			}
			seen[promptCase.ID] = true
			if len(promptCase.Attempts) == 0 {
				t.Fatal("prompt case has no representative SQL attempts")
			}
			attemptIDs := make(map[string]bool)
			for _, attempt := range promptCase.Attempts {
				attempt := attempt
				t.Run(attempt.ID, func(t *testing.T) {
					if attempt.ID == "" || attempt.SQL == "" || attempt.ExpectedCode == "" || attemptIDs[attempt.ID] {
						t.Fatal("representative attempt must have unique ID, SQL, and expected code")
					}
					attemptIDs[attempt.ID] = true
					_, decisionErr := engine.Authorize(sqlpolicy.Request{SQL: attempt.SQL, Grant: grant, RowLimit: 10_000})
					var policyErr *sqlpolicy.PolicyError
					if !errors.As(decisionErr, &policyErr) {
						t.Fatalf("expected policy rejection %s, got %v", attempt.ExpectedCode, decisionErr)
					}
					if string(policyErr.Code) != attempt.ExpectedCode {
						t.Fatalf("expected %s, got %s", attempt.ExpectedCode, policyErr.Code)
					}
				})
			}
		})
	}
}
