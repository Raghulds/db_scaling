-- Per-shard schema. Identical on every node -- that is the point: a shard is
-- an ordinary, boring Postgres database that knows nothing about the others.
--
-- Applied by scripts/shards-init.sh. Idempotent.

CREATE TABLE IF NOT EXISTS orders (
  -- Snowflake id from the app, not bigserial: see internal/shard/id.go for
  -- why a per-node sequence is unusable across shards.
  id            bigint      PRIMARY KEY,
  tenant_id     bigint      NOT NULL,
  -- The logical shard this row belongs to, stored rather than recomputed.
  -- Makes every migration query a cheap range predicate.
  logical_shard int         NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  total_cents   bigint      NOT NULL,
  state         text        NOT NULL
);

-- The single-shard fast path.
CREATE INDEX IF NOT EXISTS orders_tenant_created_idx
  ON orders (tenant_id, created_at DESC);

-- The cross-shard feed. (created_at DESC, id DESC) matches the keyset order
-- exactly, so each shard's page is an index-only range scan.
CREATE INDEX IF NOT EXISTS orders_created_id_idx
  ON orders (created_at DESC, id DESC);

-- Migration support: "give me every row for logical shards 512-767".
CREATE INDEX IF NOT EXISTS orders_logical_idx
  ON orders (logical_shard);

-- ------------------------------------------------------- cross-shard writes

-- Balances, for the 2PC and outbox demos.
CREATE TABLE IF NOT EXISTS ledger (
  tenant_id     bigint PRIMARY KEY,
  balance_cents bigint NOT NULL DEFAULT 0
);

-- Outbox: written in the same transaction as the local effect.
CREATE TABLE IF NOT EXISTS outbox (
  id           bigint PRIMARY KEY,
  target_shard int    NOT NULL,
  kind         text   NOT NULL,
  payload      jsonb  NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  delivered_at timestamptz
);

-- Undelivered rows only: a partial index keeps the relay's poll cheap even
-- when the table has millions of delivered rows in it.
CREATE INDEX IF NOT EXISTS outbox_pending_idx
  ON outbox (id) WHERE delivered_at IS NULL;

-- Idempotency ledger on the receiving side. This one table is what turns
-- at-least-once delivery into effectively-once application.
CREATE TABLE IF NOT EXISTS applied (
  event_id   bigint PRIMARY KEY,
  applied_at timestamptz NOT NULL DEFAULT now()
);
