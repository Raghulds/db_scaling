package shard

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Cluster is a set of independent Postgres nodes plus the topology that
// decides which one a tenant belongs to.
//
// The thing to internalise: there is one connection pool per shard, and pool
// sizing is now a per-shard decision multiplied by the number of app
// instances. 8 shards x 20 conns x 30 pods = 4800 backends, and Postgres
// falls over long before that. This is the first operational surprise of
// sharding, and it arrives before any of the interesting ones.
type Cluster struct {
	mu    sync.RWMutex
	pools map[int]*pgxpool.Pool
	topo  Topology
}

// DSNsFromEnv builds shard DSNs matching docker-compose.yml.
// Shard i lives on SHARD_BASE_PORT+i.
func DSNsFromEnv(n int) map[int]string {
	host := envOr("PGHOST", "127.0.0.1")
	user := envOr("PGUSER", "lab")
	pass := envOr("PGPASSWORD", "lab")
	db := envOr("PGDATABASE", "lab")
	base, err := strconv.Atoi(envOr("SHARD_BASE_PORT", "55440"))
	if err != nil {
		base = 55440
	}

	out := make(map[int]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
			user, pass, host, base+i, db)
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func Open(ctx context.Context, dsns map[int]string, topo Topology) (*Cluster, error) {
	c := &Cluster{pools: make(map[int]*pgxpool.Pool, len(dsns)), topo: topo}
	for id, dsn := range dsns {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("shard %d: %w", id, err)
		}
		// Small pools on purpose -- see the arithmetic in the type comment.
		cfg.MaxConns = 8
		cfg.MinConns = 1
		cfg.MaxConnLifetime = 30 * time.Minute
		cfg.MaxConnIdleTime = 5 * time.Minute

		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("shard %d: %w", id, err)
		}
		c.pools[id] = pool
	}
	return c, nil
}

func (c *Cluster) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pools {
		p.Close()
	}
	c.pools = map[int]*pgxpool.Pool{}
}

func (c *Cluster) Topology() Topology {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.topo
}

// SetTopology is the cutover, from the application's point of view. In a real
// system this fires from a watch on the metadata store, and the window between
// the first and the last router seeing it is where data goes missing.
func (c *Cluster) SetTopology(t Topology) {
	c.mu.Lock()
	c.topo = t
	c.mu.Unlock()
}

func (c *Cluster) Shards() []int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]int, 0, len(c.pools))
	for id := range c.pools {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func (c *Cluster) Pool(shard int) (*pgxpool.Pool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.pools[shard]
	if !ok {
		return nil, fmt.Errorf("no pool for shard %d", shard)
	}
	return p, nil
}

// ReadPool resolves a tenant to the single shard that currently owns it.
func (c *Cluster) ReadPool(tenantID uint64) (*pgxpool.Pool, int, error) {
	s := c.Topology().RouteRead(tenantID)
	p, err := c.Pool(s)
	return p, s, err
}

// WritePools resolves a tenant to every shard a write must reach: one
// normally, two during a migration window.
func (c *Cluster) WritePools(tenantID uint64) ([]*pgxpool.Pool, []int, error) {
	shards := c.Topology().RouteWrite(tenantID)
	pools := make([]*pgxpool.Pool, 0, len(shards))
	for _, s := range shards {
		p, err := c.Pool(s)
		if err != nil {
			return nil, nil, err
		}
		pools = append(pools, p)
	}
	return pools, shards, nil
}

// Ping checks every shard, so a demo fails loudly instead of hanging.
func (c *Cluster) Ping(ctx context.Context) error {
	for _, s := range c.Shards() {
		p, err := c.Pool(s)
		if err != nil {
			return err
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = p.Ping(cctx)
		cancel()
		if err != nil {
			return fmt.Errorf("shard %d unreachable: %w", s, err)
		}
	}
	return nil
}
