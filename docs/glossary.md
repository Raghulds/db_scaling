# Glossary

Definitions for the terms that get used interchangeably in design reviews and
should not be. Grouped by the problem they belong to rather than alphabetically,
because the confusions are between neighbours.

Where a term has a lab that makes it visible, the lab is named. Reading a
definition is worth about a tenth of watching the thing happen.

---

## The core pair

### Partition

One of several child tables that together make up a single logical table on a
single Postgres node. The planner routes rows to partitions on write and excludes
irrelevant ones on read; from the application's point of view there is still one
table, one transaction, and one set of joins. Partitioning is a physical-layout
decision, fully reversible, and it never changes your query semantics.
*Labs 01, 02, 03.*

### Shard

One of several independent databases, each holding a disjoint subset of the rows,
with no knowledge of the others. Nothing is transactional, joinable, or unique
across shards, and the routing is your application's job. Sharding is an
architectural decision that reaches into your data model, your query layer, your
deployment, and your on-call rotation, and it is not meaningfully reversible.
*Labs 04, 05, 06.*

### The distinction that matters

Partitioning solves vacuum time, retention cost, index size, and working-set
locality. Sharding solves "one machine cannot hold this" and "one machine cannot
absorb these writes". If you cannot state which of those two you have, with a
number, you have a partitioning problem. See
[decision-guide.md](decision-guide.md).

---

## Keys and routing

### Partition key

The column (or expression) in `PARTITION BY` that decides which child table a row
lands in. Postgres forces it into every unique constraint on the table, including
the primary key, which means a partitioned table cannot enforce global uniqueness
on anything that excludes it. Choosing it is choosing which queries prune and which
ones scan everything. *Lab 01, where the primary key becomes `(id, occurred_at)`.*

### Shard key

The value your router hashes to pick a physical database. Almost always a tenant,
customer, or user id, because that is the largest unit you can keep on one node
without breaking joins. Getting it wrong is expensive in a way that getting a
partition key wrong is not, because fixing it means moving every row. *Lab 04,
`internal/shard/hash.go`.*

### Distribution column

Citus's name for the shard key. Same decision, same consequences, expressed as
`create_distributed_table('orders', 'tenant_id')` instead of as router code. The
fact that it is one function call does not make it a smaller commitment. *Lab 06.*

### Logical shard

A stable, oversized bucket that a tenant is hashed into once and never leaves. This
repo uses 1024 of them. Because the tenant-to-logical mapping never changes, no
tenant is ever rehashed, and scaling is purely a matter of remapping buckets to
machines. *Lab 04, `internal/shard/topology.go`.*

### Physical shard

An actual database instance. Many logical shards live on one physical shard, and
the logical-to-physical map is a small mutable lookup table (the directory) that
you can edit one entry at a time. This indirection is why a directory topology can
move a single noisy tenant while consistent hashing cannot. *Labs 04, 05.*

### Consistent hashing

Placing shards and keys on a ring so that adding a node moves only the keys that
land between the new node and its predecessor, roughly `1/(n+1)` of them, and never
moves a key between two existing nodes. Better than mod-N, still worse than a
directory for a multi-tenant system, because you do not get to choose *which* keys
move and you cannot pin a whale. *Lab 04, `shardctl topology`.*

---

## Query shapes

### Pruning

The planner (or the executor) proving that a partition cannot contain any matching
row and skipping it entirely. Plan-time pruning happens when the bounds are
constants; run-time pruning happens when they are parameters, and shows up in
`EXPLAIN ANALYZE` as `Subplans Removed: N`. Pruning is the entire payoff of
partitioning, and any query without a predicate on the partition key loses it.
*Lab 01 step 3.*

### Constraint exclusion

The older, weaker mechanism (`constraint_exclusion`) that worked on inheritance
hierarchies by reasoning about `CHECK` constraints, before declarative partitioning
existed. It runs only at plan time and cannot handle parameters. If you are on a
supported Postgres version with declarative partitioning, pruning has replaced it;
knowing the difference mainly protects you from following old blog posts.

### Colocation

Two distributed tables sharded on the same key, with matching shard boundaries, so
that every row of table A that could join to a row of table B lives on the same
node. Colocated joins run locally on each worker; non-colocated joins force the
coordinator to move data between nodes at query time. Designing for colocation is
most of the work in a Citus schema.

The word that matters is *declared*. Colocation is a catalog fact, not a property
the planner infers from your schema: two tables can share a distribution column,
a column type, a shard count, and the physical placement of every shard, and the
join between them will still be refused if nothing recorded that they belong to
the same colocation group. Lab 06 step 2 builds exactly that pair and then repairs
it with `update_distributed_table_colocation`, which moves no data at all. *Lab 06
step 2.*

### Reference table

A table replicated in full to every node rather than sharded across them, so that
any join against it is local everywhere. Correct for small, slow-changing,
read-everywhere data: currency codes, plan tiers, feature flags. The cost is on
the write side and it is not small -- every `INSERT` is a transaction across the
whole cluster, holding locks on every node -- so the test is not "is this table
small" but "is this table written by traffic or by a human". A reference table is
also the only thing a distributed table may hold a foreign key to without
including the distribution column. *Lab 06 step 2.*

### Repartition join

The fallback when a join cannot be pushed down: one or both sides are re-hashed on
the join key into intermediate result files, shipped between workers, and then
joined. It is a shuffle, it is network-bound, it is not pipelined, and its cost
scales with the size of the data rather than the size of the answer. Citus refuses
it by default and requires `citus.enable_repartition_joins`. Treat that setting as
a switch that makes a nightly report possible, never as a fix for a request-path
query. *Lab 06 step 2.*

### Scatter-gather

A query the router sends to every shard, then merges. The latency is not the mean
of the shards, it is the maximum, so a single slow node makes every scatter query
in the system slow. Bound each shard with its own timeout and decide explicitly
whether a partial answer is acceptable, because an undercounted total that looks
authoritative is worse than an error. *Lab 04, `shardctl count`,
`internal/shard/scatter.go`.*

### Fan-out

The act of one request becoming N requests. The number that matters is not N but
N multiplied by your request rate multiplied by your pod count, which is how a
fan-out query becomes a self-inflicted denial of service against your own database
tier. Cap concurrency at the router. *Lab 04, `ScatterOpts.Concurrency`.*

### Read amplification

Rows read divided by rows returned. In a cross-shard sorted page it is linear in
shard count: to return the newest 50 rows from 8 shards you fetch 400 and discard
350. This is why adding shards makes global feeds worse, and why production systems
maintain a separate denormalised feed rather than merging on read. *Lab 04,
`shardctl page`.*

### Offset pagination

`ORDER BY ... LIMIT n OFFSET k`. The database must produce and discard `k` rows to
answer, so cost grows with page number, and across shards it is unusable: there is
no way to ask shard 2 for "rows 200-250 of the merged order" without reading the
first 250 from every shard. It is also incorrect under concurrent inserts, which
shift rows between pages.

### Keyset pagination

Paginating by "everything after this exact row", using the last row's sort key as
the cursor. Cost is constant per page, it is stable under concurrent writes, and it
is the only form that works across shards. The sort key must be total: `created_at`
alone can skip or repeat rows that share a timestamp, so the cursor carries
`(created_at, id)`. *Lab 04, `internal/shard/keyset.go`.*

---

## Migration

### Resharding

Changing the number of shards, or the mapping from keys to shards, and moving data
to match. This is the expensive one: it touches routing, requires a dual-write
window, and has a cutover that can lose data if ordered wrongly. *Lab 05.*

### Rebalancing

Moving whole shards or whole logical buckets between machines without changing the
key mapping, usually to even out size or load. Cheaper than resharding because no
key changes owner in the logical sense, only the machine underneath does. Citus
does this for you with `citus_rebalance_start`. *Lab 06.*

### Splitting

Taking one shard that has grown too big and dividing its key range in two. It is a
special case of resharding scoped to a single node, and it is the operation you
actually want in production because the blast radius is one shard rather than the
whole cluster. The two-level directory design exists so that a split is an edit to
a lookup table.

### Dual-write

The window during which writes for a moving key go to both the old and the new
home, so nothing written after the backfill has copied a page is lost. It must be
on **everywhere** before the backfill starts, and reads must not flip until it is
confirmed on everywhere. Every resharding data-loss bug is a violation of one of
those two orderings. *Lab 05, `Directory.BeginMove`.*

### Cutover

The moment reads flip from the old home to the new one and the dual-write window
closes. In production this is a write to the metadata store that every router must
observe before any router stops dual-writing, which is why the store needs atomic
writes, watches, and a way to fence a router that has cached an old placement. A
JSON file, as used in these labs, has none of that. *Lab 05,
`Directory.Commit`, `Cluster.SetTopology`.*

### Backfill

Copying the existing rows for a moving key to their new home while the system stays
online. It is the easy half of a reshard and the half most write-ups focus on. In
real systems it is built on logical replication rather than a copy loop, because
replication carries the ongoing changes too. *Labs 05 and 07.*

---

## Replication and MVCC

### Replica identity

The set of columns logical replication puts in the WAL so the subscriber can find
the row an `UPDATE` or `DELETE` refers to. The default is the primary key; a table
with none will reject those operations on the publisher once it is in a
publication. `REPLICA IDENTITY FULL` works by logging every column and matching on
all of them, which is correct and expensive. *Lab 07.*

### Replication slot

A server-side bookmark that guarantees the publisher retains WAL until the consumer
has confirmed reading it. This is the feature that makes logical replication
reliable and the feature that fills your disk: a slot whose consumer has gone away
retains WAL forever, on the primary, which is the node you can least afford to
lose. Alert on slot lag from the first day you create one. *Lab 07.*

### xmin horizon

The oldest transaction id that any still-running transaction might need to see.
`VACUUM` cannot reclaim a dead tuple newer than the horizon, so anything that holds
the horizon back, a long-running query, an idle-in-transaction session, an
abandoned replication slot, or a forgotten prepared transaction, stops space
reclamation across the whole database, not just the table involved. *Lab 04, via
`shardctl orphans`.*

### Bloat

Dead tuples and free space occupying pages that a table or index still owns.
Ordinary `UPDATE` traffic creates it continuously because Postgres writes a new row
version rather than modifying in place. `VACUUM` makes the space reusable but
usually does not return it to the filesystem; only `VACUUM FULL` (which takes an
`ACCESS EXCLUSIVE` lock) or `pg_repack` shrinks the file. *Lab 00 step 4.*

---

## Tenants that misbehave

### Hot tenant

A tenant whose request rate, rather than its size, is the problem. Hash routing
sends all of its traffic to one node, so it saturates that node's CPU or its
connection pool while the rest of the cluster idles. The fix is a pin: route it
somewhere of its own. *Lab 03 step 3, `Directory.Pin`.*

### Whale

A tenant whose data volume dwarfs the median, often by two or three orders of
magnitude. Whales break the assumption every hashing scheme is built on, that keys
are interchangeable, and no amount of adding shards helps because a single tenant
cannot be split by hashing its id. Find yours with the skew queries in lab 03 step
2, and build the escape hatch before you need it.

### Noisy neighbour

The consequence rather than the cause: a tenant that degrades service for the
unrelated tenants sharing its node. It is the reason a directory topology beats
consistent hashing in multi-tenant systems, because you can move the offender
specifically rather than reshuffling a fraction of everybody.

---

## Cross-shard writes

### Two-phase commit (2PC)

`PREPARE TRANSACTION` on every participant, then `COMMIT PREPARED` on every
participant, giving atomicity across nodes. It works, and you should almost never
use it: a prepared transaction survives crashes and holds its locks and its xmin
horizon until someone resolves it, the coordinator becomes a new single point of
failure needing its own durable log, and there is a window between prepare and
commit that no amount of careful code removes. *Lab 04, `internal/shard/txn.go`.*

### Outbox

Writing the local effect and a record of "this must also happen elsewhere" into the
same shard in the same ordinary transaction, then having a relay deliver the second
part and retry until it succeeds. You give up atomicity, so there is a window where
one shard reflects the change and the other does not, and you gain the removal of
distributed commit entirely: no coordinator, no held locks, and failure handling
that is just retry. *Lab 04, `internal/shard/outbox.go`.*

### Idempotency key

A stable identifier for an effect, stored by the receiver with a unique constraint,
so that applying the same effect twice is a no-op rather than a double charge. In
this repo it is the `applied` table keyed by event id. It is the single mechanism
that makes the outbox pattern safe, and it belongs on the receiving side, because
the sender cannot know whether its previous attempt landed. *Lab 04,
`labs/04-app-sharding-go/schema.sql`.*

### At-least-once

The delivery guarantee any retrying relay actually provides: crash after applying
but before marking the message delivered, and it will be applied again. Exactly-once
delivery over an unreliable network is not achievable, so systems that claim it are
describing something else.

### Effectively-once

At-least-once delivery plus idempotent application: the message may arrive many
times, but its effect happens once. This is what production systems mean when they
say exactly-once, and it is worth insisting on the distinction, because the
difference is a table with a primary key that somebody has to remember to create.
