package sqlidentity

import (
	"encoding/json"
	"strconv"
	"testing"
)

func paramRefForTest(number int64, location any) map[string]any {
	ref := map[string]any{"number": json.Number(strconv.FormatInt(number, 10))}
	if location != nil {
		ref["location"] = location
	}
	return map[string]any{"ParamRef": ref}
}

func locationForTest(value int64) json.Number { return json.Number(strconv.FormatInt(value, 10)) }

func TestRenumberSyntheticParamRefsPreservesBindsAndUsesSourceOrder(t *testing.T) {
	originalTree := []any{
		paramRefForTest(2, locationForTest(10)),
		paramRefForTest(1, locationForTest(20)),
	}
	original, err := inspectOriginalParamRefs(originalTree)
	if err != nil {
		t.Fatal(err)
	}
	refs := []any{
		paramRefForTest(2, locationForTest(5)),
		paramRefForTest(4, locationForTest(10)),
		paramRefForTest(1, locationForTest(15)),
		paramRefForTest(3, locationForTest(20)),
	}
	if err := renumberSyntheticParamRefs(refs, original); err != nil {
		t.Fatal(err)
	}
	want := []string{"2", "3", "1", "4"}
	for index, node := range refs {
		got := node.(map[string]any)["ParamRef"].(map[string]any)["number"].(json.Number).String()
		if got != want[index] {
			t.Fatalf("ParamRef %d number = %s, want %s", index, got, want[index])
		}
	}
}

func TestParamRefNormalizationFailsClosedOnMalformedTrees(t *testing.T) {
	validOriginal, err := inspectOriginalParamRefs([]any{paramRefForTest(1, locationForTest(1))})
	if err != nil {
		t.Fatal(err)
	}

	for name, testCase := range map[string]struct {
		original *originalParamRefs
		tree     any
	}{
		"missing location": {
			original: &validOriginal,
			tree:     []any{paramRefForTest(1, locationForTest(1)), paramRefForTest(2, nil)},
		},
		"negative location": {
			original: &validOriginal,
			tree:     []any{paramRefForTest(1, locationForTest(1)), paramRefForTest(2, locationForTest(-1))},
		},
		"duplicate location": {
			original: &validOriginal,
			tree:     []any{paramRefForTest(1, locationForTest(1)), paramRefForTest(2, locationForTest(1))},
		},
		"duplicate generated number": {
			original: &validOriginal,
			tree: []any{paramRefForTest(1, locationForTest(1)), paramRefForTest(2, locationForTest(2)),
				paramRefForTest(2, locationForTest(3))},
		},
		"gapped generated numbers": {
			original: &validOriginal,
			tree:     []any{paramRefForTest(1, locationForTest(1)), paramRefForTest(3, locationForTest(2))},
		},
		"bind multiplicity changed": {
			original: &validOriginal,
			tree:     []any{paramRefForTest(2, locationForTest(2))},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := renumberSyntheticParamRefs(testCase.tree, *testCase.original); err == nil || ErrorCode(err) != ParserErrorMalformedTree {
				t.Fatalf("malformed tree error = %v, want %s", err, ParserErrorMalformedTree)
			}
		})
	}

	for name, tree := range map[string]any{
		"original missing location":   []any{paramRefForTest(1, nil)},
		"original duplicate location": []any{paramRefForTest(1, locationForTest(1)), paramRefForTest(2, locationForTest(1))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := inspectOriginalParamRefs(tree); err == nil || ErrorCode(err) != ParserErrorMalformedTree {
				t.Fatalf("malformed original error = %v, want %s", err, ParserErrorMalformedTree)
			}
		})
	}
}
