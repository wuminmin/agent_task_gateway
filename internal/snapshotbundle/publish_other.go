//go:build !linux

package snapshotbundle

import (
	"errors"
	"os"
)

// Atomic no-replace directory rename is a publication safety requirement.
// Platforms without a native primitive fail closed rather than falling back
// to the racy Lstat-plus-Rename sequence.
func renameDirectoryNoReplace(_ *os.File, _, _ string) error {
	return errors.New("atomic no-replace publication rename is unsupported on this platform")
}

func openDirectoryNoFollow(_ string) (*os.File, error) {
	return nil, errors.New("no-follow directory open is unsupported on this platform")
}

func validateDirectorySecurity(_ *os.File) error {
	return errors.New("directory ownership validation is unsupported on this platform")
}

func validateArtifactSecurity(_ *os.File) error {
	return errors.New("artifact ownership validation is unsupported on this platform")
}

func openDirectoryAt(_ *os.File, _ string) (*os.File, error) {
	return nil, errors.New("relative no-follow directory open is unsupported on this platform")
}

func createArtifactFile(_ *os.File, _ string) (*os.File, error) {
	return nil, errors.New("relative no-follow artifact creation is unsupported on this platform")
}

func openArtifactFile(_ *os.File, _ string) (*os.File, error) {
	return nil, errors.New("relative no-follow artifact open is unsupported on this platform")
}

func readDirectoryEntries(_ *os.File) ([]os.DirEntry, error) {
	return nil, errors.New("relative directory read is unsupported on this platform")
}

func verifyDirectoryEntry(_, _ *os.File, _ string) error {
	return errors.New("relative directory identity is unsupported on this platform")
}

func disablePublishedDirectory(_, _ *os.File, _, _ string) error {
	return errors.New("safe publication deactivation is unsupported on this platform")
}

func verifyPublishedDirectory(_, _ *os.File, _ string) error {
	return errors.New("safe published-directory verification is unsupported on this platform")
}
