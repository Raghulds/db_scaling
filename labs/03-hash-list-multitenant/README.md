# Lab 03 -- Hash and list partitioning for multi-tenant data

[Lab 01](../01-range-partitioning/README.md) partitioned by time because the retention
job was the problem. Your `orders` table is not like that: nothing ages out, every query
carries a `tenant_id`, and the thing that hurts is that one customer is a hundred times
bigger than the median. This lab partitions by tenant instead, and the result is a
sharper lesson than "it worked". Hash partitioning distributes *keys* evenly and you do
not have a key problem -- you have a row problem, and no hash function has ever fixed
one. Step 1 builds the eight-way hash layout and measures how badly it fails; step 2 is
the pair of skew queries you should keep and run in production; step 3 builds the layout
that actually works -- `LIST` partitions for the whales over a hash-partitioned
`DEFAULT` -- and then shows you why moving a tenant into one is a lock you cannot afford,
which is the entire reason [lab 05](../05-resharding/) exists.

Numbers quoted below come from one run at `ROWS=5000000 TENANTS=500` against the lab
container (PostgreSQL 16.15, stock configuration). Your absolute values will differ. The
ratios are the point, and they will not.

## Run it

```bash
make lab03
```

`TENANTS` controls the length of the tail, and it changes the shape of the result more
than `ROWS` does:

```bash
ROWS=20000000 TENANTS=5000 make lab03
```

One step by hand (`runlab.sh` normally supplies `-v rows` and `-v tenants`, so pass them
yourself):

```bash
scripts/psql.sh lab -v ON_ERROR_STOP=1 -v rows=5000000 -v tenants=500 -f labs/03-hash-list-multitenant/02-skew.sql
```

Steps 2 and 3 both read `orders_h`, which step 1 creates, and step 3 copies its data out
of it. Run the lab in order the first time.

## The steps

### 01-hash.sql -- eight-way hash on tenant_id

Creates `orders_h` with `PARTITION BY HASH (tenant_id)`, eight partitions declared as
`MODULUS 8, REMAINDER 0..7`, an index on the parent, and seeds `:rows` rows across
`:tenants` tenants with `power(random(), 4)` skew.

What to notice:

- `PRIMARY KEY (tenant_id, id)`, partition key first. The lab 01 rule still applies --
  every unique constraint must contain the partition key -- but here it costs you
  nothing you were not already paying, because every query is tenant-scoped anyway.
  Leading with `tenant_id` also makes the primary key usable for tenant-scoped scans on
  its own, which is why several plans in this lab use `orders_h_N_pkey` rather than the
  explicit index. What you still lose is `WHERE id = ?`: with no `tenant_id`, that is
  eight index scans, exactly like lab 01.

- **The distribution table is the whole lesson.** Measured:

  ```
    relname   | distinct_tenants | approx_rows | total
  ------------+------------------+-------------+--------
   orders_h_0 |               67 |     1546588 | 214 MB
   orders_h_1 |               63 |      595943 | 82 MB
   orders_h_2 |               69 |      565307 | 77 MB
   orders_h_3 |               51 |      460668 | 64 MB
   orders_h_4 |               72 |      437827 | 61 MB
   orders_h_5 |               57 |      509339 | 69 MB
   orders_h_6 |               60 |      455721 | 63 MB
   orders_h_7 |               61 |      428607 | 60 MB
  ```

  Keys: 51 to 72 per partition, a spread of 1.41x. Rows: 428,607 to 1,546,588, a spread
  of 3.61x. The hash function did its job perfectly and the outcome is still a partition
  three and a half times the size of its siblings. **Hash partitioning promises even key
  distribution. It has never promised even row distribution, and rows are what you
  store, scan, vacuum and back up.** The cause is one tenant: tenant 1 owns 1,058,399
  rows, 21% of the whole table, and it lands in `orders_h_0`, where it is 68% of the
  partition.

- To find which partition a tenant is in -- worth keeping, because you cannot work it out
  by hand:

  ```bash
  scripts/psql.sh lab -c "SELECT c.relname FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid WHERE i.inhparent = 'orders_h'::regclass AND satisfies_hash_partition('orders_h'::regclass, 8, substring(c.relname from '[0-9]+\$')::int, 1::bigint);"
  ```

  Postgres hashes with the type's extended hash function and a fixed partition seed, so
  placement is stable across dumps and restores but is not something you can predict from
  the tenant id.

- **The tenant-scoped read prunes to one partition and is still not a point lookup.**
  `WHERE tenant_id = 3` produces:

  ```
   Finalize Aggregate (actual time=130.059..144.552 rows=1 loops=1)
     ->  Gather
           ->  Partial Aggregate
                 ->  Parallel Seq Scan on orders_h_1 orders_h (actual rows=44758 loops=3)
                       Filter: (tenant_id = 3)
                       Rows Removed by Filter: 153890
  ```

  One partition out of eight, as advertised -- and a sequential scan of all 595,943 rows
  in it, because tenant 3 is 22% of that partition and no index is selective at 22%.
  Pruning buys you a smaller haystack, not a smaller needle. This matters when you size
  partition counts: the useful question is not "how many partitions" but "how big is one
  partition, and what fraction of it is one tenant".

- **Reading these plans.** `Parallel Seq Scan on orders_h_0 orders_h_1` is partition
  **zero**. The second identifier is an alias the planner generates by uniquifying the
  parent's name over the surviving children; the numbers do not line up with the
  partition numbers and they are not supposed to. Always read the first identifier.

- **The IN-list prunes to at most three, sometimes fewer.** `tenant_id IN (3, 17, 42)`
  scanned `orders_h_1`, `orders_h_2` and `orders_h_4`. Tenants 2 and 42 both hash into
  `orders_h_2`, so a list of k tenants touches k partitions only if none of them collide.
  Equally important: only `=` and `IN` prune. `WHERE tenant_id BETWEEN 3 AND 5` scans all
  eight, even though it covers three values, because hashing destroys ordering -- that is
  what it is for. Any range predicate on the partition key of a hash-partitioned table is
  a full fan-out.

- **The cross-tenant report scans everything, every time.** The 7-day `GROUP BY state`
  took 1.39 s and read all eight partitions, 5M rows, filtering on `placed_at` with no
  index that starts with `placed_at`. There is no partition key in that query and there
  never will be, because it is not about one tenant. If reporting is your main workload,
  partitioning by tenant is the wrong choice and you have just made every report a
  fan-out; if reporting is secondary, this is the cost you accepted, and in
  [lab 04](../04-app-sharding-go/) it becomes a network fan-out where the p99 is the p99
  of the slowest participant.

### 02-skew.sql -- measure the skew you just created

Three queries. These are the two-and-a-half things worth carrying into production
unchanged.

- **Top tenants by row count.** Measured: tenant 1 at 21.17% of the table, tenant 2 at
  4.01%, tenant 3 at 2.69%, decaying from there. This is the query that tells you whether
  you have whales, and it is the input to every placement decision in the rest of the
  repo -- which tenants get their own `LIST` partition here, which get pinned to their own
  shard in the directory in [lab 04](../04-app-sharding-go/).

- **Top 1% share.** Measured: 1,582,706 of 5,000,000 rows, **31.65%**. Five tenants out
  of five hundred own a third of the table. Track this number over time. When it climbs,
  your rebalancing gets harder, not easier, because the unit you have to move is getting
  bigger.

- **Partition imbalance.** Measured `smallest 428607, largest 1546588, imbalance_ratio
  3.61`. Near 1.0 means hash did what you hoped. The file's threshold of ~1.5 is a
  reasonable page-me line. Note that this one reads `reltuples` from `pg_class` rather
  than counting, so it is nearly free and safe to run on a schedule -- but it is only as
  fresh as the last `ANALYZE`. The two exact queries above it scan the whole table (737
  ms and 357 ms at 5M rows) and belong on a replica or a schedule, not in a dashboard
  that refreshes every ten seconds.

**Why you cannot fix this by adding partitions.** The unit of placement is one tenant.
Every row with `tenant_id = 1` hashes to the same value, so at `MODULUS 16` tenant 1 is
still entirely inside one partition, which is still 1,058,399 rows. Splitting harder
subdivides the long tail, which was never the problem. Your options are to give the whale
its own partition (step 3), to give it its own shard (lab 04's `Pins` map), or to
partition on something finer than `tenant_id` -- which destroys tenant-scoped pruning and
turns every API query back into a fan-out.

**And resizing a hash-partitioned table is not online.** Adding a ninth partition fails:

```
ERROR:  every hash partition modulus must be a factor of the next larger modulus
DETAIL:  The new modulus 9 is not divisible by 8, the modulus of existing partition "orders_h_7".
```

Doubling one partition in place fails too, because `MODULUS 16, REMAINDER 15` overlaps
the existing `MODULUS 8, REMAINDER 7`. The supported move is a split: `DETACH` the
modulus-8 partition, create `MODULUS 16 REMAINDER 7` and `MODULUS 16 REMAINDER 15`, and
`INSERT ... SELECT` the detached rows back so routing splits them. That works, and for
the duration of it those rows are not in the table. Hold that thought -- it is the same
problem, at a smaller scale, that [lab 05](../05-resharding/) solves properly.

### 03-list-hot-tenant.sql -- the two-level layout, and the lock that ends the lab

Builds `orders_l`: `PARTITION BY LIST (tenant_id)` with one partition each for tenants 1
and 2, and a `DEFAULT` partition `orders_l_rest` that is itself `PARTITION BY HASH
(tenant_id)` four ways. Then it copies every row out of `orders_h`, runs three queries,
and promotes tenant 9 into its own partition.

Postgres has no single-level "hash the leftovers of a LIST" syntax, so the escape hatch is
a sub-partitioned `DEFAULT`. **This shape is the whole design pattern**, and it is the
same one you build by hand in Go in lab 04: an explicit lookup for the tenants you have
named, hashing for everyone else.

What to notice:

- **The tree output includes indexes.** `pg_inherits` describes the index hierarchy as
  well as the table hierarchy, so rows whose parent is `orders_l_pkey` or
  `orders_l_tenant_placed_idx` are index partitions, not tables. `orders_l_rest` shows as
  `0 bytes` for the same reason `pg_total_relation_size('orders_h')` returns 0: a
  partitioned relation has no storage of its own. Any monitoring query that asks a parent
  for its size gets zero and reports that everything is fine.

- **The layout fixed the imbalance.** Measured on the hashed `DEFAULT` after the two
  whales were pulled out:

  ```
   orders_l_rest_0 |  926016 | 127 MB
   orders_l_rest_1 | 1052509 | 152 MB
   orders_l_rest_2 |  820296 | 113 MB
   orders_l_rest_3 |  889275 | 122 MB
  ```

  Imbalance ratio **1.28**, against **3.61** for the flat eight-way hash on the same
  data. Two `LIST` partitions and a hashed remainder did what doubling the partition count
  could not.

- **The whale's partition is not small.** `SELECT count(*) FROM orders_l WHERE tenant_id
  = 1` is a `Parallel Seq Scan on orders_l_t1` over 1,058,400 rows, 154 ms. Isolating a
  whale does not shrink it. What it buys is that the whale's vacuum, its bloat, its index
  maintenance, its statistics and its eventual migration are its own, and that a scan of
  it does not evict the long tail's pages from `shared_buffers`. It also makes the whale
  detachable, which is the precondition for moving it to its own shard.

- **The long-tail query prunes through both levels.** `tenant_id = 137` reaches
  `orders_l_rest_3` and uses a `Bitmap Index Scan` on its primary key. Look at the line
  under it:

  ```
   Bitmap Heap Scan on orders_l_rest_3 (actual time=19.571..136.737 rows=6500 loops=1)
     Heap Blocks: exact=3178
  ```

  6,500 rows spread over 3,178 heap blocks -- roughly two rows per block. Partitioning by
  tenant puts a tenant's rows in one *file*; it does not put them next to each other
  inside that file. If you want physical locality you need `CLUSTER` (which takes
  `ACCESS EXCLUSIVE` and is not maintained afterwards), an insertion order that happens to
  be tenant-ordered, or a shard per tenant. This is one of the few things a dedicated
  shard genuinely gives you that a partition does not.

- **The promotion block is the lab's punchline.** Moving tenant 9 into its own partition
  is `CREATE` + `CHECK` + `INSERT` + `DELETE` + `ATTACH`, wrapped in one transaction so it
  is atomic. Measured, for a tenant with 52,773 rows:

  | Statement | Time |
  | --- | --- |
  | `CREATE TABLE ... (LIKE ...)` | 13 ms |
  | `ADD CONSTRAINT ck CHECK (tenant_id = 9)` | 1 ms |
  | `INSERT INTO orders_l_t9 SELECT ...` | 187 ms |
  | `DELETE FROM orders_l WHERE tenant_id = 9` | 46 ms |
  | `ATTACH PARTITION ... FOR VALUES IN (9)` | **706 ms** |
  | `COMMIT` | 2 ms |

  The `ATTACH` dominates, and the `CHECK` did not help.
  [Lab 02](../02-retention-rollover/README.md)'s trick exempts the
  *incoming* table from validation; it does nothing about the `DEFAULT`, which Postgres
  must still scan to prove that no `tenant_id = 9` row is hiding in it. Proof by
  experiment -- attaching an **empty** table for a tenant id that has never existed, with
  a matching `CHECK`:

  ```
  ALTER TABLE
  Time: 597.085 ms
  ```

  All of it is the `DEFAULT` scan. The cost of promoting a tenant is proportional to the
  size of everyone else.

- **The locks, measured from `pg_locks` inside the transaction:**

  | Relation | Mode |
  | --- | --- |
  | `orders_l` (parent) | `ShareUpdateExclusiveLock` |
  | `orders_l_rest` (DEFAULT) | `AccessExclusiveLock` |
  | `orders_l_rest_0..3` (its leaves) | `AccessExclusiveLock` |
  | `orders_l_t9` (incoming) | `AccessExclusiveLock` |

  The parent lock is the mild one, and it is the one people quote. The lock that matters
  is `ACCESS EXCLUSIVE` on the entire `DEFAULT` subtree, which is where every tenant you
  did not promote lives -- so this operation blocks essentially your whole customer base
  while it runs. And because it is one transaction, those locks are held from the first
  statement to `COMMIT`: at 52,773 rows that is about a second, at a real whale's 200M
  rows it is the copy time plus the `DEFAULT` scan time, with the site down for all of
  it.

- The `DELETE` also leaves one dead tuple per moved row in the `DEFAULT` leaves.
  [Lab 00](../00-baseline/README.md)'s bill, again: you now owe a `VACUUM` proportional
  to what you moved.

**This is why lab 05 exists.** A tenant move that is correct and atomic is a tenant move
that is unavailable. The only way out is to stop making it atomic: dual-write to old and
new, backfill in bounded batches outside any long transaction, verify the two copies
agree, flip reads, and only then delete the source. [Lab 04](../04-app-sharding-go/)
builds the routing layer that makes that flip expressible -- `Directory.BeginMove` opens
the dual-write window, `Directory.Commit` flips reads -- and
[lab 05](../05-resharding/) runs the whole cutover.

## What you should be able to answer afterwards

1. Hash spread the tenant ids across the eight partitions within 1.41x of each other and
   the rows within 3.61x. Explain exactly which invariant hash partitioning provides,
   why row counts are not it, and why going to `MODULUS 16` does not improve the ratio.
2. A query with `WHERE tenant_id = 3` pruned to one partition and then sequentially
   scanned all 595,943 rows in it. Why did the planner ignore the index, and what does
   that tell you about how to choose the number of partitions?
3. Which predicates on a hash partition key prune, and which do not? Explain why
   `BETWEEN 3 AND 5` scans every partition when `IN (3, 4, 5)` does not.
4. You need to go from eight hash partitions to sixteen on a live table. Write out the
   sequence, state which step makes rows temporarily invisible, and say what the
   application has to do during that window.
5. Promoting tenant 9 cost 706 ms in `ATTACH`, and attaching an empty partition for a
   tenant that never existed cost 597 ms. Explain where that time goes, why a matching
   `CHECK` did not remove it, and what it would cost on a table ten times the size.
6. Name every relation that is locked `ACCESS EXCLUSIVE` during the promotion, and say
   which group of customers each lock affects. Why is the parent's
   `SHARE UPDATE EXCLUSIVE` the least interesting entry in that list?
7. The long-tail query read 3,178 heap blocks to return 6,500 rows. What did partitioning
   give you here, what did it not give you, and what are the three ways to actually get
   physical locality for one tenant?
8. Your product is 90% cross-tenant reporting and 10% tenant-scoped API reads. Argue
   from the plans in step 1 whether hash-by-tenant is the right partitioning key, and say
   what you would do instead.

## Traps

- Hash partitioning distributes keys, not rows. If your tenants are unequal, your
  partitions will be unequal, and no modulus fixes it.
- One tenant is the smallest unit hashing can place. A whale bigger than your target
  partition size cannot be split by adding partitions.
- Pruning to one partition is not a point lookup. A whale is still a sequential scan of
  its own partition.
- Only `=` and `IN` prune a hash-partitioned table. Every inequality is a full fan-out.
- An `IN` list of k tenants touches at most k partitions, and fewer when they collide.
  Do not size fan-out from the length of the list.
- The second name in `Seq Scan on orders_h_0 orders_h_1` is a generated alias. Read the
  first one or you will debug the wrong partition.
- `pg_total_relation_size` on a partitioned parent returns 0. Sum over `pg_inherits` or
  your size alerts will never fire.
- `reltuples` is only as fresh as the last `ANALYZE`. An imbalance dashboard built on
  stale statistics reports that everything is fine.
- A non-empty `DEFAULT` partition makes every future `ATTACH` scan it under
  `ACCESS EXCLUSIVE`. Under a sub-partitioned `DEFAULT` that means the whole subtree.
- Promoting a tenant in one transaction is correct, atomic, and an outage. Atomicity is
  the thing you have to give up.
- Hash-by-tenant makes every cross-tenant report scan every partition. Choose the key
  from your dominant workload, not from your table's primary key.

## Timings to record

Run at the same `ROWS` you used for labs 00 and 01 if you want the seed times to be
comparable. The bold rows are the ones that justify step 3 over step 1.

| Measurement | Value at ROWS=______ TENANTS=______ |
| --- | --- |
| Seed time (`01-hash.sql` INSERT) | |
| Total size of all `orders_h` partitions | |
| Distinct tenants: smallest / largest partition | |
| **`orders_h` rows: smallest / largest partition** | |
| **`orders_h` imbalance ratio** | |
| Largest tenant's share of the table | |
| Largest tenant's share of its own partition | |
| Top 1% of tenants -- share of all rows | |
| `tenant_id = 3` -- plan node and execution time | |
| `tenant_id IN (3,17,42)` -- partitions touched | |
| Cross-tenant 7-day report -- execution time | |
| Copy time (`03-list-hot-tenant.sql` INSERT) | |
| `orders_l_t1` size / `orders_l_t2` size | |
| **`orders_l_rest_*` rows: smallest / largest** | |
| **`orders_l_rest_*` imbalance ratio** | |
| `tenant_id = 137` -- `Heap Blocks: exact=` | |
| Promotion: `INSERT` + `DELETE` time | |
| **Promotion: `ATTACH` time** | |
| **Promotion: total time locks were held** | |
| Rows promoted (dead tuples now owed to `VACUUM`) | |

Next: [Lab 04 -- Application-level sharding in Go](../04-app-sharding-go/README.md)
