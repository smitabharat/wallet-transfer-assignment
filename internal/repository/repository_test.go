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
	ctx := context.Background()
	tr := seed(t, s)
	pair := domain.NewLedgerPair(tr, time.Now())

	// The UNIQUE(transfer_id, type) constraint checks immediately (it is not
	// deferred), so the duplicate insert fails right away regardless of what
	// transaction it runs in. Complete a correct pair and mark the transfer
	// PROCESSED first so this test isolates that one constraint rather than
	// also tripping the (deferred, commit-time) ledger-pair trigger.
	err := s.WithTx(ctx, func(r Repository) error {
		if err := r.InsertLedgerEntries(ctx, pair[:]...); err != nil {
			return err
		}
		if err := markProcessed(ctx, t, r, tr); err != nil {
			return err
		}
		if err := r.InsertLedgerEntries(ctx, pair[0]); err == nil {
			t.Fatal("duplicate debit entry was accepted")
		}
		return errors.New("rollback: test does not intend to commit")
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
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

	// A PROCESSED transfer needs its balanced ledger pair to satisfy the
	// commit-time trigger; write it in the same transaction as the first,
	// legal transition.
	err := s.WithTx(ctx, func(r Repository) error {
		pair := domain.NewLedgerPair(tr, time.Now())
		if err := r.InsertLedgerEntries(ctx, pair[:]...); err != nil {
			return err
		}
		tr.Status = domain.TransferProcessed
		return r.UpdateTransferStatus(ctx, tr, domain.TransferPending)
	})
	if err != nil {
		t.Fatal(err)
	}

	tr.Status = domain.TransferFailed
	err = s.Reader().UpdateTransferStatus(ctx, tr, domain.TransferPending)
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
	got, _ := s.Reader().GetTransfer(ctx, tr.ID)
	if got.Status != domain.TransferProcessed {
		t.Errorf("status = %s, want PROCESSED", got.Status)
	}
}

// markProcessed is a test-only helper: it moves tr straight from PENDING to
// PROCESSED without writing (or checking) any ledger entries, so tests can
// probe what the constraint trigger does or does not allow independently of
// the service layer's own ordering.
func markProcessed(ctx context.Context, t *testing.T, r Repository, tr domain.Transfer) error {
	t.Helper()
	tr.Status = domain.TransferProcessed
	return r.UpdateTransferStatus(ctx, tr, domain.TransferPending)
}

func TestLedgerPairTriggerRejectsProcessedTransferWithOnlyOneEntry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tr := seed(t, s)

	err := s.WithTx(ctx, func(r Repository) error {
		if err := r.InsertLedgerEntries(ctx, domain.LedgerEntry{
			TransferID: tr.ID, WalletID: tr.FromWalletID, Type: domain.EntryDebit, Amount: tr.Amount, CreatedAt: time.Now(),
		}); err != nil {
			return err
		}
		return markProcessed(ctx, t, r, tr)
	})
	if err == nil {
		t.Fatal("commit succeeded with a PROCESSED transfer that has only a DEBIT entry, want an error")
	}
}

func TestLedgerPairTriggerRejectsEntryForWrongWalletOrAmount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tr := seed(t, s)
	pair := domain.NewLedgerPair(tr, time.Now())
	pair[1].Amount = tr.Amount + 1 // credit amount does not match the transfer

	err := s.WithTx(ctx, func(r Repository) error {
		if err := r.InsertLedgerEntries(ctx, pair[:]...); err != nil {
			return err
		}
		return markProcessed(ctx, t, r, tr)
	})
	if err == nil {
		t.Fatal("commit succeeded with a credit amount that does not match the transfer, want an error")
	}
}

func TestLedgerPairTriggerRejectsEntriesOnAPendingTransfer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tr := seed(t, s)
	pair := domain.NewLedgerPair(tr, time.Now())

	// tr is left PENDING: a balanced pair is still not allowed before the
	// transfer is actually PROCESSED.
	err := s.WithTx(ctx, func(r Repository) error {
		return r.InsertLedgerEntries(ctx, pair[:]...)
	})
	if err == nil {
		t.Fatal("commit succeeded with ledger entries on a PENDING transfer, want an error")
	}
}

func TestLedgerPairTriggerAllowsACorrectPair(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tr := seed(t, s)
	pair := domain.NewLedgerPair(tr, time.Now())

	err := s.WithTx(ctx, func(r Repository) error {
		if err := r.InsertLedgerEntries(ctx, pair[:]...); err != nil {
			return err
		}
		return markProcessed(ctx, t, r, tr)
	})
	if err != nil {
		t.Fatalf("a correct debit/credit pair should be accepted: %v", err)
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
