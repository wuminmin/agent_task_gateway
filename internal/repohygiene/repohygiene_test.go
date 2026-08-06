// Package repohygiene holds repository-level invariants that no single
// component owns but that every contributor can break by accident.
//
// # Why this guard is layered
//
// A committed root-level build artifact has now happened twice: a 24 MB
// final-v5-attestation-footprint executable in c541d7e, which prompted the
// first version of this file, and a 3 MB final-v5-dbtest-report executable in
// f1d5aa1, committed with that guard already present and reporting success.
//
// The second escape was not a coverage hole. Run uncached against f1d5aa1 the
// original guard fails correctly. The Go test cache hid it. The rule reads the
// tracked file list out of a `git ls-files` subprocess, and a subprocess's
// output is not part of the cache key, so a commit that stages a binary without
// touching this file leaves the previous PASS in place and `go test ./...`
// replays `ok (cached)`.
//
// Pinning the cache to the git index does not fix it. cmd/go hashes an opened
// path only when it lies inside the module root (cmd/go/internal/test.test.go,
// the "open" case: paths outside Package.Root are skipped, not treated as
// uncacheable). In a linked worktree .git is a pointer file and the real index
// lives under the main checkout, outside the module, so the index can never
// become a cache input. Staging is therefore invisible to `go test` by
// construction, and no single test can close this.
//
// Hence three layers:
//
//   - TestRepositoryRootCarriesNoTrackedBuildOutput is the authoritative rule
//     over the git index. It is exact on a cold cache, which is what CI and any
//     fresh clone run.
//   - TestRepositoryRootCarriesNoUnignoredBuildOutput works over the worktree
//     and catches a new command's binary the moment it is built, before it can
//     be staged at all. Its inputs are the root directory and .gitignore, both
//     inside the module root, so this layer is genuinely cache-correct.
//   - `make hygiene` runs the package with -count=1 so the gate never consults
//     the cache. That is the layer that closes the remaining hole: force-adding
//     a path that .gitignore already covers changes neither the root directory
//     nor any file the cache can see.
package repohygiene

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// buildOutputMagic are the leading bytes of the executable formats a Go build
// can leave behind on the platforms this repository is developed on.
var buildOutputMagic = map[string][]byte{
	"ELF (Linux)":            {0x7f, 'E', 'L', 'F'},
	"PE (Windows)":           {'M', 'Z'},
	"Mach-O 64-bit":          {0xcf, 0xfa, 0xed, 0xfe},
	"Mach-O 64-bit (LE)":     {0xfe, 0xed, 0xfa, 0xcf},
	"Mach-O universal":       {0xca, 0xfe, 0xba, 0xbe},
	"Go object/archive file": {'!', '<', 'a', 'r', 'c', 'h', '>'},
}

// finding is one violation of the repository-root invariant. Detection is a
// pure function of a path's name, bytes and mode so that the rule can be proven
// against a synthetic build artifact without committing one -- see
// TestForceAddedBuildArtifactIsRejected.
type finding struct {
	path   string
	reason string
	// status is how the path reached the root -- tracked, or untracked and
	// unignored. The detector cannot know it; the layer that found it sets it.
	status string
}

// String carries the remediation. A guard that only says "no" costs the next
// contributor a detour through the Makefile; this names the command that
// produces the same binary somewhere ignored.
func (f finding) String() string {
	return fmt.Sprintf("%s %s and %s.\n"+
		"Go writes a main package's executable into the working directory, so building "+
		"from the repository root leaves it where `git add -A` sweeps it into a commit.\n"+
		"Remediation:\n"+
		"    make bin                    # every command, into generated/bin (ignored)\n"+
		"    go build -o generated/bin/ ./evaluation/cmd/%s\n"+
		"    git rm --cached %s && rm %s\n"+
		"then add /%s to .gitignore.",
		f.path, f.status, f.reason, f.path, f.path, f.path, f.path)
}

// inspectRootFile reports why a repository-root path is not allowed to be
// there, or nil if it is fine.
//
// The repository root carries configuration, documentation and module
// metadata, all of which is text. Anything else there is build output, so the
// rule is stated positively -- root-level files are UTF-8 text without the
// executable bit -- rather than as a list of binary formats to recognise. A
// format nobody enumerated (a stripped binary, a shared object, an archive) is
// exactly what a magic-byte list misses, and it is caught here by not being
// text.
//
// Binary assets that genuinely belong to the project live in a subdirectory;
// testdata fixtures are the common case and are deliberately out of scope.
func inspectRootFile(name string, payload []byte, mode fs.FileMode) *finding {
	for format, magic := range buildOutputMagic {
		if bytes.HasPrefix(payload, magic) {
			return &finding{
				path:   name,
				reason: fmt.Sprintf("is %s build output (%d bytes)", format, len(payload)),
			}
		}
	}

	// Scripts are legitimate root-level executables: they are text and declare
	// an interpreter, so they clear both remaining rules.
	if bytes.HasPrefix(payload, []byte("#!")) {
		return nil
	}

	if i := bytes.IndexByte(payload, 0x00); i >= 0 {
		return &finding{
			path:   name,
			reason: fmt.Sprintf("is not text (NUL byte at offset %d, %d bytes)", i, len(payload)),
		}
	}
	if !utf8.Valid(payload) {
		return &finding{
			path:   name,
			reason: fmt.Sprintf("is not valid UTF-8 text (%d bytes)", len(payload)),
		}
	}
	if mode.Perm()&0o111 != 0 {
		return &finding{
			path:   name,
			reason: "has the executable bit set and is not a script",
		}
	}
	return nil
}

// TestRepositoryRootCarriesNoTrackedBuildOutput is the authoritative layer: no
// tracked repository-root path may be a build artifact.
//
// Exact whenever it actually runs, which on a cold cache is always. See the
// package comment for why a warm local cache can still serve a stale pass and
// which layer covers that.
func TestRepositoryRootCarriesNoTrackedBuildOutput(t *testing.T) {
	root := repositoryRoot(t)
	bindCacheToRepositoryRoot(t, root)

	for _, name := range trackedRootFiles(t, root) {
		payload, mode, ok := readRootFile(root, name)
		if !ok {
			continue
		}
		if f := inspectRootFile(name, payload, mode); f != nil {
			f.status = "is tracked"
			t.Error(f)
		}
	}
}

// TestRepositoryRootCarriesNoUnignoredBuildOutput catches build output before
// it can ever be staged.
//
// Any binary sitting at the root that .gitignore does not cover is one
// `git add -A` away from being committed, and a command added after the ignore
// list was written is exactly that case. Unlike the tracked check, this one's
// inputs -- the root directory listing and .gitignore -- are inside the module
// root, so the cache invalidates the moment `go build` drops a file there.
//
// Tracked paths belong to the layer above and are skipped here so a single
// mistake is not reported twice.
func TestRepositoryRootCarriesNoUnignoredBuildOutput(t *testing.T) {
	root := repositoryRoot(t)
	bindCacheToRepositoryRoot(t, root)

	tracked := make(map[string]bool)
	for _, name := range trackedRootFiles(t, root) {
		tracked[name] = true
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("cannot read repository root: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || tracked[name] || name == ".git" {
			continue
		}
		payload, mode, ok := readRootFile(root, name)
		if !ok {
			continue
		}
		f := inspectRootFile(name, payload, mode)
		if f == nil {
			continue
		}
		if gitIgnores(t, root, name) {
			// Ignored build output is the supported outcome, not a problem:
			// `git add -A` cannot sweep it in.
			continue
		}
		f.status = "is untracked but NOT covered by .gitignore"
		t.Error(f)
	}
}

// bindCacheToRepositoryRoot makes the root directory an input of the test so
// the Go test cache re-runs it whenever a file appears at, disappears from, or
// changes size at the repository root.
//
// cmd/go hashes an opened directory by listing it and hashing every entry's
// stat, and the root is inside the module, so this is a real cache input --
// unlike the git index, which cannot be one in a linked worktree. This is what
// makes the unignored-output layer trustworthy under a plain `go test ./...`.
// It does not make the tracked layer trustworthy, because staging an
// already-present file changes nothing the cache can observe; `make hygiene`
// exists for that.
func bindCacheToRepositoryRoot(t *testing.T, root string) {
	t.Helper()
	if _, err := os.ReadDir(root); err != nil {
		t.Fatalf("cannot read the repository root %s to pin the test cache: %v", root, err)
	}
	// .gitignore decides the unignored layer's verdict, so it is an input too.
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("cannot stat .gitignore to pin the test cache: %v", err)
	}
}

// TestForceAddedBuildArtifactIsRejected proves the rule fires on the exact
// scenario behind both incidents: a command built from the repository root and
// then force-added past .gitignore.
//
// It drives the detector rather than the repository, so proving the guard works
// does not require committing a binary to prove it against.
func TestForceAddedBuildArtifactIsRejected(t *testing.T) {
	// The first bytes `go build ./evaluation/cmd/final-v5-dbtest-report` leaves.
	elf := append([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1}, bytes.Repeat([]byte{0}, 128)...)

	f := inspectRootFile("final-v5-dbtest-report", elf, 0o755)
	if f == nil {
		t.Fatal("a force-added ELF build artifact at the repository root was accepted")
	}
	f.status = "is tracked"
	for _, want := range []string{
		"final-v5-dbtest-report",
		"ELF (Linux)",
		"generated/bin",
		"git rm --cached",
		".gitignore",
	} {
		if !strings.Contains(f.String(), want) {
			t.Errorf("the rejection does not mention %q, so it does not tell the contributor\n"+
				"how to fix it:\n%s", want, f)
		}
	}
}

// TestNonTextRootFileIsRejectedWithoutKnownMagic covers the gap a magic-byte
// list leaves: output in a format nobody enumerated is still not text.
func TestNonTextRootFileIsRejectedWithoutKnownMagic(t *testing.T) {
	for name, payload := range map[string][]byte{
		"unrecognised binary": {0x11, 0x22, 0x00, 0x33},
		"invalid UTF-8":       {0xff, 0xfe, 0xfd},
	} {
		if f := inspectRootFile("some-artifact", payload, 0o644); f == nil {
			t.Errorf("%s at the repository root was accepted", name)
		}
	}
}

// TestLegitimateRootFilesAreAccepted keeps the rule from becoming a nuisance:
// what the root is actually for must pass, including UTF-8 prose and a script.
func TestLegitimateRootFilesAreAccepted(t *testing.T) {
	for name, file := range map[string]struct {
		payload []byte
		mode    fs.FileMode
	}{
		"README.md":    {[]byte("# Title\n\n中文说明 — em dash\n"), 0o644},
		"Makefile":     {[]byte("bin:\n\tgo build ./...\n"), 0o644},
		"script.sh":    {[]byte("#!/usr/bin/env bash\nset -euo pipefail\n"), 0o755},
		".env.example": {[]byte(""), 0o644},
	} {
		if f := inspectRootFile(name, file.payload, file.mode); f != nil {
			t.Errorf("legitimate root file %s was rejected: %s", name, f)
		}
	}
}

// readRootFile returns the bytes and mode of a repository-root path. A path
// that is absent, irregular or unreadable is a different problem and is
// reported as skippable rather than as a violation.
func readRootFile(root, name string) ([]byte, fs.FileMode, bool) {
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, false
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false
	}
	return payload, info.Mode(), true
}

func gitIgnores(t *testing.T, root, name string) bool {
	t.Helper()
	// check-ignore exits 0 when the path is ignored, 1 when it is not.
	err := exec.Command("git", "-C", root, "check-ignore", "-q", "--", name).Run()
	return err == nil
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git worktree: %v", err)
	}
	return strings.TrimSpace(string(output))
}

// trackedRootFiles lists tracked paths at the repository root only. Nested
// directories legitimately contain binaries -- testdata fixtures, for one -- so
// the invariant is deliberately scoped to the root, which is where build output
// lands.
func trackedRootFiles(t *testing.T, root string) []string {
	t.Helper()
	output, err := exec.Command("git", "-C", root, "ls-files", "--cached").Output()
	if err != nil {
		t.Skipf("git ls-files failed: %v", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line != "" && !strings.Contains(line, "/") {
			names = append(names, line)
		}
	}
	return names
}
