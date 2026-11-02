package mock

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stretchr/testify/assert"
)

// CallSessionFormat is the versioned format identifier of the call session
// record produced by this package. It is embedded in every record and every
// structured session error.
const CallSessionFormat = "com.github.stretchr.testify.call-session/v1"

// defaultMaxEvents bounds a recording session when no explicit limit is
// provided.
const defaultMaxEvents = 10000

// sessionController is the attachment point stored on every Mock. It is
// implemented by recording sessions and replay sessions.
type sessionController interface {
	sessionVersionInfo() SessionVersion
	dispatch(m *Mock, methodName string, arguments []interface{}) Arguments
}

/*
	Versioning and structured errors
*/

// SessionVersion identifies a session record format and a concrete session
// lineage. ID is unique per session start; Revision increments every time an
// interrupted record is resumed.
type SessionVersion struct {
	// Format is the versioned record format identifier.
	Format string `json:"format"`
	// ID uniquely identifies a session lineage.
	ID uint64 `json:"id"`
	// Revision is 1 for a freshly started session and increments on resume.
	Revision int `json:"revision"`
}

// String renders the version as "format#id/r<revision>".
func (v SessionVersion) String() string {
	return fmt.Sprintf("%s#%d/r%d", v.Format, v.ID, v.Revision)
}

// SessionErrorKind enumerates the structured failure modes a call session can
// report.
type SessionErrorKind string

const (
	// SessionErrorTruncation means a bounded recording session ran out of
	// event slots.
	SessionErrorTruncation SessionErrorKind = "record_truncated"
	// SessionErrorCanceled means the session has been canceled and can no
	// longer record or commit.
	SessionErrorCanceled SessionErrorKind = "session_canceled"
	// SessionErrorExpectationModified means an expectation was reconfigured
	// (On, Return, Times, Run, NotBefore, Unset, ...) after the session froze
	// the expectation graph.
	SessionErrorExpectationModified SessionErrorKind = "expectation_modified"
	// SessionErrorConcurrentSession means a Mock is being used by two
	// overlapping sessions, or a call was routed into a session it does not
	// belong to.
	SessionErrorConcurrentSession SessionErrorKind = "concurrent_session"
	// SessionErrorReplayDivergence means an incoming replay call did not
	// match the next recorded event, or recorded calls were missing/extra at
	// finish.
	SessionErrorReplayDivergence SessionErrorKind = "replay_divergence"
	// SessionErrorRecoveryInvalid means an interrupted record cannot be
	// resumed (sealed, tampered, chain break, or expectation mismatch).
	SessionErrorRecoveryInvalid SessionErrorKind = "recovery_point_invalid"
	// SessionErrorCleanupFailed means detaching one or more mocks during
	// session tear down found foreign session state.
	SessionErrorCleanupFailed SessionErrorKind = "cleanup_failed"
	// SessionErrorIntegrity means an exported record failed format, version,
	// signature or chain validation.
	SessionErrorIntegrity SessionErrorKind = "record_integrity"
	// SessionErrorInvalidState means the session/record was used in a state
	// that does not allow the requested operation.
	SessionErrorInvalidState SessionErrorKind = "invalid_session_state"
	// SessionErrorUnsupportedValue means an event argument or return value
	// cannot be encoded into an exported record (functions, channels, ...).
	SessionErrorUnsupportedValue SessionErrorKind = "unsupported_value"
)

// Sentinel errors, one per failure kind, for errors.Is/errors.As callers.
var (
	ErrCallSessionTruncation          = errors.New("mock: call session record truncated")
	ErrCallSessionCanceled            = errors.New("mock: call session canceled")
	ErrCallSessionExpectationModified = errors.New("mock: call session expectation modified")
	ErrCallSessionConcurrent          = errors.New("mock: call session concurrent session crossing")
	ErrCallSessionReplayDivergence    = errors.New("mock: call session replay divergence")
	ErrCallSessionRecoveryInvalid     = errors.New("mock: call session recovery point invalid")
	ErrCallSessionCleanupFailed       = errors.New("mock: call session cleanup failed")
	ErrCallSessionIntegrity           = errors.New("mock: call session record integrity check failed")
	ErrCallSessionInvalidState        = errors.New("mock: call session invalid state")
	ErrCallSessionUnsupportedValue    = errors.New("mock: call session unsupported value")
)

func sentinelForKind(kind SessionErrorKind) error {
	switch kind {
	case SessionErrorTruncation:
		return ErrCallSessionTruncation
	case SessionErrorCanceled:
		return ErrCallSessionCanceled
	case SessionErrorExpectationModified:
		return ErrCallSessionExpectationModified
	case SessionErrorConcurrentSession:
		return ErrCallSessionConcurrent
	case SessionErrorReplayDivergence:
		return ErrCallSessionReplayDivergence
	case SessionErrorRecoveryInvalid:
		return ErrCallSessionRecoveryInvalid
	case SessionErrorCleanupFailed:
		return ErrCallSessionCleanupFailed
	case SessionErrorIntegrity:
		return ErrCallSessionIntegrity
	case SessionErrorUnsupportedValue:
		return ErrCallSessionUnsupportedValue
	default:
		return ErrCallSessionInvalidState
	}
}

// SessionError is the structured error returned for every session failure. It
// always carries the session version and the event position at which the
// failure was detected (1-based; 0 when the failure is not tied to an event).
type SessionError struct {
	// Kind classifies the failure.
	Kind SessionErrorKind
	// Operation during which the failure occurred ("start", "record",
	// "commit", "cancel", "checkpoint", "resume", "replay", "finish",
	// "export", "import", "verify", "cleanup").
	Op string
	// Version of the session that reported the failure.
	Version SessionVersion
	// EventPosition is the 1-based global call sequence at which the failure
	// was detected, or 0 when unrelated to an event.
	EventPosition int
	// MockIndex is the frozen index of the involved mock, or -1.
	MockIndex int
	// MockName is the frozen name of the involved mock.
	MockName string
	// Detail is a human-readable explanation.
	Detail string
}

// Error implements error.
func (e *SessionError) Error() string {
	return fmt.Sprintf(
		"mock: call session %s: %s during %q at event %d (mock index %d %q): %s",
		e.Version.String(), e.Kind, e.Op, e.EventPosition, e.MockIndex, e.MockName, e.Detail,
	)
}

// Unwrap exposes the sentinel matching the error kind for errors.Is.
func (e *SessionError) Unwrap() error {
	return sentinelForKind(e.Kind)
}

func newSessionError(kind SessionErrorKind, op string, version SessionVersion, position, mockIndex int, mockName, detail string) *SessionError {
	return &SessionError{
		Kind:          kind,
		Op:            op,
		Version:       version,
		EventPosition: position,
		MockIndex:     mockIndex,
		MockName:      mockName,
		Detail:        detail,
	}
}

var globalSessionCounter uint64

func nextSessionID() uint64 {
	n := atomic.AddUint64(&globalSessionCounter, 1)
	return uint64(time.Now().UnixNano()) + n
}

// sessionRegistryMu serializes session construction (freeze expectation
// graphs and attach the sessions) so two sessions starting concurrently can
// never both attach to overlapping mocks. It is not held while sessions
// dispatch calls.
var sessionRegistryMu sync.Mutex

/*
	Public configuration
*/

// CallSessionConfig configures a recording call session.
type CallSessionConfig struct {
	// Mocks are the mock objects participating in the session. Each entry is
	// either a *mock.Mock or a pointer to a struct embedding mock.Mock (or
	// *mock.Mock). Mocks are frozen in slice order; the index/name derived
	// here identifies mocks in the record and must be reproduced at
	// replay/resume time.
	Mocks []interface{}
	// Names optionally assigns stable names to the mocks, in the same order
	// as Mocks. Empty entries default to "mock#<index>".
	Names []string
	// MaxEvents bounds the number of recorded call events (including nested
	// calls). Zero means the package default.
	MaxEvents int
}

/*
	Recorded data
*/

// CallEvent is one recorded mock call.
type CallEvent struct {
	// Seq is the 1-based global sequence of the call across all session
	// mocks, in the order calls started.
	Seq int
	// MockIndex is the frozen index of the mock that received the call.
	MockIndex int
	// MockName is the frozen name of the mock.
	MockName string
	// Method is the name of the called method.
	Method string
	// ExpectationIndex is the index of the matched expectation inside the
	// frozen expectation graph of the mock.
	ExpectationIndex int
	// ArgsSummary is a bounded human-readable summary of the arguments.
	ArgsSummary string
	// ParentSeq is the Seq of the call during which this call happened, or 0
	// for top-level calls.
	ParentSeq int
	// ViaRun reports whether this call happened inside the Run callback of
	// its parent. Such events are generated by mock side effects, not by the
	// code under test, and are suppressed (never re-executed) during replay.
	ViaRun bool
	// Depth is the nesting depth (1 for top-level calls).
	Depth int
	// HadRun reports whether the matched expectation had a Run callback.
	HadRun bool
	// RanRun reports whether the Run callback actually executed while
	// recording.
	RanRun bool
	// Panic is the recorded panic value rendered as text; empty if no panic.
	Panic string
	// PanicSource is "panic" for an expectation panic, "run" for a panic
	// raised by the Run callback, empty otherwise.
	PanicSource string
	// ErrorIndex is the index of the first non-nil error return argument, or
	// -1 when none of the return arguments is an error.
	ErrorIndex int

	// verification data (unexported)
	argsDigest string
	retDigest  string
	chainHash  string
	// hasReturns marks events that reached the return-capture phase. It
	// distinguishes "the call returned no values" (Return() with zero args,
	// legitimate nil returns) from "the record lost its live values" (an
	// exported record without a decodable payload). Expectation-panic events
	// never reach that phase and stay false. payloadPresent proves the
	// captured returns are available either live (in-process) or via a
	// decoded export payload; a nil returns slice on its own is legitimate
	// for Return() with zero arguments.
	hasReturns     bool
	payloadPresent bool

	// live values, retained for in-process replay; dropped by Export unless
	// gob-encodable.
	args    []interface{}
	returns Arguments
}

// MockInfo describes a frozen mock.
type MockInfo struct {
	Index     int      `json:"index"`
	Name      string   `json:"name"`
	OwnerType string   `json:"ownerType"`
	Methods   []string `json:"methods"`
}

/*
	Frozen expectation graph
*/

type frozenMock struct {
	index int
	name  string
	m     *Mock

	info    MockInfo
	version uint64

	expects   []*frozenExpectation
	callIndex map[*Call]int
}

type frozenExpectation struct {
	index     int
	mockIndex int
	method    string
	ptr       *Call

	fingerprint   string
	repeatability int
	optional      bool
	hasPanic      bool
	hasRun        bool
	hasWait       bool
	hasAfter      bool
	requires      [][2]int
}

// resolveSessionTarget extracts the *Mock and a description of the owning
// wrapper type from a user supplied target.
func resolveSessionTarget(v interface{}) (*Mock, string, map[string]bool, error) {
	if v == nil {
		return nil, "", nil, errors.New("nil mock target")
	}
	if m, ok := v.(*Mock); ok {
		if m == nil {
			return nil, "", nil, errors.New("nil *Mock target")
		}
		return m, reflect.TypeOf(m).String(), nil, nil
	}

	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return nil, "", nil, fmt.Errorf("target %T is neither a *mock.Mock nor a pointer to a struct embedding one", v)
	}
	ownerType := rv.Type().String()
	mockType := reflect.TypeOf(Mock{})

	var found reflect.Value
	queue := []reflect.Type{rv.Elem().Type()}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.Anonymous {
				continue
			}
			switch {
			case f.Type == mockType:
				fv := rv.Elem().Field(i)
				if fv.CanInterface() && fv.CanAddr() {
					found = fv
				}
			case f.Type.Kind() == reflect.Ptr && f.Type.Elem() == mockType:
				fv := rv.Elem().Field(i)
				if fv.CanInterface() {
					if fv.IsNil() && fv.CanSet() {
						fv.Set(reflect.ValueOf(&Mock{}))
					}
					found = fv
				}
			case f.Type.Kind() == reflect.Struct:
				queue = append(queue, f.Type)
			case f.Type.Kind() == reflect.Ptr && f.Type.Elem().Kind() == reflect.Struct:
				queue = append(queue, f.Type.Elem())
			}
			if found.IsValid() {
				break
			}
		}
		if found.IsValid() {
			break
		}
	}

	if !found.IsValid() {
		return nil, "", nil, fmt.Errorf("target %T does not embed mock.Mock", v)
	}

	var m *Mock
	if found.Kind() == reflect.Ptr {
		m, _ = found.Interface().(*Mock)
	} else {
		m, _ = found.Addr().Interface().(*Mock)
	}
	if m == nil {
		return nil, "", nil, fmt.Errorf("could not obtain *mock.Mock from target %T", v)
	}

	methods := make(map[string]bool)
	for i := 0; i < rv.Type().NumMethod(); i++ {
		methods[rv.Type().Method(i).Name] = true
	}
	return m, ownerType, methods, nil
}

func freezeMocks(targets []interface{}, names []string, version SessionVersion, op string) ([]*frozenMock, error) {
	if len(targets) == 0 {
		return nil, newSessionError(SessionErrorInvalidState, op, version, 0, -1, "", "at least one mock is required")
	}

	type resolved struct {
		m         *Mock
		ownerType string
		methods   map[string]bool
	}
	all := make([]resolved, len(targets))
	seen := make(map[*Mock]int)
	for i, t := range targets {
		m, ownerType, methods, err := resolveSessionTarget(t)
		if err != nil {
			return nil, newSessionError(SessionErrorInvalidState, op, version, 0, i, nameAt(names, i), err.Error())
		}
		if prev, ok := seen[m]; ok {
			return nil, newSessionError(SessionErrorConcurrentSession, op, version, 0, i, nameAt(names, i),
				fmt.Sprintf("mock is already registered at index %d; one mock cannot take part in a session twice", prev))
		}
		seen[m] = i
		all[i] = resolved{m: m, ownerType: ownerType, methods: methods}
	}

	fms := make([]*frozenMock, len(targets))
	for i, r := range all {
		fm := &frozenMock{
			index:     i,
			name:      nameAt(names, i),
			m:         r.m,
			callIndex: make(map[*Call]int),
			info: MockInfo{
				Index:     i,
				Name:      nameAt(names, i),
				OwnerType: r.ownerType,
			},
		}

		r.m.mutex.Lock()
		if r.m.session != nil {
			r.m.mutex.Unlock()
			return nil, newSessionError(SessionErrorConcurrentSession, op, version, 0, i, fm.name,
				"mock is already attached to another active session; finish or cancel that session first")
		}

		fm.version = r.m.expectVersion
		methodSet := make(map[string]bool)
		for ei, c := range r.m.ExpectedCalls {
			fm.callIndex[c] = ei
			methodSet[c.Method] = true
		}
		fms[i] = fm
		r.m.mutex.Unlock()

		for mname := range methodSet {
			// interface surface validation: the wrapper must expose every
			// method referenced by the frozen expectations.
			if r.methods != nil && !r.methods[mname] {
				return nil, newSessionError(SessionErrorInvalidState, op, version, 0, i, fm.name,
					fmt.Sprintf("mock owner type %s has no method %s referenced by an expectation", r.ownerType, mname))
			}
			fm.info.Methods = append(fm.info.Methods, mname)
		}
		sort.Strings(fm.info.Methods)
	}

	// resolve NotBefore requirement edges across all session mocks
	ptrIndex := make(map[*Call][2]int)
	for _, fm := range fms {
		for ei, c := range fm.m.ExpectedCalls {
			ptrIndex[c] = [2]int{fm.index, ei}
		}
	}
	for _, fm := range fms {
		for ei, c := range fm.m.ExpectedCalls {
			fe := &frozenExpectation{
				index:         ei,
				mockIndex:     fm.index,
				method:        c.Method,
				ptr:           c,
				repeatability: c.Repeatability,
				optional:      c.optional,
				hasPanic:      c.PanicMsg != nil,
				hasRun:        c.RunFn != nil,
				hasWait:       c.WaitFor != nil,
				hasAfter:      c.waitTime != 0,
			}
			for _, req := range c.requires {
				if idx, ok := ptrIndex[req]; ok {
					fe.requires = append(fe.requires, idx)
				}
				// Requirements pointing at mocks outside the session cannot
				// be versioned by index; they are still part of the textual
				// fingerprint below.
			}
			fe.fingerprint = fingerprintExpectation(c, ptrIndex)
			fm.expects = append(fm.expects, fe)
		}
	}

	return fms, nil
}

func nameAt(names []string, i int) string {
	if i < len(names) && names[i] != "" {
		return names[i]
	}
	return fmt.Sprintf("mock#%d", i)
}

// fingerprintExpectation produces a stable content fingerprint for a single
// expectation, including its cross-mock requirement edges.
func fingerprintExpectation(c *Call, ptrIndex map[*Call][2]int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "method=%s\nargs=%s\nreturns=%s\n", c.Method, digestValues(c.Arguments), digestValues(c.ReturnArguments))
	fmt.Fprintf(&b, "repeat=%d optional=%t panic=%t run=%t wait=%t after=%t\n",
		c.Repeatability, c.optional, c.PanicMsg != nil, c.RunFn != nil, c.WaitFor != nil, c.waitTime != 0)
	for i, req := range c.requires {
		if idx, ok := ptrIndex[req]; ok {
			fmt.Fprintf(&b, "req[%d]=m%d.e%d\n", i, idx[0], idx[1])
		} else {
			fmt.Fprintf(&b, "req[%d]=ext:%s:%s\n", i, req.Method, digestValues(req.Arguments))
		}
	}
	return digestString(b.String())
}

// graphHash produces the version hash of the full frozen expectation graph.
func graphHash(fms []*frozenMock) string {
	var b strings.Builder
	for _, fm := range fms {
		fmt.Fprintf(&b, "mock[%d] name=%q owner=%q methods=%v\n", fm.index, fm.name, fm.info.OwnerType, fm.info.Methods)
		for _, fe := range fm.expects {
			fmt.Fprintf(&b, "expect[%d] %s\n", fe.index, fe.fingerprint)
		}
	}
	return digestString(b.String())
}

/*
	Recording session
*/

type sessionState int

const (
	sessionRecording sessionState = iota
	sessionCommitted
	sessionCanceled
	sessionFailed
)

// CallSession is a versioned recording session. Start one with
// StartCallSession, run the code under test normally, then Commit, Cancel or
// Checkpoint it. A session must be closed (Commit, Cancel or Close) to detach
// its mocks.
type CallSession struct {
	version   SessionVersion
	maxEvents int

	mu      sync.Mutex
	state   sessionState
	fatal   *SessionError
	mocks   []*frozenMock
	byMock  map[*Mock]*frozenMock
	graphID string

	// events are kept in completion order; records expose them in Seq order.
	events  []*CallEvent
	nextSeq int
	started int
	open    map[int64][]*CallEvent

	committed *CallRecord
}

// sessionVersionInfo implements sessionController.
func (s *CallSession) sessionVersionInfo() SessionVersion { return s.version }

// Version returns the session version.
func (s *CallSession) Version() SessionVersion { return s.version }

// StartCallSession freezes the expectation graphs of the supplied mocks and
// starts a bounded recording session. The capability is opt-in: mocks without
// an active session keep their original behavior.
func StartCallSession(cfg CallSessionConfig) (*CallSession, error) {
	version := SessionVersion{Format: CallSessionFormat, ID: nextSessionID(), Revision: 1}
	max := cfg.MaxEvents
	if max <= 0 {
		max = defaultMaxEvents
	}

	sessionRegistryMu.Lock()
	defer sessionRegistryMu.Unlock()

	fms, err := freezeMocks(cfg.Mocks, cfg.Names, version, "start")
	if err != nil {
		return nil, err
	}

	s := &CallSession{
		version:   version,
		maxEvents: max,
		state:     sessionRecording,
		mocks:     fms,
		byMock:    make(map[*Mock]*frozenMock),
		open:      make(map[int64][]*CallEvent),
		nextSeq:   1,
		graphID:   graphHash(fms),
	}
	for _, fm := range fms {
		s.byMock[fm.m] = fm
		fm.m.mutex.Lock()
		fm.m.session = s
		fm.m.mutex.Unlock()
	}
	return s, nil
}

// ResumeCallSession continues a recording session from an interrupted
// (unsealed, committed via Checkpoint) record. The mocks must be supplied in
// the same order and must still expose an identical expectation graph.
func ResumeCallSession(rec *CallRecord, cfg CallSessionConfig) (*CallSession, error) {
	version := SessionVersion{Format: CallSessionFormat, ID: nextSessionID(), Revision: 1}
	if rec == nil {
		return nil, newSessionError(SessionErrorRecoveryInvalid, "resume", version, 0, -1, "", "nil record")
	}
	version = SessionVersion{Format: rec.Version.Format, ID: rec.Version.ID, Revision: rec.Version.Revision + 1}

	if rec.Version.Format != CallSessionFormat {
		return nil, newSessionError(SessionErrorRecoveryInvalid, "resume", version, 0, -1, "",
			"unsupported record format: "+rec.Version.Format)
	}
	if rec.Sealed {
		return nil, newSessionError(SessionErrorRecoveryInvalid, "resume", version, rec.Len(), -1, "",
			"cannot resume a sealed record; only interrupted (checkpoint) records are resumable")
	}
	if err := rec.Verify(); err != nil {
		se := asSessionError(err, SessionErrorRecoveryInvalid, "resume", version)
		return nil, se
	}

	max := cfg.MaxEvents
	if max <= 0 {
		max = defaultMaxEvents + rec.Len()
	}

	sessionRegistryMu.Lock()
	defer sessionRegistryMu.Unlock()

	fms, err := freezeMocks(cfg.Mocks, cfg.Names, version, "resume")
	if err != nil {
		return nil, err
	}
	if err := validateFrozenAgainstRecord(fms, rec, version, "resume", SessionErrorRecoveryInvalid); err != nil {
		detachSilently(fms, version)
		return nil, err
	}

	s := &CallSession{
		version:   version,
		maxEvents: max,
		state:     sessionRecording,
		mocks:     fms,
		byMock:    make(map[*Mock]*frozenMock),
		open:      make(map[int64][]*CallEvent),
		graphID:   rec.GraphHash,
	}
	for _, ev := range rec.Events {
		cp := *ev
		s.events = append(s.events, &cp)
	}
	s.nextSeq = rec.Len() + 1
	s.started = rec.Len()
	for _, fm := range fms {
		s.byMock[fm.m] = fm
		fm.m.mutex.Lock()
		fm.m.session = s
		fm.m.mutex.Unlock()
	}
	return s, nil
}

func detachSilently(fms []*frozenMock, version SessionVersion) {
	for _, fm := range fms {
		fm.m.mutex.Lock()
		if fm.m.session != nil && fm.m.session.sessionVersionInfo().ID == version.ID {
			fm.m.session = nil
		}
		fm.m.mutex.Unlock()
	}
}

// validateFrozenAgainstRecord checks mock identity, interface surface,
// expectation graph version and record/mock alignment.
func validateFrozenAgainstRecord(fms []*frozenMock, rec *CallRecord, version SessionVersion, op string, kind SessionErrorKind) error {
	if len(fms) != len(rec.Mocks) {
		return newSessionError(kind, op, version, 0, -1, "",
			fmt.Sprintf("record contains %d mock(s) but %d supplied", len(rec.Mocks), len(fms)))
	}
	for _, fm := range fms {
		mi := rec.Mocks[fm.index]
		if mi.Name != fm.name {
			return newSessionError(kind, op, version, 0, fm.index, fm.name,
				fmt.Sprintf("mock name mismatch: record has %q", mi.Name))
		}
		if mi.OwnerType != fm.info.OwnerType {
			return newSessionError(kind, op, version, 0, fm.index, fm.name,
				fmt.Sprintf("mock interface mismatch: record has owner type %q, replay supplies %q", mi.OwnerType, fm.info.OwnerType))
		}
		if len(mi.Methods) != len(fm.info.Methods) {
			return newSessionError(kind, op, version, 0, fm.index, fm.name,
				fmt.Sprintf("mock interface mismatch: record methods %v, supplied %v", mi.Methods, fm.info.Methods))
		}
		for i := range mi.Methods {
			if mi.Methods[i] != fm.info.Methods[i] {
				return newSessionError(kind, op, version, 0, fm.index, fm.name,
					fmt.Sprintf("mock interface mismatch: record methods %v, supplied %v", mi.Methods, fm.info.Methods))
			}
		}

		// expectation version: count and per-expectation fingerprints
		if len(fm.expects) != countRecordExpectations(rec, fm.index) {
			return newSessionError(SessionErrorExpectationModified, op, version, 0, fm.index, fm.name,
				"expectation graph version mismatch: expectation count differs from the frozen record")
		}
		for _, fe := range fm.expects {
			rf := findRecordExpectation(rec, fm.index, fe.index)
			if rf == nil || rf.Fingerprint != fe.fingerprint {
				return newSessionError(SessionErrorExpectationModified, op, version, 0, fm.index, fm.name,
					fmt.Sprintf("expectation graph version mismatch at expectation %d (%s)", fe.index, fe.method))
			}
		}
	}
	if graphHash(fms) != rec.GraphHash {
		return newSessionError(SessionErrorExpectationModified, op, version, 0, -1, "",
			"expectation graph version mismatch: graph hash differs from the frozen record")
	}
	return nil
}

/*
	recording dispatch
*/

// dispatch is invoked by Mock.MethodCalled while a session is attached.
func (s *CallSession) dispatch(m *Mock, methodName string, arguments []interface{}) Arguments {
	s.mu.Lock()
	if s.state != sessionRecording || s.fatal != nil {
		s.mu.Unlock()
		return m.methodCalledLegacy(methodName, arguments...)
	}
	fm := s.byMock[m]
	if fm == nil {
		err := newSessionError(SessionErrorConcurrentSession, "record", s.version, s.nextSeq, -1, "",
			"call routed into a session that does not own this mock")
		s.setStateLocked(sessionFailed, err)
		s.mu.Unlock()
		s.detachAllReporting(err)
		return m.methodCalledLegacy(methodName, arguments...)
	}
	s.mu.Unlock()

	// Phase 1: match the expectation exactly like the legacy path and detect
	// expectation graph mutation. The mock mutex is held here.
	m.mutex.Lock()
	if m.expectVersion != fm.version {
		err := newSessionError(SessionErrorExpectationModified, "record", s.version, s.nextSeq, fm.index, fm.name,
			fmt.Sprintf("expectations of %s were modified after the session froze the expectation graph", fm.name))
		m.mutex.Unlock()
		s.abort(err)
		return m.methodCalledLegacy(methodName, arguments...)
	}

	found, call := m.findExpectedCall(methodName, arguments...)
	if found < 0 {
		// Delegate to the legacy path so its exact failure messages and
		// FailNow/panic semantics are preserved.
		m.mutex.Unlock()
		return m.methodCalledLegacy(methodName, arguments...)
	}
	for _, requirement := range call.requires {
		if satisfied, _ := requirement.Parent.checkExpectation(requirement); !satisfied {
			m.mutex.Unlock()
			return m.methodCalledLegacy(methodName, arguments...)
		}
	}
	expectIdx := fm.callIndex[call]

	// Phase 2: allocate the event and its nesting slot. Lock order is always
	// mock mutex -> session mutex in dispatch.
	gid := goroutineID()
	s.mu.Lock()
	if s.state != sessionRecording || s.fatal != nil {
		s.mu.Unlock()
		m.mutex.Unlock()
		return m.methodCalledLegacy(methodName, arguments...)
	}
	if s.started >= s.maxEvents {
		err := newSessionError(SessionErrorTruncation, "record", s.version, s.nextSeq, fm.index, fm.name,
			fmt.Sprintf("recording bound of %d event(s) reached; subsequent calls are not recorded", s.maxEvents))
		s.setStateLocked(sessionFailed, err)
		s.mu.Unlock()
		m.mutex.Unlock()
		s.detachAllReporting(err)
		return m.methodCalledLegacy(methodName, arguments...)
	}

	parentSeq, depth := 0, 1
	if stack := s.open[gid]; len(stack) > 0 {
		parentSeq = stack[len(stack)-1].Seq
		depth = len(stack) + 1
	}
	ev := &CallEvent{
		Seq:              s.nextSeq,
		MockIndex:        fm.index,
		MockName:         fm.name,
		Method:           methodName,
		ExpectationIndex: expectIdx,
		ArgsSummary:      summarizeArguments(arguments),
		argsDigest:       digestValues(arguments),
		args:             append([]interface{}{}, arguments...),
		ParentSeq:        parentSeq,
		ViaRun:           parentSeq != 0,
		Depth:            depth,
		ErrorIndex:       -1,
	}
	s.nextSeq++
	s.started++
	s.open[gid] = append(s.open[gid], ev)
	s.mu.Unlock()

	defer s.popOpen(gid, ev.Seq)

	// Phase 3: apply the same expectation state transitions as the legacy
	// path (repeatability, call counter, recorded Calls slice).
	if call.Repeatability == 1 {
		call.Repeatability = -1
	} else if call.Repeatability > 1 {
		call.Repeatability--
	}
	call.totalCalls++
	m.Calls = append(m.Calls, *newCall(m, methodName, assert.CallerInfo(), arguments, call.ReturnArguments))
	m.mutex.Unlock()

	// Phase 4: blocking (honored while recording exactly like legacy).
	m.mutex.Lock()
	waitFor := call.WaitFor
	waitTime := call.waitTime
	m.mutex.Unlock()
	if waitFor != nil {
		<-waitFor
	} else {
		time.Sleep(waitTime)
	}

	// Phase 5: expectation panic.
	m.mutex.Lock()
	panicMsg := call.PanicMsg
	m.mutex.Unlock()
	if panicMsg != nil {
		ev.Panic = *panicMsg
		ev.PanicSource = "panic"
		s.completeEvent(ev)
		panic(*panicMsg)
	}

	// Phase 6: Run callback. It executes while recording (the event nesting
	// slot is open, so calls it makes are attributed as children), and any
	// panic it raises is captured and re-raised unchanged.
	m.mutex.Lock()
	runFn := call.RunFn
	m.mutex.Unlock()
	ev.HadRun = runFn != nil
	if runFn != nil {
		ev.RanRun = true
		func() {
			defer func() {
				if r := recover(); r != nil {
					ev.Panic = fmt.Sprint(r)
					ev.PanicSource = "run"
					s.completeEvent(ev)
					panic(r)
				}
			}()
			runFn(arguments)
		}()
	}

	// Phase 7: capture return arguments.
	m.mutex.Lock()
	returnArgs := call.ReturnArguments
	m.mutex.Unlock()

	ev.hasReturns = true
	ev.payloadPresent = true
	ev.returns = returnArgs
	ev.retDigest = digestValues(returnArgs)
	ev.ErrorIndex = firstErrorIndex(returnArgs)
	s.completeEvent(ev)

	return returnArgs
}

func firstErrorIndex(args Arguments) int {
	for i, a := range args {
		if err, ok := a.(error); ok && err != nil {
			return i
		}
	}
	return -1
}

func (s *CallSession) popOpen(gid int64, seq int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.open[gid]
	if n := len(st); n > 0 && st[n-1].Seq == seq {
		s.open[gid] = st[:n-1]
	} else {
		for i := len(st) - 1; i >= 0; i-- {
			if st[i].Seq == seq {
				s.open[gid] = append(st[:i], st[i+1:]...)
				break
			}
		}
	}
	if len(s.open[gid]) == 0 {
		delete(s.open, gid)
	}
}

func (s *CallSession) completeEvent(ev *CallEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == sessionRecording && s.fatal == nil {
		s.events = append(s.events, ev)
	}
}

func (s *CallSession) setStateLocked(state sessionState, err *SessionError) {
	s.state = state
	s.fatal = err
}

func (s *CallSession) abort(err *SessionError) {
	s.mu.Lock()
	if s.state == sessionRecording {
		s.setStateLocked(sessionFailed, err)
	}
	s.mu.Unlock()
	s.detachAllReporting(err)
}

// detachAllReporting detaches this session from every mock it owns. If a mock
// is found attached to foreign session state it is left untouched and a
// cleanup failure is reported; the method never leaves partial attachment of
// mocks this session still owns.
func (s *CallSession) detachAllReporting(report *SessionError) *SessionError {
	var cleanupErr *SessionError
	for _, fm := range s.mocks {
		fm.m.mutex.Lock()
		switch {
		case fm.m.session == nil:
			// already detached
		case fm.m.session == s:
			fm.m.session = nil
		default:
			if cleanupErr == nil {
				cleanupErr = newSessionError(SessionErrorCleanupFailed, "cleanup", s.version, 0, fm.index, fm.name,
					"mock was attached to a different session during tear down; left that session untouched")
			}
		}
		fm.m.mutex.Unlock()
	}
	if cleanupErr != nil {
		s.mu.Lock()
		if s.state == sessionRecording {
			s.setStateLocked(sessionFailed, cleanupErr)
		}
		s.mu.Unlock()
		if report != nil && report.Kind == SessionErrorCleanupFailed {
			return cleanupErr
		}
	}
	return report
}

/*
	Commit / Cancel / Checkpoint / Close / inspection
*/

// Commit finalizes the recording, verifies that no expectation was modified,
// builds a sealed, integrity-protected record and detaches all mocks. Calling
// Commit again returns the same record and error; it is idempotent.
func (s *CallSession) Commit() (*CallRecord, error) {
	s.mu.Lock()
	switch s.state {
	case sessionCommitted:
		rec, fatal := s.committed, s.fatal
		s.mu.Unlock()
		if fatal != nil {
			return rec, fatal
		}
		return rec, nil
	case sessionCanceled:
		err := s.fatal
		s.mu.Unlock()
		return nil, err
	case sessionFailed:
		err := s.fatal
		s.mu.Unlock()
		s.detachAllReporting(err)
		return nil, err
	}
	version := s.version
	mocks := s.mocks
	s.mu.Unlock()

	// Expectation mutation check across every frozen mock.
	for _, fm := range mocks {
		fm.m.mutex.Lock()
		v := fm.m.expectVersion
		fm.m.mutex.Unlock()
		if v != fm.version {
			err := newSessionError(SessionErrorExpectationModified, "commit", version, s.currentPosition(), fm.index, fm.name,
				"expectations were modified during the recording session")
			s.abort(err)
			return nil, err
		}
	}

	// Calls still in flight cannot be committed.
	if open := s.openCount(); open > 0 {
		err := newSessionError(SessionErrorInvalidState, "commit", version, s.currentPosition(), -1, "",
			fmt.Sprintf("%d call event(s) are still in flight; commit after they return", open))
		s.abort(err)
		return nil, err
	}

	rec := s.buildRecord(true)

	s.mu.Lock()
	// Detach first, then mark committed; cleanup failure must win over the
	// record because the session must never leave partial state.
	s.mu.Unlock()

	if cleanupErr := s.detachMocksOnce(version); cleanupErr != nil {
		s.mu.Lock()
		s.setStateLocked(sessionFailed, cleanupErr)
		s.mu.Unlock()
		return nil, cleanupErr
	}

	s.mu.Lock()
	s.committed = rec
	s.setStateLocked(sessionCommitted, nil)
	s.mu.Unlock()
	return rec, nil
}

func (s *CallSession) detachMocksOnce(version SessionVersion) *SessionError {
	var cleanupErr *SessionError
	for _, fm := range s.mocks {
		fm.m.mutex.Lock()
		switch {
		case fm.m.session == nil:
		case fm.m.session == s:
			fm.m.session = nil
		default:
			if cleanupErr == nil {
				cleanupErr = newSessionError(SessionErrorCleanupFailed, "cleanup", version, 0, fm.index, fm.name,
					"mock was attached to a different session during commit; left that session untouched")
			}
		}
		fm.m.mutex.Unlock()
	}
	return cleanupErr
}

func (s *CallSession) currentPosition() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextSeq - 1
}

func (s *CallSession) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, st := range s.open {
		n += len(st)
	}
	return n
}

// Cancel aborts the recording and detaches all mocks. The recorded events are
// discarded. Cancel is idempotent: every successful invocation returns nil,
// and using a canceled session afterwards returns the structured
// SessionErrorCanceled error. Canceling a committed session is an
// invalid-state error.
func (s *CallSession) Cancel() error {
	s.mu.Lock()
	switch s.state {
	case sessionCanceled:
		s.mu.Unlock()
		return nil
	case sessionCommitted:
		s.mu.Unlock()
		return newSessionError(SessionErrorInvalidState, "cancel", s.version, 0, -1, "",
			"cannot cancel a committed session")
	case sessionFailed:
		s.mu.Unlock()
		s.detachAllReporting(s.fatal)
		return nil
	}
	version := s.version
	s.setStateLocked(sessionCanceled, newSessionError(SessionErrorCanceled, "cancel", version, 0, -1, "",
		"session was canceled before commit"))
	s.mu.Unlock()

	s.detachMocksOnce(version)
	return nil
}

// Checkpoint exports an unsealed snapshot of the recording so far without
// ending the session. The snapshot can be resumed with ResumeCallSession after
// an interruption.
func (s *CallSession) Checkpoint() (*CallRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sessionRecording {
		if s.fatal != nil {
			return nil, s.fatal
		}
		return nil, newSessionError(SessionErrorInvalidState, "checkpoint", s.version, 0, -1, "",
			"session is not recording")
	}
	return s.buildRecordLocked(false), nil
}

// Close releases the session. An active recording is canceled; closed
// committed/finished sessions are no-ops.
func (s *CallSession) Close() error {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	switch state {
	case sessionRecording:
		return s.Cancel()
	default:
		return nil
	}
}

// Err returns the fatal structured error that aborted the session, if any.
func (s *CallSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatal
}

// Events returns a snapshot of recorded events in global sequence order.
func (s *CallSession) Events() []CallEvent {
	s.mu.Lock()
	rec := s.buildRecordLocked(false)
	s.mu.Unlock()
	out := make([]CallEvent, len(rec.Events))
	for i, ev := range rec.Events {
		out[i] = *ev
	}
	return out
}

// buildRecord locks the session and delegates to buildRecordLocked.
func (s *CallSession) buildRecord(sealed bool) *CallRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buildRecordLocked(sealed)
}

func (s *CallSession) buildRecordLocked(sealed bool) *CallRecord {
	infos := make([]MockInfo, len(s.mocks))
	for _, fm := range s.mocks {
		infos[fm.index] = fm.info
	}

	ordered := make([]*CallEvent, len(s.events))
	copy(ordered, s.events)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })
	// detach live order-independent payload references by copying events
	for i, ev := range ordered {
		cp := *ev
		ordered[i] = &cp
	}
	sealEventChain(ordered)

	var expects []ExpectationRecord
	for _, fm := range s.mocks {
		for _, fe := range fm.expects {
			expects = append(expects, ExpectationRecord{
				MockIndex:     fm.index,
				Index:         fe.index,
				Method:        fe.method,
				Repeatability: fe.repeatability,
				Optional:      fe.optional,
				HasPanic:      fe.hasPanic,
				HasRun:        fe.hasRun,
				HasWait:       fe.hasWait,
				HasAfter:      fe.hasAfter,
				Requires:      append([][2]int(nil), fe.requires...),
				Fingerprint:   fe.fingerprint,
			})
		}
	}

	rec := &CallRecord{
		Version:      s.version,
		Sealed:       sealed,
		CreatedAt:    time.Now().UTC(),
		Mocks:        infos,
		Expectations: expects,
		GraphHash:    s.graphID,
		Events:       ordered,
	}
	rec.recordHash = computeRecordHash(rec)
	return rec
}

func asSessionError(err error, kind SessionErrorKind, op string, version SessionVersion) *SessionError {
	var se *SessionError
	if errors.As(err, &se) {
		return newSessionError(kind, op, version, se.EventPosition, se.MockIndex, se.MockName, se.Detail)
	}
	return newSessionError(kind, op, version, 0, -1, "", err.Error())
}
