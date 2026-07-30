package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
)

func requireDirectory(path, name string) error {
	if path == "" {
		return fmt.Errorf("%s is required", name)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", name, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory", name)
	}
	return nil
}

func createPrivateDirectory(path, name string) error {
	if path == "" {
		return fmt.Errorf("%s is required", name)
	}
	if err := os.Mkdir(path, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	return nil
}

func writeJSONAtomicExclusive(path string, value any) error {
	if path == "" {
		return errors.New("output path is required")
	}
	directory := filepath.Dir(path)
	if err := requireDirectory(directory, "output parent directory"); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".rq5-online-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite %s", path)
		}
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func writeBytesAtomicExclusive(path string, payload []byte, mode os.FileMode) error {
	if path == "" || len(payload) == 0 {
		return errors.New("nonempty output path and payload are required")
	}
	directory := filepath.Dir(path)
	if err := requireDirectory(directory, "output parent directory"); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".rq5-online-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite %s", path)
		}
		return err
	}
	return nil
}

func decodeJSONFileStrict(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 8<<20))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON contains a trailing value")
		}
		return err
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func normalizedJSONDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return "", err
	}
	return approval.CanonicalSHA256(normalized)
}

func positiveMilliseconds(elapsed time.Duration) (float64, error) {
	value := float64(elapsed.Nanoseconds()) / float64(time.Millisecond)
	if value <= 0 {
		return 0, errors.New("monotonic wall measurement was not positive")
	}
	return value, nil
}
