package mock

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkpointTestingT is a TestingT that records reported failures instead of
// failing the real test.
type checkpointTestingT struct {
	failed   bool
	messages []string
}

func (*checkpointTestingT) Helper() {}

func (c *checkpointTestingT) Logf(format string, args ...interface{}) {
	c.messages = append(c.messages, fmt.Sprintf(format, args...))
}

func (c *checkpointTestingT) Errorf(format string, args ...interface{}) {
	c.failed = true
	c.messages = append(c.messages, fmt.Sprintf(format, args...))
}

func (*checkpointTestingT) FailNow() {
	panic("checkpointTestingT: FailNow called")
}

func TestCheckpointBasicIncrement(t *testing.T) {
	t.Parallel()
	i := &TestExampleImplementation{}
	i.On("TheExampleMethod", 1, 2, 3).Return(0, nil)
	i.On("TheExampleMethod", 4, 5, 6).Return(0, nil)

	empty := i.Checkpoint()
	assert.Equal(t, 0, empty.Seq())
	assert.Nil(t, empty.CallsSince())

	i.TheExampleMethod(1, 2, 3)
	cp := i.Checkpoint()
	assert.Equal(t, 1, cp.Seq())
	assert.Nil(t, cp.CallsSince())
	assert.Equal(t, 0, cp.NumberOfCallsSince("TheExampleMethod"))

	i.TheExampleMethod(4, 5, 6)
	increment := cp.CallsSince()
	require.Len(t, increment, 1)
	assert.Equal(t, 2, increment[0].Seq)
	assert.Equal(t, "TheExampleMethod", increment[0].Method)
	assert.Equal(t, 4, increment[0].Arguments.Int(0))
	assert.Equal(t, 5, increment[0].Arguments.Int(1))
	assert.Equal(t, 6, increment[0].Arguments.Int(2))
	assert.Equal(t, 1, cp.NumberOfCallsSince("TheExampleMethod"))
	assert.Equal(t, 0, cp.NumberOfCallsSince("UnknownMethod"))

	// a checkpoint taken before any call now sees the whole history
	require.Len(t, empty.CallsSince(), 2)
}

func TestCheckpointNested(t *testing.T) {
	t.Parallel()
	i := &TestExampleImplementation{}
	i.On("TheExampleMethod", Anything, Anything, Anything).Return(0, nil)

	i.TheExampleMethod(1, 1, 1)
	outer := i.Checkpoint()

	i.TheExampleMethod(2, 2, 2)
	inner := i.Checkpoint()

	i.TheExampleMethod(3, 3, 3)

	outerCalls := outer.CallsSince()
	innerCalls := inner.CallsSince()
	require.Len(t, outerCalls, 2)
	require.Len(t, innerCalls, 1)
	assert.Equal(t, []int{2, 3}, []int{outerCalls[0].Seq, outerCalls[1].Seq})
	assert.Equal(t, 3, innerCalls[0].Seq)
	assert.Equal(t, 3, innerCalls[0].Arguments.Int(0))

	// after more calls arrive the old sequence numbers do not shift
	i.TheExampleMethod(4, 4, 4)
	innerAgain := inner.CallsSince()
	require.Len(t, innerAgain, 2)
	assert.Equal(t, 3, innerAgain[0].Seq)
	require.Len(t, outer.CallsSince(), 3)
	assert.Equal(t, 4, outer.CallsSince()[2].Seq)
}

func TestCheckpointMutableArgumentSnapshot(t *testing.T) {
	t.Parallel()
	i := &TestExampleImplementation{}
	i.On("TheExampleMethod3", Anything).Return(nil)
	i.On("TheExampleMethod6", Anything).Return(nil)
	i.On("TheExampleMethod7", Anything).Return(nil)
	i.On("TheExampleMethodVariadic", Anything).Return(nil)

	// pointer argument
	et := &ExampleType{ran: false}
	cp1 := i.Checkpoint()
	i.TheExampleMethod3(et)
	et.ran = true
	assert.False(t, cp1.CallsSince()[0].Arguments.Get(0).(*ExampleType).ran)

	// map argument
	argMap := map[string]bool{"a": true}
	cp2 := i.Checkpoint()
	i.TheExampleMethod6(argMap)
	argMap["a"] = false
	argMap["b"] = true
	assert.Equal(t, map[string]bool{"a": true}, cp2.CallsSince()[0].Arguments.Get(0).(map[string]bool))

	// slice argument
	argSlice := []bool{true, false}
	cp3 := i.Checkpoint()
	i.TheExampleMethod7(argSlice)
	argSlice[0] = false
	argSlice = append(argSlice, true)
	assert.Equal(t, []bool{true, false}, cp3.CallsSince()[0].Arguments.Get(0).([]bool))

	// variadic argument arrives as []int in a single interface slot
	cp4 := i.Checkpoint()
	i.TheExampleMethodVariadic(1, 2, 3)
	assert.Equal(t, []int{1, 2, 3}, cp4.CallsSince()[0].Arguments.Get(0).([]int))

	// mutation performed by a Run handler happens after the snapshot was taken
	j := &TestExampleImplementation{}
	et2 := &ExampleType{ran: false}
	j.On("TheExampleMethod3", Anything).Return(nil).Run(func(args Arguments) {
		args.Get(0).(*ExampleType).ran = true
	})
	cp5 := j.Checkpoint()
	j.TheExampleMethod3(et2)
	assert.False(t, cp5.CallsSince()[0].Arguments.Get(0).(*ExampleType).ran)
	// the existing total call record still holds the live, now mutated object
	assert.True(t, j.Calls[0].Arguments.Get(0).(*ExampleType).ran)
}

func TestCheckpointReadsAreIndependent(t *testing.T) {
	t.Parallel()
	i := &TestExampleImplementation{}
	i.On("TheExampleMethod3", Anything).Return(nil)
	et := &ExampleType{ran: false}
	cp := i.Checkpoint()
	i.TheExampleMethod3(et)

	first := cp.CallsSince()
	// mutate everything the caller received
	first[0].Method = "Mutated"
	first[0].Seq = 999
	first[0].Arguments.Get(0).(*ExampleType).ran = true

	second := cp.CallsSince()
	require.Len(t, second, 1)
	assert.Equal(t, "TheExampleMethod3", second[0].Method)
	assert.Equal(t, cp.Seq()+1, second[0].Seq)
	assert.False(t, second[0].Arguments.Get(0).(*ExampleType).ran)

	// two independent reads are deeply equal
	assert.Equal(t, second, cp.CallsSince())
}

func TestCheckpointExpectationsOverwrittenOrDeleted(t *testing.T) {
	t.Parallel()
	i := &TestExampleImplementation{}
	i.On("TheExampleMethod", 1, 1, 1).Return(0, nil)
	i.On("TheExampleMethod", 2, 2, 2).Return(0, nil)

	i.TheExampleMethod(1, 1, 1)
	cp := i.Checkpoint()

	// add a new expectation, changing the expectation set
	i.On("TheExampleMethod", 3, 3, 3).Return(0, nil).Maybe()
	i.TheExampleMethod(2, 2, 2)

	// delete the expectation backing the observed increment and the new one
	i.On("TheExampleMethod", 2, 2, 2).Unset()
	i.On("TheExampleMethod", 3, 3, 3).Unset()

	increment := cp.CallsSince()
	require.Len(t, increment, 1)
	assert.Equal(t, 2, increment[0].Seq)
	assert.Equal(t, 2, increment[0].Arguments.Int(0))

	// history appended afterwards: sequence stays dense, old range stable
	i.TheExampleMethod(1, 1, 1)
	increment = cp.CallsSince()
	require.Len(t, increment, 2)
	assert.Equal(t, 2, increment[0].Seq)
	assert.Equal(t, 3, increment[1].Seq)
	assert.Equal(t, 1, increment[1].Arguments.Int(0))

	// existing total records are untouched
	require.Len(t, i.Calls, 3)
}

func TestCheckpointConcurrent(t *testing.T) {
	t.Parallel()
	i := &TestExampleImplementation{}
	i.On("TheExampleMethod", Anything, Anything, Anything).Return(0, nil)

	cp := i.Checkpoint()

	const goroutines = 16
	const perGoroutine = 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for k := 0; k < perGoroutine; k++ {
				i.TheExampleMethod(base, k, k+1)
			}
		}(g)
	}
	wg.Wait()

	total := goroutines * perGoroutine
	increment := cp.CallsSince()
	require.Len(t, increment, total)
	require.Len(t, i.Calls, total)

	seen := make(map[int]bool, total)
	prev := cp.Seq()
	for _, rec := range increment {
		assert.Equal(t, "TheExampleMethod", rec.Method)
		assert.True(t, rec.Seq > prev, "sequence numbers must be strictly increasing in the observed order")
		assert.False(t, seen[rec.Seq], "sequence numbers must be unique")
		seen[rec.Seq] = true
		prev = rec.Seq
	}
	assert.Equal(t, cp.Seq()+1, increment[0].Seq, "range start must be stable")
	assert.Equal(t, cp.Seq()+total, increment[total-1].Seq, "range end must be stable and dense")
}

func TestCheckpointConcurrentCheckpointCreation(t *testing.T) {
	t.Parallel()
	i := &TestExampleImplementation{}
	i.On("TheExampleMethod", Anything, Anything, Anything).Return(0, nil)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for k := 0; k < 100; k++ {
				i.TheExampleMethod(base, k, k)
				cp := i.Checkpoint()
				for _, rec := range cp.CallsSince() {
					if rec.Seq <= cp.Seq() {
						t.Errorf("record seq %d must be greater than checkpoint seq %d", rec.Seq, cp.Seq())
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestCheckpointDoesNotAffectExpectations(t *testing.T) {
	t.Parallel()
	i := &TestExampleImplementation{}
	onceCall := i.On("TheExampleMethod", 1, 2, 3).Return(0, nil).Once()
	twiceCall := i.On("TheExampleMethod", 4, 5, 6).Return(0, nil).Twice()

	// taking checkpoints must not touch repeatability counters
	for k := 0; k < 5; k++ {
		_ = i.Checkpoint()
	}
	assert.Equal(t, 1, onceCall.Repeatability)
	assert.Equal(t, 2, twiceCall.Repeatability)

	cp := i.Checkpoint()
	i.TheExampleMethod(1, 2, 3)
	i.TheExampleMethod(4, 5, 6)

	assert.Equal(t, -1, onceCall.Repeatability, "Once semantics unchanged")
	assert.Equal(t, 1, twiceCall.Repeatability, "Twice semantics unchanged")
	assert.Equal(t, 1, onceCall.totalCalls)
	assert.Equal(t, 1, twiceCall.totalCalls)

	i.TheExampleMethod(4, 5, 6)
	assert.Equal(t, -1, twiceCall.Repeatability)
	assert.Equal(t, 2, twiceCall.totalCalls)

	// over-calling the Once expectation still fails as before
	assert.Panics(t, func() { i.TheExampleMethod(1, 2, 3) })

	// checkpoint reads and total records contain only successful calls
	require.Len(t, cp.CallsSince(), 3)
	require.Len(t, i.Calls, 3)
	assert.True(t, i.AssertExpectations(t))
}

func TestCheckpointAssertExpectationsResultUnchanged(t *testing.T) {
	t.Parallel()
	// satisfied expectations
	i := &TestExampleImplementation{}
	i.On("TheExampleMethod", 1, 2, 3).Return(0, nil)
	cp1 := i.Checkpoint()
	i.TheExampleMethod(1, 2, 3)
	cp2 := i.Checkpoint()
	assert.True(t, i.AssertExpectations(t))
	require.Len(t, cp1.CallsSince(), 1)
	assert.Nil(t, cp2.CallsSince())

	// unmet expectations: AssertExpectations still fails, checkpoint sees nothing
	j := &TestExampleImplementation{}
	j.On("TheExampleMethod", 9, 9, 9).Return(0, nil)
	cp := j.Checkpoint()
	tt := &checkpointTestingT{}
	assert.False(t, j.AssertExpectations(tt))
	assert.True(t, tt.failed)
	assert.Nil(t, cp.CallsSince())
}

func TestCheckpointAssertions(t *testing.T) {
	t.Parallel()
	i := &TestExampleImplementation{}
	i.On("TheExampleMethod", Anything, Anything, Anything).Return(0, nil)
	i.On("TheExampleMethod3", Anything).Return(nil)

	cp := i.Checkpoint()
	i.TheExampleMethod(1, 2, 3)
	i.TheExampleMethod(4, 5, 6)
	et := &ExampleType{ran: true}
	i.TheExampleMethod3(et)

	// AssertCalledSince
	assert.True(t, cp.AssertCalledSince(t, "TheExampleMethod", 4, 5, 6))
	assert.True(t, cp.AssertCalledSince(t, "TheExampleMethod", Anything, 2, Anything))
	assert.True(t, cp.AssertCalledSince(t, "TheExampleMethod", MatchedBy(func(v int) bool { return v > 3 }), 5, 6))
	tt := &checkpointTestingT{}
	assert.False(t, cp.AssertCalledSince(tt, "TheExampleMethod", 7, 8, 9))
	assert.True(t, tt.failed)
	tt = &checkpointTestingT{}
	assert.False(t, cp.AssertCalledSince(tt, "Unknown", 1, 2, 3))
	assert.True(t, tt.failed)

	// AssertNotCalledSince
	assert.True(t, cp.AssertNotCalledSince(t, "TheExampleMethod", 7, 8, 9))
	tt = &checkpointTestingT{}
	assert.False(t, cp.AssertNotCalledSince(tt, "TheExampleMethod", 1, 2, 3))
	assert.True(t, tt.failed)

	// AssertNumberOfCallsSince
	assert.True(t, cp.AssertNumberOfCallsSince(t, "TheExampleMethod", 2))
	assert.True(t, cp.AssertNumberOfCallsSince(t, "TheExampleMethod3", 1))
	tt = &checkpointTestingT{}
	assert.False(t, cp.AssertNumberOfCallsSince(tt, "TheExampleMethod", 3))
	assert.True(t, tt.failed)

	// AssertCallsSince: exact, ordered, complete
	assert.True(t, cp.AssertCallsSince(t,
		Invocation("TheExampleMethod", 1, 2, 3),
		Invocation("TheExampleMethod", 4, 5, 6),
		Invocation("TheExampleMethod3", et),
	))
	// argument matchers are supported
	assert.True(t, cp.AssertCallsSince(t,
		Invocation("TheExampleMethod", Anything, 2, 3),
		Invocation("TheExampleMethod", 4, MatchedBy(func(v int) bool { return v == 5 }), 6),
		Invocation("TheExampleMethod3", AnythingOfType("*mock.ExampleType")),
	))

	// wrong call count
	tt = &checkpointTestingT{}
	assert.False(t, cp.AssertCallsSince(tt,
		Invocation("TheExampleMethod", 1, 2, 3),
	))
	assert.True(t, tt.failed)
	assert.NotEmpty(t, tt.messages)

	// wrong method
	tt = &checkpointTestingT{}
	assert.False(t, cp.AssertCallsSince(tt,
		Invocation("WrongMethod", 1, 2, 3),
		Invocation("TheExampleMethod", 4, 5, 6),
		Invocation("TheExampleMethod3", et),
	))
	assert.True(t, tt.failed)

	// wrong arguments
	tt = &checkpointTestingT{}
	assert.False(t, cp.AssertCallsSince(tt,
		Invocation("TheExampleMethod", 1, 2, 3),
		Invocation("TheExampleMethod", 4, 0, 6),
		Invocation("TheExampleMethod3", et),
	))
	assert.True(t, tt.failed)

	// wrong order
	tt = &checkpointTestingT{}
	assert.False(t, cp.AssertCallsSince(tt,
		Invocation("TheExampleMethod", 4, 5, 6),
		Invocation("TheExampleMethod", 1, 2, 3),
		Invocation("TheExampleMethod3", et),
	))
	assert.True(t, tt.failed)

	// after failed assertions the increment can be read again, unchanged
	reread := cp.CallsSince()
	require.Len(t, reread, 3)
	assert.Equal(t, []int{1, 2, 3}, []int{reread[0].Seq, reread[1].Seq, reread[2].Seq})
	assert.Equal(t, "TheExampleMethod3", reread[2].Method)
}

type checkpointCycleNode struct {
	V    int
	Next *checkpointCycleNode
}

func TestCheckpointCyclicValues(t *testing.T) {
	t.Parallel()

	// cyclic pointers
	n1 := &checkpointCycleNode{V: 1}
	n2 := &checkpointCycleNode{V: 2}
	n1.Next = n2
	n2.Next = n1

	m := &Mock{}
	m.On("M", Anything).Return()
	cp := m.Checkpoint()
	m.MethodCalled("M", n1)

	got := cp.CallsSince()[0].Arguments.Get(0).(*checkpointCycleNode)
	assert.NotSame(t, n1, got)
	assert.Equal(t, 1, got.V)
	assert.Equal(t, 2, got.Next.V)
	assert.Same(t, got, got.Next.Next, "the cycle must be preserved in the copy")

	n1.V = 99
	assert.Equal(t, 1, got.V, "the snapshot must be independent of the source")

	// cyclic maps are snapshot-safe at the checkpoint layer. (Passing a cyclic
	// map through mock call matching exercises the pre-existing Diff
	// formatter and is unrelated to checkpoints.)
	cyclicMap := map[string]interface{}{}
	cyclicMap["self"] = cyclicMap
	cyclicMap["v"] = "x"
	gotMap := snapshotArguments(Arguments{cyclicMap})[0].(map[string]interface{})
	assert.Equal(t, "x", gotMap["v"])
	self, ok := gotMap["self"].(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, reflect.ValueOf(gotMap).Pointer(), reflect.ValueOf(self).Pointer(), "map self-reference must be preserved")

	cyclicMap["v"] = "mutated"
	assert.Equal(t, "x", gotMap["v"])
}

func TestCheckpointNilAndOpaqueArguments(t *testing.T) {
	t.Parallel()
	m := &Mock{}
	m.On("M", Anything, Anything, Anything, Anything).Return()
	cp := m.Checkpoint()

	ch := make(chan struct{})
	fn := func(s string) string { return s }
	var nilPtr *struct{ x int }
	m.MethodCalled("M", nil, ch, fn, nilPtr)

	rec := cp.CallsSince()[0]
	assert.Nil(t, rec.Arguments.Get(0))
	assert.Equal(t, reflect.ValueOf(ch).Pointer(), reflect.ValueOf(rec.Arguments.Get(1)).Pointer())
	assert.Equal(t, reflect.ValueOf(fn).Pointer(), reflect.ValueOf(rec.Arguments.Get(2)).Pointer())
	assert.Nil(t, rec.Arguments.Get(3))

	// zero-argument call
	m2 := &Mock{}
	m2.On("NoArgs").Return()
	cp2 := m2.Checkpoint()
	m2.MethodCalled("NoArgs")
	recs := cp2.CallsSince()
	require.Len(t, recs, 1)
	assert.Equal(t, "NoArgs", recs[0].Method)
	assert.Nil(t, recs[0].Arguments)

	// checkpoint on a zero-value Mock, before anything else happened
	var bare Mock
	bareCP := bare.Checkpoint()
	assert.Equal(t, 0, bareCP.Seq())
	assert.Nil(t, bareCP.CallsSince())
}
