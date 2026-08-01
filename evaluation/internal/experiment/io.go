package experiment

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type JSONLWriter struct {
	file    *os.File
	encoder *json.Encoder
}

func NewJSONLWriter(path string) (*JSONLWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &JSONLWriter{file: file, encoder: json.NewEncoder(file)}, nil
}

func (writer *JSONLWriter) Write(sample Sample) error {
	if writer == nil || writer.file == nil {
		return errors.New("JSONL writer is closed")
	}
	if err := sample.Validate(); err != nil {
		return err
	}
	return writer.encoder.Encode(sample)
}

func (writer *JSONLWriter) Close() error {
	if writer == nil || writer.file == nil {
		return nil
	}
	if err := writer.file.Sync(); err != nil {
		_ = writer.file.Close()
		return err
	}
	err := writer.file.Close()
	writer.file = nil
	return err
}

func ReadSamples(paths []string) ([]Sample, error) {
	var samples []Sample
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			var sample Sample
			if err := StrictJSON(scanner.Bytes(), &sample); err != nil {
				file.Close()
				return nil, fmt.Errorf("%s:%d: %w", path, line, err)
			}
			if err := sample.Validate(); err != nil {
				file.Close()
				return nil, fmt.Errorf("%s:%d: %w", path, line, err)
			}
			samples = append(samples, sample)
		}
		err = scanner.Err()
		_ = file.Close()
		if err != nil {
			return nil, err
		}
	}
	return samples, nil
}

func FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type EvidenceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}
type EvidenceManifest struct {
	SchemaVersion    int            `json:"schema_version"`
	CampaignID       string         `json:"campaign_id"`
	SubmissionCommit string         `json:"submission_commit"`
	Files            []EvidenceFile `json:"files"`
	ManifestSHA256   string         `json:"manifest_sha256"`
}

func BuildEvidenceManifest(root, campaignID, commit string, paths []string) (EvidenceManifest, error) {
	manifest := EvidenceManifest{SchemaVersion: 1, CampaignID: campaignID, SubmissionCommit: commit}
	for _, path := range paths {
		clean := filepath.Clean(path)
		rel, err := filepath.Rel(root, clean)
		if err != nil || strings.HasPrefix(rel, "..") {
			return manifest, errors.New("evidence path escapes run directory")
		}
		info, err := os.Stat(clean)
		if err != nil || !info.Mode().IsRegular() {
			return manifest, fmt.Errorf("invalid evidence file %s", clean)
		}
		digest, err := FileSHA256(clean)
		if err != nil {
			return manifest, err
		}
		manifest.Files = append(manifest.Files, EvidenceFile{Path: filepath.ToSlash(rel), SHA256: digest, Bytes: info.Size()})
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	value, _ := json.Marshal(manifest.Files)
	digest := sha256.Sum256(value)
	manifest.ManifestSHA256 = hex.EncodeToString(digest[:])
	return manifest, nil
}

var secretKeys = []string{"password", "passwd", "secret", "token", "private_key", "access_key", "dsn"}

func RedactSecrets(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			lower := strings.ToLower(key)
			redact := false
			for _, marker := range secretKeys {
				if strings.Contains(lower, marker) {
					redact = true
					break
				}
			}
			if redact {
				out[key] = "[REDACTED]"
			} else {
				out[key] = RedactSecrets(child)
			}
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = RedactSecrets(typed[i])
		}
		return out
	default:
		return value
	}
}
