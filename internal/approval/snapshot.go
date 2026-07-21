package approval

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DraftBudget is the exact budget envelope shown to and approved by OA. All
// duration values use milliseconds so the snapshot does not lose precision.
type DraftBudget struct {
	MaxQueries     int64 `json:"max_queries"`
	MaxRows        int64 `json:"max_rows"`
	MaxDBMS        int64 `json:"max_db_ms"`
	QueryTimeoutMS int64 `json:"query_timeout_ms"`
	TaskTTLMS      int64 `json:"task_ttl_ms"`
}

// authorizationSnapshot is deliberately separate from DraftRequest so the
// digest can never become self-referential. It binds every value that controls
// the eventual grant, together with the task and callback identities.
type authorizationSnapshot struct {
	TaskID          string              `json:"task_id"`
	Requester       string              `json:"requester"`
	Objective       string              `json:"objective"`
	DataProducts    []string            `json:"data_products"`
	ApprovedColumns map[string][]string `json:"approved_columns"`
	MandatoryScope  map[string]any      `json:"mandatory_scope"`
	Sensitivity     string              `json:"sensitivity"`
	Budget          DraftBudget         `json:"budget"`
	ApprovalMode    string              `json:"approval_mode"`
	Approver        string              `json:"approver,omitempty"`
	CatalogVersion  string              `json:"catalog_version"`
	CallbackContext string              `json:"callback_context"`
}

// AuthorizationSnapshotSHA256 returns the stable lowercase SHA-256 of the
// immutable OA authorization snapshot. encoding/json sorts string map keys,
// making the digest independent of Go map iteration order.
func AuthorizationSnapshotSHA256(request DraftRequest) (string, error) {
	material := authorizationSnapshot{
		TaskID: request.TaskID, Requester: request.Requester, Objective: request.Objective,
		DataProducts: request.DataProducts, ApprovedColumns: request.ApprovedColumns,
		MandatoryScope: request.MandatoryScope, Sensitivity: request.Sensitivity,
		Budget: request.Budget, ApprovalMode: request.ApprovalMode, Approver: request.Approver,
		CatalogVersion: request.CatalogVersion, CallbackContext: request.CallbackContext,
	}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "", fmt.Errorf("encode authorization snapshot: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ValidateAuthorizationSnapshot rejects incomplete or internally inconsistent
// grant snapshots and verifies that the supplied digest covers their contents.
func ValidateAuthorizationSnapshot(request DraftRequest) error {
	if strings.TrimSpace(request.TaskID) == "" || strings.TrimSpace(request.Requester) == "" || strings.TrimSpace(request.Objective) == "" {
		return errors.New("task_id, requester, and objective are required")
	}
	if len(request.DataProducts) == 0 || len(request.ApprovedColumns) == 0 {
		return errors.New("data_products and approved_columns are required")
	}
	products := make(map[string]struct{}, len(request.DataProducts))
	for _, product := range request.DataProducts {
		if strings.TrimSpace(product) == "" {
			return errors.New("data_products cannot contain an empty product")
		}
		if _, duplicate := products[product]; duplicate {
			return fmt.Errorf("duplicate data product %q", product)
		}
		products[product] = struct{}{}
		columns, ok := request.ApprovedColumns[product]
		if !ok || len(columns) == 0 {
			return fmt.Errorf("approved_columns missing for %q", product)
		}
		seenColumns := make(map[string]struct{}, len(columns))
		for _, column := range columns {
			if strings.TrimSpace(column) == "" {
				return fmt.Errorf("approved_columns for %q contains an empty column", product)
			}
			if _, duplicate := seenColumns[column]; duplicate {
				return fmt.Errorf("duplicate approved column %q.%q", product, column)
			}
			seenColumns[column] = struct{}{}
		}
	}
	for product := range request.ApprovedColumns {
		if _, approved := products[product]; !approved {
			return fmt.Errorf("approved_columns supplied for unapproved product %q", product)
		}
	}
	if len(request.MandatoryScope) == 0 {
		return errors.New("mandatory_scope is required")
	}
	for name, value := range request.MandatoryScope {
		if strings.TrimSpace(name) == "" || !validScopeSnapshotValue(value) {
			return fmt.Errorf("mandatory_scope %q has an invalid value", name)
		}
	}
	switch request.Sensitivity {
	case "low", "medium", "high":
	default:
		return errors.New("sensitivity must be low, medium, or high")
	}
	if strings.TrimSpace(request.CatalogVersion) == "" || strings.TrimSpace(request.CallbackContext) == "" {
		return errors.New("catalog_version and callback_context are required")
	}
	if request.Budget.MaxQueries <= 0 || request.Budget.MaxRows <= 0 || request.Budget.MaxDBMS <= 0 || request.Budget.QueryTimeoutMS <= 0 || request.Budget.TaskTTLMS <= 0 {
		return errors.New("all budget limits must be positive")
	}
	if request.Budget.QueryTimeoutMS > request.Budget.MaxDBMS {
		return errors.New("query_timeout_ms cannot exceed max_db_ms")
	}
	switch request.ApprovalMode {
	case "auto":
		if request.Approver != "" {
			return errors.New("auto approval cannot specify an approver")
		}
	case "manual":
		if strings.TrimSpace(request.Approver) == "" {
			return errors.New("manual approval requires an approver")
		}
	default:
		return errors.New("approval_mode must be auto or manual")
	}
	provided, err := hex.DecodeString(request.AuthorizationSnapshotSHA256)
	if err != nil || len(provided) != sha256.Size {
		return errors.New("authorization_snapshot_sha256 must be a SHA-256 hex digest")
	}
	expectedHex, err := AuthorizationSnapshotSHA256(request)
	if err != nil {
		return err
	}
	expected, _ := hex.DecodeString(expectedHex)
	if subtle.ConstantTimeCompare(provided, expected) != 1 {
		return errors.New("authorization snapshot digest does not match its contents")
	}
	return nil
}

func validScopeSnapshotValue(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed != ""
	case []string:
		return validScopeStrings(typed)
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			value, ok := item.(string)
			if !ok {
				return false
			}
			values = append(values, value)
		}
		return validScopeStrings(values)
	case map[string]string:
		return validScopeStringMap(typed)
	case map[string]any:
		values := make(map[string]string, len(typed))
		for name, item := range typed {
			value, ok := item.(string)
			if !ok {
				return false
			}
			values[name] = value
		}
		return validScopeStringMap(values)
	default:
		return false
	}
}

func validScopeStrings(values []string) bool {
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

func validScopeStringMap(values map[string]string) bool {
	if len(values) == 0 {
		return false
	}
	hasValue := false
	for name, value := range values {
		if strings.TrimSpace(name) == "" {
			return false
		}
		hasValue = hasValue || value != ""
	}
	return hasValue
}
