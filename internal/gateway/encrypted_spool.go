package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/internal/encryptedspool"
)

const (
	querySpoolThreshold = int64(128 << 20)
	querySpoolChunkSize = 1 << 20
	querySpoolMagic     = "TGSPOOL1"
)

// encryptedQuerySpool keeps the existing Gateway surface while delegating the
// encryption format and lifecycle to the shared spool used by FactSet.
type encryptedQuerySpool struct {
	*encryptedspool.Spool
}

func newEncryptedQuerySpool(baseDir, taskID, queryID string, threshold int64) (*encryptedQuerySpool, error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(queryID) == "" || threshold < 1 {
		return nil, errors.New("encrypted query spool requires task, query, and a positive threshold")
	}
	spool, err := encryptedspool.New(encryptedspool.Config{
		BaseDir: baseDir, DirectoryPrefix: ".taskgate-query-spool-", FileName: "payload.spool",
		Magic: querySpoolMagic, AAD: []byte("taskgate-query-spool-v1\x00" + taskID + "\x00" + queryID),
		Threshold: threshold, ChunkSize: querySpoolChunkSize, UnlinkOnOpen: true, SingleRead: true,
	})
	if err != nil {
		return nil, err
	}
	return &encryptedQuerySpool{Spool: spool}, nil
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) != 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

// writeStoredQueryResult preserves storedQueryResult's JSON field order while
// writing one row at a time. This avoids json.Marshal allocating a second copy
// of a large complete result before the adaptive spool can enforce its limit.
func writeStoredQueryResult(writer io.Writer, result storedQueryResult) error {
	if err := writeAll(writer, []byte(`{"columns":`)); err != nil {
		return err
	}
	columns, err := json.Marshal(result.Columns)
	if err != nil {
		return err
	}
	if err := writeAll(writer, columns); err != nil {
		return err
	}
	if err := writeAll(writer, []byte(`,"rows":`)); err != nil {
		return err
	}
	if result.Rows == nil {
		if err := writeAll(writer, []byte("null")); err != nil {
			return err
		}
	} else {
		if err := writeAll(writer, []byte("[")); err != nil {
			return err
		}
		for index, row := range result.Rows {
			if index != 0 {
				if err := writeAll(writer, []byte(",")); err != nil {
					return err
				}
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				return err
			}
			if err := writeAll(writer, encoded); err != nil {
				return err
			}
		}
		if err := writeAll(writer, []byte("]")); err != nil {
			return err
		}
	}
	if err := writeAll(writer, []byte(`,"row_count":`+strconv.FormatInt(result.RowCount, 10)+`,"database_ms":`+
		strconv.FormatInt(result.DatabaseMS, 10))); err != nil {
		return err
	}
	if len(result.ComponentMS) != 0 {
		component, err := json.Marshal(result.ComponentMS)
		if err != nil {
			return err
		}
		if err := writeAll(writer, []byte(`,"component_ms":`)); err != nil {
			return err
		}
		if err := writeAll(writer, component); err != nil {
			return err
		}
	}
	if err := writeAll(writer, []byte(`,"limited":`+strconv.FormatBool(result.Limited))); err != nil {
		return err
	}
	optional := []struct {
		name    string
		value   any
		present bool
	}{
		{name: "query_plan", value: result.QueryPlan, present: result.QueryPlan != nil},
		{name: "sql_profile", value: result.SQLProfile, present: result.SQLProfile != ""},
		{name: "plan_digest", value: result.PlanDigest, present: result.PlanDigest != ""},
		{name: "output_format", value: result.OutputFormat, present: result.OutputFormat != ""},
		{name: "display_columns", value: result.DisplayColumns, present: len(result.DisplayColumns) != 0},
		{name: "result_order", value: result.ResultOrder, present: len(result.ResultOrder) != 0},
		{name: "semantic_columns", value: result.SemanticColumns, present: len(result.SemanticColumns) != 0},
	}
	for _, field := range optional {
		if !field.present {
			continue
		}
		encoded, err := json.Marshal(field.value)
		if err != nil {
			return err
		}
		if err := writeAll(writer, []byte(`,"`+field.name+`":`)); err != nil {
			return err
		}
		if err := writeAll(writer, encoded); err != nil {
			return err
		}
	}
	return writeAll(writer, []byte(`}`))
}
