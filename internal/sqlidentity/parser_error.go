package sqlidentity

import (
	"errors"
	"fmt"
)

// ParserErrorCode names why a statement could not be given a structural
// identity.
//
// The codes exist because the underlying parser reports failures by quoting the
// statement -- "syntax error at or near \"...\"" -- and strict-AST digests are
// computed on the production path, where the failure travels into logs, audit
// records and receipts that must never carry statement text. A code is stable,
// greppable and safe to persist; the SQL stays where it was.
type ParserErrorCode string

const (
	// ParserErrorNormalize means constant normalization refused the statement.
	ParserErrorNormalize ParserErrorCode = "strict_ast_normalize_failed"
	// ParserErrorParse means the normalized statement did not parse.
	ParserErrorParse ParserErrorCode = "strict_ast_parse_failed"
	// ParserErrorDecode means the parser emitted JSON this package could not read.
	ParserErrorDecode ParserErrorCode = "strict_ast_decode_failed"
	// ParserErrorMalformedTree means the parse tree did not have the shape every
	// pg_query tree has.
	ParserErrorMalformedTree ParserErrorCode = "strict_ast_malformed_parse_tree"
	// ParserErrorNoStatements means the tree carried no statement list at all.
	ParserErrorNoStatements ParserErrorCode = "strict_ast_no_statements"
	// ParserErrorStatementCount means the input was not exactly one statement. A
	// digest over two statements identifies a pair, not a statement, and would
	// match executions that never happened together.
	ParserErrorStatementCount ParserErrorCode = "strict_ast_statement_count_not_one"
	// ParserErrorCanonicalize means the stripped tree could not be canonicalized.
	ParserErrorCanonicalize ParserErrorCode = "strict_ast_canonicalize_failed"
)

// ErrStrictAST matches every strict-AST failure under errors.Is, so a caller can
// separate "this statement has no structural identity" from a transport or
// storage failure without inspecting strings.
var ErrStrictAST = errors.New("strict AST digest failed")

// ParserError is the only error type StrictASTDigest returns. It carries a code
// and, for the statement-count case, the count -- never the statement.
type ParserError struct {
	Code ParserErrorCode
	// Statements is set only for ParserErrorStatementCount. A count is safe: it
	// is a small integer that reveals nothing about the statement's content.
	Statements int
}

func (e *ParserError) Error() string {
	if e.Code == ParserErrorStatementCount {
		return fmt.Sprintf("%s: %s (found %d)", ErrStrictAST.Error(), e.Code, e.Statements)
	}
	return fmt.Sprintf("%s: %s", ErrStrictAST.Error(), e.Code)
}

func (e *ParserError) Is(target error) bool { return target == ErrStrictAST }

// ErrorCode reports the code of a strict-AST failure, or the empty string when
// err did not come from this package.
func ErrorCode(err error) ParserErrorCode {
	var parserErr *ParserError
	if errors.As(err, &parserErr) {
		return parserErr.Code
	}
	return ""
}
