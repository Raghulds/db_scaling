# PostgreSQL Commands Reference

A deduplicated PG 16 cheat sheet (SQL + psql), ordered basic → advanced. Each
command appears once in its primary section; related sections link back instead of
repeating blocks. Where this repo exercises a command, a **Used in** note points
to the lab.

Official manual: <https://www.postgresql.org/docs/16/index.html>

---

## Table of contents

1. [Client and connection](#1-client-and-connection)
2. [Session, variables, and settings](#2-session-variables-and-settings)
3. [DDL — databases, schemas, tables](#3-ddl--databases-schemas-tables)
4. [DML and bulk load](#4-dml-and-bulk-load)
5. [Transactions and distributed commit](#5-transactions-and-distributed-commit)
6. [Constraints and integrity](#6-constraints-and-integrity)
7. [Indexes](#7-indexes)
8. [Query planning and performance](#8-query-planning-and-performance)
9. [MVCC and maintenance](#9-mvcc-and-maintenance)
10. [Partitioning (declarative)](#10-partitioning-declarative)
11. [Logical replication](#11-logical-replication)
12. [Monitoring, locks, and catalog views](#12-monitoring-locks-and-catalog-views)
13. [Roles, security, and administration](#13-roles-security-and-administration)
14. [Extensions used in this repo](#14-extensions-used-in-this-repo)
15. [psql meta-commands quick index](#15-psql-meta-commands-quick-index)

---

## 1. Client and connection

Connect to a server, run a file, or open an interactive session.

### psql command-line flags

```bash
psql -h localhost -p 55432 -U lab -d lab          # connect
psql -h localhost -p 55432 -U lab -d lab -c "SELECT 1"   # one command, exit
psql -h localhost -p 55432 -U lab -d lab -f script.sql     # run file
psql -v rows=5000000 -f seed.sql                 # pass variable to script
psql -X                                          # no ~/.psqlrc (reproducible runs)
psql -P pager=off                                # disable pager for scripts
```

| Flag | Purpose |
|---|---|
| `-h` | host |
| `-p` | port |
| `-U` | user |
| `-d` | database |
| `-c` | run one SQL command and exit |
| `-f` | run SQL from file |
| `-v` / `--set` | set psql variable (`:name` in SQL) |
| `-X` | do not read startup file |
| `-P` | psql runtime option (e.g. `pager=off`) |

### This repo

```bash
scripts/psql.sh              # labs 00–03 (:55432)
scripts/psql.sh shard 2       # shard 2 (:55442)
scripts/psql.sh citus         # Citus coordinator (:55450)
scripts/psql.sh pub           # logical-replication publisher (:55460)
scripts/psql.sh sub           # logical-replication subscriber (:55461)
```

### psql session meta-commands

```text
\conninfo          -- show connection parameters
\c lab lab         -- reconnect (database, user)
\q                 -- quit
\?                 -- list meta-commands
\h CREATE TABLE    -- SQL command help
```

---

## 2. Session, variables, and settings

Change behaviour for the current session or cluster.

### Runtime settings

```sql
SET enable_partition_pruning = off;
SHOW enable_partition_pruning;
RESET enable_partition_pruning;

ALTER SYSTEM SET wal_level = 'logical';   -- written to postgresql.auto.conf
SELECT pg_reload_conf();                  -- reload GUCs that allow it (not wal_level)
```

`wal_level` and `shared_preload_libraries` require a **postmaster restart**, not
`pg_reload_conf()`.

### Lab-relevant GUCs (single mention each)

| GUC | Purpose | Used in |
|---|---|---|
| `enable_partition_pruning` | turn plan-time partition elimination on/off | lab 01 |
| `enable_partitionwise_aggregate` | per-partition partial aggregates | lab 01 |
| `plan_cache_mode = force_generic_plan` | force generic plan (run-time pruning demo) | lab 01 |
| `lock_timeout` | fail instead of waiting forever on a lock | lab 02 |
| `wal_level` | `replica` (default) vs `logical` (needed for logical decoding) | labs 06, 07 |
| `shared_preload_libraries` | e.g. `citus,pg_stat_statements` (order matters) | lab 06 |
| `citus.enable_repartition_joins` | allow expensive repartitioned joins | lab 06 |
| `citus.stat_statements_track` | must not be `none` for `citus_stat_statements` | lab 06 |

### psql session controls

```text
\set ON_ERROR_STOP on    -- abort script on first SQL error
\timing on              -- print elapsed time per statement
\x on                   -- expanded (vertical) output
\set rows 5000000        -- user variable; reference as :rows in SQL
```

---

## 3. DDL — databases, schemas, tables

Create and alter database objects.

### Databases and schemas

```sql
CREATE DATABASE mydb;
DROP DATABASE mydb;

CREATE SCHEMA IF NOT EXISTS partman;
DROP SCHEMA partman CASCADE;
```

### Tables, sequences, views

```sql
CREATE TABLE events (
  id            bigserial PRIMARY KEY,
  tenant_id     bigint      NOT NULL,
  occurred_at   timestamptz NOT NULL,
  amount_cents  bigint      NOT NULL
);

CREATE SEQUENCE audit_log_seq;

CREATE VIEW recent_events AS
  SELECT * FROM events WHERE occurred_at > now() - interval '7 days';

CREATE MATERIALIZED VIEW daily_totals AS
  SELECT date_trunc('day', occurred_at) AS d, sum(amount_cents) AS total
  FROM events GROUP BY 1;

DROP TABLE IF EXISTS events CASCADE;
DROP MATERIALIZED VIEW daily_totals;
```

### Alter table

```sql
ALTER TABLE events ADD COLUMN kind text NOT NULL DEFAULT 'purchase';
ALTER TABLE events DROP COLUMN kind;
ALTER TABLE events ALTER COLUMN kind TYPE text;
ALTER TABLE events RENAME COLUMN kind TO event_kind;
ALTER TABLE events RENAME TO events_archive;
```

### Copy table shape

```sql
CREATE TABLE events_p_2024_01 (LIKE events INCLUDING DEFAULTS INCLUDING CONSTRAINTS);
```

### Extensions

```sql
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE EXTENSION IF NOT EXISTS pg_partman SCHEMA partman;
DROP EXTENSION pg_partman CASCADE;
```

**Used in:** lab 00 (`pg_stat_statements`), lab 02 (`pg_partman`).

### Helpers used in lab SQL

```sql
SELECT to_regclass('public.events_p_default');   -- OID or NULL if missing
SELECT generate_series(1, 100000) AS g;
```

### psql object listing

```text
\l                 -- databases
\dn                -- schemas
\dt                -- tables in search_path
\dt public.*       -- tables in schema
\d events          -- describe table
\d+ events_p       -- describe with sizes, partitions, storage
\di                -- indexes
\df                -- functions
```

---

## 4. DML and bulk load

Read and write rows.

### SELECT

```sql
-- filters, ordering, limit
SELECT id, occurred_at FROM events
WHERE tenant_id = 7 AND occurred_at > now() - interval '30 days'
ORDER BY occurred_at DESC LIMIT 50;

-- joins
SELECT e.id, t.name FROM events e JOIN tenants t ON t.id = e.tenant_id;

-- aggregates
SELECT tenant_id, count(*), sum(amount_cents) FROM events GROUP BY tenant_id;

-- window functions
SELECT id, row_number() OVER (PARTITION BY tenant_id ORDER BY occurred_at DESC) AS rn
FROM events;

-- CTEs
WITH recent AS (SELECT * FROM events WHERE occurred_at > now() - interval '1 day')
SELECT count(*) FROM recent;

-- locking read
SELECT * FROM orders WHERE id = 42 FOR UPDATE;
```

**Used in:** labs 00–03 (measurement queries), lab 06 (cross-shard cost shapes).

### INSERT, UPDATE, DELETE, TRUNCATE

```sql
INSERT INTO events (tenant_id, occurred_at, amount_cents)
VALUES (1, now(), 100);

INSERT INTO events (tenant_id, occurred_at, amount_cents)
SELECT (g % 500) + 1, now(), (g * 7919) % 50000 FROM generate_series(1, 50000) g;

UPDATE events SET amount_cents = amount_cents + 1 WHERE tenant_id = 7;
DELETE FROM events WHERE occurred_at < now() - interval '300 days';
TRUNCATE events;                    -- fast empty; cannot roll back easily
```

**Used in:** lab 00 (`UPDATE` bloat, `DELETE` retention pain), lab 01 (cross-partition `UPDATE`).

### COPY and \copy

Server-side (superuser or `pg_read_server_files`):

```sql
COPY events TO '/tmp/events.csv' WITH (FORMAT csv, HEADER true);
COPY events FROM '/tmp/events.csv' WITH (FORMAT csv, HEADER true);
```

Client-side (runs on your machine, any role with table rights):

```text
\copy events TO 'events.csv' WITH (FORMAT csv, HEADER true)
\copy events FROM 'events.csv' WITH (FORMAT csv, HEADER true)
```

**Used in:** lab 01 step 5 (archive before drop — mentioned in comments).

### Prepared statements

```sql
PREPARE recent AS
  SELECT count(*) FROM events_p WHERE occurred_at > $1;
EXECUTE recent(now() - interval '3 days');
DEALLOCATE recent;
```

**Used in:** lab 01 (generic plan / run-time pruning).

---

## 5. Transactions and distributed commit

Group statements atomically; optionally survive session disconnect (2PC).

```sql
BEGIN;
-- ... work ...
COMMIT;

ROLLBACK;

SAVEPOINT sp1;
-- ... work ...
ROLLBACK TO sp1;
RELEASE SAVEPOINT sp1;
```

### Two-phase commit (across databases)

```sql
BEGIN;
UPDATE ledger SET balance = balance - 500 WHERE tenant_id = 1;
PREPARE TRANSACTION 'xfer-42-0';
-- session can disconnect; transaction stays prepared

COMMIT PREPARED 'xfer-42-0';
-- or
ROLLBACK PREPARED 'xfer-42-0';
```

Inspect prepared transactions: see [Monitoring](#12-monitoring-locks-and-catalog-views) (`pg_prepared_xacts`).

**Used in:** lab 04 (`internal/shard/txn.go`, `shardctl transfer 2pc`).

---

## 6. Constraints and integrity

Enforce data rules at the database boundary.

```sql
CREATE TABLE orders (
  id          bigint PRIMARY KEY,
  tenant_id   bigint NOT NULL,
  total_cents bigint NOT NULL CHECK (total_cents >= 0),
  state       text   NOT NULL
);

ALTER TABLE orders ADD CONSTRAINT orders_tenant_fk
  FOREIGN KEY (tenant_id) REFERENCES tenants(id);

ALTER TABLE orders ADD CONSTRAINT orders_tenant_id_uniq UNIQUE (tenant_id, id);
ALTER TABLE orders DROP CONSTRAINT orders_tenant_fk;
```

| Constraint | Notes |
|---|---|
| `PRIMARY KEY` | implies `UNIQUE NOT NULL` |
| `UNIQUE` | allows multiple NULLs unless `NOT NULL` |
| `FOREIGN KEY` | requires matching row in referenced table |
| `CHECK` | row-level predicate; cheap partition attach when bounds match |
| `EXCLUSION` | not supported on partitioned tables (lab 01 limitation) |

On partitioned tables, every `UNIQUE`/`PRIMARY KEY` must include the **partition
key**. See [Partitioning](#10-partitioning-declarative).

**Used in:** lab 01 (six limitations), lab 02 (`CHECK` for cheap `ATTACH`), lab 06 (distribution column in PK).

---

## 7. Indexes

Speed up lookups; support uniqueness and replica identity.

```sql
CREATE INDEX events_tenant_time_idx ON events (tenant_id, occurred_at DESC);
CREATE INDEX events_recent_idx ON events (occurred_at) WHERE occurred_at > now() - interval '90 days';
CREATE INDEX events_lower_kind_idx ON events (lower(kind));

DROP INDEX events_tenant_time_idx;
REINDEX INDEX CONCURRENTLY events_tenant_time_idx;
```

### CREATE INDEX CONCURRENTLY

Build without blocking writes (two scans, cannot run inside a transaction block):

```sql
CREATE INDEX CONCURRENTLY events_kind_idx ON events (kind);
```

### Partitioned-table index pattern

`CREATE INDEX CONCURRENTLY` does not work on a partitioned parent. Workaround:

```sql
-- on each child partition
CREATE INDEX CONCURRENTLY events_p_2024_01_kind_idx ON events_p_2024_01 (kind);

-- on parent (marks index valid on parent only)
CREATE INDEX events_p_kind_idx ON ONLY events_p (kind);

-- attach each child's index
ALTER INDEX events_p_kind_idx ATTACH PARTITION events_p_2024_01_kind_idx;
```

Replica identity via index: see [Logical replication](#11-logical-replication) (`REPLICA IDENTITY USING INDEX`).

**Used in:** labs 00–03 (measurement indexes), lab 01 step 4 (partitioned index limitation).

---

## 8. Query planning and performance

Understand what the planner will do before (or while) it runs.

### EXPLAIN

```sql
EXPLAIN SELECT count(*) FROM events WHERE tenant_id = 7;

EXPLAIN (ANALYZE, BUFFERS, TIMING, COSTS, VERBOSE, SUMMARY)
SELECT count(*), sum(amount_cents)
FROM events
WHERE occurred_at > now() - interval '7 days'
GROUP BY date_trunc('day', occurred_at);
```

| Option | What it adds |
|---|---|
| `ANALYZE` | actually runs the query; shows real row counts and timing |
| `BUFFERS` | shared/local buffer hit and read counts |
| `TIMING` | per-node execution time (off for stable micro-benchmarks) |
| `COSTS` | planner cost estimates (`COSTS OFF` for readable plans) |
| `VERBOSE` | output column names, join filters |
| `SUMMARY` | planning time summary |

`EXPLAIN ANALYZE` runs the query. `ANALYZE table` collects statistics — different
command; see [MVCC and maintenance](#9-mvcc-and-maintenance).

### pg_stat_statements

Requires extension — see [DDL extensions](#3-ddl--databases-schemas-tables). Usually also needs preload via `shared_preload_libraries`.

```sql
SELECT query, calls, mean_exec_time, total_exec_time
FROM pg_stat_statements
ORDER BY total_exec_time DESC
LIMIT 20;
```

**Used in:** lab 00 (extension created), lab 06 (`citus_stat_statements`).

### Citus plan reading (brief)

Look for `Task Count` in `EXPLAIN` output: `1` = single-shard fast path; higher =
scatter-gather. `Distributed Subplan` and `Intermediate Data Size` show cross-shard
data movement. Full Citus commands: [Extensions](#14-extensions-used-in-this-repo).

**Used in:** labs 00, 01, 03, 06.

---

## 9. MVCC and maintenance

Postgres never overwrites rows in place. Dead tuples accumulate; maintenance
reclaims space and updates planner statistics.

### VACUUM (single home for all variants)

```sql
VACUUM events;
VACUUM (VERBOSE) events;
VACUUM ANALYZE events;           -- vacuum + update stats in one pass
VACUUM FULL events;              -- rewrite table; ACCESS EXCLUSIVE; returns space to OS
```

| Variant | Locks | Returns space to OS | Typical use |
|---|---|---|---|
| `VACUUM` | ShareUpdateExclusive | no (reuses pages) | routine dead-tuple cleanup |
| `VACUUM VERBOSE` | same | no | see what was scanned/frozen |
| `VACUUM ANALYZE` | same | no | after large DML |
| `VACUUM FULL` | ACCESS EXCLUSIVE | yes | last resort; outage |

Prepared transactions and long-running sessions pin the **xmin horizon** and
block `VACUUM` from reclaiming newer dead tuples. See lab 04 exercise 8.

### ANALYZE (statistics only)

```sql
ANALYZE events;
ANALYZE events, orders;
```

Not the same as `EXPLAIN ANALYZE`. Run after bulk load or large DML.

### Dead tuples and size

```sql
SELECT relname, n_live_tup, n_dead_tup
FROM pg_stat_user_tables
WHERE relname = 'events';

SELECT pg_size_pretty(pg_table_size('events'))       AS heap,
       pg_size_pretty(pg_indexes_size('events'))    AS indexes,
       pg_size_pretty(pg_total_relation_size('events')) AS total;
```

Size functions are also listed under [Monitoring](#12-monitoring-locks-and-catalog-views).

### CLUSTER and freeze (mention)

```sql
CLUSTER events USING events_tenant_time_idx;   -- one-time physical reorder; exclusive lock
SELECT age(datfrozenxid) FROM pg_database WHERE datname = current_database();
```

**Used in:** lab 00 (bloat, `VACUUM`, retention `DELETE` debt).

---

## 10. Partitioning (declarative)

One logical table, many physical child tables. Planner **pruning** skips irrelevant
partitions when the query predicate allows it.

### Create partitioned table

```sql
CREATE TABLE events_p (
  id           bigint      NOT NULL,
  tenant_id    bigint      NOT NULL,
  occurred_at  timestamptz NOT NULL,
  amount_cents bigint      NOT NULL,
  PRIMARY KEY (id, occurred_at)   -- partition key must be in every UNIQUE/PK
) PARTITION BY RANGE (occurred_at);
```

Strategies: `RANGE`, `LIST`, `HASH`.

```sql
-- RANGE child
CREATE TABLE events_p_2024_01 PARTITION OF events_p
  FOR VALUES FROM ('2024-01-01') TO ('2024-02-01');

-- LIST child
CREATE TABLE orders_l_t1 PARTITION OF orders_l FOR VALUES IN (1);

-- HASH modulus (declared on parent)
CREATE TABLE orders_h (
  id bigint, tenant_id bigint NOT NULL, ...
) PARTITION BY HASH (tenant_id);
-- children: FOR VALUES WITH (MODULUS 8, REMAINDER 0) .. REMAINDER 7

-- DEFAULT catch-all
CREATE TABLE events_p_default PARTITION OF events_p DEFAULT;
```

**Used in:** labs 01 (`RANGE`), 02 (rollover), 03 (`HASH` + `LIST`).

### Attach and detach

```sql
-- create freestanding table, load it, add matching CHECK, then attach cheaply
CREATE TABLE events_p_2024_02 (LIKE events_p INCLUDING DEFAULTS);
ALTER TABLE events_p_2024_02 ADD CONSTRAINT ck
  CHECK (occurred_at >= '2024-02-01' AND occurred_at < '2024-03-01');
ALTER TABLE events_p ATTACH PARTITION events_p_2024_02
  FOR VALUES FROM ('2024-02-01') TO ('2024-03-01');

ALTER TABLE events_p DETACH PARTITION events_p_2024_01;
ALTER TABLE events_p DETACH PARTITION events_p_2024_01 CONCURRENTLY;
```

| Operation | Lock on parent | Notes |
|---|---|---|
| `ATTACH` without matching `CHECK` | ACCESS EXCLUSIVE | validates all rows |
| `ATTACH` with matching `CHECK` | brief | catalogue update only |
| `DETACH` | ACCESS EXCLUSIVE | blocks whole parent |
| `DETACH CONCURRENTLY` | none on parent | cannot run in a transaction; no `DEFAULT` partition |

Planner knobs: [Session settings](#2-session-variables-and-settings) (`enable_partition_pruning`, `enable_partitionwise_aggregate`).

### Retention pattern

```sql
ALTER TABLE events_p DETACH PARTITION events_p_2024_01 CONCURRENTLY;
DROP TABLE events_p_2024_01;
```

No dead tuples, no post-retention `VACUUM` debt. Compare to `DELETE` in lab 00.

**Used in:** labs 01 step 5, 02 (`retire_month_partitions`).

---

## 11. Logical replication

Row-level, selective replication between Postgres instances. Foundation for
zero-downtime shard moves (labs 05, 06, 07).

### Publisher

```sql
CREATE PUBLICATION orders_pub
  FOR TABLE orders, audit_log
  WITH (publish = 'insert,update,delete,truncate');

CREATE PUBLICATION shard_move_pub
  FOR TABLE orders (id, tenant_id, logical_shard, created_at, total_cents, state)
       WHERE (logical_shard >= 512 AND logical_shard < 768)
  WITH (publish = 'insert');

ALTER PUBLICATION orders_pub ADD TABLE new_table;
ALTER PUBLICATION orders_pub SET (publish = 'insert,update,delete');
DROP PUBLICATION orders_pub;
```

`FOR ALL TABLES` and `FOR TABLES IN SCHEMA public` (PG15+) replicate future tables
automatically — use with care.

`pubviaroot` (on publication / per table): publish through root partitioned table
instead of leaf partition names.

### Subscriber

```sql
CREATE SUBSCRIPTION orders_sub
  CONNECTION 'host=publisher port=5432 user=lab password=lab dbname=lab'
  PUBLICATION orders_pub
  WITH (copy_data = true);

ALTER SUBSCRIPTION orders_sub DISABLE;
ALTER SUBSCRIPTION orders_sub ENABLE;
ALTER SUBSCRIPTION orders_sub REFRESH PUBLICATION;
ALTER SUBSCRIPTION orders_sub SET (disable_on_error = true);
ALTER SUBSCRIPTION orders_sub SKIP (lsn = '0/1A2B3C8');   -- PG16+; loses whole remote txn
DROP SUBSCRIPTION orders_sub;
```

### Replica identity

Controls what the publisher writes to WAL for `UPDATE`/`DELETE`:

```sql
ALTER TABLE audit_log REPLICA IDENTITY FULL;
ALTER TABLE orders REPLICA IDENTITY DEFAULT;              -- PK columns
ALTER TABLE orders REPLICA IDENTITY USING INDEX orders_logical_uniq;
```

| Identity | WAL for old row | Cost |
|---|---|---|
| `DEFAULT` | PK columns (nothing if no PK) | lowest |
| `USING INDEX idx` | indexed unique columns | low; production choice |
| `FULL` | every column | highest WAL and apply cost |

Row filters on publications require replica-identity columns in the filter
expression.

### Slots and origins

```sql
SELECT pg_create_logical_replication_slot('my_slot', 'pgoutput');
SELECT pg_drop_replication_slot('my_slot');

-- subscriber only, subscription disabled first
SELECT pg_replication_origin_advance(
  'pg_16384', '0/1A2B3C8'::pg_lsn
);
```

### Sequences after failover

Sequences do not replicate. After promotion:

```sql
SELECT setval('audit_log_seq', (SELECT max(seq) FROM audit_log));
```

Monitoring views: [Monitoring](#12-monitoring-locks-and-catalog-views).

**Used in:** lab 07 (full walkthrough), lab 06 (`shard_transfer_mode => 'force_logical'`).

---

## 12. Monitoring, locks, and catalog views

Inspect live state, replication lag, and object metadata.

### Activity and locks

```sql
SELECT pid, state, wait_event_type, wait_event, query, query_start
FROM pg_stat_activity
WHERE datname = current_database() AND pid <> pg_backend_pid();

SELECT locktype, relation::regclass, mode, granted, pid
FROM pg_locks
WHERE NOT granted;

SELECT * FROM pg_blocking_pids(12345);
```

**Used in:** lab 02 (`pg_locks` during `DETACH`), lab 04 (orphan 2PC).

### Prepared transactions

```sql
SELECT gid, transaction AS xid, prepared, now() - prepared AS age
FROM pg_prepared_xacts;
```

### Replication (publisher side)

```sql
SELECT slot_name, active, restart_lsn,
       pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained_wal
FROM pg_replication_slots;

SELECT application_name, state, sent_lsn, write_lsn, flush_lsn, replay_lsn, replay_lag
FROM pg_stat_replication;
```

### Replication (subscriber side)

```sql
SELECT subname, pid, relid::regclass, received_lsn, latest_end_lsn
FROM pg_stat_subscription;

SELECT subname, srrelid::regclass, srsubstate, srsublsn
FROM pg_subscription_rel r
JOIN pg_subscription s ON s.oid = r.srsubid;

SELECT subname, apply_error_count, sync_error_count
FROM pg_stat_subscription_stats;

SELECT external_id, remote_lsn, local_lsn
FROM pg_replication_origin_status;
```

`srsubstate`: `i` initialize → `d` copying → `f` copied → `s` synchronized → `r` ready.

### Publications

```sql
SELECT pubname, pubinsert, pubupdate, pubdelete, pubtruncate, pubviaroot
FROM pg_publication;

SELECT pubname, schemaname, tablename, attnames, rowfilter
FROM pg_publication_tables;
```

### Partitioning catalog

```sql
SELECT c.relname, pg_get_expr(c.relpartbound, c.oid) AS bounds
FROM pg_class c
JOIN pg_inherits i ON i.inhrelid = c.oid
WHERE i.inhparent = 'events_p'::regclass
ORDER BY 1;
```

### WAL / LSN helpers

```sql
SELECT pg_current_wal_lsn();
SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0');
```

### Statistics hygiene

```sql
SELECT pg_stat_clear_snapshot();           -- unpin stats snapshot in a loop
SELECT pg_stat_reset_subscription_stats(); -- reset subscription error counters
```

### Size helpers

See [MVCC and maintenance](#9-mvcc-and-maintenance) (`pg_table_size`, `pg_indexes_size`, `pg_total_relation_size`, `pg_size_pretty`).

### psql monitoring helpers

```text
SELECT count(*) FROM events \watch 2    -- re-run every 2 seconds
```

Conditional execution (lab 01 retention):

```sql
SELECT 'ALTER TABLE events_p DETACH PARTITION events_p_default'
WHERE EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = 'events_p_default'::regclass)
\gexec
```

**Used in:** labs 00–07 (various measurement and observe scripts).

---

## 13. Roles, security, and administration

Manage access and operational tasks outside normal DML.

### Roles

```sql
CREATE ROLE appuser LOGIN PASSWORD 'secret';
CREATE ROLE readonly NOLOGIN;
ALTER ROLE appuser SET statement_timeout = '30s';
DROP ROLE appuser;
```

### Privileges

```sql
GRANT CONNECT ON DATABASE lab TO appuser;
GRANT USAGE ON SCHEMA public TO appuser;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO appuser;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO appuser;
REVOKE INSERT ON events FROM appuser;
```

### Backup and restore (operational)

```bash
pg_dump -h localhost -p 55432 -U lab -d lab -Fc -f lab.dump
pg_restore -h localhost -p 55432 -U lab -d lab_restore -Fc lab.dump
psql -h localhost -p 55432 -U lab -d lab -f schema.sql
```

### Checkpoint

```sql
CHECKPOINT;   -- force WAL flush; rarely needed manually
```

This repo uses a single `lab/lab` superuser in Docker; production role design is
out of scope here.

---

## 14. Extensions used in this repo

Commands beyond core Postgres, as exercised in the labs.

### pg_partman (lab 02)

Install extension first — see [DDL extensions](#3-ddl--databases-schemas-tables).

```sql
SELECT partman.create_parent(
  p_parent_table    => 'public.metrics',
  p_control         => 'bucket',
  p_interval        => '1 day',
  p_premake         => 4,
  p_start_partition => (current_date - 30)::text
);

UPDATE partman.part_config
SET retention = '14 days',
    retention_keep_table = true,
    infinite_time_partitions = true
WHERE parent_table = 'public.metrics';

CALL partman.run_maintenance_proc();
```

Teardown before re-run:

```sql
DELETE FROM partman.part_config WHERE parent_table = 'public.metrics';
-- drop detached children and parent separately
```

### Citus (lab 06)

```sql
SELECT create_distributed_table('orders', 'tenant_id', shard_count => 32);
SELECT create_distributed_table('order_items', 'tenant_id', colocate_with => 'orders');
SELECT create_reference_table('currencies');

SELECT update_distributed_table_colocation('shipments', colocate_with => 'orders');

SELECT isolate_tenant_to_new_shard('orders', 42, 'CASCADE',
  shard_transfer_mode => 'force_logical');

SELECT citus_rebalance_start();
SELECT * FROM citus_rebalance_status();
SELECT citus_rebalance_stop();
```

Useful catalogs:

```sql
SELECT logicalrelid, partmethod,
       column_to_column_name(logicalrelid, partkey) AS distribution_column
FROM pg_dist_partition;

SELECT shardid, shardminvalue, shardmaxvalue
FROM pg_dist_shard WHERE logicalrelid = 'orders'::regclass;
```

`shard_transfer_mode`:

| Value | Behaviour |
|---|---|
| `block_writes` | lock source shard for full copy |
| `force_logical` | logical replication; brief lock at cutover; needs `wal_level = logical` on workers |

`pg_stat_statements` preload: see [Query planning](#8-query-planning-and-performance).

**Used in:** labs 02 (`pg_partman`), 06 (Citus).

---

## 15. psql meta-commands quick index

Alphabetical reference. SQL examples live in the sections above — not repeated here.

| Command | Purpose | Primary section |
|---|---|---|
| `\?` | meta-command help | [Client](#1-client-and-connection) |
| `\c db [user]` | connect / reconnect | [Client](#1-client-and-connection) |
| `\conninfo` | connection parameters | [Client](#1-client-and-connection) |
| `\copy` | client-side COPY | [DML](#4-dml-and-bulk-load) |
| `\d [name]` | describe table/view/index/sequence | [DDL](#3-ddl--databases-schemas-tables) |
| `\d+ [name]` | describe with extra detail | [DDL](#3-ddl--databases-schemas-tables) |
| `\df` | list functions | [DDL](#3-ddl--databases-schemas-tables) |
| `\di` | list indexes | [DDL](#3-ddl--databases-schemas-tables) |
| `\dn` | list schemas | [DDL](#3-ddl--databases-schemas-tables) |
| `\dt [pattern]` | list tables | [DDL](#3-ddl--databases-schemas-tables) |
| `\echo text` | print message in script | [Monitoring](#12-monitoring-locks-and-catalog-views) |
| `\g` | execute query buffer | — |
| `\gexec` | execute query output as SQL | [Monitoring](#12-monitoring-locks-and-catalog-views) |
| `\h COMMAND` | SQL syntax help | [Client](#1-client-and-connection) |
| `\i file` | include/run SQL file | [Client](#1-client-and-connection) |
| `\l` | list databases | [DDL](#3-ddl--databases-schemas-tables) |
| `\o file` | send query output to file | — |
| `\q` | quit | [Client](#1-client-and-connection) |
| `\set name value` | set psql variable | [Session](#2-session-variables-and-settings) |
| `\timing` | show per-statement timing | [Session](#2-session-variables-and-settings) |
| `\watch [sec]` | re-execute last query periodically | [Monitoring](#12-monitoring-locks-and-catalog-views) |
| `\x` | expanded output on/off | [Session](#2-session-variables-and-settings) |

---

## Lab index

| Lab | Commands covered in this doc |
|---|---|
| 00 baseline | `EXPLAIN`, `VACUUM`, `DELETE`, size functions, `pg_stat_user_tables` |
| 01 range partitioning | `PARTITION BY RANGE`, pruning GUCs, `ATTACH`/`DETACH`, index workaround |
| 02 retention | cheap `ATTACH`, rollover plpgsql, `pg_partman` |
| 03 hash/list | `PARTITION BY HASH`/`LIST`, skew queries |
| 04 app sharding | `PREPARE TRANSACTION`, `pg_prepared_xacts`, scatter-gather (app layer) |
| 05 resharding | dual-write/backfill concepts; logical replication primitive in lab 07 |
| 06 Citus | `create_distributed_table`, colocation, rebalance, `isolate_tenant_to_new_shard` |
| 07 logical replication | publications, subscriptions, replica identity, slots, `setval` |
