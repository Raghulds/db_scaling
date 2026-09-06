// shardctl drives the lab-04 shard cluster: routing, fan-out reads,
// cross-shard pagination, and the two ways to write across shards.
//
//	shardctl topology            compare mod-N / ring / directory routing
//	shardctl seed                load orders into the shards
//	shardctl stats               per-shard rows, tenants, bytes
//	shardctl tenant 42           single-shard read (the fast path)
//	shardctl count               scatter-gather, with per-shard timings
//	shardctl page                cross-shard keyset pagination
//	shardctl ids                 snowflake ids: encode/decode
//	shardctl transfer 2pc        cross-shard write via PREPARE TRANSACTION
//	shardctl transfer outbox     cross-shard write via the outbox pattern
//	shardctl orphans             list (and optionally resolve) prepared xacts
//	shardctl audit               find rows on the wrong shard
//	shardctl demo                run the guided tour
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/raghuls/db-scaling/internal/shard"
)

var (
	nShards  = envInt("SHARDS", 4)
	topoPath = envStr("TOPOLOGY_PATH", shard.DefaultTopologyPath)
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var err error
	switch cmd {
	case "topology":
		err = cmdTopology(args)
	case "seed":
		err = cmdSeed(ctx, args)
	case "stats":
		err = cmdStats(ctx)
	case "tenant":
		err = cmdTenant(ctx, args)
	case "count":
		err = cmdCount(ctx, args)
	case "page":
		err = cmdPage(ctx, args)
	case "ids":
		err = cmdIDs(args)
	case "transfer":
		err = cmdTransfer(ctx, args)
	case "orphans":
		err = cmdOrphans(ctx, args)
	case "audit":
		err = cmdAudit(ctx)
	case "demo":
		err = cmdDemo(ctx)
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `shardctl -- lab 04 shard router

  topology              compare mod-N / ring / directory routing (no database)
  seed [-tenants N]     load orders into the shards
  stats                 per-shard rows, tenants, bytes
  tenant ID             single-shard read
  count [-partial]      scatter-gather across all shards
  page [-pages N]       cross-shard keyset pagination
  ids [-n N]            snowflake id encode/decode
  transfer 2pc|outbox   cross-shard write
  orphans [-resolve]    prepared transactions left behind
  audit                 rows sitting on the wrong shard
  demo                  guided tour

env: SHARDS (default 4), SHARD_BASE_PORT (55440), TOPOLOGY_PATH
`)
}

// ------------------------------------------------------------------ helpers

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// open loads the placement map and opens one pool per shard.
//
// The check in the middle is the one that saves a reader an afternoon. The
// placement file is shared with lab 05's reshard tool, so once you have run a
// 4 -> 8 split it routes tenants to physical shards 4..7 while shardctl still
// defaults to SHARDS=4.
//
// Without the check there are two failures, and the quiet one is much worse.
// A single-tenant read dies with a bare "no pool for shard 5", which at least
// stops you. But stats, count, page and audit are fan-outs over the pools
// this process happens to hold, so they succeed, cover half the cluster, and
// present half the rows as the total. Measured on this repo's own lab data:
// 28560 rows reported against 52560 actually present, and audit calling the
// cluster clean without having looked at four of its eight nodes.
//
// That is the sharding bug that reaches production. A fan-out must know the
// set of shards it is supposed to cover and refuse to answer when it cannot
// reach all of them -- never quietly answer for the subset it has.
func open(ctx context.Context) (*shard.Cluster, *shard.Directory, error) {
	dir, err := shard.LoadOrInitDirectory(topoPath, 1024, nShards)
	if err != nil {
		return nil, nil, err
	}
	if err := checkPlacementFits(dir); err != nil {
		return nil, nil, err
	}
	c, err := shard.Open(ctx, shard.DSNsFromEnv(nShards), dir)
	if err != nil {
		return nil, nil, err
	}
	if err := c.Ping(ctx); err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("%w\n(hint: make shards-up shards-init)", err)
	}
	return c, dir, nil
}

// checkPlacementFits reports whether the placement map can route to a
// physical shard this process has no pool for. Migration targets and pins
// count: a tenant mid-move writes to both nodes, and a pinned whale ignores
// the hash entirely, so either can point past the end of the pool set.
func checkPlacementFits(dir *shard.Directory) error {
	highest, where := -1, ""
	note := func(p int, kind string) {
		if p > highest {
			highest, where = p, kind
		}
	}
	for _, p := range dir.Placement {
		note(p, "placement")
	}
	for _, p := range dir.Migrating {
		note(p, "an open migration window")
	}
	for _, p := range dir.Pins {
		note(p, "a tenant pin")
	}
	if highest < nShards {
		return nil
	}
	return fmt.Errorf(
		"%s routes to physical shard %d via %s, but SHARDS=%d, so this process\n"+
			"opened pools for shards 0..%d only. That file has been resharded since\n"+
			"shardctl last agreed with it (lab 05), and every fan-out below would\n"+
			"quietly report the half of the cluster it can see as the whole truth.\n"+
			"\n"+
			"fix: SHARDS=%d ./bin/shardctl ...   # address the whole cluster\n"+
			"     make reshard-up                # if shards %d..%d are not running yet\n"+
			"\n"+
			"Do not delete the placement file to make this go away. It is the only\n"+
			"record of where the rows went; without it the data is still on shards\n"+
			"%d..%d and nothing knows how to find it.",
		topoPath, highest, where, nShards, nShards-1,
		highest+1, nShards, highest,
		nShards, highest)
}

func tw() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

func header(s string) { fmt.Printf("\n\033[1m== %s\033[0m\n", s) }

func bytesHuman(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// ----------------------------------------------------------------- topology

func cmdTopology(args []string) error {
	fs := flag.NewFlagSet("topology", flag.ExitOnError)
	sample := fs.Int("sample", 100000, "tenants to sample")
	_ = fs.Parse(args)

	from, to := nShards, nShards*2

	modFrom := shard.ModN{N: from}
	modTo := shard.ModN{N: to}

	ringShards := make([]int, from)
	for i := range ringShards {
		ringShards[i] = i
	}
	ringFrom := shard.NewRing(ringShards, 200)
	ringTo := shard.NewRing(ringShards, 200)
	for i := from; i < to; i++ {
		ringTo.Add(i)
	}

	dirFrom := shard.NewDirectory(1024, from)
	dirTo := shard.NewDirectory(1024, from)
	// Move the second half of each physical shard's logical range to a new
	// node. Contiguous, reversible, and inspectable -- unlike a rehash.
	for l := range dirTo.Placement {
		if p := dirTo.Placement[l]; l%(1024/from) >= (1024/from)/2 {
			dirTo.Placement[l] = p + from
		}
	}

	header(fmt.Sprintf("churn when going from %d shards to %d", from, to))
	w := tw()
	fmt.Fprintln(w, "topology\ttenants moved\tnotes")
	fmt.Fprintf(w, "mod-N\t%.1f%%\trehash: you cannot choose which keys move\n",
		100*shard.Churn(modFrom, modTo, *sample))
	fmt.Fprintf(w, "consistent hash\t%.1f%%\tonly moves to the NEW nodes, never between old ones\n",
		100*shard.Churn(ringFrom, ringTo, *sample))
	fmt.Fprintf(w, "directory\t%.1f%%\tsame volume, but you picked it -- and can move 1 at a time\n",
		100*shard.Churn(dirFrom, dirTo, *sample))
	w.Flush()

	fmt.Println("\nmod-N doubling happens to move ~50%. Try an odd target:")
	fmt.Printf("  %d -> %d: %.1f%% of tenants move\n",
		from, from+1, 100*shard.Churn(modFrom, shard.ModN{N: from + 1}, *sample))
	fmt.Println("  That is why mod-N shops can only ever double, and why they")
	fmt.Println("  cannot migrate one tenant at a time to test the machinery.")

	header("balance (tenants per shard, mod-N vs directory)")
	w = tw()
	fmt.Fprintln(w, "shard\tmod-N\tring\tdirectory")
	dm, dr, dd := shard.Distribution(modFrom, *sample), shard.Distribution(ringFrom, *sample), shard.Distribution(dirFrom, *sample)
	for s := 0; s < from; s++ {
		fmt.Fprintf(w, "%d\t%d\t%d\t%d\n", s, dm[s], dr[s], dd[s])
	}
	w.Flush()
	fmt.Println("\n(ring balance is the one to watch: fewer vnodes = worse spread)")
	return nil
}

// --------------------------------------------------------------------- seed

func cmdSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	tenants := fs.Int("tenants", 500, "number of tenants")
	per := fs.Int("per-tenant", 40, "orders per tenant (tenants 1-2 get 20x)")
	_ = fs.Parse(args)

	c, _, err := open(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	start := time.Now()
	n, err := shard.Seed(ctx, c, *tenants, *per, 42)
	if err != nil {
		return err
	}
	fmt.Printf("copied %d orders across %d shards in %s\n", n, nShards, time.Since(start).Round(time.Millisecond))

	// Ledger rows so the transfer demos have balances to move.
	for _, s := range c.Shards() {
		pool, err := c.Pool(s)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO ledger (tenant_id, balance_cents)
			SELECT DISTINCT tenant_id, 1000000 FROM orders
			ON CONFLICT (tenant_id) DO UPDATE SET balance_cents = 1000000`); err != nil {
			return err
		}
	}
	return nil
}

// -------------------------------------------------------------------- stats

func cmdStats(ctx context.Context) error {
	c, _, err := open(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	stats, err := shard.Stats(ctx, c)
	if err != nil {
		return err
	}
	header("per-shard")
	w := tw()
	fmt.Fprintln(w, "shard\trows\ttenants\tsize")
	var totalRows int64
	minRows, maxRows := int64(-1), int64(0)
	for _, s := range stats {
		fmt.Fprintf(w, "%d\t%d\t%d\t%s\n", s.Shard, s.Rows, s.Tenants, bytesHuman(s.Bytes))
		totalRows += s.Rows
		if minRows < 0 || s.Rows < minRows {
			minRows = s.Rows
		}
		if s.Rows > maxRows {
			maxRows = s.Rows
		}
	}
	fmt.Fprintf(w, "total\t%d\t\t\n", totalRows)
	w.Flush()

	if minRows > 0 {
		fmt.Printf("\nimbalance (max/min rows): %.2fx\n", float64(maxRows)/float64(minRows))
		fmt.Println("Tenants 1 and 2 were seeded 20x larger on purpose. Hashing")
		fmt.Println("cannot fix that -- pinning them can. See `Directory.Pin`.")
	}
	return nil
}

// ------------------------------------------------------------------- tenant

func cmdTenant(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: shardctl tenant ID")
	}
	id, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return err
	}

	c, dir, err := open(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	header(fmt.Sprintf("routing tenant %d", id))
	fmt.Printf("  hash(%d)          = %d\n", id, shard.Key(id))
	fmt.Printf("  logical shard     = %d  (hash %% %d, fixed forever)\n", dir.Logical(id), dir.LogicalCount)
	fmt.Printf("  physical shard    = %d  (placement[%d], editable)\n", dir.RouteRead(id), dir.Logical(id))
	fmt.Printf("  write targets     = %v\n", dir.RouteWrite(id))

	start := time.Now()
	s, err := shard.SummarizeTenant(ctx, c, id)
	if err != nil {
		return err
	}
	fmt.Printf("\n  %d orders, %d cents, read from shard %d in %s\n",
		s.Orders, s.Cents, s.Shard, time.Since(start).Round(time.Microsecond))
	fmt.Println("\nOne shard, one index scan. This is the query sharding is for.")
	return nil
}

// -------------------------------------------------------------------- count

func cmdCount(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("count", flag.ExitOnError)
	partial := fs.Bool("partial", false, "return partial results if a shard fails")
	_ = fs.Parse(args)

	c, _, err := open(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	start := time.Now()
	merged, per, err := shard.StateCounts(ctx, c, *partial)
	total := time.Since(start)
	if err != nil && !*partial {
		return err
	}

	header("scatter-gather: orders by state")
	w := tw()
	fmt.Fprintln(w, "shard\ttook\terror\trows")
	for _, r := range per {
		var n int64
		for _, v := range r.Value {
			n += v
		}
		errStr := "-"
		if r.Err != nil {
			errStr = r.Err.Error()
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%d\n", r.Shard, r.Took.Round(time.Microsecond), errStr, n)
	}
	w.Flush()

	slowShard, slowest := shard.SlowestShard(per)
	fmt.Printf("\nwall clock %s; slowest shard %d at %s\n",
		total.Round(time.Millisecond), slowShard, slowest.Round(time.Millisecond))
	fmt.Println("The request is as slow as the slowest shard, always. Adding")
	fmt.Println("shards makes a fan-out query slower, not faster.")

	header("merged result")
	states := make([]string, 0, len(merged))
	for k := range merged {
		states = append(states, k)
	}
	sort.Strings(states)
	w = tw()
	for _, st := range states {
		fmt.Fprintf(w, "%s\t%d\n", st, merged[st])
	}
	w.Flush()
	fmt.Println("\nNot a snapshot: each shard answered as of its own commit point,")
	fmt.Println("so this total is a state the system was never actually in.")
	return nil
}

// --------------------------------------------------------------------- page

func cmdPage(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("page", flag.ExitOnError)
	pages := fs.Int("pages", 3, "how many pages to walk")
	limit := fs.Int("limit", 10, "rows per page")
	_ = fs.Parse(args)

	c, _, err := open(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	var cur shard.Cursor
	for p := 1; p <= *pages; p++ {
		start := time.Now()
		rows, next, err := c.PageRecent(ctx, cur, *limit)
		if err != nil {
			return err
		}
		header(fmt.Sprintf("page %d (cursor %q)", p, cur.Encode()))
		w := tw()
		fmt.Fprintln(w, "created_at\tid\ttenant\tshard\tcents")
		for _, o := range rows {
			fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\n",
				o.CreatedAt.UTC().Format("2006-01-02 15:04:05.000"), o.ID, o.TenantID, o.Shard, o.TotalCents)
		}
		w.Flush()
		fmt.Printf("fetched %d rows from %d shards to return %d (%.0fx read amplification) in %s\n",
			*limit*nShards, nShards, len(rows),
			float64(*limit*nShards)/float64(max(len(rows), 1)), time.Since(start).Round(time.Millisecond))
		if len(rows) == 0 {
			break
		}
		cur = next
	}
	fmt.Println("\nThe cursor is opaque and stateless, so any app instance can")
	fmt.Println("serve the next page. OFFSET could not do this: it would mean")
	fmt.Println("reading and discarding `offset` rows on EVERY shard, per page.")
	return nil
}

// ---------------------------------------------------------------------- ids

func cmdIDs(args []string) error {
	fs := flag.NewFlagSet("ids", flag.ExitOnError)
	n := fs.Int("n", 5, "how many ids")
	sh := fs.Int("shard", 3, "shard to encode")
	_ = fs.Parse(args)

	g, err := shard.NewIDGen(*sh)
	if err != nil {
		return err
	}
	header("snowflake ids")
	w := tw()
	fmt.Fprintln(w, "id\tcreated_at\tshard\tseq")
	for i := 0; i < *n; i++ {
		id := g.Next()
		at, s, seq := shard.DecodeID(id)
		fmt.Fprintf(w, "%d\t%s\t%d\t%d\n", id, at.Format(time.RFC3339Nano), s, seq)
	}
	w.Flush()
	fmt.Println("\nIds are time-ordered, so index inserts stay on the right-hand")
	fmt.Println("edge of the B-tree. The embedded shard is a HINT (where the row")
	fmt.Println("was created) -- after a reshard it is stale, so never route on it.")
	return nil
}

// ----------------------------------------------------------------- transfer

func cmdTransfer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("transfer", flag.ExitOnError)
	from := fs.Uint64("from", 1, "source tenant")
	to := fs.Uint64("to", 2, "destination tenant")
	cents := fs.Int64("cents", 500, "amount")
	crash := fs.Bool("crash", false, "2pc only: die in the window between PREPARE and COMMIT")
	mode := "2pc"
	if len(args) > 0 && args[0][0] != '-' {
		mode, args = args[0], args[1:]
	}
	_ = fs.Parse(args)

	c, _, err := open(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	srcShard := c.Topology().RouteRead(*from)
	dstShard := c.Topology().RouteRead(*to)
	header(fmt.Sprintf("transfer %d cents: tenant %d (shard %d) -> tenant %d (shard %d) via %s",
		*cents, *from, srcShard, *to, dstShard, mode))
	if srcShard == dstShard {
		fmt.Println("(both tenants are on the same shard -- this is just a local")
		fmt.Println(" transaction. Pick tenants that route apart to see the real thing.)")
	}

	before := func() {
		for _, t := range []uint64{*from, *to} {
			s, _ := shard.SummarizeTenant(ctx, c, t)
			pool, _, _ := c.ReadPool(t)
			var bal int64
			_ = pool.QueryRow(ctx, `SELECT balance_cents FROM ledger WHERE tenant_id = $1`, t).Scan(&bal)
			fmt.Printf("  tenant %d on shard %d: balance %d\n", t, s.Shard, bal)
		}
	}
	fmt.Println("\nbefore:")
	before()

	switch mode {
	case "2pc":
		gid := fmt.Sprintf("xfer-%d", time.Now().UnixNano())
		tpc := shard.NewTwoPC(c)

		parts := map[int]func(context.Context, pgx.Tx) error{
			srcShard: func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`UPDATE ledger SET balance_cents = balance_cents - $2
					 WHERE tenant_id = $1 AND balance_cents >= $2`, *from, *cents)
				return err
			},
		}
		if dstShard != srcShard {
			parts[dstShard] = func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`INSERT INTO ledger (tenant_id, balance_cents) VALUES ($1, $2)
					 ON CONFLICT (tenant_id) DO UPDATE SET balance_cents = ledger.balance_cents + $2`,
					*to, *cents)
				return err
			}
		}

		if *crash { // Crash simulation for 2PC
			// This is what a pod eviction looks like from Postgres' side.
			if err := prepareOnly(ctx, c, gid, parts); err != nil {
				return err
			}
			fmt.Println("\nprepared both legs, now exiting WITHOUT committing.")
			fmt.Println("run `shardctl orphans` to see what you left behind.")
			os.Exit(3)
		}
		if err := tpc.Do(ctx, gid, parts); err != nil {
			return err
		}
		fmt.Println("\ncommitted atomically across shards.")

	case "outbox":
		ob := shard.NewOutbox(c)
		eventID := time.Now().UnixNano()
		if err := ob.Transfer(ctx, eventID, *from, *to, *cents); err != nil {
			return err
		}
		fmt.Println("\ndebit committed on the source shard, credit queued in its outbox.")
		fmt.Println("SYSTEM IS NOW INCONSISTENT -- and that is by design:")
		before()

		pending, _ := shard.PendingOutbox(ctx, c)
		fmt.Printf("\npending outbox rows: %v\n", pending)

		n, err := ob.RelayOnce(ctx, srcShard, 100)
		if err != nil {
			return err
		}
		fmt.Printf("relay delivered %d event(s)\n", n)

		// Deliver the same event again on purpose: the `applied` table must
		// absorb it. At-least-once delivery is not a bug to be avoided, it is
		// the contract -- idempotency on the receiver is what makes it safe.
		fmt.Println("\nreplaying the same event to prove idempotency...")
		if err := replayLast(ctx, c, srcShard, eventID); err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown transfer mode %q (want 2pc or outbox)", mode)
	}

	fmt.Println("\nafter:")
	before()
	return nil
}

// prepareOnly runs phase 1 of 2PC and stops there, deliberately.
func prepareOnly(ctx context.Context, c *shard.Cluster, gid string, parts map[int]func(context.Context, pgx.Tx) error) error {
	for shardID, fn := range parts {
		pool, err := c.Pool(shardID)
		if err != nil {
			return err
		}
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return err
		}
		tx, err := conn.Conn().Begin(ctx)
		if err != nil {
			conn.Release()
			return err
		}
		if err := fn(ctx, tx); err != nil {
			_ = tx.Rollback(ctx)
			conn.Release()
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("PREPARE TRANSACTION '%s-%d'", gid, shardID)); err != nil {
			_ = tx.Rollback(ctx)
			conn.Release()
			return err
		}
		conn.Release()
		fmt.Printf("  prepared '%s-%d' on shard %d\n", gid, shardID, shardID)
	}
	return nil
}

// replayLast re-delivers an already-delivered outbox event to show that the
// `applied` table makes duplicate delivery a no-op.
func replayLast(ctx context.Context, c *shard.Cluster, srcShard int, eventID int64) error {
	pool, err := c.Pool(srcShard)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx,
		`UPDATE outbox SET delivered_at = NULL WHERE id = $1`, eventID); err != nil {
		return err
	}
	ob := shard.NewOutbox(c)
	n, err := ob.RelayOnce(ctx, srcShard, 100)
	if err != nil {
		return err
	}
	fmt.Printf("re-delivered %d event(s); balances below should be UNCHANGED\n", n)
	return nil
}

// ------------------------------------------------------------------ orphans

func cmdOrphans(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("orphans", flag.ExitOnError)
	resolve := fs.Bool("resolve", false, "ROLLBACK PREPARED every orphan found")
	_ = fs.Parse(args)

	c, _, err := open(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	orphans, err := shard.Orphans(ctx, c)
	if err != nil {
		return err
	}
	header("prepared transactions")
	if len(orphans) == 0 {
		fmt.Println("none. (try: shardctl transfer 2pc -crash)")
		return nil
	}
	w := tw()
	fmt.Fprintln(w, "shard\tgid\tage")
	for _, o := range orphans {
		fmt.Fprintf(w, "%d\t%s\t%s\n", o.Shard, o.GID, o.Age.Round(time.Second))
	}
	w.Flush()

	fmt.Println("\nEach of these holds its locks and pins the xmin horizon, so")
	fmt.Println("VACUUM cannot clean up ANY table in that database. Left alone,")
	fmt.Println("this is a transaction-wraparound outage with a several-day fuse.")

	if *resolve {
		for _, o := range orphans {
			pool, err := c.Pool(o.Shard)
			if err != nil {
				return err
			}
			if _, err := pool.Exec(ctx, fmt.Sprintf("ROLLBACK PREPARED '%s'", o.GID)); err != nil {
				return err
			}
			fmt.Printf("rolled back %s on shard %d\n", o.GID, o.Shard)
		}
		fmt.Println("\nNote you had to decide commit-vs-rollback by hand. A real")
		fmt.Println("coordinator needs its own durable log to answer that.")
	} else {
		fmt.Println("\nrun with -resolve to clean up")
	}
	return nil
}

// -------------------------------------------------------------------- audit

func cmdAudit(ctx context.Context) error {
	c, _, err := open(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	m, err := shard.Misplaced(ctx, c)
	if err != nil {
		return err
	}
	header("rows on the wrong shard")
	w := tw()
	fmt.Fprintln(w, "shard\tmisplaced rows")
	var total int64
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%d\t%d\n", k, m[k])
		total += m[k]
	}
	w.Flush()
	if total == 0 {
		fmt.Println("\nclean.")
	} else {
		fmt.Printf("\n%d misplaced rows. Expected mid-reshard (the source still\n", total)
		fmt.Println("holds its copies until cleanup). Unexpected at any other time:")
		fmt.Println("it means a router wrote using a stale topology.")
	}
	return nil
}

// --------------------------------------------------------------------- demo

func cmdDemo(ctx context.Context) error {
	steps := []struct {
		title string
		run   func() error
	}{
		{"1. routing: three topologies compared", func() error { return cmdTopology(nil) }},
		{"2. per-shard state", func() error { return cmdStats(ctx) }},
		{"3. the fast path: one tenant, one shard", func() error { return cmdTenant(ctx, []string{"7"}) }},
		{"4. the slow path: scatter-gather", func() error { return cmdCount(ctx, nil) }},
		{"5. cross-shard keyset pagination", func() error { return cmdPage(ctx, []string{"-pages", "2"}) }},
		{"6. cross-shard write, the outbox way", func() error { return cmdTransfer(ctx, []string{"outbox"}) }},
		{"7. placement audit", func() error { return cmdAudit(ctx) }},
	}
	for _, s := range steps {
		fmt.Printf("\n\n\033[1;34m######## %s\033[0m\n", s.title)
		if err := s.run(); err != nil {
			return err
		}
	}
	fmt.Println("\n\nNext: labs/05-resharding/README.md -- move these shards 4 -> 8 live.")
	return nil
}
