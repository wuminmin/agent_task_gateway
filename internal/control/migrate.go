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
	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
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
		err = s.db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, version).Scan(&exists)
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
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return opErr("begin migration", ErrConflict, err)
		}
		if _, err = tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return opErr("apply migration", ErrConflict, fmt.Errorf("%s: %w", entry.Name(), err))
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, version, formatTime(s.now())); err != nil {
			_ = tx.Rollback()
			return opErr("record migration", ErrConflict, err)
		}
		if err = tx.Commit(); err != nil {
			return opErr("commit migration", ErrConflict, err)
		}
	}
	return nil
}
