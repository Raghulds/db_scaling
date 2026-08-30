-- Lab 01 / step 3 -- pruning: the entire payoff, and how to lose it.
--
-- WHAT TO NOTICE
--   Q1 touches 1 partition       -> "Append" with a single child
--   Q2 has no partition key      -> every partition scanned, N index scans
--   Q3 pruning disabled          -> what Q1 would cost without the feature
--   Q4 generic plan + parameter  -> "Subplans Removed", pruning at execution
--   Q5 partitionwise aggregate   -> per-partition aggregation, then merge
\timing on

\echo '=== Q1: bounded window -> plan-time pruning ==='
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT count(*), sum(amount_cents)
FROM events_p
WHERE occurred_at >= date_trunc('month', now())
  AND occurred_at <  date_trunc('month', now()) + interval '1 month';

\echo
\echo '=== Q2: THE MISTAKE -- tenant lookup with no time bound ==='
\echo '(one index scan per partition. Partitioning made this slower.)'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT id, occurred_at, amount_cents
FROM events_p
WHERE tenant_id = 7
ORDER BY occurred_at DESC
LIMIT 50;

\echo
\echo '=== Q2b: same query, time-bounded -> back to one partition ==='
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT id, occurred_at, amount_cents
FROM events_p
WHERE tenant_id = 7
  AND occurred_at >= now() - interval '20 days'
ORDER BY occurred_at DESC
LIMIT 50;

\echo
\echo '=== Q3: the counterfactual -- pruning turned off ==='
SET enable_partition_pruning = off;
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT count(*) FROM events_p
WHERE occurred_at >= date_trunc('month', now());
RESET enable_partition_pruning;

\echo
\echo '=== Q4: run-time pruning with a generic plan ==='
\echo '(look for "Subplans Removed: N" -- pruning happened at EXECUTE time)'
DEALLOCATE ALL;
PREPARE recent(timestamptz) AS
  SELECT count(*) FROM events_p WHERE occurred_at >= $1;
SET plan_cache_mode = force_generic_plan;
EXPLAIN (ANALYZE, COSTS OFF) EXECUTE recent(now() - interval '3 days');
RESET plan_cache_mode;

\echo
\echo '=== Q5: partitionwise aggregate off vs on ==='
SET enable_partitionwise_aggregate = off;
EXPLAIN (ANALYZE, COSTS OFF)
SELECT tenant_id, count(*) FROM events_p GROUP BY tenant_id;
SET enable_partitionwise_aggregate = on;
EXPLAIN (ANALYZE, COSTS OFF)
SELECT tenant_id, count(*) FROM events_p GROUP BY tenant_id;
RESET enable_partitionwise_aggregate;

\echo
\echo '=== planning cost of many partitions ==='
\echo '(partition count is not free: the planner considers each one)'
EXPLAIN (ANALYZE, SUMMARY ON, COSTS OFF)
SELECT count(*) FROM events_p;
