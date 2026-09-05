package shard

import (
	"fmt"
	"sync"
	"time"
)

// Snowflake-style ids: 41 bits of millisecond time, 10 bits of shard, 12 bits
// of per-millisecond sequence.
//
// Why not bigserial? Because a sequence is per-node, so shard 0 and shard 3
// both hand out id 1000 and you cannot merge, cache, or log them together.
// Why not UUIDv4? Because random ids destroy B-tree locality on insert -- the
// index write lands on a random page every time. UUIDv7 is the modern answer
// and has the same time-ordered property as this.
//
// The 10 shard bits buy something extra: the shard is *recoverable from the
// id*, so `GET /orders/{id}` routes without a lookup. That is a real
// convenience and also a trap -- it welds the id format to the topology, so a
// tenant that moves shard keeps ids claiming the old one. Treat the encoded
// shard as a hint (where it was created), never as the current location.
const (
	idEpochMs  = 1704067200000 // 2024-01-01T00:00:00Z
	shardBits  = 10
	seqBits    = 12
	maxShardID = (1 << shardBits) - 1
	maxSeq     = (1 << seqBits) - 1
	shardShift = seqBits
	timeShift  = seqBits + shardBits
)

type IDGen struct {
	mu     sync.Mutex
	shard  int
	lastMs int64
	seq    int64
}

func NewIDGen(shard int) (*IDGen, error) {
	if shard < 0 || shard > maxShardID {
		return nil, fmt.Errorf("shard %d out of range 0..%d", shard, maxShardID)
	}
	return &IDGen{shard: shard}, nil
}

func (g *IDGen) Next() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := time.Now().UnixMilli() - idEpochMs
	switch {
	case ms == g.lastMs:
		g.seq++
		if g.seq > maxSeq {
			// 4096 ids per millisecond exhausted: spin to the next ms rather
			// than hand out a duplicate.
			for ms <= g.lastMs {
				time.Sleep(200 * time.Microsecond)
				ms = time.Now().UnixMilli() - idEpochMs
			}
			g.seq = 0
		}
	case ms > g.lastMs:
		g.seq = 0
	default:
		// Clock went backwards (NTP step, VM migration). Refusing to go back
		// is the only safe move; the alternative is duplicate ids.
		ms = g.lastMs
		g.seq++
	}
	g.lastMs = ms

	return ms<<timeShift | int64(g.shard)<<shardShift | g.seq
}

// DecodeID recovers what the generator encoded.
func DecodeID(id int64) (createdAt time.Time, shard int, seq int) {
	seq = int(id & maxSeq)
	shard = int((id >> shardShift) & maxShardID)
	ms := id >> timeShift
	return time.UnixMilli(ms + idEpochMs).UTC(), shard, seq
}
