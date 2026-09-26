package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/smitabharat/wallet-transfer-assignment/internal/domain"
)

// Repository is the persistence API used by the service layer. The same
// implementation works on a transaction or on the plain database handle.
type Repository interface {
	InsertWallet(ctx context.Context, w domain.Wallet) error
	GetWallet(ctx context.Context, id string) (domain.Wallet, error)
	// LockWallets takes a row lock (SELECT ... FOR UPDATE) on every given
	// wallet, in sorted id order so concurrent transfers cannot deadlock. It
	// fails with ErrWalletNotFound if a wallet does not exist. The locks are
	// held until the surrounding transaction ends.
	LockWallets(ctx context.Context, ids ...string) error
	// DebitIfSufficient subtracts amount only if the balance covers it and
	// reports whether the debit happened.
	DebitIfSufficient(ctx context.Context, walletID string, amount int64, now time.Time) (bool, error)
	Credit(ctx context.Context, walletID string, amount int64, now time.Time) error

	InsertTransfer(ctx context.Context, t domain.Transfer) error
	// UpdateTransferStatus moves a transfer from `from` to t.Status; it fails
	// with ErrInvalidTransition if the stored status is not `from`.
	UpdateTransferStatus(ctx context.Context, t domain.Transfer, from domain.TransferStatus) error
	GetTransfer(ctx context.Context, id string) (domain.Transfer, error)

	InsertLedgerEntries(ctx context.Context, entries ...domain.LedgerEntry) error
	ListLedgerEntriesByTransfer(ctx context.Context, transferID string) ([]domain.LedgerEntry, error)

	// GetIdempotencyRecord returns (nil, nil) when the key is unknown.
	GetIdempotencyRecord(ctx context.Context, key string) (*domain.IdempotencyRecord, error)
	InsertIdempotencyRecord(ctx context.Context, rec domain.IdempotencyRecord) error
}

type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type sqlRepository struct {
	q queryer
}

// ---- wallets ----

func (r *sqlRepository) InsertWallet(ctx context.Context, w domain.Wallet) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO wallets (id, balance, initial_balance, created_at, updated_at) VALUES ($1, $2, $3, $4, $5)`,
		w.ID, w.Balance, w.InitialBalance, w.CreatedAt.UTC(), w.UpdatedAt.UTC())
	if isUniqueViolation(err) {
		return domain.ErrWalletExists
	}
	return wrap("insert wallet", err)
}

func (r *sqlRepository) GetWallet(ctx context.Context, id string) (domain.Wallet, error) {
	var w domain.Wallet
	err := r.q.QueryRowContext(ctx,
		`SELECT id, balance, initial_balance, created_at, updated_at FROM wallets WHERE id = $1`, id).
		Scan(&w.ID, &w.Balance, &w.InitialBalance, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Wallet{}, fmt.Errorf("%w: %s", domain.ErrWalletNotFound, id)
	}
	if err != nil {
		return domain.Wallet{}, wrap("get wallet", err)
	}
	w.CreatedAt, w.UpdatedAt = w.CreatedAt.UTC(), w.UpdatedAt.UTC()
	return w, nil
}

func (r *sqlRepository) LockWallets(ctx context.Context, ids ...string) error {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	for _, id := range slices.Compact(sorted) {
		var got string
		err := r.q.QueryRowContext(ctx, `SELECT id FROM wallets WHERE id = $1 FOR UPDATE`, id).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", domain.ErrWalletNotFound, id)
		}
		if err != nil {
			return wrap("lock wallet", err)
		}
	}
	return nil
}

func (r *sqlRepository) DebitIfSufficient(ctx context.Context, walletID string, amount int64, now time.Time) (bool, error) {
	res, err := r.q.ExecContext(ctx,
		`UPDATE wallets SET balance = balance - $1, updated_at = $2 WHERE id = $3 AND balance >= $1`,
		amount, now.UTC(), walletID)
	if err != nil {
		return false, wrap("debit wallet", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, wrap("debit wallet rows", err)
	}
	return n == 1, nil
}

func (r *sqlRepository) Credit(ctx context.Context, walletID string, amount int64, now time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE wallets SET balance = balance + $1, updated_at = $2 WHERE id = $3`,
		amount, now.UTC(), walletID)
	if err != nil {
		return wrap("credit wallet", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return wrap("credit wallet rows", err)
	} else if n != 1 {
		return fmt.Errorf("%w: %s", domain.ErrWalletNotFound, walletID)
	}
	return nil
}

// ---- transfers ----

func (r *sqlRepository) InsertTransfer(ctx context.Context, t domain.Transfer) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO transfers (id, idempotency_key, from_wallet_id, to_wallet_id, amount, status, failure_reason, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		t.ID, nullString(t.IdempotencyKey), t.FromWalletID, t.ToWalletID, t.Amount, string(t.Status),
		nullString(t.FailureReason), t.CreatedAt.UTC(), t.UpdatedAt.UTC())
	if isUniqueViolation(err) {
		return domain.ErrDuplicateKey
	}
	return wrap("insert transfer", err)
}

func (r *sqlRepository) UpdateTransferStatus(ctx context.Context, t domain.Transfer, from domain.TransferStatus) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE transfers SET status = $1, failure_reason = $2, updated_at = $3 WHERE id = $4 AND status = $5`,
		string(t.Status), nullString(t.FailureReason), t.UpdatedAt.UTC(), t.ID, string(from))
	if err != nil {
		return wrap("update transfer status", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return wrap("update transfer status rows", err)
	} else if n != 1 {
		return fmt.Errorf("%w: transfer %s is not %s", domain.ErrInvalidTransition, t.ID, from)
	}
	return nil
}

func (r *sqlRepository) GetTransfer(ctx context.Context, id string) (domain.Transfer, error) {
	var (
		t           domain.Transfer
		key, reason sql.NullString
		status      string
	)
	err := r.q.QueryRowContext(ctx,
		`SELECT id, idempotency_key, from_wallet_id, to_wallet_id, amount, status, failure_reason, created_at, updated_at
		 FROM transfers WHERE id = $1`, id).
		Scan(&t.ID, &key, &t.FromWalletID, &t.ToWalletID, &t.Amount, &status, &reason, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Transfer{}, fmt.Errorf("%w: %s", domain.ErrTransferNotFound, id)
	}
	if err != nil {
		return domain.Transfer{}, wrap("get transfer", err)
	}
	t.IdempotencyKey, t.FailureReason, t.Status = key.String, reason.String, domain.TransferStatus(status)
	t.CreatedAt, t.UpdatedAt = t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	return t, nil
}

// ---- ledger ----

func (r *sqlRepository) InsertLedgerEntries(ctx context.Context, entries ...domain.LedgerEntry) error {
	for _, e := range entries {
		_, err := r.q.ExecContext(ctx,
			`INSERT INTO ledger_entries (transfer_id, wallet_id, type, amount, created_at) VALUES ($1, $2, $3, $4, $5)`,
			e.TransferID, e.WalletID, string(e.Type), e.Amount, e.CreatedAt.UTC())
		if err != nil {
			return wrap("insert ledger entry", err)
		}
	}
	return nil
}

func (r *sqlRepository) ListLedgerEntriesByTransfer(ctx context.Context, transferID string) ([]domain.LedgerEntry, error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT id, transfer_id, wallet_id, type, amount, created_at FROM ledger_entries WHERE transfer_id = $1 ORDER BY id`,
		transferID)
	if err != nil {
		return nil, wrap("list ledger entries", err)
	}
	defer func() { _ = rows.Close() }()

	entries := []domain.LedgerEntry{}
	for rows.Next() {
		var (
			e   domain.LedgerEntry
			typ string
		)
		if err := rows.Scan(&e.ID, &e.TransferID, &e.WalletID, &typ, &e.Amount, &e.CreatedAt); err != nil {
			return nil, wrap("scan ledger entry", err)
		}
		e.Type = domain.EntryType(typ)
		e.CreatedAt = e.CreatedAt.UTC()
		entries = append(entries, e)
	}
	return entries, wrap("iterate ledger entries", rows.Err())
}

// ---- idempotency ----

func (r *sqlRepository) GetIdempotencyRecord(ctx context.Context, key string) (*domain.IdempotencyRecord, error) {
	var (
		rec  domain.IdempotencyRecord
		body string
	)
	err := r.q.QueryRowContext(ctx,
		`SELECT key, request_hash, transfer_id, response_body, created_at FROM idempotency_records WHERE key = $1`, key).
		Scan(&rec.Key, &rec.RequestHash, &rec.TransferID, &body, &rec.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, wrap("get idempotency record", err)
	}
	rec.Response = []byte(body)
	rec.CreatedAt = rec.CreatedAt.UTC()
	return &rec, nil
}

func (r *sqlRepository) InsertIdempotencyRecord(ctx context.Context, rec domain.IdempotencyRecord) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO idempotency_records (key, request_hash, transfer_id, response_body, created_at) VALUES ($1, $2, $3, $4, $5)`,
		rec.Key, rec.RequestHash, rec.TransferID, string(rec.Response), rec.CreatedAt.UTC())
	if isUniqueViolation(err) {
		return domain.ErrDuplicateKey
	}
	return wrap("insert idempotency record", err)
}

// ---- helpers ----

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return mapError(fmt.Errorf("%s: %w", op, err))
}
