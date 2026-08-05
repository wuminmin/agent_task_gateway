package sqlidentity_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/sqlidentity"
)

// The digest space is shared by the observer, the classifier manifest, the
// Adapter, the finalizer, the Attestation-footprint qualification and — since
// v1.5 — the production Gateway's signed Query Execution Binding. A change to
// the construction invalidates every digest any of them ever recorded, and the
// qualifications that were run against the old space would silently become
// evidence about a different key.
//
// These vectors were produced by the pre-move implementation in
// evaluation/internal/experiment and reproduced byte for byte by this package.
// They pin the construction so a future edit has to be a deliberate
// StrictASTSchemaVersion bump rather than an accident.
func TestStrictASTDigestGoldenVectors(t *testing.T) {
	for _, testCase := range []struct{ name, sql, digest string }{
		{
			name:   "safety session pin",
			sql:    dataconnector.SafetySessionPinSQL,
			digest: "ba22d865c6543e00d12dea192639fcf809ce9be57ee4ef5c0ef51f2a6d415f2d",
		},
		{
			name:   "representation pin",
			sql:    dataconnector.RepresentationPinSQL,
			digest: "d474bfd147403663d0cd702d219f0576a91aa3ddbd6e1de169fc5b9886a87c3b",
		},
		{
			name:   "statement timeout pin",
			sql:    dataconnector.StatementTimeoutPinSQL,
			digest: "a8acade16a6702d05e7b894a642d44f3cea0f1b713fbe17a62713335c6f00c83",
		},
		{
			name:   "nested pg_rewrite lookup",
			sql:    `SELECT * FROM pg_catalog.pg_rewrite WHERE ev_class = $1 AND rulename = $2`,
			digest: "e5738df1650276a7f20e677172e067bc62bab12d48c18a378c9b6ed602433842",
		},
		{
			name:   "repeatable read begin",
			sql:    `begin isolation level repeatable read read only`,
			digest: "91d4316ad8bb8cf839ccd8b0445e35496b2604c1ef2b434b66a482f20527f0c0",
		},
		{
			name:   "commit",
			sql:    `commit`,
			digest: "f440d0cec958ea7cf465a67c131f197101a6c780a60c31aaec8368e47398832e",
		},
		{
			name:   "limited projection",
			sql:    `SELECT id, amount FROM reporting.expense ORDER BY id LIMIT 100`,
			digest: "99f2e8633019b99a6695362186f1ba9f9ad3152953ca51484d1919462708a346",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := sqlidentity.StrictASTDigest(testCase.sql)
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if got != testCase.digest {
				t.Fatalf("STRICT AST DIGEST SPACE CHANGED: %s is now %s, was %s.\n"+
					"Every recorded classifier key, observer snapshot, Attestation footprint and signed "+
					"V9 binding was produced in the old space. Bump StrictASTSchemaVersion and requalify, "+
					"or revert the construction.", testCase.name, got, testCase.digest)
			}
		})
	}
}

// The construction identity is part of the hash input. Restating it here makes a
// silent edit to any of the three a test failure rather than a quiet reissue of
// the whole digest space under the same schema version.
func TestStrictASTConstructionIdentityIsPinned(t *testing.T) {
	if sqlidentity.StrictASTDomain != "TASKGATE-FINAL-V5-STRICT-PG-AST-V1" {
		t.Fatalf("digest domain changed: %s", sqlidentity.StrictASTDomain)
	}
	if sqlidentity.StrictASTSchemaVersion != 1 {
		t.Fatalf("digest schema version changed: %d", sqlidentity.StrictASTSchemaVersion)
	}
	if want := []string{"location", "stmt_len", "stmt_location"}; !reflect.DeepEqual(sqlidentity.PositionOnlyASTFields(), want) {
		t.Fatalf("the position-only field list changed: %v, want %v.\n"+
			"Stripping a field that is not purely a byte offset erases meaning; keeping one that is "+
			"reintroduces whitespace sensitivity. Either way every digest moves.",
			sqlidentity.PositionOnlyASTFields(), want)
	}
}

// Strict-AST failures reach production logs, audit records and receipts, none of
// which may carry statement text. The parser's own messages quote the offending
// SQL ("syntax error at or near ..."), so the package must classify rather than
// pass them through.
func TestStrictASTParserErrorCodesAreStableAndSQLFree(t *testing.T) {
	const secret = "taskgate_secret_relation_name"
	for name, testCase := range map[string]struct {
		sql  string
		code sqlidentity.ParserErrorCode
	}{
		// Normalize parses before it rewrites constants, so a statement that does
		// not parse is refused there rather than at ParseToJSON.
		"two statements":   {sql: `SELECT 1 FROM ` + secret + `; SELECT 2`, code: sqlidentity.ParserErrorStatementCount},
		"trailing garbage": {sql: `SELECT 1 FROM ` + secret + ` WHERE`, code: sqlidentity.ParserErrorNormalize},
		"empty":            {sql: ``, code: sqlidentity.ParserErrorStatementCount},
		"only whitespace":  {sql: `   `, code: sqlidentity.ParserErrorStatementCount},
		"not SQL":          {sql: `this is not a statement about ` + secret, code: sqlidentity.ParserErrorNormalize},
	} {
		t.Run(name, func(t *testing.T) {
			digest, err := sqlidentity.StrictASTDigest(testCase.sql)
			if err == nil {
				t.Fatalf("an unusable statement produced a digest: %s", digest)
			}
			if !errors.Is(err, sqlidentity.ErrStrictAST) {
				t.Fatalf("failure is not recognisable as a strict-AST failure: %v", err)
			}
			if got := sqlidentity.ErrorCode(err); got != testCase.code {
				t.Fatalf("code is %q, want %q", got, testCase.code)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the error message leaks statement text: %q", err.Error())
			}
		})
	}
}
