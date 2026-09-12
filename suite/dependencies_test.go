package suite

import (
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Stable, dependency-driven execution order
// ---------------------------------------------------------------------------

type dependencyOrderSuite struct {
	Suite
	mu    sync.Mutex
	order []string
}

func (s *dependencyOrderSuite) Dependencies() map[string][]string {
	// Alphabetically TestA_Final would run first; the graph must move it
	// behind TestZ_Prepare while the independent test keeps its position.
	return map[string][]string{
		"TestA_Final": {"TestZ_Prepare"},
	}
}

func (s *dependencyOrderSuite) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, name)
}

func (s *dependencyOrderSuite) TestA_Final()       { s.record("TestA_Final") }
func (s *dependencyOrderSuite) TestM_Independent() { s.record("TestM_Independent") }
func (s *dependencyOrderSuite) TestZ_Prepare()     { s.record("TestZ_Prepare") }

func TestDependencyOrdering(t *testing.T) {
	s := new(dependencyOrderSuite)
	Run(t, s)

	assert.Equal(t,
		[]string{"TestM_Independent", "TestZ_Prepare", "TestA_Final"},
		s.order,
		"tests must run in topological order, not name order")
}

// ---------------------------------------------------------------------------
// Failing/skipping prerequisites skip dependents; cleanup is gated only on
// the test that prepared the resources.
// ---------------------------------------------------------------------------

type dependencyFailureSuite struct {
	Suite
	mu            sync.Mutex
	ran           []string
	setupCount    int
	tearDownCount int
}

func (s *dependencyFailureSuite) Dependencies() map[string][]string {
	return map[string][]string{
		"TestProcess": {"TestPrepare"},
		"TestBranch":  {"TestProcess"},
		"TestCleanup": {"TestPrepare"},
	}
}

func (s *dependencyFailureSuite) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ran = append(s.ran, name)
}

func (s *dependencyFailureSuite) SetupTest() {
	s.mu.Lock()
	s.setupCount++
	s.mu.Unlock()
}

func (s *dependencyFailureSuite) TearDownTest() {
	s.mu.Lock()
	s.tearDownCount++
	s.mu.Unlock()
}

func (s *dependencyFailureSuite) TestPrepare() { s.record("TestPrepare") }
func (s *dependencyFailureSuite) TestProcess() {
	s.record("TestProcess")
	s.T().Error("stage failed on purpose")
}
func (s *dependencyFailureSuite) TestBranch()  { s.record("TestBranch") }
func (s *dependencyFailureSuite) TestCleanup() { s.record("TestCleanup") }

func TestDependencyFailureCascade(t *testing.T) {
	// A fresh suite per F invocation keeps this correct under -count=N,
	// where RunTests invokes F multiple times.
	ok := testing.RunTests(allTestsFilter, []testing.InternalTest{{
		Name: t.Name() + "/DependencyFailureSuite",
		F: func(subT *testing.T) {
			s := new(dependencyFailureSuite)
			Run(subT, s)

			assert.ElementsMatch(subT,
				[]string{"TestPrepare", "TestProcess", "TestCleanup"},
				s.ran,
				"TestBranch must be skipped, but cleanup gated only on TestPrepare must run")
			assert.Contains(subT, s.ran, "TestCleanup",
				"cleanup runs when its own prerequisite passed, even if another stage failed")
			assert.NotContains(subT, s.ran, "TestBranch")

			// Per-test hooks must not run for auto-skipped tests.
			assert.Equal(subT, 3, s.setupCount)
			assert.Equal(subT, 3, s.tearDownCount)
		},
	}})

	assert.False(t, ok, "the suite contains one failing test")
}

// ---------------------------------------------------------------------------
// Skip reasons (including transitive chains) - inspected in a verbose
// subprocess because skip output is only rendered with -test.v.
// ---------------------------------------------------------------------------

type dependencySkipReasonSuite struct {
	Suite
}

func (s *dependencySkipReasonSuite) Dependencies() map[string][]string {
	return map[string][]string{
		"TestChild":       {"TestSkipped"},
		"TestGrandchild":  {"TestChild"},
		"TestFailedChild": {"TestFails"},
		"TestPanicChild":  {"TestPanics"},
	}
}

func (s *dependencySkipReasonSuite) TestSkipped() {
	s.T().Skip("skipped by the test itself")
}
func (s *dependencySkipReasonSuite) TestChild()       {}
func (s *dependencySkipReasonSuite) TestGrandchild()  {}
func (s *dependencySkipReasonSuite) TestFails()       { s.T().Error("failure on purpose") }
func (s *dependencySkipReasonSuite) TestFailedChild() {}
func (s *dependencySkipReasonSuite) TestPanics()      { panic("boom on purpose") }
func (s *dependencySkipReasonSuite) TestPanicChild()  {}

const depSubprocessEnv = "TESTIFY_SUITE_DEP_SUBPROCESS"

// TestDependencySkipReasonsEntry is re-executed in a verbose subprocess.
func TestDependencySkipReasonsEntry(t *testing.T) {
	if os.Getenv(depSubprocessEnv) != "1" {
		t.Skip("subprocess entry point")
	}
	Run(t, new(dependencySkipReasonSuite))
}

func TestDependencySkipReasons(t *testing.T) {
	cmd := exec.Command(os.Args[0],
		"-test.run", "^TestDependencySkipReasonsEntry$",
		"-test.v",
	)
	cmd.Env = append(os.Environ(), depSubprocessEnv+"=1")
	out, err := cmd.CombinedOutput()
	output := string(out)
	t.Log(output)

	require.Error(t, err, "the suite contains failures, so the subprocess exits non-zero")

	assert.Contains(t, output, "--- SKIP: TestDependencySkipReasonsEntry/TestChild")
	assert.Contains(t, output, `testify/suite: skipping TestChild: dependency "TestSkipped" was skipped`)

	assert.Contains(t, output, "--- SKIP: TestDependencySkipReasonsEntry/TestGrandchild")
	assert.Contains(t, output,
		`testify/suite: skipping TestGrandchild: dependency "TestChild" was skipped: dependency "TestSkipped" was skipped`,
		"the upstream skip reason must be preserved transitively")

	assert.Contains(t, output, "--- SKIP: TestDependencySkipReasonsEntry/TestFailedChild")
	assert.Contains(t, output, `testify/suite: skipping TestFailedChild: dependency "TestFails" failed`)

	assert.Contains(t, output, "--- SKIP: TestDependencySkipReasonsEntry/TestPanicChild")
	assert.Contains(t, output, `dependency "TestPanics" failed`)
}

// ---------------------------------------------------------------------------
// Invalid graphs are reported in full before any test or SetupSuite runs.
// ---------------------------------------------------------------------------

type dependencyCycleSuite struct {
	Suite
	setupRan bool
}

func (s *dependencyCycleSuite) Dependencies() map[string][]string {
	return map[string][]string{
		"TestA": {"TestB"},
		"TestB": {"TestA"},
	}
}

func (s *dependencyCycleSuite) SetupSuite() { s.setupRan = true }
func (s *dependencyCycleSuite) TestA()      {}
func (s *dependencyCycleSuite) TestB()      {}

func TestDependencyCycleReported(t *testing.T) {
	s := new(dependencyCycleSuite)
	capture := StdoutCapture{}
	capture.StartCapture()
	ok := testing.RunTests(allTestsFilter, []testing.InternalTest{{
		Name: t.Name() + "/DependencyCycleSuite",
		F: func(subT *testing.T) {
			Run(subT, s)
		},
	}})
	output, err := capture.StopCapture()
	require.NoError(t, err)

	assert.False(t, ok)
	assert.Contains(t, output, "invalid test dependencies")
	assert.Contains(t, output, "circular dependency detected: TestA -> TestB -> TestA")
	assert.False(t, s.setupRan, "SetupSuite must not run when the graph is invalid")
}

type dependencySelfCycleSuite struct{ Suite }

func (s *dependencySelfCycleSuite) Dependencies() map[string][]string {
	return map[string][]string{"TestSolo": {"TestSolo"}}
}
func (s *dependencySelfCycleSuite) TestSolo() {}

func TestDependencySelfCycleReported(t *testing.T) {
	capture := StdoutCapture{}
	capture.StartCapture()
	ok := testing.RunTests(allTestsFilter, []testing.InternalTest{{
		Name: t.Name() + "/DependencySelfCycleSuite",
		F: func(subT *testing.T) {
			Run(subT, new(dependencySelfCycleSuite))
		},
	}})
	output, err := capture.StopCapture()
	require.NoError(t, err)

	assert.False(t, ok)
	assert.Contains(t, output, "circular dependency detected: TestSolo -> TestSolo")
}

type dependencyInvalidDeclSuite struct{ Suite }

func (s *dependencyInvalidDeclSuite) Dependencies() map[string][]string {
	return map[string][]string{
		// Unknown prerequisite declared twice.
		"TestExists": {"TestGhost", "TestGhost"},
		// Dependencies for a name that is not a test method.
		"TestUnknownKey": {"TestExists"},
	}
}

func (s *dependencyInvalidDeclSuite) TestExists() {}

func TestDependencyInvalidDeclarationReported(t *testing.T) {
	capture := StdoutCapture{}
	capture.StartCapture()
	ok := testing.RunTests(allTestsFilter, []testing.InternalTest{{
		Name: t.Name() + "/DependencyInvalidDeclSuite",
		F: func(subT *testing.T) {
			Run(subT, new(dependencyInvalidDeclSuite))
		},
	}})
	output, err := capture.StopCapture()
	require.NoError(t, err)

	assert.False(t, ok)
	assert.Contains(t, output, `depends on "TestGhost", which is not a test method`)
	assert.Contains(t, output, `declares prerequisite "TestGhost" more than once`)
	assert.Contains(t, output, `dependencies are declared for test "TestUnknownKey", but no such test method exists`)
}

// ---------------------------------------------------------------------------
// -testify.m filtering: a selected test is skipped when its prerequisite is
// not selected.
// ---------------------------------------------------------------------------

type dependencyFilterSuite struct {
	Suite
	mu  sync.Mutex
	ran []string
}

func (s *dependencyFilterSuite) Dependencies() map[string][]string {
	return map[string][]string{"TestMain": {"TestPre"}}
}

func (s *dependencyFilterSuite) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ran = append(s.ran, name)
}

func (s *dependencyFilterSuite) TestPre()  { s.record("TestPre") }
func (s *dependencyFilterSuite) TestMain() { s.record("TestMain") }

// TestDependencyFilterEntry is re-executed with -testify.m below.
func TestDependencyFilterEntry(t *testing.T) {
	s := new(dependencyFilterSuite)
	if os.Getenv(depSubprocessEnv) == "1" {
		Run(t, s)
		return
	}
	// In-process sanity check: no filter means both tests run.
	Run(t, s)
	assert.ElementsMatch(t, []string{"TestPre", "TestMain"}, s.ran)
}

func TestDependencyFilteredPrerequisite(t *testing.T) {
	cmd := exec.Command(os.Args[0],
		"-test.run", "^TestDependencyFilterEntry$",
		"-test.v",
		"-testify.m", "^TestMain$",
	)
	cmd.Env = append(os.Environ(), depSubprocessEnv+"=1")
	out, err := cmd.CombinedOutput()
	output := string(out)
	t.Log(output)

	require.NoError(t, err, "a skipped test is not a failure")

	assert.Contains(t, output, "--- SKIP: TestDependencyFilterEntry/TestMain")
	assert.Contains(t, output, `testify/suite: skipping TestMain: dependency "TestPre" was not selected to run`)
	assert.NotContains(t, output, "--- PASS: TestDependencyFilterEntry/TestPre")
}

// ---------------------------------------------------------------------------
// Parallel scheduling must not violate dependencies.
// ---------------------------------------------------------------------------

type dependencyParallelSuite struct {
	Suite

	p1Started chan struct{}
	p2Started chan struct{}

	mu     sync.Mutex
	aEnd   time.Time
	bStart time.Time

	running int32
	maxSeen int32
}

func (s *dependencyParallelSuite) Dependencies() map[string][]string {
	return map[string][]string{"TestB": {"TestA"}}
}

func (s *dependencyParallelSuite) TestP1() {
	s.Parallel()
	close(s.p1Started)
	<-s.p2Started // only finish once P2 is running too
}

func (s *dependencyParallelSuite) TestP2() {
	s.Parallel()
	<-s.p1Started
	close(s.p2Started)
}

func (s *dependencyParallelSuite) TestA() {
	s.Parallel()
	// Small pause makes a scheduling violation (B starting before A ends)
	// visible if the barriers were ever wired incorrectly.
	time.Sleep(10 * time.Millisecond)
	s.mu.Lock()
	s.aEnd = time.Now()
	s.mu.Unlock()
}

func (s *dependencyParallelSuite) TestB() {
	s.Parallel()
	s.mu.Lock()
	s.bStart = time.Now()
	s.mu.Unlock()
}

func (s *dependencyParallelSuite) TestSequential1() {
	cur := atomic.AddInt32(&s.running, 1)
	for {
		max := atomic.LoadInt32(&s.maxSeen)
		if cur <= max || atomic.CompareAndSwapInt32(&s.maxSeen, max, cur) {
			break
		}
	}
	runtime.Gosched()
	atomic.AddInt32(&s.running, -1)
}

func (s *dependencyParallelSuite) TestSequential2() {
	cur := atomic.AddInt32(&s.running, 1)
	for {
		max := atomic.LoadInt32(&s.maxSeen)
		if cur <= max || atomic.CompareAndSwapInt32(&s.maxSeen, max, cur) {
			break
		}
	}
	runtime.Gosched()
	atomic.AddInt32(&s.running, -1)
}

func TestDependencyParallelScheduling(t *testing.T) {
	s := &dependencyParallelSuite{
		p1Started: make(chan struct{}),
		p2Started: make(chan struct{}),
	}
	Run(t, s)

	s.mu.Lock()
	bStart, aEnd := s.bStart, s.aEnd
	s.mu.Unlock()

	assert.False(t, aEnd.IsZero())
	assert.False(t, bStart.IsZero())
	assert.True(t, bStart.After(aEnd),
		"parallel TestB must not start before its prerequisite TestA finished")
	assert.Equal(t, int32(1), s.maxSeen,
		"non-parallel tests must not overlap each other")
}
