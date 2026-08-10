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
// The original vectors were produced by the pre-move v1 implementation. The
// values below are the deliberately reissued v2 space after source-order
// normalization of only synthetic ParamRefs. They pin that construction so a
// future edit has to be another StrictASTSchemaVersion bump rather than an
// accident.
func TestStrictASTDigestGoldenVectors(t *testing.T) {
	for _, testCase := range []struct{ name, sql, digest string }{
		{
			name:   "safety session pin",
			sql:    dataconnector.SafetySessionPinSQL,
			digest: "fa50290e772fc3b97eca7f4a90f61928059cdcbcb3dfe1e8120baa0a39efa1c5",
		},
		{
			name:   "representation pin",
			sql:    dataconnector.RepresentationPinSQL,
			digest: "14d2999dc94ba0cce01174f4543b819bfea95268723ea5339bdfb1fa65f9449d",
		},
		{
			name:   "statement timeout pin",
			sql:    dataconnector.StatementTimeoutPinSQL,
			digest: "67d7f0472443a515b9f0a9fa2b765683555eb1cb27d0541acecd497f28efedbb",
		},
		{
			name:   "nested pg_rewrite lookup",
			sql:    `SELECT * FROM pg_catalog.pg_rewrite WHERE ev_class = $1 AND rulename = $2`,
			digest: "8258e53289cbcaf0e18cf828725bbd23da953145bea8f873fcbe165b7c85dca7",
		},
		{
			name:   "repeatable read begin",
			sql:    `begin isolation level repeatable read read only`,
			digest: "c6d11346b1589b2f7461a2ee39091073cb2119abdef4caf752e220b29c83283f",
		},
		{
			name:   "commit",
			sql:    `commit`,
			digest: "211bdf2747c37282f656cb81b3bc041f63ffa5e8ce77f3dc32300215324b274f",
		},
		{
			name:   "limited projection",
			sql:    `SELECT id, amount FROM reporting.expense ORDER BY id LIMIT 100`,
			digest: "29bedfe521cabb9bec129e6ac1e8c617651c68607e1630a0ea063d39ced64591",
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
	if sqlidentity.StrictASTSchemaVersion != 2 {
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
