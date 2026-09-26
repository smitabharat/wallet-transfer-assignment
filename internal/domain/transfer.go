// Package domain holds the core entities, state transitions and validation
// rules of the wallet transfer service. It has no knowledge of HTTP or SQL.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// TransferStatus is the lifecycle state of a transfer.
type TransferStatus string

// Allowed transfer states.
const (
	TransferPending   TransferStatus = "PENDING"
	TransferProcessed TransferStatus = "PROCESSED"
	TransferFailed    TransferStatus = "FAILED"
)

// CanTransitionTo reports whether moving from s to next is a legal transition.
// Only PENDING -> PROCESSED and PENDING -> FAILED are allowed; terminal states
// never change.
func (s TransferStatus) CanTransitionTo(next TransferStatus) bool {
	return s == TransferPending && (next == TransferProcessed || next == TransferFailed)
}

// IsTerminal reports whether no further transitions are possible.
func (s TransferStatus) IsTerminal() bool {
	return s == TransferProcessed || s == TransferFailed
}

// Failure reasons recorded on FAILED transfers.
const (
	FailureInsufficientFunds = "INSUFFICIENT_FUNDS"
)

// Transfer is a request to move Amount from one wallet to another.
type Transfer struct {
	ID             string         `json:"id"`
	IdempotencyKey string         `json:"idempotencyKey,omitempty"`
	FromWalletID   string         `json:"fromWalletId"`
	ToWalletID     string         `json:"toWalletId"`
	Amount         int64          `json:"amount"`
	Status         TransferStatus `json:"status"`
	FailureReason  string         `json:"failureReason,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
}

// TransitionTo applies a state change, rejecting illegal transitions.
func (t *Transfer) TransitionTo(next TransferStatus, now time.Time) error {
	if !t.Status.CanTransitionTo(next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, t.Status, next)
	}
	t.Status = next
	t.UpdatedAt = now
	return nil
}

// TransferRequest is the validated input for creating a transfer.
type TransferRequest struct {
	IdempotencyKey string
	FromWalletID   string
	ToWalletID     string
	Amount         int64
}

// MaxIdempotencyKeyLength bounds the size of client supplied keys.
const MaxIdempotencyKeyLength = 255

// Validate checks the business rules that do not need the database.
func (r TransferRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.FromWalletID) == "":
		return fmt.Errorf("%w: fromWalletId is required", ErrValidation)
	case strings.TrimSpace(r.ToWalletID) == "":
		return fmt.Errorf("%w: toWalletId is required", ErrValidation)
	case r.FromWalletID == r.ToWalletID:
		return fmt.Errorf("%w: fromWalletId and toWalletId must differ", ErrValidation)
	case r.Amount <= 0:
		return fmt.Errorf("%w: amount must be a positive integer", ErrValidation)
	case len(r.IdempotencyKey) > MaxIdempotencyKeyLength:
		return fmt.Errorf("%w: idempotencyKey must be at most %d characters", ErrValidation, MaxIdempotencyKeyLength)
	}
	return nil
}

// Fingerprint returns a stable hash of the fields that define the transfer.
// It is stored with the idempotency key so that reusing a key with a different
// payload can be detected.
func (r TransferRequest) Fingerprint() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", r.FromWalletID, r.ToWalletID, r.Amount)))
	return hex.EncodeToString(sum[:])
}
