//go:build linux

package snapshotbundle

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// renameDirectoryNoReplace commits a staged publication relative to the
// already-open base directory. RENAME_NOREPLACE closes the race in which a
// concurrent publisher creates an empty final directory after the preflight
// Lstat; plain os.Rename may replace that directory on Linux.
func renameDirectoryNoReplace(base *os.File, oldName, newName string) error {
	if base == nil || !validRelativeName(oldName) || !validRelativeName(newName) {
		return errors.New("invalid publication rename")
	}
	if err := unix.Renameat2(int(base.Fd()), oldName, int(base.Fd()), newName, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("rename publication without replacement: %w", err)
	}
	return nil
}

func openDirectoryNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), path)
	if directory == nil {
		_ = unix.Close(fd)
		return nil, errors.New("wrap directory descriptor")
	}
	return directory, nil
}

func validateDirectorySecurity(directory *os.File) error {
	if directory == nil {
		return errors.New("directory is required")
	}
	var status unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &status); err != nil {
		return err
	}
	if status.Uid != uint32(os.Geteuid()) {
		return errors.New("directory is not owned by the effective builder user")
	}
	if status.Mode&0o022 != 0 {
		return errors.New("directory is group- or world-writable")
	}
	return nil
}

func validateArtifactSecurity(file *os.File) error {
	if file == nil {
		return errors.New("artifact file is required")
	}
	var status unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &status); err != nil {
		return err
	}
	if status.Uid != uint32(os.Geteuid()) {
		return errors.New("artifact is not owned by the effective builder user")
	}
	if status.Mode&0o022 != 0 {
		return errors.New("artifact is group- or world-writable")
	}
	return nil
}

func openDirectoryAt(parent *os.File, name string) (*os.File, error) {
	if parent == nil || !validRelativeName(name) {
		return nil, errors.New("invalid relative directory")
	}
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), name)
	if directory == nil {
		_ = unix.Close(fd)
		return nil, errors.New("wrap relative directory descriptor")
	}
	return directory, nil
}

func createArtifactFile(directory *os.File, name string) (*os.File, error) {
	if directory == nil || !validRelativeName(name) {
		return nil, errors.New("invalid relative artifact")
	}
	fd, err := unix.Openat(int(directory.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("wrap artifact descriptor")
	}
	return file, nil
}

func openArtifactFile(directory *os.File, name string) (*os.File, error) {
	if directory == nil || !validRelativeName(name) {
		return nil, errors.New("invalid relative artifact")
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("wrap artifact descriptor")
	}
	return file, nil
}

func readDirectoryEntries(directory *os.File) ([]os.DirEntry, error) {
	if directory == nil {
		return nil, errors.New("directory is required")
	}
	fd, err := unix.Openat(int(directory.Fd()), ".",
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	copy := os.NewFile(uintptr(fd), ".")
	if copy == nil {
		_ = unix.Close(fd)
		return nil, errors.New("wrap duplicated directory descriptor")
	}
	entries, readErr := copy.ReadDir(-1)
	closeErr := copy.Close()
	return entries, errors.Join(readErr, closeErr)
}

func verifyDirectoryEntry(parent, expected *os.File, name string) error {
	actual, err := openDirectoryAt(parent, name)
	if err != nil {
		return err
	}
	defer actual.Close()
	actualInfo, actualErr := actual.Stat()
	expectedInfo, expectedErr := expected.Stat()
	if actualErr != nil || expectedErr != nil || !os.SameFile(actualInfo, expectedInfo) {
		return errors.New("directory entry differs from expected inode")
	}
	return nil
}

// disablePublishedDirectory removes only the activation marker from the exact
// directory inode that was verified in staging. It is the fail-closed fallback
// if a post-rename durability failure cannot be rolled back to the private
// staging name.
func disablePublishedDirectory(base, staged *os.File, publicationName, manifestName string) error {
	if base == nil || staged == nil || !configNamePattern.MatchString(publicationName) ||
		manifestName != publicationName+".bundle.json" {
		return errors.New("invalid publication deactivation target")
	}
	finalFD, err := unix.Openat(int(base.Fd()), publicationName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open committed publication for deactivation: %w", err)
	}
	final := os.NewFile(uintptr(finalFD), publicationName)
	if final == nil {
		_ = unix.Close(finalFD)
		return errors.New("wrap committed publication descriptor")
	}
	defer final.Close()
	finalInfo, finalErr := final.Stat()
	stagedInfo, stagedErr := staged.Stat()
	if finalErr != nil || stagedErr != nil || !os.SameFile(finalInfo, stagedInfo) {
		return errors.New("committed publication identity differs from staging")
	}
	if err := unix.Unlinkat(finalFD, manifestName, 0); err != nil {
		return fmt.Errorf("remove committed activation marker: %w", err)
	}
	if err := unix.Fsync(finalFD); err != nil {
		return fmt.Errorf("sync deactivated publication: %w", err)
	}
	if err := unix.Fsync(int(base.Fd())); err != nil {
		return fmt.Errorf("sync publication base after deactivation: %w", err)
	}
	return nil
}

func verifyPublishedDirectory(base, staged *os.File, publicationName string) error {
	if base == nil || staged == nil || !configNamePattern.MatchString(publicationName) {
		return errors.New("invalid published directory identity target")
	}
	finalFD, err := unix.Openat(int(base.Fd()), publicationName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open published directory: %w", err)
	}
	final := os.NewFile(uintptr(finalFD), publicationName)
	if final == nil {
		_ = unix.Close(finalFD)
		return errors.New("wrap published directory descriptor")
	}
	defer final.Close()
	finalInfo, finalErr := final.Stat()
	stagedInfo, stagedErr := staged.Stat()
	if finalErr != nil || stagedErr != nil || !os.SameFile(finalInfo, stagedInfo) {
		return errors.New("published directory differs from verified staging inode")
	}
	return nil
}
