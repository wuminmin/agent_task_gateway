package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

func Load(path string) (*Catalog, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open catalog: %w", err)
	}
	defer file.Close()
	return ParseReader(file)
}

func Parse(data []byte) (*Catalog, error) {
	return ParseReader(bytes.NewReader(data))
}

func ParseReader(reader io.Reader) (*Catalog, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fieldError("catalog", "document is empty", ErrMissingField)
	}

	// Inspect the syntax tree before typed decoding so explicit secret-bearing
	// keys and URI userinfo are rejected with a redacted error.
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("%w: malformed YAML", ErrInvalidCatalog)
	}
	if err := rejectPlaintextSecrets(&document); err != nil {
		return nil, err
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var result Catalog
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("%w: YAML does not match the catalog schema: %v", ErrInvalidCatalog, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%w: multiple YAML documents are forbidden", ErrInvalidCatalog)
		}
		return nil, fmt.Errorf("%w: malformed trailing YAML", ErrInvalidCatalog)
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	result.SHA256 = hex.EncodeToString(digest[:])
	return &result, nil
}

func rejectPlaintextSecrets(document *yaml.Node) error {
	if document == nil || len(document.Content) == 0 {
		return nil
	}
	return walkSecretNodes(document.Content[0], false)
}

func walkSecretNodes(node *yaml.Node, inSources bool) error {
	if node.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			value := node.Content[index+1]
			normalized := normalizeKey(key.Value)
			nextInSources := inSources || normalized == "sources"
			if nextInSources && isPasswordKey(normalized) && !isEmptyYAMLValue(value) {
				return fieldError("sources", "plaintext password is forbidden; use secretRef", ErrPlaintextPassword)
			}
			if nextInSources && (normalized == "address" || normalized == "dsn" || normalized == "url") && containsUserPassword(value.Value) {
				return fieldError("sources", "credentials embedded in an address are forbidden; use secretRef", ErrPlaintextPassword)
			}
			if err := walkSecretNodes(value, nextInSources); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range node.Content {
		if err := walkSecretNodes(child, inSources); err != nil {
			return err
		}
	}
	return nil
}

func normalizeKey(value string) string {
	replacer := strings.NewReplacer("_", "", "-", "", ".", "")
	return strings.ToLower(replacer.Replace(strings.TrimSpace(value)))
}

func isPasswordKey(key string) bool {
	switch key {
	case "password", "passwd", "pwd", "plaintextpassword":
		return true
	default:
		return false
	}
}

func isEmptyYAMLValue(node *yaml.Node) bool {
	return node == nil || node.Tag == "!!null" || strings.TrimSpace(node.Value) == ""
}

func containsUserPassword(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return true
		}
	}
	// Also catch scheme-less user:password@host forms.
	at := strings.LastIndex(value, "@")
	if at > 0 && strings.Contains(value[:at], ":") {
		return true
	}
	return false
}
