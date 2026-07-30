package v4oracle

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed cold.go oracle.go sorter.go source.go types.go
var oracleSources embed.FS

func SourceSHA256() string {
	entries, err := oracleSources.ReadDir(".")
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	hashValue := sha256.New()
	for _, name := range names {
		raw, err := oracleSources.ReadFile(name)
		if err != nil {
			return ""
		}
		writeStringHash(hashValue, name)
		writeBytesHash(hashValue, raw)
	}
	return hex.EncodeToString(hashValue.Sum(nil))
}

func executableSHA256() string {
	file, err := os.Open("/proc/self/exe")
	if err != nil {
		return ""
	}
	defer file.Close()
	hashValue := sha256.New()
	if _, err := io.Copy(hashValue, file); err != nil {
		return ""
	}
	return hex.EncodeToString(hashValue.Sum(nil))
}

var repositorySourceRoots = []string{
	"evaluation/cmd/v4-million-oracle",
	"evaluation/v4oracle",
	"internal/exposure",
	"internal/ordinal",
	"internal/queryplan",
	"internal/snapshotbundle",
}

var repositorySourceFiles = []string{"go.mod", "go.sum"}

// repositorySourceDigest binds every first-party source file used by the
// oracle's canonical encoders, bitmap decoder, normal form, and COLD envelope.
// Path framing prevents two different directory layouts from colliding.
func repositorySourceDigest(root string) (string, int, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", 0, os.ErrInvalid
	}
	var paths []string
	for _, relativeRoot := range repositorySourceRoots {
		base := filepath.Join(root, filepath.FromSlash(relativeRoot))
		count := 0
		err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return os.ErrInvalid
			}
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
				paths = append(paths, path)
				count++
			}
			return nil
		})
		if err != nil || count == 0 {
			return "", 0, os.ErrInvalid
		}
	}
	for _, relative := range repositorySourceFiles {
		paths = append(paths, filepath.Join(root, relative))
	}
	sort.Strings(paths)
	hashValue := sha256.New()
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", 0, os.ErrInvalid
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", 0, err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || strings.HasPrefix(relative, "..") {
			return "", 0, os.ErrInvalid
		}
		writeStringHash(hashValue, filepath.ToSlash(relative))
		writeBytesHash(hashValue, raw)
	}
	return hex.EncodeToString(hashValue.Sum(nil)), len(paths), nil
}
