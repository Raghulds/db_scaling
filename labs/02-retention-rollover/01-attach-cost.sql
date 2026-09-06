-- Lab 02 / step 1 -- the ATTACH validation scan, and how to avoid it.
--
-- Backfilling a partition means: build the table standalone, load it fast,
-- then attach. ATTACH must prove every row belongs in the new range. If a
-- matching CHECK constraint already exists, Postgres trusts it and skips the
-- scan. If not, it scans the whole table under ACCESS EXCLUSIVE.
--
-- Two identical attaches below. The only difference is the CHECK. Compare
-- the timings -- this is the difference between a 2ms deploy and a 4-minute
-- outage on a large partition.
\timing on
\set ON_ERROR_STOP on

-- Dropping the parent drops whatever is still attached to it, but a run that
-- died between the CREATE and the ATTACH leaves cand_slow/cand_fast behind as
-- ordinary standalone tables. Name them explicitly so a retry is clean.
DROP TABLE IF EXISTS attach_demo CASCADE;
DROP TABLE IF EXISTS cand_slow, cand_fast;

CREATE TABLE attach_demo (
  id bigserial NOT NULL,
  occurred_at timestamptz NOT NULL,
  payload text NOT NULL,
  PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE TABLE attach_demo_2020_01 PARTITION OF attach_demo
  FOR VALUES FROM ('2020-01-01') TO ('2020-02-01');

-- Two candidate partitions with the same 2 million rows.
\echo '>> building two identical standalone tables'
CREATE TABLE cand_slow (LIKE attach_demo INCLUDING DEFAULTS INCLUDING INDEXES);
INSERT INTO cand_slow (occurred_at, payload)
SELECT '2020-02-01'::timestamptz + (g % 28) * interval '1 day', 'x' || g
FROM generate_series(1, 2000000) g;
ANALYZE cand_slow;

CREATE TABLE cand_fast (LIKE attach_demo INCLUDING DEFAULTS INCLUDING INDEXES);
INSERT INTO cand_fast (occurred_at, payload)
SELECT '2020-03-01'::timestamptz + (g % 28) * interval '1 day', 'x' || g
FROM generate_series(1, 2000000) g;
ANALYZE cand_fast;

\echo
\echo '=== ATTACH without a CHECK -- full validation scan ==='
ALTER TABLE attach_demo ATTACH PARTITION cand_slow
  FOR VALUES FROM ('2020-02-01') TO ('2020-03-01');

\echo
\echo '=== ATTACH with a matching CHECK -- constraint is trusted, no scan ==='
ALTER TABLE cand_fast ADD CONSTRAINT cand_fast_range CHECK (
  occurred_at >= '2020-03-01'::timestamptz AND occurred_at < '2020-04-01'::timestamptz
);
ALTER TABLE attach_demo ATTACH PARTITION cand_fast
  FOR VALUES FROM ('2020-03-01') TO ('2020-04-01');

\echo
\echo '>> the CHECK is now redundant and can be dropped'
ALTER TABLE cand_fast DROP CONSTRAINT cand_fast_range;

\echo '>> NOT NULL on the partition key matters too: a nullable key forces the'
\echo '   scan even with a CHECK, because NULL passes no range test.'
