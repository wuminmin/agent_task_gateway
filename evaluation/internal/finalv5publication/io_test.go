package finalv5publication

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteClosedOutputDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "candidate")
	names := []string{"publication-binding.provenance.json", "catalog.yaml", "publication-binding.json"}
	files := map[string][]byte{
		"catalog.yaml":                        []byte("version: test\n"),
		"publication-binding.json":            []byte("{\"version\":\"test\"}\n"),
		"publication-binding.provenance.json": []byte("{\"version\":\"test-provenance\"}\n"),
	}
	if err := WriteClosedOutputDirectory(path, files, names); err != nil {
		t.Fatalf("write closed output: %v", err)
	}
	if err := ValidateClosedOutputDirectory(path, names); err != nil {
		t.Fatalf("validate closed output: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("output directory mode = %v, err=%v", info.Mode(), err)
	}
	for name, want := range files {
		filePath := filepath.Join(path, name)
		info, err := os.Lstat(filePath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, err=%v", name, info.Mode(), err)
		}
		if got := readFile(t, filePath); string(got) != string(want) {
			t.Fatalf("%s bytes changed", name)
		}
	}
}

func TestWriteClosedOutputDirectoryRejectsExistingTargets(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "candidate")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(path, "sentinel")
		writeFile(t, sentinel, []byte("preserve"))
		if err := WriteClosedOutputDirectory(path, map[string][]byte{"a.json": []byte("{}\n")}, []string{"a.json"}); err == nil {
			t.Fatal("existing directory was overwritten")
		}
		if got := readFile(t, sentinel); string(got) != "preserve" {
			t.Fatal("existing directory contents changed")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "candidate")
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := WriteClosedOutputDirectory(path, map[string][]byte{"a.json": []byte("{}\n")}, []string{"a.json"}); err == nil {
			t.Fatal("existing output symlink was followed")
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("refused output symlink was changed")
		}
		entries, err := os.ReadDir(target)
		if err != nil || len(entries) != 0 {
			t.Fatal("symlink target received output")
		}
	})
}

func TestWriteClosedOutputDirectoryRejectsOpenOrInvalidSets(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string][]byte
		allowed []string
	}{
		{name: "missing", files: map[string][]byte{"a": []byte("a")}, allowed: []string{"a", "b"}},
		{name: "extra", files: map[string][]byte{"a": []byte("a"), "b": []byte("b")}, allowed: []string{"a"}},
		{name: "duplicate allowed", files: map[string][]byte{"a": []byte("a")}, allowed: []string{"a", "a"}},
		{name: "path traversal", files: map[string][]byte{"../a": []byte("a")}, allowed: []string{"../a"}},
		{name: "empty payload", files: map[string][]byte{"a": nil}, allowed: []string{"a"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "candidate")
			if err := WriteClosedOutputDirectory(path, test.files, test.allowed); err == nil {
				t.Fatal("invalid closed set was written")
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid input created output: %v", err)
			}
		})
	}
}

func TestValidateClosedOutputDirectoryRejectsModeClosureAndSymlink(t *testing.T) {
	newOutput := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "candidate")
		if err := WriteClosedOutputDirectory(path, map[string][]byte{"a": []byte("a")}, []string{"a"}); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Run("wrong mode", func(t *testing.T) {
		path := newOutput(t)
		if err := os.Chmod(filepath.Join(path, "a"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateClosedOutputDirectory(path, []string{"a"}); err == nil {
			t.Fatal("non-0600 output was accepted")
		}
	})
	t.Run("extra file", func(t *testing.T) {
		path := newOutput(t)
		writeFile(t, filepath.Join(path, "b"), []byte("b"))
		if err := ValidateClosedOutputDirectory(path, []string{"a"}); err == nil {
			t.Fatal("open output closure was accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		path := newOutput(t)
		target := filepath.Join(t.TempDir(), "target")
		writeFile(t, target, []byte("a"))
		if err := os.Remove(filepath.Join(path, "a")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(path, "a")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := ValidateClosedOutputDirectory(path, []string{"a"}); err == nil {
			t.Fatal("symlinked output was accepted")
		}
	})
}
