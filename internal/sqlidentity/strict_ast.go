// Package sqlidentity owns the structural identity of a SQL statement.
//
// It is deliberately neutral: the strict normalized-AST digest is the key the
// observer classifies on, the key the classifier manifest is built from, the key
// the Adapter and the finalizer independently rebuild, and — since v1.5 — the
// key the production Gateway signs into a Query Execution Binding. Those five
// consumers must agree byte for byte, so there must be exactly one
// implementation and it must be importable from both the production tree and
// the evaluation tree.
//
// It previously lived in evaluation/internal/experiment, which Go's internal
// rule makes unreachable from internal/gateway; production consequently passed a
// nil digester and signed identities with an empty strict digest. Moving it here
// is what lets production call
//
//	physicalquery.Derive(engine, sqlidentity.StrictASTDigest, ...)
//
// The initial move preserved the v1 construction byte for byte. Schema v2 adds
// the narrow ParamRef normalization below; v1 evidence remains historical and
// is deliberately not comparable with identities produced here.
package sqlidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"taskbound.local/agent-data-gateway/internal/approval"
)

// StrictASTDomain domain-separates the strict parse-tree digest.
const StrictASTDomain = "TASKGATE-FINAL-V5-STRICT-PG-AST-V1"

// StrictASTSchemaVersion is the version of this digest construction. Changing
// the field-removal list, the canonicalization or the hash input changes every
// digest, so it must be bumped rather than edited silently.
const StrictASTSchemaVersion = 2

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

// PositionOnlyASTFields returns the reviewed position-only field list.
//
// It is exported so a contract test can assert the list rather than restate it,
// and it returns a copy so no caller can widen the stripping rule at runtime.
func PositionOnlyASTFields() []string {
	names := make([]string, 0, len(positionOnlyASTFields))
	for name := range positionOnlyASTFields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
//
// Every error it returns is a *ParserError, which carries a stable code and no
// statement text. Callers log and persist these errors next to receipts and
// evidence, and the parser's own messages quote the offending SQL.
func StrictASTDigest(sql string) (string, error) {
	normalized, err := pg_query.Normalize(sql)
	if err != nil {
		return "", &ParserError{Code: ParserErrorNormalize}
	}
	tree, err := parseJSONTree(normalized)
	if err != nil {
		return "", err
	}
	statements, err := countRawStatements(tree)
	if err != nil {
		return "", err
	}
	if statements != 1 {
		return "", &ParserError{Code: ParserErrorStatementCount, Statements: statements}
	}

	// pg_query numbers constants in parse-tree traversal order, while
	// pg_stat_statements numbers them in source order around any real bind
	// parameters. Preserve every number the caller supplied and reorder only
	// the placeholders Normalize introduced. This is deliberately narrower
	// than alpha-renumbering all ParamRefs: $1,$1, $1,$2 and $2,$1 remain three
	// different identities.
	original, err := parseJSONTree(sql)
	if err != nil {
		return "", err
	}
	originalParams, err := inspectOriginalParamRefs(original)
	if err != nil {
		return "", err
	}
	if err := renumberSyntheticParamRefs(tree, originalParams); err != nil {
		return "", err
	}

	stripped := stripPositionOnlyFields(tree)
	canonical, err := approval.CanonicalJSON(stripped)
	if err != nil {
		return "", &ParserError{Code: ParserErrorCanonicalize}
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

// parseJSONTree returns pg_query's parse tree without allowing JSON numbers to
// pass through float64. StrictASTDigest hashes this representation, so an
// integer rounded during decoding would silently create another identity.
func parseJSONTree(sql string) (any, error) {
	encoded, err := pg_query.ParseToJSON(sql)
	if err != nil {
		return nil, &ParserError{Code: ParserErrorParse}
	}
	var tree any
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&tree); err != nil {
		return nil, &ParserError{Code: ParserErrorDecode}
	}
	return tree, nil
}

type originalParamRefs struct {
	maximum int64
	counts  map[int64]int
}

// inspectOriginalParamRefs returns the bind parameters present in the original
// SQL. Holes are intentional: if the caller supplies only $2, both $1 and $2
// remain reserved from synthetic renumbering.
func inspectOriginalParamRefs(tree any) (originalParamRefs, error) {
	result := originalParamRefs{counts: map[int64]int{}}
	seenLocations := map[int64]bool{}
	err := visitParamRefs(tree, func(ref map[string]any) error {
		number, err := positiveParamRefField(ref, "number")
		if err != nil {
			return err
		}
		location, err := nonNegativeParamRefField(ref, "location")
		if err != nil || seenLocations[location] {
			return &ParserError{Code: ParserErrorMalformedTree}
		}
		seenLocations[location] = true
		result.counts[number]++
		if number > result.maximum {
			result.maximum = number
		}
		return nil
	})
	return result, err
}

type locatedParamRef struct {
	fields   map[string]any
	number   int64
	location int64
}

// renumberSyntheticParamRefs maps only Normalize-generated parameters (those
// above maxOriginal) into source-location order. PostgreSQL's ParamRef numbers
// are signed 32-bit integers; malformed, missing or ambiguous locations fail
// closed instead of making map/traversal order part of the digest.
func renumberSyntheticParamRefs(tree any, original originalParamRefs) error {
	var synthetic []locatedParamRef
	bindCounts := map[int64]int{}
	allLocations := map[int64]bool{}
	err := visitParamRefs(tree, func(ref map[string]any) error {
		number, err := positiveParamRefField(ref, "number")
		if err != nil {
			return err
		}
		location, err := nonNegativeParamRefField(ref, "location")
		if err != nil || allLocations[location] {
			return &ParserError{Code: ParserErrorMalformedTree}
		}
		allLocations[location] = true
		if number <= original.maximum {
			bindCounts[number]++
			return nil
		}
		synthetic = append(synthetic, locatedParamRef{fields: ref, number: number, location: location})
		return nil
	})
	if err != nil {
		return err
	}
	if !sameParamRefCounts(original.counts, bindCounts) {
		return &ParserError{Code: ParserErrorMalformedTree}
	}
	if original.maximum > (1<<31-1)-int64(len(synthetic)) {
		return &ParserError{Code: ParserErrorMalformedTree}
	}

	seenNumbers := make(map[int64]int64, len(synthetic))
	for _, ref := range synthetic {
		if location, present := seenNumbers[ref.number]; present && location != ref.location {
			return &ParserError{Code: ParserErrorMalformedTree}
		}
		if _, present := seenNumbers[ref.number]; present {
			return &ParserError{Code: ParserErrorMalformedTree}
		}
		seenNumbers[ref.number] = ref.location
	}
	for index := range synthetic {
		if _, present := seenNumbers[original.maximum+int64(index)+1]; !present {
			return &ParserError{Code: ParserErrorMalformedTree}
		}
	}
	sort.Slice(synthetic, func(left, right int) bool {
		return synthetic[left].location < synthetic[right].location
	})
	for index, ref := range synthetic {
		ref.fields["number"] = json.Number(strconv.FormatInt(original.maximum+int64(index)+1, 10))
	}
	return nil
}

func sameParamRefCounts(left, right map[int64]int) bool {
	if len(left) != len(right) {
		return false
	}
	for number, count := range left {
		if right[number] != count {
			return false
		}
	}
	return true
}

func visitParamRefs(node any, visit func(map[string]any) error) error {
	switch value := node.(type) {
	case map[string]any:
		for name, child := range value {
			if name == "ParamRef" {
				ref, ok := child.(map[string]any)
				if !ok {
					return &ParserError{Code: ParserErrorMalformedTree}
				}
				if err := visit(ref); err != nil {
					return err
				}
				continue
			}
			if err := visitParamRefs(child, visit); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := visitParamRefs(child, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func positiveParamRefField(ref map[string]any, name string) (int64, error) {
	value, err := paramRefField(ref, name)
	if err != nil || value <= 0 {
		return 0, &ParserError{Code: ParserErrorMalformedTree}
	}
	return value, nil
}

func nonNegativeParamRefField(ref map[string]any, name string) (int64, error) {
	value, err := paramRefField(ref, name)
	if err != nil || value < 0 {
		return 0, &ParserError{Code: ParserErrorMalformedTree}
	}
	return value, nil
}

func paramRefField(ref map[string]any, name string) (int64, error) {
	raw, present := ref[name]
	if !present {
		return 0, &ParserError{Code: ParserErrorMalformedTree}
	}
	number, ok := raw.(json.Number)
	if !ok {
		return 0, &ParserError{Code: ParserErrorMalformedTree}
	}
	value, err := strconv.ParseInt(number.String(), 10, 32)
	if err != nil {
		return 0, &ParserError{Code: ParserErrorMalformedTree}
	}
	return value, nil
}

// countRawStatements reports how many RawStmt entries the tree carries. A digest
// over two statements would identify a pair rather than a statement, and the
// classifier key would then match executions that never happened together.
func countRawStatements(tree any) (int, error) {
	root, ok := tree.(map[string]any)
	if !ok {
		return 0, &ParserError{Code: ParserErrorMalformedTree}
	}
	statements, present := root["stmts"]
	if !present {
		return 0, &ParserError{Code: ParserErrorNoStatements}
	}
	list, ok := statements.([]any)
	if !ok {
		return 0, &ParserError{Code: ParserErrorMalformedTree}
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
