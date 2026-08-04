package formalbuild

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// validCommitSHA1 accepts only a full lowercase git object name. An abbreviated
// name is ambiguous across the repository's lifetime, so it cannot identify the
// source an image was built from.
func validCommitSHA1(value string) bool {
	if len(value) != 40 {
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

// SourceState is the repository state a formal image is built from.
//
// Every field is read from git rather than accepted from a caller. A formal
// build that took the commit as a parameter could be pointed at a commit the
// working tree does not contain, and the resulting image would carry a revision
// label describing source nobody built.
type SourceState struct {
	// Commit is the full object name the image is built from.
	Commit string
	// Branch is the branch the commit is checked out on.
	Branch string
	// CleanTree records that no tracked file was modified and no untracked,
	// non-ignored file was present when the context was materialized.
	CleanTree bool
	// Published records that the commit is reachable from its remote branch, so
	// a reviewer can fetch the exact source the label names.
	Published bool
}

// ReadSourceState reads the repository state and refuses anything a formal image
// must not be built from.
//
// The checks are fail-closed and ordered from the cheapest to the most
// surprising, so an operator with a dirty tree is told that rather than being
// told a digest disagreed three steps later.
func ReadSourceState(root string) (SourceState, error) {
	var state SourceState
	git := NewGit(root)

	status, err := git("status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return state, err
	}
	if strings.TrimSpace(status) != "" {
		return state, fmt.Errorf("the worktree is not clean; a formal Gateway image cannot be built from "+
			"uncommitted state:\n%s", indent(status))
	}
	state.CleanTree = true

	if state.Commit, err = git("rev-parse", "HEAD"); err != nil {
		return state, err
	}
	if !validCommitSHA1(state.Commit) {
		return state, fmt.Errorf("git reported HEAD as %q, which is not a full object name", state.Commit)
	}
	if state.Branch, err = git("rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		return state, err
	}
	if state.Branch == "HEAD" {
		return state, errors.New("HEAD is detached; a formal Gateway image is built from a named published branch")
	}

	// A commit that exists only on this host cannot be re-fetched, so its
	// revision label would name source no reviewer can obtain.
	remote, err := git("rev-parse", "origin/"+state.Branch)
	if err != nil {
		return state, fmt.Errorf("the commit is not published: no origin/%s is known to this repository: %w",
			state.Branch, err)
	}
	if remote != state.Commit {
		return state, fmt.Errorf("HEAD is %s but origin/%s is %s; a formal Gateway image is built only from a "+
			"published commit", shortCommit(state.Commit), state.Branch, shortCommit(remote))
	}
	state.Published = true
	return state, nil
}

// RequireBuildable rejects a state a formal build must not proceed from.
func (state SourceState) RequireBuildable() error {
	if !validCommitSHA1(state.Commit) {
		return errors.New("the formal build has no commit")
	}
	if !state.CleanTree {
		return errors.New("the formal build refuses a dirty worktree")
	}
	if !state.Published {
		return errors.New("the formal build refuses an unpublished commit")
	}
	return nil
}

func indent(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for index, line := range lines {
		lines[index] = "  " + line
	}
	return strings.Join(lines, "\n")
}

// Context is a materialized formal build context.
type Context struct {
	// Source is the repository state the context was materialized from.
	Source SourceState
	// Archive is the tar that will be streamed to the builder.
	Archive []byte
	// Entries is the canonical enumeration of the context.
	Entries []Entry
	// ContextSHA256 and SourceManifestSHA256 are the digests bound into the
	// image labels.
	ContextSHA256        string
	SourceManifestSHA256 string
}

// Materialize builds the tracked-file-only context for a commit and proves it
// contains exactly that commit.
//
// The archive is enumerated independently of the commit tree and the two are
// required to agree. Agreement is what makes the label meaningful: it says the
// bytes that entered the builder are the commit's bytes, and a reviewer holding
// only the commit can recompute it.
func Materialize(root, commit string) (Context, error) {
	var materialized Context
	archive, err := runGitBytes(root, "archive", "--format=tar", commit)
	if err != nil {
		return materialized, fmt.Errorf("materialize the formal build context at %s: %w", shortCommit(commit), err)
	}
	fromArchive, err := EntriesFromArchive(bytes.NewReader(archive))
	if err != nil {
		return materialized, err
	}
	fromCommit, err := EntriesFromCommit(root, commit)
	if err != nil {
		return materialized, err
	}
	if err := RequireSameContext(fromCommit, fromArchive); err != nil {
		return materialized, err
	}
	contextDigest, err := ContextDigest(fromArchive)
	if err != nil {
		return materialized, err
	}
	manifestDigest, err := SourceManifestDigest(fromArchive)
	if err != nil {
		return materialized, err
	}
	// Recomputed from the commit tree as well. If these could differ, the label
	// would describe the enumeration rather than the source.
	commitContextDigest, err := ContextDigest(fromCommit)
	if err != nil {
		return materialized, err
	}
	if commitContextDigest != contextDigest {
		return materialized, fmt.Errorf("the build context digests to %s from the archive but %s from the commit tree",
			shortDigest(contextDigest), shortDigest(commitContextDigest))
	}
	materialized.Archive = archive
	materialized.Entries = fromArchive
	materialized.ContextSHA256 = contextDigest
	materialized.SourceManifestSHA256 = manifestDigest
	return materialized, nil
}

// MaterializeHead reads the repository state and materializes its commit.
func MaterializeHead(root string) (Context, error) {
	state, err := ReadSourceState(root)
	if err != nil {
		return Context{}, err
	}
	if err := state.RequireBuildable(); err != nil {
		return Context{}, err
	}
	materialized, err := Materialize(root, state.Commit)
	if err != nil {
		return Context{}, err
	}
	materialized.Source = state
	return materialized, nil
}

func shortDigest(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}
