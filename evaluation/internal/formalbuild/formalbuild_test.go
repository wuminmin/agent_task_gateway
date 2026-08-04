package formalbuild

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func regular(path, content string) Entry {
	return Entry{Path: path, Mode: gitModeRegular, ContentSHA256: digestOf(content)}
}

func TestContextDigestIsOrderIndependentButContentSensitive(t *testing.T) {
	forward := []Entry{regular("a.go", "package a"), regular("b.go", "package b")}
	reversed := []Entry{regular("b.go", "package b"), regular("a.go", "package a")}

	first, err := ContextDigest(forward)
	if err != nil {
		t.Fatalf("ContextDigest: %v", err)
	}
	second, err := ContextDigest(reversed)
	if err != nil {
		t.Fatalf("ContextDigest: %v", err)
	}
	if first != second {
		t.Fatalf("the context digest depends on enumeration order: %s vs %s", first, second)
	}

	// Mode is load-bearing: a script losing its executable bit changes what the
	// image does with identical bytes.
	executable := []Entry{regular("a.go", "package a"), {Path: "b.go", Mode: gitModeExecutable,
		ContentSHA256: digestOf("package b")}}
	third, err := ContextDigest(executable)
	if err != nil {
		t.Fatalf("ContextDigest: %v", err)
	}
	if third == first {
		t.Fatal("the context digest ignores file mode; a lost executable bit would be invisible")
	}
}

func TestContextDigestCannotBeCollidedByPathConcatenation(t *testing.T) {
	// Without length framing "a" + "b/c" and "a/b" + "c" could hash alike.
	left, err := ContextDigest([]Entry{regular("a", "x"), regular("b/c", "y")})
	if err != nil {
		t.Fatalf("ContextDigest: %v", err)
	}
	right, err := ContextDigest([]Entry{regular("a/b", "x"), regular("c", "y")})
	if err != nil {
		t.Fatalf("ContextDigest: %v", err)
	}
	if left == right {
		t.Fatal("two distinct contexts collided; the digest is not unambiguously framed")
	}
}

func TestSourceManifestDigestIgnoresModeButNotContent(t *testing.T) {
	plain := []Entry{regular("run.sh", "#!/bin/sh")}
	executableEntry := []Entry{{Path: "run.sh", Mode: gitModeExecutable, ContentSHA256: digestOf("#!/bin/sh")}}
	first, err := SourceManifestDigest(plain)
	if err != nil {
		t.Fatalf("SourceManifestDigest: %v", err)
	}
	second, err := SourceManifestDigest(executableEntry)
	if err != nil {
		t.Fatalf("SourceManifestDigest: %v", err)
	}
	if first != second {
		t.Fatal("the source manifest is supposed to bind path and content only")
	}
	changed, err := SourceManifestDigest([]Entry{regular("run.sh", "#!/bin/bash")})
	if err != nil {
		t.Fatalf("SourceManifestDigest: %v", err)
	}
	if changed == first {
		t.Fatal("the source manifest ignores content")
	}
}

func TestEntryValidationRejectsUnsafeAndUnsupportedEntries(t *testing.T) {
	for name, entry := range map[string]Entry{
		"absolute path":      {Path: "/etc/passwd", Mode: gitModeRegular, ContentSHA256: digestOf("x")},
		"parent traversal":   {Path: "../outside", Mode: gitModeRegular, ContentSHA256: digestOf("x")},
		"embedded traversal": {Path: "a/../../b", Mode: gitModeRegular, ContentSHA256: digestOf("x")},
		"empty path":         {Path: "", Mode: gitModeRegular, ContentSHA256: digestOf("x")},
		"submodule":          {Path: "vendored", Mode: gitModeGitlink, ContentSHA256: digestOf("x")},
		"unknown mode":       {Path: "a", Mode: "100777", ContentSHA256: digestOf("x")},
		"no digest":          {Path: "a", Mode: gitModeRegular, ContentSHA256: ""},
		"symlink without target": {Path: "a", Mode: gitModeSymlink,
			ContentSHA256: digestOf("x")},
		"target without symlink mode": {Path: "a", Mode: gitModeRegular,
			ContentSHA256: digestOf("x"), SymlinkTarget: "b"},
	} {
		if err := entry.validate(); err == nil {
			t.Errorf("%s: a formal context accepted an entry it must reject", name)
		}
	}
}

func TestRequireSameContextNamesWhatLeakedIn(t *testing.T) {
	committed := []Entry{regular("main.go", "package main")}
	withUntracked := append(append([]Entry(nil), committed...), regular("secret.env", "TOKEN=1"))

	err := RequireSameContext(committed, withUntracked)
	if err == nil {
		t.Fatal("an untracked file entered the context without being rejected")
	}
	if !strings.Contains(err.Error(), "secret.env") {
		t.Fatalf("the rejection does not name the offending path: %v", err)
	}

	err = RequireSameContext(withUntracked, committed)
	if err == nil {
		t.Fatal("a context missing a tracked file was accepted")
	}
	if !strings.Contains(err.Error(), "secret.env") {
		t.Fatalf("the rejection does not name the missing path: %v", err)
	}

	edited := []Entry{regular("main.go", "package main // edited")}
	if err := RequireSameContext(committed, edited); err == nil {
		t.Fatal("a context whose file content differs from the commit was accepted")
	}
}

func TestEntriesFromArchiveSkipsDirectoriesAndCarriesSymlinks(t *testing.T) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: "pkg/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatalf("write dir header: %v", err)
	}
	body := "package pkg"
	if err := writer.WriteHeader(&tar.Header{Name: "pkg/a.go", Typeflag: tar.TypeReg,
		Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatalf("write file header: %v", err)
	}
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatalf("write file body: %v", err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink,
		Linkname: "pkg/a.go", Mode: 0o777}); err != nil {
		t.Fatalf("write symlink header: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}

	entries, err := EntriesFromArchive(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatalf("EntriesFromArchive: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (the directory must not be digested)", len(entries))
	}
	var link Entry
	for _, entry := range entries {
		if entry.Path == "link" {
			link = entry
		}
	}
	if link.Mode != gitModeSymlink || link.SymlinkTarget != "pkg/a.go" {
		t.Fatalf("the symlink target was not carried: %+v", link)
	}
}

// --- repository-backed tests -------------------------------------------------

func git(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(output))
}

// newRepository builds a repository with an origin, so the published-commit
// check has something real to resolve.
func newRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatalf("mkdir origin: %v", err)
	}
	git(t, base, "init", "--bare", "--initial-branch=main", origin)
	git(t, base, "clone", origin, work)
	git(t, work, "config", "user.name", "test")
	git(t, work, "config", "user.email", "test@example.invalid")

	write(t, work, "go.mod", "module example.invalid\n")
	write(t, work, "cmd/gateway/main.go", "package main\n\nfunc main() {}\n")
	write(t, work, ".gitignore", "ignored/\n")
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "initial")
	git(t, work, "push", "-u", "origin", "main")
	return work
}

func write(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relative, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", relative, err)
	}
}

func TestMaterializeHeadExcludesUntrackedAndIgnoredFiles(t *testing.T) {
	root := newRepository(t)
	clean, err := MaterializeHead(root)
	if err != nil {
		t.Fatalf("MaterializeHead: %v", err)
	}

	// An untracked file makes the tree dirty, which is refused outright.
	write(t, root, "leaked.txt", "host secret")
	if _, err := MaterializeHead(root); err == nil {
		t.Fatal("a formal context was materialized from a tree with an untracked file")
	}
	if err := os.Remove(filepath.Join(root, "leaked.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// An ignored file leaves the tree clean, so this is the case that proves the
	// context is built from the commit rather than from the directory.
	write(t, root, "ignored/build-artifact.bin", "stale binary")
	withIgnored, err := MaterializeHead(root)
	if err != nil {
		t.Fatalf("MaterializeHead with an ignored file: %v", err)
	}
	if withIgnored.ContextSHA256 != clean.ContextSHA256 {
		t.Fatal("an ignored host file changed the formal build context digest")
	}
	for _, entry := range withIgnored.Entries {
		if strings.HasPrefix(entry.Path, "ignored/") {
			t.Fatalf("an ignored host file entered the formal build context: %s", entry.Path)
		}
	}
}

func TestMaterializeHeadRefusesDirtyAndUnpublishedTrees(t *testing.T) {
	root := newRepository(t)

	write(t, root, "cmd/gateway/main.go", "package main\n\nfunc main() { println(1) }\n")
	if _, err := MaterializeHead(root); err == nil {
		t.Fatal("a formal context was materialized from a modified tracked file")
	} else if !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("the failure does not name the dirty tree: %v", err)
	}

	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "unpublished change")
	if _, err := MaterializeHead(root); err == nil {
		t.Fatal("a formal context was materialized from an unpublished commit")
	} else if !strings.Contains(err.Error(), "published") {
		t.Fatalf("the failure does not name the unpublished commit: %v", err)
	}

	git(t, root, "push", "origin", "main")
	if _, err := MaterializeHead(root); err != nil {
		t.Fatalf("a clean published commit was refused: %v", err)
	}
}

func TestMaterializedContextChangesWithATrackedFile(t *testing.T) {
	root := newRepository(t)
	before, err := MaterializeHead(root)
	if err != nil {
		t.Fatalf("MaterializeHead: %v", err)
	}
	write(t, root, "cmd/gateway/main.go", "package main\n\nfunc main() { println(2) }\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "change")
	git(t, root, "push", "origin", "main")
	after, err := MaterializeHead(root)
	if err != nil {
		t.Fatalf("MaterializeHead: %v", err)
	}
	if after.ContextSHA256 == before.ContextSHA256 {
		t.Fatal("changing a tracked file left the build-context digest unchanged")
	}
	if after.SourceManifestSHA256 == before.SourceManifestSHA256 {
		t.Fatal("changing a tracked file left the source-manifest digest unchanged")
	}
}
