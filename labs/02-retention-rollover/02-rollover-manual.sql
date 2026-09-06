-- Lab 02 / step 2 -- hand-rolled rollover: create ahead, retire behind.
--
-- This is ~40 lines of plpgsql and it is genuinely enough for most systems.
-- Write it once, schedule it, and understand every line -- then decide in
-- step 3 whether pg_partman earns its dependency.
\timing on
\set ON_ERROR_STOP on

CREATE OR REPLACE FUNCTION ensure_month_partitions(
  parent       regclass,
  months_ahead int DEFAULT 3
) RETURNS int LANGUAGE plpgsql AS $$
DECLARE
  m        date;
  part     text;
  created  int := 0;
BEGIN
  FOR i IN 0..months_ahead LOOP
    m    := (date_trunc('month', now()) + (i || ' months')::interval)::date;
    part := parent::text || '_' || to_char(m, 'YYYY_MM');

    IF to_regclass(part) IS NULL THEN
      EXECUTE format(
        'CREATE TABLE %I PARTITION OF %s FOR VALUES FROM (%L) TO (%L)',
        part, parent::text, m, (m + interval '1 month')::date
      );
      created := created + 1;
      RAISE NOTICE 'created %', part;
    END IF;
  END LOOP;
  RETURN created;
END $$;

CREATE OR REPLACE FUNCTION retire_month_partitions(
  parent        regclass,
  keep_months   int,
  archive       boolean DEFAULT false
) RETURNS int LANGUAGE plpgsql AS $$
DECLARE
  r        record;
  cutoff   date := (date_trunc('month', now()) - (keep_months || ' months')::interval)::date;
  dropped  int := 0;
BEGIN
  -- The ORDER BY is load-bearing, not cosmetic. Without it this is a lazily
  -- fetched cursor over pg_inherits/pg_class while the loop body is DETACHing
  -- partitions out from under it -- and catalog scans do not honour the
  -- portal's snapshot the way a user table would, so rows can be skipped.
  -- The sort forces the whole list to be materialised before the first
  -- DETACH runs. Oldest first is also the order you want in the log.
  FOR r IN
    SELECT c.relname,
           -- parse the lower bound straight out of the partition bound expr
           (regexp_match(pg_get_expr(c.relpartbound, c.oid),
                         'FROM \(''([^'']+)''\)'))[1]::date AS lower
    FROM pg_class c
    JOIN pg_inherits i ON i.inhrelid = c.oid
    WHERE i.inhparent = parent
      AND pg_get_expr(c.relpartbound, c.oid) LIKE 'FOR VALUES FROM%'
    ORDER BY 2
  LOOP
    CONTINUE WHEN r.lower >= cutoff;

    -- DETACH first: the parent is never locked while we archive or drop.
    EXECUTE format('ALTER TABLE %s DETACH PARTITION %I', parent::text, r.relname);

    IF archive THEN
      EXECUTE format('ALTER TABLE %I RENAME TO %I', r.relname, r.relname || '_archived');
      RAISE NOTICE 'archived %', r.relname;
    ELSE
      EXECUTE format('DROP TABLE %I', r.relname);
      RAISE NOTICE 'dropped %', r.relname;
    END IF;
    dropped := dropped + 1;
  END LOOP;
  RETURN dropped;
END $$;

\echo '=== run it against events_p from lab 01 ==='
SELECT ensure_month_partitions('events_p', 3)  AS partitions_created;
SELECT retire_month_partitions('events_p', 6, false) AS partitions_retired;

SELECT count(*) AS live_partitions
FROM pg_inherits WHERE inhparent = 'events_p'::regclass;

\echo
\echo '>> NOTE the ordering in retire(): DETACH, then DROP. Doing DROP TABLE on'
\echo '   an attached partition takes ACCESS EXCLUSIVE on the parent, which'
\echo '   blocks every query against the whole table, not just the old month.'
