package gateway

import (
	"context"
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

type datasourceEvidence struct {
	DatasourceID string
	SchemaDigest string
}

func (s *Service) datasourceEvidence(ctx context.Context, products []string) (datasourceEvidence, error) {
	source, err := s.catalogSourceForProducts(products)
	if err != nil {
		return datasourceEvidence{}, err
	}
	attestation, err := s.connector.Attestation(ctx)
	if err != nil {
		return datasourceEvidence{}, err
	}
	if attestation.DatasourceID != source.DatasourceID ||
		attestation.Database != source.Database ||
		attestation.User != source.User ||
		attestation.PostgreSQLMajorVersion != source.PostgreSQLMajorVersion ||
		(source.SchemaDigest != "" && attestation.SchemaDigest != source.SchemaDigest) ||
		!validSnapshotSHA256(attestation.SchemaDigest) {
		return datasourceEvidence{}, &dataconnector.Error{Code: dataconnector.CodeSchemaDrift}
	}
	return datasourceEvidence{DatasourceID: attestation.DatasourceID, SchemaDigest: attestation.SchemaDigest}, nil
}

func (s *Service) catalogSourceForProducts(products []string) (catalog.Source, error) {
	if len(products) == 0 {
		return catalog.Source{}, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "任务必须引用至少一个数据产品"}
	}
	byName := make(map[string]catalog.Source, len(s.catalog.Sources))
	for _, source := range s.catalog.Sources {
		byName[source.Name] = source
	}
	var selected catalog.Source
	for _, productName := range products {
		product, ok := s.catalog.LookupProduct(productName)
		if !ok {
			return catalog.Source{}, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "请求的数据产品不存在"}
		}
		source, ok := byName[product.Source]
		if !ok {
			return catalog.Source{}, fmt.Errorf("validated catalog product %q references missing source %q", product.Name, product.Source)
		}
		if selected.Name == "" {
			selected = source
			continue
		}
		if selected.Name != source.Name {
			return catalog.Source{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "当前实例不支持跨数据源任务"}
		}
	}
	if selected.Name == "" {
		return catalog.Source{}, errors.New("no datasource selected")
	}
	return selected, nil
}
