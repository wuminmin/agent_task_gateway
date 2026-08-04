// Package formalbuild materializes and digests the build context of a formal
// Gateway image.
//
// # Why this exists
//
// The ordinary Dockerfile ends its base stage with COPY . ., which copies the
// host directory as it stands. That is convenient for development and useless as
// provenance: an untracked file, a stale generated binary, or an edit that was
// never committed all enter the image indistinguishably from tracked source, and
// no label on the resulting image can tell a reviewer which happened.
//
// The formal path instead materializes the context with git archive, which can
// only emit blobs reachable from the named commit. Untracked and ignored host
// files are not merely discouraged from entering the image; there is no path by
// which they could.
//
// # Why the digest is computed twice
//
// ContextDigest is computed over the entries of the tar that will actually be
// fed to the builder, and separately over the commit tree read straight out of
// the object database. Requiring the two to agree is what makes the digest
// meaningful: agreement means the bytes entering the image are exactly the
// commit's bytes, and it is checkable by a reviewer who has only the commit.
package formalbuild

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
)

// contextDomain separates the build-context digest from every other digest in
// the system.
const contextDomain = "TASKGATE-FINAL-V5-FORMAL-BUILD-CONTEXT-V1"

// manifestDomain separates the source-manifest digest, which binds paths and
// content digests without the mode and symlink detail.
const manifestDomain = "TASKGATE-FINAL-V5-FORMAL-SOURCE-MANIFEST-V1"

// maxContextBytes bounds what will be read into memory while digesting. The
// repository is a few tens of megabytes; anything far larger means the context
// is not what this package thinks it is.
const maxContextBytes = 512 << 20

// Entry is one file in the build context.
//
// Mode, bytes and symlink target are all carried because each can change what
// the image does without changing the others. A script losing its executable bit
// changes behaviour with identical bytes; a symlink retargeted from one tracked
// file to another changes what is read with identical bytes and mode.
type Entry struct {
	// Path is the slash-separated path relative to the context root.
	Path string `json:"path"`
	// Mode is the git file mode: 100644, 100755, 120000 or 160000.
	Mode string `json:"mode"`
	// ContentSHA256 digests the file bytes. For a symlink the bytes are the
	// target path, so this and SymlinkTarget agree by construction.
	ContentSHA256 string `json:"content_sha256"`
	// SymlinkTarget is the link target for mode 120000 and empty otherwise.
	SymlinkTarget string `json:"symlink_target,omitempty"`
}

// gitModeRegular, gitModeExecutable, gitModeSymlink and gitModeGitlink are the
// only modes git records for a blob in a tree.
const (
	gitModeRegular    = "100644"
	gitModeExecutable = "100755"
	gitModeSymlink    = "120000"
	gitModeGitlink    = "160000"
)

// ContextDigest is the deterministic digest of a build context.
//
// The entries are sorted by path and framed with explicit lengths, so no two
// distinct contexts can collide by concatenation: a path containing a separator
// cannot be made to look like two entries.
func ContextDigest(entries []Entry) (string, error) {
	ordered, err := canonicalEntries(entries)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write([]byte(contextDomain))
	hash.Write([]byte{0})
	fmt.Fprintf(hash, "entries\x00%d\x00", len(ordered))
	for _, entry := range ordered {
		fmt.Fprintf(hash, "%d\x00%s\x00%s\x00%s\x00%d\x00%s\x00",
			len(entry.Path), entry.Path, entry.Mode, entry.ContentSHA256,
			len(entry.SymlinkTarget), entry.SymlinkTarget)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// SourceManifestDigest binds path to content only.
//
// It is separate from ContextDigest on purpose. A reviewer who wants to know
// "are these the same source files" can compare manifests across builds whose
// mode representation differs; a reviewer who wants to know "is this the same
// build context" must compare the context digest, which is strictly stronger.
func SourceManifestDigest(entries []Entry) (string, error) {
	ordered, err := canonicalEntries(entries)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write([]byte(manifestDomain))
	hash.Write([]byte{0})
	fmt.Fprintf(hash, "entries\x00%d\x00", len(ordered))
	for _, entry := range ordered {
		fmt.Fprintf(hash, "%d\x00%s\x00%s\x00", len(entry.Path), entry.Path, entry.ContentSHA256)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func canonicalEntries(entries []Entry) ([]Entry, error) {
	if len(entries) == 0 {
		return nil, errors.New("a formal build context cannot be empty")
	}
	ordered := append([]Entry(nil), entries...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].Path < ordered[right].Path })
	seen := make(map[string]struct{}, len(ordered))
	for _, entry := range ordered {
		if err := entry.validate(); err != nil {
			return nil, err
		}
		if _, duplicate := seen[entry.Path]; duplicate {
			return nil, fmt.Errorf("build context lists %q twice", entry.Path)
		}
		seen[entry.Path] = struct{}{}
	}
	return ordered, nil
}

func (entry Entry) validate() error {
	if entry.Path == "" || strings.HasPrefix(entry.Path, "/") || strings.Contains(entry.Path, "\x00") ||
		strings.HasPrefix(entry.Path, "../") || strings.Contains(entry.Path, "/../") ||
		strings.HasSuffix(entry.Path, "/") {
		return fmt.Errorf("build context path %q is not a clean relative path", entry.Path)
	}
	switch entry.Mode {
	case gitModeRegular, gitModeExecutable, gitModeSymlink:
	case gitModeGitlink:
		// A submodule reference would make the context depend on an object this
		// repository does not contain, so the archive could not reproduce it.
		return fmt.Errorf("build context path %q is a submodule; a formal context must be self-contained", entry.Path)
	default:
		return fmt.Errorf("build context path %q has unsupported mode %q", entry.Path, entry.Mode)
	}
	if !validSHA256(entry.ContentSHA256) {
		return fmt.Errorf("build context path %q has no content digest", entry.Path)
	}
	if (entry.Mode == gitModeSymlink) != (entry.SymlinkTarget != "") {
		return fmt.Errorf("build context path %q carries a symlink target inconsistent with mode %s",
			entry.Path, entry.Mode)
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9', character >= 'a' && character <= 'f':
		default:
			return false
		}
	}
	return true
}

// Git runs one git command in root and returns its trimmed stdout.
type Git func(arguments ...string) (string, error)

// NewGit binds a Git to one repository root.
func NewGit(root string) Git {
	return func(arguments ...string) (string, error) {
		command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err,
				strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(string(output)), nil
	}
}

// EntriesFromCommit enumerates the tracked files of a commit straight from the
// object database.
//
// The working tree is never read. A file the host has edited but not committed
// therefore cannot influence the result, which is the property that makes this
// usable as build provenance.
func EntriesFromCommit(root, commit string) ([]Entry, error) {
	listing, err := runGitBytes(root, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return nil, fmt.Errorf("enumerate tracked files at %s: %w", shortCommit(commit), err)
	}
	var entries []Entry
	for _, record := range strings.Split(string(listing), "\x00") {
		if record == "" {
			continue
		}
		// Format: "<mode> <type> <object>\t<path>".
		metadata, path, found := strings.Cut(record, "\t")
		if !found {
			return nil, fmt.Errorf("git ls-tree emitted a record with no path separator")
		}
		fields := strings.Fields(metadata)
		if len(fields) != 3 {
			return nil, fmt.Errorf("git ls-tree emitted %d metadata fields for %q, want 3", len(fields), path)
		}
		mode, objectType, object := fields[0], fields[1], fields[2]
		if objectType == "commit" || mode == gitModeGitlink {
			return nil, fmt.Errorf("tracked path %q is a submodule; a formal context must be self-contained", path)
		}
		if objectType != "blob" {
			return nil, fmt.Errorf("tracked path %q has unsupported object type %q", path, objectType)
		}
		content, err := runGitBytes(root, "cat-file", "blob", object)
		if err != nil {
			return nil, fmt.Errorf("read blob for %q: %w", path, err)
		}
		digest := sha256.Sum256(content)
		entry := Entry{Path: path, Mode: mode, ContentSHA256: hex.EncodeToString(digest[:])}
		if mode == gitModeSymlink {
			entry.SymlinkTarget = string(content)
		}
		if err := entry.validate(); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("commit %s has no tracked files", shortCommit(commit))
	}
	return entries, nil
}

// EntriesFromArchive reads the tar git archive produced and enumerates it in the
// same representation.
//
// This is the context that will actually be handed to the builder, so digesting
// it is what binds the label on the image to the bytes in the image rather than
// to a parallel description of them.
func EntriesFromArchive(archive io.Reader) ([]Entry, error) {
	reader := tar.NewReader(io.LimitReader(archive, maxContextBytes))
	var entries []Entry
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read formal build context archive: %w", err)
		}
		path := strings.TrimPrefix(header.Name, "./")
		switch header.Typeflag {
		case tar.TypeDir:
			// git archive emits directory entries for convenience. They carry no
			// content and are fully implied by the file paths, so including them
			// would make the digest depend on an archiver detail.
			continue
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			// git archive prepends a pax_global_header carrying the commit id.
			// It is archiver metadata rather than a file in the context, and it
			// is deliberately excluded: digesting it would make the context
			// digest depend on which commit produced the archive, so two commits
			// with byte-identical trees would digest differently and the digest
			// would no longer mean "these are the same source bytes".
			continue
		case tar.TypeReg:
			content, err := io.ReadAll(io.LimitReader(reader, maxContextBytes))
			if err != nil {
				return nil, fmt.Errorf("read %q from the formal build context: %w", path, err)
			}
			digest := sha256.Sum256(content)
			mode := gitModeRegular
			if header.Mode&0o111 != 0 {
				mode = gitModeExecutable
			}
			entries = append(entries, Entry{Path: path, Mode: mode, ContentSHA256: hex.EncodeToString(digest[:])})
		case tar.TypeSymlink:
			digest := sha256.Sum256([]byte(header.Linkname))
			entries = append(entries, Entry{Path: path, Mode: gitModeSymlink,
				ContentSHA256: hex.EncodeToString(digest[:]), SymlinkTarget: header.Linkname})
		default:
			return nil, fmt.Errorf("formal build context entry %q has unsupported tar type %q",
				path, string(header.Typeflag))
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("the formal build context archive is empty")
	}
	for _, entry := range entries {
		if err := entry.validate(); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// RequireSameContext fails unless two enumerations describe one context.
//
// The message names the first differing path rather than only reporting that the
// digests differ, because a bare digest mismatch tells an operator nothing about
// what leaked in.
func RequireSameContext(fromCommit, fromArchive []Entry) error {
	left, err := canonicalEntries(fromCommit)
	if err != nil {
		return err
	}
	right, err := canonicalEntries(fromArchive)
	if err != nil {
		return err
	}
	byPath := make(map[string]Entry, len(left))
	for _, entry := range left {
		byPath[entry.Path] = entry
	}
	for _, entry := range right {
		committed, present := byPath[entry.Path]
		if !present {
			return fmt.Errorf("the build context contains %q, which is not tracked at the build commit; "+
				"an untracked or ignored host file entered the formal image", entry.Path)
		}
		if committed != entry {
			return fmt.Errorf("the build context entry for %q differs from its committed form", entry.Path)
		}
		delete(byPath, entry.Path)
	}
	if len(byPath) > 0 {
		missing := make([]string, 0, len(byPath))
		for path := range byPath {
			missing = append(missing, path)
		}
		sort.Strings(missing)
		return fmt.Errorf("the build context omits %d tracked path(s), the first being %q",
			len(missing), missing[0])
	}
	return nil
}

func runGitBytes(root string, arguments ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err,
			strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func shortCommit(commit string) string {
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}
