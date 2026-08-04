// Package repohygiene holds repository-level invariants that no single
// component owns but that every contributor can break by accident.
package repohygiene

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

// TestNoBuildOutputInRepositoryRoot rejects a committed build artifact.
//
// `go build ./evaluation/cmd/<command>` writes the executable into the working
// directory, so running it from the repository root leaves a binary there, where
// `git add -A` sweeps it into a commit. That happened once: a 24 MB
// final-v5-attestation-footprint executable was committed in c541d7e and removed
// by a forward commit.
//
// .gitignore lists the current command names, but a command added later would
// not be in that list. This test is the guard that does not need updating: it
// looks at what is actually tracked, not at what someone remembered to ignore.
func TestNoBuildOutputInRepositoryRoot(t *testing.T) {
	root := repositoryRoot(t)
	for _, name := range trackedRootFiles(t, root) {
		path := filepath.Join(root, name)
		payload, err := os.ReadFile(path)
		if err != nil {
			// A tracked path that cannot be read is a different problem; the
			// build-output check has nothing to say about it.
			continue
		}
		for format, magic := range buildOutputMagic {
			if bytes.HasPrefix(payload, magic) {
				t.Errorf("%s is a tracked %s build artifact (%d bytes).\n"+
					"Go writes a main package's executable into the working directory. "+
					"Build into generated/bin instead:\n"+
					"    go build -o generated/bin/ ./evaluation/cmd/%s\n"+
					"then `git rm --cached %s` and add it to .gitignore.",
					name, format, len(payload), name, name)
			}
		}
	}
}

// TestRepositoryRootCarriesNoExecutableFile is the weaker companion check: a
// build output that this test does not recognise by magic bytes is still very
// unlikely to be a legitimate executable-bit file at the repository root.
func TestRepositoryRootCarriesNoExecutableFile(t *testing.T) {
	root := repositoryRoot(t)
	for _, name := range trackedRootFiles(t, root) {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			continue
		}
		// Scripts are legitimate; they are text and declare an interpreter.
		payload, err := os.ReadFile(filepath.Join(root, name))
		if err == nil && bytes.HasPrefix(payload, []byte("#!")) {
			continue
		}
		t.Errorf("%s is tracked at the repository root with the executable bit set "+
			"and is not a script; build output belongs in generated/bin", name)
	}
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
