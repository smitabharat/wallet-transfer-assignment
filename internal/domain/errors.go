package domain

import "errors"

// Sentinel errors shared across layers. Handlers map them to HTTP statuses.
var (
	ErrValidation          = errors.New("validation error")
	ErrWalletNotFound      = errors.New("wallet not found")
	ErrWalletExists        = errors.New("wallet already exists")
	ErrTransferNotFound    = errors.New("transfer not found")
	ErrInvalidTransition   = errors.New("invalid transfer state transition")
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")
	ErrInsufficientFunds   = errors.New("insufficient funds")
	ErrDuplicateKey        = errors.New("duplicate idempotency key")
	ErrBusy                = errors.New("storage busy, retry later")
)
