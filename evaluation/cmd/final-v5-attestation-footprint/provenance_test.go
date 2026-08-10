package main

import "testing"

func TestQualificationSourceClosurePinsTheStrictIdentityImplementation(t *testing.T) {
	want := map[string]bool{
		"evaluation/internal/experiment/strict_ast.go": false,
		"internal/approval/protocol.go":                false,
		"internal/dataconnector/pins.go":               false,
		"internal/dataconnector/statements.go":         false,
		"internal/sqlidentity/strict_ast.go":           false,
	}
	seen := map[string]bool{}
	for _, path := range requiredSourcePaths() {
		if seen[path] {
			t.Fatalf("qualification source closure lists %s twice", path)
		}
		seen[path] = true
		if _, required := want[path]; required {
			want[path] = true
		}
	}
	for path, present := range want {
		if !present {
			t.Errorf("qualification source closure omits %s", path)
		}
	}
}
