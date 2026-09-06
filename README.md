# db-scaling

A hands-on ladder from "one big table hurts" to "reshard a live cluster without
downtime". Eight labs, each one a disposable Postgres environment plus the SQL or
Go that exercises it. You seed real volume, measure the pain, apply the fix, and
measure again. This repo is built to be **run**, not read: the comments in the lab
files carry the lesson, and every lab is written around a failure mode rather than
a feature. Keep a notebook open as you go, because the numbers you record in lab
00 are the argument for everything after it.

---

## Partitioning is not sharding

State this to yourself before you touch anything else, because almost every bad
scaling decision starts by confusing the two.

**Partitioning is one node.** It is native, declarative, and fully transactional.
The planner routes rows for you. Joins, foreign keys, `SERIALIZABLE`, and a single
`COMMIT` all still work. It solves vacuum time, retention cost, index size, and
working-set locality.

**Sharding is N nodes that know nothing about each other.** There is no
transaction across them, no join across them, no unique constraint across them,
and no `ORDER BY ... LIMIT` across them that does not read N times more rows than
it returns. It solves write throughput and storage, and it charges you your data
model, your query layer, and your on-call rotation to do it.

| | Partitioning | Sharding |
|---|---|---|
| Nodes | one | N |
| Who routes | the Postgres planner | your application |
| Atomicity | ordinary ACID everywhere | per shard only |
| Joins | ordinary | within a shard, or reimplemented in the app |
| Unique constraints | must include the partition key | per shard only |
| Global `ORDER BY`/`LIMIT` | one index scan | fan out, over-fetch, merge |
| Solves | vacuum, retention, index size, locality | write throughput, storage, CPU |
| Schema migration | one `ALTER` | N databases, partial failure is normal |
| Reversible | yes, in an afternoon | no |

Most teams that reach for sharding needed partitioning. The symptoms overlap
almost perfectly at the point where somebody first says "we should shard": slow
queries, a table you are afraid of, a `DELETE` that never finishes, autovacuum
falling behind. Every one of those is a partitioning problem. Sharding only
answers "one machine cannot hold this" or "one machine cannot absorb these
writes", and you should be able to state which of the two, with a number, before
you start. [docs/decision-guide.md](docs/decision-guide.md) is the ordered set of
questions that gets you to that number.

---

## Prerequisites

- Docker with Compose v2 (Docker Desktop is fine). Give it at least 4 GB of RAM.
- Go 1.23 or newer. The only module dependency is `github.com/jackc/pgx/v5`.
- A `psql` client, ideally version 16 to match the servers (`brew install libpq`).
  The SQL labs run from your host, not from inside the containers.
- Roughly 10 GB of free disk at the default `ROWS=5000000`. Volumes are per lab
  group and they add up.

Check all of it in one step:

```bash
make doctor
```

`make doctor` verifies the Docker daemon is reachable, that `go` and `psql` are on
your `PATH`, and warns if any lab port is already bound. Fix anything it marks
`FAIL` before going further.

---

## Quickstart

Clone and enter the repo:

```bash
git clone <this-repo> db-scaling && cd db-scaling
```

Confirm the machine is ready:

```bash
make doctor
```

Start the single-node lab Postgres (this builds the `pg_partman` image the first
time, so allow a few minutes):

```bash
make up
```

Run lab 00 and let it hurt. It creates one flat 5M-row table, measures it, bloats
it with an update, then times a retention `DELETE`:

```bash
make lab00
```

Write down three numbers from the tail of that run: the `DELETE` wall time, the
`VACUUM` wall time after it, and the heap size before and after (it does not
shrink).

Now run the same data shape as a partitioned table:

```bash
make lab01
```

Compare the tail of lab 01 step 5 against the tail of lab 00 step 5. The same
retention job is a `DETACH CONCURRENTLY` plus a `DROP TABLE`: no dead tuples, no
vacuum debt, space returned to the operating system. That delta is the first
insight, and it is the entire argument for partitioning.

If your machine is slow or short on disk, every lab takes a row count:

```bash
ROWS=1000000 make lab00
```

Everything else is discoverable from:

```bash
make help
```

---

## The lab ladder

Times are for working through the lab and reading the comments, not just watching
it scroll. Seeding dominates the wall clock; at `ROWS=5000000` expect minutes, not
seconds, for labs 00 through 03.

| Lab | What it teaches | Time | Needs running |
|---|---|---|---|
| [00 baseline](labs/00-baseline/) | Why one big table hurts: index size overtaking the heap, MVCC bloat, `VACUUM` scaling with table size, and a retention `DELETE` that costs more than the data was worth | 45 min | `pg` (`:55432`) |
| [01 range partitioning](labs/01-range-partitioning/) | Declarative `PARTITION BY RANGE`, plan-time and run-time pruning, the queries that silently lose pruning, and the six limitations that decide whether partitioning is viable at all | 60-90 min | `pg` |
| [02 retention and rollover](labs/02-retention-rollover/) | The `ATTACH` validation scan and how a `CHECK` constraint turns a four-minute lock into two milliseconds; hand-rolled create-ahead/retire-behind in plpgsql; the same job under `pg_partman` | 60 min | `pg` |
| [03 hash and list, multi-tenant](labs/03-hash-list-multitenant/) | `HASH` by tenant, the skew it cannot fix, measuring imbalance, and `LIST` + hashed `DEFAULT` as the hot-tenant escape hatch | 60 min | `pg` |
| [04 application sharding in Go](labs/04-app-sharding-go/) | Routing (mod-N vs consistent hash vs directory), scatter-gather and its p99, cross-shard keyset pagination, snowflake ids, 2PC and the orphaned prepared transaction, the outbox pattern and idempotency | 2-3 h | shards 0-3 (`:55440-55443`) |
| [05 resharding](labs/05-resharding/) | A live 4 to 8 reshard: plan, dual-write window, backfill, verify, cut over, and the failure modes on each side of the cutover | 2-3 h | shards 0-7 (`:55440-55447`) |
| [06 Citus](labs/06-citus/) | What a distributed-Postgres extension gives you for free, distribution columns, colocation, reference tables, and the queries it still cannot make cheap | 90 min | Citus coordinator (`:55450`) + 2 workers |
| [07 logical replication](labs/07-logical-replication/) | Publications, subscriptions, replica identity, replication slots and retained WAL, and why this is the primitive underneath every zero-downtime shard split | 60-90 min | pub/sub pair (`:55460`, `:55461`) |

Labs 00-03 are single node and each one builds on the previous table. Labs 04-05
need the shard group. Labs 06 and 07 are independent of everything else and can be
run in any order once you have finished 05.

Each lab directory has its own `README.md` with the walkthrough, the expected
shape of the output, and the exercises. Start there; this file is only the map.

---

## Repository layout

```
.
├── README.md                     you are here: the map and the quickstart
├── Makefile                      every entry point; `make help` lists them
├── docker-compose.yml            all 14 Postgres containers, behind profiles
├── docker/
│   └── partman/Dockerfile        postgres:16 + pg_partman, for lab 02
├── docs/
│   ├── curriculum.md             the ladder in depth: checkpoints and what to record
│   ├── data-model.md             one entity, modelled seven ways, lab by lab
│   ├── decision-guide.md         should you partition, shard, or neither
│   ├── glossary.md               the terms that get muddled, defined precisely
│   ├── postgres-commands.md      PG 16 SQL + psql reference, basic to advanced
│   └── resources.md              annotated reading list, ordered by signal per hour
├── labs/
│   ├── 00-baseline/              flat table, bloat, and the retention DELETE
│   ├── 01-range-partitioning/    RANGE partitioning, pruning, limitations
│   ├── 02-retention-rollover/    ATTACH cost, rollover automation, pg_partman
│   ├── 03-hash-list-multitenant/ HASH by tenant, skew, LIST hot-tenant split
│   ├── 04-app-sharding-go/       per-shard schema; the Go labs live in internal/
│   ├── 05-resharding/            the 4 -> 8 online reshard runbook, plus verify.sql
│   ├── 06-citus/                 distributed tables, colocation, cross-shard cost, rebalance
│   └── 07-logical-replication/   publisher/subscriber pair, slot mechanics, failure drills
├── internal/shard/               the shard router, as a library you can read
│   ├── hash.go                   the one hash function; why it must never change
│   ├── topology.go               ModN, Ring, Directory, plus churn analysis
│   ├── store.go                  where the placement map lives (a file, standing in for etcd)
│   ├── cluster.go                one pgx pool per shard; read/write pool resolution
│   ├── scatter.go                bounded fan-out with per-shard timeouts and partials
│   ├── keyset.go                 cross-shard keyset pagination and the merge
│   ├── id.go                     snowflake ids: time | shard | sequence
│   ├── orders.go                 the workload: seed, single-tenant read, fan-out aggregates
│   ├── txn.go                    two-phase commit, and the orphan it leaves behind
│   ├── outbox.go                 the outbox pattern and effectively-once apply
│   └── *_test.go                 hash, topology, keyset and id: no database needed
├── cmd/
│   ├── shardctl/                 lab 04 driver: routing, reads, writes, audit, demo
│   └── reshard/                  lab 05 driver: plan and execute the 4 -> 8 move
├── scripts/
│   ├── env.sh                    connection settings every other script sources
│   ├── doctor.sh                 pre-flight checks
│   ├── psql.sh                   psql into any lab node by name
│   ├── wait-for-pg.sh            block until a port answers
│   ├── runlab.sh                 run one lab directory's .sql files in order
│   ├── shards-init.sh            apply the shard schema to N shards
│   ├── citus-register.sh         register the two Citus workers with the coordinator
│   └── lab07.sh                  drive the two-host logical replication lab
└── bin/                          built binaries (gitignored)
```

---

## Ports

All ports are in the 554xx range on purpose: they will not collide with a local
Postgres on 5432 or a second one on 5433. Every database is `lab/lab/lab`
(user/password/database).

| Host port | Container | Used by | Compose profile |
|---|---|---|---|
| 55432 | `dbs-pg` | labs 00-03 | default (always on) |
| 55440-55443 | `dbs-shard0` - `dbs-shard3` | labs 04, 05 | `shards` |
| 55444-55447 | `dbs-shard4` - `dbs-shard7` | lab 05 (the new half) | `reshard` |
| 55450 | `dbs-citus-coordinator` | lab 06 | `citus` |
| (none) | `dbs-citus-worker1`, `dbs-citus-worker2` | lab 06 | `citus` |
| 55460 | `dbs-logical-pub` | lab 07 | `logical` |
| 55461 | `dbs-logical-sub` | lab 07 | `logical` |

The Citus workers deliberately have no host port. They are reachable only from the
coordinator, over the compose network, by container name, which is exactly the
topology you want to be thinking in during lab 06.

Connect to any of them by name:

```bash
scripts/psql.sh shard 2
```

---

## Where the real learning is

Partition syntax is a day. `PARTITION BY RANGE`, `ATTACH`, `DETACH`, pruning: you
will have all of it after lab 01, and the reference documentation is good. Do not
mistake that for the hard part.

The hard-won knowledge is concentrated in two places.

**Lab 04's cross-shard queries.** Every single-shard operation is boring, and
every cross-shard operation is a design decision you will live with for years.
Look closely at what `shardctl count` costs against what `shardctl tenant 7`
costs, and at how many rows `shardctl page` reads to return fifty. Read
amplification is linear in shard count, which means adding shards makes your
global feed worse, not better. That is the sentence most sharding write-ups omit.
Then kill `shardctl transfer 2pc` mid-commit and run `shardctl orphans`: a
prepared transaction that nobody resolves holds its locks and pins the xmin
horizon so `VACUUM` stops reclaiming anywhere in that database. That is how a
clever atomicity solution becomes a slow-motion outage.

**Lab 05's cutover.** Copying data is the easy half and it is the half everyone
writes about. The difficulty is the window: while the backfill runs, writes must
land on both the old and the new home; the moment reads flip, every router in the
fleet must already have stopped believing the old placement. A router holding a
stale placement map and writing to the pre-cutover shard is the classic resharding
data-loss bug, and lab 05 is built so you can cause it on purpose, see it in
`shardctl audit`, and then fix the ordering. If you take one thing from this repo
into production, take the ordering of that cutover.

---
## Where to go next

- [docs/curriculum.md](docs/curriculum.md) - the ladder in depth, with a
  "you are ready to move on when" checkpoint per lab and a table of the numbers to
  record so you finish with your own comparative data set.
- [docs/data-model.md](docs/data-model.md) - the schemas side by side, lab 00 to
  lab 07: the columns barely change, so what each rung actually takes away from
  you is easiest to see by reading them against each other.
- [docs/decision-guide.md](docs/decision-guide.md) - the ordered questions to ask
  about a table that is too big, and the shard-key interrogation.
- [docs/glossary.md](docs/glossary.md) - precise definitions for the terms that
  get used interchangeably and should not be.
- [docs/resources.md](docs/resources.md) - what to read, in what order, and what
  each source is not good for.
