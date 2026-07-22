package domain

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

const (
	AuthorizationManifestV1Version = "1"
	TaskGrantCoreV1Version         = "1"
	TaskGrantV1Version             = "1"
	ApprovalReceiptV1Version       = "1"
)

var (
	ErrInvalidAuthorizationManifest = errors.New("invalid authorization manifest")
	ErrInvalidTaskGrantCore         = errors.New("invalid task grant core")
	ErrInvalidTaskGrant             = errors.New("invalid task grant")
	ErrInvalidApprovalReceipt       = errors.New("invalid approval receipt")
)

// AuthorizationBudgetV1 is the complete, integer-millisecond authorization
// budget exchanged between Gateway and OA. Milliseconds avoid implementation-
// specific duration encodings in the signed protocol objects.
type AuthorizationBudgetV1 struct {
	MaxQueries        int64 `json:"max_queries"`
	MaxResultRows     int64 `json:"max_result_rows"`
	MaxDBMS           int64 `json:"max_db_ms"`
	PerQueryTimeoutMS int64 `json:"per_query_timeout_ms"`
	TaskTTLMS         int64 `json:"task_ttl_ms"`
}

func (b AuthorizationBudgetV1) Validate() error {
	if b.MaxQueries <= 0 || b.MaxResultRows <= 0 || b.MaxDBMS <= 0 || b.PerQueryTimeoutMS <= 0 || b.TaskTTLMS <= 0 {
		return errors.New("all authorization budget limits must be positive")
	}
	if b.PerQueryTimeoutMS > b.MaxDBMS {
		return errors.New("per_query_timeout_ms cannot exceed max_db_ms")
	}
	const maxDurationMilliseconds = int64(^uint64(0)>>1) / int64(time.Millisecond)
	const maxSafeJSONInteger = int64(1<<53 - 1)
	if b.MaxQueries > maxSafeJSONInteger || b.MaxResultRows > maxSafeJSONInteger ||
		b.MaxDBMS > maxDurationMilliseconds || b.PerQueryTimeoutMS > maxDurationMilliseconds ||
		b.TaskTTLMS > maxDurationMilliseconds {
		return errors.New("authorization budget exceeds the V1 interoperable integer or duration range")
	}
	return nil
}

func (b AuthorizationBudgetV1) EnsureWithin(parent AuthorizationBudgetV1) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := parent.Validate(); err != nil {
		return fmt.Errorf("parent budget: %w", err)
	}
	if b.MaxQueries > parent.MaxQueries || b.MaxResultRows > parent.MaxResultRows ||
		b.MaxDBMS > parent.MaxDBMS || b.PerQueryTimeoutMS > parent.PerQueryTimeoutMS ||
		b.TaskTTLMS > parent.TaskTTLMS {
		return errors.New("authorization budget exceeds parent")
	}
	return nil
}

// AuthorizationManifestV1 is the immutable authorization request. Gateway
// derives HumanSubject and AgentID from the authenticated principal; neither
// is accepted from request_data_task arguments.
type AuthorizationManifestV1 struct {
	Version           string                `json:"version"`
	TaskID            string                `json:"task_id"`
	HumanSubject      string                `json:"human_subject"`
	AgentID           string                `json:"agent_id"`
	DeclaredObjective string                `json:"declared_objective"`
	Products          []string              `json:"products"`
	ApprovedColumns   map[string][]string   `json:"approved_columns"`
	MandatoryScope    map[string]any        `json:"mandatory_scope"`
	Sensitivity       Sensitivity           `json:"sensitivity"`
	Budget            AuthorizationBudgetV1 `json:"budget"`
	CatalogVersion    string                `json:"catalog_version"`
	CatalogSHA256     string                `json:"catalog_sha256"`
	CallbackContext   string                `json:"callback_context"`
	Nonce             string                `json:"nonce"`
}

func (m AuthorizationManifestV1) Validate() error {
	if m.Version != AuthorizationManifestV1Version {
		return fmt.Errorf("%w: unsupported version %q", ErrInvalidAuthorizationManifest, m.Version)
	}
	if strings.TrimSpace(m.TaskID) == "" || strings.TrimSpace(m.HumanSubject) == "" ||
		strings.TrimSpace(m.AgentID) == "" || strings.TrimSpace(m.DeclaredObjective) == "" {
		return fmt.Errorf("%w: task_id, human_subject, agent_id, and declared_objective are required", ErrInvalidAuthorizationManifest)
	}
	if err := validateAuthorizationEnvelope(m.Products, m.ApprovedColumns, m.MandatoryScope); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAuthorizationManifest, err)
	}
	if err := m.Sensitivity.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAuthorizationManifest, err)
	}
	if err := m.Budget.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAuthorizationManifest, err)
	}
	if strings.TrimSpace(m.CatalogVersion) == "" || !isSHA256Hex(m.CatalogSHA256) ||
		strings.TrimSpace(m.CallbackContext) == "" || len(m.Nonce) != 32 || !isLowerHex(m.Nonce) {
		return fmt.Errorf("%w: catalog version/digest, callback_context, and nonce are required", ErrInvalidAuthorizationManifest)
	}
	return nil
}

// TaskGrantCoreV1 is the OA-selected authorization envelope. It intentionally
// excludes approval evidence so it can be hashed and narrowed independently.
type TaskGrantCoreV1 struct {
	Version            string                `json:"version"`
	TaskID             string                `json:"task_id"`
	HumanSubject       string                `json:"human_subject"`
	AgentID            string                `json:"agent_id"`
	DeclaredObjective  string                `json:"declared_objective"`
	ApprovedProducts   []string              `json:"approved_products"`
	ApprovedColumns    map[string][]string   `json:"approved_columns"`
	MandatoryScope     map[string]any        `json:"mandatory_scope"`
	SensitivityCeiling Sensitivity           `json:"sensitivity_ceiling"`
	Budget             AuthorizationBudgetV1 `json:"budget"`
	ExpiresAt          time.Time             `json:"expires_at"`
	CatalogVersion     string                `json:"catalog_version"`
	CatalogSHA256      string                `json:"catalog_sha256"`
	ManifestDigest     string                `json:"manifest_digest"`
}

func (g TaskGrantCoreV1) Validate() error {
	if g.Version != TaskGrantCoreV1Version {
		return fmt.Errorf("%w: unsupported version %q", ErrInvalidTaskGrantCore, g.Version)
	}
	if strings.TrimSpace(g.TaskID) == "" || strings.TrimSpace(g.HumanSubject) == "" ||
		strings.TrimSpace(g.AgentID) == "" || strings.TrimSpace(g.DeclaredObjective) == "" {
		return fmt.Errorf("%w: task and identity fields are required", ErrInvalidTaskGrantCore)
	}
	if err := validateAuthorizationEnvelope(g.ApprovedProducts, g.ApprovedColumns, g.MandatoryScope); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTaskGrantCore, err)
	}
	if err := g.SensitivityCeiling.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTaskGrantCore, err)
	}
	if err := g.Budget.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTaskGrantCore, err)
	}
	if g.ExpiresAt.IsZero() || strings.TrimSpace(g.CatalogVersion) == "" ||
		!isSHA256Hex(g.CatalogSHA256) || !isSHA256Hex(g.ManifestDigest) {
		return fmt.Errorf("%w: expiry and catalog/manifest digests are required", ErrInvalidTaskGrantCore)
	}
	return nil
}

func (g TaskGrantCoreV1) ValidateAt(now time.Time) error {
	if err := g.Validate(); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("%w: current time is required", ErrInvalidTaskGrantCore)
	}
	if !now.Before(g.ExpiresAt) {
		return ErrGrantExpired
	}
	return nil
}

// CheckNarrowing rejects expansion of every authorization dimension. Scope
// enums may become subsets and date ranges may contract; unknown scope keys
// are forbidden because an execution adapter might otherwise ignore them.
func (g TaskGrantCoreV1) CheckNarrowing(candidate TaskGrantCoreV1) error {
	if err := g.Validate(); err != nil {
		return fmt.Errorf("parent: %w", err)
	}
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("candidate: %w", err)
	}
	if candidate.TaskID != g.TaskID || candidate.HumanSubject != g.HumanSubject ||
		candidate.AgentID != g.AgentID || candidate.DeclaredObjective != g.DeclaredObjective ||
		candidate.CatalogVersion != g.CatalogVersion || candidate.CatalogSHA256 != g.CatalogSHA256 ||
		candidate.ManifestDigest != g.ManifestDigest {
		return grantExpansion("identity or authorization provenance changed")
	}
	if !candidate.SensitivityCeiling.AtMost(g.SensitivityCeiling) {
		return grantExpansion("sensitivity ceiling increased")
	}
	if candidate.ExpiresAt.After(g.ExpiresAt) {
		return grantExpansion("expiry extended")
	}
	if err := candidate.Budget.EnsureWithin(g.Budget); err != nil {
		return grantExpansion("budget increased")
	}
	for _, product := range candidate.ApprovedProducts {
		if !contains(g.ApprovedProducts, product) {
			return grantExpansion("product added")
		}
		for _, column := range candidate.ApprovedColumns[product] {
			if !contains(g.ApprovedColumns[product], column) {
				return grantExpansion("column added")
			}
		}
	}
	if !scopeMapNarrower(g.MandatoryScope, candidate.MandatoryScope) {
		return grantExpansion("mandatory scope weakened or changed incompatibly")
	}
	return nil
}

func CoreFromManifest(manifest AuthorizationManifestV1, manifestDigest string, issuedAt time.Time) (TaskGrantCoreV1, error) {
	if err := manifest.Validate(); err != nil {
		return TaskGrantCoreV1{}, err
	}
	if !isSHA256Hex(manifestDigest) || issuedAt.IsZero() {
		return TaskGrantCoreV1{}, fmt.Errorf("%w: manifest digest and issue time are required", ErrInvalidTaskGrantCore)
	}
	issuedAt = issuedAt.UTC()
	return TaskGrantCoreV1{
		Version: TaskGrantCoreV1Version, TaskID: manifest.TaskID,
		HumanSubject: manifest.HumanSubject, AgentID: manifest.AgentID,
		DeclaredObjective:  manifest.DeclaredObjective,
		ApprovedProducts:   append([]string(nil), manifest.Products...),
		ApprovedColumns:    cloneAuthorizationColumns(manifest.ApprovedColumns),
		MandatoryScope:     cloneAuthorizationScope(manifest.MandatoryScope),
		SensitivityCeiling: manifest.Sensitivity, Budget: manifest.Budget,
		ExpiresAt:      issuedAt.Add(time.Duration(manifest.Budget.TaskTTLMS) * time.Millisecond),
		CatalogVersion: manifest.CatalogVersion, CatalogSHA256: manifest.CatalogSHA256,
		ManifestDigest: manifestDigest,
	}, nil
}

type ApprovalDecision string

const (
	ApprovalDecisionApprove ApprovalDecision = "approve"
	ApprovalDecisionReject  ApprovalDecision = "reject"
	ApprovalDecisionNarrow  ApprovalDecision = "narrow"
)

type ApprovalReceiptV1 struct {
	Version             string           `json:"version"`
	ReceiptID           string           `json:"receipt_id"`
	TaskID              string           `json:"task_id"`
	Decision            ApprovalDecision `json:"decision"`
	ManifestDigest      string           `json:"manifest_digest"`
	ApprovedGrantDigest string           `json:"approved_grant_digest"`
	ApproverID          string           `json:"approver_id"`
	IssuedAt            time.Time        `json:"issued_at"`
	KeyID               string           `json:"key_id"`
	Signature           string           `json:"signature"`
}

func (r ApprovalReceiptV1) ValidateUnsigned() error {
	if r.Version != ApprovalReceiptV1Version || strings.TrimSpace(r.ReceiptID) == "" ||
		strings.TrimSpace(r.TaskID) == "" || !isSHA256Hex(r.ManifestDigest) ||
		strings.TrimSpace(r.ApproverID) == "" || r.IssuedAt.IsZero() || strings.TrimSpace(r.KeyID) == "" {
		return fmt.Errorf("%w: required receipt field is missing or invalid", ErrInvalidApprovalReceipt)
	}
	switch r.Decision {
	case ApprovalDecisionApprove, ApprovalDecisionNarrow:
		if !isSHA256Hex(r.ApprovedGrantDigest) {
			return fmt.Errorf("%w: approved grant digest is required", ErrInvalidApprovalReceipt)
		}
	case ApprovalDecisionReject:
		if r.ApprovedGrantDigest != "" {
			return fmt.Errorf("%w: rejected receipt cannot bind a grant", ErrInvalidApprovalReceipt)
		}
	default:
		return fmt.Errorf("%w: unsupported decision %q", ErrInvalidApprovalReceipt, r.Decision)
	}
	return nil
}

func (r ApprovalReceiptV1) Validate() error {
	if err := r.ValidateUnsigned(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Signature) == "" {
		return fmt.Errorf("%w: signature is required", ErrInvalidApprovalReceipt)
	}
	return nil
}

type TaskGrantV1 struct {
	Version         string            `json:"version"`
	Core            TaskGrantCoreV1   `json:"core"`
	ApprovalReceipt ApprovalReceiptV1 `json:"approval_receipt"`
}

func (g TaskGrantV1) Validate() error {
	if g.Version != TaskGrantV1Version {
		return fmt.Errorf("%w: unsupported version %q", ErrInvalidTaskGrant, g.Version)
	}
	if err := g.Core.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTaskGrant, err)
	}
	if err := g.ApprovalReceipt.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTaskGrant, err)
	}
	if g.Core.TaskID != g.ApprovalReceipt.TaskID || g.Core.ManifestDigest != g.ApprovalReceipt.ManifestDigest {
		return fmt.Errorf("%w: core and receipt provenance differ", ErrInvalidTaskGrant)
	}
	if g.ApprovalReceipt.Decision != ApprovalDecisionApprove && g.ApprovalReceipt.Decision != ApprovalDecisionNarrow {
		return fmt.Errorf("%w: receipt does not approve a grant", ErrInvalidTaskGrant)
	}
	return nil
}

func validateAuthorizationEnvelope(products []string, columns map[string][]string, scope map[string]any) error {
	if len(products) == 0 {
		return errors.New("at least one product is required")
	}
	if duplicate := firstDuplicate(products); duplicate != "" {
		return fmt.Errorf("duplicate product %q", duplicate)
	}
	for _, product := range products {
		if strings.TrimSpace(product) == "" {
			return errors.New("product cannot be empty")
		}
		values, ok := columns[product]
		if !ok || len(values) == 0 {
			return fmt.Errorf("approved columns missing for %q", product)
		}
		if duplicate := firstDuplicate(values); duplicate != "" {
			return fmt.Errorf("duplicate approved column %q.%q", product, duplicate)
		}
		for _, column := range values {
			if strings.TrimSpace(column) == "" {
				return fmt.Errorf("approved column is empty for %q", product)
			}
		}
	}
	for product := range columns {
		if !contains(products, product) {
			return fmt.Errorf("columns supplied for unapproved product %q", product)
		}
	}
	if len(scope) == 0 {
		return errors.New("mandatory_scope is required")
	}
	for name, value := range scope {
		if strings.TrimSpace(name) == "" || !validAuthorizationScopeValue(value) {
			return fmt.Errorf("mandatory_scope %q has an invalid value", name)
		}
	}
	return nil
}

func validAuthorizationScopeValue(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed != ""
	case []string:
		return uniqueNonemptyStrings(typed)
	case []any:
		values, ok := anyStrings(typed)
		return ok && uniqueNonemptyStrings(values)
	case map[string]string:
		return validRangeMap(typed)
	case map[string]any:
		values := make(map[string]string, len(typed))
		for name, item := range typed {
			text, ok := item.(string)
			if !ok {
				return false
			}
			values[name] = text
		}
		return validRangeMap(values)
	default:
		return false
	}
}

func uniqueNonemptyStrings(values []string) bool {
	if len(values) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validRangeMap(values map[string]string) bool {
	if len(values) == 0 {
		return false
	}
	for name, value := range values {
		if (name != "from" && name != "to") || value == "" {
			return false
		}
	}
	from, hasFrom := values["from"]
	to, hasTo := values["to"]
	return (!hasFrom || !hasTo) || from <= to
}

func scopeMapNarrower(parent, candidate map[string]any) bool {
	if len(parent) != len(candidate) {
		return false
	}
	for name, parentValue := range parent {
		candidateValue, ok := candidate[name]
		if !ok || !scopeValueNarrower(parentValue, candidateValue) {
			return false
		}
	}
	return true
}

func scopeValueNarrower(parent, candidate any) bool {
	if parentText, ok := parent.(string); ok {
		candidateText, ok := candidate.(string)
		return ok && candidateText == parentText
	}
	if parentValues, ok := scopeStrings(parent); ok {
		candidateValues, ok := scopeStrings(candidate)
		if !ok || len(candidateValues) == 0 {
			return false
		}
		allowed := make(map[string]struct{}, len(parentValues))
		for _, value := range parentValues {
			allowed[value] = struct{}{}
		}
		for _, value := range candidateValues {
			if _, exists := allowed[value]; !exists {
				return false
			}
		}
		return true
	}
	parentRange, ok := scopeRange(parent)
	if !ok {
		return reflect.DeepEqual(parent, candidate)
	}
	candidateRange, ok := scopeRange(candidate)
	if !ok {
		return false
	}
	for key, parentBound := range parentRange {
		candidateBound, exists := candidateRange[key]
		if !exists {
			return false
		}
		if key == "from" && candidateBound < parentBound {
			return false
		}
		if key == "to" && candidateBound > parentBound {
			return false
		}
	}
	return validRangeMap(candidateRange)
}

func scopeStrings(value any) ([]string, bool) {
	switch typed := value.(type) {
	case []string:
		return typed, true
	case []any:
		return anyStrings(typed)
	default:
		return nil, false
	}
}

func anyStrings(values []any) ([]string, bool) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		result = append(result, text)
	}
	return result, true
}

func scopeRange(value any) (map[string]string, bool) {
	switch typed := value.(type) {
	case map[string]string:
		return typed, validRangeMap(typed)
	case map[string]any:
		result := make(map[string]string, len(typed))
		for name, value := range typed {
			text, ok := value.(string)
			if !ok {
				return nil, false
			}
			result[name] = text
		}
		return result, validRangeMap(result)
	default:
		return nil, false
	}
}

func cloneAuthorizationColumns(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source))
	for product, columns := range source {
		result[product] = append([]string(nil), columns...)
	}
	return result
}

func cloneAuthorizationScope(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for name, value := range source {
		switch typed := value.(type) {
		case []string:
			result[name] = append([]string(nil), typed...)
		case []any:
			result[name] = append([]any(nil), typed...)
		case map[string]string:
			copyValue := make(map[string]string, len(typed))
			for key, item := range typed {
				copyValue[key] = item
			}
			result[name] = copyValue
		case map[string]any:
			copyValue := make(map[string]any, len(typed))
			for key, item := range typed {
				copyValue[key] = item
			}
			result[name] = copyValue
		default:
			result[name] = typed
		}
	}
	return result
}

func isSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func isLowerHex(value string) bool {
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return value != ""
}
