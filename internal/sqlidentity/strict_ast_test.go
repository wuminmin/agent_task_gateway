package sqlidentity_test

import (
	"os"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/sqlidentity"
)

func mustDigest(t *testing.T, sql string) string {
	t.Helper()
	digest, err := sqlidentity.StrictASTDigest(sql)
	if err != nil {
		t.Fatalf("digest %q: %v", sql, err)
	}
	if len(digest) != 64 {
		t.Fatalf("digest is not a SHA-256: %q", digest)
	}
	return digest
}

// The digest binds the parser it was produced with, so the declared module must
// be the one actually built. A silent parser upgrade would otherwise reuse
// digests across two different grammars.
func TestStrictASTParserModuleMatchesGoMod(t *testing.T) {
	module, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if !strings.Contains(string(module), sqlidentity.StrictASTParserModule) {
		t.Fatalf("go.mod does not require %q; the strict AST digests are bound to a parser that is not built",
			sqlidentity.StrictASTParserModule)
	}
}

// Formatting is not identity. Whitespace, newlines and comments must not move a
// digest, because pg_stat_statements returns text the server reformatted.
func TestStrictASTDigestIgnoresFormattingAndComments(t *testing.T) {
	base := mustDigest(t, `SELECT a, b FROM t WHERE x = 1`)
	for name, variant := range map[string]string{
		"extra whitespace": "SELECT   a,\n\tb\nFROM t\nWHERE x = 1",
		"leading comment":  "-- pin the reader\nSELECT a, b FROM t WHERE x = 1",
		"inline comment":   "SELECT a, /* projection */ b FROM t WHERE x = 1",
		"trailing newline": "SELECT a, b FROM t WHERE x = 1\n",
	} {
		if got := mustDigest(t, variant); got != base {
			t.Errorf("%s changed the digest", name)
		}
	}
}

// Constants are erased by pg_stat_statements before the classifier ever sees a
// statement, so the digest must not depend on them. This is a deliberate limit,
// not an oversight; see docs/final_v5_observer_v3_classifier_design.md.
func TestStrictASTDigestIgnoresConstantValues(t *testing.T) {
	base := mustDigest(t, `SELECT pg_catalog.set_config('search_path', 'pg_catalog', true)`)
	same := mustDigest(t, `SELECT pg_catalog.set_config('TimeZone', 'UTC', false)`)
	if base != same {
		t.Fatal("the digest distinguishes constant values, which pg_stat_statements has already erased")
	}
}

// Target-list length must matter. This is the exact property pg_query.Fingerprint
// collapses, which is why it cannot be the classification key: under Fingerprint
// the two-call safety pin and the one-call timeout pin share a value.
func TestStrictASTDigestSeparatesTargetListLength(t *testing.T) {
	one := mustDigest(t, `SELECT pg_catalog.set_config('a', 'b', true)`)
	two := mustDigest(t, `SELECT pg_catalog.set_config('a', 'b', true), pg_catalog.set_config('c', 'd', true)`)
	if one == two {
		t.Fatal("one-call and two-call set_config share a digest; the classifier key would map to two classes")
	}
}

func TestStrictASTDigestSeparatesCTEStructure(t *testing.T) {
	plain := mustDigest(t, `SELECT x FROM t`)
	withCTE := mustDigest(t, `WITH c AS (SELECT x FROM t) SELECT x FROM c`)
	if plain == withCTE {
		t.Fatal("a CTE does not change the digest")
	}
}

// Quoted identifiers carry meaning in PostgreSQL: "X" and x are different
// relations. A canonicalization that stripped quotes would erase that.
func TestStrictASTDigestPreservesQuotedIdentifierSemantics(t *testing.T) {
	unquoted := mustDigest(t, `SELECT a FROM reporting.expense`)
	quotedSame := mustDigest(t, `SELECT "a" FROM "reporting"."expense"`)
	if unquoted != quotedSame {
		t.Fatal("quoting an already-lowercase identifier changed the digest")
	}
	quotedUpper := mustDigest(t, `SELECT a FROM "Reporting"."Expense"`)
	if quotedUpper == unquoted {
		t.Fatal("a quoted mixed-case identifier shares a digest with the folded one; quoting semantics were lost")
	}
}

func TestStrictASTDigestRejectsMultipleStatementsAndMalformedSQL(t *testing.T) {
	for name, sql := range map[string]string{
		"two statements":   `SELECT 1; SELECT 2`,
		"trailing garbage": `SELECT 1 FROM`,
		"empty":            ``,
		"only whitespace":  `   `,
		"not SQL":          `this is not a statement`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := sqlidentity.StrictASTDigest(sql); err == nil {
				t.Fatal("an unusable statement produced a digest")
			}
		})
	}
}

// The controls the accounting depends on must all be distinguishable from one
// another. The two pins are the case contracts v1.5 structurally separated.
func TestStrictASTDigestSeparatesEveryControlStatement(t *testing.T) {
	digests := map[string]string{
		"safety pin":         mustDigest(t, dataconnector.SafetySessionPinSQL),
		"representation pin": mustDigest(t, dataconnector.RepresentationPinSQL),
		"statement timeout":  mustDigest(t, `SELECT pg_catalog.set_config('statement_timeout', $1, true)`),
		"nested rewrite":     mustDigest(t, `SELECT * FROM pg_catalog.pg_rewrite WHERE ev_class = $1 AND rulename = $2`),
		"transaction begin":  mustDigest(t, `begin isolation level repeatable read read only`),
		"transaction commit": mustDigest(t, `commit`),
	}
	seen := map[string]string{}
	for name, digest := range digests {
		if other, collision := seen[digest]; collision {
			t.Fatalf("%s and %s share strict AST digest %s", name, other, digest)
		}
		seen[digest] = name
	}
}

// Determinism: Go map iteration order must not reach the digest.
func TestStrictASTDigestIsStableAcrossRepeatedComputation(t *testing.T) {
	first := mustDigest(t, dataconnector.RepresentationPinSQL)
	for i := 0; i < 32; i++ {
		if again := mustDigest(t, dataconnector.RepresentationPinSQL); again != first {
			t.Fatalf("digest is not deterministic: %s then %s", first, again)
		}
	}
}
