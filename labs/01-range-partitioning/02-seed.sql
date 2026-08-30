-- Lab 01 / step 2 -- same data, same volume as lab 00.
--
-- Compare the load time with lab 00. Routing has a cost, and index
-- maintenance on smaller B-trees has a benefit; which wins depends on volume.
\timing on
\set ON_ERROR_STOP on

\echo '>> seeding' :rows 'rows into events_p'

INSERT INTO events_p (tenant_id, occurred_at, kind, status, amount_cents, payload)
SELECT
  (1 + floor(power(random(), 3) * 500))::bigint,
  now() - (random() * interval '365 days'),
  (ARRAY['click','view','purchase','signup','email'])[1 + floor(random()*5)],
  CASE WHEN random() < 0.92 THEN 'done' ELSE 'new' END,
  (random() * 50000)::bigint,
  jsonb_build_object('src', 'seed', 'n', g)
FROM generate_series(1, :rows) AS g;

ANALYZE events_p;

\echo '>> rows per partition (watch for the DEFAULT partition being empty):'
SELECT c.relname,
       (SELECT reltuples::bigint FROM pg_class WHERE oid = c.oid) AS approx_rows,
       pg_size_pretty(pg_total_relation_size(c.oid))              AS total
FROM pg_class c
JOIN pg_inherits i ON i.inhrelid = c.oid
WHERE i.inhparent = 'events_p'::regclass
ORDER BY c.relname;
