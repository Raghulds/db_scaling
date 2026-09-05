package shard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ShardResult carries one shard's answer, its error, and how long it took.
// Keeping the per-shard timing is not decoration: in a scatter-gather the p99
// of the whole request is the p99 of the *slowest* shard, so the interesting
// number is always max, never mean.
type ShardResult[T any] struct {
	Shard int
	Value T
	Err   error
	Took  time.Duration
}

type ScatterOpts struct {
	// PerShardTimeout bounds each shard independently. Without it, one sick
	// node makes every scatter query in the system slow -- the single most
	// common way a sharded system turns a partial outage into a total one.
	PerShardTimeout time.Duration
	// Concurrency caps in-flight shard queries. Unbounded fan-out from many
	// app pods is a self-inflicted DDoS on your own database tier.
	Concurrency int
	// AllowPartial returns what succeeded instead of failing the request.
	// Correct for dashboards, wrong for anything a user acts on -- an
	// undercounted total that looks authoritative is worse than an error.
	AllowPartial bool
}

func (o ScatterOpts) withDefaults(n int) ScatterOpts {
	if o.PerShardTimeout <= 0 {
		o.PerShardTimeout = 5 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = n
	}
	return o
}

// Scatter runs fn on every shard and gathers the results.
//
// Results come back sorted by shard id so output is stable and diffable.
func Scatter[T any](
	ctx context.Context,
	c *Cluster,
	opts ScatterOpts,
	fn func(ctx context.Context, shard int, pool *pgxpool.Pool) (T, error),
) ([]ShardResult[T], error) {
	shards := c.Shards()
	opts = opts.withDefaults(len(shards))

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		out  = make([]ShardResult[T], 0, len(shards))
		sema = make(chan struct{}, opts.Concurrency)
	)

	for _, s := range shards {
		s := s
		pool, err := c.Pool(s)
		if err != nil {
			mu.Lock()
			out = append(out, ShardResult[T]{Shard: s, Err: err})
			mu.Unlock()
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			sema <- struct{}{}
			defer func() { <-sema }()

			sctx, cancel := context.WithTimeout(ctx, opts.PerShardTimeout)
			defer cancel()

			start := time.Now()
			v, err := fn(sctx, s, pool)
			res := ShardResult[T]{Shard: s, Value: v, Err: err, Took: time.Since(start)}

			mu.Lock()
			out = append(out, res)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Shard < out[j].Shard })

	if !opts.AllowPartial {
		var errs []error
		for _, r := range out {
			if r.Err != nil {
				errs = append(errs, fmt.Errorf("shard %d: %w", r.Shard, r.Err))
			}
		}
		if len(errs) > 0 {
			return out, errors.Join(errs...)
		}
	}
	return out, nil
}

// SlowestShard is the latency that actually matters for a fan-out query.
func SlowestShard[T any](rs []ShardResult[T]) (int, time.Duration) {
	worst, at := time.Duration(0), -1
	for _, r := range rs {
		if r.Took > worst {
			worst, at = r.Took, r.Shard
		}
	}
	return at, worst
}
