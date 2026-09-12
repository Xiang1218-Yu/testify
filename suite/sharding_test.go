package suite

import (
	"flag"
	"fmt"
	"hash/fnv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shardingSuite is a fixed set of seven test methods used to verify stable
// shard assignment.
type shardingSuite struct {
	Suite

	ran []string

	setupSuiteCount    int
	tearDownSuiteCount int
	setupTestCount     int
	before             []string
	after              []string

	stats     *SuiteInformation
	statsName string
}

var shardingMethodNames = []string{
	"TestAlpha",
	"TestBravo",
	"TestCharlie",
	"TestDelta",
	"TestEcho",
	"TestFoxtrot",
	"TestGolf",
}

func (s *shardingSuite) record(name string) {
	s.ran = append(s.ran, name)
}

func (s *shardingSuite) SetupSuite()    { s.setupSuiteCount++ }
func (s *shardingSuite) TearDownSuite() { s.tearDownSuiteCount++ }
func (s *shardingSuite) SetupTest()     { s.setupTestCount++ }

func (s *shardingSuite) BeforeTest(_, testName string) {
	s.before = append(s.before, testName)
}

func (s *shardingSuite) AfterTest(_, testName string) {
	s.after = append(s.after, testName)
}

func (s *shardingSuite) HandleStats(suiteName string, stats *SuiteInformation) {
	s.stats = stats
	s.statsName = suiteName
}

func (s *shardingSuite) TestAlpha()   { s.record("TestAlpha") }
func (s *shardingSuite) TestBravo()   { s.record("TestBravo") }
func (s *shardingSuite) TestCharlie() { s.record("TestCharlie") }
func (s *shardingSuite) TestDelta()   { s.record("TestDelta") }
func (s *shardingSuite) TestEcho()    { s.record("TestEcho") }
func (s *shardingSuite) TestFoxtrot() { s.record("TestFoxtrot") }
func (s *shardingSuite) TestGolf()    { s.record("TestGolf") }

// setShardingFlags overrides the sharding flags for the duration of a test
// and restores their previous values afterwards.
func setShardingFlags(t *testing.T, total, index int) {
	t.Helper()
	prevTotal, prevIndex := *shardCount, *shardIndex
	*shardCount, *shardIndex = total, index
	t.Cleanup(func() {
		*shardCount, *shardIndex = prevTotal, prevIndex
	})
}

// setMatchMethodFlag overrides -testify.m for the duration of a test.
func setMatchMethodFlag(t *testing.T, pattern string) {
	t.Helper()
	prev := *matchMethod
	*matchMethod = pattern
	t.Cleanup(func() { *matchMethod = prev })
}

// expectedShardOwner is an independent FNV-1a implementation of the shard
// assignment; it must not share code with the production implementation so
// the test can detect drift.
func expectedShardOwner(t *testing.T, fullName string, total int) int {
	t.Helper()
	h := fnv.New64a()
	_, err := h.Write([]byte(fullName))
	require.NoError(t, err)
	return int(h.Sum64()%uint64(total)) + 1 // 1-based
}

func TestNewSharding(t *testing.T) {
	t.Run("disabled when both flags are zero", func(t *testing.T) {
		s, err := newSharding(0, 0)
		require.NoError(t, err)
		assert.False(t, s.enabled())
	})

	t.Run("valid configurations", func(t *testing.T) {
		s, err := newSharding(1, 1)
		require.NoError(t, err)
		assert.Equal(t, sharding{total: 1, index: 0}, s)

		s, err = newSharding(4, 3)
		require.NoError(t, err)
		assert.Equal(t, 4, s.total)
		assert.Equal(t, 2, s.index) // converted to 0-based
	})

	invalid := []struct {
		total, index int
	}{
		{-1, 0},  // negative total
		{0, 1},   // index without total
		{3, 0},   // index below range
		{3, -1},  // negative index
		{3, 4},   // index above range
		{-1, -1}, // both invalid
	}
	for _, tc := range invalid {
		tc := tc
		t.Run(fmt.Sprintf("total=%d,index=%d", tc.total, tc.index), func(t *testing.T) {
			_, err := newSharding(tc.total, tc.index)
			require.Error(t, err)
		})
	}
}

func TestShardingPartitionsEveryMethodExactlyOnce(t *testing.T) {
	const total = 4

	expected := make(map[string]int, len(shardingMethodNames))
	for _, name := range shardingMethodNames {
		expected[name] = expectedShardOwner(t, "shardingSuite/"+name, total)
	}

	executedByShard := map[int]map[string]bool{}

	for k := 1; k <= total; k++ {
		t.Run(fmt.Sprintf("shard%d", k), func(t *testing.T) {
			s := new(shardingSuite)
			setShardingFlags(t, total, k)

			Run(t, s)

			// Suite lifecycle runs once per shard even when some (or all)
			// tests are only reported as skipped.
			assert.Equal(t, 1, s.setupSuiteCount, "shard %d", k)
			assert.Equal(t, 1, s.tearDownSuiteCount, "shard %d", k)

			require.NotNil(t, s.stats, "HandleStats must run on shard %d", k)
			assert.Equal(t, "shardingSuite", s.statsName)
			assert.Equal(t, total, s.stats.ShardCount)
			assert.Equal(t, k, s.stats.ShardIndex)
			assert.Len(t, s.stats.TestStats, len(shardingMethodNames),
				"every method must be visible in stats on shard %d", k)
			assert.True(t, s.stats.Passed(), "skipped methods must not count as failures")

			ran := map[string]int{}
			for _, name := range s.ran {
				ran[name]++
			}
			executedByShard[k] = map[string]bool{}
			for name := range ran {
				executedByShard[k][name] = true
			}

			for _, name := range shardingMethodNames {
				owned := expected[name] == k
				info, ok := s.stats.TestStats[name]
				require.True(t, ok, "stats missing %s on shard %d", name, k)

				assert.Equal(t, owned, ran[name] == 1, "%s on shard %d", name, k)
				assert.Equal(t, owned, !info.Skipped, "%s on shard %d", name, k)

				if owned {
					// The full per-test lifecycle only runs for owned methods.
					assert.Contains(t, s.before, name, "BeforeTest on shard %d", k)
					assert.Contains(t, s.after, name, "AfterTest on shard %d", k)
				} else {
					assert.NotContains(t, s.before, name, "BeforeTest on shard %d", k)
					assert.NotContains(t, s.after, name, "AfterTest on shard %d", k)
				}
			}

			assert.Equal(t, len(ran), s.setupTestCount,
				"SetupTest must run exactly once per owned method on shard %d", k)
		})
	}

	// Across all shards the executed sets must be disjoint and cover every
	// method exactly once.
	seen := map[string]int{}
	for k := 1; k <= total; k++ {
		for name := range executedByShard[k] {
			if other, dup := seen[name]; dup {
				t.Fatalf("%s executed by both shard %d and shard %d", name, other, k)
			}
			seen[name] = k
			assert.Equal(t, expected[name], k,
				"%s executed by shard %d but hash assigns it to shard %d",
				name, k, expected[name])
		}
	}
	assert.Len(t, seen, len(shardingMethodNames),
		"the union of all shards must cover every method")
}

func TestShardingOwnershipIsStable(t *testing.T) {
	// Ownership is a pure function of the full test name.
	names := []string{"shardingSuite/TestAlpha", "otherSuite/TestAlpha", "shardingSuite/TestBravo"}

	for _, total := range []int{1, 2, 3, 5, 64} {
		for _, name := range names {
			s := sharding{total: total}
			first := s.owner(name)
			for i := 0; i < 50; i++ {
				assert.Equal(t, first, s.owner(name), "%s with total %d", name, total)
			}
			assert.GreaterOrEqual(t, first, 0)
			assert.Less(t, first, total)
			assert.Equal(t, expectedShardOwner(t, name, total)-1, first,
				"assignment must match the documented FNV-1a hash")

			// Exactly one of the shards owns the name.
			owners := 0
			for k := 0; k < total; k++ {
				if (sharding{total: total, index: k}).owns(name) {
					owners++
				}
			}
			assert.Equal(t, 1, owners, "%s must be owned by exactly one of %d shards", name, total)
		}
	}
}

func TestShardingDisabledRunsEverything(t *testing.T) {
	setShardingFlags(t, 0, 0)

	s := new(shardingSuite)
	Run(t, s)

	assert.Len(t, s.ran, len(shardingMethodNames))
	assert.Equal(t, len(shardingMethodNames), s.setupTestCount)
	require.NotNil(t, s.stats)
	assert.Zero(t, s.stats.ShardCount)
	assert.Zero(t, s.stats.ShardIndex)
	for _, name := range shardingMethodNames {
		info := s.stats.TestStats[name]
		require.NotNil(t, info)
		assert.False(t, info.Skipped, name)
		assert.True(t, info.Passed, name)
	}
	assert.True(t, s.stats.Passed())
}

func TestShardingSingleShardRunsEverything(t *testing.T) {
	setShardingFlags(t, 1, 1)

	s := new(shardingSuite)
	Run(t, s)

	assert.Len(t, s.ran, len(shardingMethodNames))
	require.NotNil(t, s.stats)
	assert.Equal(t, 1, s.stats.ShardCount)
	assert.Equal(t, 1, s.stats.ShardIndex)
	for _, name := range shardingMethodNames {
		assert.False(t, s.stats.TestStats[name].Skipped, name)
	}
}

func TestShardingComposesWithMatchMethod(t *testing.T) {
	setMatchMethodFlag(t, "TestAlpha|TestBravo")

	const total = 3
	executed := map[string]bool{}
	skipped := 0

	for k := 1; k <= total; k++ {
		setShardingFlags(t, total, k)
		s := new(shardingSuite)
		Run(t, s)

		// The regexp removes every other method before sharding; only the
		// two matching methods remain, either owned or skipped.
		assert.Len(t, s.stats.TestStats, 2)
		for _, name := range []string{"TestAlpha", "TestBravo"} {
			info := s.stats.TestStats[name]
			require.NotNil(t, info, name)
			if info.Skipped {
				skipped++
			} else {
				executed[name] = true
			}
		}
	}

	assert.Len(t, executed, 2, "each matching method must run on exactly one shard")
	assert.Equal(t, 2*(total-1), skipped, "the other shards must skip them")
}

func TestShardingReportsSkipReason(t *testing.T) {
	const total = 3
	setShardingFlags(t, total, 1)

	// Skipped-subtest output is only printed in verbose mode, so enable it
	// for this internal test run and restore the previous value afterwards.
	vFlag := flag.Lookup("test.v")
	if vFlag != nil {
		prev := vFlag.Value.String()
		_ = vFlag.Value.Set("true")
		t.Cleanup(func() { _ = vFlag.Value.Set(prev) })
	}

	s := new(shardingSuite)
	internalTest := testing.InternalTest{
		Name: t.Name() + "/shardingSuite",
		F: func(subT *testing.T) {
			Run(subT, s)
		},
	}

	capture := StdoutCapture{}
	capture.StartCapture()
	testing.RunTests(allTestsFilter, []testing.InternalTest{internalTest})
	output, err := capture.StopCapture()
	require.NoError(t, err)

	assert.Contains(t, output, "--- SKIP:")
	for _, name := range shardingMethodNames {
		owner := expectedShardOwner(t, "shardingSuite/"+name, total)
		if owner == 1 {
			continue
		}
		reason := fmt.Sprintf(
			"testify: shard 1/%d skips shardingSuite/%s: it belongs to shard %d/%d",
			total, name, owner, total,
		)
		assert.Contains(t, output, reason)
	}
}
