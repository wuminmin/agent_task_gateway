package finalv5profile

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// buildSourceArtifacts writes a source artifact root holding several
// publications, the way one compiler pass produces them for a whole campaign.
func buildSourceArtifacts(t *testing.T, publications ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, publication := range publications {
		directory := filepath.Join(root, publication)
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range publicationBundleFiles(publication) {
			if err := os.WriteFile(filepath.Join(directory, name),
				[]byte("bytes-of-"+name), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func testProfile(publications ...string) Profile {
	return Profile{ID: "profile-0123456789abcdef", Alias: "test-profile",
		Closure:       Closure{Publications: publications, SHA256: strings.Repeat("a", 64)},
		CatalogSHA256: strings.Repeat("b", 64), TotalHotBytes: 1024}
}

func TestMaterializeCopiesExactlyTheClosure(t *testing.T) {
	source := buildSourceArtifacts(t, "expense-detail-v1", "final-v5-result-heavy-v1", "provsql-orders-v1")
	destination := t.TempDir()
	profile := testProfile("final-v5-result-heavy-v1")
	manifest, err := MaterializeProfileArtifacts(profile, "final-v5-contracts-v1.2", source, destination,
		strings.Repeat("c", 64))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	directory := filepath.Join(destination, profile.ID)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "final-v5-result-heavy-v1" {
		t.Fatalf("profile directory holds %d entries", len(entries))
	}
	if manifest.TotalFiles != 4 || manifest.DirectorySHA256 == "" {
		t.Fatalf("manifest = %+v", manifest)
	}
	// Every file is a regular, read-only copy -- never a symlink.
	if err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s is not a regular file", path)
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("%s is writable by the mounting runtime", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyProfileArtifactDirectory(directory, manifest); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestMaterializePublicationDirectoryModeIgnoresProcessUmask(t *testing.T) {
	source := buildSourceArtifacts(t, "final-v5-result-heavy-v1")
	destination := t.TempDir()
	profile := testProfile("final-v5-result-heavy-v1")

	previousUmask := syscall.Umask(0o077)
	defer syscall.Umask(previousUmask)
	if _, err := MaterializeProfileArtifacts(profile, "final-v5-contracts-v1.2", source, destination,
		strings.Repeat("c", 64)); err != nil {
		t.Fatalf("materialize under umask 077: %v", err)
	}
	publication := filepath.Join(destination, profile.ID, "final-v5-result-heavy-v1")
	info, err := os.Lstat(publication)
	if err != nil {
		t.Fatalf("stat materialized publication: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("materialized publication mode = %v, want a real directory", info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("materialized publication mode = %04o, want 0755 under umask 077", got)
	}
}

// A materialized directory must not become a hardlink or symlink farm: editing
// the source afterwards must not change an already frozen profile.
func TestMaterializedCopyIsIndependentOfTheSource(t *testing.T) {
	source := buildSourceArtifacts(t, "expense-detail-v1")
	destination := t.TempDir()
	profile := testProfile("expense-detail-v1")
	manifest, err := MaterializeProfileArtifacts(profile, "final-v5-contracts-v1.2", source, destination,
		strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(source, "expense-detail-v1", "expense-detail-v1.hot.tgord")
	if err := os.WriteFile(victim, []byte("tampered-source-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyProfileArtifactDirectory(filepath.Join(destination, profile.ID), manifest); err != nil {
		t.Fatalf("a source edit changed the frozen profile directory: %v", err)
	}
}

func TestMaterializeFailsClosed(t *testing.T) {
	t.Run("missing publication in source", func(t *testing.T) {
		source := buildSourceArtifacts(t, "expense-detail-v1")
		if _, err := MaterializeProfileArtifacts(testProfile("final-v5-result-heavy-v1"),
			"final-v5-contracts-v1.2", source, t.TempDir(), strings.Repeat("c", 64)); err == nil {
			t.Fatal("a closure whose Publication is absent was materialized")
		}
	})
	t.Run("incomplete bundle in source", func(t *testing.T) {
		source := buildSourceArtifacts(t, "expense-detail-v1")
		if err := os.Remove(filepath.Join(source, "expense-detail-v1", "expense-detail-v1.cold.tgord")); err != nil {
			t.Fatal(err)
		}
		if _, err := MaterializeProfileArtifacts(testProfile("expense-detail-v1"),
			"final-v5-contracts-v1.2", source, t.TempDir(), strings.Repeat("c", 64)); err == nil {
			t.Fatal("an incomplete source bundle was materialized")
		}
	})
	t.Run("symlinked source publication", func(t *testing.T) {
		source := buildSourceArtifacts(t, "expense-detail-v1")
		link := filepath.Join(source, "linked-v1")
		if err := os.Symlink(filepath.Join(source, "expense-detail-v1"), link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := MaterializeProfileArtifacts(testProfile("linked-v1"),
			"final-v5-contracts-v1.2", source, t.TempDir(), strings.Repeat("c", 64)); err == nil {
			t.Fatal("a symlinked source publication was materialized")
		}
	})
	t.Run("empty closure", func(t *testing.T) {
		if _, err := MaterializeProfileArtifacts(testProfile(), "final-v5-contracts-v1.2",
			buildSourceArtifacts(t, "expense-detail-v1"), t.TempDir(), strings.Repeat("c", 64)); err == nil {
			t.Fatal("a closure with no Publication was materialized")
		}
	})
	t.Run("unsafe profile id", func(t *testing.T) {
		profile := testProfile("expense-detail-v1")
		profile.ID = "../escape"
		if _, err := MaterializeProfileArtifacts(profile, "final-v5-contracts-v1.2",
			buildSourceArtifacts(t, "expense-detail-v1"), t.TempDir(), strings.Repeat("c", 64)); err == nil {
			t.Fatal("a traversing profile id was accepted")
		}
	})
}

// An existing directory may be reused only when it is byte-identical, and is
// never overwritten or merged.
func TestMaterializeReusesOnlyAnIdenticalDirectory(t *testing.T) {
	source := buildSourceArtifacts(t, "expense-detail-v1")
	destination := t.TempDir()
	profile := testProfile("expense-detail-v1")
	first, err := MaterializeProfileArtifacts(profile, "final-v5-contracts-v1.2", source, destination,
		strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, err := MaterializeProfileArtifacts(profile, "final-v5-contracts-v1.2", source, destination,
		strings.Repeat("c", 64))
	if err != nil {
		t.Fatalf("an identical directory was not reused: %v", err)
	}
	if first.DirectorySHA256 != second.DirectorySHA256 {
		t.Fatal("materialization is not deterministic")
	}
	// Tamper with the already materialized directory: reuse must now refuse.
	victim := filepath.Join(destination, profile.ID, "expense-detail-v1", "expense-detail-v1.hot.tgord")
	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeProfileArtifacts(profile, "final-v5-contracts-v1.2", source, destination,
		strings.Repeat("c", 64)); err == nil {
		t.Fatal("a tampered profile directory was reused")
	}
}

// Verification must reject an extra publication, a missing file and a tampered
// byte in an already materialized directory.
func TestVerifyProfileArtifactDirectoryFailsClosed(t *testing.T) {
	source := buildSourceArtifacts(t, "expense-detail-v1", "provsql-orders-v1")
	destination := t.TempDir()
	profile := testProfile("expense-detail-v1")
	manifest, err := MaterializeProfileArtifacts(profile, "final-v5-contracts-v1.2", source, destination,
		strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(destination, profile.ID)

	t.Run("extra publication", func(t *testing.T) {
		extra := filepath.Join(directory, "provsql-orders-v1")
		if err := os.Mkdir(extra, 0o755); err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(extra)
		if err := VerifyProfileArtifactDirectory(directory, manifest); err == nil {
			t.Fatal("an extra publication was accepted")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		victim := filepath.Join(directory, "expense-detail-v1", "expense-detail-v1.sidecar.ndjson")
		saved, err := os.ReadFile(victim)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(victim); err != nil {
			t.Fatal(err)
		}
		defer os.WriteFile(victim, saved, artifactFileMode)
		if err := VerifyProfileArtifactDirectory(directory, manifest); err == nil {
			t.Fatal("a missing bundle file was accepted")
		}
	})
	t.Run("tampered bytes", func(t *testing.T) {
		victim := filepath.Join(directory, "expense-detail-v1", "expense-detail-v1.bundle.json")
		saved, err := os.ReadFile(victim)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(victim, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(victim, []byte("tampered"), 0o644); err != nil {
			t.Fatal(err)
		}
		defer func() {
			os.Chmod(victim, 0o644)
			os.WriteFile(victim, saved, 0o644)
			os.Chmod(victim, artifactFileMode)
		}()
		if err := VerifyProfileArtifactDirectory(directory, manifest); err == nil {
			t.Fatal("tampered bytes were accepted")
		}
	})
}
