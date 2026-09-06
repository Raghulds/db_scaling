# application-level sharding in Go

A partitioned table is still one table: one planner, one snapshot, one `COMMIT`, one `VACUUM`,
one backup.

This lab is the hinge. From here there are N databases that have never heard of
each other. Nothing is transactional across them. There is no planner that can
see all your rows, no snapshot that spans them, no foreign key that crosses
them, and no `COMMIT` that means anything beyond one node. Every convenience you
had in lab 03 is now a thing you build, operate, and get paged for.

The code under `internal/shard/` is that set of things. It is deliberately small
-- about 1,500 lines, one dependency (pgx) -- because the point is not the code.
The point is that this is the *minimum*: routing, fan-out, cross-shard ordering,
id generation, and two mutually exclusive answers to "write to two shards at
once". You cannot shard without building all of it. Read the files before you
run anything.

---

## Run it

Start the four shard containers (ports 55440-55443):

```bash
make shards-up
```

Apply `schema.sql` to every shard. Idempotent, safe to re-run:

```bash
make shards-init
```

Run the whole lab: unit tests, then bring the shards up, seed them, and run the
guided tour:

```bash
make lab04
```

Re-run just the tour without reseeding:

```bash
make lab04-demo
```

`make lab04` builds `./bin/shardctl` and `./bin/reshard`. Everything below drives
`shardctl` directly. It reads `SHARDS` (default 4), `SHARD_BASE_PORT` (55440),
and `TOPOLOGY_PATH` (default `tmp/topology.json`) from the environment.

Compare the three routing strategies. No database needed -- this is pure
arithmetic over sampled tenant ids:

```bash
./bin/shardctl topology
```

Load orders. Tenants 1 and 2 are seeded 20x larger than the rest so the labs
have whales to find:

```bash
./bin/shardctl seed -tenants 500 -per-tenant 40
```

Per-shard rows, distinct tenants, and on-disk size:

```bash
./bin/shardctl stats
```

The fast path: resolve one tenant to one shard and read it:

```bash
./bin/shardctl tenant 1
```

Scatter-gather with per-shard timings. Add `-partial` to return what succeeded
instead of failing:

```bash
./bin/shardctl count
```

Cross-shard keyset pagination, printing read amplification per page:

```bash
./bin/shardctl page -pages 3 -limit 10
```

Snowflake ids, encoded and decoded:

```bash
./bin/shardctl ids -n 5 -shard 3
```

A cross-shard write via `PREPARE TRANSACTION`. Add `-crash` to exit in the
window between prepare and commit:

```bash
./bin/shardctl transfer 2pc -from 1 -to 2 -cents 500
```

The same write via the outbox, including a deliberate duplicate delivery:

```bash
./bin/shardctl transfer outbox -from 1 -to 2 -cents 500
```

List prepared transactions nobody resolved. Add `-resolve` to roll them back:

```bash
./bin/shardctl orphans
```

Find rows sitting on a shard the current topology would not route them to:

```bash
./bin/shardctl audit
```

---

## The map

| file | what it solves | the thing it encodes |
| --- | --- | --- |
| `hash.go` | `Key(tenantID) uint64` -- the one hash every routing decision goes through | Changing this function silently relocates every tenant, so it is pinned by golden tests in `hash_test.go` and never "improved". Stability beats quality. |
| `topology.go` | `ModN`, `Ring`, `Directory` behind one `Topology` interface | Mod-N can only ever double. Consistent hashing moves ~1/n but you do not choose *which* keys. A directory costs you a lookup table and buys you the ability to move one tenant on a Tuesday. |
| `store.go` | `LoadOrInitDirectory` -- the placement map on disk | A file has no atomic multi-reader visibility and no watch. Production uses etcd/Consul because the dangerous state is "one router still believes the old placement". |
| `cluster.go` | one `pgxpool.Pool` per shard, plus `ReadPool`/`WritePools` | Pool sizing became a multiplication problem the moment you sharded. See the arithmetic below; it bites before anything interesting does. |
| `scatter.go` | generic `Scatter` with per-shard timeout, concurrency cap, and `AllowPartial` | The p99 of a fan-out is the p99 of the *slowest* shard, so the interesting statistic is `max`, never `mean`. `SlowestShard` exists to make you look at it. |
| `keyset.go` | `Cursor`, `MergeDesc`, `PageRecent` -- the global feed | `OFFSET` is not implementable across shards. Keyset is, at a cost of `limit * shards` rows read per page. |
| `id.go` | snowflake ids: 41 bits ms, 10 bits shard, 12 bits sequence | A per-node `bigserial` hands out the same integer on every shard. Random UUIDs destroy B-tree insert locality. Time-ordered ids fix both, and the embedded shard is a *hint*, never a route. |
| `orders.go` | `Insert`, `Seed`, `SummarizeTenant`, `StateCounts`, `Stats`, `Misplaced` | `StateCounts` returns a number the system was never actually in: each shard answered as of its own commit point. Fine for a dashboard, wrong for "did we oversell". |
| `txn.go` | `TwoPC.Do`, `Orphans` -- atomicity across shards | A prepared transaction survives a crash, holds its locks, and pins the xmin horizon until a human resolves it. `Orphans` is an alert, not a command. |
| `outbox.go` | `Outbox.Transfer`, `RelayOnce`, `PendingOutbox` -- what to do instead | The local effect and the record of "this must also happen elsewhere" commit on *one* node in *one* transaction. Delivery is then at-least-once, and the `applied` table is what makes that safe. |

### A wrong number to keep in view

`./bin/shardctl topology` prints a `ring` column in the balance table that looks
like this on a clean checkout:

```
shard  mod-N  ring   directory
0      25001  50336  24998
1      24999  0      25002
2      25000  0      24997
3      25000  49664  25003
```

Shards 1 and 2 receive nothing. That is not a property of consistent hashing and
it is not what "fewer vnodes means worse spread" refers to -- it is a defect in
how `Ring.Add` derives vnode positions. Exercise 2 asks you to find it. Diagnose
it before you read anyone's explanation; a hash function that quietly fails to
spread is the most expensive bug in this whole subject area, and it never
announces itself.

---

## The flows

### 1. Single-tenant read -- the fast path

This is the query you sharded *for*. One tenant, one shard, one index scan, no
coordination with anything.

```
  tenant_id = 1
        |
        v
  shard.Key(1)                    FNV-1a over 8 big-endian bytes
        |  = 12161961113530546194
        v
  Directory.Logical(1)            hash % LogicalCount (1024)
        |  = logical 18           <-- FIXED FOREVER. No tenant is ever rehashed.
        v
  Directory.Placement[18]         a plain []int you are free to edit
        |  = physical 0           <-- MUTABLE. This is the whole scaling story.
        v
  Cluster.Pool(0)  ------------>  dbs-shard0 :55440
        |
        v
  SELECT count(*), sum(total_cents) FROM orders WHERE tenant_id = 1
  using orders_tenant_created_idx
```

Two levels of indirection instead of one, and the second level is the reason the
design works. `LogicalCount` is chosen once and generously (1024 here) and never
changes, so `Key` never has to be re-run against a different modulus. Scaling out
is editing `Placement`. You can move one logical shard or five hundred, move a
specific noisy neighbour, or bypass the hash entirely with `Directory.Pin` to put
a whale on dedicated hardware.

`Directory.Pin` is the hot-tenant escape hatch. Build it on day one; you will use
it on day four hundred, at 2am, when one customer's bulk import is making three
hundred other customers slow. Hashing cannot fix skew -- it distributes tenants
evenly, and tenants are not evenly sized.

### 2. Write, and the dual-write window

`Cluster.WritePools` returns a slice, not a single pool, and `orders.Insert`
loops over it:

```
  Insert(ctx, c, id, tenantID, cents, state)
        |
        v
  Cluster.WritePools(tenantID) -> Topology.RouteWrite(tenantID)
        |
        +-- normal case:        [0]        one shard
        +-- migration open:     [0, 3]     source AND target
```

`Directory.RouteWrite` returns two shards only while `Migrating[logical]` is set
-- the window opened by `BeginMove` and closed by `Commit`. During that window
reads still go to the old home and writes go to both, because the backfill copies
rows page by page and must not lose a write that lands behind the copy cursor.

Three things about that loop worth internalising:

- **It is not atomic.** The inserts run sequentially against two independent
  databases. If the second fails, the source has the row and the target does not.
  Dual-write is not a correctness mechanism on its own; the pattern is
  dual-write *plus verify plus repair*. `orders.Misplaced` (`shardctl audit`) is
  the verify half.
- **`Directory.Commit` is the cutover**, and it is the dangerous instant. Every
  router must see the new placement *before* any of them stops dual-writing. A
  router holding a stale copy of `tmp/topology.json` writes to the old shard
  after the flip, and nothing in the system will ever tell you -- except the
  audit.
- **`Seed` does not dual-write.** It routes with `RouteRead` and does one
  `CopyFrom` per shard, because bulk loading through the dual-write path would be
  pointlessly slow. That asymmetry is real in production systems too, and it is a
  good source of migration bugs.

`logical_shard` is stored as a column rather than recomputed in SQL. It is a
small denormalisation that turns every migration query into a cheap range
predicate against `orders_logical_idx` -- "give me every row for logical shards
512 through 767" -- instead of an expression Postgres cannot index.

### 3. Scatter-gather

`Scatter[T]` runs one function on every shard concurrently, bounded by
`Concurrency`, each under its own `PerShardTimeout`, and returns results sorted by
shard id with per-shard errors and durations attached.

Measured on this laptop, four healthy shards, ~21,500 rows:

```
shard  took     error  rows
0      5.06ms   -      5760
1      6.31ms   -      4960
2      7.38ms   -      5760
3      7.59ms   -      5040

wall clock 8ms; slowest shard 3 at 8ms
```

Wall clock tracks the maximum, not the mean, and it always will. This is the
single most important operational fact about fan-out: **adding shards makes a
scatter query slower**. Four shards means four chances to hit a slow one; sixteen
means sixteen. If each shard independently has a 1% chance of a slow response,
your fan-out is slow 4% of the time at four shards and 15% of the time at
sixteen. The fast path gets better as you shard. The fan-out gets worse. Design
your product surface accordingly.

`PerShardTimeout` (5s by default) is the circuit breaker that stops one sick node
from making every scatter query in the system slow -- the standard way a partial
outage becomes a total one.

`AllowPartial` is not a performance option. It is a correctness decision, and you
make it per endpoint:

- With `AllowPartial: false`, `Scatter` joins the per-shard errors and the caller
  gets a hard failure. Loud, honest, and unavailable.
- With `AllowPartial: true`, the caller gets a number. When one of four shards is
  unreachable, that number is roughly 25% low, arrives with the same latency, and
  carries no marker of its own incompleteness. Exercise 5 makes you watch a total
  drop from 21,520 to 15,760 with nothing in the output to say so.

An undercounted total that looks authoritative is worse than an error. A billing
report or an inventory check must not use `-partial`. A "roughly how many orders
today" tile on an internal dashboard can, provided the tile renders which shards
answered.

### 4. Cross-shard keyset pagination

`Cluster.PageRecent` asks every shard for a full page, then merges:

```
  PageRecent(ctx, cursor, limit=10)
        |
        v
  Scatter -> each shard: SELECT ... WHERE (created_at, id) < ($1, $2)
                         ORDER BY created_at DESC, id DESC LIMIT 10
        |                using orders_created_id_idx
        v
  MergeDesc([][]Order, 10)   pure function, no database, unit-tested
        |
        v
  10 rows + next Cursor{CreatedAt, ID}  -->  base64, opaque, stateless
```

**Why every shard is asked for a full page.** To be certain of the global top 10
you need 10 candidates from each shard, because in the worst case all 10 winners
live on one node. So you fetch `limit * shards` rows and discard most of them.
The CLI prints the ratio:

```
fetched 20 rows from 4 shards to return 5 (4x read amplification) in 10ms
```

Read amplification is linear in shard count and does not decay with depth. This
is why "just add shards" makes a global activity feed *worse*, and why systems
that need one maintain a separate denormalised feed rather than merging on read.

**Why the cursor is `(created_at, id)` and not `created_at`.** Rows sharing a
timestamp would otherwise be skipped or repeated at page boundaries. The tiebreak
makes the sort total. Across shards this matters more than on one node, because
`Seed` writes timestamps generated by the app and collisions are common.

**Why `OFFSET` is impossible.** There is no way to ask shard 2 for "rows 40
through 49 of the merged ordering" -- shard 2 has no idea what the merged
ordering is. The only way to honour `OFFSET 40` is to read the first 50 rows from
*every* shard and discard 40 after merging, so the cost is O(offset) per shard
per page and grows without bound as users page deeper. Keyset pagination costs
the same for page 1 and page 10,000, and the cursor is stateless, so any app
instance can serve the next page.

Note also the two literal SQL strings in `PageRecent` rather than one query with
`OR $1 IS NULL`. An `OR` over a parameter forces a filter the planner cannot
convert into an index range scan, so the tidier single-query version quietly
sequential-scans every shard.

### 5. Id generation

```
  63                    22        12         0
   +---------------------+---------+---------+
   | 41 bits: ms since   | 10 bits | 12 bits |
   |   2024-01-01 epoch  |  shard  |   seq   |
   +---------------------+---------+---------+
```

**Why not `bigserial`.** A sequence is per-node. Shard 0 and shard 3 both hand
out 1000, and now you cannot merge two result sets, cache by id, or grep two log
streams together. Every id collision is silent.

**Why not UUIDv4.** Random ids land on a random B-tree page on every insert. You
lose insert locality, your index working set becomes the whole index instead of
its right-hand edge, and write amplification goes up. UUIDv7 is the modern answer
and has the same time-ordered property as this layout.

**Why the shard bits.** They make the shard recoverable from the id, so
`GET /orders/{id}` can pick a pool without a lookup. That is a genuine
convenience and a genuine trap, because it welds the id format to the topology.
After a tenant moves shard, its existing ids still claim the old one. Treat the
encoded shard as *where this row was created*, never as *where this row is*.
Exercise 10 has you pin a tenant and then decode its ids to watch the two
disagree.

`IDGen.Next` also refuses to move backwards when the clock does (NTP step, VM
migration). Going backwards would mint duplicate ids; stalling merely makes ids
briefly non-monotonic against wall time. And 12 sequence bits means 4,096 ids per
millisecond per generator, after which it spins to the next millisecond rather
than reusing one.

### 6. Cross-shard write via 2PC

`TwoPC.Do` runs one function per shard, then `PREPARE TRANSACTION` on each, then
`COMMIT PREPARED` on each.

```
  phase 1   shard 0: BEGIN; UPDATE ledger -500; PREPARE TRANSACTION 'xfer-N-0'
            shard 2: BEGIN; INSERT ledger +500; PREPARE TRANSACTION 'xfer-N-2'

  >>>>>>>>>>>>>>>>>>>>>>  THE WINDOW  <<<<<<<<<<<<<<<<<<<<<<
  every shard has promised it can commit. nothing has committed.
  the process dies here and both promises outlive it, forever.

  phase 2   shard 0: COMMIT PREPARED 'xfer-N-0'
            shard 2: COMMIT PREPARED 'xfer-N-2'
```

It works, and you should still almost never use it. What you buy is atomicity
across nodes. What you pay:

- `max_prepared_transactions` defaults to 0, so this fails out of the box. The
  compose file sets it to 10.
- A prepared transaction is durable and session-independent. It survives the
  crash, the restart, and the deploy. It holds its locks until somebody resolves
  it, and "somebody" means a human unless you built a recovery daemon.
- It pins the xmin horizon. `VACUUM` cannot reclaim any tuple newer than the
  prepared transaction's xid, **anywhere in that database** -- not just in the
  tables it touched. Exercise 8 has you watch `VACUUM (VERBOSE)` report
  `removable cutoff: 760` where 760 is the prepared xid, with 800 dead tuples it
  is not allowed to remove.
- Your standard long-transaction monitoring does not see it. A prepared
  transaction is detached from every session, so it does not appear in
  `pg_stat_activity` at all and `max(age(backend_xmin))` will not find it. You
  must query `pg_prepared_xacts`. If your alerting does not, a forgotten prepared
  transaction is a transaction-wraparound outage with a multi-day fuse and no
  warning on the dashboard you actually look at.
- Phase 2 has no rollback. Once any leg has committed, the only correct action is
  to retry the rest until they succeed. `TwoPC.Do` collects and joins the errors;
  it cannot undo.
- The coordinator is a new single point of failure and needs its own durable log
  to answer "was this transaction supposed to commit or abort". This lab
  deliberately does not have one, which is why `orphans -resolve` makes *you*
  decide, by hand, and why the exercise asks you to write down your reasoning.

### 7. Cross-shard write via the outbox

The trick is that the local effect and the record of "this must also happen
elsewhere" go into the *same shard* in the *same transaction*. One node, one
commit, ordinary ACID. No prepared state, no coordinator, no held locks across
nodes, and failure handling reduces to "retry".

```
   shard 0  (source, tenant 1)                shard 2  (target, tenant 2)
   +---------------------------------+        +-----------------------------+
   | BEGIN                           |        |                             |
   |   UPDATE ledger  -500           |        |                             |
   |     WHERE balance_cents >= 500  |        |                             |
   |   INSERT INTO outbox (credit)   |        |                             |
   | COMMIT   <- one node, plain ACID|        |                             |
   +---------------------------------+        +-----------------------------+
              |                                             ^
              |  <=== the system is now INCONSISTENT ===>    |
              |       tenant 1: 999500   tenant 2: 1000000  |
              |                                             |
              |  Outbox.RelayOnce(ctx, srcShard, batch)     |
              |    SELECT ... WHERE delivered_at IS NULL    |
              |    (outbox_pending_idx, a partial index)    |
              +---------------------------------------------+
                                                            |
                                        BEGIN
                                          INSERT INTO applied (event_id)
                                            ON CONFLICT DO NOTHING
                                          if RowsAffected == 0: return  <- dupe
                                          UPDATE ledger +500
                                        COMMIT
                                                            |
              +---------------------------------------------+
              |  UPDATE outbox SET delivered_at = now()
              v
        crash between that COMMIT and this UPDATE
          => the relay delivers the event again
          => the `applied` primary key absorbs it
```

**What you gave up: atomicity.** There is a real interval where shard 0 shows the
debit and shard 2 does not. `shardctl transfer outbox` prints the balances inside
that window on purpose. Every reader in your system must tolerate it, and that
tolerance is a product decision, not an implementation detail. "Money left my
account and has not arrived yet" is an acceptable state to show a user. "Your
order shipped but does not exist" is not.

**What makes it correct: idempotency on the receiver.** The relay delivers *at
least once*, by construction -- crash after the apply commits and before
`delivered_at` is set, and the event is redelivered. That is not a bug to be
engineered away; it is the contract. The `applied` table, with a primary key on
`event_id`, is what converts at-least-once delivery into effectively-once
application. Note that the dedupe insert and the effect share one transaction: if
they did not, a crash between them would leave the event marked applied but never
applied.

**The window of inconsistency is bounded by the relay, and the relay is a thing
you must run.** `PendingOutbox` is the lag metric. If it grows without bound, the
relay is down and your shards are silently diverging while every individual query
still returns instantly and looks fine.

Exercise 9 has you delete one row from `applied` and replay. The credit applies a
second time, tenant 2 gains 500 cents that tenant 1 was never debited, and money
is created from nothing. That one row is the entire safety property.

---

## Connection pool arithmetic

This is the operational surprise that arrives first, before any of the
interesting ones, and it is pure multiplication.

`cluster.Open` configures each shard's pool with `MaxConns = 8`, `MinConns = 1`.
Every app instance opens a pool to *every* shard.

```
  backends on ONE shard   =  pool size  x  app instances
  backends across fleet   =  pool size  x  app instances  x  shards
```

With this lab's settings and Postgres' default `max_connections = 100` (verified
with `SHOW max_connections` on `dbs-shard0`), minus the default
`superuser_reserved_connections = 3`:

```
  97 usable / 8 conns per instance  =  12 app instances
```

The 13th pod gets `FATAL: sorry, too many clients already` -- and it gets it from
*whichever shard it contacts first*, so the failure looks like a random shard
being down rather than a capacity limit you set.

Now scale it. Thirty pods, eight shards, the same pool size:

```
  per shard:  8 x 30            =    240 backends
  fleet:      8 x 30 x 8        =  1,920 backends
```

The trap in that arithmetic: **per-shard connection pressure does not fall when
you add shards.** Going from 8 to 16 shards halves the rows per shard and halves
the disk per shard, and leaves each shard facing the same 240 connections while
doubling the fleet total to 3,840. Sharding solves data volume. It actively
worsens connection count.

**Where Postgres actually falls over.** Every backend is an OS process with
several megabytes of private memory before `work_mem` is allocated per sort or
hash node. Snapshot computation and `ProcArray` scanning are proportional to the
number of backends, so idle-but-connected sessions are not free. Useful
concurrency saturates at a small multiple of core count; past that you are
queueing inside Postgres instead of in your own pool, where you cannot see it or
shed it. Treat `max_connections` above roughly 500 as a design smell rather than
a tuning knob -- raising it moves the cliff, it does not remove it.

**The answer is pgbouncer in transaction pooling mode.** A client connection is
bound to a server connection only for the duration of a transaction, so 240 idle
client connections can share 25 server backends. Session pooling gives you none
of this; transaction pooling is the mode that matters.

**What transaction pooling then forbids**, because two statements from the same
client may land on different backends:

- **Session state.** `SET` outside a transaction, session GUCs, `search_path`
  fiddling. Anything you configure once and expect to persist is gone.
- **Server-side prepared statements.** pgx v5 keeps a statement cache by default
  and will send `EXECUTE` for a statement the new backend never prepared. Either
  set `default_query_exec_mode=exec` (or the simple protocol) in the DSN, or run
  a pgbouncer new enough to track protocol-level prepared statements via
  `max_prepared_statements`.
- **Session-level advisory locks.** `pg_advisory_lock` is held by a session you
  no longer own after `COMMIT`. Use the transaction-scoped
  `pg_advisory_xact_lock` instead, which releases at commit by definition.
- **`LISTEN` / `NOTIFY`.** `LISTEN` is session state and the notification arrives
  on a backend that is no longer yours. This is the usual reason a team keeps one
  small session-pooled route alongside the transaction-pooled one.
- **`WITH HOLD` cursors and temp tables**, for the same reason.

Two notes specific to this lab. `TwoPC.Do` calls `pool.Acquire` explicitly and
holds that connection across `BEGIN`, the work, and `PREPARE TRANSACTION` -- all
inside one transaction, so it survives transaction pooling. `COMMIT PREPARED` is
global rather than session-bound, so it can safely execute on any backend, which
is why phase 2 uses `pool.Exec` and not the held connection.

---

## What sharding took away from you

Stated bluntly, because every one of these will surface as a ticket:

- **No cross-shard `JOIN`.** Joining orders to a table on another shard is now
  two round trips and a merge in Go. Reference data gets copied to every shard;
  everything else gets denormalised or fetched twice.
- **No cross-shard foreign key.** `REFERENCES` cannot span databases. Referential
  integrity between shards is an application invariant and a reconciliation job.
- **No global unique constraint.** `UNIQUE (email)` holds within one shard only.
  Global uniqueness needs a separate allocator -- typically a small dedicated
  table or service that owns the namespace and is consulted before the insert.
- **No global sequence.** Hence `id.go`. Every id scheme you might reach for
  instead is either not unique or not ordered.
- **No consistent snapshot.** `StateCounts` queries N shards at N different commit
  points. The total is a state the system was never in. There is no
  `REPEATABLE READ` that spans nodes.
- **No cross-shard transaction without 2PC**, and 2PC is a prepared-transaction
  liability with a `VACUUM` failure mode attached. The realistic answer is the
  outbox, and the outbox is eventual consistency, which is a change to your
  product's semantics rather than to its implementation.
- **Schema migrations run N times and can be half-applied.** `ALTER TABLE` fails
  on shard 5 of 8 and your fleet now has two schemas. Every migration must be
  backward compatible with the un-migrated shards for the whole rollout, and your
  migration tool needs per-shard state and a resume path.
- **Backups and PITR are per-shard and not aligned in time.** There is no cluster
  restore point. Restoring eight shards to "the same moment" gives you eight
  independently consistent databases whose combined state never existed -- the
  scatter-gather problem again, except permanent and applied to your recovery
  plan.
- **`count(*)` stops being a number.** Every aggregate over all tenants is now a
  fan-out, which means it is slow, non-snapshot, and gets worse as you grow.

---

