package domain

import (
	"errors"
	"testing"
	"time"
)

func TestTransferStatusTransitions(t *testing.T) {
	cases := []struct {
		from, to TransferStatus
		allowed  bool
	}{
		{TransferPending, TransferProcessed, true},
		{TransferPending, TransferFailed, true},
		{TransferPending, TransferPending, false},
		{TransferProcessed, TransferFailed, false},
		{TransferProcessed, TransferPending, false},
		{TransferFailed, TransferProcessed, false},
		{TransferFailed, TransferPending, false},
	}
	for _, c := range cases {
		if got := c.from.CanTransitionTo(c.to); got != c.allowed {
			t.Errorf("%s -> %s: got %v, want %v", c.from, c.to, got, c.allowed)
		}
	}
}

func TestTransitionToRejectsChangesToTerminalTransfer(t *testing.T) {
	tr := Transfer{Status: TransferPending}
	if err := tr.TransitionTo(TransferProcessed, time.Now()); err != nil {
		t.Fatalf("first transition: %v", err)
	}
	err := tr.TransitionTo(TransferFailed, time.Now())
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}
	if tr.Status != TransferProcessed {
		t.Fatalf("status changed to %s after rejected transition", tr.Status)
	}
}

func TestTransferRequestValidate(t *testing.T) {
	valid := TransferRequest{IdempotencyKey: "k", FromWalletID: "a", ToWalletID: "b", Amount: 1}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	invalid := map[string]TransferRequest{
		"missing from":  {FromWalletID: "", ToWalletID: "b", Amount: 1},
		"missing to":    {FromWalletID: "a", ToWalletID: "", Amount: 1},
		"same wallet":   {FromWalletID: "a", ToWalletID: "a", Amount: 1},
		"zero amount":   {FromWalletID: "a", ToWalletID: "b", Amount: 0},
		"negative":      {FromWalletID: "a", ToWalletID: "b", Amount: -5},
		"key too large": {IdempotencyKey: string(make([]byte, MaxIdempotencyKeyLength+1)), FromWalletID: "a", ToWalletID: "b", Amount: 1},
	}
	for name, req := range invalid {
		if err := req.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: expected ErrValidation, got %v", name, err)
		}
	}
}

func TestFingerprintDependsOnPayloadOnly(t *testing.T) {
	a := TransferRequest{IdempotencyKey: "k1", FromWalletID: "a", ToWalletID: "b", Amount: 10}
	b := TransferRequest{IdempotencyKey: "k2", FromWalletID: "a", ToWalletID: "b", Amount: 10}
	c := TransferRequest{IdempotencyKey: "k1", FromWalletID: "a", ToWalletID: "b", Amount: 11}
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("same payload produced different fingerprints")
	}
	if a.Fingerprint() == c.Fingerprint() {
		t.Error("different amounts produced the same fingerprint")
	}
}

func TestNewLedgerPairIsBalanced(t *testing.T) {
	tr := Transfer{ID: "t1", FromWalletID: "a", ToWalletID: "b", Amount: 42}
	pair := NewLedgerPair(tr, time.Now())
	if pair[0].Type != EntryDebit || pair[0].WalletID != "a" {
		t.Errorf("first entry should debit source wallet, got %+v", pair[0])
	}
	if pair[1].Type != EntryCredit || pair[1].WalletID != "b" {
		t.Errorf("second entry should credit destination wallet, got %+v", pair[1])
	}
	if pair[0].Amount != pair[1].Amount {
		t.Error("ledger pair is not balanced")
	}
}

func TestNewWalletValidation(t *testing.T) {
	if _, err := NewWallet("", 0, time.Now()); !errors.Is(err, ErrValidation) {
		t.Errorf("empty id: got %v", err)
	}
	if _, err := NewWallet("w", -1, time.Now()); !errors.Is(err, ErrValidation) {
		t.Errorf("negative balance: got %v", err)
	}
}

func TestFingerprintIsUnambiguousAboutFieldBoundaries(t *testing.T) {
	a := TransferRequest{FromWalletID: "a|b", ToWalletID: "c", Amount: 1}
	b := TransferRequest{FromWalletID: "a", ToWalletID: "b|c", Amount: 1}
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("fingerprint collided across a field boundary containing the delimiter")
	}
}
