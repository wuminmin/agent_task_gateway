package control

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type Store struct {
	db     *sql.DB
	cipher ResultCipher
	clock  Clock
	closed atomic.Bool
}

type Option func(*storeOptions)

type storeOptions struct {
	clock   Clock
	recover bool
}

func WithClock(clock Clock) Option {
	return func(options *storeOptions) {
		if clock != nil {
			options.clock = clock
		}
	}
}

// WithoutStartupRecovery is intended for forensic/repair tooling. Normal
// gateway startup should retain the default recovery behavior.
func WithoutStartupRecovery() Option {
	return func(options *storeOptions) { options.recover = false }
}

// Open opens a SQLite control database, applies embedded migrations, and
// recovers transactions that were in flight when the previous process exited.
func Open(ctx context.Context, dsn string, cipher ResultCipher, options ...Option) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, opErr("open", ErrInvalid, fmt.Errorf("empty SQLite DSN"))
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, opErr("open", ErrConflict, err)
	}
	store, err := New(ctx, db, cipher, options...)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// New initializes a store around an existing SQLite *sql.DB. The store takes
// ownership of db and Close closes it.
func New(ctx context.Context, db *sql.DB, cipher ResultCipher, options ...Option) (*Store, error) {
	if db == nil {
		return nil, opErr("new store", ErrInvalid, fmt.Errorf("nil database"))
	}
	config := storeOptions{clock: systemClock{}, recover: true}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	store := &Store{db: db, cipher: cipher, clock: config.clock}

	// Gateway v1 is deliberately single-instance. A single connection also
	// makes each task's reservation/settlement sequence strictly serialized.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return nil, opErr("ping", ErrConflict, err)
	}
	for _, pragma := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = FULL`,
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return nil, opErr("configure SQLite", ErrConflict, err)
		}
	}
	if err := store.migrate(ctx); err != nil {
		return nil, err
	}
	if config.recover {
		if _, err := store.Recover(ctx); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error {
	if s == nil || s.db == nil || s.closed.Swap(true) {
		return nil
	}
	return s.db.Close()
}

func (s *Store) checkOpen(op string) error {
	if s == nil || s.db == nil || s.closed.Load() {
		return opErr(op, ErrClosed, nil)
	}
	return nil
}

func (s *Store) now() time.Time { return s.clock.Now().UTC() }

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func nullableTime(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return formatTime(*value)
}

func scanNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

func randomID(prefix string) (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(bytes), nil
}

func normalizeJSON(raw json.RawMessage, empty string) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(empty)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func beginTx(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	return db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
}

func rollback(tx *sql.Tx) {
	if tx != nil {
		_ = tx.Rollback()
	}
}
