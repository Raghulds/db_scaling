# Decision guide

A table is too big or too slow and somebody has said the word "shard". This is the
ordered set of questions to answer before you agree.

## The ordering rule

**Every option above sharding is cheaper than sharding, and sharding is a one-way
door.** Work down the list. Do not skip a rung because it feels beneath the
problem. The rungs are ordered by cost to you, not by sophistication, and the
cheapest fix that works is the correct fix.

Sharding is a one-way door in a specific, practical sense: once your data model
assumes no cross-shard transactions, no cross-shard joins, and no global unique
constraints, everything built afterwards assumes it too. Unwinding that is not a
migration, it is a rewrite. You can undo a partition in an afternoon. You cannot
undo a shard.

---

## The tree

```
                  "this table is too big / too slow"
                                  |
   (0) is the database actually the bottleneck?
       |  no  -> fix the N+1, the missing cache, the chatty client,
       |         the serialisation. STOP.
       yes
        |
   (1) one query, or the whole workload?
       |  one  -> index it, rewrite it, or materialise it. STOP.
       whole
        |
   (2) can you delete or archive instead?
       |  yes -> retention policy, cold storage, summary tables. STOP.
       no
        |
   (3) is the pain vacuum / retention / index size / working set,
       on ONE node that is otherwise coping?
       |  yes -> PARTITION.  (labs 01, 02, 03)         <- most teams stop here
       no
        |
   (4) is this one table crowding out everything else on the box?
       |  yes -> move the table to its own database / instance. STOP.
       no
        |
   (5) is it read load?
       |  yes -> read replicas + a connection pooler + caching. STOP.
       no
        |
   (6) is it write throughput or storage that exceeds one machine,
       measured, at the largest instance you can actually buy?
       |  no  -> go back to (3). you are not there yet.
       yes
        |
   (7) have you exhausted vertical scaling?
       (bigger box, faster disk, more RAM, tuned checkpoints,
        cheaper column types, fewer indexes)
       |  no  -> do that first. it is one afternoon and no new failure modes.
       yes
        |
   (8) do you have the operational prerequisites? (see below)
       |  no  -> build them first. sharding without them is not a
       |         scaling project, it is an incident generator.
       yes
        |
   (9) what is the shard key, and how will you know it was wrong?
       |  no answer -> stop. this is the whole decision.
       answered
        |
      SHARD  (labs 04, 05, 06)
```

---

## The questions, in prose

### 0. Is it actually the database?

Measure before you believe. A slow endpoint with a fast database is the most common
shape of this complaint, and the usual causes are an N+1 in the ORM, a cache that
was never warm, a client doing serial round trips, or JSON serialisation of a
result set nobody needed. Get a flame graph or a span breakdown that attributes the
time. If the database is under a third of the request, stop here and go fix the
rest.

If you have `pg_stat_statements` (the labs enable it), start with total time rather
than mean time. The query that is slow is rarely the query that is expensive.

### 1. One query, or the whole workload?

If a single query shape dominates, that is an indexing or rewriting problem, and it
is hours of work rather than quarters. Look for the usual suspects: a missing
composite index, a leading column that is not selective, an `OR` that prevents an
index range, a function on the indexed column, a sort that could be served by the
index order, or a count that could be an estimate. A partial index over the rows
you actually query is frequently the entire fix, and it costs nothing to try.

If instead everything is uniformly slower than it was six months ago, and the
degradation tracks table growth, you have a structural problem. Continue.

### 2. Can you delete or archive instead?

The cheapest row is the one you do not store. Ask what the retention requirement
actually is, in writing, from whoever owns the data. In practice the honest answer
is often much shorter than the table's age, and nobody has ever been asked.

Three variants, in increasing order of effort: delete outright; move to cold
storage and delete; or aggregate into summary rows and delete the detail. All three
are dramatically cheaper than any form of splitting, and all three leave you with a
smaller, simpler system rather than a larger, more complex one.

The catch, which lab 00 makes visceral, is that deleting from a large flat table is
itself expensive: the `DELETE` is slow, it leaves the space unreturned, and it
hands you a `VACUUM` bill proportional to what you removed. That is an argument for
partitioning **so that** you can delete, not an argument against deleting.

### 3. Is the pain vacuum, retention, index size, or working set?

This is the rung most systems belong on, and it is where the ladder in this repo
spends four of its eight labs. The symptoms:

- Autovacuum never finishes, or finishes only during your quietest hour.
- The retention job is scheduled monthly because it cannot survive running daily.
- Total index size has overtaken the heap, and index maintenance dominates write
  latency.
- The hot working set no longer fits in `shared_buffers`, so cache hit ratio has
  quietly fallen and every query pays for I/O.

All four are partitioning problems on one node. Partitioning turns retention into
`DETACH` plus `DROP` (milliseconds, no vacuum debt, space returned), keeps each
index small enough to stay cached, bounds vacuum work to one partition at a time,
and makes the working set an explicit function of your partition key.

The requirement is a partition key that appears in most of your queries. Time is
the easy case and the common one. Tenant works when your access is tenant-scoped
but hurts cross-tenant reporting, which lab 03 measures. If no candidate key is
present in the majority of your predicates, partitioning will make some queries
faster and others slower, and you need to count which.

### 4. Can you move the table to its own database?

Sometimes the table is fine and its neighbours are not, or the reverse. One
high-churn table can consume the shared buffer pool, the autovacuum workers, and
the WAL bandwidth that the other forty tables needed. Moving it to its own instance
is a well-understood operation with a rehearsed playbook, it gives you independent
tuning and independent failure domains, and it does not change your data model for
anything else.

The cost is real: cross-database joins are gone for that table, and you have a
second instance to operate. But it is one extra instance, not N, and you keep
ordinary transactions within each side.

### 5. Is it read load?

Read replicas plus a pooler solve read scaling without touching your data model,
and they are the single highest-leverage thing on this list after retention. The
price is replication lag, which forces you to classify every read as "must be
current" or "may be stale", and that classification is genuine work you should do
deliberately rather than discover through a bug report.

A connection pooler belongs here too. If your problem is that connection count has
outgrown the server, that is a pooler problem, not a sharding problem, and
sharding will make it worse: pools multiply by shard count and by pod count at the
same time.

### 6. Is it write throughput or storage, measured?

To pass this rung you need a number and a unit. "Write throughput" means sustained
transactions or bytes per second that a single instance cannot absorb, with the
evidence being WAL generation rate, checkpoint frequency, or replication lag on a
synchronous standby, not a feeling about growth. "Storage" means projected size
that exceeds the largest volume you can attach, with a date attached to the
projection.

If neither number exists, you are on rung 3 and have not finished it.

Note what does **not** qualify: a table with a lot of rows in it. Row count is not
a problem. Postgres is entirely comfortable with billions of rows in a partitioned
table on one machine, provided vacuum keeps up and the working set is bounded.

### 7. Have you exhausted vertical scaling?

The largest cloud instances have hundreds of gigabytes of RAM and local NVMe that
will absorb write rates most teams never approach. Doubling an instance is a
maintenance window; sharding is a quarter or three. Before you accept the quarter,
be sure you have done the boring things: right-sized column types, dropped the
indexes nobody uses, tuned checkpoint and WAL settings, moved a heavy column to
`TOAST`-friendly storage or out of the row entirely, and compressed what compresses.

It is unsatisfying advice and it is usually correct.

### 8. Do you have the operational prerequisites?

See the checklist below. If you do not have them, build them first. Every one of
them is useful on an unsharded system too, which means none of the work is wasted
if you never shard.

### 9. What is the shard key, and how will you know it was wrong?

The rest of this document.

---

## Choosing the shard key

The shard key is the most expensive decision in the project, and unlike almost
everything else in software it is not cheaply revisable. Interrogate it.

**Is it present in every hot query?** Write down your top twenty query shapes by
total time. For each, ask whether the shard key is in the predicate. Anything
without it becomes a scatter-gather, so the answer to "what fraction of my traffic
becomes fan-out" is the fraction of that list that does not contain the key. If it
is more than a few percent, you have the wrong key or the wrong architecture.

**Does it bound your transactions?** The unit of atomicity after sharding is the
shard. If a single user action has to modify two rows with different key values,
that action now needs an outbox or 2PC and its own idempotency story. Count how
many such actions you have. If checkout touches two tenants, sharding by tenant
just made checkout eventually consistent.

**Does it bound your joins?** Same test. A join whose two sides can land on
different nodes is either a colocation problem (lab 06) or an application-level
join you now have to write and maintain.

**How skewed is it?** Run the two queries in `labs/03-hash-list-multitenant/02-skew.sql`
against your production data. If the top 1% of key values own a large share of the
rows, hashing will not save you, because a single key value cannot be split. You
need the escape hatch before launch, not after.

**Does it ever change?** A key that can be reassigned (a user moving between
organisations, a merged account, a corrected tenant id) means rows migrating
between shards as an ordinary product operation rather than an exceptional one.
Prefer an immutable key, and if you cannot have one, build the move path on day
one because you will use it weekly.

**Is it available at the edge?** Routing happens before you touch the database, so
the key has to be derivable from the request: in the path, the token, or a cheap
cached lookup. A shard key you can only learn by querying the database is a
lookup on every request, and that lookup service is now your most critical
dependency.

**Is the hash pinned?** Whatever function maps key to shard must be stable forever,
golden-tested, and free of per-process seeding. Changing it moves every key at once
and strands the data silently. See the comment in `internal/shard/hash.go`; it is
the shortest file in the repo and the most important one to not touch.

### How you will know it was wrong

Decide these thresholds now, while it is cheap, and instrument them:

- The share of queries that fan out exceeds what you predicted, and it is rising.
- One shard holds a disproportionate share of rows or traffic and rebalancing does
  not fix it, because the imbalance is inside a single key value.
- The number of writes requiring an outbox or 2PC grows with each feature rather
  than staying flat.
- Engineers are adding denormalised copies of data specifically to avoid a
  cross-shard join, and nobody has counted how many.

The first two are dashboards. The last two are code review, and they are the early
warnings, because they show up long before the graphs do.

### The escape hatch for a whale

Build it before you need it, because you will need it, and because retrofitting it
during an incident is how directories get corrupted.

The mechanism is an override that takes precedence over the hash: a pin table
mapping specific key values to specific physical shards, consulted first on every
routing decision. `Directory.Pin` in `internal/shard/topology.go` is the whole
idea in nine lines. With it, moving your largest customer to dedicated hardware is
a backfill plus one row change; without it, it is a reshard.

The partitioning-level version of the same idea is lab 03's `LIST` partition for
the whale over a hash-partitioned `DEFAULT` for everyone else. It is worth doing
that lab specifically to see that the two-level shape is identical at both scales.

Two operational notes. Pins must bypass the migration logic or you get subtle
double-write bugs (see how `RouteWrite` treats a pinned tenant). And a pin is a
permanent entry in a table someone has to maintain, so cap how many you will
tolerate before you admit the key was wrong.

---

## Operational prerequisites

Do not start a sharding project without these. Each is independently useful, so
building them is not speculative work, and the absence of any one of them turns a
routine reshard into an incident.

**Per-shard observability.** Every metric you have must be sliced by shard: query
latency, error rate, connection count, replication lag, disk usage, vacuum
progress. A cluster-wide average hides the one sick node, and in a scatter-gather
system the sick node sets your p99. You should be able to answer "which shard is
slow" in one dashboard, without a query.

**An audit for misplaced rows.** A query that finds rows sitting on a shard the
current topology would not route them to. Run it continuously, not just during
migrations. It is how you detect a stale router, a bug in the placement map, or a
half-finished move, and it is the difference between finding data loss in an hour
and finding it in a quarter. `shardctl audit` is the toy version.

**A schema migration runner that handles N databases and partial failure.** Applying
a migration to eight databases is not eight times applying it to one. You need
per-shard status tracking, resumability, a plan for the shard that failed halfway,
and an expand/contract discipline strict enough that the cluster is correct while
half of it has the old schema and half the new. Assume partial failure is the
normal case, because it is.

**Backup and restore, tested per shard.** Backups that have not been restored are
not backups. In a sharded system you additionally need to answer: can you restore
one shard to a point in time without restoring the others, and what is the
consistency story if you do? There is no cluster-wide consistent snapshot unless
you built one, so know what you are promising before you are asked at 3am.

**A rehearsed reshard.** Move one logical shard, end to end, in production, on a
quiet day, when nothing is on fire. Time each step. Break the ordering deliberately
in staging and watch the audit catch it. The first reshard should never be the
urgent one, and a directory topology exists precisely so that the rehearsal can be
scoped to a single bucket. That rehearsal is what lab 05 is.

**A connection budget.** Write down shards multiplied by pool size multiplied by
application instances, compare it to `max_connections`, and put a pooler in front
before the number embarrasses you. This surprises people in week one of sharding,
before any of the interesting problems arrive.

---

## If you are still here

You have measured that one machine cannot hold or absorb your data, exhausted
vertical scaling, built the six prerequisites, chosen a key that appears in your hot
queries and bounds your transactions, and built the whale escape hatch. Start at
[lab 04](../labs/04-app-sharding-go/) and do not skip [lab 05](../labs/05-resharding/):
the cutover ordering is the part that loses data, and it is much cheaper to learn
it here.
