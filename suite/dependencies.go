package suite

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// depState is the execution state of a dependency-graph test node.
type depState int

const (
	// stateNotSelected is a test that exists but was excluded by a filter.
	stateNotSelected depState = iota
	// statePending is a selected test that has not run yet.
	statePending
	// statePassed is a test that completed successfully.
	statePassed
	// stateFailed is a test that failed or panicked.
	stateFailed
	// stateSkipped is a test skipped because a prerequisite did not pass.
	stateSkipped
)

// depTest is a test method in the dependency graph.
type depTest struct {
	test
	idx        int
	prereqs    []*depTest
	dependents []*depTest

	done chan struct{}

	selected   bool
	state      depState
	skipReason string
	parallel   bool
}

// parallelReleaser is implemented by the embedded suite.Suite and lets the
// scheduler hand Suite.Parallel its token-release callback.
type parallelReleaser interface {
	setDependencyParallel(release func()) uint64
	clearDependencyParallel(id uint64)
	setDependencyScheduling(active bool)
}

// buildDependencyGraph creates the graph for all discovered test methods,
// validates the declaration (unknown names, duplicate prerequisites,
// cycles) and returns the tests in a stable topological order.
//
// allNames contains every Test* method in reflection order (including
// tests removed by -testify.m), while tests only contains the methods
// selected for this run.
func buildDependencyGraph(suiteName string, allNames []string, tests []test, declaration map[string][]string) ([]*depTest, error) {
	byName := make(map[string]*depTest, len(allNames))
	nodes := make([]*depTest, 0, len(allNames))
	for i, name := range allNames {
		n := &depTest{
			test:     test{name: name},
			idx:      i,
			done:     make(chan struct{}),
			state:    stateNotSelected,
			selected: false,
		}
		byName[name] = n
		nodes = append(nodes, n)
	}
	for _, tc := range tests {
		if n, ok := byName[tc.name]; ok {
			n.test = tc
			n.selected = true
			n.state = statePending
		}
	}

	var problems []string

	// Sort declaration keys so error reports are deterministic regardless
	// of map iteration order.
	declared := make([]string, 0, len(declaration))
	for name := range declaration {
		declared = append(declared, name)
	}
	sort.Strings(declared)

	for _, name := range declared {
		n, ok := byName[name]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"dependencies are declared for test %q, but no such test method exists in suite %q",
				name, suiteName))
			continue
		}
		seen := make(map[string]bool, len(declaration[name]))
		for _, prereq := range declaration[name] {
			if prereq == "" {
				problems = append(problems, fmt.Sprintf(
					"test %q declares an empty prerequisite name", name))
				continue
			}
			if seen[prereq] {
				problems = append(problems, fmt.Sprintf(
					"test %q declares prerequisite %q more than once", name, prereq))
				continue
			}
			seen[prereq] = true
			p, ok := byName[prereq]
			if !ok {
				problems = append(problems, fmt.Sprintf(
					"test %q depends on %q, which is not a test method in suite %q",
					name, prereq, suiteName))
				continue
			}
			n.prereqs = append(n.prereqs, p)
			p.dependents = append(p.dependents, n)
		}
	}

	if cycle := findDependencyCycle(nodes); cycle != nil {
		problems = append(problems, "circular dependency detected: "+strings.Join(cycle, " -> "))
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("testify/suite: invalid test dependencies in suite %q:\n  - %s",
			suiteName, strings.Join(problems, "\n  - "))
	}

	return topologicalOrder(nodes), nil
}

// findDependencyCycle returns the first cycle found as a path whose last
// element repeats the first one (e.g. [TestA, TestB, TestA]), or nil.
func findDependencyCycle(nodes []*depTest) []string {
	const (
		white = 0 // not visited
		gray  = 1 // on the current DFS stack
		black = 2 // fully processed
	)
	color := make(map[string]int, len(nodes))
	var stack []*depTest

	var dfs func(n *depTest) []string
	dfs = func(n *depTest) []string {
		color[n.name] = gray
		stack = append(stack, n)
		for _, p := range n.prereqs {
			switch color[p.name] {
			case white:
				if cycle := dfs(p); cycle != nil {
					return cycle
				}
			case gray:
				// Found a back edge: extract the cycle from p on the stack.
				start := 0
				for i, onStack := range stack {
					if onStack == p {
						start = i
						break
					}
				}
				cycle := []string{p.name}
				for _, onStack := range stack[start+1:] {
					cycle = append(cycle, onStack.name)
				}
				cycle = append(cycle, p.name)
				return cycle
			}
		}
		stack = stack[:len(stack)-1]
		color[n.name] = black
		return nil
	}

	// Iterate in reflection order for stability.
	for _, n := range nodes {
		if color[n.name] == white {
			if cycle := dfs(n); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// topologicalOrder returns a stable order: among tests whose prerequisites
// have all been emitted, the one with the smallest reflection index wins,
// so independent tests keep running in their natural order.
func topologicalOrder(nodes []*depTest) []*depTest {
	indegree := make(map[string]int, len(nodes))
	ready := make([]*depTest, 0, len(nodes))
	for _, n := range nodes {
		indegree[n.name] = len(n.prereqs)
		if len(n.prereqs) == 0 {
			ready = append(ready, n)
		}
	}

	ordered := make([]*depTest, 0, len(nodes))
	for len(ready) > 0 {
		// Linear minimum-index scan; test counts are small.
		next := 0
		for i := 1; i < len(ready); i++ {
			if ready[i].idx < ready[next].idx {
				next = i
			}
		}
		n := ready[next]
		ready = append(ready[:next], ready[next+1:]...)
		ordered = append(ordered, n)

		for _, d := range n.dependents {
			indegree[d.name]--
			if indegree[d.name] == 0 {
				ready = append(ready, d)
			}
		}
	}
	return ordered
}

// runDependencyTests executes the graph. Each test runs in its own
// goroutine blocked on the completion channels of its prerequisites, so
// parallel tests never start before their prerequisites and dependent
// tests always wait for parallel prerequisites to finish.
func runDependencyTests(t *testing.T, s TestingSuite, nodes []*depTest, stats *SuiteInformation) {
	selected := 0
	for _, n := range nodes {
		if n.selected {
			selected++
		} else {
			close(n.done)
		}
	}
	if selected == 0 {
		t.Log("warning: no tests to run")
		return
	}

	// sequential serializes tests that do not call Parallel. A test holds
	// it from the moment it is scheduled until it either finishes or
	// explicitly releases it through Suite.Parallel.
	var sequential sync.Mutex
	pr, _ := s.(parallelReleaser)
	if pr != nil {
		pr.setDependencyScheduling(true)
		defer pr.setDependencyScheduling(false)
	}

	var wg sync.WaitGroup
	// Dispatch follows the stable topological order. Each test starts on
	// its own goroutine, blocked on the sequential token. The dispatch
	// loop then waits until the test either opts into parallelism (so
	// ordering no longer constrains it) or completes as a sequential
	// test. This way sequential tests run in exactly the graph order,
	// while Suite.Parallel tests may overlap and remain gated by the
	// prerequisite channels above.
	for _, n := range nodes {
		if !n.selected {
			continue
		}

		for _, prereq := range n.prereqs {
			<-prereq.done
		}

		if reason := dependencySkipReason(n); reason != "" {
			skipDependencyTest(t, n, reason, stats)
			close(n.done)
			continue
		}

		wentParallel := make(chan struct{})
		wg.Add(1)
		go func(n *depTest, wentParallel chan struct{}) {
			defer wg.Done()

			sequential.Lock()
			var once sync.Once
			release := func() {
				once.Do(func() {
					n.parallel = true
					sequential.Unlock()
					close(wentParallel)
				})
			}
			var parallelID uint64
			if pr != nil {
				parallelID = pr.setDependencyParallel(release)
			}

			// t.Run blocks until the subtest finishes (the subtest must
			// not call (*testing.T).Parallel directly; Suite.Parallel is
			// what releases the sequential token above).
			//
			// The wrapper records the final state itself because a
			// subtest that skips itself looks successful to t.Run.
			passed := t.Run(n.name, func(st *testing.T) {
				defer func() {
					switch {
					case st.Skipped():
						n.state = stateSkipped
					case st.Failed():
						n.state = stateFailed
					default:
						n.state = statePassed
					}
				}()
				n.run(st)
			})

			if pr != nil {
				pr.clearDependencyParallel(parallelID)
			}
			if !n.parallel {
				sequential.Unlock()
			}

			// Defensive fallback in case the deferred state update above
			// did not run (for example a recovered panic bypassing it).
			if n.state == statePending {
				if passed {
					n.state = statePassed
				} else {
					n.state = stateFailed
				}
			}
			close(n.done)
		}(n, wentParallel)

		select {
		case <-wentParallel:
			// The test opted into parallel execution; dispatching can
			// continue while it runs.
		case <-n.done:
			// The test ran sequentially and already finished.
		}
	}
	wg.Wait()
}

// dependencySkipReason returns why n must be skipped, or "" when all its
// prerequisites passed. Prerequisites are inspected in declaration order
// and the first blocking one explains the skip.
func dependencySkipReason(n *depTest) string {
	for _, prereq := range n.prereqs {
		switch prereq.state {
		case stateFailed:
			return fmt.Sprintf("dependency %q failed", prereq.name)
		case stateSkipped:
			if prereq.skipReason != "" {
				return fmt.Sprintf("dependency %q was skipped: %s", prereq.name, prereq.skipReason)
			}
			return fmt.Sprintf("dependency %q was skipped", prereq.name)
		case stateNotSelected:
			return fmt.Sprintf("dependency %q was not selected to run", prereq.name)
		}
	}
	return ""
}

// skipDependencyTest reports the test as a skipped subtest without running
// SetupTest/BeforeTest, as the shared state it depends on does not exist.
func skipDependencyTest(t *testing.T, n *depTest, reason string, stats *SuiteInformation) {
	n.skipReason = reason
	message := fmt.Sprintf("testify/suite: skipping %s: %s", n.name, reason)

	stats.start(n.name)
	t.Run(n.name, func(st *testing.T) {
		st.Skip(message)
	})
	stats.end(n.name, true)

	n.state = stateSkipped
}
