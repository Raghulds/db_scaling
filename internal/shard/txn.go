package shard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------- 2PC

// TwoPC runs a write that spans shards using Postgres' PREPARE TRANSACTION.
//
// It works, and you should still almost never use it. What you are buying:
// atomicity across nodes. What you are paying:
//
//   - max_prepared_transactions must be > 0 (it defaults to 0, so this fails
//     out of the box -- the compose file sets it to 10)
//   - a prepared transaction survives crashes and holds its locks until
//     someone resolves it. Forever, if nobody does.
//   - it pins the xmin horizon, so VACUUM cannot clean up anywhere in that
//     database. A forgotten prepared xact is a slow-motion outage.
//   - the coordinator is a new single point of failure, and it needs its own
//     durable log to recover -- which this lab deliberately does not have.
//
// Run `shardctl orphans` after killing the process mid-commit to see the mess.
// Then read outbox.go, which is what production actually does.
type TwoPC struct{ c *Cluster }

func NewTwoPC(c *Cluster) *TwoPC { return &TwoPC{c: c} }

// Do executes one function per shard, then commits them together.
// prepLeg is one shard's prepared-but-uncommitted branch of a 2PC.
type prepLeg struct {
	shard int
	gid   string
}

func (t *TwoPC) Do(ctx context.Context, gid string, parts map[int]func(context.Context, pgx.Tx) error) error {
	var legs []prepLeg

	// Phase 1: do the work and PREPARE on every shard.
	for shardID, fn := range parts {
		pool, err := t.c.Pool(shardID)
		if err != nil {
			t.rollbackAll(ctx, legs)
			return err
		}
		// PREPARE TRANSACTION detaches the transaction from its session, so we
		// drive it on a connection we hold explicitly and hand back straight
		// after -- the session is idle again once PREPARE returns.
		acquired, err := pool.Acquire(ctx)
		if err != nil {
			t.rollbackAll(ctx, legs)
			return fmt.Errorf("shard %d acquire: %w", shardID, err)
		}

		tx, err := acquired.Conn().Begin(ctx)
		if err != nil {
			acquired.Release()
			t.rollbackAll(ctx, legs)
			return fmt.Errorf("shard %d begin: %w", shardID, err)
		}
		if err := fn(ctx, tx); err != nil {
			_ = tx.Rollback(ctx)
			acquired.Release()
			t.rollbackAll(ctx, legs)
			return fmt.Errorf("shard %d work: %w", shardID, err)
		}

		legGID := fmt.Sprintf("%s-%d", gid, shardID)
		// pgx has no PREPARE TRANSACTION helper, deliberately: the statement
		// takes the transaction out from under the driver. Hence raw SQL, and
		// hence the gid being interpolated -- it cannot be a bind parameter.
		if _, err := tx.Exec(ctx, fmt.Sprintf("PREPARE TRANSACTION '%s'", legGID)); err != nil {
			_ = tx.Rollback(ctx)
			acquired.Release()
			t.rollbackAll(ctx, legs)
			return fmt.Errorf("shard %d prepare: %w", shardID, err)
		}
		acquired.Release()

		legs = append(legs, prepLeg{shard: shardID, gid: legGID})
	}

	// >>> THE WINDOW <<<
	// Every shard has promised to be able to commit. Nothing has committed.
	// If the process dies here, all of those prepared transactions sit there
	// holding locks and blocking VACUUM until a human or a recovery daemon
	// resolves them from pg_prepared_xacts. This is the entire reason 2PC is
	// unpopular, and it is worth pausing on: no amount of care in the code
	// below removes this window.

	// Phase 2: commit. A failure here is not recoverable by rolling back --
	// some legs may already be committed, so the only correct move is retry
	// until it succeeds.
	var errs []error
	for _, l := range legs {
		pool, err := t.c.Pool(l.shard)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf("COMMIT PREPARED '%s'", l.gid)); err != nil {
			errs = append(errs, fmt.Errorf("shard %d commit prepared %s: %w", l.shard, l.gid, err))
		}
	}
	return errors.Join(errs...)
}

func (t *TwoPC) rollbackAll(ctx context.Context, legs []prepLeg) {
	for _, l := range legs {
		if pool, err := t.c.Pool(l.shard); err == nil {
			_, _ = pool.Exec(ctx, fmt.Sprintf("ROLLBACK PREPARED '%s'", l.gid))
		}
	}
}

// Orphan is a prepared transaction nobody resolved.
type Orphan struct {
	Shard    int
	GID      string
	Prepared time.Time
	Age      time.Duration
}

// Orphans lists prepared transactions across the cluster. In production this
// is an alert, not a command: page on any prepared xact older than a minute.
func Orphans(ctx context.Context, c *Cluster) ([]Orphan, error) {
	var out []Orphan
	for _, s := range c.Shards() {
		pool, err := c.Pool(s)
		if err != nil {
			return nil, err
		}
		rows, err := pool.Query(ctx,
			`SELECT gid, prepared, now() - prepared FROM pg_prepared_xacts ORDER BY prepared`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			o := Orphan{Shard: s}
			if err := rows.Scan(&o.GID, &o.Prepared, &o.Age); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
