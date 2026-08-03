package compilerfixture

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

func TestFrozenCompilerMatrixBuildsAndUsesRealCompiler(t *testing.T) {
	if len(FrozenCells) != 11 {
		t.Fatalf("frozen compiler cells = %d, want 11", len(FrozenCells))
	}
	seen := map[Cell]bool{}
	for _, cell := range FrozenCells {
		if seen[cell] {
			t.Fatalf("duplicate frozen cell: %+v", cell)
		}
		seen[cell] = true
		one, err := Build(cell.WorkloadID, cell.Scale, cell.Mode)
		if err != nil {
			t.Fatalf("build %+v: %v", cell, err)
		}
		compiler, err := one.NewCompiler()
		if err != nil {
			t.Fatalf("construct compiler %+v: %v", cell, err)
		}
		artifact, err := compiler.Compile(one.MeasuredRoot)
		if cell.Mode == "structured_rejection" {
			var structured *viewcompiler.Error
			if !errors.As(err, &structured) {
				t.Fatalf("control %+v returned %T, want *viewcompiler.Error", cell, err)
			}
			want := viewcompiler.CodeDepthLimit
			if cell.Scale == "sources-17" {
				want = viewcompiler.CodeSourceLimit
			}
			if structured.Code != want {
				t.Fatalf("control %+v code = %q, want %q", cell, structured.Code, want)
			}
			continue
		}
		if err != nil {
			t.Fatalf("compile %+v: %v", cell, err)
		}
		if artifact.CanonicalPlanDigest == "" || len(artifact.BaseProducts) != one.ExpectedSources {
			t.Fatalf("compile %+v returned incomplete artifact", cell)
		}

		semantic := map[string]viewcompiler.Artifact{}
		for name, root := range one.SemanticRoots {
			compiled, compileErr := compiler.Compile(root)
			if compileErr != nil {
				t.Fatalf("compile %+v variant %s: %v", cell, name, compileErr)
			}
			semantic[name] = compiled
		}
		direct := semantic["direct"]
		for name, compiled := range semantic {
			if compiled.CanonicalPlanDigest != direct.CanonicalPlanDigest ||
				compiled.InterfaceDigest != direct.InterfaceDigest ||
				!reflect.DeepEqual(compiled.Plan, direct.Plan) ||
				!reflect.DeepEqual(compiled.Outputs, direct.Outputs) ||
				!reflect.DeepEqual(compiled.BaseProducts, direct.BaseProducts) {
				t.Fatalf("semantic variant %s drifted in %+v", name, cell)
			}
		}
	}
}

func TestExpectedVariantNamesAreExact(t *testing.T) {
	for _, cell := range FrozenCells {
		one, err := Build(cell.WorkloadID, cell.Scale, cell.Mode)
		if err != nil {
			t.Fatal(err)
		}
		names := one.ExpectedVariantNames()
		if cell.Mode == "structured_rejection" {
			if len(names) != 0 {
				t.Fatalf("control variants = %v", names)
			}
			continue
		}
		want := []string{"alias", "allocation", "direct", "measured", "nested", "parenthesized", "repeat"}
		if cell.WorkloadID == "join-sources" {
			want = append(want, "join_order")
			sort.Strings(want)
		}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("%+v variants = %v, want %v", cell, names, want)
		}
	}
}

func TestFixtureSQLDigestIsFrozen(t *testing.T) {
	path := filepath.Join("..", "..", "..", "db", "init", "08-final-v5-compiler-fixture.sql")
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := SHA256Bytes(bytes); got != FixtureSQLSHA256 {
		t.Fatalf("fixture SQL SHA-256 = %s, want %s", got, FixtureSQLSHA256)
	}
}

func TestUnsupportedCompilerCellFailsClosed(t *testing.T) {
	if _, err := Build("view-depth", "17", "compile"); err == nil {
		t.Fatal("unsupported performance point was accepted")
	}
}
