-- Lab 00 / step 2 -- load :rows rows spread over the last 12 months.
--
-- Override the volume with:  ROWS=20000000 make lab00
-- 5M rows is enough to feel the difference; 50M+ makes it unmistakable.
\timing on
\set ON_ERROR_STOP on

\echo '>> seeding' :rows 'rows (this is the slow part)'

INSERT INTO events (tenant_id, occurred_at, kind, status, amount_cents, payload)
SELECT
  -- Zipf-ish tenant skew: a few tenants own most of the rows, like reality.
  (1 + floor(power(random(), 3) * 500))::bigint                     AS tenant_id,
  now() - (random() * interval '365 days')                          AS occurred_at,
  (ARRAY['click','view','purchase','signup','email'])[1 + floor(random()*5)] AS kind,
  CASE WHEN random() < 0.92 THEN 'done' ELSE 'new' END              AS status,
  (random() * 50000)::bigint                                        AS amount_cents,
  jsonb_build_object('src', 'seed', 'n', g)                         AS payload
FROM generate_series(1, :rows) AS g;

ANALYZE events;

\echo '>> seeded. row count:'
SELECT count(*) AS rows FROM events;
