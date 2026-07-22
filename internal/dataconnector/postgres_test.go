package dataconnector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestNormalizeConfigDefaultsAndRejectsSecretsInErrors(t *testing.T) {
	config, err := normalizeConfig(Config{DSN: "postgres://reader:super-secret@postgres/demo"})
	if err != nil {
		t.Fatalf("normalizeConfig() error = %v", err)
	}
	if config.StatementTimeout != DefaultStatementTimeout || config.MaxRows != DefaultMaxRows || config.MaxConnections != DefaultMaxConnections {
		t.Fatalf("normalizeConfig() defaults = %+v", config)
	}

	_, err = normalizeConfig(Config{DSN: "postgres://reader:super-secret@postgres/demo", MaxRows: -1})
	if !IsCode(err, CodeInvalidConfig) {
		t.Fatalf("normalizeConfig() error = %v", err)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("safe error leaked DSN: %v", err)
	}
}

func TestCeilingsCanOnlyNarrow(t *testing.T) {
	if got := clampRows(25, 100); got != 25 {
		t.Errorf("clampRows(25, 100) = %d", got)
	}
	if got := clampRows(250, 100); got != 100 {
		t.Errorf("clampRows(250, 100) = %d", got)
	}
	if got := clampRows(0, 100); got != 100 {
		t.Errorf("clampRows(0, 100) = %d", got)
	}

	maximum := 5 * time.Second
	if got := clampTimeout(time.Second, maximum); got != time.Second {
		t.Errorf("clampTimeout(short) = %s", got)
	}
	if got := clampTimeout(30*time.Second, maximum); got != maximum {
		t.Errorf("clampTimeout(long) = %s", got)
	}
	if got := timeoutSetting(500 * time.Microsecond); got != "1ms" {
		t.Errorf("timeoutSetting(sub-ms) = %q", got)
	}
}

func TestSchemaAttestationComparisonIsExact(t *testing.T) {
	expected := []SchemaColumn{{Name: "month", PostgreSQLType: "text"}, {Name: "total_amount", PostgreSQLType: "numeric"}}
	if !sameSchemaColumns(expected, []SchemaColumn{{Name: "month", PostgreSQLType: "TEXT"}, {Name: "total_amount", PostgreSQLType: "numeric"}}) {
		t.Fatal("equivalent PostgreSQL type spelling was rejected")
	}
	for name, actual := range map[string][]SchemaColumn{
		"missing":   {{Name: "month", PostgreSQLType: "text"}},
		"reordered": {{Name: "total_amount", PostgreSQLType: "numeric"}, {Name: "month", PostgreSQLType: "text"}},
		"renamed":   {{Name: "month", PostgreSQLType: "text"}, {Name: "amount", PostgreSQLType: "numeric"}},
		"retyped":   {{Name: "month", PostgreSQLType: "text"}, {Name: "total_amount", PostgreSQLType: "double precision"}},
	} {
		t.Run(name, func(t *testing.T) {
			if sameSchemaColumns(expected, actual) {
				t.Fatalf("schema drift %q was accepted", name)
			}
		})
	}
}

func TestClassifyQueryError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code ErrorCode
	}{
		{name: "deadline", err: context.DeadlineExceeded, code: CodeQueryTimeout},
		{name: "postgres cancellation", err: &pgconn.PgError{Code: "57014"}, code: CodeQueryTimeout},
		{name: "ordinary", err: errors.New("query failed"), code: CodeQueryFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyQueryError(test.err); !IsCode(got, test.code) {
				t.Fatalf("classifyQueryError() = %v, want %s", got, test.code)
			}
		})
	}
}

func TestClosedConnectorFailsSafely(t *testing.T) {
	var connector *Connector
	_, err := connector.Query(context.Background(), QueryRequest{SQL: "SELECT 1", MaxRows: 1})
	if !IsCode(err, CodeConnection) {
		t.Fatalf("Query() error = %v", err)
	}
}
