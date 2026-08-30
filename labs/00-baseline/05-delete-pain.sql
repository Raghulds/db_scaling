-- Lab 00 / step 5 -- deleting old data, the expensive way.
--
-- This is the single strongest argument for partitioning. Time it, then run
-- lab 01 step 5 where the same retention job is a DROP TABLE.
\timing on

\echo '=== how much are we about to delete? ==='
SELECT count(*) AS doomed
FROM events
WHERE occurred_at < now() - interval '300 days';

\echo
\echo '=== DELETE (note the time, and that the heap does not shrink) ==='
EXPLAIN (ANALYZE, BUFFERS)
DELETE FROM events WHERE occurred_at < now() - interval '300 days';

ANALYZE events;
SELECT n_live_tup, n_dead_tup, pg_size_pretty(pg_table_size('events')) AS heap
FROM pg_stat_user_tables WHERE relname = 'events';

\echo
\echo '=== and now you owe a VACUUM for every row you deleted ==='
VACUUM (VERBOSE) events;

\echo
\echo '--------------------------------------------------------------'
\echo ' Record these numbers. Lab 01 does the same retention in ~1ms.'
\echo '--------------------------------------------------------------'
