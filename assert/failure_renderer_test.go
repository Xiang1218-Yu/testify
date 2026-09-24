package assert_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/assert/internal/failureharness"
)

// fakeFailureT is a minimal TestingT for failure renderer tests.
type fakeFailureT struct {
	mu       sync.Mutex
	errors   []string
	logs     []string
	failNow  bool
	goexit   bool
	testName string
}

func (f *fakeFailureT) Errorf(format string, args ...interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *fakeFailureT) Logf(format string, args ...interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

func (f *fakeFailureT) FailNow() {
	f.failNow = true
	if f.goexit {
		runtime.Goexit()
	}
}

func (f *fakeFailureT) Helper() {}

func (f *fakeFailureT) Name() string { return f.testName }

func (f *fakeFailureT) errorCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.errors)
}

// requireTerminatesInGoroutine runs fn in a goroutine whose FailNow calls
// runtime.Goexit (like testing.T) and reports whether fn was terminated
// before returning.
func requireTerminatesInGoroutine(renderer FailureRenderer, fn func(t TestingT)) (terminated bool, ctx *FailureContext) {
	fake := &fakeFailureT{goexit: true}
	ctx = WithFailureRenderer(fake, renderer)
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(done)
		fn(ctx)
		close(returned)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	select {
	case <-returned:
		return false, ctx
	default:
		return true, ctx
	}
}

func TestFailureRecordCarriesStructuredFields(t *testing.T) {
	t.Parallel()

	fake := &fakeFailureT{testName: "TestFailureRecordCarriesStructuredFields"}
	ctx := WithFailureRenderer(fake, nil) // nil renderer -> default text

	passed := failureharness.Equal(ctx, 1, 2, "extra %s", "message")
	False(t, passed, "failing Equal must return false")

	records := ctx.Failures()
	Len(t, records, 1)
	record := records[0]

	Equal(t, "Equal", record.Assertion())
	Equal(t, 1, record.Expected())
	Equal(t, 2, record.Actual())
	False(t, record.Fatal(), "assert failures must not be fatal")
	Equal(t, "extra message", record.ExtraMessage())
	Equal(t, fake.testName, record.TestName())
	Contains(t, record.Message(), "Not equal:")
	False(t, record.Time().IsZero(), "failure time must be recorded")

	location := record.Location()
	Contains(t, location, failureharness.File)
	locationInCallers := false
	for _, c := range record.Callers() {
		if c == location {
			locationInCallers = true
		}
	}
	True(t, locationInCallers)
}

func TestFailureRecordIsImmutable(t *testing.T) {
	t.Parallel()

	fake := &fakeFailureT{}
	ctx := WithFailureRenderer(fake, NewTextFailureRenderer())
	failureharness.Equal(ctx, "a", "b", "note")

	first := ctx.Failures()[0]
	Contains(t, first.Location(), failureharness.File)

	// Mutating the slice returned by Callers must not affect the record.
	callers := first.Callers()
	callers[0] = "mutated-location"
	NotEqual(t, "mutated-location", ctx.Failures()[0].Callers()[0])

	// Snapshot slices are independent.
	Equal(t, 1, len(ctx.Failures()))
	Equal(t, first, ctx.Failures()[0], "snapshots expose the same immutable record")

	// Default text output keeps the classic labeled layout.
	output := strings.Join(fake.errors, "\n")
	Contains(t, output, "Error Trace:")
	Contains(t, output, "Error:")
	Contains(t, output, "Not equal:")
	Contains(t, output, "Messages:")
}

func TestJSONFailureRendererOutput(t *testing.T) {
	t.Parallel()

	fake := &fakeFailureT{testName: "TestJSONFailureRendererOutput"}
	ctx := WithFailureRenderer(fake, NewJSONFailureRenderer())

	ok := failureharness.HTTPStatusCodeFailure(ctx, "status check")
	False(t, ok, "HTTPStatusCode must fail with 404 vs 200")

	records := ctx.Failures()
	Len(t, records, 1)
	record := records[0]
	Equal(t, "HTTPStatusCode", record.Assertion())
	Equal(t, http.StatusOK, record.Expected())
	Equal(t, http.StatusNotFound, record.Actual())
	Equal(t, "status check", record.ExtraMessage())

	// The rendered error is valid JSON carrying the same information.
	RawJSON := strings.TrimPrefix(strings.Join(fake.errors, ""), "\n")
	var decoded map[string]interface{}
	NoError(t, json.Unmarshal([]byte(RawJSON), &decoded))
	Equal(t, "HTTPStatusCode", decoded["assertion"])
	Equal(t, false, decoded["fatal"])
	Equal(t, float64(http.StatusOK), decoded["expected"])
	Equal(t, float64(http.StatusNotFound), decoded["actual"])
	Equal(t, "status check", decoded["extra_message"])
	Contains(t, decoded["message"].(string), "Expected HTTP status code")
	Contains(t, decoded["location"].(string), failureharness.File)
	NotNil(t, decoded["callers"])
	NotEmpty(t, decoded["time"])
}

// pointerRecordingRenderer is a custom renderer that captures the record it
// receives so the test can prove record identity across consumers.
type pointerRecordingRenderer struct {
	mu      sync.Mutex
	seen    []*FailureRecord
	outputs []string
}

func (r *pointerRecordingRenderer) RenderFailure(record *FailureRecord) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, record)
	out := fmt.Sprintf("CUSTOM[%s fatal=%t]: %s", record.Assertion(), record.Fatal(), record.Message())
	r.outputs = append(r.outputs, out)
	return out
}

func TestCustomRendererSharesRecordWithAggregator(t *testing.T) {
	t.Parallel()

	fake := &fakeFailureT{}
	renderer := &pointerRecordingRenderer{}
	ctx := WithFailureRenderer(fake, renderer)

	Equal(ctx, 1, 2)
	Nil(ctx, errors.New("boom"))

	records := ctx.Failures()
	Len(t, records, 2)
	Equal(t, "Equal", records[0].Assertion())
	Equal(t, 1, records[0].Expected())
	Equal(t, 2, records[0].Actual())
	Equal(t, "Nil", records[1].Assertion())
	Equal(t, errors.New("boom"), records[1].Actual())

	// The custom renderer observed the exact same immutable pointers that the
	// aggregator exposes.
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	Equal(t, records[0], renderer.seen[0])
	Equal(t, records[1], renderer.seen[1])

	// All three renderers describe one identical record, and their outputs
	// all carry the same core information.
	shared := records[0]
	text := NewTextFailureRenderer().RenderFailure(shared)
	var jsonView map[string]interface{}
	NoError(t, json.Unmarshal([]byte(NewJSONFailureRenderer().RenderFailure(shared)), &jsonView))
	Contains(t, text, "Not equal:")
	Equal(t, "Equal", jsonView["assertion"])
	Equal(t, shared.Message(), jsonView["message"])
	Contains(t, renderer.outputs[0], "CUSTOM[Equal fatal=false]")
}

func TestFailNowIsFatalAndAssertContinues(t *testing.T) {
	t.Parallel()

	// FailNow marks the record fatal and invokes FailNow on the context.
	fake := &fakeFailureT{}
	ctx := WithFailureRenderer(fake, NewTextFailureRenderer())
	FailNow(ctx, "fatal failure", "extra")
	True(t, fake.failNow, "FailNow must call FailNow on the test context")
	records := ctx.Failures()
	Len(t, records, 1)
	Equal(t, "FailNow", records[0].Assertion())
	True(t, records[0].Fatal(), "FailNow records must be fatal")
	Equal(t, "extra", records[0].ExtraMessage())

	// assert keeps executing and aggregates every failure.
	continued := 0
	Equal(ctx, "a", "b")
	Equal(ctx, "c", "d")
	continued++
	Equal(t, 1, continued)
	Len(t, ctx.Failures(), 3, "assert failures must accumulate without stopping")
}

func TestFailNowTerminatesEvenWhenRendererPanics(t *testing.T) {
	t.Parallel()

	terminated, termCtx := requireTerminatesInGoroutine(panickingRenderer{}, func(tt TestingT) {
		FailNow(tt, "boom")
		panic("must not be reached")
	})
	True(t, terminated, "FailNow must terminate even if the renderer panics")
	records := termCtx.Failures()
	Len(t, records, 1)
	True(t, records[0].Fatal())
}

func TestNestedAndInferredAssertionNames(t *testing.T) {
	t.Parallel()

	fake := &fakeFailureT{}
	ctx := WithFailureRenderer(fake, NewTextFailureRenderer())

	// An assertion invoked through a user-code helper keeps the assertion
	// name and is located at the user helper.
	failureharness.HelperCallingTrue(ctx)
	True(t, Equal(ctx, 1, 1), "sanity: passing assertion returns true")

	// Nested assertions (one assertion inside another) report both records.
	False(t, failureharness.NestedTrueEqual(ctx))

	records := ctx.Failures()
	Len(t, records, 3)
	Equal(t, []string{"True", "Equal", "True"}, []string{
		records[0].Assertion(),
		records[1].Assertion(),
		records[2].Assertion(),
	})
	for _, record := range records {
		Contains(t, record.Location(), failureharness.File)
	}

	// Direct Fail is attributed to Fail and is non-fatal.
	Fail(ctx, "manual failure")
	Equal(t, "Fail", ctx.Failures()[3].Assertion())
	False(t, ctx.Failures()[3].Fatal())
}

func TestEventuallyWithTNestedAssertions(t *testing.T) {
	t.Parallel()

	fake := &fakeFailureT{}
	ctx := WithFailureRenderer(fake, NewTextFailureRenderer())

	ok := failureharness.EventuallyWithTFailure(ctx)
	False(t, ok)
	True(t, ctx.HasFailure())
	records := ctx.Failures()
	Equal(t, "EventuallyWithT", records[0].Assertion())
	False(t, records[0].Fatal())
	Contains(t, records[0].Location(), failureharness.File)
}

type panickingRenderer struct{}

func (panickingRenderer) RenderFailure(*FailureRecord) string { panic("renderer boom") }

type emptyRenderer struct{}

func (emptyRenderer) RenderFailure(*FailureRecord) string { return "" }

func TestRendererFailureNeverSwallowsOriginal(t *testing.T) {
	t.Parallel()

	// Panicking renderer: original failure survives and assert keeps going.
	fake := &fakeFailureT{}
	ctx := WithFailureRenderer(fake, panickingRenderer{})
	failureharness.Equal(ctx, 1, 2, "kept message")
	True(t, ctx.HasFailure(), "record must still be observed")
	record := ctx.Failures()[0]
	Contains(t, record.Location(), failureharness.File)
	output := fake.errors[0]
	Contains(t, output, "Not equal:")
	Contains(t, output, "renderer boom")
	Contains(t, output, "original failure is shown above")

	// Empty renderer output also falls back to the original text failure.
	fake2 := &fakeFailureT{}
	ctx2 := WithFailureRenderer(fake2, emptyRenderer{})
	failed := False(ctx2, true)
	False(t, failed, "False must report a failure")
	Contains(t, fake2.errors[0], "Should be false")
	Contains(t, fake2.errors[0], "returned empty output")

}

type panickingObserverT struct {
	*fakeFailureT
}

func (panickingObserverT) FailureRenderer() FailureRenderer { return NewJSONFailureRenderer() }

func (panickingObserverT) ObserveFailure(*FailureRecord) { panic("observer boom") }

func TestPanickingObserverKeepsFailureReporting(t *testing.T) {
	t.Parallel()

	fake := &fakeFailureT{}
	ctx := panickingObserverT{fake}
	passed := Equal(ctx, 1, 2)
	False(t, passed, "failure must be reported despite observer panic")
	Equal(t, 1, fake.errorCount())
	var decoded map[string]interface{}
	NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(fake.errors[0], "\n")), &decoded))
	Equal(t, "Equal", decoded["assertion"])
}

func TestMultipleFailuresAggregateInOrder(t *testing.T) {
	t.Parallel()

	fake := &fakeFailureT{}
	ctx := WithFailureRenderer(fake, NewJSONFailureRenderer())

	failureharness.Equal(ctx, 1, 2)
	failureharness.True(ctx, false)
	failureharness.Nil(ctx, errors.New("nope"))
	failureharness.Len(ctx, []int{1}, 2)
	failureharness.Fail(ctx, "done")

	records := ctx.Failures()
	Equal(t, []string{"Equal", "True", "Nil", "Len", "Fail"}, []string{
		records[0].Assertion(),
		records[1].Assertion(),
		records[2].Assertion(),
		records[3].Assertion(),
		records[4].Assertion(),
	})
	Equal(t, 2, records[3].Expected()) // Len expected length 2
	Equal(t, 1, records[3].Actual())   // Len actual length 1
	Equal(t, 5, fake.errorCount(), "every failure is rendered exactly once")

	// Every rendered document is independent valid JSON.
	for _, rendered := range fake.errors {
		var doc map[string]interface{}
		NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(rendered, "\n")), &doc))
	}
}

func TestFailureContextSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	fake := &fakeFailureT{}
	ctx := WithFailureRenderer(fake, NewTextFailureRenderer())

	const goroutines = 60
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			failureharness.Equal(ctx, i, i+1)
			if i%3 == 0 {
				failureharness.True(ctx, false)
			}
		}(i)
	}
	wg.Wait()

	expected := goroutines + goroutines/3 // 60 Equal + 20 True failures
	records := ctx.Failures()
	Len(t, records, expected)
	Equal(t, expected, fake.errorCount())
	for _, record := range records {
		False(t, record.Fatal())
		NotEmpty(t, record.Location())
	}
}

func TestAssertionsObjectForwardsToRendererContext(t *testing.T) {
	t.Parallel()

	fake := &fakeFailureT{}
	ctx := WithFailureRenderer(fake, NewJSONFailureRenderer())

	// The forward Assertions object keeps using the selected test context.
	False(t, New(ctx).Equal(1, 2, "via assertions object"))
	records := ctx.Failures()
	Len(t, records, 1)
	Equal(t, "Equal", records[0].Assertion())
	Equal(t, 1, records[0].Expected())
	Equal(t, 2, records[0].Actual())
	Equal(t, "via assertions object", records[0].ExtraMessage())
}

func TestJSONRendererHandlesUnserializableValues(t *testing.T) {
	t.Parallel()

	fake := &fakeFailureT{}
	ctx := WithFailureRenderer(fake, NewJSONFailureRenderer())

	failureharness.UnserializableEqual(ctx)

	var decoded map[string]interface{}
	NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(fake.errors[0], "\n")), &decoded))
	Equal(t, "Equal", decoded["assertion"])
	// Channels cannot be JSON encoded; they fall back to a {"go": ...} view
	// instead of dropping the failure.
	actual, ok := decoded["actual"].(map[string]interface{})
	True(t, ok)
	Contains(t, actual["go"].(string), "chan int")
}
