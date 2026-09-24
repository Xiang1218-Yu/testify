package require

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type requireFakeT struct {
	mu      sync.Mutex
	errors  []string
	failNow bool
	goexit  bool
	name    string
}

func (f *requireFakeT) Errorf(format string, args ...interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *requireFakeT) FailNow() {
	f.failNow = true
	if f.goexit {
		runtime.Goexit()
	}
}

func (f *requireFakeT) Helper() {}

func (f *requireFakeT) Name() string { return f.name }

func (f *requireFakeT) errorCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.errors)
}

// fatalTestingT is the subset require assertions need from a test context.
type fatalTestingT interface {
	Errorf(string, ...interface{})
	FailNow()
}

// runWithGoexit runs fn in a goroutine whose FailNow terminates the goroutine
// (like testing.T) and reports whether fn was stopped.
func runWithGoexit(renderer assert.FailureRenderer, fn func(t fatalTestingT)) (stopped bool, ctx *assert.FailureContext) {
	fake := &requireFakeT{goexit: true}
	ctx = assert.WithFailureRenderer(fake, renderer)
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

func TestRequireRecordsFatalFailureAndStops(t *testing.T) {
	fake := &requireFakeT{}
	ctx := assert.WithFailureRenderer(fake, assert.NewJSONFailureRenderer())

	// The fake FailNow does not goexit, so execution continues here.
	Equal(ctx, 1, 2, "require failure")

	True(t, fake.failNow, "require must invoke FailNow")
	records := ctx.Failures()
	Equal(t, 1, len(records))
	record := records[0]
	Equal(t, "Equal", record.Assertion(), "assertion name is inferred through require frames")
	True(t, record.Fatal(), "require records must be fatal")
	Equal(t, 1, record.Expected())
	Equal(t, 2, record.Actual())
	Equal(t, "require failure", record.ExtraMessage())

	var decoded map[string]interface{}
	NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(fake.errors[0], "\n")), &decoded))
	Equal(t, "Equal", decoded["assertion"])
	Equal(t, true, decoded["fatal"])
}

func TestRequireTerminatesExecutionImmediately(t *testing.T) {
	stopped, ctx := runWithGoexit(assert.NewJSONFailureRenderer(), func(tt fatalTestingT) {
		True(tt, false, "stops here")
		panic("code after failed require must not run")
	})
	True(t, stopped, "require must stop execution like FailNow")
	records := ctx.Failures()
	Equal(t, 1, len(records))
	Equal(t, "True", records[0].Assertion())
	True(t, records[0].Fatal())
}

type panickingRequireRenderer struct{}

func (panickingRequireRenderer) RenderFailure(*assert.FailureRecord) string {
	panic("renderer panic")
}

func TestRequireStillTerminatesWhenRendererPanics(t *testing.T) {
	stopped, ctx := runWithGoexit(panickingRequireRenderer{}, func(tt fatalTestingT) {
		Equal(tt, "a", "b")
		panic("must not be reached")
	})
	True(t, stopped, "renderer panic must not undo require termination")
	True(t, ctx.Failures()[0].Fatal())
}

func TestRequireHTTPFailureIsStructured(t *testing.T) {
	fake := &requireFakeT{}
	ctx := assert.WithFailureRenderer(fake, assert.NewJSONFailureRenderer())

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	HTTPStatusCode(ctx, handler, "GET", "/boom", nil, http.StatusOK, "http check")

	True(t, fake.failNow, "require HTTP failure must terminate")
	records := ctx.Failures()
	Equal(t, 1, len(records))
	record := records[0]
	Equal(t, "HTTPStatusCode", record.Assertion())
	Equal(t, http.StatusOK, record.Expected())
	Equal(t, http.StatusInternalServerError, record.Actual())
	True(t, record.Fatal())

	var decoded map[string]interface{}
	NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(fake.errors[0], "\n")), &decoded))
	Equal(t, "HTTPStatusCode", decoded["assertion"])
	Equal(t, true, decoded["fatal"])
}

func TestRequireFailWithIsFatal(t *testing.T) {
	fake := &requireFakeT{}
	ctx := assert.WithFailureRenderer(fake, assert.NewTextFailureRenderer())

	FailWith(ctx, assert.FailureOptions{
		Assertion: "CustomHelper",
		Expected:  "ok",
		Actual:    "bad",
	}, "custom helper failed")

	True(t, fake.failNow)
	records := ctx.Failures()
	Equal(t, 1, len(records))
	// The explicitly provided assertion name is kept, but require semantics
	// always make the failure fatal.
	Equal(t, "CustomHelper", records[0].Assertion())
	True(t, records[0].Fatal())
	Equal(t, "ok", records[0].Expected())
	Equal(t, "bad", records[0].Actual())
}

func TestRequireFailureRendererIsOptional(t *testing.T) {
	// Without a renderer wrapper, require keeps behaving exactly as before.
	fake := &requireFakeT{}
	Equal(fake, 1, 1) // passing: no error, no FailNow
	Equal(t, 0, fake.errorCount())
	False(t, fake.failNow)

	FailNow(fake, "plain failure")
	True(t, fake.failNow)
	Equal(t, 1, fake.errorCount())
	Contains(t, fake.errors[0], "plain failure")
	Contains(t, fake.errors[0], "Error Trace:")
}
