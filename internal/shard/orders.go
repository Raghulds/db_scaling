package shard

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Insert writes one order to every shard the topology says it belongs on.
//
// During a migration window that is two shards, and the write is NOT atomic
// across them. If the second insert fails, the source has the row and the
// target does not -- which is exactly what the verify step of a reshard is
// for. Dual-write plus verify plus repair, not dual-write and hope.
func Insert(ctx context.Context, c *Cluster, id int64, tenantID uint64, cents int64, state string) error {
	pools, shards, err := c.WritePools(tenantID)
	if err != nil {
		return err
	}
	logical := logicalOf(c.Topology(), tenantID)

	const q = `
		INSERT INTO orders (id, tenant_id, logical_shard, created_at, total_cents, state)
		VALUES ($1, $2, $3, now(), $4, $5)
		ON CONFLICT (id) DO NOTHING`

	for i, pool := range pools {
		if _, err := pool.Exec(ctx, q, id, tenantID, logical, cents, state); err != nil {
			return fmt.Errorf("insert on shard %d: %w", shards[i], err)
		}
	}
	return nil
}

// logicalOf records which logical shard a row belongs to, so a reshard can
// select rows to move by logical id instead of recomputing the hash in SQL.
// Storing it is a small denormalisation that makes every migration query
// trivial -- worth it.
func logicalOf(t Topology, tenantID uint64) int {
	if d, ok := t.(*Directory); ok {
		return d.Logical(tenantID)
	}
	return int(Key(tenantID) % 1024)
}

// Seed loads orders for `tenants` tenants using CopyFrom, which is an order of
// magnitude faster than INSERT and the only sane way to backfill a shard.
func Seed(ctx context.Context, c *Cluster, tenants int, perTenant int, seed int64) (int64, error) {
	rng := rand.New(rand.NewSource(seed))
	topo := c.Topology()
	states := []string{"new", "paid", "shipped", "refunded"}

	// Bucket rows per shard first, then one CopyFrom per shard.
	type row struct {
		id      int64
		tenant  uint64
		logical int
		created time.Time
		cents   int64
		state   string
	}
	buckets := map[int][]row{}

	gens := map[int]*IDGen{}
	for _, s := range c.Shards() {
		g, err := NewIDGen(s)
		if err != nil {
			return 0, err
		}
		gens[s] = g
	}

	now := time.Now()
	for t := 1; t <= tenants; t++ {
		tenant := uint64(t)
		// Skew, so the labs have whales to find: tenant 1 and 2 get 20x.
		n := perTenant
		if t <= 2 {
			n = perTenant * 20
		}
		target := topo.RouteRead(tenant)
		for i := 0; i < n; i++ {
			buckets[target] = append(buckets[target], row{
				id:      gens[target].Next(),
				tenant:  tenant,
				logical: logicalOf(topo, tenant),
				created: now.Add(-time.Duration(rng.Int63n(int64(90 * 24 * time.Hour)))),
				cents:   rng.Int63n(20000),
				state:   states[rng.Intn(len(states))],
			})
		}
	}

	var total int64
	for shardID, rows := range buckets {
		pool, err := c.Pool(shardID)
		if err != nil {
			return total, err
		}
		src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			r := rows[i]
			return []any{r.id, r.tenant, r.logical, r.created, r.cents, r.state}, nil
		})
		n, err := pool.CopyFrom(ctx, pgx.Identifier{"orders"},
			[]string{"id", "tenant_id", "logical_shard", "created_at", "total_cents", "state"}, src)
		if err != nil {
			return total, fmt.Errorf("copy into shard %d: %w", shardID, err)
		}
		total += n
	}
	return total, nil
}

// TenantSummary is a single-shard read: the fast path, and the reason you
// sharded by tenant in the first place.
type TenantSummary struct {
	TenantID uint64
	Shard    int
	Orders   int64
	Cents    int64
}

func SummarizeTenant(ctx context.Context, c *Cluster, tenantID uint64) (TenantSummary, error) {
	pool, shard, err := c.ReadPool(tenantID)
	if err != nil {
		return TenantSummary{}, err
	}
	s := TenantSummary{TenantID: tenantID, Shard: shard}
	err = pool.QueryRow(ctx,
		`SELECT count(*), coalesce(sum(total_cents), 0) FROM orders WHERE tenant_id = $1`,
		tenantID).Scan(&s.Orders, &s.Cents)
	return s, err
}

// StateCounts is the scatter-gather: N queries, one merged answer.
//
// Note that this number is not a snapshot. Each shard answers as of its own
// commit point, so the total is not a value the system was ever actually in.
// For a dashboard that is fine. For "did we oversell?" it is not.
func StateCounts(ctx context.Context, c *Cluster, allowPartial bool) (map[string]int64, []ShardResult[map[string]int64], error) {
	results, err := Scatter(ctx, c,
		ScatterOpts{PerShardTimeout: 5 * time.Second, AllowPartial: allowPartial},
		func(ctx context.Context, s int, pool *pgxpool.Pool) (map[string]int64, error) {
			rows, err := pool.Query(ctx, `SELECT state, count(*) FROM orders GROUP BY state`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := map[string]int64{}
			for rows.Next() {
				var st string
				var n int64
				if err := rows.Scan(&st, &n); err != nil {
					return nil, err
				}
				out[st] = n
			}
			return out, rows.Err()
		})

	merged := map[string]int64{}
	for _, r := range results {
		for st, n := range r.Value {
			merged[st] += n
		}
	}
	return merged, results, err
}

// ShardStats is the per-shard health line you want on a dashboard.
type ShardStats struct {
	Shard   int
	Rows    int64
	Tenants int64
	Bytes   int64
}

func Stats(ctx context.Context, c *Cluster) ([]ShardStats, error) {
	results, err := Scatter(ctx, c, ScatterOpts{},
		func(ctx context.Context, s int, pool *pgxpool.Pool) (ShardStats, error) {
			st := ShardStats{Shard: s}
			err := pool.QueryRow(ctx,
				`SELECT count(*), count(DISTINCT tenant_id),
				        pg_total_relation_size('orders')
				 FROM orders`).Scan(&st.Rows, &st.Tenants, &st.Bytes)
			return st, err
		})
	if err != nil {
		return nil, err
	}
	out := make([]ShardStats, 0, len(results))
	for _, r := range results {
		out = append(out, r.Value)
	}
	return out, nil
}

// Misplaced counts rows sitting on a shard the current topology would not
// route them to. After a reshard this must be zero on every shard except the
// sources you have not cleaned up yet.
//
// Run this as a continuous audit, not just during migrations. A router with a
// stale topology cache writes misplaced rows silently, and nothing else in the
// system will ever tell you.
func Misplaced(ctx context.Context, c *Cluster) (map[int]int64, error) {
	topo := c.Topology()
	out := map[int]int64{}

	for _, s := range c.Shards() {
		pool, err := c.Pool(s)
		if err != nil {
			return nil, err
		}
		rows, err := pool.Query(ctx, `SELECT DISTINCT tenant_id FROM orders`)
		if err != nil {
			return nil, err
		}
		var tenants []uint64
		for rows.Next() {
			var t uint64
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return nil, err
			}
			tenants = append(tenants, t)
		}
		rows.Close()

		var wrong []uint64
		for _, t := range tenants {
			if topo.RouteRead(t) != s {
				wrong = append(wrong, t)
			}
		}
		if len(wrong) == 0 {
			out[s] = 0
			continue
		}
		var n int64
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM orders WHERE tenant_id = ANY($1)`, wrong).Scan(&n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, nil
}
