package shard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Topology answers the only question a shard router asks: given a tenant,
// which physical node holds its rows?
//
// Three implementations, in increasing order of how much you would want them
// at 3am during a reshard:
//
//	ModN      -- hash % n. Simplest, and rehashes almost everything on resize.
//	Ring      -- consistent hashing. Resize moves ~1/n of keys, but you do not
//	             get to choose *which* keys, and you cannot pin a whale.
//	Directory -- tenant -> logical shard (fixed forever) -> physical shard
//	             (a mutable lookup table). More moving parts, total control.
//
// Lab 05 makes the difference concrete by resharding 4 -> 8 with each one.
type Topology interface {
	Name() string
	// PhysicalCount is how many nodes this topology can address.
	PhysicalCount() int
	// RouteRead returns the shard that currently owns the tenant's rows.
	RouteRead(tenantID uint64) int
	// RouteWrite returns every shard a write must land on. It has more than
	// one element only while a migration window is open for that tenant.
	RouteWrite(tenantID uint64) []int
}

// ---------------------------------------------------------------- ModN

// ModN is hash(tenant) % n.
//
// The failure mode is not subtle: change n and (n-1)/n of your tenants move.
// Doubling is the one merciful case -- exactly half move -- which is why
// mod-N shops always resize by doubling, and why they can never do it
// gradually.
type ModN struct{ N int }

func (m ModN) Name() string              { return fmt.Sprintf("mod-%d", m.N) }
func (m ModN) PhysicalCount() int        { return m.N }
func (m ModN) RouteRead(t uint64) int    { return int(Key(t) % uint64(m.N)) }
func (m ModN) RouteWrite(t uint64) []int { return []int{m.RouteRead(t)} }

// ---------------------------------------------------------------- Ring

type ringPoint struct {
	hash  uint64
	shard int
}

// Ring is consistent hashing with virtual nodes.
//
// VNodes per shard trades memory for balance: 1 vnode gives wildly uneven
// shards, 100-200 is the usual range. Adding one shard to an n-shard ring
// moves roughly 1/(n+1) of keys, and only from existing shards to the new
// one -- no key ever moves between two existing shards. That is the property
// mod-N lacks.
type Ring struct {
	points []ringPoint // sorted by hash
	shards []int
	vnodes int
}

func NewRing(shards []int, vnodesPerShard int) *Ring {
	r := &Ring{vnodes: vnodesPerShard}
	for _, s := range shards {
		r.Add(s)
	}
	return r
}

// ringPos maps a hash value to a position on the circle.
//
// This extra step is not decoration. Key is FNV-1a, and FNV-1a's avalanche is
// weak in exactly the direction a ring cares about: over small, near
// consecutive inputs its output is very nearly an arithmetic progression with
// a constant stride (pinned in TestKeyIsEvenModuloNButClustersInRingOrder).
//
// ModN and Directory survive that, because taking a modulus only ever reads
// the low bits and the stride happens to be coprime with the shard count. A
// ring does not survive it, because a ring compares the FULL 64-bit value.
// Feed raw Key output to a ring and shard s's vnodes 0..199 land as 200
// adjacent points inside one narrow band instead of being scattered around
// the circle. Eight shards become eight contiguous arcs covering about a
// third of the keyspace, nearly every tenant falls into the same one or two
// arcs, and adding vnodes buys nothing at all. Measured before this fix: 8
// shards, 200 vnodes each, and two of the eight owned 100% of the traffic.
//
// The mixer below is MurmurHash3's fmix64 finalizer. It is deliberately NOT a
// second routing hash -- Key still decides everything and its golden values
// are untouched. This is only the map from a hash value onto the circle, and
// both vnodes and tenants go through it, so the ring stays self-consistent.
//
// The general lesson: a hash that is "good enough" for hash % n can be
// useless for a ring, because the two designs read different bits.
func ringPos(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

func (r *Ring) Add(shard int) {
	for i := 0; i < r.vnodes; i++ {
		// position of (shard, vnode index) on the ring
		h := ringPos(Key(uint64(shard)<<32 | uint64(i)))
		r.points = append(r.points, ringPoint{hash: h, shard: shard})
	}
	r.shards = append(r.shards, shard)
	sort.Slice(r.points, func(i, j int) bool { return r.points[i].hash < r.points[j].hash })
}

func (r *Ring) Name() string       { return fmt.Sprintf("ring-%d-vnodes-%d", len(r.shards), r.vnodes) }
func (r *Ring) PhysicalCount() int { return len(r.shards) }

func (r *Ring) RouteRead(t uint64) int {
	if len(r.points) == 0 {
		return 0
	}
	h := ringPos(Key(t))
	// first point with hash >= h, wrapping around
	i := sort.Search(len(r.points), func(i int) bool { return r.points[i].hash >= h })
	if i == len(r.points) {
		i = 0
	}
	return r.points[i].shard
}

func (r *Ring) RouteWrite(t uint64) []int { return []int{r.RouteRead(t)} }

// ---------------------------------------------------------------- Directory

// Directory is the design Vitess, Figma, Notion and friends converge on.
//
// Two levels of indirection:
//
//	tenant -> logical shard   hash % LogicalCount, fixed for all time
//	logical -> physical       a lookup table you are free to edit
//
// LogicalCount is chosen once, generously (1024 here), and never changes --
// so no tenant is ever rehashed. Scaling out is editing the second map, which
// means you can move 1 logical shard or 512, move a specific noisy neighbour,
// or pin a whale to dedicated hardware. Pins bypass the hash entirely.
//
// Migrating is what makes an online move possible: while a logical shard is
// listed there, reads still go to the old physical node and writes go to
// both. That is the dual-write window, and closing it correctly is the whole
// difficulty of resharding.
type Directory struct {
	LogicalCount int            `json:"logical_count"`
	Placement    []int          `json:"placement"`           // logical -> physical
	Migrating    map[int]int    `json:"migrating,omitempty"` // logical -> target physical
	Pins         map[uint64]int `json:"pins,omitempty"`      // tenant -> physical (whales)
}

func NewDirectory(logicalCount, physicalCount int) *Directory {
	d := &Directory{
		LogicalCount: logicalCount,
		Placement:    make([]int, logicalCount),
		Migrating:    map[int]int{},
		Pins:         map[uint64]int{},
	}
	// Contiguous ranges, not round-robin: a range is one line in a runbook,
	// and moving "logical 512-767" is easier to reason about at 3am.
	per := logicalCount / physicalCount
	for i := range d.Placement {
		p := i / per
		if p >= physicalCount {
			p = physicalCount - 1
		}
		d.Placement[i] = p
	}
	return d
}

func (d *Directory) Name() string { return fmt.Sprintf("directory-%dL", d.LogicalCount) }

func (d *Directory) PhysicalCount() int {
	seen := map[int]bool{}
	for _, p := range d.Placement {
		seen[p] = true
	}
	for _, p := range d.Migrating {
		seen[p] = true
	}
	return len(seen)
}

// Logical is the half of routing that must never change.
func (d *Directory) Logical(tenantID uint64) int {
	return int(Key(tenantID) % uint64(d.LogicalCount))
}

func (d *Directory) RouteRead(tenantID uint64) int {
	if p, ok := d.Pins[tenantID]; ok {
		return p
	}
	return d.Placement[d.Logical(tenantID)]
}

func (d *Directory) RouteWrite(tenantID uint64) []int {
	src := d.RouteRead(tenantID)
	if _, pinned := d.Pins[tenantID]; pinned {
		return []int{src}
	}
	if dst, migrating := d.Migrating[d.Logical(tenantID)]; migrating && dst != src {
		// Dual-write window: the new home must not miss writes that land
		// after the backfill has already copied that row's page.
		return []int{src, dst}
	}
	return []int{src}
}

// BeginMove opens the dual-write window for a logical shard.
func (d *Directory) BeginMove(logical, target int) {
	if d.Migrating == nil {
		d.Migrating = map[int]int{}
	}
	d.Migrating[logical] = target
}

// Commit flips reads to the new home and closes the window. In production
// this write is the cutover: it must be atomic and every router must see it
// before any of them stops dual-writing, which is why the store is usually
// etcd/Consul with a watch rather than a file like this.
func (d *Directory) Commit(logical int) {
	if target, ok := d.Migrating[logical]; ok {
		d.Placement[logical] = target
		delete(d.Migrating, logical)
	}
}

// Pin gives one tenant its own physical shard regardless of hash. The
// hot-tenant escape hatch -- build it on day one, use it on day four hundred.
func (d *Directory) Pin(tenantID uint64, physical int) {
	if d.Pins == nil {
		d.Pins = map[uint64]int{}
	}
	d.Pins[tenantID] = physical
}

func (d *Directory) LogicalsOn(physical int) []int {
	var out []int
	for l, p := range d.Placement {
		if p == physical {
			out = append(out, l)
		}
	}
	return out
}

// ------------------------------------------------- persistence (a stand-in)

// Save/Load use a JSON file so the labs can share topology between the
// shardctl and reshard binaries. Read this as "the metadata store", and note
// what a file cannot give you: atomic multi-router visibility, watches, and
// fencing against a router that cached an old placement. A stale router
// writing to the pre-cutover shard is the classic resharding data-loss bug.
func (d *Directory) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path) // atomic-ish, the file-system version of a CAS
}

func LoadDirectory(path string) (*Directory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Directory
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	if d.Migrating == nil {
		d.Migrating = map[int]int{}
	}
	if d.Pins == nil {
		d.Pins = map[uint64]int{}
	}
	return &d, nil
}

// ---------------------------------------------------------------- analysis

// Churn reports the fraction of tenants that change shard when moving from
// one topology to another. Run it before any reshard; the number tells you
// how much data has to move and therefore how long you are exposed.
func Churn(from, to Topology, sampleTenants int) float64 {
	moved := 0
	for t := 1; t <= sampleTenants; t++ {
		if from.RouteRead(uint64(t)) != to.RouteRead(uint64(t)) {
			moved++
		}
	}
	return float64(moved) / float64(sampleTenants)
}

// Distribution counts how many of the sampled tenants land on each shard.
func Distribution(t Topology, sampleTenants int) map[int]int {
	out := map[int]int{}
	for i := 1; i <= sampleTenants; i++ {
		out[t.RouteRead(uint64(i))]++
	}
	return out
}
