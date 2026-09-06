package shard

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cursor is a keyset (not offset) pagination position: the last row seen,
// ordered by (created_at DESC, id DESC). id breaks ties so the sort is total
// -- without it, rows sharing a timestamp can be skipped or repeated.
//
// OFFSET is not an option across shards. There is no way to ask shard 2 for
// "rows 200-250 of the merged order" without reading the first 250 from all
// of them, so offset pagination costs O(offset) per shard per page.
type Cursor struct {
	CreatedAt time.Time
	ID        int64
}

func (c Cursor) Encode() string {
	raw := fmt.Sprintf("%d:%d", c.CreatedAt.UTC().UnixMicro(), c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func DecodeCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("bad cursor: %w", err)
	}
	parts := strings.SplitN(string(b), ":", 2)
	if len(parts) != 2 {
		return Cursor{}, fmt.Errorf("bad cursor payload")
	}
	us, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("bad cursor time: %w", err)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("bad cursor id: %w", err)
	}
	return Cursor{CreatedAt: time.UnixMicro(us).UTC(), ID: id}, nil
}

type Order struct {
	ID           int64
	TenantID     uint64
	LogicalShard int
	CreatedAt    time.Time
	TotalCents   int64
	State        string
	Shard        int // where this copy was read from; not a stored column
}

// MergeDesc merges per-shard result sets that are each already sorted by
// (created_at DESC, id DESC) and returns the global top `limit`.
//
// Pure function, no database: this is the part worth unit-testing, and
// keyset_test.go does.
//
// The cost to notice: to return `limit` rows you fetched limit*shards rows and
// threw most away. Read amplification is linear in shard count, which is why
// "just add shards" makes global feeds *worse*, and why real systems keep a
// separate denormalised feed rather than merging on read.
func MergeDesc(perShard [][]Order, limit int) []Order {
	all := make([]Order, 0, limit*len(perShard))
	for _, rows := range perShard {
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID > all[j].ID
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

// PageRecent returns one page of the cross-shard feed, newest first.
func (c *Cluster) PageRecent(ctx context.Context, after Cursor, limit int) ([]Order, Cursor, error) {
	// Two literal queries rather than one with `OR $1 IS NULL`: an OR over a
	// parameter forces a filter the planner cannot turn into an index range,
	// so the "clever" single-query version quietly seq-scans every shard.
	const firstPage = `
		SELECT id, tenant_id, logical_shard, created_at, total_cents, state
		FROM orders
		ORDER BY created_at DESC, id DESC
		LIMIT $1`

	const nextPage = `
		SELECT id, tenant_id, logical_shard, created_at, total_cents, state
		FROM orders
		WHERE (created_at, id) < ($1::timestamptz, $2::bigint)
		ORDER BY created_at DESC, id DESC
		LIMIT $3`

	// Every shard is asked for a full page: the merge needs `limit` candidates
	// from each to be sure of the global top `limit`.
	results, err := Scatter(ctx, c, ScatterOpts{PerShardTimeout: 5 * time.Second},
		func(ctx context.Context, s int, pool *pgxpool.Pool) ([]Order, error) {
			var (
				rows pgx.Rows
				err  error
			)
			if after.CreatedAt.IsZero() {
				rows, err = pool.Query(ctx, firstPage, limit)
			} else {
				rows, err = pool.Query(ctx, nextPage, after.CreatedAt, after.ID, limit)
			}
			if err != nil {
				return nil, err
			}
			defer rows.Close()

			var out []Order
			for rows.Next() {
				var o Order
				if err := rows.Scan(&o.ID, &o.TenantID, &o.LogicalShard,
					&o.CreatedAt, &o.TotalCents, &o.State); err != nil {
					return nil, err
				}
				o.Shard = s
				out = append(out, o)
			}
			return out, rows.Err()
		})
	if err != nil {
		return nil, Cursor{}, err
	}

	perShard := make([][]Order, 0, len(results))
	for _, r := range results {
		perShard = append(perShard, r.Value)
	}

	page := MergeDesc(perShard, limit)
	next := after
	if len(page) > 0 {
		last := page[len(page)-1]
		next = Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, next, nil
}
