package shard

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestNewIDGenRejectsShardIDsAboveTheBitBudget guards a constant that is
// harmless at 4 shards and an incident at 2000.
//
// The id layout spends 10 bits on the shard, so shard 1024 does not "wrap
// harmlessly" -- it overflows into the timestamp field and produces ids from
// roughly 35 years in the future that decode to shard 0. Those ids sort ahead
// of everything real forever, and they collide with shard 0's id space.
//
// The operational shape of this bug: it cannot happen for years, and then one
// day capacity planning says "grow to 2000 shards" and the constraint is
// discovered by the shard that will not start. Fail loudly at construction,
// where the failure is a startup error and not a data corruption.
func TestNewIDGenRejectsShardIDsAboveTheBitBudget(t *testing.T) {
	for _, shard := range []int{-1, maxShardID + 1, 2000, 1 << 20} {
		if g, err := NewIDGen(shard); err == nil {
			t.Errorf("NewIDGen(%d) returned a generator (%p) with no error, want a rejection.\n"+
				"%d bits of shard id is a hard ceiling of %d physical shards. A shard id past\n"+
				"the ceiling overflows into the timestamp field: the ids decode to the wrong\n"+
				"shard, sort out of order, and collide with a real shard's id space.",
				shard, g, shardBits, maxShardID)
		}
	}
	// The boundary itself must still be allowed; an off-by-one here costs you
	// the last shard in the cluster.
	for _, shard := range []int{0, maxShardID} {
		if _, err := NewIDGen(shard); err != nil {
			t.Errorf("NewIDGen(%d) = %v, want it accepted: %d is inside the %d-bit budget",
				shard, err, shard, shardBits)
		}
	}
}

// TestDecodeIDRecoversTheShard checks the property that lets GET /orders/{id}
// route without a directory lookup.
//
// Worth restating the trap in the id.go comment, because this test is the one
// that makes the property look safe: the encoded shard is where the row was
// CREATED, not where it lives. After a reshard the two differ, and any code
// that treats DecodeID's shard as the current location will read from the
// drained node and find nothing.
func TestDecodeIDRecoversTheShard(t *testing.T) {
	for _, shard := range []int{0, 1, 7, 512, maxShardID} {
		g, err := NewIDGen(shard)
		if err != nil {
			t.Fatalf("NewIDGen(%d): %v", shard, err)
		}
		id := g.Next()
		createdAt, gotShard, gotSeq := DecodeID(id)

		if gotShard != shard {
			t.Errorf("DecodeID(%d) shard = %d, want %d: the id no longer identifies its origin shard",
				id, gotShard, shard)
		}
		if gotSeq < 0 || gotSeq > maxSeq {
			t.Errorf("DecodeID(%d) seq = %d, outside 0..%d", id, gotSeq, maxSeq)
		}
		if skew := time.Since(createdAt); skew < -time.Minute || skew > time.Minute {
			t.Errorf("DecodeID(%d) timestamp = %s, which is %s away from now.\n"+
				"The embedded time is used for time-ordered index locality and for retention;\n"+
				"a wrong epoch here silently misfiles every row.",
				id, createdAt.Format(time.RFC3339), skew)
		}
	}
}

// TestIDsAreStrictlyIncreasingAndUnique is the whole reason for a snowflake id
// instead of a UUIDv4.
//
// Uniqueness is the correctness requirement: the id is a primary key on a
// shard, and a duplicate is a constraint violation at best and an overwritten
// order at worst.
//
// Strict monotonicity is the performance requirement, and it is the one people
// discard when they reach for UUIDv4. Ids that increase over time mean every
// INSERT appends to the rightmost B-tree page, so the hot part of the index
// stays in shared_buffers and page splits are cheap appends. Random ids write
// to a random page every time: the whole index becomes the working set, the
// buffer cache stops helping, and write amplification climbs. This is the
// difference between an index that fits in RAM and one that does not.
func TestIDsAreStrictlyIncreasingAndUnique(t *testing.T) {
	const n = 50000

	g, err := NewIDGen(9)
	if err != nil {
		t.Fatalf("NewIDGen: %v", err)
	}

	seen := make(map[int64]int, n)
	prev := int64(-1)
	start := time.Now()

	for i := 0; i < n; i++ {
		id := g.Next()

		if id <= prev {
			t.Fatalf("id %d at position %d is not greater than the previous id %d.\n"+
				"Non-monotonic ids destroy B-tree insert locality: every INSERT lands on a\n"+
				"random index page instead of appending to the rightmost one.", id, i, prev)
		}
		prev = id

		if first, dup := seen[id]; dup {
			t.Fatalf("id %d generated twice, at positions %d and %d: this is a primary key",
				id, first, i)
		}
		seen[id] = i

		if _, shard, _ := DecodeID(id); shard != 9 {
			t.Fatalf("id %d at position %d decodes to shard %d, want 9", id, i, shard)
		}
	}

	t.Logf("%d ids in %s (the generator is rate-limited to %d ids/ms by design)",
		n, time.Since(start).Round(time.Microsecond), maxSeq+1)
}

// TestIDSequenceRolloverDoesNotDuplicate drives the 12-bit sequence past its
// ceiling inside a single millisecond.
//
// 4096 ids per millisecond is roughly 4 million per second per shard, which
// sounds like plenty right up until a backfill loop. The generator's answer is
// to stall until the clock advances rather than hand out a duplicate: the
// right trade, since a few hundred microseconds of latency is recoverable and
// a duplicate primary key is not.
//
// The forced case below is white-box on purpose. Driving rollover by racing
// the wall clock only works on a machine fast enough to emit 4096 ids in a
// millisecond, so it would pass on a laptop and quietly skip itself in CI.
func TestIDSequenceRolloverDoesNotDuplicate(t *testing.T) {
	t.Run("forced at the ceiling", func(t *testing.T) {
		g, err := NewIDGen(4)
		if err != nil {
			t.Fatalf("NewIDGen: %v", err)
		}

		first := g.Next()
		// Pretend the current millisecond has already issued all 4096 ids.
		g.mu.Lock()
		g.seq = maxSeq
		exhausted := g.lastMs
		g.mu.Unlock()

		next := g.Next()
		ms, shard, seq := DecodeID(next)

		if next <= first {
			t.Errorf("after sequence exhaustion, id %d is not greater than %d", next, first)
		}
		if shard != 4 {
			t.Errorf("rollover produced shard %d, want 4: the sequence overflowed into the shard bits", shard)
		}
		if seq > maxSeq {
			t.Errorf("rollover produced seq %d, above the %d-bit ceiling %d", seq, seqBits, maxSeq)
		}
		if gotMs := ms.UnixMilli() - idEpochMs; gotMs <= exhausted {
			t.Errorf("rollover reused millisecond %d (exhausted at %d).\n"+
				"Once a millisecond's 4096 sequence values are spent the generator must wait\n"+
				"for the clock, not wrap the sequence and reissue ids it already handed out.",
				gotMs, exhausted)
		}
	})

	t.Run("tight loop stays within the budget", func(t *testing.T) {
		// Generate far more than one millisecond's worth as fast as possible.
		// The invariant is machine-independent even if rollover never triggers
		// on a slow machine: no millisecond may ever contain more than 4096
		// ids, because there is nowhere to put the 4097th.
		const n = 40000

		g, err := NewIDGen(11)
		if err != nil {
			t.Fatalf("NewIDGen: %v", err)
		}

		perMs := map[int64]int{}
		seen := make(map[int64]struct{}, n)
		for i := 0; i < n; i++ {
			id := g.Next()
			if _, dup := seen[id]; dup {
				t.Fatalf("duplicate id %d at position %d while hammering the sequence", id, i)
			}
			seen[id] = struct{}{}
			perMs[id>>timeShift]++
		}

		full := 0
		for ms, count := range perMs {
			if count > maxSeq+1 {
				t.Errorf("millisecond %d issued %d ids, above the ceiling of %d: the sequence wrapped",
					ms, count, maxSeq+1)
			}
			if count == maxSeq+1 {
				full++
			}
		}
		t.Logf("%d ids spread over %d milliseconds; %d of them hit the %d-id ceiling and forced a rollover",
			n, len(perMs), full, maxSeq+1)
	})
}

// TestIDGenClockGoingBackwardsStillIncreases covers the branch nobody exercises
// until an NTP step or a VM live-migration moves the clock backwards.
//
// The generator refuses to follow the clock down. It pins lastMs and keeps
// incrementing the sequence, which is the only safe choice: emitting ids from
// a millisecond that has already been used means reissuing ids that are
// already primary keys on that shard.
//
// See TestIDGenClockSkewOverflowsIntoShardBits for the limit of that strategy.
func TestIDGenClockGoingBackwardsStillIncreases(t *testing.T) {
	g, err := NewIDGen(6)
	if err != nil {
		t.Fatalf("NewIDGen: %v", err)
	}

	first := g.Next()

	// Simulate a clock that has just stepped ~10 seconds backwards, by telling
	// the generator its last issued millisecond is 10s ahead of wall time.
	g.mu.Lock()
	g.lastMs += 10_000
	skewedMs := g.lastMs
	g.mu.Unlock()

	prev := first
	for i := 0; i < 100; i++ {
		id := g.Next()
		if id <= prev {
			t.Fatalf("id %d at position %d did not increase past %d during clock skew.\n"+
				"Following the clock backwards reissues ids that are already primary keys.",
				id, i, prev)
		}
		ms, shard, _ := DecodeID(id)
		if got := ms.UnixMilli() - idEpochMs; got != skewedMs {
			t.Errorf("id %d encodes millisecond %d, want the pinned %d", id, got, skewedMs)
		}
		if shard != 6 {
			t.Errorf("id %d decodes to shard %d, want 6", id, shard)
		}
		prev = id
	}
}

// TestIDGenClockSkewOverflowsIntoShardBits is a characterization test: it pins
// behaviour that is WRONG, so the defect is visible in test output rather than
// discovered from a duplicate-key incident after an NTP step.
//
// IDGen.Next has a sequence-ceiling check in the `ms == lastMs` branch, which
// spins to the next millisecond rather than overflow. The `default:` branch --
// the clock-went-backwards path -- has no such check:
//
//	default:
//	    ms = g.lastMs
//	    g.seq++          // unbounded
//
// So during a backwards clock step the sequence increments without a ceiling.
// Past 4096 it carries into the shard field, and the ids produced decode to
// shard+1 and occupy shard+1's id space. On a cluster where shard+1 is also
// issuing ids, those are not merely mislabelled -- they are collisions on a
// primary key, generated by a node that believes it is behaving correctly.
//
// The window is real: an NTP step of a few seconds on a shard doing more than
// 4096 writes/sec is enough, and every write in the window is affected.
//
// The fix belongs in IDGen.Next, not here: the `default:` branch needs the
// same ceiling handling as the `ms == lastMs` branch (roll lastMs forward and
// reset seq, rather than letting seq grow past maxSeq). Whoever fixes it will
// see this test fail, which is the intent -- delete it and assert the shard is
// preserved for an unbounded number of ids during skew.
func TestIDGenClockSkewOverflowsIntoShardBits(t *testing.T) {
	const shard = 6

	g, err := NewIDGen(shard)
	if err != nil {
		t.Fatalf("NewIDGen: %v", err)
	}
	g.Next()

	g.mu.Lock()
	g.lastMs += 60_000 // clock stepped a minute backwards
	g.mu.Unlock()

	var wrongShard int64
	var firstBad int64
	for i := 0; i <= maxSeq+8; i++ {
		id := g.Next()
		if _, got, _ := DecodeID(id); got != shard {
			if wrongShard == 0 {
				firstBad = id
			}
			wrongShard++
		}
	}

	if wrongShard == 0 {
		t.Errorf("the sequence no longer overflows into the shard bits during a backwards clock step.\n" +
			"That is the CORRECT behaviour, so IDGen.Next has presumably been fixed. Good.\n" +
			"Delete this characterization test and replace it with an assertion that the shard\n" +
			"is preserved for an unbounded number of ids while the clock is skewed.")
		return
	}

	_, badShard, _ := DecodeID(firstBad)
	t.Logf("KNOWN DEFECT: %d of %d ids issued during clock skew decoded to shard %d instead of %d "+
		"(first was id %d). These collide with shard %d's id space.",
		wrongShard, maxSeq+9, badShard, shard, firstBad, badShard)
}

// TestIDGenIsConcurrencySafe defends the mutex.
//
// One generator per process, shared by every request handler, is the normal
// deployment. The lock is the only thing making the read-modify-write of
// (lastMs, seq) atomic, and without it two goroutines that observe the same
// millisecond both compute the same sequence value and emit the same id.
//
// Run this with -race. A missing lock here is not merely a data race in the
// abstract; it is duplicate primary keys under exactly the load that makes
// them hardest to reproduce.
func TestIDGenIsConcurrencySafe(t *testing.T) {
	const (
		goroutines = 8
		perG       = 10000
	)

	g, err := NewIDGen(12)
	if err != nil {
		t.Fatalf("NewIDGen: %v", err)
	}

	batches := make([][]int64, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for w := 0; w < goroutines; w++ {
		go func(w int) {
			defer wg.Done()
			out := make([]int64, perG)
			for i := range out {
				out[i] = g.Next()
			}
			batches[w] = out
		}(w)
	}
	wg.Wait()

	seen := make(map[int64]string, goroutines*perG)
	for w, batch := range batches {
		for i, id := range batch {
			where := fmt.Sprintf("goroutine %d index %d", w, i)
			if prev, dup := seen[id]; dup {
				t.Fatalf("duplicate id %d issued to %s and to %s.\n"+
					"Concurrent callers observed the same (millisecond, sequence) state, which\n"+
					"means the generator's read-modify-write is not actually serialised.",
					id, prev, where)
			}
			seen[id] = where

			if _, shard, _ := DecodeID(id); shard != 12 {
				t.Fatalf("id %d from %s decodes to shard %d, want 12: a torn write corrupted the shard bits",
					id, where, shard)
			}
		}
	}

	if len(seen) != goroutines*perG {
		t.Errorf("collected %d distinct ids, want %d", len(seen), goroutines*perG)
	}
}
