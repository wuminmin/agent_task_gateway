package catalog

import (
	"time"

	"taskbound.local/agent-data-gateway/internal/domain"
)

// Catalog is the complete, versioned logical data contract loaded at startup.
type Catalog struct {
	CatalogVersion string `yaml:"catalog_version" json:"catalog_version"`
	// SHA256 is the lowercase digest of the exact validated catalog artifact
	// bytes. It is carried into manifests and receipts so formatting or policy
	// edits necessarily create a new authorization binding.
	SHA256               string                `yaml:"-" json:"catalog_sha256"`
	Sources              []Source              `yaml:"sources" json:"sources"`
	SnapshotPublications []SnapshotPublication `yaml:"snapshot_publications,omitempty" json:"snapshot_publications,omitempty"`
	Products             []Product             `yaml:"products" json:"products"`
	Scopes               []Scope               `yaml:"scopes" json:"scopes"`
	ApprovalRoutes       []ApprovalRoute       `yaml:"approval_routes" json:"approval_routes"`
	BudgetProfiles       []BudgetProfile       `yaml:"budget_profiles" json:"budget_profiles"`
}

// V4Enabled reports the deployment mode selected by the validated Catalog.
// Validation guarantees that snapshot publications and every reachable
// approval route agree on this mode; scanning both keeps the method
// fail-closed for callers holding a manually constructed Catalog in tests.
func (c *Catalog) V4Enabled() bool {
	if c == nil {
		return false
	}
	if len(c.SnapshotPublications) != 0 {
		return true
	}
	profiles := make(map[string]string, len(c.BudgetProfiles))
	for _, profile := range c.BudgetProfiles {
		profiles[profile.Name] = profile.ExposureProfileVersion
	}
	for _, route := range c.ApprovalRoutes {
		if profiles[route.BudgetProfile] == exposureProfileV4 {
			return true
		}
	}
	return false
}

// SnapshotPublication binds an immutable business snapshot to the exact V4
// ordinal sidecar and content-addressed dictionary artifacts approved by the
// Catalog. Digests are verified again when SnapshotRegistry resolves it.
type SnapshotPublication struct {
	Name             string `yaml:"name" json:"name"`
	Source           string `yaml:"source" json:"source"`
	SourceNamespace  string `yaml:"source_namespace" json:"source_namespace"`
	Snapshot         string `yaml:"snapshot" json:"snapshot"`
	OrdinalSidecar   string `yaml:"ordinal_sidecar" json:"ordinal_sidecar"`
	SidecarDigest    string `yaml:"sidecar_digest" json:"sidecar_digest"`
	DictionaryDigest string `yaml:"dictionary_digest" json:"dictionary_digest"`
	ManifestDigest   string `yaml:"manifest_digest" json:"manifest_digest"`
}

type Source struct {
	Name                   string `yaml:"name" json:"name"`
	DatasourceID           string `yaml:"datasource_id" json:"datasource_id"`
	Type                   string `yaml:"type" json:"type"`
	Address                string `yaml:"address" json:"address"`
	Port                   int    `yaml:"port" json:"port"`
	Database               string `yaml:"database" json:"database"`
	User                   string `yaml:"user" json:"user"`
	PostgreSQLMajorVersion int    `yaml:"postgres_major_version" json:"postgres_major_version"`
	SchemaDigest           string `yaml:"schema_digest" json:"schema_digest"`
	SecretRef              string `yaml:"secretRef" json:"secret_ref"`

	// These fields exist only so a strict decoder can return a deliberate,
	// redacted security error instead of treating secret-bearing input as an
	// ordinary unknown field.
	Password string `yaml:"password,omitempty" json:"-"`
	DSN      string `yaml:"dsn,omitempty" json:"-"`
}

type Product struct {
	Name                string             `yaml:"name" json:"name"`
	Source              string             `yaml:"source" json:"source"`
	ReportingView       string             `yaml:"reporting_view" json:"reporting_view"`
	Description         string             `yaml:"description" json:"description"`
	Sensitivity         domain.Sensitivity `yaml:"sensitivity" json:"sensitivity"`
	Fields              []Field            `yaml:"fields" json:"fields"`
	Scopes              []string           `yaml:"scopes" json:"scopes"`
	AllowedFunctions    []string           `yaml:"allowed_functions,omitempty" json:"allowed_functions,omitempty"`
	AllowedOperators    []string           `yaml:"allowed_operators,omitempty" json:"allowed_operators,omitempty"`
	AllowedAggregates   []string           `yaml:"allowed_aggregates,omitempty" json:"allowed_aggregates,omitempty"`
	Snapshot            string             `yaml:"snapshot" json:"snapshot"`
	SnapshotPublication string             `yaml:"snapshot_publication,omitempty" json:"snapshot_publication,omitempty"`
	EntityKey           []string           `yaml:"entity_key" json:"entity_key"`
	// FactNamespace is the Catalog-owned canonical semantic relation, not the
	// logical reporting view name. StableRelationRole disambiguates self-joins.
	FactNamespace      string `yaml:"fact_namespace,omitempty" json:"fact_namespace,omitempty"`
	StableRelationRole string `yaml:"stable_relation_role,omitempty" json:"stable_relation_role,omitempty"`
	// A derived/generalized product must pin its trusted base-lineage manifest.
	LineageManifestDigest string `yaml:"lineage_manifest_digest,omitempty" json:"lineage_manifest_digest,omitempty"`
}

type Field struct {
	Name             string             `yaml:"name" json:"name"`
	Type             string             `yaml:"type" json:"type"`
	Description      string             `yaml:"description" json:"description"`
	Sensitivity      domain.Sensitivity `yaml:"sensitivity,omitempty" json:"sensitivity,omitempty"`
	Collation        string             `yaml:"collation,omitempty" json:"collation,omitempty"`
	CollationVersion string             `yaml:"collation_version,omitempty" json:"collation_version,omitempty"`
}

type Scope struct {
	Name          string   `yaml:"name" json:"name"`
	Type          string   `yaml:"type" json:"type"`
	Description   string   `yaml:"description" json:"description"`
	AllowedValues []string `yaml:"allowed_values,omitempty" json:"allowed_values,omitempty"`
	Min           string   `yaml:"min,omitempty" json:"min,omitempty"`
	Max           string   `yaml:"max,omitempty" json:"max,omitempty"`
}

const (
	ScopeTypeEnum      = "enum"
	ScopeTypeDateRange = "date_range"
)

type ApprovalRoute struct {
	Sensitivity   domain.Sensitivity  `yaml:"sensitivity" json:"sensitivity"`
	Mode          domain.ApprovalMode `yaml:"mode" json:"mode"`
	Approver      string              `yaml:"approver,omitempty" json:"approver,omitempty"`
	BudgetProfile string              `yaml:"budget_profile" json:"budget_profile"`
}

type BudgetProfile struct {
	Name                   string   `yaml:"name" json:"name"`
	MaxQueries             int64    `yaml:"max_queries" json:"max_queries"`
	MaxRows                int64    `yaml:"max_rows" json:"max_rows"`
	MaxDBTime              Duration `yaml:"max_db_time" json:"max_db_time"`
	QueryTimeout           Duration `yaml:"query_timeout" json:"query_timeout"`
	TaskTTL                Duration `yaml:"task_ttl" json:"task_ttl"`
	MaxReleaseFacts        int64    `yaml:"max_release_facts,omitempty" json:"max_release_facts,omitempty"`
	MaxInfluenceFacts      int64    `yaml:"max_influence_facts,omitempty" json:"max_influence_facts,omitempty"`
	MaxOutcomeFacts        int64    `yaml:"max_outcome_facts,omitempty" json:"max_outcome_facts,omitempty"`
	ExposureProfileVersion string   `yaml:"exposure_profile_version,omitempty" json:"exposure_profile_version,omitempty"`
}

func (p BudgetProfile) Budget() domain.Budget {
	return domain.Budget{
		MaxQueries:      p.MaxQueries,
		MaxRows:         p.MaxRows,
		MaxDBTime:       p.MaxDBTime.Duration,
		PerQueryTimeout: p.QueryTimeout.Duration,
		TaskTTL:         p.TaskTTL.Duration,
		MaxReleaseFacts: p.MaxReleaseFacts, MaxInfluenceFacts: p.MaxInfluenceFacts, MaxOutcomeFacts: p.MaxOutcomeFacts,
		ExposureProfileVersion: p.ExposureProfileVersion,
	}
}

// EffectiveSensitivity includes field-level classifications so a product can
// never be routed below its most sensitive published field.
func (p Product) EffectiveSensitivity() (domain.Sensitivity, error) {
	values := []domain.Sensitivity{p.Sensitivity}
	for _, field := range p.Fields {
		if field.Sensitivity != "" {
			values = append(values, field.Sensitivity)
		}
	}
	return domain.HighestSensitivity(values...)
}

func (p Product) FieldNames() []string {
	names := make([]string, 0, len(p.Fields))
	for _, field := range p.Fields {
		names = append(names, field.Name)
	}
	return names
}

// ExpiresAt derives the immutable grant expiry from a selected profile.
func (p BudgetProfile) ExpiresAt(approvedAt time.Time) time.Time {
	return approvedAt.Add(p.TaskTTL.Duration)
}
