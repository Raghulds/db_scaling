-- Lab 01 / step 1 -- declarative RANGE partitioning by time.
--
-- Same columns as lab 00, one keyword different. Note what the partition key
-- forces on you: it must be part of every unique constraint, including the
-- primary key. `id` alone can no longer be the PK.
\timing on
\set ON_ERROR_STOP on

DROP TABLE IF EXISTS events_p CASCADE;

CREATE TABLE events_p (
  id           bigserial   NOT NULL,
  tenant_id    bigint      NOT NULL,
  occurred_at  timestamptz NOT NULL,
  kind         text        NOT NULL,
  status       text        NOT NULL DEFAULT 'new',
  amount_cents bigint      NOT NULL DEFAULT 0,
  payload      jsonb       NOT NULL DEFAULT '{}'::jsonb,
  -- the partition key is dragged into the PK. This is not a formality:
  -- it means `WHERE id = ?` alone can never be a single-partition lookup.
  PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

-- Indexes created on the parent are cloned onto every partition, existing
-- and future. Create them here, not per-partition.
CREATE INDEX events_p_tenant_time_idx ON events_p (tenant_id, occurred_at DESC);
CREATE INDEX events_p_status_idx      ON events_p (status) WHERE status <> 'done';

-- 15 monthly partitions: 13 back, the current one, 1 ahead. That single
-- month of headroom is the bug you only find in production at midnight on
-- the 1st -- see lab 02 for the job that keeps it topped up.
DO $$
DECLARE
  m date;
BEGIN
  FOR i IN -13..1 LOOP
    m := date_trunc('month', now())::date + (i || ' months')::interval;
    EXECUTE format(
      'CREATE TABLE %I PARTITION OF events_p FOR VALUES FROM (%L) TO (%L)',
      'events_p_' || to_char(m, 'YYYY_MM'),
      m,
      (m + interval '1 month')::date
    );
  END LOOP;
END $$;

-- A DEFAULT partition catches rows outside every range. It saves you from
-- INSERT errors and costs you something later -- see step 4.
CREATE TABLE events_p_default PARTITION OF events_p DEFAULT;

\echo '>> partitions:'
SELECT c.relname,
       pg_get_expr(c.relpartbound, c.oid) AS bounds
FROM pg_class c
JOIN pg_inherits i ON i.inhrelid = c.oid
WHERE i.inhparent = 'events_p'::regclass
ORDER BY c.relname;
