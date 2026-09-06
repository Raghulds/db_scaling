-- Lab 02 / step 3 -- the same job with pg_partman.
--
-- Read what it generates, then compare against your step 2 functions. The
-- interesting parts are the ones you did not think of: retention on a
-- schedule, template tables for per-partition objects, and the
-- premake/optimise settings.
\timing on
\set ON_ERROR_STOP on

CREATE SCHEMA IF NOT EXISTS partman;
CREATE EXTENSION IF NOT EXISTS pg_partman SCHEMA partman;

DROP TABLE IF EXISTS metrics CASCADE;

-- pg_partman ships no DDL event trigger, so DROP TABLE does not unregister
-- anything. Two things outlive the CASCADE and both break the second run of
-- this file:
--   * the part_config row. create_parent does a plain INSERT into a table
--     keyed on parent_table, so a re-run dies with a primary key violation
--     rather than anything that names the real problem.
--   * any child that retention DETACHED and kept (retention_keep_table below).
--     Once detached it is an ordinary table with no dependency on the parent,
--     so the CASCADE never sees it.
-- The documented teardown is partman.undo_partition() followed by a DELETE
-- from part_config. Forgetting it is why a "clean" redeploy of a partition
-- set works once and fails forever after.
DO $$
DECLARE r record;
BEGIN
  DELETE FROM partman.part_config WHERE parent_table = 'public.metrics';
  FOR r IN
    SELECT c.oid::regclass AS t
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public'
      AND c.relkind IN ('r', 'p')
      AND c.relname LIKE 'metrics\_%'
  LOOP
    EXECUTE format('DROP TABLE IF EXISTS %s CASCADE', r.t);
  END LOOP;
END $$;

CREATE TABLE metrics (
  id         bigserial NOT NULL,
  tenant_id  bigint NOT NULL,
  bucket     timestamptz NOT NULL,
  value      double precision NOT NULL,
  PRIMARY KEY (id, bucket)
) PARTITION BY RANGE (bucket);

CREATE INDEX metrics_tenant_bucket_idx ON metrics (tenant_id, bucket DESC);

-- This is the pg_partman 5.x signature: every parameter is p_-prefixed,
-- p_interval is the third argument, and p_type defaults to 'range'. On 4.x the
-- function was create_parent(p_parent_table, p_control, p_type, p_interval, ...)
-- with p_type mandatory and set to 'native' for declarative partitioning; the
-- call below fails there with "function partman.create_parent(...) does not
-- exist", which is the fastest way to discover which major version you are on.
--
-- p_start_partition is doing real work here, not decoration. Left NULL,
-- create_parent builds partitions from now() - premake*interval to
-- now() + premake*interval -- nine days, for these settings -- and the 30 days
-- of history seeded further down would all land in metrics_default. Retention
-- would then find nothing older than 14 days to act on, and the rest of this
-- file would demonstrate exactly nothing. Every backfill into a fresh
-- partition set has this problem, and p_start_partition is the answer to it.
SELECT partman.create_parent(
  p_parent_table    => 'public.metrics',
  p_control         => 'bucket',
  p_interval        => '1 day',
  p_premake         => 4,
  p_start_partition => (current_date - 30)::text
);

-- Retention: keep 14 days, detach rather than drop so nothing is lost by
-- accident, and let maintenance do the work.
UPDATE partman.part_config
SET retention              = '14 days',
    retention_keep_table   = true,
    retention_keep_index   = false,
    infinite_time_partitions = true
WHERE parent_table = 'public.metrics';

\echo '=== what partman created ==='
SELECT c.relname, pg_get_expr(c.relpartbound, c.oid) AS bounds
FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
WHERE i.inhparent = 'metrics'::regclass
ORDER BY c.relname;

\echo
\echo '=== seed 30 days, then run maintenance and watch retention fire ==='
INSERT INTO metrics (tenant_id, bucket, value)
SELECT (g % 50) + 1,
       now() - (g % 30) * interval '1 day',
       random() * 100
FROM generate_series(1, 200000) g;

SELECT count(*) AS partitions_before
FROM pg_inherits WHERE inhparent = 'metrics'::regclass;

CALL partman.run_maintenance_proc();

SELECT count(*) AS partitions_after
FROM pg_inherits WHERE inhparent = 'metrics'::regclass;

\echo
\echo '=== detached-but-kept tables (retention_keep_table = true) ==='
-- relkind and the escaped underscore both matter. Without relkind this also
-- lists metrics_pkey: the parent's index is a partitioned index, so it is an
-- inhparent in pg_inherits and never an inhrelid, which is exactly the test
-- being used for "detached". Detached children's own indexes would show up
-- the same way. An unescaped _ in LIKE is a wildcard, too.
SELECT relname FROM pg_class
WHERE relkind = 'r'
  AND relnamespace = 'public'::regnamespace
  AND relname LIKE 'metrics\_p%'
  AND oid NOT IN (SELECT inhrelid FROM pg_inherits)
ORDER BY relname;

\echo
\echo '>> in production: run `CALL partman.run_maintenance_proc()` from pg_cron'
\echo '   or an external scheduler, and ALERT if the newest partition upper'
\echo '   bound is less than 24h away. That alert is the one that saves you.'
