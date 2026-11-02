package suite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// EventKind identifies the kind of an execution Event emitted while a
// suite is running.
type EventKind string

const (
	// EventSuiteStart is emitted once when an observed suite run starts,
	// before any test method is discovered.
	EventSuiteStart EventKind = "suite_start"
	// EventSuiteFinish is emitted once when an observed suite run finishes,
	// after the statistics event.
	EventSuiteFinish EventKind = "suite_finish"
	// EventSuiteSkipped is emitted when the suite itself is skipped, for
	// example from SetupSuite.
	EventSuiteSkipped EventKind = "suite_skipped"
	// EventSuitePanicked is emitted when a suite level lifecycle phase
	// (SetupSuite or TearDownSuite) panics.
	EventSuitePanicked EventKind = "suite_panicked"

	// EventTestDiscovered is emitted for every method with a "Test" prefix
	// found on the suite, including methods with an invalid signature.
	EventTestDiscovered EventKind = "test_discovered"
	// EventTestFiltered is emitted for discovered methods excluded by the
	// -testify.m filter.
	EventTestFiltered EventKind = "test_filtered"

	// EventTestStart is emitted right before a test method runs.
	EventTestStart EventKind = "test_start"
	// EventTestFinish is emitted when a test method has finished, whether
	// it passed, failed or was skipped.
	EventTestFinish EventKind = "test_finish"
	// EventTestSkipped is emitted for a skipped test method.
	EventTestSkipped EventKind = "test_skipped"
	// EventTestFailed is emitted for a failed test method. A test that
	// panicked also fails, and additionally emits EventTestPanicked.
	EventTestFailed EventKind = "test_failed"
	// EventTestPanicked is emitted when a test level lifecycle phase or
	// the test body panics.
	EventTestPanicked EventKind = "test_panicked"

	// EventSubtestStart is emitted right before a subtest created through
	// Suite.Run runs. Nested subtests are reported in execution order.
	EventSubtestStart EventKind = "subtest_start"
	// EventSubtestFinish is emitted when a subtest has finished.
	EventSubtestFinish EventKind = "subtest_finish"
	// EventSubtestSkipped is emitted for a skipped subtest.
	EventSubtestSkipped EventKind = "subtest_skipped"
	// EventSubtestFailed is emitted for a failed subtest. A subtest that
	// panicked also fails, and additionally emits EventSubtestPanicked.
	EventSubtestFailed EventKind = "subtest_failed"
	// EventSubtestPanicked is emitted when a subtest level lifecycle phase
	// or the subtest body panics.
	EventSubtestPanicked EventKind = "subtest_panicked"

	// EventPhaseStart is emitted when a lifecycle phase starts.
	EventPhaseStart EventKind = "phase_start"
	// EventPhaseFinish is emitted when a lifecycle phase finishes, even
	// when the phase panicked.
	EventPhaseFinish EventKind = "phase_finish"

	// EventStats is emitted last but one (before EventSuiteFinish) and
	// carries the aggregated statistics of the run.
	EventStats EventKind = "stats"
)

// LifecyclePhase identifies the suite lifecycle phase an event belongs to.
type LifecyclePhase string

const (
	// PhaseNone is used for events that are not tied to a lifecycle phase.
	PhaseNone LifecyclePhase = ""
	// PhaseSetupSuite is the SetupSuite call.
	PhaseSetupSuite LifecyclePhase = "setup_suite"
	// PhaseTearDownSuite is the TearDownSuite call.
	PhaseTearDownSuite LifecyclePhase = "teardown_suite"
	// PhaseSetupTest is the SetupTest call.
	PhaseSetupTest LifecyclePhase = "setup_test"
	// PhaseBeforeTest is the BeforeTest call.
	PhaseBeforeTest LifecyclePhase = "before_test"
	// PhaseTest is the test method body itself.
	PhaseTest LifecyclePhase = "test"
	// PhaseAfterTest is the AfterTest call.
	PhaseAfterTest LifecyclePhase = "after_test"
	// PhaseTearDownTest is the TearDownTest call.
	PhaseTearDownTest LifecyclePhase = "teardown_test"
	// PhaseSetupSubTest is the SetupSubTest call.
	PhaseSetupSubTest LifecyclePhase = "setup_subtest"
	// PhaseSubTest is the subtest body itself.
	PhaseSubTest LifecyclePhase = "subtest"
	// PhaseTearDownSubTest is the TearDownSubTest call.
	PhaseTearDownSubTest LifecyclePhase = "teardown_subtest"
)

// FilterDetail describes the filter that excluded a discovered method.
type FilterDetail struct {
	// Method is the name of the discovered test method.
	Method string `json:"method"`
	// Pattern is the regular expression (from -testify.m) that did not match.
	Pattern string `json:"pattern"`
}

// PanicDetail describes a recovered panic.
type PanicDetail struct {
	// Phase is the lifecycle phase that panicked.
	Phase LifecyclePhase `json:"phase"`
	// Value is the formatted panic value.
	Value string `json:"value"`
	// Stack is the captured stack trace at recovery time.
	Stack string `json:"stack,omitempty"`
}

// RunStats contains the aggregated statistics of one observed suite run.
type RunStats struct {
	Start    time.Time     `json:"start"`
	End      time.Time     `json:"end"`
	Duration time.Duration `json:"duration_ns"`

	// Discovered is the number of methods with a "Test" prefix found on
	// the suite, including methods excluded by the filter and methods
	// with an invalid signature.
	Discovered int `json:"discovered"`
	// Filtered is the number of discovered methods excluded by the
	// filter; it is a subset of Discovered.
	Filtered int `json:"filtered"`

	// TestsRun is the number of discovered test methods that were scheduled.
	TestsRun int `json:"tests_run"`
	// TestsPassed is the number of scheduled test methods that passed.
	TestsPassed int `json:"tests_passed"`
	// TestsFailed is the number of scheduled test methods that failed.
	TestsFailed int `json:"tests_failed"`
	// TestsSkipped is the number of scheduled test methods that were skipped.
	TestsSkipped int `json:"tests_skipped"`
	// TestsPanicked is the number of test scoped panics that were recovered.
	TestsPanicked int `json:"tests_panicked"`

	// SubtestsPassed is the number of subtests that passed.
	SubtestsPassed int `json:"subtests_passed"`
	// SubtestsFailed is the number of subtests that failed.
	SubtestsFailed int `json:"subtests_failed"`
	// SubtestsSkipped is the number of subtests that were skipped.
	SubtestsSkipped int `json:"subtests_skipped"`
	// SubtestsPanicked is the number of subtest scoped panics recovered.
	SubtestsPanicked int `json:"subtests_panicked"`
}

// Event is a single immutable record in the execution event stream.
//
// Events are delivered to the configured EventSink (if any) in execution
// order and are also retained, in order, in the ExecutionReport returned by
// RunObserved. Slices and pointer fields returned through ExecutionReport
// accessors are copies; callers cannot mutate the recorded stream.
type Event struct {
	// Sequence is the zero based position of the event within the run.
	Sequence int `json:"sequence"`
	// Time is when the event was recorded.
	Time time.Time `json:"time"`
	// Kind is the event kind.
	Kind EventKind `json:"kind"`

	// SuiteName is the suite type name, for example "SuiteTester".
	SuiteName string `json:"suite_name"`
	// SuitePath is the *testing.T name of the suite run, for example
	// "TestRunSuite/SuiteTester".
	SuitePath string `json:"suite_path"`
	// TestName is the test method name for test scoped events, if any.
	TestName string `json:"test_name,omitempty"`
	// TestPath is the full *testing.T path of the test or subtest the
	// event belongs to, for example
	// "TestRunSuite/SuiteTester/TestSubtest/first".
	TestPath string `json:"test_path,omitempty"`

	// Phase is the lifecycle phase the event belongs to, if any.
	Phase LifecyclePhase `json:"phase,omitempty"`

	// Duration is populated on finish events with the time elapsed between
	// the matching start and finish events.
	Duration time.Duration `json:"duration_ns,omitempty"`

	// Message carries optional human readable details, for example the
	// reason a discovered method signature is invalid.
	Message string `json:"message,omitempty"`

	// Filter is populated on EventTestFiltered events.
	Filter *FilterDetail `json:"filter,omitempty"`
	// Panic is populated on panic events.
	Panic *PanicDetail `json:"panic,omitempty"`
	// Stats is populated on the EventStats event.
	Stats *RunStats `json:"stats,omitempty"`
}

// EventSink receives execution events while an observed suite is running.
//
// ReceiveEvent is called synchronously, in execution order, once per event.
// Returning a non-nil error rejects the event: observation stops for the
// rest of the run (test execution is never affected) and the rejection is,
// after the run completes, returned by RunObserved as an *ObserverError.
//
// Implementations must not call t.Fail, t.Skip or runtime.Goexit and must
// not panic; doing so is undefined behavior.
type EventSink interface {
	ReceiveEvent(ctx context.Context, event Event) error
}

// ObserveOption configures RunObserved.
type ObserveOption func(*observerOptions)

type observerOptions struct {
	sink     EventSink
	capacity int
	registry *ReportRegistry
}

// WithEventSink forwards every recorded event to sink in addition to the
// in-memory ExecutionReport.
func WithEventSink(sink EventSink) ObserveOption {
	return func(o *observerOptions) {
		o.sink = sink
	}
}

// WithEventCapacity bounds the number of events retained for the
// ExecutionReport. When the capacity is exhausted, observation stops and
// RunObserved returns an *ObserverError wrapping ErrEventCapacityExhausted.
// A capacity of zero or less keeps the stream unbounded (the default).
func WithEventCapacity(capacity int) ObserveOption {
	return func(o *observerOptions) {
		o.capacity = capacity
	}
}

// WithReportRegistry registers the sealed ExecutionReport in registry once
// the run completes, so that it can later be queried by its suite path.
func WithReportRegistry(registry *ReportRegistry) ObserveOption {
	return func(o *observerOptions) {
		o.registry = registry
	}
}

// Observer operation tags used in ObserverError.Op.
const (
	// OpEventSinkRejected tags an error caused by an EventSink rejection.
	OpEventSinkRejected = "event_sink_rejected"
	// OpEventCapacityExhausted tags an error caused by a full event buffer.
	OpEventCapacityExhausted = "event_capacity_exhausted"
	// OpContextCanceled tags an error caused by the observation context
	// being canceled or reaching its deadline.
	OpContextCanceled = "context_canceled"
	// OpReportExport tags an error while exporting a report.
	OpReportExport = "report_export"
)

// Sentinel errors reported through *ObserverError. They are matched with
// errors.Is.
var (
	// ErrEventSinkRejected is returned when an EventSink rejects an event.
	ErrEventSinkRejected = errors.New("testify/suite: event sink rejected an event")
	// ErrEventCapacityExhausted is returned when the retained event stream
	// reaches its configured capacity.
	ErrEventCapacityExhausted = errors.New("testify/suite: event capacity exhausted")
	// ErrObserverContextCanceled is returned when the observation context
	// is canceled or expires.
	ErrObserverContextCanceled = errors.New("testify/suite: observation context canceled")
	// ErrReportExport is returned when exporting a report fails.
	ErrReportExport = errors.New("testify/suite: report export failed")
)

// ObserverError is returned by RunObserved and by report export methods when
// the reporting pipeline fails. It never indicates a test failure or panic:
// test results are always preserved on *testing.T regardless of observer
// errors.
type ObserverError struct {
	// Op is one of the Op* constants.
	Op string
	// Sequence is the event sequence number the error happened at, or -1
	// when it is not tied to an event.
	Sequence int
	// EventKind is the kind of event being delivered, if any.
	EventKind EventKind
	// SuitePath is the suite path the error happened in.
	SuitePath string
	// TestPath is the test or subtest path the error happened at, if any.
	TestPath string
	// Phase is the lifecycle phase the error happened in, if any.
	Phase LifecyclePhase
	// Cause is the underlying error (the EventSink error, the context
	// error, the encoder or writer error, ...).
	Cause error
}

func (e *ObserverError) Error() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "testify/suite: observer %s", e.Op)
	if e.Sequence >= 0 {
		fmt.Fprintf(&b, " at event #%d (kind=%q)", e.Sequence, string(e.EventKind))
	}
	if e.SuitePath != "" {
		fmt.Fprintf(&b, " suite=%q", e.SuitePath)
	}
	if e.TestPath != "" {
		fmt.Fprintf(&b, " test=%q", e.TestPath)
	}
	if e.Phase != PhaseNone {
		fmt.Fprintf(&b, " phase=%q", string(e.Phase))
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, ": %v", e.Cause)
	}
	return b.String()
}

// Unwrap exposes Cause to errors.Is and errors.As.
func (e *ObserverError) Unwrap() error {
	return e.Cause
}

// Is lets errors.Is match the sentinel error corresponding to Op.
func (e *ObserverError) Is(target error) bool {
	switch target {
	case ErrEventSinkRejected:
		return e.Op == OpEventSinkRejected
	case ErrEventCapacityExhausted:
		return e.Op == OpEventCapacityExhausted
	case ErrObserverContextCanceled:
		return e.Op == OpContextCanceled
	case ErrReportExport:
		return e.Op == OpReportExport
	}
	return false
}

// observerCarrier is implemented by *Suite so that the package level run
// function can scope an observer to a single run, including nested runs of
// the same suite value. Custom TestingSuite implementations that do not
// embed Suite simply cannot carry subtest observations.
type observerCarrier interface {
	currentExecutionObserver() *executionObserver
	setExecutionObserver(*executionObserver)
}

// executionObserver collects the ordered event stream of one suite run.
//
// The suite package does not support parallel tests, and every suite run is
// driven from a single goroutine, but the mutex also guards the report
// against callers querying a shared registry while a run is finishing.
type executionObserver struct {
	ctx      context.Context
	sink     EventSink
	capacity int
	registry *ReportRegistry

	mu       sync.Mutex
	disabled bool
	sealed   bool
	firstErr *ObserverError

	sequence int
	events   []Event

	suiteName string
	suitePath string

	startTimes map[string]time.Time
	stats      RunStats
	start      time.Time
	end        time.Time

	report *ExecutionReport
}

func newExecutionObserver(ctx context.Context, opts ...ObserveOption) *executionObserver {
	cfg := observerOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &executionObserver{
		ctx:        ctx,
		sink:       cfg.sink,
		capacity:   cfg.capacity,
		registry:   cfg.registry,
		startTimes: make(map[string]time.Time),
	}
}

// failLocked records the first reporting error and disables further
// observation. Test execution itself is never interrupted.
func (o *executionObserver) failLocked(op string, e Event, cause error) {
	if o.disabled {
		return
	}
	o.disabled = true
	o.firstErr = &ObserverError{
		Op:        op,
		Sequence:  e.Sequence,
		EventKind: e.Kind,
		SuitePath: firstNonEmpty(e.SuitePath, o.suitePath),
		TestPath:  e.TestPath,
		Phase:     e.Phase,
		Cause:     cause,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// deliver records and forwards an event. The Sequence and Time are assigned
// here. Reporting failures disable the observer but never propagate.
func (o *executionObserver) deliver(e Event) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.disabled {
		return
	}
	if err := o.ctx.Err(); err != nil {
		e.Sequence = o.sequence
		if e.SuitePath == "" {
			e.SuitePath = o.suitePath
		}
		o.failLocked(OpContextCanceled, e, err)
		return
	}

	e.Sequence = o.sequence
	e.Time = time.Now()
	if e.SuitePath == "" {
		e.SuitePath = o.suitePath
	}

	if o.capacity > 0 && len(o.events) >= o.capacity {
		o.failLocked(OpEventCapacityExhausted, e, nil)
		return
	}

	o.countLocked(e)
	o.sequence++
	o.events = append(o.events, e)

	if o.sink != nil {
		if err := o.sink.ReceiveEvent(o.ctx, e); err != nil {
			o.failLocked(OpEventSinkRejected, e, err)
		}
	}
}

func (o *executionObserver) countLocked(e Event) {
	switch e.Kind {
	case EventTestDiscovered:
		o.stats.Discovered++
	case EventTestFiltered:
		// A filtered method was first discovered; Filtered is a subset
		// of Discovered, just like TestsPanicked is a subset of
		// TestsFailed.
		o.stats.Discovered++
		o.stats.Filtered++
	case EventTestStart:
		o.stats.TestsRun++
	case EventTestSkipped:
		o.stats.TestsSkipped++
	case EventTestFailed:
		o.stats.TestsFailed++
	case EventTestPanicked:
		o.stats.TestsPanicked++
	case EventSubtestSkipped:
		o.stats.SubtestsSkipped++
	case EventSubtestFailed:
		o.stats.SubtestsFailed++
	case EventSubtestPanicked:
		o.stats.SubtestsPanicked++
	}
}

func (o *executionObserver) newEvent(kind EventKind, t *testing.T, phase LifecyclePhase) Event {
	e := Event{
		Kind:      kind,
		SuiteName: o.suiteName,
		SuitePath: o.suitePath,
		Phase:     phase,
	}
	if t != nil {
		path := t.Name()
		e.TestPath = path
		// The test name is the path segment immediately below the suite
		// path, both for test methods and subtests.
		if rest := strings.TrimPrefix(path, o.suitePath+"/"); rest != path && rest != "" {
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				e.TestName = rest[:i]
			} else {
				e.TestName = rest
			}
		}
	}
	return e
}

// beginSuite marks the start of the run.
func (o *executionObserver) beginSuite(t *testing.T, suiteName string) {
	if o == nil {
		return
	}
	o.suiteName = suiteName
	o.suitePath = t.Name()
	o.start = time.Now()
	o.stats.Start = o.start
	o.deliver(Event{
		Kind:      EventSuiteStart,
		SuiteName: suiteName,
		SuitePath: o.suitePath,
	})
}

func (o *executionObserver) discovered(methodName, suitePath string, validSignature bool) {
	if o == nil {
		return
	}
	e := Event{
		Kind:      EventTestDiscovered,
		SuiteName: o.suiteName,
		SuitePath: o.suitePath,
		TestName:  methodName,
		TestPath:  joinPath(suitePath, methodName),
	}
	if !validSignature {
		e.Message = "test method has an invalid signature: expected no input or output parameters"
	}
	o.deliver(e)
}

func (o *executionObserver) filtered(methodName, suitePath, pattern string) {
	if o == nil {
		return
	}
	o.deliver(Event{
		Kind:      EventTestFiltered,
		SuiteName: o.suiteName,
		SuitePath: o.suitePath,
		TestName:  methodName,
		TestPath:  joinPath(suitePath, methodName),
		Filter: &FilterDetail{
			Method:  methodName,
			Pattern: pattern,
		},
	})
}

func joinPath(base, leaf string) string {
	if base == "" {
		return leaf
	}
	if leaf == "" {
		return base
	}
	return base + "/" + leaf
}

// beginTest marks the start of a scheduled test method.
func (o *executionObserver) beginTest(t *testing.T) {
	if o == nil {
		return
	}
	path := t.Name()
	o.mu.Lock()
	o.startTimes[path] = time.Now()
	o.mu.Unlock()
	o.deliver(o.newEvent(EventTestStart, t, PhaseTest))
}

// finishTest records the outcome of a scheduled test method.
func (o *executionObserver) finishTest(t *testing.T) {
	if o == nil {
		return
	}
	switch {
	case t.Skipped():
		o.deliver(o.newEvent(EventTestSkipped, t, PhaseTest))
	case t.Failed():
		o.deliver(o.newEvent(EventTestFailed, t, PhaseTest))
	default:
		o.mu.Lock()
		o.stats.TestsPassed++
		o.mu.Unlock()
	}
	e := o.newEvent(EventTestFinish, t, PhaseTest)
	e.Duration = o.elapsed(t.Name())
	o.deliver(e)
}

// beginSubtest marks the start of a subtest run through Suite.Run.
func (o *executionObserver) beginSubtest(t *testing.T) {
	if o == nil {
		return
	}
	path := t.Name()
	o.mu.Lock()
	o.startTimes[path] = time.Now()
	o.mu.Unlock()
	o.deliver(o.newEvent(EventSubtestStart, t, PhaseSubTest))
}

// finishSubtest records the outcome of a subtest.
func (o *executionObserver) finishSubtest(t *testing.T) {
	if o == nil {
		return
	}
	switch {
	case t.Skipped():
		o.deliver(o.newEvent(EventSubtestSkipped, t, PhaseSubTest))
	case t.Failed():
		o.deliver(o.newEvent(EventSubtestFailed, t, PhaseSubTest))
	default:
		o.mu.Lock()
		o.stats.SubtestsPassed++
		o.mu.Unlock()
	}
	e := o.newEvent(EventSubtestFinish, t, PhaseSubTest)
	e.Duration = o.elapsed(t.Name())
	o.deliver(e)
}

func (o *executionObserver) elapsed(path string) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	if start, ok := o.startTimes[path]; ok {
		return time.Since(start)
	}
	return 0
}

func (o *executionObserver) suiteSkipped(t *testing.T) {
	if o == nil {
		return
	}
	o.deliver(o.newEvent(EventSuiteSkipped, t, PhaseSetupSuite))
}

// phase runs fn as a lifecycle phase, delimiting it with phase events and
// reporting a recovered panic before re-panicking so that the suite's
// existing panic handling is preserved unchanged. It is a no-op wrapper
// when the observer is nil.
func (o *executionObserver) phase(t *testing.T, p LifecyclePhase, fn func()) {
	if o == nil {
		fn()
		return
	}
	o.phaseStart(t, p)
	defer o.phaseFinish(t, p)
	defer func() {
		if r := recover(); r != nil {
			o.panic(t, p, r)
			panic(r)
		}
	}()
	fn()
}

// panicGuard runs fn (a test or subtest body) and, on panic, records a
// panic event before re-panicking so the suite's existing panic handling is
// preserved unchanged. Unlike phase it does not emit phase start/finish
// events; the body is already delimited by test/subtest start and finish.
// It is a no-op wrapper when the observer is nil.
func (o *executionObserver) panicGuard(t *testing.T, p LifecyclePhase, fn func()) {
	if o == nil {
		fn()
		return
	}
	defer func() {
		if r := recover(); r != nil {
			o.panic(t, p, r)
			panic(r)
		}
	}()
	fn()
}

func (o *executionObserver) phaseStart(t *testing.T, p LifecyclePhase) {
	if o == nil {
		return
	}
	o.deliver(o.newEvent(EventPhaseStart, t, p))
}

func (o *executionObserver) phaseFinish(t *testing.T, p LifecyclePhase) {
	if o == nil {
		return
	}
	o.deliver(o.newEvent(EventPhaseFinish, t, p))
}

func panicEventKind(p LifecyclePhase) EventKind {
	switch p {
	case PhaseSetupSuite, PhaseTearDownSuite:
		return EventSuitePanicked
	case PhaseSetupSubTest, PhaseSubTest, PhaseTearDownSubTest:
		return EventSubtestPanicked
	default:
		return EventTestPanicked
	}
}

func (o *executionObserver) panic(t *testing.T, p LifecyclePhase, value interface{}) {
	if o == nil {
		return
	}
	e := o.newEvent(panicEventKind(p), t, p)
	e.Panic = &PanicDetail{
		Phase: p,
		Value: fmt.Sprint(value),
		Stack: string(debug.Stack()),
	}
	o.deliver(e)
}

// finishSuite emits the final statistics and suite finish events, seals the
// immutable report and registers it. It is safe to call multiple times; only
// the first call seals.
func (o *executionObserver) finishSuite(t *testing.T) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.sealed {
		o.mu.Unlock()
		return
	}
	o.sealed = true
	o.end = time.Now()
	o.stats.End = o.end
	if !o.start.IsZero() {
		o.stats.Duration = o.end.Sub(o.start)
	}

	statsCopy := o.stats
	statsEvent := Event{
		Kind:      EventStats,
		SuiteName: o.suiteName,
		SuitePath: o.suitePath,
		Stats:     &statsCopy,
	}
	finishEvent := Event{
		Kind:      EventSuiteFinish,
		SuiteName: o.suiteName,
		SuitePath: o.suitePath,
	}
	// Final events go through the same delivery pipeline so capacity,
	// sink and context failures are reported consistently.
	if !o.disabled {
		o.deliverLocked(&statsEvent)
		o.deliverLocked(&finishEvent)
	}

	events := make([]Event, len(o.events))
	copy(events, o.events)
	index := make(map[string][]int)
	for _, e := range events {
		if e.TestPath == "" {
			continue
		}
		index[e.TestPath] = append(index[e.TestPath], e.Sequence)
	}
	report := &ExecutionReport{
		suiteName: o.suiteName,
		suitePath: o.suitePath,
		start:     o.start,
		end:       o.end,
		stats:     o.stats,
		events:    events,
		byTest:    index,
	}
	o.report = report
	registry := o.registry
	o.mu.Unlock()

	if registry != nil {
		registry.add(report)
	}
}

// deliverLocked is the locked half of deliver used for the final events.
func (o *executionObserver) deliverLocked(e *Event) {
	if o.disabled {
		return
	}
	if err := o.ctx.Err(); err != nil {
		e.Sequence = o.sequence
		if e.SuitePath == "" {
			e.SuitePath = o.suitePath
		}
		o.failLocked(OpContextCanceled, *e, err)
		return
	}
	e.Sequence = o.sequence
	e.Time = time.Now()
	if e.SuitePath == "" {
		e.SuitePath = o.suitePath
	}
	if o.capacity > 0 && len(o.events) >= o.capacity {
		o.failLocked(OpEventCapacityExhausted, *e, nil)
		return
	}
	o.sequence++
	o.events = append(o.events, *e)
	if o.sink != nil {
		if err := o.sink.ReceiveEvent(o.ctx, *e); err != nil {
			o.failLocked(OpEventSinkRejected, *e, err)
		}
	}
}

// result returns the sealed report and the first reporting error, if any.
func (o *executionObserver) result() (*ExecutionReport, error) {
	o.mu.Lock()
	report := o.report
	var err error
	if o.firstErr != nil {
		// Return a copy so callers cannot mutate observer state.
		e := *o.firstErr
		err = &e
	}
	o.mu.Unlock()
	return report, err
}

// RunObserved runs a testing suite with structured execution observation
// enabled. It behaves exactly like Run as far as test discovery, the
// -testify.m filter, lifecycle hooks, failure reporting and statistics are
// concerned; observation never changes the result of the tests.
//
// The returned *ExecutionReport contains an immutable, execution ordered
// event stream covering discovery and filtering, lifecycle phases,
// subtests (including nested ones), skips, failures, panics and aggregate
// statistics. A non-nil error is an *ObserverError describing a reporting
// pipeline failure (sink rejection, exhausted capacity, canceled context or
// export failure), never a test failure or panic: test results are always
// preserved on *testing.T.
//
// Observation is opt-in: calling Run instead keeps the historical behavior.
//
// As with Run, a suite level t.Skip (for example from SetupSuite) or a
// recovered SetupSuite/TearDownSuite panic ends the calling test goroutine
// through runtime.Goexit, so the lines after RunObserved do not execute and
// the returned values cannot be observed at the call site. Configure
// WithEventSink or WithReportRegistry to retain the report in that case;
// the report is sealed while deferred functions are running.
func RunObserved(ctx context.Context, t *testing.T, suite TestingSuite, opts ...ObserveOption) (*ExecutionReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	obs := newExecutionObserver(ctx, opts...)
	run(t, suite, obs)
	report, err := obs.result()
	return report, err
}

// ExecutionReport is the immutable result of one observed suite run.
type ExecutionReport struct {
	suiteName string
	suitePath string
	start     time.Time
	end       time.Time
	stats     RunStats

	events []Event
	// byTest maps a full test path to the sequence numbers of its events.
	byTest map[string][]int
}

// SuiteName returns the suite type name of the run.
func (r *ExecutionReport) SuiteName() string { return r.suiteName }

// SuitePath returns the *testing.T path the suite ran under.
func (r *ExecutionReport) SuitePath() string { return r.suitePath }

// Start returns when the suite run started.
func (r *ExecutionReport) Start() time.Time { return r.start }

// End returns when the suite run ended.
func (r *ExecutionReport) End() time.Time { return r.end }

// Stats returns a copy of the aggregate run statistics.
func (r *ExecutionReport) Stats() RunStats { return r.stats }

// Events returns a copy of the recorded event stream, in execution order.
func (r *ExecutionReport) Events() []Event {
	events := make([]Event, len(r.events))
	for i, e := range r.events {
		events[i] = cloneEvent(e)
	}
	return events
}

// Event returns the event with the given sequence number.
func (r *ExecutionReport) Event(sequence int) (Event, bool) {
	for _, e := range r.events {
		if e.Sequence == sequence {
			return cloneEvent(e), true
		}
	}
	return Event{}, false
}

// EventsForTest returns the events recorded for the given full test path,
// in execution order. It returns nil for unknown paths.
func (r *ExecutionReport) EventsForTest(testPath string) []Event {
	sequences, ok := r.byTest[testPath]
	if !ok {
		return nil
	}
	events := make([]Event, 0, len(sequences))
	for _, seq := range sequences {
		if seq >= 0 && seq < len(r.events) {
			events = append(events, cloneEvent(r.events[seq]))
		}
	}
	return events
}

// TestPaths returns the test and subtest paths that produced events,
// ordered by the sequence number of their first event.
func (r *ExecutionReport) TestPaths() []string {
	paths := make([]string, 0, len(r.byTest))
	first := make(map[string]int, len(r.byTest))
	for path, sequences := range r.byTest {
		if len(sequences) > 0 {
			first[path] = sequences[0]
		}
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool {
		return first[paths[i]] < first[paths[j]]
	})
	return paths
}

func cloneEvent(e Event) Event {
	cp := e
	if e.Filter != nil {
		filter := *e.Filter
		cp.Filter = &filter
	}
	if e.Panic != nil {
		panicDetail := *e.Panic
		cp.Panic = &panicDetail
	}
	if e.Stats != nil {
		stats := *e.Stats
		cp.Stats = &stats
	}
	return cp
}

// WriteJSON exports the report as a JSON object to w. The events array is
// encoded event by event, so an encoding or writer failure is returned as
// an *ObserverError (OpReportExport) carrying the offending event sequence
// and test path alongside the suite path.
func (r *ExecutionReport) WriteJSON(w io.Writer) error {
	return writeJSONReports(w, []*ExecutionReport{r}, false)
}

// ReportRegistry keeps sealed ExecutionReports indexed by their suite path
// so callers can query or export results by suite path.
type ReportRegistry struct {
	mu      sync.Mutex
	reports map[string]*ExecutionReport
	order   []string
}

// NewReportRegistry creates an empty ReportRegistry.
func NewReportRegistry() *ReportRegistry {
	return &ReportRegistry{reports: make(map[string]*ExecutionReport)}
}

func (r *ReportRegistry) add(report *ExecutionReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.reports[report.suitePath]; !exists {
		r.order = append(r.order, report.suitePath)
	}
	r.reports[report.suitePath] = report
}

// ReportForSuite returns the report recorded under suitePath, if any.
func (r *ReportRegistry) ReportForSuite(suitePath string) (*ExecutionReport, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	report, ok := r.reports[suitePath]
	return report, ok
}

// SuitePaths returns the registered suite paths in registration order.
func (r *ReportRegistry) SuitePaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	paths := make([]string, len(r.order))
	copy(paths, r.order)
	return paths
}

// WriteJSON exports every registered report as a JSON object with a
// "reports" array. Export failures are returned as *ObserverError.
func (r *ReportRegistry) WriteJSON(w io.Writer) error {
	r.mu.Lock()
	reports := make([]*ExecutionReport, 0, len(r.order))
	for _, path := range r.order {
		reports = append(reports, r.reports[path])
	}
	r.mu.Unlock()
	// Registries always emit the envelope, even with a single report.
	return writeJSONReports(w, reports, true)
}

// writeJSONReports streams one report object, or {"reports":[...]}, to w.
// Every event is marshaled separately so failures can be attributed to an
// event sequence and test path.
func writeJSONReports(w io.Writer, reports []*ExecutionReport, wrap bool) error {
	if wrap {
		if err := writeChunk(w, []byte(`{"reports":[`), nil); err != nil {
			return err
		}
	}
	for i, report := range reports {
		if wrap && i > 0 {
			if err := writeChunk(w, []byte(`,`), nil); err != nil {
				return err
			}
		}
		if err := writeOneJSONReport(w, report); err != nil {
			return err
		}
	}
	if wrap {
		if err := writeChunk(w, []byte(`]}`), nil); err != nil {
			return err
		}
	}
	return nil
}

func writeOneJSONReport(w io.Writer, r *ExecutionReport) error {
	// Marshal every event up front so encoding errors pinpoint the event.
	rawEvents := make([]json.RawMessage, len(r.events))
	for i, e := range r.events {
		raw, err := json.Marshal(e)
		if err != nil {
			return &ObserverError{
				Op:        OpReportExport,
				Sequence:  e.Sequence,
				EventKind: e.Kind,
				SuitePath: r.suitePath,
				TestPath:  e.TestPath,
				Phase:     e.Phase,
				Cause:     err,
			}
		}
		rawEvents[i] = raw
	}

	if err := writeChunk(w, []byte(`{"suite_name":`), nil); err != nil {
		return exportWriteError(r, nil, err)
	}
	if err := writeJSONValue(w, r.suiteName); err != nil {
		return exportWriteError(r, nil, err)
	}
	if err := writeChunk(w, []byte(`,"suite_path":`), nil); err != nil {
		return exportWriteError(r, nil, err)
	}
	if err := writeJSONValue(w, r.suitePath); err != nil {
		return exportWriteError(r, nil, err)
	}
	if err := writeChunk(w, []byte(`,"start":`), nil); err != nil {
		return exportWriteError(r, nil, err)
	}
	if err := writeJSONValue(w, r.start); err != nil {
		return exportWriteError(r, nil, err)
	}
	if err := writeChunk(w, []byte(`,"end":`), nil); err != nil {
		return exportWriteError(r, nil, err)
	}
	if err := writeJSONValue(w, r.end); err != nil {
		return exportWriteError(r, nil, err)
	}
	if err := writeChunk(w, []byte(`,"stats":`), nil); err != nil {
		return exportWriteError(r, nil, err)
	}
	if err := writeJSONValue(w, r.stats); err != nil {
		return exportWriteError(r, nil, err)
	}
	if err := writeChunk(w, []byte(`,"events":[`), nil); err != nil {
		return exportWriteError(r, nil, err)
	}
	for i, raw := range rawEvents {
		if i > 0 {
			if err := writeChunk(w, []byte(`,`), eventRef(r, i)); err != nil {
				return exportWriteError(r, eventRef(r, i), err)
			}
		}
		if err := writeChunk(w, raw, eventRef(r, i)); err != nil {
			return exportWriteError(r, eventRef(r, i), err)
		}
	}
	if err := writeChunk(w, []byte(`]}`), nil); err != nil {
		return exportWriteError(r, nil, err)
	}
	return nil
}

// eventRef returns the event currently being written, or nil for report
// level chunks.
func eventRef(r *ExecutionReport, index int) *Event {
	if index < 0 || index >= len(r.events) {
		return nil
	}
	e := r.events[index]
	return &e
}

func exportWriteError(r *ExecutionReport, e *Event, cause error) error {
	oe := &ObserverError{
		Op:        OpReportExport,
		Sequence:  -1,
		SuitePath: r.suitePath,
		Cause:     cause,
	}
	if e != nil {
		oe.Sequence = e.Sequence
		oe.EventKind = e.Kind
		oe.TestPath = e.TestPath
		oe.Phase = e.Phase
	}
	return oe
}

func writeJSONValue(w io.Writer, value interface{}) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeChunk(w, raw, nil)
}

func writeChunk(w io.Writer, chunk []byte, _ *Event) error {
	_, err := w.Write(chunk)
	return err
}
