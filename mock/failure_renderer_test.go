package mock

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type mockFailureFakeT struct {
	mu      sync.Mutex
	logs    []string
	errors  []string
	failNow bool
	goexit  bool
}

func (f *mockFailureFakeT) Logf(format string, args ...interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

func (f *mockFailureFakeT) Errorf(format string, args ...interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *mockFailureFakeT) FailNow() {
	f.failNow = true
	if f.goexit {
		runtime.Goexit()
	}
}

func (f *mockFailureFakeT) Helper() {}

func (f *mockFailureFakeT) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.errors))
	copy(out, f.errors)
	return out
}

type rendererMock struct{ Mock }

func (m *rendererMock) DoThing(arg string) string {
	return m.Called(arg).String(0)
}

func TestMockAssertExpectationsStructuredAndLegacyOutput(t *testing.T) {
	fake := &mockFailureFakeT{}
	ctx := assert.WithFailureRenderer(fake, assert.NewTextFailureRenderer())

	m := new(rendererMock)
	m.On("DoThing", "hello").Return("world")
	// Never call the expected method: AssertExpectations must fail.
	pass := m.AssertExpectations(ctx)
	assert.False(t, pass)
	assert.False(t, fake.failNow, "AssertExpectations is non-fatal (assert semantics)")

	records := ctx.Failures()
	assert.Len(t, records, 1)
	record := records[0]
	assert.Equal(t, "Mock.AssertExpectations", record.Assertion())
	assert.False(t, record.Fatal())
	assert.Equal(t, 1, record.Expected())
	assert.Equal(t, 0, record.Actual())

	// Default text output is the historical raw mock message, byte compatible.
	raw := fake.snapshot()[0]
	assert.True(t, strings.HasPrefix(raw, "FAIL: 0 out of 1 expectation(s) were met."))
	assert.Contains(t, raw, "The code you are testing needs to make 1 more call(s).")
	assert.NotContains(t, raw, "Error Trace:", "legacy mock output must not gain labeled layout")
}

func TestMockAssertCalledAndNumberOfCallsStructured(t *testing.T) {
	fake := &mockFailureFakeT{}
	ctx := assert.WithFailureRenderer(fake, assert.NewTextFailureRenderer())

	m := new(rendererMock)
	m.On("DoThing", "hello").Return("world").Maybe()

	assert.False(t, m.AssertCalled(ctx, "DoThing", "hello"))
	records := ctx.Failures()
	assert.Len(t, records, 1)
	assert.Equal(t, "Mock.AssertCalled", records[0].Assertion())
	assert.Contains(t, records[0].Message(), "Should have called with given arguments")

	assert.False(t, m.AssertNumberOfCalls(ctx, "DoThing", 2))
	records = ctx.Failures()
	assert.Len(t, records, 2)
	assert.Equal(t, "Mock.AssertNumberOfCalls", records[1].Assertion())
	assert.Equal(t, 2, records[1].Expected())
	assert.Equal(t, 0, records[1].Actual())
	assert.False(t, fake.failNow)
}

func TestMockArgumentsAssertStructured(t *testing.T) {
	fake := &mockFailureFakeT{}
	ctx := assert.WithFailureRenderer(fake, assert.NewTextFailureRenderer())

	args := Arguments{"a", 1}
	pass := args.Assert(ctx, "a", 2)
	assert.False(t, pass)

	records := ctx.Failures()
	assert.Len(t, records, 1)
	assert.Equal(t, "Mock.Arguments.Assert", records[0].Assertion())
	assert.Contains(t, fake.snapshot()[0], "Arguments do not match.")
}

func TestMockUnexpectedCallFatalAndRendered(t *testing.T) {
	fake := &mockFailureFakeT{goexit: true}
	ctx := assert.WithFailureRenderer(fake, assert.NewJSONFailureRenderer())
	m := new(rendererMock)
	m.Test(ctx)

	stopped := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(stopped)
		// No expectation registered: the unexpected call fails fatally.
		m.DoThing("surprise")
		close(returned)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("mock failure did not stop the goroutine")
	}
	select {
	case <-returned:
		t.Fatal("code after the fatal mock failure must not run")
	default:
	}

	assert.True(t, fake.failNow, "unexpected mock call must FailNow (require-like semantics)")
	records := ctx.Failures()
	assert.Len(t, records, 1)
	record := records[0]
	assert.Equal(t, "Mock", record.Assertion())
	assert.True(t, record.Fatal(), "runtime mock failures terminate immediately")
	assert.Contains(t, record.Message(), "method call was unexpected")
	assert.Contains(t, record.Message(), "DoThing(string)")

	// The renderer selected by the context covers mock failures too.
	var decoded map[string]interface{}
	assert.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(fake.snapshot()[0], "\n")), &decoded))
	assert.Equal(t, "Mock", decoded["assertion"])
	assert.Equal(t, true, decoded["fatal"])
	assert.Contains(t, decoded["message"], "method call was unexpected")
}

func TestMockFailureWithoutTestStillPanics(t *testing.T) {
	// The pre-renderer panic path for mocks without a registered test context
	// must remain intact.
	m := new(rendererMock)
	assert.Panics(t, func() {
		defer func() {
			if r := recover(); r != nil {
				assert.Contains(t, fmt.Sprint(r), "method call was unexpected")
				panic(r)
			}
		}()
		m.DoThing("no-test")
	})
}
