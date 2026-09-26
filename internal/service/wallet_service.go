package service

import (
	"context"
	"time"

	"github.com/smitabharat/wallet-transfer-assignment/internal/domain"
	"github.com/smitabharat/wallet-transfer-assignment/internal/repository"
)

// WalletService manages wallets.
type WalletService struct {
	store repository.Transactor
	now   func() time.Time
}

// NewWalletService builds the service.
func NewWalletService(store repository.Transactor) *WalletService {
	return &WalletService{store: store, now: utcNow}
}

// CreateWallet creates a wallet with an opening balance.
func (s *WalletService) CreateWallet(ctx context.Context, id string, initialBalance int64) (domain.Wallet, error) {
	w, err := domain.NewWallet(id, initialBalance, s.now())
	if err != nil {
		return domain.Wallet{}, err
	}
	if err := s.store.Reader().InsertWallet(ctx, w); err != nil {
		return domain.Wallet{}, err
	}
	return w, nil
}

// GetWallet returns a wallet with its current balance.
func (s *WalletService) GetWallet(ctx context.Context, id string) (domain.Wallet, error) {
	return s.store.Reader().GetWallet(ctx, id)
}

// utcNow keeps every timestamp the service produces in UTC, truncated to the
// microsecond precision PostgreSQL stores, so values read back compare equal.
func utcNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }
