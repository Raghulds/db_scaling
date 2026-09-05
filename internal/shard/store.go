package shard

import (
	"fmt"
	"os"
)

// DefaultTopologyPath is where shardctl and reshard share the placement map.
// In production this is etcd, Consul, ZooKeeper, or a small highly-available
// Postgres -- something with atomic writes and a watch API. A file has neither,
// which is precisely why lab 05 asks you to think about routers holding a
// stale copy.
const DefaultTopologyPath = "tmp/topology.json"

// LoadOrInitDirectory reads the shared directory, creating it on first use.
func LoadOrInitDirectory(path string, logical, physical int) (*Directory, error) {
	d, err := LoadDirectory(path)
	if err == nil {
		return d, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read topology %s: %w", path, err)
	}
	d = NewDirectory(logical, physical)
	if err := d.Save(path); err != nil {
		return nil, err
	}
	return d, nil
}
