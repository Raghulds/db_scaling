# Lab 01 -- Range partitioning, pruning, and what it costs you

Same columns as [lab 00](../00-baseline/README.md), one keyword different:
`PARTITION BY RANGE (occurred_at)`. This lab exists to make two things concrete at the
same time. The payoff: retention stops being a `DELETE` and becomes a `DROP TABLE`, and
time-bounded queries stop reading a twelve-month index. The bill: the partition key is
dragged into every unique constraint, a query that does not mention the partition key
gets measurably *slower* than it was on the flat table, and the planner now pays for
every partition on every query. Partitioning is a trade, not an upgrade, and the second
half of this lab is a list of failures you should read the error text of.

## Run it

```bash
make lab01
```

Use the same `ROWS` you used for lab 00 or the comparison is meaningless:

```bash
ROWS=20000000 make lab01
```

One step by hand (`runlab.sh` normally supplies `-v rows`, so pass it yourself):

```bash
scripts/psql.sh lab -v ON_ERROR_STOP=1 -v rows=5000000 -f labs/01-range-partitioning/03-pruning.sql
```

`04-limitations.sql` deliberately sets `ON_ERROR_STOP off` and is expected to raise
errors. That is the content, not a failure of the lab.

## The steps

### 01-schema.sql -- the partitioned parent

Creates `events_p` partitioned by range on `occurred_at`, two indexes on the parent, 14
monthly partitions (13 back, 1 ahead) built in a `DO` loop, and a `DEFAULT` partition.

What to notice:

- `PRIMARY KEY (id, occurred_at)`. Not a style choice -- Postgres requires that every
  unique constraint on a partitioned table include all partition key columns, because a
  unique index is per-partition and there is no cross-partition uniqueness check. The
  consequences are not cosmetic:
  - `WHERE id = ?` alone can never prune. It becomes an index scan on every partition.
  - You can no longer enforce global uniqueness on an external identifier -- an idempotency
    key, a Stripe event id, a webhook delivery id -- inside this table. If you need that,
    it lives in a separate small unpartitioned table that you insert into first, or it
    does not get enforced by the database at all.
  - A foreign key *pointing at* this table needs the full `(id, occurred_at)`, so every
    child table now carries the timestamp too.
- Indexes created on the parent are cloned to every existing and future partition. Create
  them once, on the parent. Never hand-create indexes per partition, or a partition
  created at 00:00 on the 1st will be missing one.
- The loop runs `-13..1`. That single partition of headroom is the bug you find in
  production, and [lab 02](../02-retention-rollover/README.md) is about not having it.
- The `DEFAULT` partition prevents insert errors for out-of-range timestamps. It is not
  free, and step 4 shows the bill.

### 02-seed.sql -- the same data, the same volume

Identical distribution to lab 00, into `events_p`. Prints approximate rows and total size
per partition.

What to notice:

- Compare the insert wall clock against lab 00. Tuple routing costs something per row;
  maintaining fourteen smaller B-trees instead of one large one gives something back
  (shallower trees, better cache locality on the hot months). Which wins depends on your
  row count and how much of the index fits in `shared_buffers`. Record both numbers
  rather than assuming.
- `events_p_default` should be empty. If it is not, some row's timestamp fell outside
  every range and you now have a partition that is expensive to attach around -- see L3
  below.
- Per-partition sizes should be roughly even, because the seed spreads uniformly over
  365 days. Real event streams grow, so real partitions are not even, and the newest one
  is the hot one.

### 03-pruning.sql -- the entire payoff, and five ways to read it

Five queries plus a planning-cost probe.

**Q1 -- bounded window.** The file's comment calls this plan-time pruning. Read the
actual output, because it is more interesting than the comment. The bound is
`date_trunc('month', now())`, and `now()` is `STABLE`, not immutable -- its value is not
known when the plan is built. So the plan keeps the `Append`, and you get:

```
Append (actual time=0.008..9.714 rows=23357 loops=1)
  Subplans Removed: 15
  ->  Seq Scan on events_p_2026_08 ...
```

Fifteen of sixteen children discarded at execution start. The result is right, one
partition is touched, but the *planning* still considered all sixteen. To see genuine
plan-time pruning, paste the same query with literal bounds:

```bash
scripts/psql.sh lab -c "EXPLAIN (COSTS OFF) SELECT count(*) FROM events_p WHERE occurred_at >= '2026-08-01' AND occurred_at < '2026-09-01';"
```

There is no `Append` node at all -- just `Seq Scan on events_p_2026_08`. That is the
difference: plan-time pruning removes children before the plan exists, run-time pruning
removes them after. Both give the right answer; only the first one makes planning cheap,
and only the first one shows a small plan in `auto_explain` logs.

**Q2 -- the mistake.** `WHERE tenant_id = 7 ORDER BY occurred_at DESC LIMIT 50`, no time
bound. You get a `Merge Append` over **one `Index Scan` per partition**, sixteen of them:

```
Limit
  ->  Merge Append
        Sort Key: events_p.occurred_at DESC
        ->  Index Scan using events_p_2025_07_tenant_id_occurred_at_idx on events_p_2025_07
        ->  Index Scan using events_p_2025_08_tenant_id_occurred_at_idx on events_p_2025_08
        ... 14 more ...
```

On the flat table in lab 00 this was one index scan. Partitioning made it worse: sixteen
B-tree descents, sixteen buffer pins, sixteen sets of index pages competing for cache,
and a merge on top. It still returns in well under a millisecond at lab scale, which is
exactly the trap -- at 60 monthly partitions and real concurrency it is the query that
takes the site down. **This is the single most common partitioning regret: an access
path that does not carry the partition key.**

**Q2b** adds `occurred_at >= now() - interval '20 days'` and collapses back to one or two
partitions. The lesson is not "add a time bound to this query" -- it is that partitioning
by time is only correct if *every* hot access path is time-bounded. If your API lets a
user page back through all of a tenant's history, range partitioning by time is the wrong
key for that table, and [lab 03](../03-hash-list-multitenant/README.md) is where you go
instead.

**Q3 -- the counterfactual.** `SET enable_partition_pruning = off` shows what the same
query costs with every partition scanned. Use the ratio against Q1 as your honest
statement of what pruning is worth on your data.

**Q4 -- run-time pruning with a generic plan.** `PREPARE` plus
`plan_cache_mode = force_generic_plan` is how your application actually runs: pgx and
every other driver use extended-protocol prepared statements, and after five executions
Postgres may switch to a generic plan with `$1` placeholders. A generic plan cannot prune
at plan time -- the parameter is unknown -- so pruning must happen in the executor. Look
for:

```
Parallel Append
  Subplans Removed: 13
```

If that line is missing from a plan you expected to prune, pruning is not happening and
you are scanning everything. Note that `Subplans Removed` only appears under
`EXPLAIN ANALYZE` for executor pruning; a plain `EXPLAIN` of a generic plan shows the
children that survived planning.

**Q5 -- partitionwise aggregate.** Off (the default), the plan is one `HashAggregate`
over an `Append` of every partition -- one hash table holding every tenant. On, each
partition gets its own `Partial HashAggregate` and a `Finalize HashAggregate` merges
them:

```
Finalize HashAggregate
  Group Key: events_p.tenant_id
  ->  Append
        ->  Partial HashAggregate  (Seq Scan on events_p_2025_08)
        ->  Partial HashAggregate  (Seq Scan on events_p_2025_09)
        ...
```

Smaller hash tables mean less chance of spilling to disk (watch `Batches:` and
`Memory Usage:` on the aggregate nodes), and each partial can be a separate parallel
worker. It is off by default because with many partitions it multiplies planning work
and, when the grouping key has high cardinality across every partition, the finalize step
does all the work anyway -- as here, where all 500 tenants appear in every month, so the
partials do not reduce anything.

Its sibling, `enable_partitionwise_join`, is also off by default and matters more. It
lets Postgres join partition-to-partition instead of joining two full `Append`s, but only
when both sides are partitioned on the join key with **identical** bounds. That
"identical bounds" requirement is the same colocation rule you will meet as an explicit
concept in [lab 06](../06-citus/), and the same rule your application-level sharding has
to enforce by hand in [lab 04](../04-app-sharding-go/). Getting colocation right is what
makes distributed joins possible at all.

**Planning cost.** The final `EXPLAIN (ANALYZE, SUMMARY ON)` exists so you look at
`Planning Time` and, on Q1, at the `Planning:` buffer line:

```
Planning:
  Buffers: shared hit=574 read=6
Planning Time: 1.844 ms
```

574 buffers touched to plan a query that reads 219. The planner opens and locks every
partition it has not yet pruned. That is roughly linear in partition count, it happens on
every plan of every query, and it is the reason "just use daily partitions and keep three
years" is a bad default. A few hundred partitions is fine. A few thousand is a planning
problem before it is a storage problem.

### 04-limitations.sql -- the walls, each one deliberately hit

Every block here is expected to fail. Read the error text; these are the constraints that
decide whether a table can be partitioned at all.

- **L1** -- `CREATE UNIQUE INDEX ... ON events_p (id)`:

  ```
  ERROR:  unique constraint on partitioned table must include all partitioning columns
  DETAIL:  UNIQUE constraint on table "events_p" lacks column "occurred_at" which is
           part of the partition key.
  ```

  So: **no global uniqueness on an external id, ever.** Design around it before you
  partition, not after.

- **L2** -- with the `DEFAULT` partition detached, inserting a 1999 row:

  ```
  ERROR:  no partition of relation "events_p" found for row
  DETAIL:  Partition key of the failing row contains (occurred_at) = (1999-01-01 00:00:00+00).
  ```

  This is what a missing future partition looks like at midnight on the 1st: not slow,
  not degraded -- every insert fails. **L2b** reattaches `DEFAULT` and the same insert
  succeeds.

- **L3** -- the `DEFAULT` partition's hidden cost. The script moves the 1999 rows into a
  standalone table and attaches it for that range. Because a `DEFAULT` partition exists,
  Postgres must prove that no row currently sitting in `DEFAULT` belongs in the new range,
  so it **scans the entire `DEFAULT` partition under `ACCESS EXCLUSIVE`** before the
  attach can commit. `\timing` is on for this block for a reason. On an empty `DEFAULT`
  it is instant. On a `DEFAULT` that quietly absorbed six months of misrouted rows it is
  an outage, and it happens during the routine deploy that adds next month's partition.
  Keep `DEFAULT` empty, and alert on `count(*)` in it.

- **L4** -- an `UPDATE` that moves a row across partitions. Legal since PG11, and
  implemented as a `DELETE` in the old partition plus an `INSERT` in the new one. The row
  gets a new `ctid`, you pay full index maintenance on both sides, `BEFORE UPDATE`
  triggers fire in one partition while `BEFORE INSERT`/`AFTER INSERT` fire in another, and
  a cursor holding the old ctid loses the row. Backdating a timestamp is not a cheap
  update on a partitioned table.

- **L5** -- `CREATE INDEX CONCURRENTLY` on the parent:

  ```
  ERROR:  cannot create index on partitioned table "events_p" concurrently
  ```

  The workaround is three steps and it is the one you will actually run in production:
  `CREATE INDEX CONCURRENTLY` on each partition individually, then
  `CREATE INDEX ... ON ONLY events_p (...)` on the parent (which creates an invalid,
  empty parent index without touching data), then
  `ALTER INDEX <parent_idx> ATTACH PARTITION <child_idx>` for each child. The parent index
  flips to valid automatically once every partition's index is attached.

- **L6** -- exclusion constraints:

  ```
  ERROR:  exclusion constraints are not supported on partitioned tables
  ```

  If you use a GiST exclusion constraint to stop overlapping bookings or overlapping
  validity ranges, that table cannot be partitioned. Same underlying reason as L1: the
  constraint would have to be enforced across partitions.

### 05-retention.sql -- the payoff, timed

Prints the size of the 13-month-old partition, then `DETACH PARTITION ... CONCURRENTLY`
and `DROP TABLE`, then checks the damage.

What to notice:

- `DETACH ... CONCURRENTLY` does not take `ACCESS EXCLUSIVE` on the parent. It works in
  two internal transactions: mark the partition as being detached and wait for every
  transaction that could still see it in the parent to finish, then finish the detach.
  Because it waits and because it commits in the middle, **it cannot run inside a
  transaction block**:

  ```
  ERROR:  ALTER TABLE ... DETACH CONCURRENTLY cannot run inside a transaction block
  ```

  That is why the script drives it through `\gexec` rather than a `DO` block -- a `DO`
  block is a transaction. The same restriction is why the plpgsql helper in
  [lab 02](../02-retention-rollover/README.md) has to use the plain, locking form.
  Corollary: if a detach is interrupted you can be left with a partition in
  `pg_class.relispartition = false` but still pending, and you finish it with
  `ALTER TABLE ... DETACH PARTITION ... FINALIZE`.

- After the `DROP`, `sum(n_dead_tup)` across the remaining partitions is zero and the
  total size dropped by the full partition size. No dead tuples were created, no VACUUM
  is owed, and the space went back to the filesystem immediately -- `DROP TABLE` unlinks
  files, it does not rewrite anything.

- Put the two numbers side by side. Lab 00: a `DELETE` measured in seconds to minutes,
  a `VACUUM` that costs more than the delete, and a heap that never shrank. Lab 01:
  milliseconds, and the bytes are gone. **That delta is the argument.**

## What you should be able to answer afterwards

1. Why must every unique constraint include the partition key, and what are the three
   concrete things that stops you doing (external ids, single-column primary key lookups,
   foreign keys pointing in)? Where does the idempotency key live now?
2. Q1 shows `Subplans Removed: 15` even though it "should" be plan-time pruning. Explain
   why `now()` produces run-time pruning, what the plan looks like with a literal bound
   instead, and why the difference matters to something other than this one query.
3. Q2 is slower after partitioning than it was in lab 00. Describe the plan node by node,
   say what the cost is proportional to, and give the two ways to fix it -- one that keeps
   range partitioning and one that abandons it.
4. Your ORM uses server-side prepared statements. Which of the two pruning mechanisms are
   you relying on, what single line in `EXPLAIN ANALYZE` confirms it is working, and what
   do you check if that line is absent?
5. What exactly does a `DEFAULT` partition cost at `ATTACH` time, which lock is held while
   that cost is paid, and what is the monitoring query that tells you the cost is growing?
6. `DETACH CONCURRENTLY` cannot run in a transaction block. Explain what it does
   internally that makes that impossible, what it buys you over the plain form, and how
   you recover from an interrupted detach.
7. You have 500 daily partitions and a query whose `Planning Time` exceeds its
   `Execution Time`. What is the planner doing, and what are the two structural fixes?
8. `enable_partitionwise_join` is off by default. State the precise condition under which
   it can fire, and name the concept in lab 06 that is the same requirement under a
   different word.

## Traps

- Partitioning is not an optimisation. It is a trade: cheap retention and pruned scans in
  exchange for weaker constraints, more planning, and one new way to break inserts.
- Any hot query that does not carry the partition key gets slower. Enumerate your access
  paths before you choose the key, not after.
- You cannot have global uniqueness on a non-partition-key column. Decide where that
  invariant lives before you migrate.
- One month of headroom is not headroom. Inserts fail outright, they do not degrade.
- A `DEFAULT` partition that is not empty turns every future `ATTACH` into a full scan
  under `ACCESS EXCLUSIVE`.
- `CREATE INDEX CONCURRENTLY` does not work on the parent. Learn the three-step
  per-partition dance before you need it at 2am.
- Backdating a timestamp is a cross-partition row move: delete plus insert, new ctid, both
  sets of triggers.
- More partitions is not more better. Planning cost is roughly linear in partition count
  and is paid by every query.
- Never create indexes on individual partitions by hand. Create them on the parent so new
  partitions inherit them.

## Timings to record

Same `ROWS` as [lab 00](../00-baseline/README.md). The bold rows are the direct
comparison.

| Measurement | Value at ROWS=______ |
| --- | --- |
| Seed time (`02-seed.sql` INSERT) | |
| Total size of all partitions after seed | |
| Number of partitions | |
| Q1 -- bounded window, execution time | |
| Q1 -- `Subplans Removed:` count | |
| Q1 -- `Planning Time` and planning `Buffers` | |
| Q2 -- no partition key, execution time | |
| Q2 -- number of `Index Scan` children | |
| Q2b -- time-bounded, execution time | |
| Q3 -- pruning disabled, execution time | |
| Q5 -- partitionwise aggregate off / on | |
| L3 -- `ATTACH` time with a non-empty `DEFAULT` | |
| **Retention: rows in the dropped partition** | |
| **Retention: `DETACH CONCURRENTLY` time** | |
| **Retention: `DROP TABLE` time** | |
| **Retention: `VACUUM` time owed afterwards** | (expect: none) |
| **Total size after retention** | |

Next: [Lab 02 -- Retention, rollover, and pg_partman](../02-retention-rollover/README.md)
