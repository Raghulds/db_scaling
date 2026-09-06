# Lab 02 -- Retention, rollover, and whether pg_partman earns its dependency

[Lab 01](../01-range-partitioning/README.md) proved that dropping a partition beats
deleting rows. That leaves the part nobody demos: something has to create next month's
partition before next month starts, and something has to retire the old ones, forever,
without waking anyone up. This lab is about the operational half. It measures the
`ATTACH` validation scan and shows how to make it free, builds the rollover job by hand
in about forty lines of plpgsql so you understand every lock it takes, and then runs the
same job under pg_partman so you can decide -- with the hand-rolled version in front of
you -- whether the extension is worth adding.

## Run it

Step 2 operates on `events_p`, which is created by lab 01. Run lab 01 first or step 2
fails immediately on the `regclass` cast:

```bash
make lab01
```

```bash
make lab02
```

`01-attach-cost.sql` builds its own 2,000,000-row tables and ignores `ROWS`, so this lab
takes roughly the same time regardless of how you sized the earlier ones.

One step by hand:

```bash
scripts/psql.sh lab -v ON_ERROR_STOP=1 -f labs/02-retention-rollover/01-attach-cost.sql
```

Note that step 2 is destructive to lab 01's data: `retire_month_partitions('events_p', 6)`
drops every partition whose lower bound is more than six months old, including the 1999
partition lab 01 created. Re-run `make lab01` if you want that state back.

## The steps

### 01-attach-cost.sql -- the validation scan, and how to make it free

Builds a partitioned `attach_demo` and two identical 2M-row standalone tables, then
attaches both. The only difference is that the second one carries a `CHECK` constraint
matching its future bounds.

What `ATTACH PARTITION` has to prove is that every row in the incoming table falls inside
the declared range. It can do that in one of two ways: scan the table, or find an already
validated constraint that implies the range and trust it. With the `CHECK` present, the
attach is a catalog update.

Measured on a default-configuration PostgreSQL 16 container with 2M rows -- your absolute
numbers will differ, the ratio is the point:

| Statement | Time |
| --- | --- |
| `ATTACH` with no `CHECK` (full validation scan) | ~520 ms |
| `ALTER TABLE ... ADD CONSTRAINT ... CHECK (...)` | ~360 ms |
| `ATTACH` with the matching `CHECK` | ~2 ms |

What to notice, and it is not "the second way is faster":

- The `CHECK` path does *more* total work (360 + 2 vs 520). What changed is **where the
  lock is held**. The `ADD CONSTRAINT` scan happens on a standalone table that is not yet
  part of `attach_demo`, so no query against `attach_demo` is blocked while it runs. The
  `ATTACH` itself then holds its locks for two milliseconds instead of half a second. At
  a realistic partition size that is the difference between a two-millisecond deploy step
  and a multi-minute stall on the whole partitioned table.
- Scale it. The validation scan is linear in the partition, so a 200M-row backfill
  partition is not 520 ms, it is minutes, and `ACCESS EXCLUSIVE` on the table being
  attached plus `SHARE UPDATE EXCLUSIVE` on the parent is held for all of it. Worse, if
  the parent has a non-empty `DEFAULT` partition, the `DEFAULT` gets scanned too, under
  `ACCESS EXCLUSIVE` -- see L3 in [lab 01](../01-range-partitioning/README.md).
- The script drops the `CHECK` immediately after the attach. Once the partition bound
  exists the constraint is redundant, and leaving it means every insert evaluates the
  same predicate twice.
- The closing note is the part people get wrong: **`NOT NULL` on the partition key matters
  as much as the `CHECK`.** A nullable key means a row could be `NULL`, `NULL` satisfies
  no range comparison (it evaluates to unknown, not false), so the constraint no longer
  implies the bound and Postgres scans anyway. If the attach is unexpectedly slow, check
  `attnotnull` on the key column before you check anything else.

This is the whole backfill pattern for a partitioned table: create standalone, load with
`COPY` at full speed with no indexes, build indexes, add `NOT NULL` and the matching
`CHECK`, then attach.

### 02-rollover-manual.sql -- the job, written out

Defines two plpgsql functions and runs them against `events_p`.

`ensure_month_partitions(parent, months_ahead)` walks forward from the current month and
creates any partition that does not already exist, keyed off `to_regclass(part) IS NULL`
so it is idempotent -- you can run it every hour and it costs a catalog lookup.

`retire_month_partitions(parent, keep_months, archive)` reads the partition list out of
`pg_inherits`, parses the lower bound out of the text of `pg_get_expr(relpartbound, oid)`,
and for anything older than the cutoff does `DETACH` and then either `RENAME` (archive) or
`DROP`.

What to notice:

- **The ordering is the lesson.** `DETACH` first, `DROP` second. `DROP TABLE` on a table
  that is still an attached partition takes `ACCESS EXCLUSIVE` **on the parent**, which
  blocks every query against every partition -- not just the old month you are deleting.
  Confirmed by watching `pg_locks` inside a transaction:

  ```
   relname |        mode
  ---------+---------------------
   events_p | AccessExclusiveLock
  ```

  Detaching first turns the drop into an ordinary `DROP TABLE` on a table nobody
  references.
- The plain `DETACH` is itself not free: it takes `ACCESS EXCLUSIVE` on both the parent
  and the child for the duration of the catalog update. It is short, but it queues behind
  and then blocks every running query on the table, so on a busy system you run it with a
  `lock_timeout` and retry. The lock-free form, `DETACH CONCURRENTLY`, **cannot be used
  here**: a plpgsql function body is a transaction, and `DETACH CONCURRENTLY` refuses to
  run inside a transaction block. If you want the concurrent form you have to drive the
  loop from outside the database -- from `\gexec` as lab 01 does, or from your scheduler,
  or from Go. That constraint, not the SQL, is what shapes the design of a real rollover
  job.
- `archive => true` renames instead of dropping, which is the right default when you are
  not yet certain about your retention policy. A detached, renamed table still consumes
  disk and still needs `pg_dump`ing or moving to object storage; renaming is a delay, not
  a policy.
- The bound parsing is the fragile part of the whole file. It regex-matches the text
  rendering of the partition bound. It works, but it is version-dependent formatting, it
  assumes a single-column key, and it silently returns `NULL` if the format ever changes
  -- and a `NULL` lower bound falls through `CONTINUE WHEN r.lower >= cutoff` and drops
  the partition. If you ship a hand-rolled rollover, keep your own metadata table of
  `(partition_name, lower_bound, upper_bound)` rather than parsing the catalog's prose.

**The outage this prevents.** At 00:00:00 on the 1st, if no partition covers the new
month and there is no `DEFAULT`, every insert fails with `no partition of relation ...
found for row`. Not slow, not degraded: a hard error on the write path. `DEFAULT` turns
it from an outage into a slow leak that makes your next `ATTACH` scan a large table. So
you need both headroom and an alarm. The alarm is not "did the cron job run" -- it is a
statement about the data:

```sql
SELECT max((regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz)
       AS newest_upper_bound
FROM pg_class c
JOIN pg_inherits i ON i.inhrelid = c.oid
WHERE i.inhparent = 'events_p'::regclass
  AND pg_get_expr(c.relpartbound, c.oid) LIKE 'FOR VALUES FROM%';
```

Page when `newest_upper_bound - now()` drops below a couple of days, and alert separately
on `count(*)` in the `DEFAULT` partition being greater than zero. Those two checks cover
every way this fails.

### 03-partman.sql -- the same job, as an extension

Creates the `partman` schema and extension (the lab image is
`postgres:16` plus `postgresql-16-partman`, built from `docker/partman/Dockerfile`),
creates a `metrics` table partitioned daily, hands it to `partman.create_parent` with
`p_premake => 4`, then configures retention directly in `partman.part_config` and calls
`partman.run_maintenance_proc()`.

What to notice, reading it against your own step 2:

- `p_premake => 4` is `ensure_month_partitions(parent, 3)` under a different name, and
  partman maintains that headroom on every maintenance run rather than only when you
  remember to call it.
- `retention => '14 days'` is `retire_month_partitions(parent, keep, ...)`, and
  `retention_keep_table => true` is your `archive => true`. `retention_keep_index =>
  false` is the part you did not write: it drops the indexes off retired tables, which is
  usually most of their remaining footprint.
- `infinite_time_partitions => true` is the setting whose absence causes the 3am page.
  Without it, partman stops creating partitions once it sees a gap with no data, which is
  precisely the state a quiet weekend puts you in before a Monday traffic spike.
- `run_maintenance_proc` is a `PROCEDURE`, not a function, and that is deliberate: it
  commits between tables so one slow partition operation does not hold a single long
  transaction open across the whole maintenance pass. A long transaction here would pin
  the xmin horizon (see [lab 00](../00-baseline/README.md)) and stall VACUUM everywhere.
- Compare `partitions_before` and `partitions_after` around the maintenance call, then
  look at the detached-but-kept tables. Those are `metrics_p*` relations with no row in
  `pg_inherits`. If nothing ever deletes them you have swapped a retention problem for a
  disk-usage problem with a longer fuse.
- What partman gives you that forty lines of plpgsql does not: template tables (so
  per-partition objects that are not inherited from the parent -- unique indexes not on
  the partition key, per-partition privileges -- get applied to new partitions), an
  `optimise`/subpartition story, epoch and integer-keyed partitioning, and a retention
  implementation that other people have already hit the edge cases of.

**How to decide.** Take the extension if you have more than one or two partitioned
tables, if you need anything the parent does not inherit, if you cannot easily run a
scheduler outside the database, or if nobody on the team wants to own the rollover code.
Write it yourself if you have exactly one partitioned table with a plain monthly cadence,
if you are on a managed provider that will not install the extension, or if you want
`DETACH CONCURRENTLY` -- which the hand-rolled job can have only because it is driven
from outside a transaction. Either way you need the two alerts above; the extension does
not supply them.

## What you should be able to answer afterwards

1. `ATTACH` with a matching `CHECK` does more total work than `ATTACH` without one, yet
   it is the correct choice. Explain precisely what changed, in terms of which lock is
   held on which relation for how long.
2. Your `ATTACH` is slow even though the `CHECK` matches the bounds exactly. Name the
   most likely cause, and the second most likely.
3. Why is `DROP TABLE` on an attached partition worse than `DETACH` followed by
   `DROP TABLE`, and which relation does the bad version lock?
4. `DETACH CONCURRENTLY` cannot be used inside `retire_month_partitions`. Say why, and
   describe two ways to restructure the job so it can use the concurrent form.
5. Write out the full backfill procedure for loading a 200M-row historical partition into
   a live partitioned table, in order, naming the lock taken at each step.
6. It is 00:00:00 on the 1st and no partition covers the new month. What happens with a
   `DEFAULT` partition present, and what happens without one? Which of the two is worse
   six months later, and why?
7. Give the two monitoring queries that make partition rollover safe, and explain why
   "the cron job exited zero" is not one of them.
8. `infinite_time_partitions` and `retention_keep_index` both default to the value you
   probably do not want. What does each actually do, and what is the failure if you leave
   it alone?

## Traps

- `ATTACH` without a matching `CHECK` is a full table scan under lock. It is fast in a
  demo and an outage in production.
- A nullable partition key defeats the `CHECK` optimisation entirely. Add `NOT NULL`.
- Leaving the redundant `CHECK` on after the attach makes every insert evaluate the
  predicate twice.
- `DROP TABLE` on an attached partition takes `ACCESS EXCLUSIVE` on the parent. Always
  `DETACH` first.
- `DETACH` without `CONCURRENTLY` blocks the whole table. Run it under a `lock_timeout`
  and retry rather than letting it queue.
- A `DO` block or a plpgsql function is a transaction, so `DETACH CONCURRENTLY` will
  never work inside one.
- Parsing partition bounds out of `pg_get_expr` text is brittle, and in this file a parse
  failure drops the partition instead of skipping it. Keep your own metadata.
- Archiving by renaming is deferral, not retention. The disk is still full.
- One month of premake is not premake. Insert failures are immediate and total.
- pg_partman does not monitor itself. You still write both alerts.

## Timings to record

| Measurement | Value |
| --- | --- |
| `ATTACH` without `CHECK`, 2M rows | |
| `ADD CONSTRAINT ... CHECK`, 2M rows | |
| `ATTACH` with matching `CHECK` | |
| Ratio of the two `ATTACH` times | |
| `ensure_month_partitions` -- partitions created | |
| `retire_month_partitions` -- partitions retired | |
| `retire_month_partitions` -- wall clock | |
| Live partitions on `events_p` afterwards | |
| partman -- partitions before maintenance | |
| partman -- partitions after maintenance | |
| partman -- `run_maintenance_proc` wall clock | |
| partman -- detached-but-kept tables | |

Next: [Lab 03 -- Hash and list partitioning for multi-tenant data](../03-hash-list-multitenant/README.md)
