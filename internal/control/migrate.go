package control

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func (s *Store) migrate(ctx context.Context) error {
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr("begin migrations", ErrConflict, err)
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(728194631044)`); err != nil {
		return opErr("lock migrations", ErrConflict, err)
	}
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version BIGINT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL
)`); err != nil {
		return opErr("create migration table", ErrConflict, err)
	}

	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return opErr("read migrations", ErrConflict, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return opErr("parse migration", ErrInvalid, fmt.Errorf("invalid migration name %q", entry.Name()))
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return opErr("parse migration", ErrInvalid, fmt.Errorf("invalid migration name %q: %w", entry.Name(), err))
		}
		var exists int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = $1`, version).Scan(&exists)
		if err == nil {
			continue
		}
		if !isNoRows(err) {
			return opErr("check migration", ErrConflict, err)
		}
		sqlBytes, err := migrationFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return opErr("read migration", ErrConflict, err)
		}
		if _, err = tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			return opErr("apply migration", ErrConflict, fmt.Errorf("%s: %w", entry.Name(), err))
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES ($1, $2)`, version, dbTime(s.now())); err != nil {
			return opErr("record migration", ErrConflict, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return opErr("commit migrations", ErrConflict, err)
	}
	return nil
}
