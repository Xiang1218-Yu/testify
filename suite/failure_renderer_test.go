package suite

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failureRendererSuite exercises structured failure rendering inside suite
// methods, together with suite statistics and require termination semantics.
type failureRendererSuite struct {
	Suite
	mu                sync.Mutex
	records           []*assert.FailureRecord
	stats             *SuiteInformation
	assertReachedEnd  bool
	requireReachedEnd bool
}

func (s *failureRendererSuite) HandleStats(_ string, stats *SuiteInformation) {
	s.stats = stats
}

func (s *failureRendererSuite) jsonContext() *assert.FailureContext {
	return assert.WithFailureRenderer(s.T(), assert.NewJSONFailureRenderer())
}

func (s *failureRendererSuite) store(records []*assert.FailureRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, records...)
}

func (s *failureRendererSuite) TestAssertFailuresAggregateAndContinue() {
	ctx := s.jsonContext()
	assert.Equal(ctx, 1, 2, "suite assert failure")
	assert.True(ctx, false, "second suite failure")

	// assert semantics: the method keeps running after failures.
	s.mu.Lock()
	s.assertReachedEnd = true
	s.mu.Unlock()

	s.store(ctx.Failures())
}

func (s *failureRendererSuite) TestRequireFailureStopsMethod() {
	ctx := s.jsonContext()
	// FailNow goexits the subtest goroutine; the deferred store still runs.
	defer func() { s.store(ctx.Failures()) }()

	require.Equal(ctx, "want", "got", "suite require failure")

	s.mu.Lock()
	s.requireReachedEnd = true
	s.mu.Unlock()
}

func (s *failureRendererSuite) TestPassingMethod() {
	ctx := s.jsonContext()
	assert.Equal(ctx, 1, 1)
	assert.True(ctx, true)
	s.store(ctx.Failures())
}

func TestSuiteFailureRendererIntegration(t *testing.T) {
	s := new(failureRendererSuite)

	suiteSuccess := testing.RunTests(allTestsFilter, []testing.InternalTest{
		{
			Name: t.Name() + "/failureRendererSuite",
			F: func(t *testing.T) {
				Run(t, s)
			},
		},
	})
	require.False(t, suiteSuccess, "suite must report failure because two of its methods fail")

	// Statistics keep marking failed/passed methods correctly.
	require.NotNil(t, s.stats)
	assert.False(t, s.stats.Passed())
	assert.False(t, s.stats.TestStats["TestAssertFailuresAggregateAndContinue"].Passed)
	assert.False(t, s.stats.TestStats["TestRequireFailureStopsMethod"].Passed)
	assert.True(t, s.stats.TestStats["TestPassingMethod"].Passed)

	// assert failures allowed the method to finish; require stopped its method.
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.True(t, s.assertReachedEnd, "assert method must continue after failures")
	assert.False(t, s.requireReachedEnd, "require method must not continue after failure")

	// Two non-fatal records from the assert method, one fatal from require.
	assert.Len(t, s.records, 3)
	assert.Equal(t, "Equal", s.records[0].Assertion())
	assert.False(t, s.records[0].Fatal())
	assert.Contains(t, s.records[0].Location(), "failure_renderer_test.go")
	assert.Equal(t, "True", s.records[1].Assertion())
	assert.False(t, s.records[1].Fatal())
	assert.Equal(t, "Equal", s.records[2].Assertion())
	assert.True(t, s.records[2].Fatal(), "require failure inside a suite must be fatal")
	assert.Equal(t, "want", s.records[2].Expected())
	assert.Equal(t, "got", s.records[2].Actual())
}
