package shard

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

// ---------------------------------------------------------------- helpers

var feedBase = time.Date(2026, 2, 3, 4, 5, 6, 123456000, time.UTC)

func order(id int64, shard int, ts time.Time) Order {
	return Order{
		ID:         id,
		TenantID:   uint64(id % 97),
		CreatedAt:  ts,
		TotalCents: id * 10,
		State:      "paid",
		Shard:      shard,
	}
}

func ids(rows []Order) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// sortDesc puts rows in the order a shard's `ORDER BY created_at DESC, id DESC`
// would return them. MergeDesc's contract requires its inputs to already be in
// this order.
func sortDesc(rows []Order) []Order {
	out := append([]Order(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// ---------------------------------------------------------------- MergeDesc

// TestMergeDescOrdersAcrossShards is the base case: four shards, each already
// sorted, merged into one globally ordered page.
//
// The read amplification is the thing to notice in the numbers below. To
// return 4 rows the merge consumed rows from all four shards and discarded
// most of them. Every shard you add makes a global feed more expensive, not
// less, which is why "scale the feed by sharding" is backwards and why real
// systems maintain a separate denormalised feed.
func TestMergeDescOrdersAcrossShards(t *testing.T) {
	sec := func(n int) time.Time { return feedBase.Add(time.Duration(n) * time.Second) }

	perShard := [][]Order{
		{order(100, 0, sec(9)), order(101, 0, sec(5)), order(102, 0, sec(1))},
		{order(200, 1, sec(8)), order(201, 1, sec(4))},
		{order(300, 2, sec(7)), order(301, 2, sec(6)), order(302, 2, sec(2))},
		{order(400, 3, sec(3))},
	}

	got := MergeDesc(perShard, 9)
	want := []int64{100, 200, 300, 301, 101, 201, 400, 302, 102}
	if !reflect.DeepEqual(ids(got), want) {
		t.Errorf("MergeDesc order = %v, want %v", ids(got), want)
	}

	// Verify the invariant directly rather than only against a golden list:
	// the result must be non-increasing in (created_at, id).
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if cur.CreatedAt.After(prev.CreatedAt) ||
			(cur.CreatedAt.Equal(prev.CreatedAt) && cur.ID >= prev.ID) {
			t.Fatalf("result is not sorted descending at index %d: %v then %v",
				i, prev.CreatedAt.Format(time.RFC3339Nano), cur.CreatedAt.Format(time.RFC3339Nano))
		}
	}
}

// TestMergeDescRespectsLimit checks that the merge truncates to the requested
// page size after ordering, not before.
func TestMergeDescRespectsLimit(t *testing.T) {
	sec := func(n int) time.Time { return feedBase.Add(time.Duration(n) * time.Second) }
	perShard := [][]Order{
		{order(10, 0, sec(6)), order(11, 0, sec(2))},
		{order(20, 1, sec(5)), order(21, 1, sec(1))},
		{order(30, 2, sec(4)), order(31, 2, sec(3))},
	}

	for _, tc := range []struct {
		limit int
		want  []int64
	}{
		{1, []int64{10}},
		{3, []int64{10, 20, 30}},
		{6, []int64{10, 20, 30, 31, 11, 21}},
		{100, []int64{10, 20, 30, 31, 11, 21}}, // limit beyond the input is not an error
	} {
		got := ids(MergeDesc(perShard, tc.limit))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("MergeDesc(limit=%d) = %v, want %v", tc.limit, got, tc.want)
		}
	}
}

// TestMergeDescBreaksTimestampTiesByID is the case that actually bites.
//
// Timestamps collide constantly in practice: a batch import, a bulk state
// transition, or simply two orders inside the same microsecond. If the sort is
// only on created_at then the order of tied rows is whatever the merge happened
// to produce, and it can differ between two calls with the same data. Keyset
// pagination then either skips a tied row or returns it twice, because the
// cursor says "everything before (t, id)" and the previous page did not agree
// on which tied rows came before that point.
//
// The fix is that the sort key must be TOTAL: (created_at DESC, id DESC), with
// id unique. This test pins that, including a tie that spans shards.
func TestMergeDescBreaksTimestampTiesByID(t *testing.T) {
	tied := feedBase
	later := feedBase.Add(time.Second)

	perShard := [][]Order{
		{order(500, 0, later), order(140, 0, tied), order(110, 0, tied)},
		{order(150, 1, tied), order(120, 1, tied)},
		{order(130, 2, tied)},
	}

	got := ids(MergeDesc(perShard, 10))
	want := []int64{500, 150, 140, 130, 120, 110}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeDesc with a 5-way timestamp tie = %v, want %v\n"+
			"Tied timestamps must be ordered by id DESC. Without a total order the page\n"+
			"boundary is ambiguous and keyset pagination silently drops or repeats rows.",
			got, want)
	}

	// Determinism: identical input must produce identical output every time,
	// not merely a valid ordering.
	for i := 0; i < 20; i++ {
		if again := ids(MergeDesc(perShard, 10)); !reflect.DeepEqual(again, want) {
			t.Fatalf("MergeDesc is not deterministic: run %d gave %v, want %v", i, again, want)
		}
	}
}

// TestMergeDescDegenerateInputs covers the shapes that show up the moment a
// shard is empty, drained, or newly added.
func TestMergeDescDegenerateInputs(t *testing.T) {
	sec := func(n int) time.Time { return feedBase.Add(time.Duration(n) * time.Second) }

	t.Run("no shards at all", func(t *testing.T) {
		if got := MergeDesc(nil, 10); len(got) != 0 {
			t.Errorf("MergeDesc(nil) = %v, want empty", ids(got))
		}
	})

	t.Run("every shard empty", func(t *testing.T) {
		got := MergeDesc([][]Order{nil, {}, nil, {}}, 10)
		if len(got) != 0 {
			t.Errorf("MergeDesc of four empty shards = %v, want empty.\n"+
				"An exhausted feed must return no rows, not a nil-deref: this is the state\n"+
				"every paging loop ends in.", ids(got))
		}
	})

	t.Run("single shard passes through", func(t *testing.T) {
		in := [][]Order{{order(3, 0, sec(3)), order(2, 0, sec(2)), order(1, 0, sec(1))}}
		if got := ids(MergeDesc(in, 10)); !reflect.DeepEqual(got, []int64{3, 2, 1}) {
			t.Errorf("single-shard merge = %v, want [3 2 1]", got)
		}
	})

	t.Run("some shards empty", func(t *testing.T) {
		// A newly added shard holds nothing yet; a drained one holds nothing
		// any more. Neither may stall or truncate the merge.
		in := [][]Order{
			nil,
			{order(20, 1, sec(4)), order(21, 1, sec(1))},
			{},
			{order(40, 3, sec(3)), order(41, 3, sec(2))},
		}
		if got := ids(MergeDesc(in, 10)); !reflect.DeepEqual(got, []int64{20, 40, 41, 21}) {
			t.Errorf("merge with empty shards = %v, want [20 40 41 21]", got)
		}
	})

	t.Run("limit larger than total input", func(t *testing.T) {
		in := [][]Order{{order(1, 0, sec(1))}, {order(2, 1, sec(2))}}
		if got := ids(MergeDesc(in, 1000)); !reflect.DeepEqual(got, []int64{2, 1}) {
			t.Errorf("merge with oversized limit = %v, want [2 1]", got)
		}
	})
}

// ---------------------------------------------------------------- Cursor

// TestCursorRoundTrip pins the encoding, because a cursor is a value you hand
// to a client and get back an unknown amount of time later. It is, in effect,
// a wire format with no version negotiation: mobile clients will still be
// sending you last week's cursors after you deploy.
//
// The zero-cursor case is the one worth staring at. PageRecent branches on
// after.CreatedAt.IsZero() to decide between the first-page query and the
// next-page query. So an empty cursor and a cursor that round-tripped through
// base64 must BOTH still report IsZero, or the first page of the feed silently
// runs the wrong query.
func TestCursorRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   Cursor
	}{
		{"zero cursor means first page", Cursor{}},
		{"microsecond precision", Cursor{CreatedAt: time.Date(2026, 3, 14, 15, 9, 26, 535897000, time.UTC), ID: 987654321}},
		{"epoch second boundary", Cursor{CreatedAt: time.Unix(0, 0).UTC(), ID: 0}},
		{"negative id", Cursor{CreatedAt: feedBase, ID: -1}},
		{"max int64 id", Cursor{CreatedAt: feedBase, ID: 9223372036854775807}},
		{"pre-epoch timestamp", Cursor{CreatedAt: time.Date(1969, 7, 20, 20, 17, 40, 0, time.UTC), ID: 11}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeCursor(tc.in.Encode())
			if err != nil {
				t.Fatalf("DecodeCursor(%q): %v", tc.in.Encode(), err)
			}
			if !got.CreatedAt.Equal(tc.in.CreatedAt) {
				t.Errorf("CreatedAt round-trip: got %s want %s",
					got.CreatedAt.Format(time.RFC3339Nano), tc.in.CreatedAt.Format(time.RFC3339Nano))
			}
			if got.ID != tc.in.ID {
				t.Errorf("ID round-trip: got %d want %d", got.ID, tc.in.ID)
			}
			if got.CreatedAt.IsZero() != tc.in.CreatedAt.IsZero() {
				t.Errorf("IsZero changed across the round trip: got %v want %v.\n"+
					"PageRecent uses IsZero to choose between the first-page and next-page\n"+
					"query. If a round-tripped empty cursor stops reporting zero, the first\n"+
					"page runs the WHERE-clause query against a garbage bound.",
					got.CreatedAt.IsZero(), tc.in.CreatedAt.IsZero())
			}
		})
	}
}

// TestDecodeCursorEmptyIsFirstPage pins that an absent cursor is not an error.
// Clients start paging with no cursor at all; treating that as malformed would
// make the first request of every feed fail.
func TestDecodeCursorEmptyIsFirstPage(t *testing.T) {
	got, err := DecodeCursor("")
	if err != nil {
		t.Fatalf("DecodeCursor(\"\") returned %v, want no error: an absent cursor means page one", err)
	}
	if !got.CreatedAt.IsZero() || got.ID != 0 {
		t.Errorf("DecodeCursor(\"\") = %+v, want the zero Cursor", got)
	}
}

// TestDecodeCursorRejectsGarbage checks that a malformed cursor is an error
// rather than a silently wrong page.
//
// Cursors arrive from the network. They get truncated by URL length limits,
// mangled by a client that URL-encodes them twice, or fabricated by someone
// probing your API. The failure mode to avoid is a cursor that parses into a
// plausible-but-wrong position: the client then pages through a slice of the
// feed with no indication anything went wrong.
func TestDecodeCursorRejectsGarbage(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"not base64", "!!!not-base64!!!"},
		{"base64 with padding (this encoder is Raw)", "MTIzNDo1Ng=="},
		{"decodes but has no separator", "bm9jb2xvbg"},                  // "nocolon"
		{"timestamp is not a number", "YWJjOjEyMw"},                     // "abc:123"
		{"id is not a number", "MTIzOmFiYw"},                            // "123:abc"
		{"timestamp overflows int64", "OTk5OTk5OTk5OTk5OTk5OTk5OTk6MQ"}, // "99999999999999999999:1"
		{"empty fields", "Og"},                                          // ":"
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeCursor(tc.in)
			if err == nil {
				t.Errorf("DecodeCursor(%q) = %+v, want an error.\n"+
					"A cursor that parses into a plausible-but-wrong position pages the client\n"+
					"through an arbitrary slice of the feed with no signal that anything broke.",
					tc.in, got)
			}
		})
	}
}

// ---------------------------------------------------------------- paging

// shardPage is what each shard's SQL does in PageRecent: everything strictly
// before the cursor in (created_at, id) order, newest first, capped at limit.
// Postgres row comparison `(created_at, id) < ($1, $2)` is lexicographic, and
// this must match it exactly or the boundary between pages moves.
func shardPage(rows []Order, after Cursor, limit int) []Order {
	var out []Order
	for _, o := range rows { // rows are already sorted DESC
		if !after.CreatedAt.IsZero() {
			before := o.CreatedAt.Before(after.CreatedAt) ||
				(o.CreatedAt.Equal(after.CreatedAt) && o.ID < after.ID)
			if !before {
				continue
			}
		}
		out = append(out, o)
		if len(out) == limit {
			break
		}
	}
	return out
}

// TestKeysetPagingVisitsEveryRowExactlyOnce walks a fabricated four-shard feed
// end to end and asserts the property that makes keyset pagination worth the
// extra code: concatenating every page yields every row exactly once, in
// global order, with no gaps and no repeats.
//
// This is precisely the property OFFSET violates. `ORDER BY created_at DESC
// LIMIT 20 OFFSET 40` computes its window from a row count, so any insert
// above the window shifts every subsequent row down by one: the reader sees
// the row at the old boundary a second time. Any delete above the window
// shifts rows up and the reader never sees one. On a feed of new orders --
// where inserts land at the top by definition, which is the whole point of
// ordering by created_at DESC -- this is not a rare race. It happens on
// essentially every page turn under load, and it is invisible: no error, no
// warning, just a duplicated or missing order in the customer's list.
//
// A keyset cursor names a POSITION rather than a count, so rows appearing or
// disappearing above the cursor cannot move the boundary. The cost is that you
// give up random access to page N; that is the trade, and it is a good one.
//
// The data below deliberately includes timestamps shared within a single shard
// and across several shards, since ties are where a not-quite-total sort key
// shows up as exactly the drop/repeat bug described above.
func TestKeysetPagingVisitsEveryRowExactlyOnce(t *testing.T) {
	const shards = 4

	var all []Order
	nextID := int64(9000)
	for i := 0; i < 40; i++ {
		// Groups of three consecutive rows share a timestamp, and consecutive
		// rows sit on different shards, so most ties span shards.
		ts := feedBase.Add(time.Duration(i/3) * time.Second)
		all = append(all, order(nextID, i%shards, ts))
		nextID++
	}
	// A deliberate pile-up on one instant, including two rows on the SAME
	// shard. Bulk state transitions and importers produce exactly this.
	tie := feedBase.Add(7 * time.Second)
	for _, s := range []int{1, 1, 2, 0} {
		all = append(all, order(nextID, s, tie))
		nextID++
	}

	perShard := make([][]Order, shards)
	for _, o := range all {
		perShard[o.Shard] = append(perShard[o.Shard], o)
	}
	for s := range perShard {
		perShard[s] = sortDesc(perShard[s])
	}
	want := ids(sortDesc(all))

	for _, limit := range []int{1, 3, 5, 7, 13, 100} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			var got []int64
			after := Cursor{}
			pages := 0

			for {
				pages++
				if pages > len(all)+5 {
					t.Fatalf("paging did not terminate after %d pages: the cursor is not advancing,\n"+
						"which means a page is re-serving rows it already returned", pages)
				}

				page := make([][]Order, shards)
				for s := range perShard {
					page[s] = shardPage(perShard[s], after, limit)
				}
				merged := MergeDesc(page, limit)
				if len(merged) == 0 {
					break
				}
				got = append(got, ids(merged)...)

				last := merged[len(merged)-1]
				next := Cursor{CreatedAt: last.CreatedAt, ID: last.ID}

				// Round-trip the cursor through the wire format on every page,
				// the way a real client does. A microsecond lost in the
				// encoding here re-serves or skips a row at the boundary.
				encoded := next.Encode()
				decoded, err := DecodeCursor(encoded)
				if err != nil {
					t.Fatalf("cursor %q failed to decode mid-feed: %v", encoded, err)
				}
				if !decoded.CreatedAt.Equal(next.CreatedAt) || decoded.ID != next.ID {
					t.Fatalf("cursor lost precision on the wire: %+v -> %q -> %+v", next, encoded, decoded)
				}
				after = decoded
			}

			if !reflect.DeepEqual(got, want) {
				// Report the two failure shapes separately: they have different
				// causes and different customer-visible symptoms.
				seen := map[int64]int{}
				for _, id := range got {
					seen[id]++
				}
				var dupes, missing []int64
				for _, id := range want {
					switch seen[id] {
					case 0:
						missing = append(missing, id)
					case 1:
					default:
						dupes = append(dupes, id)
					}
				}
				t.Errorf("limit=%d: paged %d rows over %d pages, want %d rows.\n"+
					"repeated: %v\nmissing: %v\n"+
					"got  %v\nwant %v\n"+
					"Rows repeated or dropped across page boundaries is the OFFSET bug that\n"+
					"keyset pagination exists to prevent. The usual cause is a sort key that\n"+
					"is not total, so tied rows land on different sides of the cursor.",
					limit, len(got), pages, len(want), dupes, missing, got, want)
			}
		})
	}
}
