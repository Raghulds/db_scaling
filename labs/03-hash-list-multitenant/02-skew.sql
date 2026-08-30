-- Lab 03 / step 2 -- measure the skew you just created.
--
-- These two queries are the ones worth keeping. Run them against any
-- hash-partitioned or sharded table in production: the first finds the
-- whales, the second tells you when a rebalance is due.
\timing on

\echo '=== tenant size distribution (top 15) ==='
SELECT tenant_id,
       count(*) AS rows,
       round(100.0 * count(*) / sum(count(*)) OVER (), 2) AS pct_of_table
FROM orders_h
GROUP BY tenant_id
ORDER BY rows DESC
LIMIT 15;

\echo
\echo '=== how much of the table do the top 1% of tenants own? ==='
WITH per_tenant AS (
  SELECT tenant_id, count(*) AS n FROM orders_h GROUP BY tenant_id
), ranked AS (
  SELECT n, ntile(100) OVER (ORDER BY n DESC) AS pct_bucket FROM per_tenant
)
SELECT
  sum(n) FILTER (WHERE pct_bucket = 1)                                   AS top_1pct_rows,
  sum(n)                                                                 AS all_rows,
  round(100.0 * sum(n) FILTER (WHERE pct_bucket = 1) / sum(n), 2)        AS top_1pct_share
FROM ranked;

\echo
\echo '=== partition imbalance (reltuples -- run ANALYZE first) ==='
WITH per_part AS (
  SELECT c.relname,
         (SELECT reltuples::bigint FROM pg_class WHERE oid = c.oid) AS n
  FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
  WHERE i.inhparent = 'orders_h'::regclass
)
SELECT min(n) AS smallest, max(n) AS largest,
       round(max(n)::numeric / greatest(min(n), 1), 2) AS imbalance_ratio
FROM per_part;

\echo
\echo '>> A ratio near 1.0 means hash did its job. Above ~1.5 means one'
\echo '   partition is your bottleneck -- and adding partitions will not help,'
\echo '   because a single huge tenant cannot be split by hashing its id.'
\echo '   That is what step 3 is for.'
