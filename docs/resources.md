# Resources

An annotated reading list, ordered by signal per hour rather than by prestige.
Every entry says what it is good for, what it is **not** good for, roughly how long
it takes, and which rung of [the ladder](curriculum.md) to read it at.

Where a link is given, it is one I am confident about. Where a source is named
without a link, the name is precise enough to find in one search, and that is
deliberate: a wrong deep link wastes more of your time than a search does.

## At a glance

| Source | Hours | Read at |
|---|---|---|
| Postgres docs: DDL Partitioning | 1.5 | Before lab 01 |
| Postgres docs: Routine Vacuuming | 1 | Before lab 00 |
| Rogov, *PostgreSQL 14 Internals*: MVCC and vacuum chapters | 4 | Before lab 00, again after lab 04 |
| Kleppmann, *DDIA* chapter 6 | 1.5 | Before lab 04 |
| Notion sharding posts | 1 | Before lab 04, again before lab 05 |
| Figma database scaling post | 1 | Before lab 05 |
| Citus docs: distributed data modeling and multi-tenant | 2 | Before lab 06 |
| Postgres docs: Logical Replication | 1.5 | Before lab 07 |
| Vitess docs: VSchema and resharding | 2 | After lab 05 |
| pg_partman documentation | 1 | During lab 02 |
| pgbouncer documentation | 1 | After lab 04 |
| Crunchy Data blog | ongoing | Anytime |
| Percona blog | ongoing | Anytime |

---

## Tier 1: read these

Companion reference for this repo: [postgres-commands.md](postgres-commands.md) —
deduplicated PG 16 SQL and psql commands, grouped by concept, with lab cross-links.

### Postgres documentation, "Table Partitioning"

<https://www.postgresql.org/docs/current/ddl-partitioning.html>

**Good for:** the authoritative account of declarative partitioning, and unusually
readable for reference documentation. The two sections that repay slow reading are
the declarative-partitioning limitations and the "Best Practices" material near the
end. Almost every constraint lab 01 step 4 demonstrates is stated there in one
sentence, and having read the sentence first makes the failure land harder.

**Not good for:** knowing whether to partition at all, or how to choose a key for a
real workload. It describes the mechanism, not the judgement. It also says nothing
about operating partition rollover, which is why lab 02 exists.

**Time:** 90 minutes for the whole chapter, of which the caveats are 20.

**Read at:** before lab 01, then again after lab 03 when the caveats mean something.

### Postgres documentation, "Routine Vacuuming"

<https://www.postgresql.org/docs/current/routine-vacuuming.html>

**Good for:** understanding the thing partitioning actually rescues you from. Dead
tuples, the visibility map, freezing and transaction id wraparound, and why
autovacuum's cost-based delay makes it slow on exactly the tables you most need it
to be fast on.

**Not good for:** tuning advice with numbers in it. The parameters are documented,
the values that suit your workload are not, and the defaults are conservative in a
way that surprises people at scale.

**Time:** an hour.

**Read at:** before lab 00, so that step 4 confirms something rather than teaching
it from scratch.

### Egor Rogov, *PostgreSQL 14 Internals*

Free PDF, published by Postgres Professional. Search for the title and the author's
name; it is also available in print.

**Good for:** the best explanation in existence of how Postgres actually stores and
cleans up rows. The MVCC, vacuum and buffer cache chapters are directly load-bearing
for this repo: after them you can predict what lab 00 step 4 will print before you
run it. The indexing chapters are excellent secondary material.

**Not good for:** distributed systems, sharding, or anything above a single node. It
is a book about one Postgres instance, thoroughly, and it does not pretend
otherwise. Also worth noting it targets version 14, so a handful of details have
moved on, none of them in the parts that matter here.

**Time:** four hours for the chapters that matter to this repo, considerably more
for the whole book, which is worth it.

**Read at:** the MVCC and vacuum chapters before lab 00. Return to the indexing
chapters after lab 04, when index size per shard has become your problem.

### Martin Kleppmann, *Designing Data-Intensive Applications*, chapter 6

**Good for:** the vocabulary and the shape of the problem, at the right level of
abstraction and without vendor framing. Key-range versus hash partitioning, hot
spots, secondary indexes by document versus by term, rebalancing strategies, and
the argument against `mod n` that lab 04's `shardctl topology` measures for you.
Chapter 5 on replication and chapter 9 on consistency are the natural follow-ups.

**Not good for:** Postgres specifics, or anything you can execute. It is deliberately
implementation-agnostic, so it will not tell you that `CREATE INDEX CONCURRENTLY`
does not work on a partitioned parent. Read it for the map, not the terrain.

**Time:** 90 minutes for chapter 6 alone.

**Read at:** before lab 04. Reading it before lab 01 is also fine, but it will feel
abstract until you have felt a fan-out query.

### Notion engineering, the sharding posts

Two posts on the Notion blog: "Herding elephants: Lessons learned from sharding
Postgres at Notion" and the follow-up about adding capacity again with zero
downtime.

**Good for:** the most honest published account of doing this to a live product.
The first covers choosing the partition key from the application's object graph
rather than from the database, the decision to shard by workspace, and the double-
write plus backfill plus verify plus cutover sequence. The second is the more
valuable of the two if you have already done lab 05, because it is about doing the
same operation a second time with better tooling, which is the situation you will
actually be in.

**Not good for:** copyable technique. They run a specific stack against a specific
data model and the details do not transfer. Read them for the sequencing, the
verification step, and the sizing of the effort in people and months, which is the
number most proposals omit.

**Time:** an hour for both.

**Read at:** the first before lab 04, the second before lab 05.

### Figma engineering, "How Figma's databases team lived to tell the scale"

On the Figma blog. There is a related, also useful post about their vertical
partitioning work that preceded horizontal sharding.

**Good for:** the argument this repo's [decision guide](decision-guide.md) makes,
made by a team that lived it: they exhausted the cheaper options first, split
tables to their own databases before splitting rows across databases, and were
explicit about buying time deliberately. It is also one of the few write-ups that
describes the routing layer and the shadow-read verification honestly.

**Not good for:** a runbook. It is a narrative, at a company with an engineering
team dedicated to the problem, and the interesting decisions are compressed into
paragraphs. Treat it as evidence for the ordering of the rungs, not as instructions.

**Time:** an hour, including the predecessor post.

**Read at:** before lab 05, when you are about to do the cutover yourself.

---

## Tier 2: read these when you reach the rung

### Citus documentation, distributed data modeling and the multi-tenant guide

<https://docs.citusdata.com/>

**Good for:** the clearest published treatment of choosing a distribution column
and of colocation. The multi-tenant application guide is genuinely the best short
piece of writing on sharding a SaaS product by tenant, whether or not you ever run
Citus, because the modelling questions are identical to the ones you answer by hand
in lab 04. The reference-table concept is worth stealing regardless of stack.

**Not good for:** neutrality about whether you need it. It is vendor documentation
and the answer to most questions is Citus. It also under-sells the operational
weight of running a coordinator plus workers, and the query shapes it cannot make
cheap are discussed less prominently than the ones it can.

**Time:** two hours for the modelling and multi-tenant sections.

**Read at:** before lab 06. Reading the multi-tenant guide before lab 03 is also
defensible, and it will make the whale problem clearer earlier.

### Postgres documentation, "Logical Replication"

<https://www.postgresql.org/docs/current/logical-replication.html>

**Good for:** publications, subscriptions, replica identity, conflict handling, the
restrictions (sequences, DDL, truncate behaviour), and the monitoring section on
slots. Read the restrictions list carefully. Most of the surprises people report
about logical replication are documented there in advance.

**Not good for:** operating it at scale. It will not tell you what to alert on,
what an abandoned slot does to your disk on a busy primary, or how to think about
initial-copy duration against your WAL retention. Lab 07 covers those.

**Time:** 90 minutes for the chapter, plus the `CREATE SUBSCRIPTION` and
`pg_replication_slots` reference pages.

**Read at:** before lab 07.

### Vitess documentation, VSchema and resharding

<https://vitess.io/docs/>

**Good for:** seeing the fully-worked version of everything lab 04 and 05 build by
hand. VSchema is a directory topology with vindexes as first-class objects,
including secondary vindexes for routing by a non-primary key, which is the problem
you hit immediately after choosing a shard key. The resharding workflow documentation
(the copy phase, `VDiff` verification, `SwitchTraffic`, and the reverse-replication
that makes the cutover abortable) is the reference design for lab 05's five steps.

**Not good for:** learning by adoption. Vitess is a large system built for MySQL and
it will not transfer to a Postgres deployment. Also, its terminology is entirely its
own (keyspace, vindex, shard, tablet), which takes an hour to absorb before anything
reads clearly.

**Time:** two hours for VSchema plus the resharding workflow.

**Read at:** after lab 05, as the "how the professionals do it" chaser. The reverse
replication idea in particular is the thing lab 05 does not do and you would want.

### pg_partman documentation

<https://github.com/pgpartman/pg_partman>

**Good for:** the concrete list of things a partition manager has to handle that
you would not think of: premake counts, template tables for per-partition objects,
retention with detach-versus-drop, the maintenance procedure and how to schedule it,
and the migration path from an existing table. Read the reference for `part_config`
column by column; each column is a decision somebody had to make.

**Not good for:** understanding partitioning. It assumes you already do. Reaching
for it before lab 01 means learning an abstraction over a mechanism you have not
seen, which is the wrong order.

**Time:** an hour, alongside lab 02 step 3.

**Read at:** during lab 02, after you have written the plpgsql version yourself in
step 2. The comparison is the point.

### pgbouncer documentation

<https://www.pgbouncer.org/>

**Good for:** the pooling modes and exactly what each one breaks. Transaction
pooling is what makes connection counts survivable at N shards times M pods, and it
silently disables session state, prepared statements in some configurations,
advisory locks, `LISTEN`/`NOTIFY`, and anything else that spans statements. Knowing
that list before you deploy it is the difference between a fix and an outage.

**Not good for:** capacity planning. It tells you the knobs, not the numbers, and
the numbers depend on your workload's transaction duration more than anything else.

**Time:** an hour, mostly the FAQ and the pooling-mode section.

**Read at:** after lab 04, when `shards times pool size times pods` has made its
point.

---

## Tier 3: ongoing, not sequential

### Crunchy Data blog

<https://www.crunchydata.com/blog>

**Good for:** consistently high-quality, practical Postgres writing with runnable
examples. Strong coverage of partitioning patterns, `EXPLAIN` reading, indexing,
and query tuning. When you have a specific Postgres question, a search restricted to
this domain is usually a better first move than a general one.

**Not good for:** systematic learning. It is a blog, so coverage is uneven and
depends on what interested the author that month. Post age matters more than usual
in Postgres, where the answer genuinely changes between major versions.

**Time:** 15 minutes per post.

**Read at:** anytime, and specifically whenever a lab raises a question this repo
does not answer.

### Percona blog

<https://www.percona.com/blog/>

**Good for:** the operational and performance-tuning end, with more benchmarks than
most sources publish. Useful on vacuum tuning, bloat measurement, connection
management, and comparisons between approaches with numbers attached.

**Not good for:** design guidance, and the benchmarks are on their hardware with
their workload, so treat the ratios as interesting and the absolutes as
inapplicable. It also covers several databases, so the Postgres material needs
filtering.

**Time:** 15 minutes per post.

**Read at:** anytime, especially when you need an argument with a number in it.

---

## Also worth your time

- **Postgres documentation, "Using EXPLAIN"** and the `EXPLAIN` reference page.
  Half an hour, and it pays for itself in the first lab. Specifically: what
  `BUFFERS` reports and why shared hit versus read is the number to watch.
- **The `pg_stat_statements` documentation.** Fifteen minutes. Sorting by total time
  rather than mean time is the single most useful habit it gives you.
- **Postgres release notes for every major version since the one you run.**
  Partitioning has improved substantially and steadily, and a limitation you
  memorised three versions ago may no longer exist. This is the highest-value
  boring reading on the list.
- **Your own `pg_stat_user_tables` and `pg_stat_user_indexes`.** The most relevant
  data set about your system is the one you already have and have not looked at.
  Lab 00 step 3 is a template for the query.
