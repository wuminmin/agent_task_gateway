package sqlpolicy

import (
	"errors"
	"testing"
)

// FuzzAuthorizeNeverPanics is also the entry point used by the documented
// long-running CPU campaign. Ordinary go test executes the security seeds.
func FuzzAuthorizeNeverPanics(f *testing.F) {
	for _, seed := range []string{
		`SELECT department FROM expense_detail`,
		`SELECT * FROM expense_detail`,
		`WITH x AS (SELECT amount FROM expense_detail) SELECT amount FROM x`,
		`SELECT department FROM expense_detail UNION SELECT department FROM expense_detail`,
		`SELECT department FROM expense_detail; DROP TABLE expense_detail`,
		"SELECT department FROM expense_detail /*\x00*/",
	} {
		f.Add(seed)
	}
	engine := New(Config{})
	f.Fuzz(func(t *testing.T, sql string) {
		decision, err := engine.Authorize(Request{SQL: sql, Grant: testGrant(), RowLimit: 17})
		if err == nil {
			if decision.SQL == "" || decision.Fingerprint == "" || decision.RowLimit != 17 {
				t.Fatalf("successful decision is incomplete: %#v", decision)
			}
			return
		}
		var policyErr *PolicyError
		if !errors.As(err, &policyErr) || policyErr.Code == "" {
			t.Fatalf("unstable non-policy error escaped: %T %v", err, err)
		}
	})
}

func TestWhitespaceAndCommentMetamorphism(t *testing.T) {
	engine := New(Config{})
	variants := []string{
		`SELECT department, amount FROM expense_detail WHERE amount >= 10`,
		"  SELECT department, amount\nFROM expense_detail\nWHERE amount >= 10;  ",
		`SELECT /* harmless */ department, amount FROM expense_detail WHERE amount >= 10 -- tail`,
	}
	var fingerprint string
	for _, sql := range variants {
		decision, err := engine.Authorize(Request{SQL: sql, Grant: testGrant(), RowLimit: 9})
		if err != nil {
			t.Fatalf("Authorize(%q): %v", sql, err)
		}
		if fingerprint == "" {
			fingerprint = decision.Fingerprint
		} else if decision.Fingerprint != fingerprint {
			t.Fatalf("semantically equivalent SQL changed fingerprint: %s != %s", decision.Fingerprint, fingerprint)
		}
	}
}
