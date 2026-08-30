# Lab 00 -- Baseline: one big table and the bill it runs up

Everything in this repo is an argument that you should split data up. This lab is the
control group. You build the table every product actually starts with -- one flat
`events` heap, tenant id, timestamp, status, jsonb payload, three sensible indexes --
and then you measure the two operations that eventually force the redesign: reclaiming
space after churn, and deleting old rows. Nothing here is a mistake. The schema is
correct, the indexes are the ones you would have chosen, and the queries are fast right
up until they are not. The point is to write down the numbers, because every later lab
is scored against them.

## Run it

```bash
make lab00
```

The default is 5,000,000 rows. That is enough to feel the shape. To make it
unmistakable, push the volume up -- the seed is the slow part, budget accordingly:

```bash
ROWS=20000000 make lab00
```

To re-run one step by hand without reseeding (note that `runlab.sh` normally supplies
`-v rows`, so you pass it yourself):

```bash
scripts/psql.sh lab -v ON_ERROR_STOP=1 -v rows=5000000 -f labs/00-baseline/04-vacuum-pain.sql
```

An interactive session on the same database:

```bash
make psql
```

## The steps

### 01-schema.sql -- the naive table

Creates `events` with a `bigserial` primary key and three indexes: `(tenant_id,
occurred_at DESC)` for the API query, `(occurred_at)` for the reporting query, and a
partial index on `status` for the work-queue query. Also creates `pg_stat_statements`.

What to notice: `id bigserial PRIMARY KEY` works here. Hold onto that, because in
[lab 01](../01-range-partitioning/README.md) it stops being legal and that single
change cascades into your application's uniqueness assumptions.

### 02-seed.sql -- load the rows

Inserts `:rows` rows spread over the last 365 days, with `power(random(), 3)` tenant
skew so a handful of tenants own most of the table. That skew is not decoration -- it is
what makes [lab 03](../03-hash-list-multitenant/README.md) and the resharding labs
non-trivial. Ends with `ANALYZE`.

What to notice: the wall clock on the `INSERT`. You will compare it against the same
insert into a 14-partition table in lab 01, where routing costs something and smaller
per-partition B-trees give something back.

### 03-measure.sql -- what does one big table cost

Prints the size breakdown, per-index sizes, then `EXPLAIN (ANALYZE, BUFFERS, TIMING)`
for three queries.

What to notice:

- The index total is the same order of magnitude as the heap. On a 1M-row run of this
  exact schema the split came out around 112 MB heap against 90 MB of indexes. Whether
  indexes exceed the heap depends on your column widths; what matters is that index
  bytes are a large second cost, and no `DELETE` ever gives them back.
- **Q1**, the 7-day report across all tenants, has to consult a 12-month index to find
  1.9% of the rows. Read `Buffers: shared hit/read` on the scan node, not just the time
  -- a warm cache hides the problem that a cold one will not.
- **Q2**, one tenant plus a 30-day bound with `LIMIT 50`, is an `Index Scan using
  events_tenant_time_idx` and it is genuinely fast. This is why the pain arrives late:
  your p99 API latency stays flat for two years while the maintenance cost compounds
  underneath it.
- **Q3**, `count(*)`, is a `Parallel Seq Scan` over the entire heap. It will never be
  fast, at any table size, in any of the later labs. Partitioning does not fix it;
  partitionwise aggregation only parallelises it.

### 04-vacuum-pain.sql -- MVCC, and why an UPDATE grows the table

Postgres does not overwrite a row. An `UPDATE` writes a whole new tuple version and
marks the old one dead; the dead version stays on the page, still indexed, until
`VACUUM` reclaims the line pointer. This step updates roughly 10% of rows and then runs
`VACUUM (VERBOSE, ANALYZE)`.

What to notice, in order:

- `n_live_tup` is unchanged, `n_dead_tup` jumps to about 10% of the table, and
  `pg_table_size` grew by about the same 10%. Measured on a 1M-row run: 112 MB before
  the update, 123 MB after. You changed one column on one row in ten and the file got
  ten percent bigger.
- The indexes grew too. Non-HOT updates insert a new index entry into every index whose
  columns are not covered by the fill factor's free space, and `status` is indexed, so
  this update could not be HOT.
- In the `VACUUM VERBOSE` output, the line that carries the lesson is:

  ```
  pages: 0 removed, 15715 remain, 15715 scanned (100.00% of total)
  ```

  100% of the table scanned to clean up a 10% change. The visibility map spares VACUUM
  pages that are all-visible and untouched, but a 10% update spread by `id % 10 = 0`
  dirties nearly every page. The companion line said `index scan needed: 14286 pages
  from table (90.91% of total)`. This is the whole argument: **VACUUM cost tracks table
  size and page spread, not the size of your change.** A 500M-row table with a 10%
  update is not a 10% job, it is a full pass over half a terabyte plus a pass over every
  index.
- `removable cutoff: N, which was 0 XIDs old when operation ended` and `M are dead but
  not yet removable`. That cutoff is the xmin horizon: VACUUM may only remove tuple
  versions invisible to every snapshot that could still exist. Anything holding an old
  snapshot pins it -- a long-running `REPEATABLE READ` report, an idle-in-transaction
  connection, a replication slot with no consumer (see
  [lab 07](../07-logical-replication/)), a standby with `hot_standby_feedback = on`, and
  most treacherously a `PREPARE TRANSACTION` that nobody ever committed or rolled back.
  A prepared transaction survives client disconnects and server restarts, so its xid
  holds the horizon indefinitely; VACUUM keeps running, keeps scanning everything, and
  reclaims nothing while `dead but not yet removable` climbs. That is why
  `max_prepared_transactions=10` in `docker-compose.yml` carries a warning comment, and
  why [lab 04](../04-app-sharding-go/) makes you go find orphaned prepared transactions
  by hand.
- The last query prints the heap size after VACUUM. It did not shrink. Measured: 123 MB
  before, 123 MB after. VACUUM makes space **reusable by this table**; it returns pages
  to the OS only when the free pages happen to sit at the physical end of the file.

The three ways to actually give the space back, and what each one takes:

| Method | Lock | Extra disk | Notes |
| --- | --- | --- | --- |
| `VACUUM` | `SHARE UPDATE EXCLUSIVE` | none | Concurrent with reads and writes. Does not shrink the file except by truncating trailing empty pages. |
| `VACUUM FULL` | `ACCESS EXCLUSIVE` | a full second copy | Rewrites the table and every index. No reads, no writes, for the whole rewrite. Unusable on a large hot table. |
| `pg_repack` | brief `ACCESS EXCLUSIVE` at start and at swap | a full second copy plus a log table | Copies into a new table while a trigger captures concurrent changes, then swaps. Not in this lab image; know that it exists and that it needs a superuser-installed extension and enough headroom. |

On the same 1M-row run, `VACUUM FULL` took the heap from 123 MB to 92 MB and the
indexes from 94 MB to 61 MB, in about 1.9 s of exclusive lock. Multiply that lock
duration by your real table size and decide whether you are ever going to run it.

### 05-delete-pain.sql -- retention, the expensive way

Counts the rows older than 300 days, deletes them with `EXPLAIN (ANALYZE, BUFFERS)`,
then vacuums.

What to notice:

- A `DELETE` does not remove anything. It writes an xmax on every matched tuple. The
  row count drops, the file does not. On the 1M-row run this deleted 177,453 rows
  (17.7% of the table) and `pg_table_size` went from 123 MB to 123 MB.
- After the `VACUUM` that follows, still 123 MB. The dead space is now reusable by
  future inserts into `events`, which is useful only if `events` is still growing. If
  you delete on a schedule and insert at a steady rate, you are permanently carrying a
  retention window's worth of dead space.
- Every index entry for every deleted row also has to be found and removed. That is the
  `index scan needed` line again, and it is why the VACUUM after a bulk delete costs
  more than the delete.
- Read the `EXPLAIN` output for the delete itself. The scan is only part of the time --
  the rest is WAL. A bulk delete writes a WAL record per tuple, which is replication lag
  on every standby and, in a managed environment, real money.

**This is the baseline.** Write the delete time, the vacuum time, and the heap size into
the table at the bottom. In [lab 01](../01-range-partitioning/README.md) the same
retention job is a `DETACH` plus a `DROP TABLE`, and the comparison is the entire
argument for partitioning.

## What you should be able to answer afterwards

1. An `UPDATE` that changes one non-indexed column on 10% of rows grew the heap by 10%.
   Walk through why, in terms of tuple versions and line pointers, and explain the one
   mechanism (HOT) that would have avoided the index writes and what disqualified this
   update from using it.
2. The heap does not shrink after `DELETE` and does not shrink after the `VACUUM` that
   follows it. What are the three ways to actually return the bytes to the filesystem,
   what lock does each take, and how much spare disk does each need?
3. `VACUUM VERBOSE` reported `15715 scanned (100.00% of total)` for a 10% change.
   Explain what the visibility map is supposed to buy you, and why this particular
   update pattern defeated it. What update pattern would have kept the scan small?
4. A `VACUUM` finishes reporting a large number of tuples `dead but not yet removable`.
   Name five distinct things that could be pinning the xmin horizon, and say which of
   them survive a client disconnect and which survive a server restart.
5. Q2 (one tenant, 30-day window, `LIMIT 50`) is fast on this table and will still be
   fast at 500M rows. Given that, what is the actual failure that forces a redesign, and
   roughly when in a table's life does it arrive?
6. `pg_indexes_size` is close to `pg_table_size` here. Which operations in this lab paid
   that cost twice, and which one gave any of it back?
7. You need to delete 200M rows from a 2B-row table in production without a maintenance
   window. Describe how you would do it on this schema, and state the two limits you
   would have to tune the batch size against.
8. Why does `SELECT count(*)` stay slow no matter what you do in the rest of this
   repo -- and which specific technique in the later labs makes it merely parallel
   rather than fast?

## Traps

- `DELETE` is not a space-reclamation strategy. It is a write amplifier that also
  creates a VACUUM obligation larger than itself.
- Autovacuum is not "handled". At scale its default cost limits mean it falls behind
  during exactly the bulk operations that produce the most dead tuples.
- `VACUUM FULL` on a hot table is an outage with a progress meter, not a maintenance
  command.
- Fast tenant-scoped queries are not evidence that the table is fine. Measure the
  maintenance operations, not the p99.
- Do not benchmark on a warm cache and call it a result. Compare `Buffers: shared read`,
  not just `actual time`.
- A forgotten prepared transaction or an unconsumed replication slot silently converts
  every future VACUUM into an expensive no-op.
- Index bloat is a separate problem from heap bloat and `VACUUM` does not fix it either.
- `reltuples` and `n_live_tup` are estimates. If a number matters, `count(*)` it.

## Timings to record

Fill this in, then fill in the matching table in
[lab 01](../01-range-partitioning/README.md). The last three rows are the comparison
that justifies the rest of the repo.

| Measurement | Value at ROWS=1000000 |
| --- | --- |
| Seed time (`02-seed.sql` INSERT) | 14.549 s |
| Heap size after seed | 112 MB |
| Index size after seed | 90 MB |
| Q1 -- 7-day report, execution time | 35.8 ms (`EXPLAIN ANALYZE`; `\timing` 39.3 ms) |
| Q1 -- `Buffers: shared read` | 0 on the query (`shared hit=10678`; planning `read=3`). Warm cache. |
| Q2 -- one tenant, 30 days, execution time | 11.1 ms (`EXPLAIN ANALYZE`; planning 11.2 ms) |
| Q3 -- `count(*)`, execution time | 678 ms (`EXPLAIN ANALYZE`; `\timing` 704 ms) |
| Heap size after 10% UPDATE | 123 MB (indexes 95 MB) |
| `n_dead_tup` after 10% UPDATE | 100000 |
| `VACUUM` time after the UPDATE | 1.73 s (`VACUUM` + `ANALYZE`; heap vacuum elapsed 1.49 s) |
| Pages scanned / pages total in `VACUUM VERBOSE` | 15736 / 15736 (100%) |
| **Retention: rows deleted** | 178811 |
| **Retention: DELETE time** | 446 ms (`EXPLAIN ANALYZE`; `\timing` 453 ms) |
| **Retention: VACUUM time after the DELETE** | 477 ms (see note) |
| **Heap size after retention + VACUUM** | 123 MB (unchanged) |

The post-`DELETE` vacuum scanned 1 of 15736 pages (`0.01%`) and removed 0 tuples:
autovacuum had already reclaimed the dead line pointers during the `ANALYZE` that
the script runs first. The number to keep is still the heap size: **123 MB after
deleting 18% of the rows**. That vacuum was cheap because the corpses were already
gone, not because `DELETE` is cheap.

Next: [Lab 01 -- Range partitioning and pruning](../01-range-partitioning/README.md)
