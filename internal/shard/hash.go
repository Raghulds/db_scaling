package shard

import (
	"encoding/binary"
	"hash/fnv"
)

// Key is the one hash function every routing decision in this repo goes
// through.
//
// Two properties matter more than speed:
//
//  1. Stability. If this function ever changes, every tenant moves shard and
//     you have silently lost their data. Pin it, test it with golden values
//     (see hash_test.go), and never "improve" it. This is why production
//     systems avoid Go's map hash, hash/maphash, and anything seeded per
//     process.
//  2. Avalanche. Sequential tenant ids must not land in sequential shards, or
//     a bulk signup lands entirely on one node. FNV-1a is adequate here;
//     xxhash or murmur3 are the usual production picks.
func Key(tenantID uint64) uint64 {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], tenantID)
	h := fnv.New64a()
	_, _ = h.Write(b[:])
	return h.Sum64()
}
