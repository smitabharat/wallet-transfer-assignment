// Package repository implements persistence on PostgreSQL. It performs reads
// and writes only; workflow decisions live in the service layer.
package repository

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/smitabharat/wallet-transfer-assignment/internal/domain"
)

//go:embed schema.sql
var schemaSQL string

// maxOpenConns caps the pool so bursts of requests queue in the application
// instead of exhausting the server's max_connections.
const maxOpenConns = 20

// PostgreSQL error codes that mean "contention, retry later".
const (
	codeUniqueViolation      = "23505"
	codeSerializationFailure = "40001"
	codeDeadlockDetected     = "40P01"
	codeLockNotAvailable     = "55P03"
)

// codeNumericValueOutOfRange is raised when a value does not fit the target
// column, including BIGINT overflow. domain.TransferRequest.Validate rejects
// amounts large enough to risk this before any query runs; this mapping is a
// backstop so a value that reaches the database some other way (a future
// caller, a balance that has grown very large over many transfers) fails as
// a client error instead of an unmapped 500.
const codeNumericValueOutOfRange = "22003"

// Transactor runs a function inside a single database transaction.
type Transactor interface {
	WithTx(ctx context.Context, fn func(Repository) error) error
	Reader() Repository
}

// Store owns the database handle.
type Store struct {
	db *sql.DB
}

var _ Transactor = (*Store)(nil)

// Open connects to the PostgreSQL database at dsn (a postgres:// URL or a
// key=value connection string) and applies the schema.
//
// Session settings, applied unless the DSN already sets them:
//   - lock_timeout: a transaction waiting on a row lock gives up after 10s
//     with an error mapped to domain.ErrBusy, instead of waiting forever.
//   - timezone=UTC: timestamps come back in UTC.
func Open(dsn string) (*Store, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	for k, v := range map[string]string{"lock_timeout": "10s", "timezone": "UTC"} {
		if _, ok := cfg.RuntimeParams[k]; !ok {
			cfg.RuntimeParams[k] = v
		}
	}

	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(maxOpenConns)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Reader returns a repository for reads outside of an explicit transaction.
func (s *Store) Reader() Repository { return &sqlRepository{q: s.db} }

// WithTx runs fn in one READ COMMITTED transaction: commit if fn returns nil,
// rollback otherwise. Callers serialize conflicting work with row locks
// (Repository.LockWallets) rather than a stricter isolation level.
func (s *Store) WithTx(ctx context.Context, fn func(Repository) error) (err error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return mapError(fmt.Errorf("begin tx: %w", err))
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(&sqlRepository{q: tx}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return mapError(fmt.Errorf("commit tx: %w", err))
	}
	return nil
}

// mapError converts driver errors that callers must react to into domain
// sentinel errors while keeping the original error in the chain.
func mapError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case codeSerializationFailure, codeDeadlockDetected, codeLockNotAvailable:
		return fmt.Errorf("%w: %w", domain.ErrBusy, err)
	case codeNumericValueOutOfRange:
		return fmt.Errorf("%w: amount out of range: %w", domain.ErrValidation, err)
	default:
		return err
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == codeUniqueViolation
}
