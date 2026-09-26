# Wallet Transfer Service — Design Note

This note was written before the implementation (documentation-first workflow)
and kept in sync with the code.

## 1. Problem statement

Move money between two wallets such that:

- a request carrying the same `idempotencyKey` is executed **at most once** and
  every retry receives the **original result** (exactly-once at the API level);
- every successful transfer writes **exactly two** ledger entries (one DEBIT, one
  CREDIT) of the same amount, so the ledger always balances;
- wallet balances stay correct and never go negative, even when many transfers
  debit the same wallet concurrently;
- a transfer moves through a small, guarded state machine:
  `PENDING -> PROCESSED` or `PENDING -> FAILED`.

## 2. API contract

All amounts are integers in the smallest currency unit (e.g. paise/cents).
Floating point is never used for money.

### `POST /wallets`

```json
{ "id": "wallet_1", "initialBalance": 1000 }
```

`201` with the wallet, `409` if the id exists, `400` on invalid input.
Convenience endpoint so the service can be exercised end-to-end.

### `GET /wallets/{id}`

`200` with `{ "id", "balance", "createdAt" }`, `404` if unknown.

### `POST /transfers`

```json
{
  "idempotencyKey": "abc123",
  "fromWalletId": "wallet_1",
  "toWalletId": "wallet_2",
  "amount": 100
}
```

The key may also be sent as the `Idempotency-Key` header; the body wins if both
are present and they differ → `400`.

| Outcome | Status | Body |
|---|---|---|
| Transfer executed | `201` | transfer, `status = PROCESSED` |
| Insufficient funds | `422` | transfer, `status = FAILED`, `failureReason` |
| Replay of a stored key (same payload) | original status | original body, header `Idempotent-Replayed: true` |
| Key reused with a different payload | `409` | error |
| Unknown wallet | `404` | error (nothing persisted) |
| Invalid input (amount ≤ 0, same wallet, missing ids) | `400` | error (nothing persisted) |

### `GET /transfers/{id}`

Returns the transfer and its ledger entries.

## 3. Schema

```
wallets(id PK, balance CHECK >= 0, initial_balance CHECK >= 0, created_at, updated_at)

transfers(id PK, idempotency_key UNIQUE NULL, from_wallet_id FK, to_wallet_id FK,
          amount CHECK > 0, status CHECK IN (PENDING, PROCESSED, FAILED),
          failure_reason, created_at, updated_at,
          CHECK from_wallet_id <> to_wallet_id)

ledger_entries(id PK AUTOINCREMENT, transfer_id FK, wallet_id FK,
               type CHECK IN (DEBIT, CREDIT), amount CHECK > 0, created_at,
               UNIQUE(transfer_id, type))

idempotency_records(key PK, request_hash, transfer_id FK, response_body, created_at)
```

Indexes: `ledger_entries(wallet_id, id)` for wallet history,
`transfers(from_wallet_id)`, `transfers(to_wallet_id)`.

Why these constraints:

- `CHECK (balance >= 0)` — the database, not only application code, forbids
  overdrafts. It is the last line of defence against double spending.
- `UNIQUE(transfer_id, type)` — a transfer can never get a second DEBIT or a
  second CREDIT, even through a bug or a replay.
- `ledger_entries.transfer_id` is a FK — ledger rows cannot exist without a
  transfer.
- `idempotency_records.key` is the primary key and `transfers.idempotency_key`
  is unique — two concurrent requests with the same key cannot both commit.

**Balance model:** a stored balance is updated inside the same transaction that
writes the ledger. The invariant, verified in tests, is

```
wallets.balance == initial_balance + SUM(CREDIT) - SUM(DEBIT)
SUM(all DEBIT amounts) == SUM(all CREDIT amounts)
```

A stored balance keeps reads O(1) and lets the DB enforce `balance >= 0`
atomically; the ledger remains the audit trail that can recompute it.

## 4. Transfer workflow (one DB transaction)

```
BEGIN (READ COMMITTED)
  record := idempotency_records[key]
  if record exists:
      if record.request_hash != hash(request) -> 409 (rollback)
      else return record.response            -- replay, no side effects
  SELECT ... FOR UPDATE both wallets,
     in sorted id order                     -> 404 if missing (rollback)
  INSERT transfer (PENDING)
  UPDATE wallets SET balance = balance - amt
     WHERE id = from AND balance >= amt     -- conditional debit
  if 0 rows affected:
      UPDATE transfer SET status = FAILED WHERE status = PENDING
  else:
      UPDATE wallets SET balance = balance + amt WHERE id = to
      INSERT ledger DEBIT, INSERT ledger CREDIT
      UPDATE transfer SET status = PROCESSED WHERE status = PENDING
  INSERT idempotency_record(key, hash, transfer_id, transfer snapshot)
COMMIT
```

Everything — transfer row, balance changes, ledger entries and the stored
response — commits or rolls back together. There is no window in which money
has moved but the idempotency record is missing, or vice versa.

## 5. Idempotency

- **Storage:** `idempotency_records` holds the key, a SHA-256 fingerprint of the
  canonical request (`from|to|amount`), the transfer id and a JSON snapshot of
  the transfer returned the first time. The HTTP status is a pure function of
  the transfer status (`PROCESSED` → 201, `FAILED` → 422), so the service layer
  stays transport-agnostic while replays still get the same status and body.
  The record is durable, so replays work across process restarts.
- **Detection:** lookup by primary key inside the write transaction; the unique
  constraint is a backstop if two writers ever race (on a unique violation the
  service re-reads the winner's record and replays it).
- **Original result:** the stored snapshot is returned (identical body and
  status) with the header `Idempotent-Replayed: true`.
- **Payload mismatch:** same key, different payload → `409`; we never silently
  return a result for a request that the client did not actually send.
- **Which outcomes are stored:** business outcomes (`PROCESSED`, and `FAILED`
  for insufficient funds) are stored and replayed — a retry of a failed transfer
  returns the same failure rather than unexpectedly succeeding later.
  Validation errors, unknown wallets and infrastructure errors are **not**
  stored: nothing was persisted, so a retry is safe and may succeed.
- **Response lost after commit:** the client retries with the same key and gets
  the stored result — no second transfer.
- **No key supplied:** the transfer runs normally with no replay protection
  (the API promises exactly-once only when a key is provided).

## 6. Concurrency strategy

PostgreSQL at `READ COMMITTED`, with explicit row locks:

- **Wallet locks.** After the idempotency lookup, the transaction runs
  `SELECT ... FOR UPDATE` on both wallet rows. Transfers touching the same
  wallet are serialized; transfers on unrelated wallets run in parallel.
- **Deadlock avoidance.** Locks are always taken in sorted wallet-id order, so
  `A -> B` and `B -> A` running at once cannot deadlock (covered by
  `TestOpposingConcurrentTransfersDoNotDeadlock`).
- **Same key, concurrent requests.** Both may miss the idempotency lookup. The
  unique index on `transfers.idempotency_key` then makes the second insert wait
  for the first transaction and fail once it commits. The service catches that
  (`ErrDuplicateKey`) and replays the committed record, so the transfer runs
  exactly once.
- **Timeouts.** `lock_timeout = 10s` bounds lock waits. Lock timeouts,
  deadlocks and serialization failures map to `ErrBusy` (`503`, safe to retry).

Independently of the lock, the debit is a **conditional update**
(`balance >= amount` in the `WHERE` clause) and the table has
`CHECK (balance >= 0)`. So correctness does not rely on application-level
read-then-check logic.

Choosing `SERIALIZABLE` instead would mean retrying on every `40001`; row
locks give the same safety with no application-level retry loop.

## 7. State machine

`PENDING -> PROCESSED`, `PENDING -> FAILED`; terminal states cannot change.
The rule lives in `domain.TransferStatus.CanTransitionTo` and is enforced in SQL
with `UPDATE ... WHERE status = 'PENDING'`; zero affected rows is treated as an
illegal transition and aborts the transaction. In this synchronous design the
PENDING row is never visible outside the transaction; it exists so the model
extends naturally to an asynchronous or external settlement step.

## 8. Failure modes

| Failure | Behaviour |
|---|---|
| Duplicate / retried request | stored response replayed, no new side effects |
| Crash before commit | full rollback; retry executes normally |
| Crash after commit, response lost | retry replays stored response |
| Insufficient funds | transfer stored as FAILED, `422`, replayable |
| DB busy beyond timeout | `503`, nothing persisted, safe to retry |
| Unknown wallet / bad input | `404` / `400`, nothing persisted |

## 9. Observability

Structured logs (`log/slog`, JSON) for every transfer outcome with
`transfer_id`, `idempotency_key`, `status`, `replayed` and duration; request
logging middleware with method, path, status and latency.

## 10. Testing strategy

- Domain unit tests: state transitions, request validation.
- Service tests against a real PostgreSQL database (a fresh schema per test,
  from `TEST_DATABASE_URL`): successful
  transfer, ledger correctness, insufficient funds, idempotent replay, key reuse
  with a different payload, unknown wallets, and a concurrency test where many
  goroutines debit the same wallet at once (no overdraft, invariants hold) and
  where many goroutines send the same key (exactly one transfer).
- HTTP handler tests with `httptest` for status codes and replay headers.
