package main

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/domain"
)

func catalogArtifact(publication loadedPublication) (*catalog.Catalog, []byte, error) {
	input := publication.Input
	bundle := publication.Bundle
	logical := catalog.Catalog{
		CatalogVersion: fmt.Sprintf("rq5-online-%s-%s", publication.Day, bundle.ManifestDigest[:12]),
		Sources: []catalog.Source{{
			Name: input.CatalogSource, DatasourceID: input.Snapshot.SourceID, Type: "postgres",
			Address: "business-postgres", Port: 5432, Database: businessDatabase + "_" + publication.Day, User: businessUser,
			PostgreSQLMajorVersion: postgresMajor, SchemaDigest: input.Snapshot.SchemaDigest,
			SecretRef: "DAILY_GATEWAY_DB_PASSWORD",
		}},
		SnapshotPublications: []catalog.SnapshotPublication{{
			Name: input.PublicationName, Source: input.CatalogSource,
			SourceNamespace: input.Snapshot.SourceNamespace, Snapshot: input.Snapshot.Snapshot,
			OrdinalSidecar: input.OrdinalSidecar, SidecarDigest: bundle.DictionaryManifest.SidecarDigest,
			DictionaryDigest: bundle.DictionaryManifest.DictionaryDigest, ManifestDigest: bundle.ManifestDigest,
		}},
		Scopes: []catalog.Scope{{
			Name: "dataset_partition", Type: catalog.ScopeTypeEnum,
			Description: "Fixed deterministic online correctness fixture partition", AllowedValues: []string{"1"},
		}},
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
			Snapshot: input.Snapshot.Snapshot, SnapshotPublication: input.PublicationName,
			EntityKey: []string{"l_orderkey", "l_linenumber"}, FactNamespace: input.Snapshot.SourceNamespace,
			StableRelationRole: "daily_lineitem",
		}},
		ApprovalRoutes: []catalog.ApprovalRoute{{
			Sensitivity: domain.SensitivityLow, Mode: domain.ApprovalModeManual,
			Approver: "bob", BudgetProfile: "rq5-online-v4",
		}},
		BudgetProfiles: []catalog.BudgetProfile{{
			Name: "rq5-online-v4", MaxQueries: 20, MaxRows: 100, MaxDBTime: catalog.Duration{Duration: time.Minute},
			QueryTimeout: catalog.Duration{Duration: 15 * time.Second}, TaskTTL: catalog.Duration{Duration: time.Hour},
			MaxReleaseFacts: 1000, MaxInfluenceFacts: 100000, MaxOutcomeFacts: 20,
			ExposureProfileVersion: "taskgate-exposure-v5",
			PredicateFootprint: &domain.PredicateFootprintLimitsV1{
				Version: domain.PredicateFootprintV1, MaxRawLiteralsPerQuery: 64,
				MaxUniqueAtomsPerQuery: 8, MaxAtomPayloadBytes: 4096,
				MaxTotalAtomPayloadBytes: 32768,
			},
		}},
	}
	encoded, err := yaml.Marshal(logical)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := catalog.Parse(encoded)
	if err != nil {
		return nil, nil, fmt.Errorf("parse generated %s Catalog: %w", publication.Day, err)
	}
	return parsed, encoded, nil
}
