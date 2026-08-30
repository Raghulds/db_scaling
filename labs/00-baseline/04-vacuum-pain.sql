-- Lab 00 / step 4 -- MVCC bloat, the real reason big tables hurt.
--
-- Postgres does not update rows in place: it writes a new version and leaves
-- the old one dead until VACUUM reclaims it. On a 5M-row table that is a
-- shrug. On a 500M-row table it is a pager at 3am.
--
-- WHAT TO NOTICE
--   * the table grows even though the row count does not
--   * VACUUM time scales with table size, not with the size of your change
--   * VACUUM FULL takes an ACCESS EXCLUSIVE lock -- unusable in production
\timing on

\echo '=== before ==='
SELECT n_live_tup, n_dead_tup,
       pg_size_pretty(pg_table_size('events')) AS heap
FROM pg_stat_user_tables WHERE relname = 'events';

\echo
\echo '=== update ~10% of rows ==='
UPDATE events SET status = 'reopened', amount_cents = amount_cents + 1
WHERE id % 10 = 0;

SELECT pg_stat_reset_single_table_counters('events'::regclass);
ANALYZE events;

\echo
\echo '=== after the update: dead tuples and a bigger heap ==='
SELECT n_live_tup, n_dead_tup,
       pg_size_pretty(pg_table_size('events'))   AS heap,
       pg_size_pretty(pg_indexes_size('events')) AS indexes
FROM pg_stat_user_tables WHERE relname = 'events';

\echo
\echo '=== VACUUM (verbose) -- read the scan counts, they are the whole point ==='
VACUUM (VERBOSE, ANALYZE) events;

\echo
\echo '=== space is reusable now, but NOT returned to the OS ==='
SELECT pg_size_pretty(pg_table_size('events')) AS heap_after_vacuum;
\echo '(only VACUUM FULL / pg_repack shrinks the file -- and VACUUM FULL locks the table)'
