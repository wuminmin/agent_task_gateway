package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"taskbound.local/agent-data-gateway/internal/approval"
)

// StrictASTDomain domain-separates the strict parse-tree digest.
const StrictASTDomain = "TASKGATE-FINAL-V5-STRICT-PG-AST-V1"

// StrictASTSchemaVersion is the version of this digest construction. Changing
// the field-removal list, the canonicalization or the hash input changes every
// digest, so it must be bumped rather than edited silently.
const StrictASTSchemaVersion = 1

// StrictASTParserModule is the parser the digests are bound to. A digest is only
// comparable against another produced by the same parser: pg_query_go embeds a
// PostgreSQL grammar, and a different grammar can parse the same bytes into a
// different tree.
//
// TestStrictASTParserModuleMatchesGoMod keeps this honest.
const StrictASTParserModule = "github.com/pganalyze/pg_query_go/v6 v6.1.0"

// positionOnlyASTFields are the parse-tree fields that carry byte offsets and
// nothing else. They are removed before hashing so that whitespace, comments and
// statement position cannot change a digest.
//
// The list is explicit and reviewed rather than matched by suffix: a broad
// "anything ending in _len" rule would silently swallow a future semantic field.
// It was derived by enumerating every key pg_query emits for the control
// statements, the target templates and quoted-identifier cases; `location` was
// the only position-only key present, and `stmt_location`/`stmt_len` are the
// RawStmt offsets protojson omits when zero.
var positionOnlyASTFields = map[string]struct{}{
	"location":      {},
	"stmt_location": {},
	"stmt_len":      {},
}

// StrictASTDigest is the structural identity of one SQL statement.
//
// It is deliberately stricter than pg_query.Fingerprint, which is built to group
// *similar* queries and collapses list-length differences -- under Fingerprint
// the two-call safety pin and the one-call statement-timeout pin share a value,
// which would map one key to two classes. This digest preserves target-list
// length, CTE structure and quoted-identifier semantics, and ignores only
// constants, whitespace, comments and byte offsets.
//
// What it deliberately does NOT distinguish is two statements differing only in
// constant values: pg_stat_statements has already replaced those with
// placeholders before any observer sees them. See
// docs/final_v5_observer_v3_classifier_design.md -- that gap is closed by source
// and image identity, not here.
func StrictASTDigest(sql string) (string, error) {
	normalized, err := pg_query.Normalize(sql)
	if err != nil {
		return "", fmt.Errorf("normalize statement: %w", err)
	}
	encoded, err := pg_query.ParseToJSON(normalized)
	if err != nil {
		return "", fmt.Errorf("parse statement: %w", err)
	}
	var tree any
	decoder := json.NewDecoder(strings.NewReader(encoded))
	// UseNumber keeps integers as their exact source text, so canonicalization
	// cannot round a parse-tree value through float64.
	decoder.UseNumber()
	if err := decoder.Decode(&tree); err != nil {
		return "", fmt.Errorf("decode parse tree: %w", err)
	}
	statements, err := countRawStatements(tree)
	if err != nil {
		return "", err
	}
	if statements != 1 {
		return "", fmt.Errorf("strict AST digest requires exactly one statement, found %d", statements)
	}
	stripped := stripPositionOnlyFields(tree)
	canonical, err := approval.CanonicalJSON(stripped)
	if err != nil {
		return "", fmt.Errorf("canonicalize parse tree: %w", err)
	}
	hash := sha256.New()
	hash.Write([]byte(StrictASTDomain + "\x00"))
	fmt.Fprintf(hash, "%d\x00", StrictASTSchemaVersion)
	hash.Write([]byte(StrictASTParserModule + "\x00"))
	// The grammar version pg_query reports for this tree, so a parser upgrade
	// that changes the tree shape cannot silently reuse an old digest.
	hash.Write([]byte(parseTreeVersion(tree) + "\x00"))
	hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// countRawStatements reports how many RawStmt entries the tree carries. A digest
// over two statements would identify a pair rather than a statement, and the
// classifier key would then match executions that never happened together.
func countRawStatements(tree any) (int, error) {
	root, ok := tree.(map[string]any)
	if !ok {
		return 0, errors.New("parse tree root is not an object")
	}
	statements, present := root["stmts"]
	if !present {
		return 0, errors.New("parse tree carries no statements")
	}
	list, ok := statements.([]any)
	if !ok {
		return 0, errors.New("parse tree statement list is malformed")
	}
	return len(list), nil
}

func parseTreeVersion(tree any) string {
	root, ok := tree.(map[string]any)
	if !ok {
		return ""
	}
	if version, present := root["version"]; present {
		return fmt.Sprintf("%v", version)
	}
	return ""
}

// stripPositionOnlyFields removes exactly the reviewed byte-offset fields,
// everywhere they occur.
func stripPositionOnlyFields(node any) any {
	switch value := node.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for name, child := range value {
			if _, positionOnly := positionOnlyASTFields[name]; positionOnly {
				continue
			}
			result[name] = stripPositionOnlyFields(child)
		}
		return result
	case []any:
		result := make([]any, 0, len(value))
		for _, child := range value {
			result = append(result, stripPositionOnlyFields(child))
		}
		return result
	default:
		return value
	}
}
