package shard

import "testing"

// The routing hash is the most load-bearing constant in a sharded system.
// These tests defend three separate properties, and it is worth being clear
// that they are separate, because a hash can pass one and fail another:
//
//	stability     -- the same tenant maps to the same value forever
//	balance mod N -- the low bits cycle evenly, so hash%N fills every shard
//	ring order    -- the full 64-bit values are spread evenly around 2^64
//
// Key() passes the first two and fails the third. See
// TestKeyIsEvenModuloNButClustersInRingOrder below, which measures the
// failure. Key is not fixed for it -- it cannot be, its golden values are
// load-bearing -- so Ring compensates instead, by running Key's output
// through the fmix64 finalizer in topology.go before placing anything on the
// circle. Read the two files together: the hash is weak, and the one consumer
// that cares about the weakness is the one that pays to work around it.

// TestKeyGoldenValues pins the exact output of Key for four tenant ids.
//
// This test exists so that nobody ever "optimises" the hash function. If it
// fails, every tenant in production just changed shard: reads go to a node
// that has none of their rows, writes go to a node that will now hold a second
// partial copy, and there is no migration to undo it because nothing was
// migrated. The failure is silent at the database layer -- every query
// succeeds and returns zero rows.
//
// The correct response to a failure here is to revert the hash, not to update
// the constants. Updating the constants is how you find out in production.
//
// If you genuinely must change the hash (say FNV-1a is too weak for your ring,
// which as it happens it is), that is not an edit to this function: it is a
// versioned second hash, a directory that records which tenants use which
// version, and a migration. Same shape of work as any other reshard.
func TestKeyGoldenValues(t *testing.T) {
	golden := []struct {
		tenantID uint64
		want     uint64
	}{
		{1, 12161961113530546194},
		{2, 12161960014018917983},
		{42, 12161933625739840919},
		{1000000, 2740105236383194386},
	}
	for _, g := range golden {
		if got := Key(g.tenantID); got != g.want {
			t.Errorf("Key(%d) = %d, want %d\n"+
				"the routing hash changed. Every tenant just moved shard and no data moved with them.\n"+
				"Revert the change to Key; do not update this constant.",
				g.tenantID, got, g.want)
		}
	}
}

// TestKeySequentialTenantsSpreadAcrossAllShards is the avalanche smoke test.
//
// Tenant ids come out of a sequence, so a bulk signup, a backfill, or a
// migration that creates tenants in a loop hands you consecutive ids. If
// consecutive ids map to consecutive shards, that whole batch lands on one
// node in id order and you have built a hot shard out of nothing but INSERT
// ordering.
//
// Note what this test does NOT claim. FNV-1a over consecutive integers is not
// avalanche in any real sense: the mapping below is the fixed rotation
//
//	shard(n+1) = shard(n) + 5  (mod 8)
//
// which is a permutation of the residues, not a scramble. That is sufficient
// for the property that matters here -- consecutive tenants land on different
// shards, and 8 consecutive tenants cover all 8 shards exactly once -- but it
// is emphatically not a strong hash. If you need one, xxhash or murmur3.
func TestKeySequentialTenantsSpreadAcrossAllShards(t *testing.T) {
	const shards = 8
	topo := ModN{N: shards}

	mapping := make([]int, 0, 64)
	for tenant := 1; tenant <= 64; tenant++ {
		mapping = append(mapping, topo.RouteRead(uint64(tenant)))
	}
	t.Logf("tenants 1..64 over %d shards: %v", shards, mapping)

	// Property 1: not the identity pattern. If Key were the identity function
	// (or any monotone function of the id), tenant n would sit on shard n%8
	// and a batch of new tenants would fill shards in lockstep.
	identity := true
	for tenant := 1; tenant <= 64; tenant++ {
		if mapping[tenant-1] != tenant%shards {
			identity = false
			break
		}
	}
	if identity {
		t.Error("tenants 1..64 map to shard id%8: the hash is not hashing.\n" +
			"A sequential bulk signup would fill shards in lockstep and every\n" +
			"range of tenant ids would be co-located on one node.")
	}

	// Property 2: consecutive tenants never land on the same shard, so a batch
	// of new tenants is spread on arrival rather than after a rebalance.
	for i := 1; i < len(mapping); i++ {
		if mapping[i] == mapping[i-1] {
			t.Errorf("tenants %d and %d both map to shard %d: consecutive ids collide,\n"+
				"so a bulk signup concentrates on one node",
				i, i+1, mapping[i])
		}
	}

	// Property 3: every shard is reachable. A hash that never emits shard 7
	// leaves a node idle while the other seven carry the load, and you will
	// not notice until you look at per-node disk usage.
	hit := map[int]bool{}
	for _, s := range mapping {
		hit[s] = true
	}
	for s := 0; s < shards; s++ {
		if !hit[s] {
			t.Errorf("shard %d received none of tenants 1..64: a node in the cluster is dead weight", s)
		}
	}
}

// TestKeyDistributionIsEvenAcrossShards checks the property you actually pay
// for at 2am: no shard is meaningfully larger than any other.
//
// A 1.1 max/min ratio is a deliberately loose bar. Real skew in a multi-tenant
// system does not come from the hash -- it comes from one tenant being 400x
// the size of the median, which no hash function can fix. That is what the
// Directory's Pin exists for. This test only proves the hash is not itself
// the source of the skew, so that when you see an uneven cluster you look at
// tenant sizes and not at this function.
func TestKeyDistributionIsEvenAcrossShards(t *testing.T) {
	const (
		tenants  = 100000
		shards   = 8
		maxRatio = 1.1
	)
	dist := Distribution(ModN{N: shards}, tenants)

	minCount, maxCount := tenants+1, 0
	for s := 0; s < shards; s++ {
		c := dist[s]
		if c < minCount {
			minCount = c
		}
		if c > maxCount {
			maxCount = c
		}
	}
	if minCount == 0 {
		t.Fatalf("a shard received zero of %d tenants: %v", tenants, dist)
	}

	ratio := float64(maxCount) / float64(minCount)
	t.Logf("%d tenants over %d shards: min=%d max=%d ratio=%.4f", tenants, shards, minCount, maxCount, ratio)
	if ratio >= maxRatio {
		t.Errorf("shard imbalance ratio %.4f >= %.2f (min=%d max=%d, dist=%v)\n"+
			"The hash itself is skewing tenant placement, which means the largest\n"+
			"shard hits its disk and IOPS ceiling before the others are half full.",
			ratio, maxRatio, minCount, maxCount, dist)
	}
}

// TestKeyIsEvenModuloNButClustersInRingOrder is the test that explains why
// Ring cannot use Key's output directly, and it is the most useful thing in
// this file.
//
// FNV-1a ends with `h ^= lastByte; h *= prime`. Every 8-byte input in 0..255
// therefore shares the same pre-state h and differs only in the low 8 bits
// that get XORed in, so the outputs are 256 points spaced at most
//
//	255 * 1099511628211 = 280375465193805
//
// apart -- about one 65790th of the 2^64 keyspace. Not a scatter. A speck.
//
// Two consequences, pointing in opposite directions:
//
//   - hash % N is fine. The multiplier is odd, so the low bits still cycle
//     through every residue. That is why TestKeyDistributionIsEvenAcrossShards
//     passes and why ModN is a perfectly balanced topology.
//
//   - Ring order is destroyed. Consistent hashing ignores the low bits; it
//     sorts the full 64-bit values and asks which arc a key falls into. Vnode
//     i of shard s is derived from s<<32|i, and i runs 0..199 -- exactly the
//     small-input regime above. Hand raw Key output to a ring and a shard's
//     200 vnodes do not spread around the circle, they pile into one speck,
//     and 199 of them do no work. That is measured here as Property 2; the
//     ring's answer to it is ringPos in topology.go.
//
// The general lesson outlives the specific bug: "the hash distributes evenly"
// is a claim about one consumer, not about the hash. Verify it against the
// consumer you actually have. A hash that is uniform mod N can be useless for
// consistent hashing, range partitioning, or any scheme that buckets on
// leading bits.
func TestKeyIsEvenModuloNButClustersInRingOrder(t *testing.T) {
	const fnvPrime = uint64(1099511628211)

	// Property 1: small inputs collapse into a tiny arc of the ring.
	lo, hi := ^uint64(0), uint64(0)
	for n := uint64(0); n < 256; n++ {
		h := Key(n)
		if h < lo {
			lo = h
		}
		if h > hi {
			hi = h
		}
	}
	span := hi - lo
	if span > 255*fnvPrime {
		t.Errorf("Key over inputs 0..255 spans %d, want at most %d.\n"+
			"If this grew, Key changed; check TestKeyGoldenValues first.", span, 255*fnvPrime)
	}
	t.Logf("Key(0..255) occupies a span of %d, which is 1/%d of the 2^64 keyspace",
		span, (1<<62)/(span/4))

	// Property 2: stated the way consistent hashing sees it. Cut the circle
	// into 65536 equal arcs; the RAW Key values for a shard's 200 vnodes land
	// in at most 2 of them, no matter how many vnodes there are. This is the
	// mechanism, and it is why Ring.Add does not use these values directly.
	const shards, vnodes = 8, 200
	for s := 0; s < shards; s++ {
		arcs := map[uint64]bool{}
		for i := 0; i < vnodes; i++ {
			arcs[Key(uint64(s)<<32|uint64(i))>>48] = true
		}
		if len(arcs) > 2 {
			t.Errorf("raw Key values for shard %d's %d vnodes now cover %d of 65536 ring arcs (was 2).\n"+
				"That would mean Key itself changed, which TestKeyGoldenValues should have caught\n"+
				"first. Note this measures Key, not Ring: Ring already mixes these through\n"+
				"ringPos, so a change here does not make the ring better or worse.",
				s, vnodes, len(arcs))
		}
	}

	// Property 3: the very same inputs are perfectly uniform mod 8. Even
	// distribution and good hashing are not the same claim.
	counts := map[uint64]int{}
	for n := uint64(0); n < 8000; n++ {
		counts[Key(n)%shards]++
	}
	for s := uint64(0); s < shards; s++ {
		if counts[s] != 8000/shards {
			t.Errorf("mod-%d bucket %d got %d, want %d (dist=%v)", shards, s, counts[s], 8000/shards, counts)
		}
	}
}
