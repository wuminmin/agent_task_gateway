package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var embeddedSourceDigest string

var boundSourceRoots = []string{
	"evaluation/cmd/v4-concurrency",
	"internal/control",
	"internal/gateway",
	"internal/ordinal",
	"internal/exposure",
	"internal/queryplan",
	"internal/semanticcache",
}

var boundSourceFiles = []string{
	"go.mod",
	"go.sum",
	"compose.yaml",
	"evaluation/v4-concurrency/catalog.yaml",
	"evaluation/v4-concurrency/compose.yaml",
	"evaluation/v4-concurrency/template.json",
}

func sourceDigest() string {
	if embeddedSourceDigest != "" {
		if validDigest(embeddedSourceDigest) {
			return embeddedSourceDigest
		}
		return ""
	}
	root := findRepositoryRoot()
	if root == "" {
		return ""
	}
	value, err := sourceDigestFromRoot(root)
	if err != nil {
		return ""
	}
	return value
}

func sourceDigestFromRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New("repository root is empty")
	}
	var paths []string
	for _, relativeRoot := range boundSourceRoots {
		count := 0
		err := filepath.WalkDir(filepath.Join(root, relativeRoot), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || (!strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".sql")) {
				return nil
			}
			paths = append(paths, path)
			count++
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("walk %s: %w", relativeRoot, err)
		}
		if count == 0 {
			return "", fmt.Errorf("bound source root %s is empty", relativeRoot)
		}
	}
	for _, relative := range boundSourceFiles {
		paths = append(paths, filepath.Join(root, relative))
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
		_, _ = hash.Write([]byte(filepath.ToSlash(relative)))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(raw)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func findRepositoryRoot() string {
	working, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if info, err := os.Stat(filepath.Join(working, "go.mod")); err == nil && info.Mode().IsRegular() {
			return working
		}
		parent := filepath.Dir(working)
		if parent == working {
			return ""
		}
		working = parent
	}
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
