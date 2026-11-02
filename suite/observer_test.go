package suite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runObserved executes RunObserved inside an isolated internal test, the
// same pattern used by the rest of the suite tests, and returns its result.
// It always registers a registry so the report remains retrievable when the
// suite level goroutine is terminated by t.Skip or FailNow.
func runObserved(t *testing.T, suffix string, s TestingSuite, expectFailure bool, opts ...ObserveOption) (*ExecutionReport, *ReportRegistry, error) {
	t.Helper()

	registry := NewReportRegistry()
	allOpts := make([]ObserveOption, 0, len(opts)+1)
	allOpts = append(allOpts, opts...)
	allOpts = append(allOpts, WithReportRegistry(registry))

	var report *ExecutionReport
	var obsErr error
	name := t.Name() + suffix
	ok := testing.RunTests(allTestsFilter, []testing.InternalTest{{
		Name: name,
		F: func(tt *testing.T) {
			report, obsErr = RunObserved(context.Background(), tt, s, allOpts...)
		},
	}})
	if expectFailure {
		assert.False(t, ok, "internal test %q was expected to fail", name)
	} else {
		assert.True(t, ok, "internal test %q was expected to pass", name)
	}
	if report == nil {
		// Suite level Skip/FailNow terminated the F goroutine, so
		// RunObserved could not return its values.
		var found bool
		report, found = registry.ReportForSuite(name)
		require.True(t, found, "sealed report must be retrievable from the registry")
	}
	return report, registry, obsErr
}

func eventKinds(report *ExecutionReport) []EventKind {
	kinds := make([]EventKind, 0, len(report.events))
	for _, e := range report.events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func eventsOfKind(report *ExecutionReport, kind EventKind) []Event {
	var found []Event
	for _, e := range report.Events() {
		if e.Kind == kind {
			found = append(found, e)
		}
	}
	return found
}

func containsKindInOrder(kinds []EventKind, want ...EventKind) bool {
	i := 0
	for _, k := range kinds {
		if i < len(want) && k == want[i] {
			i++
		}
	}
	return i == len(want)
}

// observeFixtureSuite is a full lifecycle suite used to assert the shape of
// the event stream.
type observeFixtureSuite struct {
	Suite

	ran int32

	setupSubTestNames    []string
	tearDownSubTestNames []string
}

func (s *observeFixtureSuite) SetupSuite()            {}
func (s *observeFixtureSuite) TearDownSuite()         {}
func (s *observeFixtureSuite) SetupTest()             {}
func (s *observeFixtureSuite) TearDownTest()          {}
func (s *observeFixtureSuite) BeforeTest(_, _ string) {}
func (s *observeFixtureSuite) AfterTest(_, _ string)  {}
func (s *observeFixtureSuite) SetupSubTest() {
	s.setupSubTestNames = append(s.setupSubTestNames, s.T().Name())
}
func (s *observeFixtureSuite) TearDownSubTest() {
	s.tearDownSubTestNames = append(s.tearDownSubTestNames, s.T().Name())
}

func (s *observeFixtureSuite) TestPass() {
	atomic.AddInt32(&s.ran, 1)
	assert.True(s.T(), true)
}

func (s *observeFixtureSuite) TestFail() {
	atomic.AddInt32(&s.ran, 1)
	s.T().Error("planned failure")
}

func (s *observeFixtureSuite) TestPanic() {
	atomic.AddInt32(&s.ran, 1)
	panic("boom-test")
}

func (s *observeFixtureSuite) TestSkip() {
	atomic.AddInt32(&s.ran, 1)
	s.T().Skip("skipping on purpose")
}

func (s *observeFixtureSuite) TestSub() {
	atomic.AddInt32(&s.ran, 1)
	s.Run("outer", func() {
		s.Run("inner", func() {
			assert.Equal(s.T(), 1, 1)
		})
	})
}

func TestObservedEventStream(t *testing.T) {
	fixture := new(observeFixtureSuite)
	report, _, obsErr := runObserved(t, "/Fixture", fixture, true)
	require.NoError(t, obsErr, "test failures and panics must not be reported as observer errors")

	events := report.Events()
	kinds := eventKinds(report)

	// Stream boundaries.
	require.NotEmpty(t, events)
	assert.Equal(t, EventSuiteStart, events[0].Kind)
	assert.Equal(t, "observeFixtureSuite", events[0].SuiteName)
	assert.Equal(t, t.Name()+"/Fixture", events[0].SuitePath)
	assert.Equal(t, EventSuiteFinish, events[len(events)-1].Kind)

	// Sequences are consecutive execution order numbers.
	for i, e := range events {
		assert.Equal(t, i, e.Sequence, "event sequences must be consecutive")
		assert.False(t, e.Time.IsZero(), "every event must carry a timestamp")
		assert.Equal(t, t.Name()+"/Fixture", e.SuitePath)
	}

	// Stats event immediately precedes suite finish and carries aggregate
	// results.
	statsEvents := eventsOfKind(report, EventStats)
	require.Len(t, statsEvents, 1)
	require.NotNil(t, statsEvents[0].Stats)
	assert.Equal(t, kinds[len(kinds)-2], EventStats)

	stats := statsEvents[0].Stats
	assert.Equal(t, 5, stats.Discovered)
	assert.Equal(t, 0, stats.Filtered)
	assert.Equal(t, 5, stats.TestsRun)
	assert.Equal(t, 2, stats.TestsPassed) // TestPass and TestSub
	assert.Equal(t, 2, stats.TestsFailed) // TestFail and TestPanic
	assert.Equal(t, 1, stats.TestsSkipped)
	assert.Equal(t, 1, stats.TestsPanicked)
	assert.Equal(t, 2, stats.SubtestsPassed) // outer and inner
	assert.Equal(t, 0, stats.SubtestsFailed)
	assert.Equal(t, 0, stats.SubtestsSkipped)
	assert.Equal(t, 0, stats.SubtestsPanicked)
	assert.False(t, stats.Start.IsZero())
	assert.False(t, stats.End.IsZero())

	// Test bodies really executed even though observation was on.
	assert.Equal(t, int32(5), atomic.LoadInt32(&fixture.ran))

	// Discovery: every Test method is discovered exactly once.
	discovered := eventsOfKind(report, EventTestDiscovered)
	assert.Len(t, discovered, 5)
	discoveredNames := map[string]bool{}
	for _, e := range discovered {
		discoveredNames[e.TestName] = true
		assert.Equal(t, t.Name()+"/Fixture/"+e.TestName, e.TestPath)
	}
	for _, name := range []string{"TestPass", "TestFail", "TestPanic", "TestSkip", "TestSub"} {
		assert.True(t, discoveredNames[name], "missing discovery event for %s", name)
	}

	// Lifecycle phases bracket the suite, every test and the subtests.
	phasePairs := map[LifecyclePhase]int{}
	for _, e := range eventsOfKind(report, EventPhaseStart) {
		phasePairs[e.Phase]++
	}
	for _, phase := range []LifecyclePhase{
		PhaseSetupSuite, PhaseTearDownSuite,
		PhaseSetupTest, PhaseBeforeTest, PhaseAfterTest, PhaseTearDownTest,
		PhaseSetupSubTest, PhaseTearDownSubTest,
	} {
		assert.Greater(t, phasePairs[phase], 0, "missing phase_start for %s", phase)
	}
	assert.Equal(t, 5, phasePairs[PhaseSetupTest])
	assert.Equal(t, 2, phasePairs[PhaseSetupSubTest])
	for _, e := range eventsOfKind(report, EventPhaseFinish) {
		assert.NotEmpty(t, e.Phase)
	}

	// Start/finish pairs exist for every test.
	assert.Len(t, eventsOfKind(report, EventTestStart), 5)
	assert.Len(t, eventsOfKind(report, EventTestFinish), 5)

	// Outcomes.
	failed := eventsOfKind(report, EventTestFailed)
	require.Len(t, failed, 2)
	for _, e := range failed {
		assert.Contains(t, []string{"TestFail", "TestPanic"}, e.TestName)
	}
	skipped := eventsOfKind(report, EventTestSkipped)
	require.Len(t, skipped, 1)
	assert.Equal(t, "TestSkip", skipped[0].TestName)
	panicked := eventsOfKind(report, EventTestPanicked)
	require.Len(t, panicked, 1)
	assert.Equal(t, "TestPanic", panicked[0].TestName)
	require.NotNil(t, panicked[0].Panic)
	assert.Equal(t, PhaseTest, panicked[0].Panic.Phase)
	assert.Equal(t, "boom-test", panicked[0].Panic.Value)
	assert.NotEmpty(t, panicked[0].Panic.Stack)

	// Subtests including nesting are reported with full paths.
	subStarts := eventsOfKind(report, EventSubtestStart)
	require.Len(t, subStarts, 2)
	assert.Equal(t, t.Name()+"/Fixture/TestSub/outer", subStarts[0].TestPath)
	assert.Equal(t, t.Name()+"/Fixture/TestSub/outer/inner", subStarts[1].TestPath)
	subFinishes := eventsOfKind(report, EventSubtestFinish)
	require.Len(t, subFinishes, 2)
	assert.Equal(t, EventSubtestFinish, subFinishes[1].Kind)

	// The nested subtest finishes before its parent subtest.
	assert.True(t, containsKindInOrder(kinds,
		EventSubtestStart, EventSubtestStart, EventSubtestFinish, EventSubtestFinish),
		"nested subtest events must be properly ordered, got %v", kinds)

	// Subtest setup/teardown hooks are attributed to full subtest paths.
	assert.Contains(t, fixture.setupSubTestNames, t.Name()+"/Fixture/TestSub/outer")
	assert.Contains(t, fixture.setupSubTestNames, t.Name()+"/Fixture/TestSub/outer/inner")
}

// observeFilterSuite is used with the -testify.m flag.
type observeFilterSuite struct {
	Suite
	ran []string
}

func (s *observeFilterSuite) TestAlpha() { s.ran = append(s.ran, "TestAlpha") }
func (s *observeFilterSuite) TestBeta()  { s.ran = append(s.ran, "TestBeta") }

func TestObservedDiscoveryAndFilter(t *testing.T) {
	previous := *matchMethod
	defer func() { *matchMethod = previous }()
	*matchMethod = "TestAlpha$"

	fixture := new(observeFilterSuite)
	report, _, obsErr := runObserved(t, "/Filter", fixture, false)
	require.NoError(t, obsErr)

	filtered := eventsOfKind(report, EventTestFiltered)
	require.Len(t, filtered, 1)
	assert.Equal(t, "TestBeta", filtered[0].TestName)
	require.NotNil(t, filtered[0].Filter)
	assert.Equal(t, "TestBeta", filtered[0].Filter.Method)
	assert.Equal(t, "TestAlpha$", filtered[0].Filter.Pattern)
	assert.Equal(t, t.Name()+"/Filter/TestBeta", filtered[0].TestPath)

	assert.Equal(t, []string{"TestAlpha"}, fixture.ran, "the filter must still control what actually runs")

	stats := report.Stats()
	assert.Equal(t, 2, stats.Discovered)
	assert.Equal(t, 1, stats.Filtered)
	assert.Equal(t, 1, stats.TestsRun)
	assert.Equal(t, 1, stats.TestsPassed)
}

// observeInvalidSignatureSuite is only used through a custom fixture, since
// invalid signatures are declared on the suite test type itself.
type observeInvalidSignatureSuite struct {
	Suite
}

func (s *observeInvalidSignatureSuite) TestValid() {}
func (s *observeInvalidSignatureSuite) TestInvalidReturns() string {
	return "nope"
}

func TestObservedInvalidSignatureDiscovered(t *testing.T) {
	fixture := new(observeInvalidSignatureSuite)
	report, _, obsErr := runObserved(t, "/InvalidSig", fixture, true)
	require.NoError(t, obsErr)

	var invalid *Event
	for _, e := range report.Events() {
		if e.Kind == EventTestDiscovered && e.TestName == "TestInvalidReturns" {
			ee := e
			invalid = &ee
		}
	}
	require.NotNil(t, invalid)
	assert.Contains(t, invalid.Message, "invalid signature")

	// The invalid method is still scheduled and fails exactly like Run.
	failed := eventsOfKind(report, EventTestFailed)
	assert.Len(t, failed, 1)
	assert.Equal(t, "TestInvalidReturns", failed[0].TestName)
}

// skipSetupSuite skips from SetupSuite.
type observeSkipSetupSuite struct {
	Suite
	toreDown bool
}

func (s *observeSkipSetupSuite) SetupSuite()    { s.T().Skip("skip whole suite") }
func (s *observeSkipSetupSuite) TearDownSuite() { s.toreDown = true }
func (s *observeSkipSetupSuite) TestNothing()   {}

// skipSetupTestSuite skips from SetupTest.
type observeSkipSetupTestSuite struct {
	Suite
	bodyRan bool
}

func (s *observeSkipSetupTestSuite) SetupTest() { s.T().Skip("skip this test") }
func (s *observeSkipSetupTestSuite) TestSomething() {
	s.bodyRan = true
	s.Fail("body must not run")
}

func TestObservedSkips(t *testing.T) {
	suiteSkip := new(observeSkipSetupSuite)
	report, _, obsErr := runObserved(t, "/SkipSetup", suiteSkip, false)
	require.NoError(t, obsErr)
	// TearDownSuite is registered after SetupSuite and therefore does not
	// run when SetupSuite ends the goroutine via t.Skip: this matches the
	// historical Run behavior exactly.
	assert.False(t, suiteSkip.toreDown, "TearDownSuite must not run after a SetupSuite skip, matching Run")

	suiteSkips := eventsOfKind(report, EventSuiteSkipped)
	require.Len(t, suiteSkips, 1)
	assert.Equal(t, EventSuiteSkipped, suiteSkips[0].Kind)
	assert.Equal(t, 0, report.Stats().TestsRun)

	// Ordering: suite skip happens before stats and after setup phase.
	kinds := eventKinds(report)
	assert.True(t, containsKindInOrder(kinds, EventPhaseStart, EventPhaseFinish, EventSuiteSkipped, EventStats, EventSuiteFinish))

	testSkip := new(observeSkipSetupTestSuite)
	report2, _, obsErr2 := runObserved(t, "/SkipSetupTest", testSkip, false)
	require.NoError(t, obsErr2)
	assert.False(t, testSkip.bodyRan)
	require.Len(t, eventsOfKind(report2, EventTestSkipped), 1)
	assert.Equal(t, 1, report2.Stats().TestsSkipped)
	assert.Equal(t, 0, report2.Stats().TestsFailed)
}

// panicPhaseSuite reports a panic in the configured phase.
type panicPhaseSuite struct {
	Suite
	where string

	tearDownSuiteRan bool
	tearDownTestRan  bool
	afterTestRan     bool
}

func (s *panicPhaseSuite) SetupSuite() {
	if s.where == "setup_suite" {
		panic("boom-setup-suite")
	}
}
func (s *panicPhaseSuite) TearDownSuite() { s.tearDownSuiteRan = true }
func (s *panicPhaseSuite) SetupTest() {
	if s.where == "setup_test" {
		panic("boom-setup-test")
	}
}
func (s *panicPhaseSuite) AfterTest(_, _ string) { s.afterTestRan = true }
func (s *panicPhaseSuite) TearDownTest() {
	s.tearDownTestRan = true
	if s.where == "teardown_test" {
		panic("boom-teardown-test")
	}
}
func (s *panicPhaseSuite) TestBody() {
	if s.where == "test" {
		panic("boom-body")
	}
}
func (s *panicPhaseSuite) TestSubtestPanic() {
	if s.where == "subtest" {
		s.Run("panicking", func() { panic("boom-subtest") })
	}
}

func TestObservedPanicPhases(t *testing.T) {
	cases := []struct {
		where        string
		kind         EventKind
		phase        LifecyclePhase
		value        string
		method       string
		tearDown     bool
		subtestPath  bool
		afterTestRan bool
		tearTestRan  bool
	}{
		// SetupSuite panics before runTests and before the TearDownSuite
		// defer is registered, exactly like the historical Run.
		{"setup_suite", EventSuitePanicked, PhaseSetupSuite, "boom-setup-suite", "", false, false, false, false},
		{"setup_test", EventTestPanicked, PhaseSetupTest, "boom-setup-test", "TestBody", true, false, true, true},
		{"test", EventTestPanicked, PhaseTest, "boom-body", "TestBody", true, false, true, true},
		{"teardown_test", EventTestPanicked, PhaseTearDownTest, "boom-teardown-test", "TestBody", true, false, true, true},
		{"subtest", EventSubtestPanicked, PhaseSubTest, "boom-subtest", "TestSubtestPanic", true, true, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.where, func(t *testing.T) {
			fixture := &panicPhaseSuite{where: tc.where}
			suffix := "/Panic_" + tc.where
			report, _, obsErr := runObserved(t, suffix, fixture, true)
			require.NoError(t, obsErr, "panics must never be observer errors")

			var matched *Event
			for _, e := range eventsOfKind(report, tc.kind) {
				if e.Panic != nil && e.Panic.Phase == tc.phase && e.TestName == tc.method {
					ee := e
					matched = &ee
				}
			}
			require.NotNil(t, matched, "missing %s panic event for phase %s and method %q", tc.kind, tc.phase, tc.method)
			assert.Equal(t, tc.value, matched.Panic.Value)
			assert.NotEmpty(t, matched.Panic.Stack)
			if tc.subtestPath {
				assert.True(t, strings.HasSuffix(matched.TestPath, "/TestSubtestPanic/panicking"),
					"got %q", matched.TestPath)
			}

			// Original behavior of the panic is preserved.
			assert.Equal(t, tc.afterTestRan, fixture.afterTestRan, "AfterTest run mismatch")
			assert.Equal(t, tc.tearTestRan, fixture.tearDownTestRan, "TearDownTest run mismatch")
			assert.Equal(t, tc.tearDown, fixture.tearDownSuiteRan, "TearDownSuite run mismatch")
		})
	}
}

// rejectingSink fails when it sees a given sequence number.
type rejectingSink struct {
	rejectAt int
	cause    error
	seen     []Event
}

func (s *rejectingSink) ReceiveEvent(_ context.Context, e Event) error {
	s.seen = append(s.seen, e)
	if e.Sequence == s.rejectAt {
		return s.cause
	}
	return nil
}

type observedErrorProbeSuite struct {
	Suite
	ran int32
}

func (s *observedErrorProbeSuite) TestA() { atomic.AddInt32(&s.ran, 1) }
func (s *observedErrorProbeSuite) TestB() { atomic.AddInt32(&s.ran, 1) }

func TestObservedSinkRejection(t *testing.T) {
	cause := errors.New("sink unavailable")
	sink := &rejectingSink{rejectAt: 2, cause: cause}
	fixture := new(observedErrorProbeSuite)

	report, _, obsErr := runObserved(t, "/Reject", fixture, false, WithEventSink(sink))
	require.Error(t, obsErr)

	var oe *ObserverError
	require.True(t, errors.As(obsErr, &oe))
	assert.Equal(t, OpEventSinkRejected, oe.Op)
	assert.True(t, errors.Is(obsErr, ErrEventSinkRejected))
	assert.Equal(t, 2, oe.Sequence)
	assert.Equal(t, EventTestDiscovered, oe.EventKind)
	assert.Equal(t, t.Name()+"/Reject", oe.SuitePath)
	assert.Equal(t, t.Name()+"/Reject/TestB", oe.TestPath)
	assert.Same(t, cause, oe.Cause)
	assert.Contains(t, oe.Error(), oe.TestPath)
	assert.Contains(t, oe.Error(), "event #2")

	// Observation stopped at the rejection, tests still executed.
	require.Len(t, sink.seen, 3)
	assert.Equal(t, int32(2), atomic.LoadInt32(&fixture.ran), "test execution must continue untouched")

	// The report is still available with everything recorded so far.
	assert.Len(t, report.Events(), 3)
}

func TestObservedCapacityExhausted(t *testing.T) {
	fixture := new(observedErrorProbeSuite)
	report, _, obsErr := runObserved(t, "/Cap", fixture, false, WithEventCapacity(3))
	require.Error(t, obsErr)

	var oe *ObserverError
	require.True(t, errors.As(obsErr, &oe))
	assert.Equal(t, OpEventCapacityExhausted, oe.Op)
	assert.True(t, errors.Is(obsErr, ErrEventCapacityExhausted))
	assert.Equal(t, t.Name()+"/Cap", oe.SuitePath)
	assert.NotEmpty(t, oe.TestPath)
	assert.GreaterOrEqual(t, oe.Sequence, 0)

	assert.Len(t, report.Events(), 3)
	assert.Equal(t, int32(2), atomic.LoadInt32(&fixture.ran), "tests must keep running with observation disabled")
}

func TestObservedContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fixture := new(observedErrorProbeSuite)
	var report *ExecutionReport
	var obsErr error
	ok := testing.RunTests(allTestsFilter, []testing.InternalTest{{
		Name: t.Name() + "/Canceled",
		F: func(tt *testing.T) {
			report, obsErr = RunObserved(ctx, tt, fixture)
		},
	}})
	assert.True(t, ok, "context cancellation must not fail the tests themselves")
	require.Error(t, obsErr)

	var oe *ObserverError
	require.True(t, errors.As(obsErr, &oe))
	assert.Equal(t, OpContextCanceled, oe.Op)
	assert.True(t, errors.Is(obsErr, ErrObserverContextCanceled))
	assert.True(t, errors.Is(obsErr, context.Canceled), "the context error must remain reachable")
	assert.Equal(t, EventSuiteStart, oe.EventKind)
	assert.Equal(t, t.Name()+"/Canceled", oe.SuitePath)
	assert.NotNil(t, report)
	assert.Equal(t, int32(2), atomic.LoadInt32(&fixture.ran), "canceling observation must not cancel test execution")
}

// recordingSink collects every event for direct stream assertions.
type recordingSink struct{ events []Event }

func (s *recordingSink) ReceiveEvent(_ context.Context, e Event) error {
	s.events = append(s.events, e)
	return nil
}

func TestObservedSinkReceivesInOrder(t *testing.T) {
	sink := new(recordingSink)
	fixture := new(observedErrorProbeSuite)
	report, _, obsErr := runObserved(t, "/Sink", fixture, false, WithEventSink(sink))
	require.NoError(t, obsErr)

	assert.Equal(t, eventKinds(report), kindsOf(sink.events), "sink must observe the same ordered stream")
}

func kindsOf(events []Event) []EventKind {
	kinds := make([]EventKind, len(events))
	for i, e := range events {
		kinds[i] = e.Kind
	}
	return kinds
}

func TestObservedReportQueryAndExport(t *testing.T) {
	fixture := new(observeFixtureSuite)
	report, registry, obsErr := runObserved(t, "/Query", fixture, true)
	require.NoError(t, obsErr)

	suitePath := t.Name() + "/Query"

	// Registry lookup by suite path.
	fromRegistry, ok := registry.ReportForSuite(suitePath)
	require.True(t, ok)
	assert.Same(t, report, fromRegistry)
	assert.Equal(t, []string{suitePath}, registry.SuitePaths())

	// Direct sequence lookup.
	first, ok := report.Event(0)
	require.True(t, ok)
	assert.Equal(t, EventSuiteStart, first.Kind)
	if _, ok := report.Event(99999); ok {
		t.Fatal("unexpected event for unknown sequence")
	}

	// Per test path query.
	passEvents := report.EventsForTest(suitePath + "/TestPass")
	require.NotEmpty(t, passEvents)
	for _, e := range passEvents {
		assert.Equal(t, "TestPass", e.TestName)
	}
	assert.Nil(t, report.EventsForTest("does/not/exist"))

	innerPath := suitePath + "/TestSub/outer/inner"
	innerEvents := report.EventsForTest(innerPath)
	var sawInnerFinish bool
	for _, e := range innerEvents {
		assert.Equal(t, innerPath, e.TestPath)
		if e.Kind == EventSubtestFinish {
			sawInnerFinish = true
		}
	}
	assert.True(t, sawInnerFinish)

	paths := report.TestPaths()
	assert.Contains(t, paths, suitePath+"/TestPass")
	assert.Contains(t, paths, innerPath)
	assert.NotContains(t, paths, "")

	// JSON round trip.
	var buf bytes.Buffer
	require.NoError(t, report.WriteJSON(&buf))
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, suitePath, decoded["suite_path"])
	assert.Equal(t, "observeFixtureSuite", decoded["suite_name"])
	rawEvents, ok := decoded["events"].([]interface{})
	require.True(t, ok)
	assert.Len(t, rawEvents, len(report.Events()))
	stats := decoded["stats"].(map[string]interface{})
	assert.Equal(t, float64(5), stats["discovered"])

	// Registry export contains a reports array.
	var regBuf bytes.Buffer
	require.NoError(t, registry.WriteJSON(&regBuf))
	var regDecoded struct {
		Reports []json.RawMessage `json:"reports"`
	}
	require.NoError(t, json.Unmarshal(regBuf.Bytes(), &regDecoded))
	assert.Len(t, regDecoded.Reports, 1)
}

// failingWriter fails on the first chunk carrying a non empty test_path,
// simulating a broken export destination.
type failingWriter struct {
	failed bool
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if !w.failed && bytes.Contains(p, []byte(`"test_path":"`)) && !bytes.Contains(p, []byte(`"test_path":""`)) {
		w.failed = true
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

func TestObservedExportFailureCarriesLocation(t *testing.T) {
	fixture := new(observeFixtureSuite)
	report, _, obsErr := runObserved(t, "/ExportFail", fixture, true)
	require.NoError(t, obsErr)

	writer := new(failingWriter)
	err := report.WriteJSON(writer)
	require.Error(t, err)
	assert.True(t, errors.Is(err, io.ErrShortWrite))

	var oe *ObserverError
	require.True(t, errors.As(err, &oe))
	assert.Equal(t, OpReportExport, oe.Op)
	assert.True(t, errors.Is(err, ErrReportExport))
	assert.GreaterOrEqual(t, oe.Sequence, 0)
	assert.Equal(t, t.Name()+"/ExportFail", oe.SuitePath)
	assert.NotEmpty(t, oe.TestPath, "export error must pinpoint the test path")
	assert.Contains(t, oe.Error(), oe.TestPath)
	assert.Same(t, io.ErrShortWrite, oe.Cause)
}

func TestObservedReportIsImmutable(t *testing.T) {
	fixture := new(observeFixtureSuite)
	report, _, obsErr := runObserved(t, "/Immutable", fixture, true)
	require.NoError(t, obsErr)

	events := report.Events()
	events[0].Kind = EventSuiteFinish
	again := report.Events()
	assert.Equal(t, EventSuiteStart, again[0].Kind)

	// Mutating nested pointer fields of a returned event must not change
	// the sealed stream.
	statsEvent := eventsOfKind(report, EventStats)[0]
	require.NotNil(t, statsEvent.Stats)
	statsEvent.Stats.Discovered = 999
	assert.Equal(t, 5, eventsOfKind(report, EventStats)[0].Stats.Discovered)
}

// nestedOuterSuite embeds a plain Run and an observed Run of other suites
// inside a test method to prove runs cannot report into each other.
type nestedOuterSuite struct {
	Suite
	innerObserved *ExecutionReport
	innerErr      error
}

type nestedInnerSuite struct {
	Suite
	ran int32
}

func (s *nestedInnerSuite) TestInner() {
	atomic.AddInt32(&s.ran, 1)
	s.Run("innerSub", func() {})
}

func (s *nestedOuterSuite) TestOuter() {
	// A plain nested suite run must not leak into the outer report.
	plain := new(nestedInnerSuite)
	Run(s.T(), plain)
	assert.Equal(s.T(), int32(1), atomic.LoadInt32(&plain.ran))

	// An observed nested run gets its own independent report.
	observed := new(nestedInnerSuite)
	s.innerObserved, s.innerErr = RunObserved(context.Background(), s.T(), observed)
	assert.NoError(s.T(), s.innerErr)
	assert.Equal(s.T(), int32(1), atomic.LoadInt32(&observed.ran))

	// The outer suite's own subtest is reported to the outer report.
	s.Run("outerSub", func() {})
}

func TestObservedNestedRunsAreIsolated(t *testing.T) {
	fixture := new(nestedOuterSuite)
	report, _, obsErr := runObserved(t, "/Outer", fixture, false)
	require.NoError(t, obsErr)
	require.NoError(t, fixture.innerErr)
	require.NotNil(t, fixture.innerObserved)

	// Outer report only knows outer test paths.
	for _, e := range report.Events() {
		assert.NotContains(t, e.TestPath, "/TestInner", "inner suite events must not leak: %v", e)
	}
	outerPaths := report.TestPaths()
	assert.Contains(t, outerPaths, t.Name()+"/Outer/TestOuter/outerSub")

	// The nested observed report is complete and independent.
	innerPath := t.Name() + "/Outer/TestOuter"
	assert.Equal(t, innerPath, fixture.innerObserved.SuitePath())
	assert.NotEqual(t, report.SuitePath(), fixture.innerObserved.SuitePath())
	innerDiscovered := eventsOfKind(fixture.innerObserved, EventTestDiscovered)
	require.Len(t, innerDiscovered, 1)
	assert.Equal(t, "TestInner", innerDiscovered[0].TestName)
	assert.NotEmpty(t, eventsOfKind(fixture.innerObserved, EventSubtestStart))

	// Nested plain run restored the carrier state.
	assert.Nil(t, fixture.currentExecutionObserver(), "observer must be detached after the run")
}

func TestObservedDefaultOff(t *testing.T) {
	fixture := new(observedErrorProbeSuite)
	Run(t, fixture)
	assert.Nil(t, fixture.currentExecutionObserver(), "plain Run must not attach an observer")
	assert.Equal(t, int32(2), atomic.LoadInt32(&fixture.ran))
}

func TestObserverErrorMessage(t *testing.T) {
	err := &ObserverError{
		Op:        OpEventSinkRejected,
		Sequence:  7,
		EventKind: EventTestFailed,
		SuitePath: "TestX/S",
		TestPath:  "TestX/S/TestY",
		Phase:     PhaseTest,
		Cause:     fmt.Errorf("down"),
	}
	msg := err.Error()
	assert.Contains(t, msg, "event_sink_rejected")
	assert.Contains(t, msg, "event #7")
	assert.Contains(t, msg, "TestX/S/TestY")
	assert.Contains(t, msg, "down")
}
