-- Lab 00 / step 3 -- what does one big table cost?
--
-- WHAT TO NOTICE
--   * the index total is often larger than the heap
--   * the "last 7 days" query still reads the whole 12-month index
--   * a tenant-scoped query is fine -- that is why the pain arrives late
\timing on

\echo '=== size breakdown ==='
SELECT
  pg_size_pretty(pg_table_size('events'))            AS heap,
  pg_size_pretty(pg_indexes_size('events'))          AS indexes,
  pg_size_pretty(pg_total_relation_size('events'))   AS total;

SELECT indexrelname, pg_size_pretty(pg_relation_size(indexrelid)) AS size
FROM pg_stat_user_indexes
WHERE relname = 'events'
ORDER BY pg_relation_size(indexrelid) DESC;

\echo
\echo '=== Q1: recent window across all tenants (the reporting query) ==='
EXPLAIN (ANALYZE, BUFFERS, TIMING)
SELECT date_trunc('day', occurred_at) AS d, count(*), sum(amount_cents)
FROM events
WHERE occurred_at >= now() - interval '7 days'
GROUP BY 1 ORDER BY 1;

\echo
\echo '=== Q2: one tenant, recent window (the API query) ==='
EXPLAIN (ANALYZE, BUFFERS, TIMING)
SELECT id, occurred_at, kind, amount_cents
FROM events
WHERE tenant_id = 7 AND occurred_at >= now() - interval '30 days'
ORDER BY occurred_at DESC
LIMIT 50;

\echo
\echo '=== Q3: full-table aggregate (the one that will never be fast) ==='
EXPLAIN (ANALYZE, BUFFERS, TIMING)
SELECT count(*) FROM events;
