// Package service contains the business workflows: transfer orchestration,
// idempotency and state transitions.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/smitabharat/wallet-transfer-assignment/internal/domain"
	"github.com/smitabharat/wallet-transfer-assignment/internal/repository"
)

// TransferResult is the outcome of CreateTransfer.
type TransferResult struct {
	Transfer domain.Transfer
	// Replayed is true when the result was served from a stored idempotency
	// record instead of executing a new transfer.
	Replayed bool
}

// TransferDetails is a transfer together with its ledger entries.
type TransferDetails struct {
	Transfer      domain.Transfer      `json:"transfer"`
	LedgerEntries []domain.LedgerEntry `json:"ledgerEntries"`
}

// TransferService executes wallet transfers.
type TransferService struct {
	store repository.Transactor
	log   *slog.Logger
	now   func() time.Time
	newID func() string
}

// Option customises a TransferService (used by tests).
type Option func(*TransferService)

// WithClock overrides the time source.
func WithClock(now func() time.Time) Option { return func(s *TransferService) { s.now = now } }

// NewTransferService builds the service.
func NewTransferService(store repository.Transactor, log *slog.Logger, opts ...Option) *TransferService {
	s := &TransferService{store: store, log: log, now: utcNow, newID: newTransferID}
	for _, o := range opts {
		o(s)
	}
	return s
}

// CreateTransfer moves money between wallets exactly once per idempotency key.
//
// The whole workflow runs in one transaction that row-locks both wallets
// before touching them, so transfers on the same wallets are serialized and
// the balance check cannot race. Concurrent requests with the same key are
// serialized by the unique index on transfers.idempotency_key.
// Business outcomes (PROCESSED, or FAILED for insufficient funds) are stored
// with the key and replayed on retries; validation and infrastructure errors
// are not stored because nothing was persisted.
func (s *TransferService) CreateTransfer(ctx context.Context, req domain.TransferRequest) (TransferResult, error) {
	start := s.now()
	if err := req.Validate(); err != nil {
		return TransferResult{}, err
	}

	var result TransferResult
	err := s.store.WithTx(ctx, func(repo repository.Repository) error {
		var err error
		result, err = s.executeOnce(ctx, repo, req)
		return err
	})

	// Backstop: another writer committed the same key between our lookup and
	// insert. The unique constraint rejected our attempt; replay the winner.
	if errors.Is(err, domain.ErrDuplicateKey) && req.IdempotencyKey != "" {
		result, err = s.replayFromStore(ctx, req)
	}

	s.logOutcome(req, result, err, start)
	return result, err
}

func (s *TransferService) executeOnce(ctx context.Context, repo repository.Repository, req domain.TransferRequest) (TransferResult, error) {
	if req.IdempotencyKey != "" {
		rec, err := repo.GetIdempotencyRecord(ctx, req.IdempotencyKey)
		if err != nil {
			return TransferResult{}, err
		}
		if rec != nil {
			return replay(*rec, req)
		}
	}

	// Also checks that both wallets exist.
	if err := repo.LockWallets(ctx, req.FromWalletID, req.ToWalletID); err != nil {
		return TransferResult{}, err
	}

	now := s.now()
	t := domain.Transfer{
		ID:             s.newID(),
		IdempotencyKey: req.IdempotencyKey,
		FromWalletID:   req.FromWalletID,
		ToWalletID:     req.ToWalletID,
		Amount:         req.Amount,
		Status:         domain.TransferPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := repo.InsertTransfer(ctx, t); err != nil {
		return TransferResult{}, err
	}

	debited, err := repo.DebitIfSufficient(ctx, t.FromWalletID, t.Amount, now)
	if err != nil {
		return TransferResult{}, err
	}

	if debited {
		if err := repo.Credit(ctx, t.ToWalletID, t.Amount, now); err != nil {
			return TransferResult{}, err
		}
		pair := domain.NewLedgerPair(t, now)
		if err := repo.InsertLedgerEntries(ctx, pair[:]...); err != nil {
			return TransferResult{}, err
		}
		if err := t.TransitionTo(domain.TransferProcessed, now); err != nil {
			return TransferResult{}, err
		}
	} else {
		if err := t.TransitionTo(domain.TransferFailed, now); err != nil {
			return TransferResult{}, err
		}
		t.FailureReason = domain.FailureInsufficientFunds
	}
	if err := repo.UpdateTransferStatus(ctx, t, domain.TransferPending); err != nil {
		return TransferResult{}, err
	}

	if req.IdempotencyKey != "" {
		snapshot, err := json.Marshal(t)
		if err != nil {
			return TransferResult{}, fmt.Errorf("encode idempotent response: %w", err)
		}
		rec := domain.IdempotencyRecord{
			Key:         req.IdempotencyKey,
			RequestHash: req.Fingerprint(),
			TransferID:  t.ID,
			Response:    snapshot,
			CreatedAt:   now,
		}
		if err := repo.InsertIdempotencyRecord(ctx, rec); err != nil {
			return TransferResult{}, err
		}
	}
	return TransferResult{Transfer: t}, nil
}

func (s *TransferService) replayFromStore(ctx context.Context, req domain.TransferRequest) (TransferResult, error) {
	rec, err := s.store.Reader().GetIdempotencyRecord(ctx, req.IdempotencyKey)
	if err != nil {
		return TransferResult{}, err
	}
	if rec == nil {
		return TransferResult{}, fmt.Errorf("idempotency record for key %q vanished", req.IdempotencyKey)
	}
	return replay(*rec, req)
}

func replay(rec domain.IdempotencyRecord, req domain.TransferRequest) (TransferResult, error) {
	if !rec.Matches(req) {
		return TransferResult{}, domain.ErrIdempotencyConflict
	}
	var t domain.Transfer
	if err := json.Unmarshal(rec.Response, &t); err != nil {
		return TransferResult{}, fmt.Errorf("decode idempotent response: %w", err)
	}
	return TransferResult{Transfer: t, Replayed: true}, nil
}

// GetTransfer returns a transfer and its ledger entries.
func (s *TransferService) GetTransfer(ctx context.Context, id string) (TransferDetails, error) {
	repo := s.store.Reader()
	t, err := repo.GetTransfer(ctx, id)
	if err != nil {
		return TransferDetails{}, err
	}
	entries, err := repo.ListLedgerEntriesByTransfer(ctx, id)
	if err != nil {
		return TransferDetails{}, err
	}
	return TransferDetails{Transfer: t, LedgerEntries: entries}, nil
}

func (s *TransferService) logOutcome(req domain.TransferRequest, res TransferResult, err error, start time.Time) {
	attrs := []any{
		slog.String("idempotency_key", req.IdempotencyKey),
		slog.String("from_wallet_id", req.FromWalletID),
		slog.String("to_wallet_id", req.ToWalletID),
		slog.Int64("amount", req.Amount),
		slog.Duration("duration", s.now().Sub(start)),
	}
	if err != nil {
		s.log.Warn("transfer rejected", append(attrs, slog.String("error", err.Error()))...)
		return
	}
	s.log.Info("transfer completed", append(attrs,
		slog.String("transfer_id", res.Transfer.ID),
		slog.String("status", string(res.Transfer.Status)),
		slog.Bool("replayed", res.Replayed))...)
}

func newTransferID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return "tr_" + hex.EncodeToString(b)
}
