-- Lab 01 / step 4 -- the walls you will hit. Each block is expected to FAIL.
--
-- Read the error text, not just the fact that it failed. These constraints
-- decide whether partitioning is viable for a given table, so knowing them
-- up front is worth more than knowing the syntax.
\set ON_ERROR_STOP off
\timing off

\echo '=== L1: a UNIQUE constraint that omits the partition key ==='
\echo '(so: no global uniqueness on an external id, ever)'
CREATE UNIQUE INDEX events_p_ext_uniq ON events_p (id);

\echo
\echo '=== L2: a row outside every range, with the DEFAULT partition detached ==='
ALTER TABLE events_p DETACH PARTITION events_p_default;
INSERT INTO events_p (tenant_id, occurred_at, kind)
VALUES (1, '1999-01-01', 'ancient');

\echo
\echo '=== L2b: reattach DEFAULT, and the insert now lands there ==='
\set ON_ERROR_STOP on
ALTER TABLE events_p ATTACH PARTITION events_p_default DEFAULT;
INSERT INTO events_p (tenant_id, occurred_at, kind)
VALUES (1, '1999-01-01', 'ancient');
SELECT count(*) AS rows_in_default FROM events_p_default;

\echo
\echo '=== L3: the DEFAULT partition now blocks cheap partition creation ==='
\echo '(Postgres must scan DEFAULT to prove no row belongs in the new range,'
\echo ' holding an ACCESS EXCLUSIVE lock on it while it does)'
\timing on
-- Re-run safety: this file is the only thing that creates events_p_1999_01,
-- and from here down ON_ERROR_STOP is back on, so a second run of 04 without
-- a fresh 01 would abort on "relation already exists".
DROP TABLE IF EXISTS events_p_1999_01;
CREATE TABLE events_p_1999_01 (LIKE events_p INCLUDING DEFAULTS);
INSERT INTO events_p_1999_01 SELECT * FROM events_p_default;
DELETE FROM events_p_default;
ALTER TABLE events_p_1999_01 ADD CONSTRAINT ck
  CHECK (occurred_at >= '1999-01-01' AND occurred_at < '1999-02-01');
ALTER TABLE events_p ATTACH PARTITION events_p_1999_01
  FOR VALUES FROM ('1999-01-01') TO ('1999-02-01');
\timing off

\echo
\echo '=== L4: an UPDATE that moves a row across partitions ==='
\echo '(allowed since PG11, but it is a DELETE + INSERT -- new ctid, more bloat,'
\echo ' and it can surprise a cursor or a BEFORE UPDATE trigger)'
UPDATE events_p SET occurred_at = occurred_at - interval '400 days'
WHERE id IN (SELECT id FROM events_p LIMIT 5);

\echo
\echo '=== L5: CREATE INDEX CONCURRENTLY on a partitioned parent ==='
\set ON_ERROR_STOP off
CREATE INDEX CONCURRENTLY events_p_kind_idx ON events_p (kind);
\echo '(the workaround: CIC on each partition, then a bare ON ONLY parent index,'
\echo ' then ALTER INDEX ... ATTACH PARTITION for each child)'

\echo
\echo '=== L6: no exclusion constraints on partitioned tables ==='
CREATE TABLE booking (
  id bigint, room int, during tstzrange,
  EXCLUDE USING gist (room WITH =, during WITH &&)
) PARTITION BY RANGE (id);

\set ON_ERROR_STOP on
\echo
\echo '>> done. Every failure above is a design constraint worth writing down.'
