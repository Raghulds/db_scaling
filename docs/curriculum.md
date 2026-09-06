# Curriculum

The ladder in depth. [The README](../README.md) tells you what each lab is; this
tells you what you are supposed to walk away with, what to write down, and how to
know you are actually done rather than merely finished.

Two rules for the whole ladder:

1. **Record your numbers.** Not because the absolute values matter (they depend on
   your laptop), but because the ratios do, and because the comparison table at the
   bottom is the artefact you keep. A claim like "partitioning makes retention
   cheap" is worth nothing next to "on my machine the `DELETE` took 41 seconds and
   left 300 MB of dead tuples, the `DETACH` took 6 ms and returned the space".
2. **Read the errors.** Several labs deliberately fail. Lab 01 step 4 fails six
   times on purpose. The error text is the content.

---

## Lab 00 - Baseline: what one big table costs

**The question it answers.** Before any technique: where does the pain actually
come from on a table that has grown past comfort? Is it query time, index size,
vacuum, or the operations you run on it?

**Prerequisite understanding.** How Postgres MVCC works at a high level: an
`UPDATE` writes a new row version and leaves the old one dead; `VACUUM` reclaims
dead tuples for reuse but does not usually return space to the filesystem. If that
sentence is new, read Rogov chapter on MVCC before starting (see
[resources.md](resources.md)).

**Run it.**

```bash
make lab00
```

**Artefacts produced.** A flat `events` table with three indexes, seeded with
`ROWS` rows spread over twelve months, with a deliberately Zipf-ish tenant
distribution. It stays in the database; lab 01 builds `events_p` alongside it so
you can compare directly.

**Measure and record.**

- Heap size, total index size, total relation size. Note which is larger.
- `EXPLAIN (ANALYZE, BUFFERS)` for the three query shapes: recent window across all
  tenants, one tenant over a recent window, and the full-table count. Record
  execution time and shared buffers hit/read for each.
- `n_dead_tup` and heap size before and after updating 10% of rows.
- `VACUUM` wall time, and heap size after it.
- The retention `DELETE`: rows removed, wall time, heap size afterwards, and the
  `VACUUM` time you now owe.

**You are ready to move on when you can:** explain, without looking, why the heap
did not shrink after the `DELETE`; say what determines `VACUUM` duration on this
table (table size, not change size); and name the two things a single flat table
makes structurally impossible to do cheaply, no matter how good your indexes are
(bulk deletion, and bounding the working set).

---

## Lab 01 - Range partitioning and pruning

**The question it answers.** What does declarative partitioning actually buy, what
does it cost in query planning, and which of your existing queries will get slower
rather than faster?

**Prerequisite understanding.** Lab 00's numbers. Also: what a B-tree index scan
costs as a function of index height, so that "smaller indexes per partition" means
something concrete to you.

**Run it.**

```bash
make lab01
```

**Artefacts produced.** `events_p`, partitioned by range on `occurred_at`, with
monthly partitions covering thirteen months back through one month ahead, plus a
`DEFAULT`. Note that the primary key had to become `(id, occurred_at)`: Postgres
requires the partition key in every unique constraint, so a single-column `id`
primary key is not available to you any more.

**Measure and record.**

- Seed time versus lab 00 for the same row count. Routing costs something; smaller
  per-partition indexes save something. Which won on your machine?
- For Q1 (bounded window): how many partitions appear under `Append`, and the
  execution time.
- For Q2 (tenant lookup with no time bound): how many partitions were scanned.
  This query got **slower** than lab 00. Write down both times side by side.
- For Q2b (same query, time-bounded): partitions scanned, execution time.
- Q3, with `enable_partition_pruning = off`: the counterfactual cost.
- Q4: the `Subplans Removed: N` line under a forced generic plan. This is run-time
  pruning, and it is a different mechanism from the plan-time kind.
- Q5: partitionwise aggregate off versus on.
- Planning time on a full scan of the parent, as a function of partition count.
- Step 5: `DETACH CONCURRENTLY` plus `DROP TABLE` wall time, dead tuples afterwards
  (zero), and total size afterwards.

**The six limitations in step 4.** Each one is a design constraint, not a bug.
Write them down as constraints you will have to design around:

1. No unique index that omits the partition key, so no database-enforced global
   uniqueness on an external id.
2. A row outside every range fails to insert unless a `DEFAULT` partition exists.
3. Once the `DEFAULT` has rows in it, creating a new partition that overlaps its
   contents requires scanning it under an `ACCESS EXCLUSIVE` lock.
4. An `UPDATE` that moves a row across partitions is a delete plus an insert: new
   ctid, extra bloat, and surprising interaction with triggers and cursors.
5. `CREATE INDEX CONCURRENTLY` does not work on a partitioned parent. The
   workaround is per-partition `CIC`, then `ON ONLY` on the parent, then
   `ALTER INDEX ... ATTACH PARTITION` for each child.
6. No exclusion constraints on partitioned tables at all.

**You are ready to move on when you can:** look at a query and say whether it will
prune, before running `EXPLAIN`; explain the difference between plan-time pruning,
run-time pruning (`Subplans Removed`), and the old constraint-exclusion mechanism;
and state the cost of the `DEFAULT` partition in one sentence.

---

## Lab 02 - Retention, rollover, and pg_partman

**The question it answers.** Partitioning is only a win if partitions appear and
disappear without a human. What does that automation have to get right, and what
does it cost when it gets it wrong?

**Prerequisite understanding.** Lab 01. Specifically, what `ATTACH` has to prove
before it can succeed.

**Run it.**

```bash
make lab02
```

**Artefacts produced.** `attach_demo` with two attached partitions loaded
identically; the `ensure_month_partitions` and `retire_month_partitions` plpgsql
functions applied to `events_p`; a `metrics` table managed by `pg_partman` with a
14-day retention policy.

**Measure and record.**

- `ATTACH` without a matching `CHECK` versus `ATTACH` with one, on the same 2M
  rows. This is the headline number of the lab: one is a validation scan of the
  whole table under `ACCESS EXCLUSIVE`, the other is a catalogue update.
- Partitions created and retired by your own functions.
- `pg_partman` partition count before and after `run_maintenance_proc()`.

**The subtle points to internalise.**

- A nullable partition key forces the validation scan even with a `CHECK`, because
  `NULL` satisfies no range test. Make the partition key `NOT NULL`.
- In `retire_month_partitions`, the order is `DETACH` then `DROP`. Dropping an
  attached partition takes `ACCESS EXCLUSIVE` on the **parent**, which blocks every
  query against the whole table, not just the old month.
- The alert that actually saves you is not "maintenance failed". It is "the newest
  partition's upper bound is less than 24 hours away". Write that check down.

**You are ready to move on when you can:** write the create-ahead/retire-behind job
from memory in outline; explain why the `CHECK` constraint makes `ATTACH` cheap;
and argue either side of "use pg_partman or write 40 lines of plpgsql" with real
trade-offs rather than preference.

---

## Lab 03 - Hash and list partitioning for multi-tenant data

**The question it answers.** Your data is tenant-scoped, not time-scoped. Does
partitioning by tenant help, and what happens when one tenant is a hundred times
bigger than the median?

**Prerequisite understanding.** Labs 01 and 02. Plus an honest look at your own
product: are your queries tenant-scoped, or are they reports across tenants? The
answer determines whether this lab is a solution or a trap.

**Run it.**

```bash
make lab03
```

Increase the tenant count if you want a longer tail:

```bash
TENANTS=5000 make lab03
```

**Artefacts produced.** `orders_h` hash-partitioned eight ways on `tenant_id`, and
`orders_l` with `LIST` partitions for two whales over a hash-partitioned `DEFAULT`.

**Measure and record.**

- Rows per hash partition, and the imbalance ratio (largest divided by smallest).
  Hash distributes tenants evenly; it does nothing about tenants being uneven.
- The share of the table owned by the top 1% of tenants.
- Execution time and partitions touched for: a single-tenant query, a three-tenant
  `IN` list, and a cross-tenant report over a time window.
- The two-level prune in `orders_l`: confirm a long-tail tenant reaches exactly one
  leaf through both levels.
- The wall time of the online whale promotion at the end of step 3, and note that
  the whole thing ran inside one transaction holding locks.

**You are ready to move on when you can:** state the one workload for which hash
partitioning by tenant is actively harmful (cross-tenant reporting: every report
now scans every partition); explain why adding more hash partitions cannot fix a
single oversized tenant; and describe the two-level list-over-hash shape and why it
is the same shape you want in a sharded system.

---

## Lab 04 - Application-level sharding in Go

**The question it answers.** Everything above was one node. What changes the moment
there are four, and which of those changes are permanent?

**Prerequisite understanding.** Lab 03, and comfort reading Go. Read
`internal/shard/topology.go` and `internal/shard/hash.go` before running anything.
They are short and the comments are the lesson.

**Run it.**

```bash
make lab04
```

Re-run just the guided tour without reseeding:

```bash
make lab04-demo
```

**Artefacts produced.** Four independent Postgres nodes running the identical
schema in `labs/04-app-sharding-go/schema.sql`, a placement directory at
`tmp/topology.json`, and seeded `orders` plus `ledger` data. The `shardctl` binary
in `bin/`.

**Measure and record.**

- `shardctl topology`: the churn percentage for mod-N, consistent hash, and
  directory routing when going from 4 shards to 8. Then look at the odd-number case
  the command prints. Mod-N doubling moves about half your tenants; mod-N going
  from 4 to 5 moves about four fifths of them. That asymmetry is why mod-N shops
  can only ever double, and can never rehearse the migration on one tenant.
- `shardctl stats`: rows, distinct tenants, and bytes per shard. Note the imbalance,
  and that it comes from tenant size, exactly as in lab 03.
- `shardctl tenant 7`: the single-shard fast path. Latency.
- `shardctl count`: scatter-gather. Record the total, and the **slowest** shard.
  The p99 of a fan-out query is the p99 of its worst participant, so the mean is
  not a number you should ever report.
- `shardctl count -partial`: what a partial answer looks like, and think about
  which of your endpoints could honestly serve one.
- `shardctl page`: rows fetched versus rows returned. To return 50 rows from 4
  shards you fetch 200. At 32 shards you fetch 1600. Read amplification is linear
  in shard count.
- `shardctl transfer 2pc`, then kill it during the commit phase, then
  `shardctl orphans`. Record what the orphan holds: locks, and the xmin horizon,
  which stops `VACUUM` from reclaiming anywhere in that database.
- `shardctl transfer outbox`: the same effect with no distributed commit. Note that
  there is a window in which the source shard shows the transfer and the
  destination does not.
- `shardctl audit`: rows sitting on a shard the current topology would not have
  routed them to. Should be zero here. It will not be zero during lab 05.

**Things worth pausing on.**

- Connection pools multiply. 8 shards times 20 connections times 30 application
  pods is 4800 backends, and Postgres falls over long before that. This is the
  first operational surprise of sharding and it arrives before any of the
  interesting ones. See `internal/shard/cluster.go`.
- The hash function can never change. Changing it moves every tenant and silently
  strands their data. Pin it, golden-test it, and never improve it.
- Snowflake ids encode the shard they were created on. That is a convenience for
  routing by id and a trap after a tenant moves. Treat the encoded shard as a hint
  about the past, never as the current location.

**You are ready to move on when you can:** explain why the directory topology beats
consistent hashing for a system with whales, even though both move a similar
fraction of keys on a resize; describe the exact failure sequence that leaves an
orphaned prepared transaction and what it does to the database; and explain what
makes the outbox pattern correct despite delivering at least once (the `applied`
table's primary key).

---

## Lab 05 - Resharding a live cluster, 4 to 8

**The question it answers.** How do you move a tenant's data to a new node while
that tenant is writing, and what exactly can go wrong in each of the five steps?

**Prerequisite understanding.** Lab 04 in full, and lab 07 if you want to see the
primitive real systems use for the backfill. Have the directory topology's
`BeginMove`/`Commit` semantics clear in your head before starting.

**Run it.**

```bash
make lab05
```

Then do it again on a single bucket, aborting halfway on purpose, and resume:

```bash
./bin/reshard run -logical 128 -fail-after 2
```

**Artefacts produced.** Eight shards, a modified `tmp/topology.json` with logical
shards remapped to the new physical nodes, and a `reshard` binary that prints its
plan before executing it. `reshard status` shows the current placement and any
dual-write window still open, which is the state an aborted move leaves behind.
See `labs/05-resharding/README.md` and `cmd/reshard/`.

**Measure and record.**

- The plan: how many logical shards move, how many rows that is, and where they go.
- Backfill duration and rows copied per logical shard.
- The length of the dual-write window in wall-clock time.
- `shardctl audit` during the window (rows legitimately in two places, or on the
  old home) and after the cutover (should return to zero).
- What an abort leaves behind: run one logical shard with `-fail-after 2`, then
  `reshard status`. The window is open, the copy is unverified, and the system is
  still correct, because reads have not moved. Do this for each of the five stop
  points and write down, for each, whether you could safely park there overnight.
- What happens when you deliberately let a router keep a stale placement across the
  cutover. This is the exercise that matters.

**The five steps, and the failure mode of each.**

| Step | What it does | How it fails |
|---|---|---|
| Plan | choose which logical shards move where | move too much at once, and you have no way to abort |
| Open dual-write | writes go to old and new | a router that has not seen the update writes only to the old home |
| Backfill | copy existing rows | rows written after a page is copied are missed unless dual-write is already on |
| Verify | compare counts and checksums per logical shard | comparing while writes are in flight gives false mismatches |
| Cut over | flip reads, close the window | flipping reads before every router dual-writes loses the writes in between |

The ordering constraint is the whole lab: dual-write must be **on everywhere**
before the backfill starts, and reads must not flip until dual-write is
**confirmed on everywhere**. Anything else is a data-loss window.

**You are ready to move on when you can:** write the five steps in order from
memory with the failure mode of each; explain why the backfill must not start
before the dual-write window opens; and say what a metadata store gives you that a
JSON file does not (atomic visibility, watches, and fencing a stale router).

---

## Lab 06 - Citus: distribution as an extension

**The question it answers.** Everything in labs 04 and 05 was hand-written. What
does a distributed-Postgres extension do for you, and what does it still refuse to
make cheap?

**Prerequisite understanding.** Labs 03 and 04. You need to have felt the
scatter-gather and the cross-shard write by hand, or Citus will look like magic
instead of like a well-made version of the same trade-offs.

**Run it.**

```bash
make lab06
```

**Artefacts produced.** A coordinator plus two workers, `orders` distributed over
32 shards, a colocated `order_items`, a deliberately non-colocated `shipments`, a
`currencies` reference table, and a tenant isolated onto a shard of its own. Four
SQL steps; see `labs/06-citus/README.md`.

**Measure and record.**

- `Task Count` for a single-tenant query versus a cross-tenant one. That one line
  is the whole plan-reading skill for this lab.
- The colocated join versus the same join repartitioned. This is the number the
  whole lab exists for, and it is order of 100x at this data size.
- `count(DISTINCT tenant_id)` versus `count(DISTINCT id)`: bytes received from the
  nodes, and the wall clock. Same table, same rows, one column changed.
- `Intermediate Data Size` on the two `Distributed Subplan` lines in step 3. One
  is bytes, one is megabytes.
- Shard-size imbalance ratio before and after a whale tenant arrives, and again
  after `isolate_tenant_to_new_shard`.
- The rebalancer's plan for that skewed cluster, and why it is empty.
- The step 4 shard move run both ways: once with
  `shard_transfer_mode => 'block_writes'`, once with `=> 'force_logical'`. Wall
  clock for each, and then the number that actually decides it -- how long a
  concurrent writer against that shard is stalled. `block_writes` holds the source
  locked for the whole copy, so the stall is the move. `force_logical` copies under
  logical decoding and takes the lock only at the cutover, so the stall is a small
  fraction of it, paid at the end. Step 4 measures the stall for you, with a
  background writer inserting for the whale throughout each move: a synchronous
  move cannot be observed from the session performing it. Record the writer's
  slowest insert for each mode. The two totals will land in the same band at this
  shard size, because `force_logical` does strictly more work and the shard is
  only tens of megabytes, so the total is the number that does **not** separate
  the modes.

**Two things this environment gives you that a stock cluster does not.** The
three `citusdata/citus` services run with
`shared_preload_libraries=citus,pg_stat_statements` (`citus` has to come first,
or the extension fails to load) and with `wal_level=logical`. So
`citus_stat_statements` works here, and so does
`shard_transfer_mode => 'force_logical'` on shard moves and on
`isolate_tenant_to_new_shard`. Neither is a default. Postgres ships
`wal_level=replica`, and raising it is a `postmaster` setting: a restart of the
node, not a `pg_reload_conf()`. Adding a library to `shared_preload_libraries`
costs you the same restart. This repo sets both deliberately; the cluster you
actually operate almost certainly does not, and the day you find out is the day
you want to move a shard without blocking writes and cannot get the restart. Run
`SHOW wal_level` on your own workers now rather than then. Note also that the
preload is only the first of three gates on `citus_stat_statements`, and the only
one that costs a restart: the extension is per-database, so
`CREATE EXTENSION pg_stat_statements` is a separate plain DDL statement, and
`citus.stat_statements_track` has defaulted to `none` since Citus 11, which is a
GUC and leaves the view empty on a cluster that looks correctly configured. Step
4 walks the second and third for you. The first is the one you cannot do
mid-incident, which is the whole point. Read lab 06's README on why
`force_logical` versus `block_writes` is the argument of labs 05 and 07 compressed
into one function argument.

**You are ready to move on when you can:** define colocation precisely and say what
must be true for two tables to be colocated; explain why a distribution column is
the same decision as a shard key, with the same consequences if it is wrong; name
two query shapes Citus cannot make cheap no matter how it is configured; and say
why a rebalancer correctly refuses to fix single-tenant skew.

---

## Lab 07 - Logical replication as a reshard primitive

**The question it answers.** How does the data actually get from the old shard to
the new one in a real system, continuously, without locking the source?

**Prerequisite understanding.** Lab 05, so you know what problem this solves. Also
the difference between physical (byte-level, whole cluster) and logical (row-level,
selectable) replication.

**Run it.**

```bash
make lab07
```

**Artefacts produced.** A publisher and a subscriber, a publication named
`orders_pub`, a subscription `orders_sub`, and a replication slot you can watch
advance. See `labs/07-logical-replication/`.

**Measure and record.**

- Initial `copy_data` duration and rows copied.
- Lag between publisher and subscriber while the write load runs.
- `restart_lsn` on the slot and the retained WAL size derived from it. Watch this
  number while the subscriber is stopped: it only grows.
- What breaks when the table has no replica identity, and what
  `REPLICA IDENTITY FULL` costs.

**The operational lesson.** A replication slot is a promise to keep WAL until the
consumer has read it. A slot with no consumer fills the disk on the **publisher**,
which is the node you cannot afford to lose. Alert on slot lag from day one, and
know the command that drops an abandoned slot.

**You are ready to move on when you can:** explain replica identity and what
happens to `UPDATE`/`DELETE` replication without a suitable one; describe what a
replication slot retains and the failure mode of forgetting one; and sketch how
you would build lab 05's backfill on top of this instead of on a copy loop.

---

## Suggested schedules

### A few hours a week (eight weeks)

One lab per week, roughly two hours, in order. The pacing works because each lab
leaves an artefact the next one uses, and because the week in between is when the
ideas settle.

| Week | Lab | Homework between sessions |
|---|---|---|
| 1 | 00 | Find the largest table in your own production database and record the same size breakdown for it |
| 2 | 01 | Take your three most expensive queries on that table and decide, on paper, whether each would prune under a time-range partition key |
| 3 | 02 | Write down what your retention policy actually is. If nobody knows, that is the finding |
| 4 | 03 | Run lab 03's two skew queries against your real data. Identify your whales by name |
| 5 | 04 | Read `internal/shard/` end to end. It is roughly 1,400 lines and every comment is load-bearing |
| 6 | 05 | Write the cutover runbook for one of your own tables, including the abort path |
| 7 | 06 | Compare your lab 04 hand-rolled router against Citus honestly: what did you build worse, what did you build more cheaply |
| 8 | 07 | Check whether any replication slot exists in your production cluster that nobody is consuming |

### One weekend (fast path)

Skip nothing in labs 00, 01, 04 and 05. Skim the rest. Use a smaller row count so
the seeds do not eat the schedule:

```bash
ROWS=1000000 make lab00
```

| Slot | Do this |
|---|---|
| Saturday morning | Labs 00 and 01. Do not skip step 4 of lab 01; the six limitations are the highest-value 20 minutes in the repo |
| Saturday afternoon | Lab 02 step 1 only (the `ATTACH` timing), then lab 03 in full |
| Saturday evening | Read `internal/shard/topology.go`, `scatter.go`, `keyset.go`, `txn.go`, `outbox.go`. No running, just reading |
| Sunday morning | Lab 04 in full, including killing the 2PC transfer mid-commit |
| Sunday afternoon | Lab 05 in full, twice: once following the runbook, once breaking the ordering on purpose |
| Sunday evening | Lab 07 if you have energy. Lab 06 can wait; it is the least surprising of the eight once you have done 04 |

---

## The numbers to record

Fill this in as you go. The point is the comparison, and the comparison is only
convincing when the numbers are yours.

### Single node: flat versus partitioned

| Measurement | Source | Flat (lab 00) | Partitioned (lab 01) |
|---|---|---|---|
| Heap size | `pg_table_size` | | |
| Total index size | `pg_indexes_size` | | |
| Recent-window aggregate: exec time | Q1 | | |
| Recent-window aggregate: buffers read | Q1 | | |
| Single-tenant, no time bound: exec time | lab 00 Q2 / lab 01 Q2 | | |
| Single-tenant, time-bounded: exec time | lab 01 Q2b | | |
| Partitions scanned, bounded query | lab 01 Q1 | n/a | |
| Partitions scanned, unbounded tenant query | lab 01 Q2 | n/a | |
| Planning time, full parent scan | lab 01 step 3 | | |
| Retention: rows removed | step 5 | | |
| Retention: wall time | step 5 | | |
| Retention: dead tuples afterwards | step 5 | | |
| Retention: space returned to the OS | step 5 | | |

### Partition maintenance

| Measurement | Source | Value |
|---|---|---|
| `ATTACH` without `CHECK`, 2M rows | lab 02 step 1 | |
| `ATTACH` with matching `CHECK`, 2M rows | lab 02 step 1 | |
| Ratio between the two | | |
| Partitions created ahead by the rollover job | lab 02 step 2 | |
| Partitions retired behind | lab 02 step 2 | |

### Skew

| Measurement | Source | Value |
|---|---|---|
| Largest hash partition / smallest | lab 03 step 2 | |
| Share of table owned by top 1% of tenants | lab 03 step 2 | |
| Single-tenant query, partitions touched | lab 03 step 1 | |
| Cross-tenant report, partitions touched | lab 03 step 1 | |
| Whale query on a dedicated LIST partition | lab 03 step 3 | |

### Routing and fan-out

| Measurement | Source | Value |
|---|---|---|
| Tenants moved, mod-N, 4 to 8 | `shardctl topology` | |
| Tenants moved, mod-N, 4 to 5 | `shardctl topology` | |
| Tenants moved, consistent hash, 4 to 8 | `shardctl topology` | |
| Tenants moved, directory, 4 to 8 | `shardctl topology` | |
| Rows / tenants / bytes on the largest shard | `shardctl stats` | |
| Single-shard read latency | `shardctl tenant 7` | |
| Scatter-gather total latency | `shardctl count` | |
| Slowest single shard in that scatter | `shardctl count` | |
| Rows fetched to return one page of 50 | `shardctl page` | |
| Orphaned prepared transactions after a kill | `shardctl orphans` | |
| Outbox rows pending at peak | `shardctl transfer outbox` | |

### Reshard and replication

| Measurement | Source | Value |
|---|---|---|
| Logical shards moved | `reshard plan` | |
| Rows copied | `reshard run` | |
| Backfill duration | `reshard run` | |
| Dual-write window duration | lab 05 | |
| Misplaced rows during the window | `shardctl audit` | |
| Misplaced rows after cutover | `shardctl audit` | |
| Initial logical-replication copy duration | lab 07 | |
| Peak subscriber lag under write load | lab 07 | |
| WAL retained by an idle slot after 5 minutes | lab 07 | |

### Bought instead of built (lab 06)

The comparison against your own router. Every row has a counterpart above.

| Measurement | Source | Value |
|---|---|---|
| `Task Count`, single-tenant read | 06 step 1, plan A | |
| `Task Count`, cross-tenant aggregate | 06 step 1, plan B | |
| Colocated join, execution time | 06 step 2, J4 after repair | |
| Repartitioned join, execution time | 06 step 2, repartition block | |
| Ratio of those two | 06 step 2 | |
| `count(DISTINCT tenant_id)`, bytes from nodes | 06 step 3, C1a | |
| `count(DISTINCT id)`, bytes from nodes | 06 step 3, C1b | |
| Window over `tenant_id` vs over `state`, wall clock | 06 step 3, C3a/C3b | |
| Shard-size imbalance ratio, uniform seed | 06 step 4 | |
| Shard-size imbalance ratio, after the whale | 06 step 4 | |
| Rebalancer moves proposed for that cluster | 06 step 4 | (expect: none) |
| Single shard move, wall clock, `block_writes` | 06 step 4 | |
| Single shard move, wall clock, `force_logical` | 06 step 4 | |
| Concurrent-writer stall during that move, `block_writes` | 06 step 4 | |
| Concurrent-writer stall during that move, `force_logical` | 06 step 4 | |

When the table is full, you own something more useful than this repo: a set of
measured ratios you can quote in a design review, from a machine you understand.
