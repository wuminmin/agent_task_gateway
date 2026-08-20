package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type Store struct {
	db                  *sql.DB
	cipher              ResultCipher
	clock               Clock
	callbackPhaseTiming *callbackPhaseTimingRecorder
	closed              atomic.Bool
}

type Option func(*storeOptions)

type storeOptions struct {
	clock                     Clock
	recover                   bool
	recoveryReceiptBuilder    TerminalReceiptBuilder
	pool                      PoolConfig
	callbackPhaseTimingWriter io.Writer
}

// PoolConfig controls the production Control PostgreSQL connection pool.
// Zero values select the long-standing defaults.  The explicit option exists
// so a deployment that advertises a measured concurrency width can provision
// and attest a matching server-side queue/pool boundary instead of silently
// inheriting the historical ten-connection development limit.
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

const (
	defaultMaxOpenConns = 10
	defaultMaxIdleConns = 4
)

var defaultConnMaxLifetime = 30 * time.Minute

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

// WithRecoveryReceiptBuilder persists signed receipts for queries that startup
// recovery conservatively marks INDETERMINATE. Without it, recovery still
// settles budget safely but leaves receipt backfill to a later gateway read.
func WithRecoveryReceiptBuilder(builder TerminalReceiptBuilder) Option {
	return func(options *storeOptions) { options.recoveryReceiptBuilder = builder }
}

// WithPoolConfig configures the Control PostgreSQL pool. Invalid values fail
// New/Open rather than being coerced to an unbounded database/sql setting.
func WithPoolConfig(config PoolConfig) Option {
	return func(options *storeOptions) { options.pool = config }
}

// WithCallbackPhaseTiming enables diagnostic-only submitted-callback timing.
// Ordinary stores do not install this option and therefore take no timing or
// output path. The Gateway wires it only behind an exact diagnosis marker.
func WithCallbackPhaseTiming(writer io.Writer) Option {
	return func(options *storeOptions) {
		if writer != nil {
			options.callbackPhaseTimingWriter = writer
		}
	}
}

// Open opens a PostgreSQL control database, applies embedded migrations, and
// recovers transactions that were in flight when the previous process exited.
func Open(ctx context.Context, dsn string, cipher ResultCipher, options ...Option) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, opErr("open", ErrInvalid, fmt.Errorf("empty PostgreSQL DSN"))
	}
	db, err := sql.Open("pgx", dsn)
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

// New initializes a store around an existing PostgreSQL *sql.DB. The store takes
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
	pool, err := normalizedPoolConfig(config.pool)
	if err != nil {
		return nil, opErr("new store", ErrInvalid, err)
	}
	store := &Store{db: db, cipher: cipher, clock: config.clock}
	if config.callbackPhaseTimingWriter != nil {
		store.callbackPhaseTiming = &callbackPhaseTimingRecorder{writer: config.callbackPhaseTimingWriter}
	}

	db.SetMaxOpenConns(pool.MaxOpenConns)
	db.SetMaxIdleConns(pool.MaxIdleConns)
	db.SetConnMaxLifetime(pool.ConnMaxLifetime)
	if err := db.PingContext(ctx); err != nil {
		return nil, opErr("ping", ErrConflict, err)
	}
	if err := store.migrate(ctx); err != nil {
		return nil, err
	}
	if config.recover {
		if _, err := store.recover(ctx, config.recoveryReceiptBuilder); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) DB() *sql.DB { return s.db }

// DBStats returns database/sql's non-sensitive pool telemetry. It is used by
// authenticated operational/evaluation endpoints to bind offered load to the
// actual service pool, never to expose a DSN or query text.
func (s *Store) DBStats() sql.DBStats {
	if s == nil || s.db == nil {
		return sql.DBStats{}
	}
	return s.db.Stats()
}

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

func (s *Store) now() time.Time { return dbTime(s.clock.Now()) }

func normalizedPoolConfig(config PoolConfig) (PoolConfig, error) {
	if config.MaxOpenConns == 0 {
		config.MaxOpenConns = defaultMaxOpenConns
	}
	if config.MaxIdleConns == 0 {
		config.MaxIdleConns = defaultMaxIdleConns
		if config.MaxIdleConns > config.MaxOpenConns {
			config.MaxIdleConns = config.MaxOpenConns
		}
	}
	if config.ConnMaxLifetime == 0 {
		config.ConnMaxLifetime = defaultConnMaxLifetime
	}
	if config.MaxOpenConns < 1 || config.MaxOpenConns > 4096 ||
		config.MaxIdleConns < 0 || config.MaxIdleConns > config.MaxOpenConns ||
		config.ConnMaxLifetime < time.Second {
		return PoolConfig{}, errors.New("invalid Control PostgreSQL pool configuration")
	}
	return config, nil
}

func dbTime(value time.Time) time.Time { return value.UTC().Truncate(time.Microsecond) }

func formatTime(value time.Time) string { return dbTime(value).Format(time.RFC3339Nano) }

func nullableTime(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return dbTime(*value)
}

func scanNullableTime(value sql.NullTime) *time.Time {
	if !value.Valid || value.Time.IsZero() {
		return nil
	}
	parsed := dbTime(value.Time)
	return &parsed
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

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func beginTx(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	return db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}

func rollback(tx *sql.Tx) {
	if tx != nil {
		_ = tx.Rollback()
	}
}
