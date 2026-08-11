package finalv5publication

import (
	"bytes"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const publicDatabaseSecretReference = "GATEWAY_DB_PASSWORD"

var (
	assignmentPattern = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_])([a-z][a-z0-9_.-]{0,127})[ \t]*[:=][ \t]*([^\s,}\]]+)`)
	urlPattern        = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
)

// ValidateCredentialFree rejects credential material in generated JSON and
// YAML artifacts. The sole permitted credential-shaped value is the public,
// source-controlled reference GATEWAY_DB_PASSWORD under the exact key
// secretRef or secret_ref; it is a name, never a credential value.
//
// Errors identify only the failed gate. They deliberately omit source values,
// parser details, keys, and document bytes so a rejection cannot echo a secret.
func ValidateCredentialFree(files map[string][]byte, sensitiveValues []string) error {
	if len(files) == 0 {
		return credentialGateError("empty_input")
	}
	sensitive := make(map[string]struct{}, len(sensitiveValues))
	for _, value := range sensitiveValues {
		if value != "" {
			sensitive[value] = struct{}{}
		}
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := files[name]
		if len(value) == 0 {
			return credentialGateError("empty_artifact")
		}
		root, err := credentialDocument(name, value)
		if err != nil {
			return err
		}
		if err := scanCredentialText(string(value), true); err != nil {
			return err
		}
		if err := scanCredentialNode(root, sensitive, false); err != nil {
			return err
		}
		for sensitiveValue := range sensitive {
			// GATEWAY_DB_PASSWORD is a source-controlled reference name,
			// not a credential value. Its exact placement is checked by the
			// structured scanner above; every runtime DSN/password is instead
			// forbidden as a byte substring anywhere in an output artifact.
			if sensitiveValue != publicDatabaseSecretReference &&
				bytes.Contains(value, []byte(sensitiveValue)) {
				return credentialGateError("sensitive_substring")
			}
		}
	}
	return nil
}

func credentialDocument(name string, value []byte) (*yaml.Node, error) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json":
		var document any
		if err := strictJSON(value, &document); err != nil {
			return nil, credentialGateError("invalid_json")
		}
	case ".yaml", ".yml":
	default:
		return nil, credentialGateError("unsupported_format")
	}

	decoder := yaml.NewDecoder(strings.NewReader(string(value)))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil || len(root.Content) == 0 {
		return nil, credentialGateError("invalid_document")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, credentialGateError("multiple_documents")
	}
	return &root, nil
}

func scanCredentialNode(node *yaml.Node, sensitive map[string]struct{}, allowPublicReference bool) error {
	if node == nil {
		return credentialGateError("invalid_document")
	}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := scanCredentialNode(child, sensitive, false); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		if len(node.Content)%2 != 0 {
			return credentialGateError("invalid_document")
		}
		for index := 0; index < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Kind != yaml.ScalarNode {
				return credentialGateError("invalid_mapping_key")
			}
			publicReference := isPublicReferenceKey(key.Value) && value.Kind == yaml.ScalarNode &&
				value.Value == publicDatabaseSecretReference
			if isCredentialKey(key.Value) && !publicReference {
				return credentialGateError("credential_key")
			}
			if err := scanCredentialNode(key, sensitive, publicReference); err != nil {
				return err
			}
			if err := scanCredentialNode(value, sensitive, publicReference); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		if node.Alias == nil {
			return credentialGateError("invalid_alias")
		}
		return scanCredentialNode(node.Alias, sensitive, allowPublicReference)
	case yaml.ScalarNode:
		if !allowPublicReference {
			if node.Value == publicDatabaseSecretReference {
				return credentialGateError("public_reference_context")
			}
			if _, found := sensitive[node.Value]; found {
				return credentialGateError("sensitive_scalar")
			}
		}
		return scanCredentialText(node.Value, false)
	default:
		return credentialGateError("invalid_document")
	}
	return nil
}

func scanCredentialText(value string, allowStructuredPublicReference bool) error {
	upper := strings.ToUpper(value)
	if strings.Contains(upper, "-----BEGIN ") || strings.Contains(upper, "-----END ") {
		return credentialGateError("pem_marker")
	}
	for _, candidate := range urlPattern.FindAllString(value, -1) {
		candidate = strings.TrimRight(candidate, ".,;:!?)]}")
		parsed, err := url.Parse(candidate)
		if err == nil && parsed.User != nil {
			return credentialGateError("url_userinfo")
		}
	}
	for _, match := range assignmentPattern.FindAllStringSubmatch(value, -1) {
		if len(match) != 3 || !isCredentialKey(match[1]) {
			continue
		}
		if allowStructuredPublicReference && isPublicReferenceKey(match[1]) &&
			strings.Trim(match[2], `"'`) == publicDatabaseSecretReference {
			continue
		}
		return credentialGateError("secret_assignment")
	}
	return nil
}

func isPublicReferenceKey(value string) bool {
	return value == "secretRef" || value == "secret_ref"
}

func isCredentialKey(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.NewReplacer("-", "_", ".", "_").Replace(normalized)
	if normalized == "secretref" || normalized == "secret_ref" {
		return true
	}
	switch normalized {
	case "password", "passwd", "pwd", "secret", "token", "dsn", "api_key", "apikey",
		"private_key", "access_key", "access_key_id", "secret_key", "secret_access_key",
		"authorization", "auth_token", "bearer_token", "refresh_token", "id_token",
		"database_url", "connection_url", "connection_string", "credential", "credentials",
		"object_access_key", "object_store_access_key", "object_secret_key", "object_store_secret_key",
		"aws_access_key_id", "aws_secret_access_key", "minio_root_user", "minio_root_password":
		return true
	}
	for _, suffix := range []string{"_password", "_passwd", "_secret", "_token", "_dsn", "_private_key",
		"_access_key", "_access_key_id", "_secret_key", "_secret_access_key", "_database_url",
		"_connection_url", "_connection_string"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

func credentialGateError(gate string) error {
	return errors.New("generated artifact failed credential gate: " + gate)
}
