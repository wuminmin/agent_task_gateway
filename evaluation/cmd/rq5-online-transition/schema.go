package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

const (
	businessDatabase = "taskgate_daily"
	businessUser     = "gateway_reader"
	postgresMajor    = 16
)

func expectedViewSchema(input snapshotbundle.CompilerInput) (dataconnector.ViewSchema, error) {
	schema, view, found := strings.Cut(input.SourceRelation, ".")
	if !found || schema != "reporting" || view == "" {
		return dataconnector.ViewSchema{}, errors.New("compiler input has an invalid reporting source_relation")
	}
	return dataconnector.ViewSchema{
		Schema: schema,
		View:   view,
		// The connector obtains the live pg_get_viewdef value and includes it in
		// the attested digest. ExpectedSchema pins the exact ordered columns;
		// ExpectedSchemaDigest, when supplied, pins the resulting definition too.
		Columns: []dataconnector.SchemaColumn{
			{Name: "dataset_partition", PostgreSQLType: "smallint", CollationDeterministic: true},
			{Name: "l_orderkey", PostgreSQLType: "bigint", CollationDeterministic: true},
			{Name: "l_linenumber", PostgreSQLType: "integer", CollationDeterministic: true},
			{Name: "l_extendedprice", PostgreSQLType: "numeric", CollationDeterministic: true},
		},
	}, nil
}

func openPublicationConnector(ctx context.Context, dsn string, input snapshotbundle.CompilerInput,
	expectedSchemaDigest, expectedDatabase string) (*dataconnector.Connector, dataconnector.Attestation, error) {
	view, err := expectedViewSchema(input)
	if err != nil {
		return nil, dataconnector.Attestation{}, err
	}
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: dsn, StatementTimeout: 30 * time.Second, ConnectTimeout: 10 * time.Second,
		MaxRows: 1000, MaxConnections: 4, ApplicationName: "taskgate-rq5-online-transition",
		ExpectedSchema:       []dataconnector.ViewSchema{view},
		ExpectedSchemaDigest: expectedSchemaDigest,
		ExpectedAttestation: dataconnector.ExpectedAttestation{
			DatasourceID: input.Snapshot.SourceID, Database: expectedDatabase,
			User: businessUser, PostgreSQLMajorVersion: postgresMajor,
		},
	})
	if err != nil {
		return nil, dataconnector.Attestation{}, err
	}
	attestation, err := connector.Attestation(ctx)
	if err != nil {
		connector.Close()
		return nil, dataconnector.Attestation{}, err
	}
	if !sha256Regexp.MatchString(attestation.SchemaDigest) {
		connector.Close()
		return nil, dataconnector.Attestation{}, fmt.Errorf("live schema attestation is not SHA-256: %q", attestation.SchemaDigest)
	}
	return connector, attestation, nil
}
