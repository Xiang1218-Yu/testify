package suite

import (
	"flag"
	"fmt"
	"hash/fnv"
)

var (
	shardCount = flag.Int(
		"testify.shardcount",
		0,
		"total number of fixed shards the testify suite is split into for parallel execution; 0 disables sharding",
	)
	shardIndex = flag.Int(
		"testify.shardindex",
		0,
		"1-based index of the shard to run; must be between 1 and the value of -testify.shardcount",
	)
)

// sharding is a validated shard selection. Its zero value means sharding is
// disabled.
type sharding struct {
	total int // total number of shards (> 0 when enabled)
	index int // 0-based index of the current shard
}

// newSharding validates the total shard count and the 1-based current shard
// index passed through the command-line flags. The zero sharding is returned
// (without an error) when both arguments are left at their zero defaults, so
// runs that do not opt into sharding behave exactly as before.
func newSharding(total, oneBasedIndex int) (sharding, error) {
	if total == 0 && oneBasedIndex == 0 {
		return sharding{}, nil
	}
	switch {
	case total < 0:
		return sharding{}, fmt.Errorf("invalid -testify.shardcount %d: must be greater than 0", total)
	case total == 0:
		return sharding{}, fmt.Errorf("-testify.shardindex=%d requires -testify.shardcount to be set", oneBasedIndex)
	case oneBasedIndex < 1:
		return sharding{}, fmt.Errorf("invalid -testify.shardindex %d: must be between 1 and %d", oneBasedIndex, total)
	case oneBasedIndex > total:
		return sharding{}, fmt.Errorf("invalid -testify.shardindex %d: must be between 1 and %d", oneBasedIndex, total)
	}
	return sharding{total: total, index: oneBasedIndex - 1}, nil
}

func (s sharding) enabled() bool {
	return s.total > 0
}

// owner returns the 0-based index of the shard that owns the test with the
// given full name. The assignment is a pure function of fullName, so the same
// suite is partitioned identically on every machine and independently of the
// order in which methods are discovered. Adding, removing or renaming a test
// only changes the ownership of that test.
func (s sharding) owner(fullName string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fullName))
	return int(h.Sum64() % uint64(s.total))
}

// owns reports whether the current shard is responsible for running the test
// with the given full name.
func (s sharding) owns(fullName string) bool {
	return s.owner(fullName) == s.index
}

// skipReason explains why a test is skipped on the current shard and points
// at the single shard that will run it.
func (s sharding) skipReason(fullName string, owner int) string {
	return fmt.Sprintf(
		"testify: shard %d/%d skips %s: it belongs to shard %d/%d",
		s.index+1, s.total, fullName, owner+1, s.total,
	)
}
