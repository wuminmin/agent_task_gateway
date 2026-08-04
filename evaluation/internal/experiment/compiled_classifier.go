package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"taskbound.local/agent-data-gateway/internal/approval"
)

// CompiledClassifierVersion identifies the operation-bound compiled classifier.
const CompiledClassifierVersion = "taskgate-final-v5-compiled-classifier-v1"

// compiledClassifierDomain domain-separates the binding digest.
const compiledClassifierDomain = "TASKGATE-FINAL-V5-COMPILED-CLASSIFIER-V1"

// OperationIdentity is what a classifier is bound to.
//
// A manifest alone says which statements are allowed; it does not say which
// operation they were allowed for. Binding the two means a manifest cannot be
// presented for a different operation, a different execution path, a different
// ExpectedSchema or a different Attestation qualification -- each of which
// legitimately changes what the closed world contains.
type OperationIdentity struct {
	// OperationID names the measured operation within its run.
	OperationID string `json:"operation_id"`
	// PathKind fixes the execution path, and with it the exact target
	// cardinality the manifest must define.
	PathKind GatewayPathKind `json:"path_kind"`
	// ContractIdentity names the frozen contract the targets were rendered
	// from.
	ContractIdentity string `json:"contract_identity"`
	// ExpectedSchemaDigest is the ExpectedSchema the operation attests against,
	// empty exactly when the path performs no Attestation.
	ExpectedSchemaDigest string `json:"expected_schema_digest,omitempty"`
	// AttestationFootprintSHA256 is the qualification the internal keys came
	// from, empty exactly when the path performs no Attestation.
	AttestationFootprintSHA256 string `json:"attestation_footprint_sha256,omitempty"`
}

// Validate rejects an identity that cannot bind a classifier.
func (identity OperationIdentity) Validate() error {
	if identity.OperationID == "" {
		return errors.New("operation identity names no operation")
	}
	dimensions, known := dimensionsFor(identity.PathKind)
	if !known {
		return fmt.Errorf("operation identity path_kind %q is not a derivable execution path", identity.PathKind)
	}
	if identity.ContractIdentity == "" {
		return errors.New("operation identity names no frozen contract")
	}
	// Presence-coupled in both directions, exactly as the control plan is: an
	// operation that attests must name what against and under which
	// qualification, and one that does not must claim neither.
	switch {
	case dimensions.requiresSchema:
		if !validSHA256(identity.ExpectedSchemaDigest) {
			return fmt.Errorf("path_kind %s attests but names no ExpectedSchema", identity.PathKind)
		}
		if !validSHA256(identity.AttestationFootprintSHA256) {
			return fmt.Errorf("path_kind %s attests but names no qualified footprint", identity.PathKind)
		}
	default:
		if identity.ExpectedSchemaDigest != "" || identity.AttestationFootprintSHA256 != "" {
			return fmt.Errorf("path_kind %s performs no Attestation but carries attestation bindings", identity.PathKind)
		}
	}
	return nil
}

// TargetContractIdentity derives a target's contract identity from its
// operation's. It is the single construction both TargetEntry and the compiled
// classifier use, so a target cannot claim a contract the operation does not own.
func TargetContractIdentity(operationContractIdentity string, class GatewayStatementClassV3) (string, error) {
	if operationContractIdentity == "" {
		return "", errors.New("a target identity must be derived from a frozen operation contract")
	}
	switch class {
	case V3TargetedVisible:
		return operationContractIdentity + "#visible", nil
	case V3TargetedCompanion:
		return operationContractIdentity + "#companion", nil
	default:
		return "", fmt.Errorf("class %s is not a target statement", class)
	}
}

// requiredTargets is the exact target cardinality each path produces.
func requiredTargets(kind GatewayPathKind) (visible, companion int64, known bool) {
	dimensions, known := dimensionsFor(kind)
	return dimensions.visible, dimensions.companion, known
}

// CompiledClassifier is an immutable operation-bound lookup.
//
// It is compiled rather than scanned: the manifest is validated once, its
// entries are frozen into a map keyed by the full classifier key, and every
// later classification is a lookup. A linear scan over a mutable slice would let
// an entry appended after validation take effect without changing any digest.
type CompiledClassifier struct {
	version        string
	operation      OperationIdentity
	manifestSHA256 string
	bindingSHA256  string
	entries        map[classifierKey]ClassifierEntry
	// targets keeps the target entries per class so cardinality is answerable
	// after compilation without walking the whole table.
	targets map[GatewayStatementClassV3][]ClassifierEntry
}

// CompileClassifier validates a manifest against one operation and freezes it.
//
// The cardinality check is what stops a manifest from being reused across paths:
// a paired-novel operation must define exactly one visible and one companion
// target, a single-query one exactly one visible and no companion, and a replay
// none at all. A manifest carrying a companion cannot classify for a path that
// issues none, even though every control entry would match.
func CompileClassifier(operation OperationIdentity, manifest ClassifierManifest) (*CompiledClassifier, error) {
	if err := operation.Validate(); err != nil {
		return nil, err
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	manifestDigest, err := manifest.SHA256()
	if err != nil {
		return nil, err
	}

	entries := make(map[classifierKey]ClassifierEntry, len(manifest.Entries))
	targets := map[GatewayStatementClassV3][]ClassifierEntry{}
	for _, entry := range manifest.Entries {
		key := classifierKey{digest: entry.StrictASTSHA256, topLevel: entry.RequiredTopLevel}
		// Validate already rejects an ambiguous manifest; this is the compiled
		// restatement, so a future manifest change cannot quietly lose it.
		if _, duplicate := entries[key]; duplicate {
			return nil, fmt.Errorf("classifier manifest maps one key to two entries for class %s", entry.Class)
		}
		entries[key] = entry
		switch entry.Class {
		case V3TargetedVisible, V3TargetedCompanion:
			targets[entry.Class] = append(targets[entry.Class], entry)
			// Every target must belong to the operation's own frozen contract.
			// Without this a manifest could carry another workload's target,
			// which no class count would notice.
			expected, err := TargetContractIdentity(operation.ContractIdentity, entry.Class)
			if err != nil {
				return nil, err
			}
			if entry.ContractIdentity != expected {
				return nil, fmt.Errorf("target class %s is bound to contract %q, the operation requires %q",
					entry.Class, entry.ContractIdentity, expected)
			}
		case V3PostgreSQLInternalAttestation:
			if entry.FootprintSHA256 != operation.AttestationFootprintSHA256 {
				return nil, fmt.Errorf("internal key %s came from qualification %s, the operation is bound to %s",
					shortDigest(entry.StrictASTSHA256), shortDigest(entry.FootprintSHA256),
					shortDigest(operation.AttestationFootprintSHA256))
			}
		}
	}

	visible, companion, _ := requiredTargets(operation.PathKind)
	if got := int64(len(targets[V3TargetedVisible])); got != visible {
		return nil, fmt.Errorf("path_kind %s executes %d visible target statements, the manifest defines %d",
			operation.PathKind, visible, got)
	}
	if got := int64(len(targets[V3TargetedCompanion])); got != companion {
		return nil, fmt.Errorf("path_kind %s executes %d companion target statements, the manifest defines %d",
			operation.PathKind, companion, got)
	}

	binding, err := classifierBindingSHA256(operation, manifestDigest)
	if err != nil {
		return nil, err
	}
	return &CompiledClassifier{
		version: CompiledClassifierVersion, operation: operation,
		manifestSHA256: manifestDigest, bindingSHA256: binding,
		entries: entries, targets: targets,
	}, nil
}

// classifierBindingSHA256 is the digest of the operation-to-manifest binding.
func classifierBindingSHA256(operation OperationIdentity, manifestSHA256 string) (string, error) {
	canonical, err := approval.CanonicalJSON(struct {
		Version        string            `json:"version"`
		Operation      OperationIdentity `json:"operation"`
		ManifestSHA256 string            `json:"manifest_sha256"`
	}{CompiledClassifierVersion, operation, manifestSHA256})
	if err != nil {
		return "", fmt.Errorf("canonicalize classifier binding: %w", err)
	}
	hash := sha256.New()
	hash.Write([]byte(compiledClassifierDomain + "\x00"))
	hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Classify resolves one observed statement. A key the classifier does not define
// is V3Unexpected -- the closed world's sink, which no plan expects.
func (classifier *CompiledClassifier) Classify(strictASTSHA256 string, topLevel bool) GatewayStatementClassV3 {
	entry, found := classifier.entries[classifierKey{digest: strictASTSHA256, topLevel: topLevel}]
	if !found {
		return V3Unexpected
	}
	return entry.Class
}

// Operation is the identity this classifier is bound to.
func (classifier *CompiledClassifier) Operation() OperationIdentity { return classifier.operation }

// ManifestSHA256 is the digest of the manifest that was compiled.
func (classifier *CompiledClassifier) ManifestSHA256() string { return classifier.manifestSHA256 }

// BindingSHA256 is the digest of the operation-to-manifest binding. It enters
// the sealed evidence, and the finalizer recomputes it from its own independently
// derived operation identity and manifest.
func (classifier *CompiledClassifier) BindingSHA256() string { return classifier.bindingSHA256 }

// RequireBinding rejects a classifier that was not compiled for this operation
// and manifest.
func (classifier *CompiledClassifier) RequireBinding(operation OperationIdentity, manifestSHA256 string) error {
	expected, err := classifierBindingSHA256(operation, manifestSHA256)
	if err != nil {
		return err
	}
	if expected != classifier.bindingSHA256 {
		return fmt.Errorf("classifier is bound to %s, the finalizer derives %s",
			shortDigest(classifier.bindingSHA256), shortDigest(expected))
	}
	return nil
}

// InternalKeys is the set of PostgreSQL-internal keys the classifier accepts, in
// canonical order. The finalizer compares it against the qualified footprint's
// keys, so an internal statement outside the qualification cannot be classified
// as expected.
func (classifier *CompiledClassifier) InternalKeys() []string {
	var keys []string
	for key, entry := range classifier.entries {
		if entry.Class == V3PostgreSQLInternalAttestation {
			keys = append(keys, key.digest)
		}
	}
	sort.Strings(keys)
	return keys
}

// TargetKeys is the set of target keys for one class, in canonical order.
func (classifier *CompiledClassifier) TargetKeys(class GatewayStatementClassV3) []string {
	var keys []string
	for _, entry := range classifier.targets[class] {
		keys = append(keys, entry.StrictASTSHA256)
	}
	sort.Strings(keys)
	return keys
}
