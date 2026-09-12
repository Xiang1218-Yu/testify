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

	return oldT.Run(name, func(t *testing.T) {
		suite.SetT(t)
		defer suite.SetT(oldT)

		defer recoverAndFailOnPanic(t)

		if setupSubTest, ok := suite.s.(SetupSubTest); ok {
			setupSubTest.SetupSubTest()
		}

		if tearDownSubTest, ok := suite.s.(TearDownSubTest); ok {
			defer tearDownSubTest.TearDownSubTest()
		}

		subtest()
	})
}

type test = struct {
	name string
	run  func(t *testing.T)

	// skip and skipReason mark a test that belongs to another shard: it is
	// still reported as a skipped subtest instead of being executed.
	skip       bool
	skipReason string
}

// Run takes a testing suite and runs all of the tests attached
// to it.
func Run(t *testing.T, suite TestingSuite) {
	defer recoverAndFailOnPanic(t)

	suite.SetT(t)
	suite.SetS(suite)

	var stats *SuiteInformation
	if _, ok := suite.(WithStats); ok {
		stats = newSuiteInformation()
	}

	var tests []test
	methodFinder := reflect.TypeOf(suite)
	suiteName := methodFinder.Elem().Name()

	var matchMethodRE *regexp.Regexp
	if *matchMethod != "" {
		var err error
		matchMethodRE, err = regexp.Compile(*matchMethod)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testify: invalid regexp for -m: %s\n", err)
			os.Exit(1)
		}
	}

	shards, err := newSharding(*shardCount, *shardIndex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testify: %s\n", err)
		os.Exit(1)
	}
	if stats != nil && shards.enabled() {
		stats.ShardCount = shards.total
		stats.ShardIndex = shards.index + 1
	}

	for i := 0; i < methodFinder.NumMethod(); i++ {
		method := methodFinder.Method(i)

		if !strings.HasPrefix(method.Name, "Test") {
			continue
		}
		// Apply -testify.m filter
		if matchMethodRE != nil && !matchMethodRE.MatchString(method.Name) {
			continue
		}
		// Apply fixed shard assignment, computed from the stable full test
		// name so every machine partitions the suite identically. A test
		// owned by another shard is reported as skipped with a reason that
		// names the shard responsible for running it.
		fullName := suiteName + "/" + method.Name
		if shards.enabled() {
			owner := shards.owner(fullName)
			if owner != shards.index {
				tests = append(tests, test{
					name:       method.Name,
					skip:       true,
					skipReason: shards.skipReason(fullName, owner),
				})
				continue
			}
		}
		// Check method signature
		if method.Type.NumIn() > 1 || method.Type.NumOut() > 0 {
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
		test := test{
			name: method.Name,
			run: func(t *testing.T) {
				parentT := suite.T()
				suite.SetT(t)
				defer recoverAndFailOnPanic(t)
				defer func() {
					t.Helper()

					r := recover()

					stats.end(method.Name, !t.Failed() && r == nil)

					if afterTestSuite, ok := suite.(AfterTest); ok {
						afterTestSuite.AfterTest(suiteName, method.Name)
					}

					if tearDownTestSuite, ok := suite.(TearDownTestSuite); ok {
						tearDownTestSuite.TearDownTest()
					}

					suite.SetT(parentT)
					failOnPanic(t, r)
				}()

				stats.start(method.Name)

				if setupTestSuite, ok := suite.(SetupTestSuite); ok {
					setupTestSuite.SetupTest()
				}
				if beforeTestSuite, ok := suite.(BeforeTest); ok {
					beforeTestSuite.BeforeTest(methodFinder.Elem().Name(), method.Name)
				}

				method.Func.Call([]reflect.Value{reflect.ValueOf(suite)})
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
		setupAllSuite.SetupSuite()
	}

	defer func() {
		if tearDownAllSuite, ok := suite.(TearDownAllSuite); ok {
			tearDownAllSuite.TearDownSuite()
		}

		if suiteWithStats, measureStats := suite.(WithStats); measureStats {
			stats.End = time.Now()
			suiteWithStats.HandleStats(suiteName, stats)
		}
	}()

	runTests(t, tests, stats)
}

func runTests(t *testing.T, tests []test, stats *SuiteInformation) {
	if len(tests) == 0 {
		t.Log("warning: no tests to run")
		return
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			if test.skip {
				// Keep the test visible in stats and to the testing framework,
				// but do not run it: another shard owns it. Setup/teardown and
				// BeforeTest/AfterTest are intentionally not invoked.
				stats.skip(test.name)
				t.Skip(test.skipReason)
				return
			}
			test.run(t)
		})
	}
}
