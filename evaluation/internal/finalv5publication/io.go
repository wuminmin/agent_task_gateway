package finalv5publication

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

const (
	outputDirectoryMode = 0o700
	outputFileMode      = 0o600
	outputFileMaxBytes  = 4 << 20
	outputFileMaxCount  = 16
)

// WriteClosedOutputDirectory creates a previously absent private directory
// and writes exactly allowedNames with O_EXCL. A failed write removes only the
// directory created by this call; an existing file, directory, or symlink is
// never overwritten.
func WriteClosedOutputDirectory(path string, files map[string][]byte, allowedNames []string) (resultErr error) {
	names, err := validateClosedNames(allowedNames)
	if err != nil {
		return err
	}
	if err := validateOutputPayloads(files, names); err != nil {
		return err
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("publication output directory is required")
	}
	clean := filepath.Clean(path)
	parentInfo, err := os.Lstat(filepath.Dir(clean))
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("publication output parent must be an existing non-symlink directory")
	}
	if err := os.Mkdir(clean, outputDirectoryMode); err != nil {
		return fmt.Errorf("create publication output directory: %w", err)
	}
	created, err := os.Lstat(clean)
	if err != nil || !created.IsDir() || created.Mode()&os.ModeSymlink != 0 || created.Mode().Perm() != outputDirectoryMode {
		_ = os.Remove(clean)
		return errors.New("created publication output directory is not a private non-symlink directory")
	}
	complete := false
	defer func() {
		if complete {
			return
		}
		current, err := os.Lstat(clean)
		if err != nil || !current.IsDir() || !os.SameFile(created, current) {
			return
		}
		for _, name := range names {
			_ = os.Remove(filepath.Join(clean, name))
		}
		_ = os.Remove(clean)
	}()

	for _, name := range names {
		filePath := filepath.Join(clean, name)
		file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, outputFileMode)
		if err != nil {
			return fmt.Errorf("create publication output %q exclusively: %w", name, err)
		}
		payload := files[name]
		written, writeErr := file.Write(payload)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil || written != len(payload) {
			return fmt.Errorf("write complete publication output %q: %w", name,
				errors.Join(writeErr, syncErr, closeErr, errors.New("short or failed output write")))
		}
	}
	directory, err := os.Open(clean)
	if err != nil {
		return errors.New("reopen publication output directory")
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("sync publication output directory")
	}
	if err := validateClosedOutputDirectory(clean, names, files); err != nil {
		return err
	}
	complete = true
	return nil
}

// ValidateClosedOutputDirectory verifies the exact name closure and private
// file modes without accepting expected content from the directory itself.
func ValidateClosedOutputDirectory(path string, allowedNames []string) error {
	names, err := validateClosedNames(allowedNames)
	if err != nil {
		return err
	}
	return validateClosedOutputDirectory(filepath.Clean(path), names, nil)
}

func validateClosedOutputDirectory(path string, names []string, expected map[string][]byte) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != outputDirectoryMode {
		return errors.New("publication output must be a private non-symlink directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != len(names) {
		return errors.New("publication output directory does not contain the exact closed file set")
	}
	gotNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		gotNames = append(gotNames, entry.Name())
	}
	sort.Strings(gotNames)
	if !reflect.DeepEqual(gotNames, names) {
		return errors.New("publication output directory contains a missing or unexpected file")
	}
	for _, name := range names {
		path := filepath.Join(path, name)
		before, err := os.Lstat(path)
		if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 ||
			before.Mode().Perm() != outputFileMode || before.Size() <= 0 || before.Size() > outputFileMaxBytes {
			return fmt.Errorf("publication output %q is not a bounded regular non-symlink mode-0600 file", name)
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open publication output %q", name)
		}
		value, readErr := io.ReadAll(io.LimitReader(file, outputFileMaxBytes+1))
		opened, statErr := file.Stat()
		closeErr := file.Close()
		after, lstatErr := os.Lstat(path)
		if readErr != nil || statErr != nil || closeErr != nil || lstatErr != nil ||
			!os.SameFile(before, opened) || !os.SameFile(opened, after) ||
			!after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || after.Mode().Perm() != outputFileMode ||
			int64(len(value)) != after.Size() || len(value) == 0 || len(value) > outputFileMaxBytes {
			return fmt.Errorf("publication output %q changed or became unsafe while being read", name)
		}
		if expected != nil && !bytes.Equal(value, expected[name]) {
			return fmt.Errorf("publication output %q differs after its create-exclusive write", name)
		}
	}
	return nil
}

func validateClosedNames(allowed []string) ([]string, error) {
	if len(allowed) == 0 || len(allowed) > outputFileMaxCount {
		return nil, errors.New("publication output closed filename set has an invalid size")
	}
	names := append([]string(nil), allowed...)
	sort.Strings(names)
	for index, name := range names {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name ||
			strings.ContainsAny(name, "\x00/\\") {
			return nil, errors.New("publication output filename is invalid")
		}
		if index > 0 && names[index-1] == name {
			return nil, errors.New("publication output closed filename set contains a duplicate")
		}
	}
	return names, nil
}

func validateOutputPayloads(files map[string][]byte, names []string) error {
	if len(files) != len(names) {
		return errors.New("publication output payload set is not the allowed filename closure")
	}
	for _, name := range names {
		value, found := files[name]
		if !found || len(value) == 0 || len(value) > outputFileMaxBytes {
			return fmt.Errorf("publication output payload %q is absent, empty, or too large", name)
		}
	}
	return nil
}
