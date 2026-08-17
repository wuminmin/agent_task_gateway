package experiment

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/domain"
)

type RQ5ProfileBindingRequest struct {
	RegistryPath         string
	ProfileAlias         string
	DatasetBindingSHA256 string
	CatalogFamilyPath    string
	BuildManifestPath    string
	BuildManifestSHA256  string
	SubmissionCommit     string
	GeneratorSHA256      string
	ConfigSHA256         string
}

type RQ5ProfileBindingResolution struct {
	Binding *ProfileBinding
	Family  finalv5profile.RQ5CatalogFamilyIdentity
}

// ResolveRQ5ProfileBinding derives the operation/sample identity independently
// from the sealed source inventory. Caller-provided source digests are checks,
// never the source of the family identity.
func ResolveRQ5ProfileBinding(request RQ5ProfileBindingRequest) (RQ5ProfileBindingResolution, error) {
	var result RQ5ProfileBindingResolution
	owner, err := ResolveTargetedProfileIdentity(request.RegistryPath, request.ProfileAlias)
	if err != nil {
		return result, err
	}
	family, err := finalv5profile.ResolveRQ5CatalogFamilyIdentity(
		request.CatalogFamilyPath, request.BuildManifestPath, request.BuildManifestSHA256,
		finalv5profile.RQ5CatalogFamilyOwner{ProfileID: owner.ProfileID, ProfileAlias: owner.ProfileAlias,
			ClosureSHA256: owner.ClosureSHA256, WorkloadCells: owner.WorkloadCells})
	if err != nil {
		return result, err
	}
	if family.SubmissionCommit != strings.TrimSpace(request.SubmissionCommit) ||
		family.GeneratorSHA256 != strings.TrimSpace(request.GeneratorSHA256) ||
		family.ConfigSHA256 != strings.TrimSpace(request.ConfigSHA256) {
		return result, errors.New("RQ5 caller source identity differs from the sealed build manifest")
	}
	publicationIdentity, err := CanonicalPublicationSetSHA256(family.PublicationNames)
	if err != nil {
		return result, err
	}
	binding := &ProfileBinding{Version: ProfileBindingVersion, ProfileID: owner.ProfileID,
		ClosureSHA256: owner.ClosureSHA256, CatalogSHA256: family.FamilySHA256,
		DatasetBindingSHA256: strings.TrimSpace(request.DatasetBindingSHA256),
		PublicationIdentity:  publicationIdentity}
	if err := binding.Validate(); err != nil {
		return result, fmt.Errorf("RQ5 dynamic Catalog family binding: %w", err)
	}
	result.Binding, result.Family = binding, family
	return result, nil
}

type RQ5DailyCatalogInput struct {
	Day                       string
	PublicationName           string
	CatalogSource             string
	SourceID                  string
	SourceNamespace           string
	SourceRelation            string
	Snapshot                  string
	OrdinalSidecar            string
	PublicationManifestSHA256 string
	DictionarySHA256          string
	SidecarSHA256             string
	SchemaSHA256              string
}

// BuildRQ5DailyCatalog is the one evaluation-harness projection from verified
// daily publication descriptors to the production Catalog parser. The live RQ5
// command and the pre-query evidence validator both call this implementation.
func BuildRQ5DailyCatalog(input RQ5DailyCatalogInput) (*catalog.Catalog, []byte, error) {
	dayIndex := -1
	for index, day := range rq5fixture.Days {
		if input.Day == day {
			dayIndex = index
			break
		}
	}
	wantPublication := fmt.Sprintf("daily-lineitem-%s-r%d", input.Day, rq5fixture.RowsPerPublication)
	wantSnapshot := fmt.Sprintf("rq5-daily-lineitem-%s-rows-%d", input.Day, rq5fixture.RowsPerPublication)
	wantSidecar := fmt.Sprintf("taskgate_ordinal.daily_lineitem_%s_r%d", input.Day, rq5fixture.RowsPerPublication)
	if dayIndex < 0 || input.PublicationName != wantPublication || input.CatalogSource != "daily_reporting" ||
		input.SourceID != "taskgate-eval-daily-publication" || input.SourceNamespace != "evaluation.daily_lineitem" ||
		input.SourceRelation != "reporting.daily_lineitem_"+input.Day || input.Snapshot != wantSnapshot ||
		input.OrdinalSidecar != wantSidecar {
		return nil, nil, errors.New("RQ5 daily Catalog input is outside the frozen day/publication set")
	}
	for name, digest := range map[string]string{
		"publication manifest": input.PublicationManifestSHA256,
		"dictionary":           input.DictionarySHA256, "sidecar": input.SidecarSHA256, "schema": input.SchemaSHA256,
	} {
		if !validSHA256(digest) {
			return nil, nil, fmt.Errorf("RQ5 daily Catalog %s digest is invalid", name)
		}
	}
	logical := catalog.Catalog{
		CatalogVersion: fmt.Sprintf("rq5-online-%s-%s", input.Day, input.PublicationManifestSHA256[:12]),
		Sources: []catalog.Source{{
			Name: input.CatalogSource, DatasourceID: input.SourceID, Type: "postgres",
			Address: "business-postgres", Port: 5432, Database: "taskgate_daily_" + input.Day,
			User: "gateway_reader", PostgreSQLMajorVersion: 16, SchemaDigest: input.SchemaSHA256,
			SecretRef: "DAILY_GATEWAY_DB_PASSWORD",
		}},
		SnapshotPublications: []catalog.SnapshotPublication{{
			Name: input.PublicationName, Source: input.CatalogSource,
			SourceNamespace: input.SourceNamespace,
			Snapshot:        input.Snapshot,
			OrdinalSidecar:  input.OrdinalSidecar,
			SidecarDigest:   input.SidecarSHA256, DictionaryDigest: input.DictionarySHA256,
			ManifestDigest: input.PublicationManifestSHA256,
		}},
		Scopes: []catalog.Scope{{Name: "dataset_partition", Type: catalog.ScopeTypeEnum,
			Description: "Fixed deterministic online correctness fixture partition", AllowedValues: []string{"1"}}},
		Products: []catalog.Product{{
			Name: "daily_lineitem", Source: input.CatalogSource, ReportingView: input.SourceRelation,
			Description: "Frozen deterministic RQ5 daily lineitem publication", Sensitivity: domain.SensitivityLow,
			Fields: []catalog.Field{
				{Name: "dataset_partition", Type: "smallint", Description: "Fixed fixture partition"},
				{Name: "l_orderkey", Type: "bigint", Description: "Stable deterministic order key"},
				{Name: "l_linenumber", Type: "integer", Description: "Stable line number"},
				{Name: "l_extendedprice", Type: "numeric", Description: "Deterministic extended price"},
			},
			Scopes: []string{"dataset_partition"}, AllowedOperators: []string{"=", "<", "<=", ">", ">="},
			Snapshot:            input.Snapshot,
			SnapshotPublication: input.PublicationName, EntityKey: []string{"l_orderkey", "l_linenumber"},
			FactNamespace: input.SourceNamespace, StableRelationRole: "daily_lineitem",
		}},
		ApprovalRoutes: []catalog.ApprovalRoute{{Sensitivity: domain.SensitivityLow,
			Mode: domain.ApprovalModeManual, Approver: "bob", BudgetProfile: "rq5-online-v4"}},
		BudgetProfiles: []catalog.BudgetProfile{{
			Name: "rq5-online-v4", MaxQueries: 20, MaxRows: 100, MaxDBTime: catalog.Duration{Duration: time.Minute},
			QueryTimeout: catalog.Duration{Duration: 15 * time.Second}, TaskTTL: catalog.Duration{Duration: time.Hour},
			MaxReleaseFacts: 1000, MaxInfluenceFacts: 100000, MaxOutcomeFacts: 20,
			ExposureProfileVersion: "taskgate-exposure-v5",
			PredicateFootprint: &domain.PredicateFootprintLimitsV1{Version: domain.PredicateFootprintV1,
				MaxRawLiteralsPerQuery: 64, MaxUniqueAtomsPerQuery: 8, MaxAtomPayloadBytes: 4096,
				MaxTotalAtomPayloadBytes: 32768},
		}},
	}
	encoded, err := yaml.Marshal(logical)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := catalog.Parse(encoded)
	if err != nil {
		return nil, nil, fmt.Errorf("parse generated %s Catalog: %w", input.Day, err)
	}
	return parsed, encoded, nil
}

func expectedRQ5DailyCatalogSHA256(publication RQ5PublicationEvidence) (string, error) {
	logical, _, err := BuildRQ5DailyCatalog(RQ5DailyCatalogInput{Day: publication.Day,
		PublicationName:           publication.PublicationName,
		CatalogSource:             "daily_reporting",
		SourceID:                  "taskgate-eval-daily-publication",
		SourceNamespace:           "evaluation.daily_lineitem",
		SourceRelation:            "reporting.daily_lineitem_" + publication.Day,
		Snapshot:                  fmt.Sprintf("rq5-daily-lineitem-%s-rows-%d", publication.Day, rq5fixture.RowsPerPublication),
		OrdinalSidecar:            fmt.Sprintf("taskgate_ordinal.daily_lineitem_%s_r%d", publication.Day, rq5fixture.RowsPerPublication),
		PublicationManifestSHA256: publication.PublicationManifestSHA256,
		DictionarySHA256:          publication.DictionarySHA256, SidecarSHA256: publication.SidecarSHA256,
		SchemaSHA256: publication.SchemaSHA256})
	if err != nil {
		return "", err
	}
	return logical.SHA256, nil
}
