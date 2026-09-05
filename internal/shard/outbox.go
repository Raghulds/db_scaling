package shard

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The outbox pattern: what to do instead of 2PC.
//
// The trick is that the local write and the record of "and this must also
// happen elsewhere" go into the *same shard* in the *same transaction*. One
// node, one commit, ordinary ACID. A relay then reads the outbox and applies
// the effect to the other shard, retrying until it succeeds.
//
// What you give up: atomicity. There is a window where shard A shows the
// transfer and shard B does not, so the system is eventually consistent and
// every reader must tolerate that. What you gain: no distributed commit, no
// held locks, no coordinator, and failure handling that is just "retry".
//
// What makes it correct is idempotency on the apply side. The relay will
// deliver at least once -- crash after apply, before marking done, and it
// delivers again. The `applied` table with a primary key on the event id is
// what turns at-least-once into effectively-once.
type Outbox struct{ c *Cluster }

func NewOutbox(c *Cluster) *Outbox { return &Outbox{c: c} }

// Transfer moves money between two tenants that may live on different shards.
// The debit and the outbox row commit together on the source shard.
func (o *Outbox) Transfer(ctx context.Context, eventID int64, from, to uint64, cents int64) error {
	srcPool, srcShard, err := o.c.ReadPool(from)
	if err != nil {
		return err
	}
	dstShard := o.c.Topology().RouteRead(to)

	return pgx.BeginFunc(ctx, srcPool, func(tx pgx.Tx) error {
		var balance int64
		err := tx.QueryRow(ctx,
			`UPDATE ledger SET balance_cents = balance_cents - $2
			 WHERE tenant_id = $1 AND balance_cents >= $2
			 RETURNING balance_cents`, from, cents).Scan(&balance)
		if err != nil {
			return fmt.Errorf("debit tenant %d on shard %d: %w", from, srcShard, err)
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO outbox (id, target_shard, kind, payload)
			 VALUES ($1, $2, 'credit', jsonb_build_object(
			   'tenant_id', $3::bigint, 'cents', $4::bigint, 'from', $5::bigint))`,
			eventID, dstShard, to, cents, from)
		return err
	})
}

// RelayOnce drains up to `batch` outbox rows from one shard. Run it in a loop,
// on a timer, forever. Notice what happens if it crashes between Apply and
// Mark: the event is delivered twice, and the `applied` primary key absorbs it.
func (o *Outbox) RelayOnce(ctx context.Context, srcShard, batch int) (delivered int, err error) {
	src, err := o.c.Pool(srcShard)
	if err != nil {
		return 0, err
	}

	type event struct {
		id      int64
		target  int
		kind    string
		payload []byte
	}

	rows, err := src.Query(ctx,
		`SELECT id, target_shard, kind, payload::text
		 FROM outbox WHERE delivered_at IS NULL
		 ORDER BY id LIMIT $1`, batch)
	if err != nil {
		return 0, err
	}
	var evs []event
	for rows.Next() {
		var e event
		var payload string
		if err := rows.Scan(&e.id, &e.target, &e.kind, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		e.payload = []byte(payload)
		evs = append(evs, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, e := range evs {
		dst, err := o.c.Pool(e.target)
		if err != nil {
			return delivered, err
		}

		// Apply idempotently on the destination: the INSERT into `applied` is
		// the dedupe, and it shares the transaction with the effect.
		err = pgx.BeginFunc(ctx, dst, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`INSERT INTO applied (event_id) VALUES ($1) ON CONFLICT DO NOTHING`, e.id)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return nil // already applied; a duplicate delivery, correctly ignored
			}
			_, err = tx.Exec(ctx,
				`INSERT INTO ledger (tenant_id, balance_cents)
				 VALUES ((($1::jsonb)->>'tenant_id')::bigint, (($1::jsonb)->>'cents')::bigint)
				 ON CONFLICT (tenant_id)
				 DO UPDATE SET balance_cents = ledger.balance_cents + (($1::jsonb)->>'cents')::bigint`,
				string(e.payload))
			return err
		})
		if err != nil {
			return delivered, fmt.Errorf("apply event %d on shard %d: %w", e.id, e.target, err)
		}

		// <<< crash here and the event is delivered twice. That is fine.
		if _, err := src.Exec(ctx,
			`UPDATE outbox SET delivered_at = now() WHERE id = $1`, e.id); err != nil {
			return delivered, err
		}
		delivered++
	}
	return delivered, nil
}

// PendingOutbox is the lag metric to alert on: if it grows without bound the
// relay is down and your system is silently diverging.
func PendingOutbox(ctx context.Context, c *Cluster) (map[int]int64, error) {
	out := map[int]int64{}
	for _, s := range c.Shards() {
		pool, err := c.Pool(s)
		if err != nil {
			return nil, err
		}
		var n int64
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM outbox WHERE delivered_at IS NULL`).Scan(&n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, nil
}
