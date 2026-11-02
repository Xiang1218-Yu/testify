package suite

import (
	"flag"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var matchMethod = flag.String("testify.m", "", "regular expression to select tests of the testify suite to run")

// Suite is a basic testing suite with methods for storing and
// retrieving the current *testing.T context.
type Suite struct {
	*assert.Assertions

	mu      sync.RWMutex
	require *require.Assertions
	t       *testing.T

	// Parent suite to have access to the implemented methods of parent struct
	s TestingSuite

	// observer is the execution observer attached to the enclosing
	// package level run. It is nil unless the suite is run through
	// RunObserved, and it is scoped (saved and restored) across nested
	// runs so subtests of one suite can never report into another.
	observer *executionObserver
}

// currentExecutionObserver implements observerCarrier.
func (suite *Suite) currentExecutionObserver() *executionObserver {
	suite.mu.RLock()
	defer suite.mu.RUnlock()
	return suite.observer
}

// setExecutionObserver implements observerCarrier.
func (suite *Suite) setExecutionObserver(observer *executionObserver) {
	suite.mu.Lock()
	defer suite.mu.Unlock()
	suite.observer = observer
}

// T retrieves the current *testing.T context.
func (suite *Suite) T() *testing.T {
	suite.mu.RLock()
	defer suite.mu.RUnlock()
	return suite.t
}

// SetT sets the current *testing.T context.
func (suite *Suite) SetT(t *testing.T) {
	suite.mu.Lock()
	defer suite.mu.Unlock()
	suite.t = t
	suite.Assertions = assert.New(t)
	suite.require = require.New(t)
}

// SetS needs to set the current test suite as parent
// to get access to the parent methods
func (suite *Suite) SetS(s TestingSuite) {
	suite.s = s
}

// Require returns a require context for suite.
func (suite *Suite) Require() *require.Assertions {
	suite.mu.Lock()
	defer suite.mu.Unlock()
	if suite.require == nil {
		panic("'Require' must not be called before 'Run' or 'SetT'")
	}
	return suite.require
}

// Assert returns an assert context for suite. Normally, you can call:
//
//	suite.NoError(err)
//
// But for situations where the embedded methods are overridden (for example,
// you might want to override assert.Assertions with require.Assertions), this
// method is provided so you can call:
//
//	suite.Assert().NoError(err)
func (suite *Suite) Assert() *assert.Assertions {
	suite.mu.Lock()
	defer suite.mu.Unlock()
	if suite.Assertions == nil {
		panic("'Assert' must not be called before 'Run' or 'SetT'")
	}
	return suite.Assertions
}

func recoverAndFailOnPanic(t *testing.T) {
	t.Helper()
	r := recover()
	failOnPanic(t, r)
}

func failOnPanic(t *testing.T, r interface{}) {
	t.Helper()
	if r != nil {
		t.Errorf("test panicked: %v\n%s", r, debug.Stack())
		t.FailNow()
	}
}

// Run provides suite functionality around golang subtests.  It should be
// called in place of t.Run(name, func(t *testing.T)) in test suite code.
// The passed-in func will be executed as a subtest with a fresh instance of t.
// Provides compatibility with go test pkg -run TestSuite/TestName/SubTestName.
func (suite *Suite) Run(name string, subtest func()) bool {
	oldT := suite.T()
	obs := suite.currentExecutionObserver()

	return oldT.Run(name, func(t *testing.T) {
		suite.SetT(t)
		defer suite.SetT(oldT)

		if obs == nil {
			defer recoverAndFailOnPanic(t)

			if setupSubTest, ok := suite.s.(SetupSubTest); ok {
				setupSubTest.SetupSubTest()
			}

			if tearDownSubTest, ok := suite.s.(TearDownSubTest); ok {
				defer tearDownSubTest.TearDownSubTest()
			}

			subtest()
			return
		}

		// Observed path. Statement and defer order mirrors the
		// unobserved path exactly: a body panic still runs a deferred
		// TearDownSubTest, while a panic in SetupSubTest happens before
		// the TearDownSubTest defer is registered, so it does not run.
		// The observer only records; it never changes the outcome.
		obs.beginSubtest(t)
		defer obs.finishSubtest(t)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("test panicked: %v\n%s", r, debug.Stack())
				t.FailNow()
			}
		}()

		if setupSubTest, ok := suite.s.(SetupSubTest); ok {
			obs.phase(t, PhaseSetupSubTest, setupSubTest.SetupSubTest)
		}

		if tearDownSubTest, ok := suite.s.(TearDownSubTest); ok {
			tearDown := tearDownSubTest
			defer obs.phase(t, PhaseTearDownSubTest, tearDown.TearDownSubTest)
		}

		obs.panicGuard(t, PhaseSubTest, subtest)
	})
}

type test = struct {
	name string
	run  func(t *testing.T)
}

// Run takes a testing suite and runs all of the tests attached
// to it.
func Run(t *testing.T, suite TestingSuite) {
	run(t, suite, nil)
}

// run is the shared implementation of Run and RunObserved. When obs is nil
// the suite runs with observation disabled and behaves exactly like the
// historical Run; when obs is non nil the lifecycle is recorded without
// changing any test outcome.
func run(t *testing.T, suite TestingSuite, obs *executionObserver) {
	defer recoverAndFailOnPanic(t)

	suite.SetT(t)
	suite.SetS(suite)

	// Scope the observer to this run. This also detaches a run that does
	// not opt into observation from an enclosing observed run, so nested
	// suite runs can never double report or pollute each other.
	if c, ok := suite.(observerCarrier); ok {
		previous := c.currentExecutionObserver()
		c.setExecutionObserver(obs)
		defer c.setExecutionObserver(previous)
	}

	var stats *SuiteInformation
	if _, ok := suite.(WithStats); ok {
		stats = newSuiteInformation()
	}

	var tests []test
	methodFinder := reflect.TypeOf(suite)
	suiteName := methodFinder.Elem().Name()

	if obs != nil {
		obs.beginSuite(t, suiteName)
		// Register finish first and the skip check after it, so during
		// unwind the skip event is emitted before the report is sealed.
		// t.Skip (for example in SetupSuite) ends the goroutine through
		// runtime.Goexit, so the check has to live in a defer.
		defer obs.finishSuite(t)
		defer func() {
			if t.Skipped() {
				obs.suiteSkipped(t)
			}
		}()
	}

	var matchMethodRE *regexp.Regexp
	if *matchMethod != "" {
		var err error
		matchMethodRE, err = regexp.Compile(*matchMethod)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testify: invalid regexp for -m: %s\n", err)
			os.Exit(1)
		}
	}

	for i := 0; i < methodFinder.NumMethod(); i++ {
		method := methodFinder.Method(i)

		if !strings.HasPrefix(method.Name, "Test") {
			continue
		}
		// Apply -testify.m filter
		if matchMethodRE != nil && !matchMethodRE.MatchString(method.Name) {
			if obs != nil {
				obs.filtered(method.Name, t.Name(), *matchMethod)
			}
			continue
		}
		// Check method signature
		if method.Type.NumIn() > 1 || method.Type.NumOut() > 0 {
			if obs != nil {
				obs.discovered(method.Name, t.Name(), false)
			}
			tests = append(tests, test{
				name: method.Name,
				run: func(t *testing.T) {
					t.Errorf(
						"testify: suite method %q has invalid signature: expected no input or output parameters, method has %d input parameters and %d output parameters",
						method.Name, method.Type.NumIn()-1, method.Type.NumOut(),
					)
				},
			})
			continue
		}
		if obs != nil {
			obs.discovered(method.Name, t.Name(), true)
		}
		test := test{
			name: method.Name,
			run: func(t *testing.T) {
				parentT := suite.T()
				suite.SetT(t)
				defer recoverAndFailOnPanic(t)
				defer func() {
					t.Helper()

					// A panic from SetupTest, BeforeTest or the test body
					// was already recorded by panicGuard; recover here so
					// AfterTest and TearDownTest still run, exactly like
					// the unobserved implementation.
					r := recover()

					stats.end(method.Name, !t.Failed() && r == nil)

					if afterTestSuite, ok := suite.(AfterTest); ok {
						obs.phase(t, PhaseAfterTest, func() {
							afterTestSuite.AfterTest(suiteName, method.Name)
						})
					}

					if tearDownTestSuite, ok := suite.(TearDownTestSuite); ok {
						obs.phase(t, PhaseTearDownTest, tearDownTestSuite.TearDownTest)
					}

					suite.SetT(parentT)
					failOnPanic(t, r)
				}()

				stats.start(method.Name)

				if setupTestSuite, ok := suite.(SetupTestSuite); ok {
					obs.phase(t, PhaseSetupTest, setupTestSuite.SetupTest)
				}
				if beforeTestSuite, ok := suite.(BeforeTest); ok {
					obs.phase(t, PhaseBeforeTest, func() {
						beforeTestSuite.BeforeTest(methodFinder.Elem().Name(), method.Name)
					})
				}

				obs.panicGuard(t, PhaseTest, func() {
					method.Func.Call([]reflect.Value{reflect.ValueOf(suite)})
				})
			},
		}
		tests = append(tests, test)
	}

	if len(tests) == 0 {
		return
	}

	if stats != nil {
		stats.Start = time.Now()
	}

	if setupAllSuite, ok := suite.(SetupAllSuite); ok {
		obs.phase(t, PhaseSetupSuite, setupAllSuite.SetupSuite)
	}

	defer func() {
		if tearDownAllSuite, ok := suite.(TearDownAllSuite); ok {
			obs.phase(t, PhaseTearDownSuite, tearDownAllSuite.TearDownSuite)
		}

		if suiteWithStats, measureStats := suite.(WithStats); measureStats {
			stats.End = time.Now()
			suiteWithStats.HandleStats(suiteName, stats)
		}
	}()

	runTests(t, tests, obs)
}

func runTests(t *testing.T, tests []test, obs *executionObserver) {
	if len(tests) == 0 {
		t.Log("warning: no tests to run")
		return
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			if obs != nil {
				obs.beginTest(t)
				defer obs.finishTest(t)
			}
			test.run(t)
		})
	}
}
