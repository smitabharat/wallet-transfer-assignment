package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/smitabharat/wallet-transfer-assignment/internal/domain"
	"github.com/smitabharat/wallet-transfer-assignment/internal/repository/pgtest"
)

// These tests check that the schema itself protects invariants, independent
// of the service layer.

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seed(t *testing.T, s *Store) domain.Transfer {
	t.Helper()
	ctx, now := context.Background(), time.Now()
	r := s.Reader()
	for _, id := range []string{"a", "b"} {
		if err := r.InsertWallet(ctx, domain.Wallet{ID: id, Balance: 100, InitialBalance: 100, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	tr := domain.Transfer{ID: "t1", IdempotencyKey: "k1", FromWalletID: "a", ToWalletID: "b", Amount: 10,
		Status: domain.TransferPending, CreatedAt: now, UpdatedAt: now}
	if err := r.InsertTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestDebitIsRefusedWhenBalanceIsInsufficient(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	ok, err := s.Reader().DebitIfSufficient(context.Background(), "a", 101, time.Now())
	if err != nil || ok {
		t.Fatalf("want (false, nil), got (%v, %v)", ok, err)
	}
	w, _ := s.Reader().GetWallet(context.Background(), "a")
	if w.Balance != 100 {
		t.Errorf("balance changed to %d", w.Balance)
	}
}

func TestSchemaRejectsNegativeBalance(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	_, err := s.db.Exec(`UPDATE wallets SET balance = -1 WHERE id = 'a'`)
	if err == nil {
		t.Fatal("negative balance was accepted")
	}
}

func TestSchemaRejectsSecondDebitForSameTransfer(t *testing.T) {
	s := openTestStore(t)
	tr := seed(t, s)
	pair := domain.NewLedgerPair(tr, time.Now())
	if err := s.Reader().InsertLedgerEntries(context.Background(), pair[:]...); err != nil {
		t.Fatal(err)
	}
	if err := s.Reader().InsertLedgerEntries(context.Background(), pair[0]); err == nil {
		t.Fatal("duplicate debit entry was accepted")
	}
}

func TestSchemaRejectsLedgerEntryWithoutTransfer(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	err := s.Reader().InsertLedgerEntries(context.Background(), domain.LedgerEntry{
		TransferID: "missing", WalletID: "a", Type: domain.EntryDebit, Amount: 1, CreatedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("orphan ledger entry was accepted")
	}
}

func TestDuplicateIdempotencyKeyIsRejected(t *testing.T) {
	s := openTestStore(t)
	tr := seed(t, s)
	dup := tr
	dup.ID = "t2"
	if err := s.Reader().InsertTransfer(context.Background(), dup); !errors.Is(err, domain.ErrDuplicateKey) {
		t.Fatalf("want ErrDuplicateKey for transfer, got %v", err)
	}
	rec := domain.IdempotencyRecord{Key: "k1", RequestHash: "h", TransferID: tr.ID, Response: []byte("{}"), CreatedAt: time.Now()}
	if err := s.Reader().InsertIdempotencyRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if err := s.Reader().InsertIdempotencyRecord(context.Background(), rec); !errors.Is(err, domain.ErrDuplicateKey) {
		t.Fatalf("want ErrDuplicateKey for record, got %v", err)
	}
}

func TestStatusUpdateOnlyFromExpectedState(t *testing.T) {
	s := openTestStore(t)
	tr := seed(t, s)
	ctx := context.Background()
	tr.Status = domain.TransferProcessed
	if err := s.Reader().UpdateTransferStatus(ctx, tr, domain.TransferPending); err != nil {
		t.Fatal(err)
	}
	tr.Status = domain.TransferFailed
	err := s.Reader().UpdateTransferStatus(ctx, tr, domain.TransferPending)
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
	got, _ := s.Reader().GetTransfer(ctx, tr.ID)
	if got.Status != domain.TransferProcessed {
		t.Errorf("status = %s, want PROCESSED", got.Status)
	}
}

func TestWithTxRollsBackOnError(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	ctx := context.Background()
	boom := errors.New("boom")
	err := s.WithTx(ctx, func(r Repository) error {
		if _, err := r.DebitIfSufficient(ctx, "a", 50, time.Now()); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	w, _ := s.Reader().GetWallet(ctx, "a")
	if w.Balance != 100 {
		t.Errorf("rolled back debit left balance at %d", w.Balance)
	}
}
