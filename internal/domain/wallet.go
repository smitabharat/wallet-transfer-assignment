package domain

import (
	"fmt"
	"strings"
	"time"
)

// Wallet holds a balance in the smallest currency unit.
type Wallet struct {
	ID             string    `json:"id"`
	Balance        int64     `json:"balance"`
	InitialBalance int64     `json:"initialBalance"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// NewWallet validates input and builds a wallet.
func NewWallet(id string, initialBalance int64, now time.Time) (Wallet, error) {
	if strings.TrimSpace(id) == "" {
		return Wallet{}, fmt.Errorf("%w: id is required", ErrValidation)
	}
	if initialBalance < 0 {
		return Wallet{}, fmt.Errorf("%w: initialBalance must not be negative", ErrValidation)
	}
	return Wallet{ID: id, Balance: initialBalance, InitialBalance: initialBalance, CreatedAt: now, UpdatedAt: now}, nil
}

// EntryType is the side of a double-entry ledger line.
type EntryType string

// Ledger entry types.
const (
	EntryDebit  EntryType = "DEBIT"
	EntryCredit EntryType = "CREDIT"
)

// LedgerEntry is one side of a transfer in the double-entry ledger.
type LedgerEntry struct {
	ID         int64     `json:"id"`
	WalletID   string    `json:"walletId"`
	TransferID string    `json:"transferId"`
	Type       EntryType `json:"type"`
	Amount     int64     `json:"amount"`
	CreatedAt  time.Time `json:"createdAt"`
}

// NewLedgerPair builds the balanced DEBIT/CREDIT pair for a transfer.
func NewLedgerPair(t Transfer, now time.Time) [2]LedgerEntry {
	return [2]LedgerEntry{
		{WalletID: t.FromWalletID, TransferID: t.ID, Type: EntryDebit, Amount: t.Amount, CreatedAt: now},
		{WalletID: t.ToWalletID, TransferID: t.ID, Type: EntryCredit, Amount: t.Amount, CreatedAt: now},
	}
}
