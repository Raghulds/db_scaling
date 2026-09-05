package shard

import (
	"math"
	"path/filepath"
	"reflect"
	"testing"
)

// ---------------------------------------------------------------- helpers

// liveShards reports how many distinct shards receive at least one of the
// sampled tenants. A topology that addresses 8 nodes but only ever routes to
// 2 of them is not a balanced 8-shard cluster; it is a 2-shard cluster with a
// six-node hardware bill.
func liveShards(topo Topology, sample int) (live int, dist map[int]int) {
	dist = Distribution(topo, sample)
	for _, n := range dist {
		if n > 0 {
			live++
		}
	}
	return live, dist
}

// tenantsInLogical finds `want` tenant ids that hash to a given logical shard.
// Tests need this because the whole point of the Directory is that you operate
// on logical shards, not on individual tenants.
func tenantsInLogical(t *testing.T, d *Directory, logical, want int) []uint64 {
	t.Helper()
	var out []uint64
	for id := uint64(1); len(out) < want; id++ {
		if id > 1_000_000 {
			t.Fatalf("could not find %d tenants in logical shard %d", want, logical)
		}
		if d.Logical(id) == logical {
			out = append(out, id)
		}
	}
	return out
}

// ---------------------------------------------------------------- ModN

// TestModNResizeRehashesAlmostEverything measures the cost of resizing a mod-N
// cluster, which is the reason mod-N is a trap rather than a design.
//
// 4 -> 8 moves half the tenants. That is the merciful special case: when n
// doubles, hash%2n is either hash%n or hash%n + n, so a tenant either stays
// put or moves to exactly one predictable new node. Nothing moves between two
// pre-existing shards.
//
// 4 -> 5 moves four fifths of them. There is no incremental version of this.
// You cannot move one tenant as a rehearsal, you cannot move your smallest
// customer first to prove the runbook, and you cannot stop halfway. Every
// tenant is in flight at once, so the dual-write window covers the entire
// cluster and the rollback plan is "hope".
//
// This is why mod-N shops only ever double, and why they schedule it for a
// weekend. The Directory below exists to make the same operation a Tuesday.
func TestModNResizeRehashesAlmostEverything(t *testing.T) {
	const sample = 200000

	doubling := Churn(ModN{N: 4}, ModN{N: 8}, sample)
	t.Logf("mod-4 -> mod-8 churn: %.4f", doubling)
	if doubling < 0.45 || doubling > 0.55 {
		t.Errorf("mod-4 -> mod-8 moved %.4f of tenants, want 0.45..0.55.\n"+
			"Doubling should move exactly half. A different number means the hash\n"+
			"or the modulus arithmetic changed, and the reshard runbook is wrong.",
			doubling)
	}

	awkward := Churn(ModN{N: 4}, ModN{N: 5}, sample)
	t.Logf("mod-4 -> mod-5 churn: %.4f", awkward)
	if awkward <= 0.70 {
		t.Errorf("mod-4 -> mod-5 moved only %.4f of tenants, want > 0.70.\n"+
			"If adding one node to a mod-N cluster looks cheap, the measurement is wrong;\n"+
			"the real number is (n-1)/n and it is why nobody adds one node.",
			awkward)
	}

	if awkward <= doubling {
		t.Errorf("mod-4 -> mod-5 (%.4f) should churn more than mod-4 -> mod-8 (%.4f):\n"+
			"adding one node must be strictly worse than doubling, which is the\n"+
			"counter-intuitive fact the whole lab turns on", awkward, doubling)
	}
}

// ---------------------------------------------------------------- Ring

// TestRingOnlyMovesKeysOntoNewShards defends the one property that makes
// consistent hashing worth its complexity.
//
// When you add shards 4..7 to a ring holding 0..3, some keys move. The
// guarantee is not about how many -- it is about where they go. Every key that
// moves must move onto a newly added shard. No key may move from shard 1 to
// shard 2, because both of those nodes already hold data, and a key moving
// between them means two live nodes both believe they own it during the
// window.
//
// That is the difference between a reshard where the migration is "copy the
// keys that moved onto the new nodes, then cut over" and a mod-N reshard where
// the migration is "every node ships data to every other node simultaneously".
//
// Note that this property is structural: it follows from only ever adding
// points to the ring, and it holds whether the ring is well balanced or not.
// Safety and balance are independent properties, and a ring can pass this
// test while routing every key to two of its eight nodes -- which is exactly
// what this one did before Ring.Add was fixed. Test both.
// TestRingVNodesBuyBalance is the other half.
func TestRingOnlyMovesKeysOntoNewShards(t *testing.T) {
	const sample = 200000
	const vnodes = 200

	before := NewRing([]int{0, 1, 2, 3}, vnodes)
	after := NewRing([]int{0, 1, 2, 3}, vnodes)
	for s := 4; s <= 7; s++ {
		after.Add(s)
	}

	moved, illegal := 0, 0
	var firstIllegal [3]int // tenant, from, to
	for id := uint64(1); id <= sample; id++ {
		from, to := before.RouteRead(id), after.RouteRead(id)
		if from == to {
			continue
		}
		moved++
		if to < 4 {
			if illegal == 0 {
				firstIllegal = [3]int{int(id), from, to}
			}
			illegal++
		}
	}

	frac := float64(moved) / float64(sample)
	t.Logf("ring %d -> %d vnodes=%d: churn %.4f (%d of %d keys moved)",
		before.PhysicalCount(), after.PhysicalCount(), vnodes, frac, moved, sample)

	if illegal != 0 {
		t.Errorf("%d keys moved between two pre-existing shards, first: tenant %d moved %d -> %d.\n"+
			"This breaks the only guarantee consistent hashing offers. If a key can move\n"+
			"from one live node to another live node, adding capacity means every node\n"+
			"must ship data to every other node, and the migration is a full mod-N reshard\n"+
			"wearing a ring costume.",
			illegal, firstIllegal[0], firstIllegal[1], firstIllegal[2])
	}

	// A ring that moves nothing has not actually taken on the new capacity;
	// a ring that moves nearly everything has lost its advantage over mod-N.
	if frac <= 0.05 || frac >= 0.60 {
		t.Errorf("ring 4 -> 8 moved %.4f of keys, expected a meaningful but bounded fraction (0.05..0.60).\n"+
			"Near zero means the new shards are receiving no traffic and the reshard did nothing.\n"+
			"Near one means the ring is behaving like mod-N.", frac)
	}
}

// TestRingVNodesBuyBalance asserts the reason virtual nodes exist.
//
// A ring with one point per shard cuts the circle into n arcs at n random
// positions, and random arc lengths are wildly uneven: with 8 shards you
// should expect the largest to be tens of times the smallest. That is not a
// cluster, it is a lottery. Giving each shard v points cuts the circle into
// n*v arcs and gives each shard the SUM of v of them, and a sum of v
// independent lengths concentrates around its mean as v grows. 100-200 is the
// usual production setting; it is the point where the extra memory stops
// buying noticeable balance.
//
// This test replaces a characterization test that pinned a real defect: Ring
// placed vnode i of shard s at Key(s<<32|i) with no further mixing, and
// FNV-1a over such near-consecutive inputs is very nearly an arithmetic
// progression (see TestKeyIsEvenModuloNButClustersInRingOrder). Every shard's
// 200 vnodes piled into one narrow band, the 8 bands covered about a third of
// the keyspace, and two shards of eight served 100% of traffic at every vnode
// count. Ring.Add and Ring.RouteRead now run positions through ringPos, a
// fmix64 finalizer, so the circle is actually used.
//
// The two assertions below are the ones that would have caught that bug, and
// they are worth stealing for any ring you own: every node must receive
// traffic, and raising the vnode count must measurably improve balance. Neither
// is implied by the safety property in the test above.
func TestRingVNodesBuyBalance(t *testing.T) {
	const sample = 100000
	const shards = 8

	all := []int{0, 1, 2, 3, 4, 5, 6, 7}

	imbalance := func(dist map[int]int) float64 {
		mn, mx := 1<<62, 0
		for s := 0; s < shards; s++ {
			if dist[s] < mn {
				mn = dist[s]
			}
			if dist[s] > mx {
				mx = dist[s]
			}
		}
		if mn == 0 {
			// An idle node is infinitely imbalanced; return something large
			// rather than dividing by zero, and let the caller report it.
			return math.Inf(1)
		}
		return float64(mx) / float64(mn)
	}

	coarseLive, coarse := liveShards(NewRing(all, 1), sample)
	fineLive, fine := liveShards(NewRing(all, 200), sample)
	coarseImb, fineImb := imbalance(coarse), imbalance(fine)

	t.Logf("ring shards=%d vnodes=1   live=%d max/min=%.2f dist=%v", shards, coarseLive, coarseImb, coarse)
	t.Logf("ring shards=%d vnodes=200 live=%d max/min=%.2f dist=%v", shards, fineLive, fineImb, fine)

	// Every node must earn its hardware bill.
	if fineLive != shards {
		t.Errorf("ring with 200 vnodes routes to only %d of %d shards (dist=%v).\n"+
			"Idle shards mean the vnode positions are clustered rather than spread, which\n"+
			"is a defect in how Ring.Add derives a position -- not in the vnode count.",
			fineLive, shards, fine)
	}

	// A well-mixed 8x200 ring lands close to even. 1.3 is loose on purpose:
	// this is sampling noise around a real distribution, not a fixed constant,
	// and tightening it would make the test brittle without teaching anything.
	if fineImb > 1.3 {
		t.Errorf("ring with 200 vnodes has max/min = %.2f, want under 1.3 (dist=%v).\n"+
			"At 1600 points on the circle the arc-length sums should be close to even;\n"+
			"if they are not, the position function is not spreading points uniformly.",
			fineImb, fine)
	}

	// And the whole justification for paying for 200 points per shard: it has
	// to be materially better than paying for 1.
	if !(coarseImb > 3*fineImb) {
		t.Errorf("1 vnode gives max/min %.2f and 200 vnodes gives %.2f.\n"+
			"Raising the vnode count 200x bought less than a 3x improvement in balance,\n"+
			"so vnodes are not doing their job. Either the position function ignores the\n"+
			"vnode index, or all the points are landing in the same region of the circle.",
			coarseImb, fineImb)
	}
}

// ---------------------------------------------------------------- Directory

// TestDirectoryLogicalIsInvariantAcrossPlacementEdits is THE property of the
// directory design, and everything else it can do is downstream of it.
//
// tenant -> logical is a pure function of the hash and LogicalCount, both
// frozen for the life of the system. logical -> physical is a table you may
// rewrite freely. So no matter how many times you move shards around, split
// nodes, or pin whales, a tenant's logical shard is the same value it was on
// day one. No tenant is ever rehashed, which means no reshard can ever lose a
// tenant by computing a different answer than it did yesterday.
//
// Compare mod-N, where the routing answer is a function of the current node
// count -- so a router that has not noticed the resize computes a different,
// confidently wrong answer.
func TestDirectoryLogicalIsInvariantAcrossPlacementEdits(t *testing.T) {
	d := NewDirectory(1024, 4)

	const sample = 20000
	want := make([]int, sample+1)
	for id := uint64(1); id <= sample; id++ {
		want[id] = d.Logical(id)
	}

	// Arbitrary, deliberately chaotic edits to the physical layer: a resize,
	// a scatter, an in-flight move, and a pin.
	for i := range d.Placement {
		d.Placement[i] = (i*7 + 3) % 8
	}
	d.BeginMove(11, 5)
	d.BeginMove(900, 0)
	d.Pin(42, 6)
	for i := range d.Placement {
		d.Placement[i] = i % 3
	}
	d.Commit(11)

	for id := uint64(1); id <= sample; id++ {
		if got := d.Logical(id); got != want[id] {
			t.Fatalf("tenant %d moved logical shard %d -> %d after physical edits.\n"+
				"The logical mapping must be frozen forever. If it can shift, then two\n"+
				"routers that disagree about the physical layer also disagree about which\n"+
				"logical shard a tenant is in, and the dual-write window protects nothing.",
				id, want[id], got)
		}
	}
}

// TestDirectoryDualWriteWindow walks the three states of an online move and
// asserts the routing at each one. This sequence is the entire reason the
// Directory exists; get it wrong and you lose writes during a reshard.
//
//	before  reads -> source, writes -> [source]
//	during  reads -> source, writes -> [source, target]
//	after   reads -> target, writes -> [target]
//
// Reads must stay on the source for the whole window. The target is being
// backfilled and is missing rows until the backfill completes, so a read that
// lands there returns a partial result -- which looks exactly like a customer
// losing data, because it is.
//
// Writes must go to both. A write that lands only on the source after the
// backfill has already copied that row's page is lost at cutover. Ordering
// matters too: source first, so that if the target write fails you have not
// yet acknowledged a write that only the doomed copy has.
func TestDirectoryDualWriteWindow(t *testing.T) {
	d := NewDirectory(1024, 4)

	const logical = 300 // NewDirectory puts logicals 256..511 on physical 1
	const target = 3
	source := d.Placement[logical]
	if source == target {
		t.Fatalf("test setup: logical %d already lives on the target shard %d", logical, target)
	}

	tenants := tenantsInLogical(t, d, logical, 5)
	bystander := uint64(0)
	for id := uint64(1); ; id++ {
		if d.Logical(id) != logical {
			bystander = id
			break
		}
	}
	bystanderShard := d.RouteRead(bystander)

	// --- before
	for _, id := range tenants {
		if got := d.RouteRead(id); got != source {
			t.Fatalf("before move: RouteRead(%d) = %d, want source %d", id, got, source)
		}
		if got := d.RouteWrite(id); !reflect.DeepEqual(got, []int{source}) {
			t.Fatalf("before move: RouteWrite(%d) = %v, want exactly one shard [%d].\n"+
				"Dual-writing outside a migration window doubles write cost and silently\n"+
				"creates a second copy nobody reconciles.", id, got, source)
		}
	}

	// --- during
	d.BeginMove(logical, target)
	for _, id := range tenants {
		if got := d.RouteRead(id); got != source {
			t.Errorf("during move: RouteRead(%d) = %d, want the SOURCE %d.\n"+
				"Reads must not follow the write until cutover: the target is still being\n"+
				"backfilled, so a read there returns a silently incomplete result set.",
				id, got, source)
		}
		got := d.RouteWrite(id)
		if !reflect.DeepEqual(got, []int{source, target}) {
			t.Errorf("during move: RouteWrite(%d) = %v, want [%d %d] (source first, then target).\n"+
				"Both copies must receive every write for the length of the window, or writes\n"+
				"that land after the backfill has passed their page are lost at cutover.",
				id, got, source, target)
		}
	}
	if got := d.RouteRead(bystander); got != bystanderShard {
		t.Errorf("during move: uninvolved tenant %d moved %d -> %d; a move must not perturb other logical shards",
			bystander, bystanderShard, got)
	}
	if got := d.RouteWrite(bystander); len(got) != 1 {
		t.Errorf("during move: uninvolved tenant %d is being dual-written to %v.\n"+
			"Only the migrating logical shard pays the dual-write cost.", bystander, got)
	}

	// --- after
	d.Commit(logical)
	if _, still := d.Migrating[logical]; still {
		t.Errorf("after commit: logical %d is still listed as migrating; the window never closes\n"+
			"and every write to it stays doubled forever", logical)
	}
	for _, id := range tenants {
		if got := d.RouteRead(id); got != target {
			t.Errorf("after commit: RouteRead(%d) = %d, want target %d", id, got, target)
		}
		if got := d.RouteWrite(id); !reflect.DeepEqual(got, []int{target}) {
			t.Errorf("after commit: RouteWrite(%d) = %v, want exactly [%d].\n"+
				"Cutover must close the window; a router that keeps dual-writing to the\n"+
				"drained source resurrects rows on a node nobody reads.", id, got, target)
		}
	}
}

// TestDirectoryPinOverridesHashAndIsNeverDualWritten covers the hot-tenant
// escape hatch, and one sharp edge in how it currently behaves.
//
// A pin sends one tenant to a chosen node regardless of hash or placement.
// This is how you give the customer who is 400x the median their own hardware
// without resharding anyone else. Build it on day one; you will use it.
//
// The sharp edge: a pinned tenant is NOT dual-written even when its logical
// shard has an open migration window. That is deliberate here -- the pin has
// already taken the tenant out of the logical shard's placement, so the
// migration of that logical shard has nothing of theirs to carry. But it means
// "pin a tenant" and "move a logical shard" are two different operations with
// two different safety stories, and moving a pinned tenant needs its own
// window rather than riding along on a shard move.
func TestDirectoryPinOverridesHashAndIsNeverDualWritten(t *testing.T) {
	d := NewDirectory(1024, 4)

	const pinnedTo = 2
	whale := tenantsInLogical(t, d, 700, 1)[0] // logicals 512..767 -> physical 2... pick a different node below
	logical := d.Logical(whale)
	byHash := d.Placement[logical]

	pinTarget := pinnedTo
	if byHash == pinTarget {
		pinTarget = (pinTarget + 1) % 4
	}
	d.Pin(whale, pinTarget)

	if got := d.RouteRead(whale); got != pinTarget {
		t.Fatalf("RouteRead(pinned %d) = %d, want the pin %d (hash would say %d).\n"+
			"A pin that the router ignores is worse than no pin: you provisioned dedicated\n"+
			"hardware and the traffic still lands on the shared node.", whale, got, pinTarget, byHash)
	}

	// The pin must beat the placement table, not merely agree with it.
	d.Placement[logical] = (pinTarget + 1) % 4
	if got := d.RouteRead(whale); got != pinTarget {
		t.Errorf("after editing Placement, RouteRead(pinned %d) = %d, want the pin %d.\n"+
			"Pins must sit above the placement table or a routine reshard silently\n"+
			"un-pins your largest customer.", whale, got, pinTarget)
	}

	// A migration window on the underlying logical shard must not drag the
	// pinned tenant into a dual write.
	d.BeginMove(logical, 3)
	got := d.RouteWrite(whale)
	if !reflect.DeepEqual(got, []int{pinTarget}) {
		t.Errorf("RouteWrite(pinned %d) = %v during a move of logical %d, want exactly [%d].\n"+
			"A pinned tenant is not part of its logical shard's placement, so it must not\n"+
			"be dual-written by that shard's migration. Moving a pinned tenant is a separate\n"+
			"operation that needs its own window.", whale, got, logical, pinTarget)
	}

	// Neighbours in the same logical shard are unaffected by the pin.
	for _, id := range tenantsInLogical(t, d, logical, 4) {
		if id == whale {
			continue
		}
		if got := d.RouteRead(id); got != d.Placement[logical] {
			t.Errorf("pinning %d moved co-located tenant %d to %d, want %d: a pin is one tenant, not a range",
				whale, id, got, d.Placement[logical])
		}
	}
}

// TestDirectorySaveLoadRoundTrip checks that the metadata store preserves
// every field routing depends on.
//
// Migrating is the field to watch. If a save drops it, a router that reloads
// mid-migration stops dual-writing while the backfill is still running, and
// every write in that gap is lost at cutover with no error anywhere. An
// omitempty on a map that is legitimately empty is fine; an omitempty that
// swallows a live migration is a data-loss bug that only fires during the one
// operation you were most careful about.
func TestDirectorySaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "topology.json")

	orig := NewDirectory(1024, 4)
	orig.Placement[0] = 3
	orig.Placement[1023] = 0
	orig.Placement[512] = 7
	orig.BeginMove(77, 6)
	orig.BeginMove(78, 6)
	orig.Pin(42, 5)
	orig.Pin(18446744073709551615, 1) // max uint64: JSON must not round-trip this through float64

	if err := orig.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadDirectory(path)
	if err != nil {
		t.Fatalf("LoadDirectory: %v", err)
	}

	if got.LogicalCount != orig.LogicalCount {
		t.Errorf("LogicalCount = %d, want %d. This value must never change for the life of the\n"+
			"system; a load that alters it rehashes every tenant.", got.LogicalCount, orig.LogicalCount)
	}
	if !reflect.DeepEqual(got.Placement, orig.Placement) {
		t.Errorf("Placement did not round-trip:\n got %v\nwant %v", got.Placement, orig.Placement)
	}
	if !reflect.DeepEqual(got.Migrating, orig.Migrating) {
		t.Errorf("Migrating did not round-trip: got %v want %v.\n"+
			"A router that reloads mid-migration and loses this map stops dual-writing while\n"+
			"the backfill is still running. Every write in that gap is lost at cutover.",
			got.Migrating, orig.Migrating)
	}
	if !reflect.DeepEqual(got.Pins, orig.Pins) {
		t.Errorf("Pins did not round-trip: got %v want %v.\n"+
			"Losing a pin sends your largest tenant back onto shared hardware.", got.Pins, orig.Pins)
	}

	// Routing decisions, not just fields, must survive the round trip.
	for id := uint64(1); id <= 5000; id++ {
		if a, b := orig.RouteRead(id), got.RouteRead(id); a != b {
			t.Fatalf("tenant %d routes to %d before save and %d after load", id, a, b)
		}
		if a, b := orig.RouteWrite(id), got.RouteWrite(id); !reflect.DeepEqual(a, b) {
			t.Fatalf("tenant %d writes to %v before save and %v after load", id, a, b)
		}
	}
}

// TestDirectoryMovesExactlyOneLogicalShard is the granularity property, and
// it is the practical payoff of the whole design.
//
// Moving one logical shard moves the tenants in that logical shard and nobody
// else. Not "roughly", not "mostly" -- exactly. That is what lets you rehearse
// a reshard on your smallest logical shard on a Tuesday afternoon, watch it,
// and then do the other 511 with the same runbook.
//
// Mod-N cannot offer this at any price: its unit of change is the node count,
// so its smallest possible reshard is (n-1)/n of the cluster. A ring is
// better but still cannot target -- you get the keys the arcs happen to hand
// you, and you cannot choose to move one noisy tenant.
func TestDirectoryMovesExactlyOneLogicalShard(t *testing.T) {
	d := NewDirectory(1024, 4)

	const sample = 40000
	const logical = 42
	const target = 7

	source := d.Placement[logical]
	before := make([]int, sample+1)
	for id := uint64(1); id <= sample; id++ {
		before[id] = d.RouteRead(id)
	}

	d.BeginMove(logical, target)
	d.Commit(logical)

	movedInShard, movedOutside, expected := 0, 0, 0
	for id := uint64(1); id <= sample; id++ {
		inShard := d.Logical(id) == logical
		if inShard {
			expected++
		}
		after := d.RouteRead(id)
		if after == before[id] {
			if inShard && source != target {
				t.Errorf("tenant %d is in logical %d but did not move off %d", id, logical, source)
			}
			continue
		}
		if inShard {
			movedInShard++
			if after != target {
				t.Errorf("tenant %d moved to %d, want the migration target %d", id, after, target)
			}
		} else {
			movedOutside++
			t.Errorf("tenant %d is NOT in logical shard %d but moved %d -> %d.\n"+
				"Moving one logical shard must not touch any other tenant. Collateral movement\n"+
				"means the blast radius of a reshard is unbounded and cannot be rehearsed.",
				id, logical, before[id], after)
		}
	}

	t.Logf("moving logical %d (%d -> %d) moved %d of %d sampled tenants (%.4f), collateral: %d",
		logical, source, target, movedInShard, sample, float64(movedInShard)/float64(sample), movedOutside)

	if movedInShard != expected {
		t.Errorf("moved %d tenants, want all %d tenants of logical shard %d", movedInShard, expected, logical)
	}
	if expected == 0 {
		t.Fatalf("test is vacuous: no sampled tenant falls in logical shard %d", logical)
	}
}
