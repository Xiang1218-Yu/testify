package assert

import (
	"runtime"
	"strings"
	"time"
)

// FailureEvent is the machine-readable representation of a single failed
// assertion. In addition to the usual text output, exactly one FailureEvent
// is delivered to every [FailureEventSink] attached to the [TestingT] that
// is passed to the failing assertion.
//
// All assert and require assertions share this shape: the plain, formatted
// ("f" suffix) and terminal (require / [FailNow]) variants of the same
// assertion produce the same AssertionName.
type FailureEvent struct {
	// ID is the process-unique identifier of this event. Every reported
	// failure gets exactly one, monotonically increasing ID, so consumers
	// can deduplicate and correlate events.
	ID uint64

	// Time is the moment the failure was reported.
	Time time.Time

	// TestName is the name of the owning (sub)test as reported by
	// TestingT.Name(), including its full parent path. It is empty when
	// the TestingT does not implement Name().
	TestName string

	// AssertionName is the canonical name of the failed assertion, for
	// example "Equal". Forwarding methods ("(*Assertions).Equal"),
	// formatted variants ("Equalf") and terminal require variants
	// ("require.Equal") are all normalized to the base assertion name.
	AssertionName string

	// Expected and Actual contain the raw values compared by the
	// assertion. They are nil for assertions that do not distinguish an
	// expected value from an actual value.
	Expected interface{}
	Actual   interface{}

	// Message is the optional, already formatted caller-supplied message.
	Message string

	// FailureMessage is the human-readable failure description rendered by
	// the assertion (the same text that appears on the "Error" output line).
	FailureMessage string

	// File and Line identify the failing assertion call in user code.
	File string
	Line int

	// Fatal reports whether the failure is terminal, i.e. it was produced
	// through the require package or through [FailNow], and stops the
	// current test goroutine.
	Fatal bool
}

// FailureEventSink is implemented by [TestingT] values that want to receive
// structured [FailureEvent]s. The sink is invoked in addition to, never
// instead of, the regular text output. A TestingT that does not implement
// the interface keeps behaving exactly as before, so existing TestingT
// implementations remain fully compatible.
type FailureEventSink interface {
	OnAssertionFailure(event FailureEvent)
}

// FailureEventFunc is the callback signature accepted by [NewEventT].
type FailureEventFunc func(event FailureEvent)

// EventT is a [TestingT] decorator which delivers a [FailureEvent] to a
// callback for every failed assertion, while preserving the usual text
// output, Helper reporting and FailNow behavior of the wrapped TestingT.
//
// It makes the failure event capability usable with a plain *testing.T:
//
//	events := make([]assert.FailureEvent, 0)
//	et := assert.NewEventT(t, func(event assert.FailureEvent) {
//	    events = append(events, event)
//	})
//	assert.Equal(et, expected, actual)
//
// The same EventT can be passed to assert, require, [New], [require.New]
// and to suite tests.
type EventT struct {
	TestingT
	onFailure FailureEventFunc
}

// NewEventT wraps t and invokes onFailure for every assertion failure
// reported through the returned [EventT]. A nil onFailure means no events
// are delivered but text output is otherwise unaffected.
func NewEventT(t TestingT, onFailure FailureEventFunc) *EventT {
	return &EventT{TestingT: t, onFailure: onFailure}
}

// OnAssertionFailure implements [FailureEventSink].
func (t *EventT) OnAssertionFailure(event FailureEvent) {
	if t.onFailure != nil {
		t.onFailure(event)
	}
}

// Helper forwards [testing.T.Helper] to the wrapped TestingT when supported.
func (t *EventT) Helper() {
	if h, ok := t.TestingT.(tHelper); ok {
		h.Helper()
	}
}

// FailNow forwards termination to the wrapped TestingT when it supports it,
// and panics otherwise, mirroring the fallback behavior of [FailNow].
func (t *EventT) FailNow() {
	if f, ok := t.TestingT.(failNower); ok {
		f.FailNow()
		return
	}
	panic("test failed and t is missing `FailNow()`")
}

// Name returns the name of the wrapped test when supported, so events keep
// their owning test context.
func (t *EventT) Name() string {
	if n, ok := t.TestingT.(interface{ Name() string }); ok {
		return n.Name()
	}
	return ""
}

const (
	assertPkgPath  = "github.com/stretchr/testify/assert"
	requirePkgPath = "github.com/stretchr/testify/require"
)

// failureOrigin walks the stack from the failure reporting chokepoint and
// derives the canonical assertion name, the user call site and whether the
// failure is terminal (require package / FailNow).
func failureOrigin() (name string, file string, line int, fatal bool) {
	pcs := make([]uintptr, 64)
	// Skip runtime.Callers and failureOrigin itself; the first recorded
	// frame is the reportFailure chokepoint.
	n := runtime.Callers(2, pcs)

	frames := runtime.CallersFrames(pcs[:n])
	var lastAssertion string
	for {
		frame, more := frames.Next()

		pkgPath, funcName := splitQualifiedFunc(frame.Function)
		if pkgPath == assertPkgPath || pkgPath == requirePkgPath {
			// Internal tests live in the assert/require packages. A
			// Test/Benchmark/Example frame (including methods on a test
			// suite and their closures) is user code, not library code.
			bareName := stripReceiver(funcName)
			if isTest(bareName, "Test") || isTest(bareName, "Benchmark") || isTest(bareName, "Example") {
				file, line = frame.File, frame.Line
				break
			}
			if pkgPath == requirePkgPath {
				fatal = true
			}
			if strings.HasPrefix(funcName, "FailNow") {
				fatal = true
			}
			if funcName != "reportFailure" && funcName != "failWithValues" {
				lastAssertion = funcName
			}
		} else {
			// First frame outside the assertion libraries is the call in
			// user code (or in a goroutine bootstrap when no user frame
			// exists).
			file, line = frame.File, frame.Line
			break
		}

		if !more {
			break
		}
	}

	return canonicalAssertionName(lastAssertion), file, line, fatal
}

// splitQualifiedFunc splits a runtime function name such as
// "github.com/stretchr/testify/assert.(*Assertions).Equal" into its package
// import path and its short function name (still including a possible
// receiver).
func splitQualifiedFunc(full string) (pkgPath string, funcName string) {
	lastSlash := strings.LastIndexByte(full, '/')
	dot := strings.IndexByte(full[lastSlash+1:], '.')
	if dot < 0 {
		return "", full
	}
	dot += lastSlash + 1
	return full[:dot], full[dot+1:]
}

// stripReceiver removes an optional method receiver, turning
// "(*Assertions).Equal" or "(*mySuite).TestX.func1" into "Equal" /
// "TestX.func1".
func stripReceiver(name string) string {
	if idx := strings.Index(name, ")."); idx >= 0 {
		return name[idx+2:]
	}
	return name
}

// canonicalAssertionName normalizes an assertion function name: receiver
// prefixes are removed and the trailing "f" of formatted variants is
// dropped ("Equalf" becomes "Equal").
func canonicalAssertionName(funcName string) string {
	funcName = stripReceiver(funcName)
	if len(funcName) > 1 && funcName[len(funcName)-1] == 'f' && funcName != "Errorf" {
		funcName = funcName[:len(funcName)-1]
	}
	return funcName
}
