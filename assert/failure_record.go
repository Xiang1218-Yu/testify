package assert

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FailureRecord is an immutable snapshot of a single assertion failure.
//
// The same instance is handed to every consumer of a failure: the default
// human oriented text renderer, the built-in JSON renderer, custom renderers
// and FailureObserver implementations such as report aggregators. All fields
// are exposed through read-only accessors; there is no way to mutate a record
// after it has been constructed, so the terminal output, machine readable
// reports and custom renderings always describe the exact same failure.
type FailureRecord struct {
	assertion    string
	message      string
	extraMessage string
	callers      []string
	testName     string
	expected     interface{}
	actual       interface{}
	fatal        bool
	// preformatted failures (e.g. the historical mock output) keep their raw
	// message under the default text renderer for backwards compatibility.
	preformatted bool
	at           time.Time
}

// Assertion returns the name of the failing assertion, e.g. "Equal".
// Failures triggered through the require package keep the short assertion
// name ("Equal"); use [FailureRecord.Fatal] to distinguish them.
func (r *FailureRecord) Assertion() string {
	return r.assertion
}

// Message returns the primary failure message produced by the assertion.
func (r *FailureRecord) Message() string {
	return r.message
}

// ExtraMessage returns the optional message supplied by the caller
// (msgAndArgs). It is empty when no extra message was provided.
func (r *FailureRecord) ExtraMessage() string {
	return r.extraMessage
}

// Callers returns a copy of the file:line locations leading from the test to
// the failing assertion.
func (r *FailureRecord) Callers() []string {
	if len(r.callers) == 0 {
		return nil
	}
	out := make([]string, len(r.callers))
	copy(out, r.callers)
	return out
}

// Location returns the user code location closest to the failing assertion,
// or an empty string when unavailable.
func (r *FailureRecord) Location() string {
	if len(r.callers) == 0 {
		return ""
	}
	return r.callers[0]
}

// TestName returns the name of the running test when the test context
// exposes it (testing.T.Name()), or an empty string.
func (r *FailureRecord) TestName() string {
	return r.testName
}

// Expected returns the structured expected value captured by comparison
// assertions (Equal, Nil, Len, HTTPStatusCode, ...). It is nil when the
// assertion does not expose a structured expected value; use [FailureRecord.Message]
// in that case.
func (r *FailureRecord) Expected() interface{} {
	return r.expected
}

// Actual returns the structured actual value captured by comparison
// assertions. It is nil when the assertion does not expose one.
func (r *FailureRecord) Actual() interface{} {
	return r.actual
}

// Fatal reports whether the failure terminates the test immediately. It is
// true for failures triggered through the require package or through
// [FailNow], and false for regular assert failures that allow the test to
// continue.
func (r *FailureRecord) Fatal() bool {
	return r.fatal
}

// Time returns the moment at which the failure was recorded.
func (r *FailureRecord) Time() time.Time {
	return r.at
}

// failureRecordView is the JSON projection of a FailureRecord.
type failureRecordView struct {
	Assertion    string          `json:"assertion"`
	Fatal        bool            `json:"fatal"`
	TestName     string          `json:"test_name,omitempty"`
	Location     string          `json:"location,omitempty"`
	Callers      []string        `json:"callers,omitempty"`
	Message      string          `json:"message"`
	ExtraMessage string          `json:"extra_message,omitempty"`
	Expected     json.RawMessage `json:"expected"`
	Actual       json.RawMessage `json:"actual"`
	Time         time.Time       `json:"time"`
}

// MarshalJSON renders the record as stable machine readable JSON. Values that
// encoding/json cannot represent (functions, channels, cycles, ...) fall
// back to their Go syntax inside a {"go": ...} object instead of failing the
// whole rendering.
func (r *FailureRecord) MarshalJSON() ([]byte, error) {
	view := failureRecordView{
		Assertion:    r.assertion,
		Fatal:        r.fatal,
		TestName:     r.testName,
		Location:     r.Location(),
		Callers:      r.Callers(),
		Message:      r.message,
		ExtraMessage: r.extraMessage,
		Expected:     marshalFailureValue(r.expected),
		Actual:       marshalFailureValue(r.actual),
		Time:         r.at,
	}
	return json.Marshal(view)
}

func marshalFailureValue(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err == nil {
		return b
	}
	fallback, _ := json.Marshal(map[string]string{
		"go": fmt.Sprintf("%#v", v),
	})
	return fallback
}

// FailureRenderer turns an immutable [FailureRecord] into the string reported
// through [TestingT.Errorf]. Implementations must be safe for concurrent use
// when the same test context is used from multiple goroutines.
type FailureRenderer interface {
	RenderFailure(record *FailureRecord) string
}

// TextFailureRenderer is the default renderer. It reproduces the historical
// testify terminal layout, so existing output parsing and golden files keep
// working unchanged.
type TextFailureRenderer struct{}

// NewTextFailureRenderer returns the default human oriented renderer.
func NewTextFailureRenderer() TextFailureRenderer {
	return TextFailureRenderer{}
}

// RenderFailure implements [FailureRenderer].
func (TextFailureRenderer) RenderFailure(record *FailureRecord) string {
	if record.preformatted {
		// Legacy producers (e.g. mock) used to report their message verbatim.
		return record.message
	}
	content := []labeledContent{
		{"Error Trace", strings.Join(record.callers, "\n\t\t\t")},
		{"Error", record.message},
	}
	if record.testName != "" {
		content = append(content, labeledContent{"Test", record.testName})
	}
	if record.extraMessage != "" {
		content = append(content, labeledContent{"Messages", record.extraMessage})
	}
	return labeledOutput(content...)
}

// JSONFailureRenderer emits every failure as an indented JSON document,
// suitable for machine readable test reports.
type JSONFailureRenderer struct{}

// NewJSONFailureRenderer returns a renderer that serializes failure records
// to indented JSON.
func NewJSONFailureRenderer() JSONFailureRenderer {
	return JSONFailureRenderer{}
}

// RenderFailure implements [FailureRenderer].
func (JSONFailureRenderer) RenderFailure(record *FailureRecord) string {
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		// MarshalJSON is defensive, so this only happens for truly exotic
		// payloads. Emit a valid JSON envelope instead of dropping the failure.
		return fmt.Sprintf(
			"{\n  \"renderer_error\": %s,\n  \"assertion\": %s,\n  \"message\": %s\n}",
			strconv.Quote(err.Error()),
			strconv.Quote(record.assertion),
			strconv.Quote(record.message),
		)
	}
	return string(b)
}

// FailureRendererProvider is implemented by test contexts that want to choose
// the renderer used for their failures. The plain *testing.T does not
// implement it and therefore keeps the default text rendering; wrap it with
// [WithFailureRenderer] or implement it on a custom TestingT.
type FailureRendererProvider interface {
	FailureRenderer() FailureRenderer
}

// FailureObserver is implemented by test contexts that want to receive the
// immutable record of every failure, for example to aggregate multiple
// failures into a machine readable report. Observing a failure never replaces
// rendering and reporting it through [TestingT.Errorf].
type FailureObserver interface {
	ObserveFailure(record *FailureRecord)
}

// FailureOptions carries optional structured metadata when reporting a
// failure through [FailWith]. The zero value is valid.
type FailureOptions struct {
	// Assertion overrides the inferred assertion name.
	Assertion string
	// Expected and Actual attach structured compared values to the record.
	Expected interface{}
	Actual   interface{}
	// Fatal marks an immediately terminating failure (require semantics).
	Fatal bool
	// Preformatted keeps the failure message verbatim under the default text
	// renderer instead of wrapping it in the standard labeled layout. It is
	// meant for historical producers with an established output format.
	Preformatted bool
}

// FailWith is the structured failure entry point. It behaves like [Fail] -
// it reports the failure through t.Errorf and returns false - but additionally
// records structured metadata in the immutable [FailureRecord] and lets the
// current test context select how the failure is rendered. It is intended for
// code outside the assertion functions (such as mock) and for custom
// assertion helpers.
func FailWith(t TestingT, opts FailureOptions, failureMessage string, msgAndArgs ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return reportFailure(t, opts, failureMessage, msgAndArgs...)
}

// failWithValues is the single entry point used by comparison assertions that
// can provide structured expected/actual values.
func failWithValues(t TestingT, expected, actual interface{}, failureMessage string, msgAndArgs ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return reportFailure(t, FailureOptions{Expected: expected, Actual: actual}, failureMessage, msgAndArgs...)
}

// safeRenderFailure invokes the renderer while guaranteeing that a panicking,
// erroring or empty renderer can never hide the original failure: rendering
// falls back to the default text output of the same immutable record and the
// renderer problem is appended to the output.
func safeRenderFailure(renderer FailureRenderer, record *FailureRecord) (output string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			output = fallbackFailureOutput(record, fmt.Sprintf("renderer %T panicked: %v", renderer, recovered))
		}
	}()
	output = renderer.RenderFailure(record)
	if strings.TrimSpace(output) == "" {
		return fallbackFailureOutput(record, fmt.Sprintf("renderer %T returned empty output", renderer))
	}
	return output
}

func fallbackFailureOutput(record *FailureRecord, note string) string {
	base := NewTextFailureRenderer().RenderFailure(record)
	return base + "\n[testify] failure renderer error; the original failure is shown above: " + note
}

// FailureContext wraps a [TestingT] so that failures reported against it are
// aggregated as immutable [FailureRecord] values and rendered with the chosen
// [FailureRenderer]. Assertion semantics do not change: assert failures keep
// running and require still calls FailNow on the wrapped context. It is safe
// for concurrent use.
type FailureContext struct {
	TestingT
	renderer FailureRenderer

	mu      sync.Mutex
	records []*FailureRecord
}

// WithFailureRenderer wraps t and selects the renderer used for every failure
// reported through the returned context. Pass [NewTextFailureRenderer],
// [NewJSONFailureRenderer] or a custom [FailureRenderer].
//
//	ctx := assert.WithFailureRenderer(t, assert.NewJSONFailureRenderer())
//	assert.Equal(ctx, 1, 2)
//	for _, record := range ctx.Failures() { ... }
func WithFailureRenderer(t TestingT, renderer FailureRenderer) *FailureContext {
	if renderer == nil {
		renderer = NewTextFailureRenderer()
	}
	return &FailureContext{TestingT: t, renderer: renderer}
}

// Errorf reports through the wrapped context. It is marked as a test helper
// so that failure attribution does not point at the wrapper itself.
func (c *FailureContext) Errorf(format string, args ...interface{}) {
	c.Helper()
	c.TestingT.Errorf(format, args...)
}

// Helper delegates helper marking to the wrapped context.
func (c *FailureContext) Helper() {
	if h, ok := c.TestingT.(interface{ Helper() }); ok {
		h.Helper()
	}
}

// FailNow terminates the wrapped test, preserving require semantics.
func (c *FailureContext) FailNow() {
	c.Helper()
	if fn, ok := c.TestingT.(interface{ FailNow() }); ok {
		fn.FailNow()
	} else {
		panic("test failed and t is missing `FailNow()`")
	}
}

// Logf delegates log reporting to the wrapped context when it supports it
// (mock relies on it).
func (c *FailureContext) Logf(format string, args ...interface{}) {
	if l, ok := c.TestingT.(interface{ Logf(string, ...interface{}) }); ok {
		l.Logf(format, args...)
	}
}

// Name delegates the test name to the wrapped context when it exposes it.
func (c *FailureContext) Name() string {
	if n, ok := c.TestingT.(interface{ Name() string }); ok {
		return n.Name()
	}
	return ""
}

// Skipped reports whether the wrapped test was skipped.
func (c *FailureContext) Skipped() bool {
	if s, ok := c.TestingT.(interface{ Skipped() bool }); ok {
		return s.Skipped()
	}
	return false
}

// FailureRenderer implements [FailureRendererProvider].
func (c *FailureContext) FailureRenderer() FailureRenderer {
	return c.renderer
}

// ObserveFailure implements [FailureObserver]; it stores an immutable copy of
// the record for later aggregation.
func (c *FailureContext) ObserveFailure(record *FailureRecord) {
	c.mu.Lock()
	c.records = append(c.records, record)
	c.mu.Unlock()
}

// Failures returns a snapshot copy of all records observed since the context
// was created. It can be used to aggregate multiple failures from one test.
func (c *FailureContext) Failures() []*FailureRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*FailureRecord, len(c.records))
	copy(out, c.records)
	return out
}

// HasFailure reports whether at least one failure was observed.
func (c *FailureContext) HasFailure() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.records) > 0
}

// failureOrigin walks the call stack and returns (assertion name, fatal),
// picking the outermost testify production frame, i.e. the assertion the user
// actually invoked. A require frame makes the failure fatal; mock frames are
// prefixed with "Mock.". Frames living in _test.go files (testify's own test
// packages are package assert/require/mock too) are treated as user frames.
func failureOrigin() (assertion string, fatal bool) {
	pcs := make([]uintptr, 64)
	// Skip runtime.Callers, failureOrigin itself and reportFailure.
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])

	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.File, "_test.go") {
			if assertion != "" {
				return assertion, fatal
			}
		} else if pkg, name, ok := testifyFrame(frame.Function); ok {
			switch pkg {
			case "assert", "require":
				assertion = name
				fatal = pkg == "require"
			case "mock":
				assertion = "Mock." + name
			}
		} else if assertion != "" {
			// First user frame after the failing assertion: stop walking.
			return assertion, fatal
		}
		if !more {
			break
		}
	}
	if assertion == "" {
		assertion = "Fail"
	}
	return assertion, fatal
}

// testifyFrame splits a fully qualified function name into its package short
// name and function/receiver method name, returning ok=false outside testify.
func testifyFrame(function string) (pkg string, name string, ok bool) {
	lastSlash := strings.LastIndexByte(function, '/')
	tail := function[lastSlash+1:] // e.g. assert.(*Assertions).Equal
	firstDot := strings.IndexByte(tail, '.')
	if firstDot < 0 {
		return "", "", false
	}
	pkg = tail[:firstDot]
	switch pkg {
	case "assert", "require", "mock":
	default:
		return "", "", false
	}
	name = tail[firstDot+1:]
	// Strip a pointer/value receiver, e.g. "(*Assertions).Equal" -> "Equal",
	// "Arguments.Assert" stays intact.
	if strings.HasPrefix(name, "(") {
		if closeParen := strings.Index(name, ")."); closeParen >= 0 {
			name = name[closeParen+2:]
		}
	}
	return pkg, name, true
}
