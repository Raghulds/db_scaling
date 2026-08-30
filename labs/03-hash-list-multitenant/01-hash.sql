-- Lab 03 / step 1 -- HASH partitioning by tenant.
--
-- The multi-tenant shape: every query is scoped to one tenant, and you want
-- each tenant's rows clustered so its working set is small.
--
-- WHAT TO NOTICE
--   * HASH gives you even *row* distribution only if tenants are even-sized.
--     They never are.
--   * a tenant-scoped query prunes to exactly 1 partition -- good
--   * a cross-tenant report scans all of them -- so HASH by tenant is the
--     wrong choice if your main workload is reporting
\timing on
\set ON_ERROR_STOP on

DROP TABLE IF EXISTS orders_h CASCADE;
CREATE TABLE orders_h (
  id          bigserial   NOT NULL,
  tenant_id   bigint      NOT NULL,
  placed_at   timestamptz NOT NULL DEFAULT now(),
  total_cents bigint      NOT NULL,
  state       text        NOT NULL,
  PRIMARY KEY (tenant_id, id)     -- partition key first: intentional
) PARTITION BY HASH (tenant_id);

DO $$
BEGIN
  FOR i IN 0..7 LOOP
    EXECUTE format(
      'CREATE TABLE orders_h_%s PARTITION OF orders_h FOR VALUES WITH (MODULUS 8, REMAINDER %s)',
      i, i);
  END LOOP;
END $$;

CREATE INDEX orders_h_tenant_placed_idx ON orders_h (tenant_id, placed_at DESC);

\echo '>> seeding' :rows 'rows across' :tenants 'tenants, heavily skewed'
INSERT INTO orders_h (tenant_id, placed_at, total_cents, state)
SELECT
  -- power(random(), 4) => the top few tenants own most rows. Realistic.
  (1 + floor(power(random(), 4) * :tenants))::bigint,
  now() - (random() * interval '90 days'),
  (random() * 20000)::bigint,
  (ARRAY['new','paid','shipped','refunded'])[1 + floor(random()*4)]
FROM generate_series(1, :rows) g;

ANALYZE orders_h;

\echo
\echo '=== row distribution across hash partitions ==='
SELECT c.relname,
       (SELECT reltuples::bigint FROM pg_class WHERE oid = c.oid) AS approx_rows,
       pg_size_pretty(pg_total_relation_size(c.oid))              AS total
FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
WHERE i.inhparent = 'orders_h'::regclass
ORDER BY c.relname;

\echo
\echo '=== tenant-scoped read -> exactly one partition ==='
EXPLAIN (ANALYZE, COSTS OFF)
SELECT count(*), sum(total_cents) FROM orders_h WHERE tenant_id = 3;

\echo
\echo '=== IN-list with 3 tenants -> up to 3 partitions ==='
EXPLAIN (COSTS OFF)
SELECT count(*) FROM orders_h WHERE tenant_id IN (3, 17, 42);

\echo
\echo '=== cross-tenant report -> every partition, every time ==='
EXPLAIN (ANALYZE, COSTS OFF)
SELECT state, count(*) FROM orders_h
WHERE placed_at >= now() - interval '7 days'
GROUP BY state;
