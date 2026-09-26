package service_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/smitabharat/wallet-transfer-assignment/internal/domain"
	"github.com/smitabharat/wallet-transfer-assignment/internal/repository"
	"github.com/smitabharat/wallet-transfer-assignment/internal/repository/pgtest"
	"github.com/smitabharat/wallet-transfer-assignment/internal/service"
)

type fixture struct {
	transfers *service.TransferService
	wallets   *service.WalletService
	dsn       string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dsn := pgtest.DSN(t)
	store, err := repository.Open(dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return fixture{
		transfers: service.NewTransferService(store, log),
		wallets:   service.NewWalletService(store),
		dsn:       dsn,
	}
}

func (f fixture) wallet(t *testing.T, id string, balance int64) {
	t.Helper()
	if _, err := f.wallets.CreateWallet(context.Background(), id, balance); err != nil {
		t.Fatalf("create wallet %s: %v", id, err)
	}
}

func (f fixture) balance(t *testing.T, id string) int64 {
	t.Helper()
	w, err := f.wallets.GetWallet(context.Background(), id)
	if err != nil {
		t.Fatalf("get wallet %s: %v", id, err)
	}
	return w.Balance
}

// assertLedgerInvariants checks, straight from the database, that
//   - every wallet balance == initial balance + credits - debits
//   - total debits == total credits
//   - every PROCESSED transfer has exactly one debit and one credit of its amount
//   - FAILED transfers have no ledger entries
func (f fixture) assertLedgerInvariants(t *testing.T) {
	t.Helper()
	db, err := sql.Open("pgx", f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var mismatched int
	err = db.QueryRow(`
		SELECT COUNT(*) FROM wallets w
		WHERE w.balance <> w.initial_balance
		  + COALESCE((SELECT SUM(amount) FROM ledger_entries WHERE wallet_id = w.id AND type = 'CREDIT'), 0)
		  - COALESCE((SELECT SUM(amount) FROM ledger_entries WHERE wallet_id = w.id AND type = 'DEBIT'), 0)`).
		Scan(&mismatched)
	if err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Errorf("%d wallet(s) have a balance that does not match the ledger", mismatched)
	}

	var debits, credits int64
	if err := db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN type = 'DEBIT' THEN amount END), 0)::BIGINT,
		COALESCE(SUM(CASE WHEN type = 'CREDIT' THEN amount END), 0)::BIGINT FROM ledger_entries`).Scan(&debits, &credits); err != nil {
		t.Fatal(err)
	}
	if debits != credits {
		t.Errorf("ledger does not balance: debits=%d credits=%d", debits, credits)
	}

	var badProcessed int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM transfers t WHERE t.status = 'PROCESSED' AND (
		  (SELECT COUNT(*) FROM ledger_entries e WHERE e.transfer_id = t.id AND e.type = 'DEBIT'
		     AND e.wallet_id = t.from_wallet_id AND e.amount = t.amount) <> 1 OR
		  (SELECT COUNT(*) FROM ledger_entries e WHERE e.transfer_id = t.id AND e.type = 'CREDIT'
		     AND e.wallet_id = t.to_wallet_id AND e.amount = t.amount) <> 1 OR
		  (SELECT COUNT(*) FROM ledger_entries e WHERE e.transfer_id = t.id) <> 2)`).Scan(&badProcessed); err != nil {
		t.Fatal(err)
	}
	if badProcessed != 0 {
		t.Errorf("%d processed transfer(s) without exactly one matching debit and credit", badProcessed)
	}

	var failedWithEntries int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM transfers t WHERE t.status <> 'PROCESSED'
		  AND EXISTS (SELECT 1 FROM ledger_entries e WHERE e.transfer_id = t.id)`).Scan(&failedWithEntries); err != nil {
		t.Fatal(err)
	}
	if failedWithEntries != 0 {
		t.Errorf("%d non-processed transfer(s) have ledger entries", failedWithEntries)
	}
}

func (f fixture) countTransfers(t *testing.T) int {
	t.Helper()
	db, err := sql.Open("pgx", f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM transfers`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTransferMovesMoneyAndWritesBalancedLedger(t *testing.T) {
	f := newFixture(t)
	f.wallet(t, "alice", 500)
	f.wallet(t, "bob", 100)

	res, err := f.transfers.CreateTransfer(context.Background(), domain.TransferRequest{
		IdempotencyKey: "k1", FromWalletID: "alice", ToWalletID: "bob", Amount: 200,
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if res.Transfer.Status != domain.TransferProcessed || res.Replayed {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := f.balance(t, "alice"); got != 300 {
		t.Errorf("alice balance = %d, want 300", got)
	}
	if got := f.balance(t, "bob"); got != 300 {
		t.Errorf("bob balance = %d, want 300", got)
	}

	details, err := f.transfers.GetTransfer(context.Background(), res.Transfer.ID)
	if err != nil {
		t.Fatalf("get transfer: %v", err)
	}
	if len(details.LedgerEntries) != 2 {
		t.Fatalf("want 2 ledger entries, got %d", len(details.LedgerEntries))
	}
	debit, credit := details.LedgerEntries[0], details.LedgerEntries[1]
	if debit.Type != domain.EntryDebit || debit.WalletID != "alice" || debit.Amount != 200 {
		t.Errorf("unexpected debit entry: %+v", debit)
	}
	if credit.Type != domain.EntryCredit || credit.WalletID != "bob" || credit.Amount != 200 {
		t.Errorf("unexpected credit entry: %+v", credit)
	}
	f.assertLedgerInvariants(t)
}

func TestInsufficientFundsFailsTransferWithoutSideEffects(t *testing.T) {
	f := newFixture(t)
	f.wallet(t, "alice", 50)
	f.wallet(t, "bob", 0)

	res, err := f.transfers.CreateTransfer(context.Background(), domain.TransferRequest{
		IdempotencyKey: "k1", FromWalletID: "alice", ToWalletID: "bob", Amount: 51,
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if res.Transfer.Status != domain.TransferFailed || res.Transfer.FailureReason != domain.FailureInsufficientFunds {
		t.Fatalf("want FAILED/INSUFFICIENT_FUNDS, got %+v", res.Transfer)
	}
	if f.balance(t, "alice") != 50 || f.balance(t, "bob") != 0 {
		t.Error("balances changed after failed transfer")
	}
	details, err := f.transfers.GetTransfer(context.Background(), res.Transfer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(details.LedgerEntries) != 0 {
		t.Errorf("failed transfer has %d ledger entries", len(details.LedgerEntries))
	}
	f.assertLedgerInvariants(t)
}

func TestTransferOfEntireBalanceSucceeds(t *testing.T) {
	f := newFixture(t)
	f.wallet(t, "alice", 100)
	f.wallet(t, "bob", 0)

	res, err := f.transfers.CreateTransfer(context.Background(), domain.TransferRequest{
		FromWalletID: "alice", ToWalletID: "bob", Amount: 100,
	})
	if err != nil || res.Transfer.Status != domain.TransferProcessed {
		t.Fatalf("got %+v, %v", res, err)
	}
	if f.balance(t, "alice") != 0 {
		t.Error("alice should have a zero balance")
	}
}

func TestDuplicateRequestReturnsOriginalResultWithoutSecondTransfer(t *testing.T) {
	f := newFixture(t)
	f.wallet(t, "alice", 500)
	f.wallet(t, "bob", 0)
	req := domain.TransferRequest{IdempotencyKey: "dup", FromWalletID: "alice", ToWalletID: "bob", Amount: 100}

	first, err := f.transfers.CreateTransfer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.transfers.CreateTransfer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if !second.Replayed {
		t.Error("second call should be marked as replayed")
	}
	if first.Transfer.ID != second.Transfer.ID || first.Transfer.Status != second.Transfer.Status ||
		!first.Transfer.CreatedAt.Equal(second.Transfer.CreatedAt) {
		t.Errorf("replay differs from original:\n first=%+v\nsecond=%+v", first.Transfer, second.Transfer)
	}
	if got := f.balance(t, "alice"); got != 400 {
		t.Errorf("alice balance = %d, want 400 (debited once)", got)
	}
	if n := f.countTransfers(t); n != 1 {
		t.Errorf("want 1 transfer row, got %d", n)
	}
	f.assertLedgerInvariants(t)
}

func TestReplayOfFailedTransferStaysFailedEvenAfterTopUp(t *testing.T) {
	f := newFixture(t)
	f.wallet(t, "alice", 10)
	f.wallet(t, "bob", 1000)
	req := domain.TransferRequest{IdempotencyKey: "retry-failed", FromWalletID: "alice", ToWalletID: "bob", Amount: 100}

	first, err := f.transfers.CreateTransfer(context.Background(), req)
	if err != nil || first.Transfer.Status != domain.TransferFailed {
		t.Fatalf("expected failed transfer, got %+v, %v", first, err)
	}
	// Alice receives funds; a retry with the same key must still report the
	// original failure instead of silently executing a new transfer.
	if _, err := f.transfers.CreateTransfer(context.Background(), domain.TransferRequest{
		IdempotencyKey: "top-up", FromWalletID: "bob", ToWalletID: "alice", Amount: 500,
	}); err != nil {
		t.Fatal(err)
	}
	retry, err := f.transfers.CreateTransfer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Replayed || retry.Transfer.Status != domain.TransferFailed || retry.Transfer.ID != first.Transfer.ID {
		t.Errorf("retry should replay original failure, got %+v", retry)
	}
	if got := f.balance(t, "alice"); got != 510 {
		t.Errorf("alice balance = %d, want 510", got)
	}
}

func TestReusingKeyWithDifferentPayloadIsRejected(t *testing.T) {
	f := newFixture(t)
	f.wallet(t, "alice", 500)
	f.wallet(t, "bob", 0)

	if _, err := f.transfers.CreateTransfer(context.Background(), domain.TransferRequest{
		IdempotencyKey: "same-key", FromWalletID: "alice", ToWalletID: "bob", Amount: 100,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.transfers.CreateTransfer(context.Background(), domain.TransferRequest{
		IdempotencyKey: "same-key", FromWalletID: "alice", ToWalletID: "bob", Amount: 999,
	})
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("want ErrIdempotencyConflict, got %v", err)
	}
	if got := f.balance(t, "alice"); got != 400 {
		t.Errorf("alice balance = %d, want 400", got)
	}
}

func TestRequestsWithoutKeyAreEachExecuted(t *testing.T) {
	f := newFixture(t)
	f.wallet(t, "alice", 500)
	f.wallet(t, "bob", 0)
	req := domain.TransferRequest{FromWalletID: "alice", ToWalletID: "bob", Amount: 100}
	for i := 0; i < 2; i++ {
		if _, err := f.transfers.CreateTransfer(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.balance(t, "alice"); got != 300 {
		t.Errorf("alice balance = %d, want 300", got)
	}
}

func TestRejectedRequestsPersistNothing(t *testing.T) {
	f := newFixture(t)
	f.wallet(t, "alice", 500)

	cases := map[string]struct {
		req  domain.TransferRequest
		want error
	}{
		"unknown destination": {domain.TransferRequest{IdempotencyKey: "a", FromWalletID: "alice", ToWalletID: "ghost", Amount: 1}, domain.ErrWalletNotFound},
		"unknown source":      {domain.TransferRequest{IdempotencyKey: "b", FromWalletID: "ghost", ToWalletID: "alice", Amount: 1}, domain.ErrWalletNotFound},
		"same wallet":         {domain.TransferRequest{IdempotencyKey: "c", FromWalletID: "alice", ToWalletID: "alice", Amount: 1}, domain.ErrValidation},
		"zero amount":         {domain.TransferRequest{IdempotencyKey: "d", FromWalletID: "alice", ToWalletID: "x", Amount: 0}, domain.ErrValidation},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.transfers.CreateTransfer(context.Background(), c.req); !errors.Is(err, c.want) {
				t.Fatalf("want %v, got %v", c.want, err)
			}
		})
	}
	if n := f.countTransfers(t); n != 0 {
		t.Errorf("rejected requests created %d transfer rows", n)
	}

	// A key used by a rejected request is not burned: once the wallet exists
	// the same key executes normally.
	f.wallet(t, "ghost", 0)
	res, err := f.transfers.CreateTransfer(context.Background(), cases["unknown destination"].req)
	if err != nil || res.Transfer.Status != domain.TransferProcessed || res.Replayed {
		t.Fatalf("retry after fixing input: %+v, %v", res, err)
	}
}

func TestConcurrentDebitsNeverOverdraw(t *testing.T) {
	f := newFixture(t)
	const (
		start    = 1000
		amount   = 30
		attempts = 100
	)
	f.wallet(t, "source", start)
	f.wallet(t, "sink-a", 0)
	f.wallet(t, "sink-b", 0)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		processed int
		failed    int
		errs      []error
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			to := "sink-a"
			if i%2 == 0 {
				to = "sink-b"
			}
			res, err := f.transfers.CreateTransfer(context.Background(), domain.TransferRequest{
				IdempotencyKey: fmt.Sprintf("c-%d", i), FromWalletID: "source", ToWalletID: to, Amount: amount,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, err)
			case res.Transfer.Status == domain.TransferProcessed:
				processed++
			default:
				failed++
			}
		}(i)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d transfers errored, first: %v", len(errs), errs[0])
	}
	wantProcessed := start / amount // 33
	if processed != wantProcessed || failed != attempts-wantProcessed {
		t.Errorf("processed=%d failed=%d, want %d/%d", processed, failed, wantProcessed, attempts-wantProcessed)
	}
	if got := f.balance(t, "source"); got != start-int64(wantProcessed*amount) {
		t.Errorf("source balance = %d, want %d", got, start-wantProcessed*amount)
	}
	total := f.balance(t, "source") + f.balance(t, "sink-a") + f.balance(t, "sink-b")
	if total != start {
		t.Errorf("money was created or destroyed: total=%d, want %d", total, start)
	}
	f.assertLedgerInvariants(t)
}

// Transfers in opposite directions lock the same two wallets; without a
// consistent lock order they would deadlock.
func TestOpposingConcurrentTransfersDoNotDeadlock(t *testing.T) {
	f := newFixture(t)
	f.wallet(t, "alice", 1000)
	f.wallet(t, "bob", 1000)

	const n = 50
	var (
		wg   sync.WaitGroup
		errs = make([]error, n)
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			from, to := "alice", "bob"
			if i%2 == 0 {
				from, to = to, from
			}
			_, errs[i] = f.transfers.CreateTransfer(context.Background(), domain.TransferRequest{
				IdempotencyKey: fmt.Sprintf("o-%d", i), FromWalletID: from, ToWalletID: to, Amount: 10,
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("transfer %d failed: %v", i, err)
		}
	}
	if total := f.balance(t, "alice") + f.balance(t, "bob"); total != 2000 {
		t.Errorf("money was created or destroyed: total=%d, want 2000", total)
	}
	f.assertLedgerInvariants(t)
}

func TestConcurrentDuplicatesExecuteExactlyOnce(t *testing.T) {
	f := newFixture(t)
	f.wallet(t, "alice", 1000)
	f.wallet(t, "bob", 0)
	req := domain.TransferRequest{IdempotencyKey: "burst", FromWalletID: "alice", ToWalletID: "bob", Amount: 100}

	const n = 50
	var (
		wg      sync.WaitGroup
		results = make([]service.TransferResult, n)
		errs    = make([]error, n)
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = f.transfers.CreateTransfer(context.Background(), req)
		}(i)
	}
	wg.Wait()

	executed := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d failed: %v", i, errs[i])
		}
		if !results[i].Replayed {
			executed++
		}
		if results[i].Transfer.ID != results[0].Transfer.ID {
			t.Fatalf("request %d got a different transfer id", i)
		}
	}
	if executed != 1 {
		t.Errorf("transfer executed %d times, want exactly 1", executed)
	}
	if got := f.balance(t, "alice"); got != 900 {
		t.Errorf("alice balance = %d, want 900", got)
	}
	if c := f.countTransfers(t); c != 1 {
		t.Errorf("want 1 transfer row, got %d", c)
	}
	f.assertLedgerInvariants(t)
}

func TestIdempotencySurvivesRestart(t *testing.T) {
	f := newFixture(t)
	f.wallet(t, "alice", 500)
	f.wallet(t, "bob", 0)
	req := domain.TransferRequest{IdempotencyKey: "durable", FromWalletID: "alice", ToWalletID: "bob", Amount: 100}
	first, err := f.transfers.CreateTransfer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a process restart: open a brand new store on the same database.
	store, err := repository.Open(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	restarted := service.NewTransferService(store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	again, err := restarted.CreateTransfer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Transfer.ID != first.Transfer.ID {
		t.Errorf("expected replay of %s after restart, got %+v", first.Transfer.ID, again)
	}
	if got := f.balance(t, "alice"); got != 400 {
		t.Errorf("alice balance = %d, want 400", got)
	}
}
