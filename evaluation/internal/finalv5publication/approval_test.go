package finalv5publication

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateC2Approval(t *testing.T) {
	root := repositoryRoot(t)
	evidence, err := ValidateC2Approval(root)
	if err != nil {
		t.Fatalf("validate source-controlled C2 approval: %v", err)
	}
	if evidence.Approval.SHA256 != C2ApprovalSHA256 || evidence.Candidate.SHA256 != C2CandidateSHA256 {
		t.Fatalf("wrong approval anchors: %+v", evidence)
	}
	if evidence.ApprovalID != "APPROVE-C2-v1.8" || evidence.Author != "wuminmin" ||
		evidence.ApprovedOn != "2026-08-14" || evidence.DecisionNumber != 23 || len(evidence.CompanionFiles) != 3 {
		t.Fatalf("incomplete Decision-20 evidence: %+v", evidence)
	}
	if evidence.CandidateState.Status != "REVIEW_CANDIDATE" || evidence.CandidateState.AuthorApproved ||
		evidence.CandidateState.OutcomeIdentity != "NOT_GENERATED" || evidence.CandidateState.SetAlgebra != "NOT_GENERATED" {
		t.Fatalf("approval evidence lost the pre-generation sequence: %+v", evidence.CandidateState)
	}
	for index := 1; index < len(evidence.CompanionFiles); index++ {
		if evidence.CompanionFiles[index-1].Path >= evidence.CompanionFiles[index].Path {
			t.Fatal("companion evidence is not sorted")
		}
	}
}

func TestValidateC2ApprovalAcceptsTrackedReadOnlyMode(t *testing.T) {
	root := copyC2Fixture(t)
	approval := filepath.Join(root, filepath.FromSlash(C2ApprovalRelativePath))
	if err := os.Chmod(approval, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateC2Approval(root); err != nil {
		t.Fatalf("tracked 0644 approval should remain valid: %v", err)
	}
}

func TestValidateC2ApprovalRejectsStrictJSONViolations(t *testing.T) {
	tests := []struct {
		name string
		add  string
		want string
	}{
		{name: "unknown field", add: `,"unexpected":true`, want: "unknown field"},
		{name: "duplicate field", add: `,"approval_id":"APPROVE-C2"`, want: "duplicate JSON object key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := copyC2Fixture(t)
			path := filepath.Join(root, filepath.FromSlash(C2ApprovalRelativePath))
			value := readFile(t, path)
			writeFile(t, path, insertBeforeFinalObjectEnd(t, value, test.add))
			_, err := ValidateC2Approval(root)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	t.Run("trailing JSON", func(t *testing.T) {
		root := copyC2Fixture(t)
		path := filepath.Join(root, filepath.FromSlash(C2ApprovalRelativePath))
		writeFile(t, path, append(readFile(t, path), []byte("{}\n")...))
		_, err := ValidateC2Approval(root)
		if err == nil || !strings.Contains(err.Error(), "trailing JSON value") {
			t.Fatalf("error = %v, want trailing JSON", err)
		}
	})
	t.Run("candidate unknown field", func(t *testing.T) {
		root := copyC2Fixture(t)
		path := filepath.Join(root, filepath.FromSlash(C2CandidateRelativePath))
		value := readFile(t, path)
		writeFile(t, path, insertBeforeFinalObjectEnd(t, value, `,"unexpected":true`))
		_, err := ValidateC2Approval(root)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("error = %v, want unknown field", err)
		}
	})
}

func TestValidateC2ApprovalRejectsAuthorityAndByteDrift(t *testing.T) {
	t.Run("decision scope drift", func(t *testing.T) {
		root := copyC2Fixture(t)
		path := filepath.Join(root, filepath.FromSlash(C2ApprovalRelativePath))
		value := readFile(t, path)
		value = bytes.Replace(value, []byte(`"scale_cells": 12`), []byte(`"scale_cells": 13`), 1)
		writeFile(t, path, value)
		_, err := ValidateC2Approval(root)
		if err == nil || !strings.Contains(err.Error(), "Decision 20") {
			t.Fatalf("error = %v, want exact Decision 20 rejection", err)
		}
	})
	t.Run("candidate state drift", func(t *testing.T) {
		root := copyC2Fixture(t)
		path := filepath.Join(root, filepath.FromSlash(C2CandidateRelativePath))
		value := readFile(t, path)
		value = bytes.Replace(value, []byte(`"outcome_identity": "NOT_GENERATED"`), []byte(`"outcome_identity": "GENERATED"`), 1)
		writeFile(t, path, value)
		_, err := ValidateC2Approval(root)
		if err == nil || (!strings.Contains(err.Error(), "Decision 20") && !strings.Contains(err.Error(), "pre-generation")) {
			t.Fatalf("error = %v, want candidate-state rejection", err)
		}
	})
	t.Run("companion byte drift", func(t *testing.T) {
		root := copyC2Fixture(t)
		path := filepath.Join(root, filepath.FromSlash(filepath.Join(filepath.Dir(C2CandidateRelativePath), "catalog.yaml")))
		writeFile(t, path, append(readFile(t, path), '\n'))
		_, err := ValidateC2Approval(root)
		if err == nil || !strings.Contains(err.Error(), "differs from review.json") {
			t.Fatalf("error = %v, want descriptor-byte rejection", err)
		}
	})
}

func TestValidateC2ApprovalRejectsUnsafeFilesAndOpenClosure(t *testing.T) {
	t.Run("group writable approval", func(t *testing.T) {
		root := copyC2Fixture(t)
		path := filepath.Join(root, filepath.FromSlash(C2ApprovalRelativePath))
		if err := os.Chmod(path, 0o660); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateC2Approval(root); err == nil {
			t.Fatal("group-writable approval was accepted")
		}
	})
	t.Run("symlink companion", func(t *testing.T) {
		root := copyC2Fixture(t)
		path := filepath.Join(root, filepath.FromSlash(filepath.Join(filepath.Dir(C2CandidateRelativePath), "catalog.yaml")))
		target := filepath.Join(t.TempDir(), "catalog.yaml")
		writeFile(t, target, readFile(t, path))
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := ValidateC2Approval(root); err == nil {
			t.Fatal("symlinked companion was accepted")
		}
	})
	t.Run("extra review entry", func(t *testing.T) {
		root := copyC2Fixture(t)
		writeFile(t, filepath.Join(root, filepath.FromSlash(filepath.Join(filepath.Dir(C2CandidateRelativePath), "extra.json"))), []byte("{}\n"))
		if _, err := ValidateC2Approval(root); err == nil || !strings.Contains(err.Error(), "four-file closure") {
			t.Fatalf("error = %v, want closed-set rejection", err)
		}
	})
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "../../.."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func copyC2Fixture(t *testing.T) string {
	t.Helper()
	source := repositoryRoot(t)
	destination := t.TempDir()
	paths := []string{C2ApprovalRelativePath, C2CandidateRelativePath}
	for _, name := range c2CompanionNames {
		paths = append(paths, filepath.ToSlash(filepath.Join(filepath.Dir(C2CandidateRelativePath), name)))
	}
	for _, relative := range paths {
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, target, readFile(t, filepath.Join(source, filepath.FromSlash(relative))))
	}
	return destination
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func writeFile(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatal(err)
	}
}

func insertBeforeFinalObjectEnd(t *testing.T, value []byte, insertion string) []byte {
	t.Helper()
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || trimmed[len(trimmed)-1] != '}' {
		t.Fatal("fixture is not a JSON object")
	}
	index := bytes.LastIndex(value, []byte("}"))
	result := append([]byte(nil), value[:index]...)
	result = append(result, insertion...)
	result = append(result, value[index:]...)
	return result
}
