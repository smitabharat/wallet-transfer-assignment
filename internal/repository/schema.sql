-- Schema for the wallet transfer service (PostgreSQL).
-- All amounts are integers in the smallest currency unit.

CREATE TABLE IF NOT EXISTS wallets (
    id              TEXT        PRIMARY KEY,
    balance         BIGINT      NOT NULL CHECK (balance >= 0),
    initial_balance BIGINT      NOT NULL CHECK (initial_balance >= 0),
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS transfers (
    id              TEXT        PRIMARY KEY,
    idempotency_key TEXT        UNIQUE,
    from_wallet_id  TEXT        NOT NULL REFERENCES wallets (id),
    to_wallet_id    TEXT        NOT NULL REFERENCES wallets (id),
    amount          BIGINT      NOT NULL CHECK (amount > 0),
    status          TEXT        NOT NULL CHECK (status IN ('PENDING', 'PROCESSED', 'FAILED')),
    failure_reason  TEXT,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    CHECK (from_wallet_id <> to_wallet_id)
);

CREATE INDEX IF NOT EXISTS idx_transfers_from_wallet ON transfers (from_wallet_id);
CREATE INDEX IF NOT EXISTS idx_transfers_to_wallet ON transfers (to_wallet_id);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transfer_id TEXT        NOT NULL REFERENCES transfers (id),
    wallet_id   TEXT        NOT NULL REFERENCES wallets (id),
    type        TEXT        NOT NULL CHECK (type IN ('DEBIT', 'CREDIT')),
    amount      BIGINT      NOT NULL CHECK (amount > 0),
    created_at  TIMESTAMPTZ NOT NULL,
    -- a transfer has exactly one debit and one credit, never more
    UNIQUE (transfer_id, type)
);

CREATE INDEX IF NOT EXISTS idx_ledger_entries_wallet ON ledger_entries (wallet_id, id);

CREATE TABLE IF NOT EXISTS idempotency_records (
    key             TEXT        PRIMARY KEY,
    request_hash    TEXT        NOT NULL,
    transfer_id     TEXT        NOT NULL REFERENCES transfers (id),
    -- kept as TEXT (not JSONB) so the replayed body is byte-for-byte the original
    response_body   TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL
);
