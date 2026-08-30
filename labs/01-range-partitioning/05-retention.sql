-- Lab 01 / step 5 -- retention, the reason you did all this.
--
-- Compare wall-clock time and post-op VACUUM debt against lab 00 step 5.
\timing on


\echo '=== the partition we are about to retire ==='
SELECT relname, pg_size_pretty(pg_total_relation_size(oid)) AS size
FROM pg_class
WHERE relname = 'events_p_' || to_char(now() - interval '13 months', 'YYYY_MM');

\echo
\echo '=== DETACH CONCURRENTLY: takes no ACCESS EXCLUSIVE lock on the parent ==='
\echo '(it cannot run inside a transaction block -- that is the trade-off, and'
\echo ' the reason this is driven by \\gexec rather than a DO block)'

-- The second restriction, and the one that ambushes people at 03:00:
--
--   ERROR:  cannot detach partitions concurrently when a default partition exists
--
-- CONCURRENTLY works by leaving the partition attached-but-invisible for one
-- transaction while old snapshots drain. With a DEFAULT in the picture the
-- rows being detached would momentarily be routable to two places, and
-- Postgres refuses to reason about that under anything weaker than ACCESS
-- EXCLUSIVE. So the default comes off first.
--
-- That plain DETACH does take ACCESS EXCLUSIVE on the parent -- briefly, and
-- on an empty table, but it takes it, which means it queues behind every open
-- transaction touching events_p and blocks every new one while it waits. The
-- lock is short; the wait for it is not bounded by anything you control. This
-- is the real cost of keeping a DEFAULT partition, and it is why mature
-- retention setups do not have one: they pre-create partitions far enough
-- ahead (lab 02) that no row can fall outside every range, and accept a failed
-- INSERT as the alarm if that ever stops being true.
--
-- Every step below is emitted conditionally and run through \gexec, so a
-- zero-row result means "already done" rather than an error. That matters
-- more than it looks: this file runs under ON_ERROR_STOP=1, and a retention
-- job that dies halfway would leave events_p with no DEFAULT partition at
-- all -- which turns the next misdated INSERT into an application error.
-- Retention jobs get re-run after they fail. Write them so that is safe.
SELECT 'ALTER TABLE events_p DETACH PARTITION events_p_default'
WHERE EXISTS (SELECT 1 FROM pg_inherits
              WHERE inhparent = 'events_p'::regclass
                AND inhrelid  = to_regclass('public.events_p_default'))
\gexec

SELECT format('ALTER TABLE events_p DETACH PARTITION %I CONCURRENTLY', v.name)
FROM (SELECT 'events_p_' || to_char(now() - interval '13 months', 'YYYY_MM')) AS v(name)
WHERE EXISTS (SELECT 1 FROM pg_inherits
              WHERE inhparent = 'events_p'::regclass
                AND inhrelid  = to_regclass('public.' || v.name))
\gexec

\echo
\echo '=== the data is now an ordinary table: archive it, or drop it ==='
\echo '(in production this is where you COPY to S3 / pg_dump before dropping)'
SELECT format('DROP TABLE %I', v.name)
FROM (SELECT 'events_p_' || to_char(now() - interval '13 months', 'YYYY_MM')) AS v(name)
WHERE to_regclass('public.' || v.name) IS NOT NULL
\gexec

\echo
\echo '=== put the safety net back ==='
-- ATTACH ... DEFAULT scans events_p_default to prove that nothing in it
-- belongs to a partition that already exists. It is empty here, so this is
-- instant. On a default that has quietly accumulated a month of misdated rows
-- it is a full scan under ACCESS EXCLUSIVE, and you will find that out during
-- the retention job rather than before it.
SELECT 'ALTER TABLE events_p ATTACH PARTITION events_p_default DEFAULT'
WHERE NOT EXISTS (SELECT 1 FROM pg_inherits
                  WHERE inhparent = 'events_p'::regclass
                    AND inhrelid  = to_regclass('public.events_p_default'))
\gexec

\echo
\echo '=== zero dead tuples, space returned to the OS, no VACUUM owed ==='
SELECT coalesce(sum(n_dead_tup), 0) AS dead_tuples_across_partitions
FROM pg_stat_user_tables WHERE relname LIKE 'events_p%';

SELECT pg_size_pretty(sum(pg_total_relation_size(c.oid))) AS total_now
FROM pg_class c
JOIN pg_inherits i ON i.inhrelid = c.oid
WHERE i.inhparent = 'events_p'::regclass;

\echo
\echo '----------------------------------------------------------------'
\echo ' lab 00 step 5: DELETE + VACUUM, seconds-to-minutes, heap never shrank.'
\echo ' lab 01 step 5: DETACH + DROP, milliseconds, space reclaimed.'
\echo ' That delta is the whole argument for partitioning.'
\echo '----------------------------------------------------------------'
