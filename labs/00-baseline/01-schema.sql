-- Lab 00 / step 1 -- the naive table.
--
-- One flat table, the shape almost every product starts with: a tenant id,
-- a timestamp, a status, a payload. No partitioning, no sharding.
\timing on
\set ON_ERROR_STOP on

DROP TABLE IF EXISTS events;

CREATE TABLE events (
  id          bigserial   PRIMARY KEY,
  tenant_id   bigint      NOT NULL,
  occurred_at timestamptz NOT NULL,
  kind        text        NOT NULL,
  status      text        NOT NULL DEFAULT 'new',
  amount_cents bigint     NOT NULL DEFAULT 0,
  payload     jsonb       NOT NULL DEFAULT '{}'::jsonb
);

-- The indexes you would actually create for these access patterns.
CREATE INDEX events_tenant_time_idx ON events (tenant_id, occurred_at DESC);
CREATE INDEX events_occurred_at_idx ON events (occurred_at);
CREATE INDEX events_status_idx      ON events (status) WHERE status <> 'done';

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

\echo '>> flat `events` table created'
