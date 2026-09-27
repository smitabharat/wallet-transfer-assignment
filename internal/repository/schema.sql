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
    -- at most one debit and one credit per transfer; the trigger below
    -- enforces that a PROCESSED transfer has exactly both, matching its
    -- own wallets and amount, which UNIQUE alone cannot express
    UNIQUE (transfer_id, type)
);

CREATE INDEX IF NOT EXISTS idx_ledger_entries_wallet ON ledger_entries (wallet_id, id);

-- check_transfer_ledger_pair enforces, for one transfer, that:
--   - a PROCESSED transfer has exactly one DEBIT (from_wallet_id, amount) and
--     exactly one CREDIT (to_wallet_id, amount) ledger row, never fewer, more,
--     or one that disagrees with the transfer's own wallets/amount;
--   - a PENDING or FAILED transfer has no ledger rows at all.
-- It is installed as a constraint trigger on both tables so it also fires,
-- deferred to commit, when a transfer's status changes after its ledger rows
-- were written.
CREATE OR REPLACE FUNCTION check_transfer_ledger_pair() RETURNS trigger AS $$
DECLARE
    t          transfers%ROWTYPE;
    ok         boolean;
    transfer_i TEXT;
BEGIN
    -- NEW is shaped differently depending on which table fired the trigger:
    -- a ledger_entries row has transfer_id, a transfers row's own id IS the
    -- transfer id.
    IF TG_TABLE_NAME = 'ledger_entries' THEN
        transfer_i := NEW.transfer_id;
    ELSE
        transfer_i := NEW.id;
    END IF;

    SELECT * INTO t FROM transfers WHERE id = transfer_i;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    IF t.status = 'PROCESSED' THEN
        SELECT COUNT(*) = 2 AND bool_and(
            (type = 'DEBIT'  AND wallet_id = t.from_wallet_id AND amount = t.amount) OR
            (type = 'CREDIT' AND wallet_id = t.to_wallet_id   AND amount = t.amount)
        ) INTO ok
        FROM ledger_entries WHERE transfer_id = t.id;

        IF NOT ok THEN
            RAISE EXCEPTION
                'transfer % is PROCESSED but its ledger entries are not a balanced debit/credit pair',
                t.id;
        END IF;
    ELSIF EXISTS (SELECT 1 FROM ledger_entries WHERE transfer_id = t.id) THEN
        RAISE EXCEPTION 'transfer % has status % but has ledger entries', t.id, t.status;
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ledger_entries_pair ON ledger_entries;
CREATE CONSTRAINT TRIGGER trg_ledger_entries_pair
    AFTER INSERT OR UPDATE ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_transfer_ledger_pair();

DROP TRIGGER IF EXISTS trg_transfers_ledger_pair ON transfers;
CREATE CONSTRAINT TRIGGER trg_transfers_ledger_pair
    AFTER INSERT OR UPDATE ON transfers
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_transfer_ledger_pair();

CREATE TABLE IF NOT EXISTS idempotency_records (
    key             TEXT        PRIMARY KEY,
    request_hash    TEXT        NOT NULL,
    transfer_id     TEXT        NOT NULL REFERENCES transfers (id),
    -- kept as TEXT (not JSONB) so the replayed body is byte-for-byte the original
    response_body   TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL
);
