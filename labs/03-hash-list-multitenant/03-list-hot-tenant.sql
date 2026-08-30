-- Lab 03 / step 3 -- LIST partitioning: the hot-tenant escape hatch.
--
-- Give the whale its own partition (later: its own shard) and hash the rest.
-- Postgres has no "sub-hash under a LIST default" in one level, so this is
-- LIST at the top with a DEFAULT that is itself hash-partitioned.
--
-- This two-level shape is exactly what you want in a sharded system too:
-- a lookup table pinning big tenants, hashing for the long tail.
\timing on
\set ON_ERROR_STOP on

DROP TABLE IF EXISTS orders_l CASCADE;
CREATE TABLE orders_l (
  id          bigserial   NOT NULL,
  tenant_id   bigint      NOT NULL,
  placed_at   timestamptz NOT NULL DEFAULT now(),
  total_cents bigint      NOT NULL,
  state       text        NOT NULL,
  PRIMARY KEY (tenant_id, id)
) PARTITION BY LIST (tenant_id);

-- Whales: one partition each. Find them with the query from step 2.
CREATE TABLE orders_l_t1 PARTITION OF orders_l FOR VALUES IN (1);
CREATE TABLE orders_l_t2 PARTITION OF orders_l FOR VALUES IN (2);

-- Everyone else: hashed, 4 ways, under the DEFAULT partition.
CREATE TABLE orders_l_rest PARTITION OF orders_l DEFAULT
  PARTITION BY HASH (tenant_id);
DO $$
BEGIN
  FOR i IN 0..3 LOOP
    EXECUTE format(
      'CREATE TABLE orders_l_rest_%s PARTITION OF orders_l_rest FOR VALUES WITH (MODULUS 4, REMAINDER %s)',
      i, i);
  END LOOP;
END $$;

CREATE INDEX orders_l_tenant_placed_idx ON orders_l (tenant_id, placed_at DESC);

\echo '>> copying lab-03 data into the two-level layout'
INSERT INTO orders_l (tenant_id, placed_at, total_cents, state)
SELECT tenant_id, placed_at, total_cents, state FROM orders_h;
ANALYZE orders_l;

\echo
\echo '=== the tree ==='
SELECT
  parent.relname AS parent,
  child.relname  AS partition,
  pg_get_expr(child.relpartbound, child.oid) AS bounds,
  pg_size_pretty(pg_total_relation_size(child.oid)) AS size
FROM pg_inherits i
JOIN pg_class child  ON child.oid  = i.inhrelid
JOIN pg_class parent ON parent.oid = i.inhparent
-- pg_inherits records partitioned INDEX trees as well as table trees, and
-- orders_l_pkey / orders_l_tenant_placed_idx both match the name pattern. The
-- relkind filter is what keeps this listing to tables; without it every index
-- shows up with a NULL bounds column and the shape of the tree is lost.
WHERE parent.relname LIKE 'orders\_l%'
  AND parent.relkind = 'p'
  AND child.relkind IN ('r', 'p')
ORDER BY parent.relname, child.relname;

\echo
\echo '=== whale query: one dedicated partition ==='
EXPLAIN (ANALYZE, COSTS OFF)
SELECT count(*) FROM orders_l WHERE tenant_id = 1;

\echo
\echo '=== long-tail query: prunes through both levels to one leaf ==='
EXPLAIN (ANALYZE, COSTS OFF)
SELECT count(*) FROM orders_l WHERE tenant_id = 137;

\echo
\echo '=== promoting a tenant to its own partition, online ==='
\echo '(split the DEFAULT: this is the partitioning-level version of the'
\echo ' shard split you will do by hand in lab 05)'
BEGIN;
  CREATE TABLE orders_l_t9 (LIKE orders_l INCLUDING DEFAULTS INCLUDING INDEXES);
  ALTER TABLE orders_l_t9 ADD CONSTRAINT ck CHECK (tenant_id = 9);
  INSERT INTO orders_l_t9 SELECT * FROM orders_l WHERE tenant_id = 9;
  DELETE FROM orders_l WHERE tenant_id = 9;
  ALTER TABLE orders_l ATTACH PARTITION orders_l_t9 FOR VALUES IN (9);
COMMIT;

SELECT count(*) AS tenant_9_rows FROM orders_l_t9;
\echo
\echo '>> that transaction held locks the whole time. At tenant scale you do'
\echo '   it the lab-05 way instead: dual-write, backfill, verify, cut over.'
