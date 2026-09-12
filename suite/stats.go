package suite

import "time"

// SuiteInformation stats stores stats for the whole suite execution.
type SuiteInformation struct {
	Start, End time.Time
	TestStats  map[string]*TestInformation

	// ShardIndex and ShardCount describe the fixed shard that ran the
	// suite, using the same 1-based numbering as the -testify.shardindex
	// and -testify.shardcount flags. Both are zero when sharding is
	// disabled.
	ShardIndex int
	ShardCount int
}

// TestInformation stores information about the execution of each test.
type TestInformation struct {
	TestName   string
	Start, End time.Time
	Passed     bool
	// Skipped reports that the test was not executed by this run, for
	// example because it belongs to another shard.
	Skipped bool
}

func newSuiteInformation() *SuiteInformation {
	return &SuiteInformation{
		TestStats: make(map[string]*TestInformation),
	}
}

func (s *SuiteInformation) start(testName string) {
	if s == nil {
		return
	}
	s.TestStats[testName] = &TestInformation{
		TestName: testName,
		Start:    time.Now(),
	}
}

func (s *SuiteInformation) end(testName string, passed bool) {
	if s == nil {
		return
	}
	s.TestStats[testName].End = time.Now()
	s.TestStats[testName].Passed = passed
}

// skip records a test that was filtered out of this run (for example by
// suite sharding) so that HandleStats consumers can see every test of the
// suite, including the ones that were not executed here.
func (s *SuiteInformation) skip(testName string) {
	if s == nil {
		return
	}
	now := time.Now()
	s.TestStats[testName] = &TestInformation{
		TestName: testName,
		Start:    now,
		End:      now,
		Passed:   true,
		Skipped:  true,
	}
}

func (s *SuiteInformation) Passed() bool {
	for _, stats := range s.TestStats {
		if stats.Skipped {
			continue
		}
		if !stats.Passed {
			return false
		}
	}

	return true
}
