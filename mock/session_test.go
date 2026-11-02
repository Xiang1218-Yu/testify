package mock

import (
	"encoding/gob"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

/*
	Test doubles for the session feature.
*/

type sessSvc1 struct{ Mock }

func (s *sessSvc1) Do(x int) (int, error) {
	args := s.Called(x)
	return args.Int(0), args.Error(1)
}
func (s *sessSvc1) Trigger()                  { s.Called() }
func (s *sessSvc1) ChanCall(ch chan struct{}) { s.Called(ch) }

type sessSvc2 struct{ Mock }

func (s *sessSvc2) Work(b bool) string {
	args := s.Called(b)
	return args.String(0)
}
func (s *sessSvc2) Nested() { s.Called() }
func (s *sessSvc2) Fetch() (*sessPayload, error) {
	args := s.Called()
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*sessPayload), args.Error(1)
}

// sessPayload is a named, gob-encodable return value used by the export test.
type sessPayload struct {
	N    int
	Name string
}

func init() {
	gob.Register(&sessPayload{})
}

func newSessPair() (*sessSvc1, *sessSvc2) {
	return &sessSvc1{}, &sessSvc2{}
}

func startRecord(t *testing.T, max int, targets ...interface{}) *CallSession {
	t.Helper()
	s, err := StartCallSession(CallSessionConfig{Mocks: targets, MaxEvents: max})
	require.NoError(t, err)
	return s
}

func commitErr(s *CallSession) error {
	_, err := s.Commit()
	return err
}

func mustSessionError(t *testing.T, err error) *SessionError {
	t.Helper()
	var se *SessionError
	require.Error(t, err)
	require.True(t, errors.As(err, &se), "expected *SessionError, got %T: %v", err, err)
	return se
}

func assertDetached(t *testing.T, ms ...*Mock) {
	t.Helper()
	for _, m := range ms {
		m.mutex.Lock()
		attached := m.session != nil
		m.mutex.Unlock()
		assert.False(t, attached, "mock should be detached from the session")
	}
}

/*
	Default-off compatibility
*/

func TestCallSession_DisabledByDefault(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessPair()
	s1.On("Do", 1).Return(5, nil)
	s2.On("Work", true).Return("ok")

	s1.mutex.Lock()
	assert.Nil(t, s1.session)
	s1.mutex.Unlock()

	n, err := s1.Do(1)
	assert.Equal(t, 5, n)
	assert.NoError(t, err)
	assert.Equal(t, "ok", s2.Work(true))
	assert.True(t, s1.AssertExpectations(t))
	assert.True(t, s2.AssertExpectations(t))
}

/*
	Recording across mocks: sequence, args summary, matched expectation,
	returns, Run state, nesting.
*/

func TestCallSession_RecordsCrossMockSequenceAndNesting(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessPair()

	// Trigger on svc1 runs a callback that calls svc2 nested.
	s1.On("Trigger").Return().Run(func(args Arguments) {
		s2.Nested()
	})
	s2.On("Nested").Return()
	s1.On("Do", 7).Return(42, nil)

	s := startRecord(t, 0, s1, s2)

	s1.Trigger()
	v, err := s1.Do(7)
	require.NoError(t, err)
	require.Equal(t, 42, v)

	rec, err := s.Commit()
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.True(t, rec.IsSealed())
	require.NoError(t, rec.Verify())
	assert.Equal(t, 3, rec.Len())

	// event 1: top-level Trigger on mock#0, Run present and executed
	ev1 := rec.Event(0)
	assert.Equal(t, 1, ev1.Seq)
	assert.Equal(t, 0, ev1.MockIndex)
	assert.Equal(t, "mock#0", ev1.MockName)
	assert.Equal(t, "Trigger", ev1.Method)
	assert.Equal(t, 0, ev1.ExpectationIndex)
	assert.NotEmpty(t, ev1.ArgsSummary)
	assert.Equal(t, 0, ev1.ParentSeq)
	assert.Equal(t, 1, ev1.Depth)
	assert.True(t, ev1.HadRun)
	assert.True(t, ev1.RanRun)
	assert.Empty(t, ev1.Panic)

	// event 2: nested Nested on mock#1 attributed to the Run callback
	ev2 := rec.Event(1)
	assert.Equal(t, 2, ev2.Seq)
	assert.Equal(t, 1, ev2.MockIndex)
	assert.Equal(t, "mock#1", ev2.MockName)
	assert.Equal(t, "Nested", ev2.Method)
	assert.Equal(t, 1, ev2.ParentSeq)
	assert.Equal(t, 2, ev2.Depth)
	assert.True(t, ev2.ViaRun)
	assert.False(t, ev2.HadRun)

	// event 3: top-level Do, return value and error index recorded
	ev3 := rec.Event(2)
	assert.Equal(t, 3, ev3.Seq)
	assert.Equal(t, 0, ev3.MockIndex)
	assert.Equal(t, "Do", ev3.Method)
	assert.Equal(t, 1, ev3.ExpectationIndex)
	assert.Equal(t, 0, ev3.ParentSeq)
	require.Len(t, ev3.returns, 2)
	assert.Equal(t, 42, ev3.returns[0])
	assert.Nil(t, ev3.returns[1])
	assert.Equal(t, -1, ev3.ErrorIndex)

	assertDetached(t, &s1.Mock, &s2.Mock)
}

func TestCallSession_RecordsErrorAndPanicAndRunPanic(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	boom := errors.New("boom")
	s1.On("Do", 1).Return(0, boom)
	s1.On("Trigger").Return().Run(func(args Arguments) { panic("run exploded") })

	s := startRecord(t, 0, s1)
	_, err := s1.Do(1)
	assert.Same(t, boom, err)
	assert.PanicsWithValue(t, "run exploded", func() { s1.Trigger() })

	rec, err := s.Commit()
	require.NoError(t, err)
	require.Equal(t, 2, rec.Len())

	e1 := rec.Event(0)
	assert.Equal(t, 1, e1.ErrorIndex)
	assert.Equal(t, boom, e1.returns[1])

	e2 := rec.Event(1)
	assert.Equal(t, "run exploded", e2.Panic)
	assert.Equal(t, "run", e2.PanicSource)
	assert.True(t, e2.HadRun)
	assert.True(t, e2.RanRun)
}

func TestCallSession_RecordsExpectationPanic(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Trigger").Panic("panic-msg")
	s := startRecord(t, 0, s1)
	assert.PanicsWithValue(t, "panic-msg", func() { s1.Trigger() })
	rec, err := s.Commit()
	require.NoError(t, err)
	e := rec.Event(0)
	assert.Equal(t, "panic-msg", e.Panic)
	assert.Equal(t, "panic", e.PanicSource)
}

/*
	Bounded recording
*/

func TestCallSession_BoundedRecordingTruncates(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Trigger").Return()

	s := startRecord(t, 2, s1)
	s1.Trigger()
	s1.Trigger()
	// third event exceeds the bound: session aborts, call still executes
	s1.Trigger()

	err := s.Err()
	require.Error(t, err)
	se := mustSessionError(t, commitErr(s))
	assert.Equal(t, SessionErrorTruncation, se.Kind)
	assert.ErrorIs(t, se, ErrCallSessionTruncation)
	assert.Equal(t, 3, se.EventPosition)
	assert.Equal(t, CallSessionFormat, se.Version.Format)
	assert.NotZero(t, se.Version.ID)
	assertDetached(t, &s1.Mock)

	// the mock keeps working normally after abort
	s1.Trigger()
}

/*
	Cancel
*/

func TestCallSession_CancelIsIdempotentAndDetaches(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessPair()
	s1.On("Trigger").Return()
	s2.On("Nested").Return()

	s := startRecord(t, 0, s1, s2)
	s1.Trigger()

	require.NoError(t, s.Cancel())
	require.NoError(t, s.Cancel()) // idempotent

	se := mustSessionError(t, func() error { _, e := s.Commit(); return e }())
	assert.Equal(t, SessionErrorCanceled, se.Kind)
	assert.ErrorIs(t, se, ErrCallSessionCanceled)

	assertDetached(t, &s1.Mock, &s2.Mock)

	// no pollution: mocks behave normally, and new sessions can attach
	s2.Nested()
	s2b, err := StartCallSession(CallSessionConfig{Mocks: []interface{}{s2}})
	require.NoError(t, err)
	require.NoError(t, s2b.Cancel())
}

/*
	Expectation mutation
*/

func TestCallSession_ExpectationModifiedDuringRecord(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessPair()
	s1.On("Trigger").Return()
	s2.On("Nested").Return()

	s := startRecord(t, 0, s1, s2)
	s1.Trigger()

	// mutate the graph after freezing
	s2.On("Work", false).Return("later")

	// the next call aborts the session and falls back to normal behavior
	s2.Nested()

	se := mustSessionError(t, commitErr(s))
	assert.Equal(t, SessionErrorExpectationModified, se.Kind)
	assert.Equal(t, 1, se.MockIndex)
	assertDetached(t, &s1.Mock, &s2.Mock)

	// other mocks are not polluted
	_, err := StartCallSession(CallSessionConfig{Mocks: []interface{}{s1}})
	require.NoError(t, err)
}

func TestCallSession_ExpectationModifiedBeforeCommit(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Do", 1).Return(1, nil)
	s := startRecord(t, 0, s1)
	_, _ = s1.Do(1)
	s1.On("Trigger") // mutate between last call and commit
	se := mustSessionError(t, commitErr(s))
	assert.Equal(t, SessionErrorExpectationModified, se.Kind)
	assertDetached(t, &s1.Mock)
}

/*
	Concurrent sessions
*/

func TestCallSession_OverlappingSessionsRejected(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessPair()
	s1.On("Trigger").Return()

	first := startRecord(t, 0, s1)
	_, err := StartCallSession(CallSessionConfig{Mocks: []interface{}{s1, s2}})
	se := mustSessionError(t, err)
	assert.Equal(t, SessionErrorConcurrentSession, se.Kind)
	assert.Equal(t, 0, se.MockIndex)

	// first session is untouched and can finish cleanly
	s1.Trigger()
	rec, err := first.Commit()
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Len())

	// once detached, a new session can take over
	next := startRecord(t, 0, s2)
	require.NoError(t, next.Cancel())
}

func TestCallSession_DuplicateMockRejected(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	_, err := StartCallSession(CallSessionConfig{Mocks: []interface{}{s1, s1}})
	se := mustSessionError(t, err)
	assert.Equal(t, SessionErrorConcurrentSession, se.Kind)
}

/*
	Replay
*/

func configureRecordPair(s1 *sessSvc1, s2 *sessSvc2, sideEffect *int) {
	s1.On("Do", 1).Return(42, nil).Run(func(args Arguments) {
		*sideEffect++ // external side effect, must not replay
	})
	s2.On("Work", true).Return("ok")
}

func exercisePair(s1 *sessSvc1, s2 *sessSvc2) (int, string) {
	msg := s2.Work(true)
	n, _ := s1.Do(1)
	return n, msg
}

func TestCallSession_ReplayVerifiesAndSuppressesSideEffects(t *testing.T) {
	t.Parallel()

	// record
	s1, s2 := newSessPair()
	var sideEffects int
	configureRecordPair(s1, s2, &sideEffects)
	s := startRecord(t, 0, s1, s2)
	n, msg := exercisePair(s1, s2)
	require.Equal(t, 42, n)
	require.Equal(t, "ok", msg)
	require.Equal(t, 1, sideEffects)
	rec, err := s.Commit()
	require.NoError(t, err)

	// replay on fresh mocks configured identically
	r1, r2 := newSessPair()
	var replaySideEffects int
	configureRecordPair(r1, r2, &replaySideEffects)
	rep, err := StartReplay(rec, ReplayConfig{Mocks: []interface{}{r1, r2}})
	require.NoError(t, err)

	n2, msg2 := exercisePair(r1, r2)
	assert.Equal(t, 42, n2)
	assert.Equal(t, "ok", msg2)
	assert.Equal(t, 0, replaySideEffects, "Run callback side effects must not replay")

	require.NoError(t, rep.Finish())
	// idempotent replay finish
	require.NoError(t, rep.Finish())

	// expectation accounting was mirrored onto the live mocks
	assert.True(t, r1.AssertExpectations(t))
	assert.True(t, r2.AssertExpectations(t))
	assertDetached(t, &r1.Mock, &r2.Mock)
}

func TestCallSession_ReplayNestedRunChildIsSuppressed(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessPair()
	var nested int
	s1.On("Trigger").Return().Run(func(args Arguments) {
		nested++
		s2.Nested()
	})
	s2.On("Nested").Return()
	s := startRecord(t, 0, s1, s2)
	s1.Trigger()
	require.Equal(t, 1, nested)
	rec, err := s.Commit()
	require.NoError(t, err)
	assert.Equal(t, 2, rec.Len()) // parent + Run child recorded

	r1, r2 := newSessPair()
	var replayNested int
	r1.On("Trigger").Return().Run(func(args Arguments) {
		replayNested++
		r2.Nested()
	})
	r2.On("Nested").Return()
	rep, err := StartReplay(rec, ReplayConfig{Mocks: []interface{}{r1, r2}})
	require.NoError(t, err)

	r1.Trigger() // only the top-level event is expected
	assert.Equal(t, 0, replayNested, "nested Run callback must not replay")
	require.NoError(t, rep.Finish())
}

func TestCallSession_ReplayPanic(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Trigger").Panic("boom")
	s := startRecord(t, 0, s1)
	assert.PanicsWithValue(t, "boom", func() { s1.Trigger() })
	rec, err := s.Commit()
	require.NoError(t, err)

	r1, _ := newSessPair()
	r1.On("Trigger").Panic("boom")
	rep, err := StartReplay(rec, ReplayConfig{Mocks: []interface{}{r1}})
	require.NoError(t, err)
	assert.PanicsWithValue(t, "boom", func() { r1.Trigger() })
	require.NoError(t, rep.Finish())
}

func TestCallSession_ReplayDoesNotWait(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	recordCh := make(chan time.Time, 1)
	s1.On("Trigger").WaitUntil(recordCh).Return()
	s := startRecord(t, 0, s1)
	done := make(chan struct{})
	go func() { s1.Trigger(); close(done) }()
	recordCh <- time.Now()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recording blocked on WaitUntil")
	}
	rec, err := s.Commit()
	require.NoError(t, err)

	// replay: a channel that never fires must not block the replayed call
	r1, _ := newSessPair()
	never := make(chan time.Time)
	r1.On("Trigger").WaitUntil(never).Return()
	rep, err := StartReplay(rec, ReplayConfig{Mocks: []interface{}{r1}})
	require.NoError(t, err)
	finished := make(chan struct{})
	go func() { r1.Trigger(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("replay blocked on the recorded WaitUntil channel")
	}
	require.NoError(t, rep.Finish())
}

func TestCallSession_ReplayDivergenceWrongArguments(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Do", 1).Return(42, nil)
	s := startRecord(t, 0, s1)
	_, _ = s1.Do(1)
	rec, err := s.Commit()
	require.NoError(t, err)

	r1, _ := newSessPair()
	r1.On("Do", 1).Return(42, nil)
	rep, err := StartReplay(rec, ReplayConfig{Mocks: []interface{}{r1}})
	require.NoError(t, err)

	func() {
		defer func() {
			rec2 := recover()
			se, ok := rec2.(*SessionError)
			require.True(t, ok, "expected structured SessionError panic, got %v", rec2)
			assert.Equal(t, SessionErrorReplayDivergence, se.Kind)
			assert.ErrorIs(t, se, ErrCallSessionReplayDivergence)
			assert.Equal(t, 1, se.EventPosition)
			assert.Contains(t, se.Detail, "arguments")
		}()
		_, _ = r1.Do(2)
	}()

	se := mustSessionError(t, rep.Finish())
	assert.Equal(t, SessionErrorReplayDivergence, se.Kind)
	assertDetached(t, &r1.Mock)
}

func TestCallSession_ReplayDivergenceMissingCalls(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessPair()
	s1.On("Do", 1).Return(1, nil)
	s2.On("Work", true).Return("ok")
	s := startRecord(t, 0, s1, s2)
	_, _ = exercisePair(s1, s2)
	rec, err := s.Commit()
	require.NoError(t, err)

	r1, r2 := newSessPair()
	r1.On("Do", 1).Return(1, nil)
	r2.On("Work", true).Return("ok")
	rep, err := StartReplay(rec, ReplayConfig{Mocks: []interface{}{r1, r2}})
	require.NoError(t, err)

	// only the first event replayed; the second is missing
	_ = r2.Work(true)
	se := mustSessionError(t, rep.Finish())
	assert.Equal(t, SessionErrorReplayDivergence, se.Kind)
	assert.Equal(t, 2, se.EventPosition)
	assert.Contains(t, se.Detail, "never happened")
}

func TestCallSession_ReplayDivergenceExtraCall(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Do", 1).Return(1, nil)
	s := startRecord(t, 0, s1)
	_, _ = s1.Do(1)
	rec, err := s.Commit()
	require.NoError(t, err)

	r1, _ := newSessPair()
	r1.On("Do", 1).Return(1, nil)
	rep, err := StartReplay(rec, ReplayConfig{Mocks: []interface{}{r1}})
	require.NoError(t, err)
	defer rep.Close()
	_, _ = r1.Do(1)

	func() {
		defer func() {
			se := recover().(*SessionError)
			assert.Equal(t, SessionErrorReplayDivergence, se.Kind)
			assert.Contains(t, se.Detail, "extra call")
		}()
		_, _ = r1.Do(1)
	}()
	assertDetached(t, &r1.Mock)
}

func TestCallSession_ReplayDivergenceWrongOrderAcrossMocks(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessPair()
	s2.On("Work", true).Return("ok")
	s1.On("Do", 1).Return(1, nil).NotBefore(s2.On("Work", true))
	s := startRecord(t, 0, s1, s2)
	_, _ = exercisePair(s1, s2) // Work then Do
	rec, err := s.Commit()
	require.NoError(t, err)

	r1, r2 := newSessPair()
	r2.On("Work", true).Return("ok")
	r1.On("Do", 1).Return(1, nil).NotBefore(r2.On("Work", true))
	rep, err := StartReplay(rec, ReplayConfig{Mocks: []interface{}{r1, r2}})
	require.NoError(t, err)
	defer rep.Close()

	// dependency order inverted: Do arrives first but Work was recorded first
	func() {
		defer func() {
			se := recover().(*SessionError)
			assert.Equal(t, SessionErrorReplayDivergence, se.Kind)
			assert.Equal(t, 1, se.EventPosition)
			assert.Contains(t, se.Detail, "wrong mock")
		}()
		_, _ = r1.Do(1)
	}()
	require.NoError(t, rep.Close())
	assertDetached(t, &r1.Mock, &r2.Mock)
}

func TestCallSession_ReplayDivergenceReportedThroughTestingT(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Do", 1).Return(1, nil)
	s := startRecord(t, 0, s1)
	_, _ = s1.Do(1)
	rec, err := s.Commit()
	require.NoError(t, err)

	r1, _ := newSessPair()
	r1.On("Do", 1).Return(1, nil)
	tt := &MockTestingT{}
	rep, err := StartReplay(rec, ReplayConfig{Mocks: []interface{}{r1}, T: tt})
	require.NoError(t, err)
	assert.PanicsWithValue(t, mockTestingTFailNowCalled, func() { _, _ = r1.Do(2) })
	assert.Equal(t, 1, tt.errorfCount)
	assert.Equal(t, 1, tt.failNowCount)
	require.NoError(t, rep.Close())
	assertDetached(t, &r1.Mock)
}

func TestCallSession_ReplayRejectsUnsealedAndMismatchedMocks(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessPair()
	s1.On("Do", 1).Return(1, nil)
	s2.On("Work", true).Return("ok")
	s := startRecord(t, 0, s1, s2)
	_, _ = s1.Do(1)
	cp, err := s.Checkpoint()
	require.NoError(t, err)
	require.NoError(t, s.Cancel())

	// unsealed records cannot be replayed
	_, err = StartReplay(cp, ReplayConfig{Mocks: []interface{}{s1, s2}})
	se := mustSessionError(t, err)
	assert.Equal(t, SessionErrorInvalidState, se.Kind)

	// sealed record, wrong number of mocks
	s1b, s2b := newSessPair()
	s1b.On("Do", 1).Return(1, nil)
	s2b.On("Work", true).Return("ok")
	sA := startRecord(t, 0, s1b, s2b)
	_, _ = s1b.Do(1)
	_ = s2b.Work(true)
	rec, err := sA.Commit()
	require.NoError(t, err)

	only1 := &sessSvc1{}
	only1.On("Do", 1).Return(1, nil)
	_, err = StartReplay(rec, ReplayConfig{Mocks: []interface{}{only1}})
	se = mustSessionError(t, err)
	assert.Equal(t, SessionErrorReplayDivergence, se.Kind)
	assert.Contains(t, se.Detail, "record contains 2 mock")

	// different owner type for the same slot
	other1 := &sessSvc2{}
	other1.On("Work", true).Return("ok")
	other2 := &sessSvc2{}
	other2.On("Work", true).Return("ok")
	_, err = StartReplay(rec, ReplayConfig{Mocks: []interface{}{other1, other2}})
	se = mustSessionError(t, err)
	assert.Equal(t, SessionErrorReplayDivergence, se.Kind)
	assert.Contains(t, se.Detail, "owner type")

	// changed return value -> expectation graph version mismatch
	d1, d2 := newSessPair()
	d1.On("Do", 1).Return(2, nil) // different return
	d2.On("Work", true).Return("ok")
	_, err = StartReplay(rec, ReplayConfig{Mocks: []interface{}{d1, d2}})
	se = mustSessionError(t, err)
	assert.Equal(t, SessionErrorExpectationModified, se.Kind)
	assert.Contains(t, se.Detail, "expectation graph version mismatch")
}

/*
	Export / import
*/

func TestCallSession_ExportImportRoundTripAndReplay(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessPair()
	s1.On("Do", 1).Return(9, nil)
	s2.On("Work", true).Return("hello")
	s2.On("Fetch").Return(&sessPayload{N: 3, Name: "x"}, nil)
	s := startRecord(t, 0, s1, s2)
	_, _ = s1.Do(1)
	_ = s2.Work(true)
	got, ferr := s2.Fetch()
	require.NoError(t, ferr)
	require.Equal(t, &sessPayload{N: 3, Name: "x"}, got)
	rec, err := s.Commit()
	require.NoError(t, err)

	data, err := rec.Export()
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(data), CallSessionFormat))

	imp, err := ImportCallRecord(data)
	require.NoError(t, err)
	require.NoError(t, imp.Verify())
	assert.Equal(t, rec.Hash(), imp.Hash())
	assert.Equal(t, 3, imp.Len())
	assert.Equal(t, &sessPayload{N: 3, Name: "x"}, imp.Event(2).returns[0])

	// replay from the imported record
	r1, r2 := newSessPair()
	r1.On("Do", 1).Return(9, nil)
	r2.On("Work", true).Return("hello")
	r2.On("Fetch").Return(&sessPayload{N: 3, Name: "x"}, nil)
	rep, err := StartReplay(imp, ReplayConfig{Mocks: []interface{}{r1, r2}})
	require.NoError(t, err)
	n, _ := r1.Do(1)
	assert.Equal(t, 9, n)
	assert.Equal(t, "hello", r2.Work(true))
	g2, ferr2 := r2.Fetch()
	require.NoError(t, ferr2)
	assert.Equal(t, 3, g2.N)
	require.NoError(t, rep.Finish())
}

func TestCallSession_ImportRejectsTamperedRecord(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Do", 1).Return(1, nil)
	s := startRecord(t, 0, s1)
	_, _ = s1.Do(1)
	rec, err := s.Commit()
	require.NoError(t, err)
	data, err := rec.Export()
	require.NoError(t, err)

	// tamper with the JSON payload (MarshalIndent writes "key": value)
	tampered := []byte(strings.Replace(string(data), `"seq": 1`, `"seq": 9`, 1))
	require.NotEqual(t, data, tampered)
	_, err = ImportCallRecord(tampered)
	se := mustSessionError(t, err)
	assert.Equal(t, SessionErrorIntegrity, se.Kind)
	assert.ErrorIs(t, se, ErrCallSessionIntegrity)
	assert.Equal(t, 1, se.EventPosition)

	// unsupported format
	bad := []byte(strings.Replace(string(data), CallSessionFormat, "bogus/v9", 1))
	_, err = ImportCallRecord(bad)
	se = mustSessionError(t, err)
	assert.Equal(t, SessionErrorIntegrity, se.Kind)

	// garbage
	_, err = ImportCallRecord([]byte("{not json"))
	se = mustSessionError(t, err)
	assert.Equal(t, SessionErrorIntegrity, se.Kind)
}

func TestCallSession_ExportRejectsUnsupportedValues(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	ch := make(chan struct{})
	s1.On("Do", 1).Return(1, nil)
	// channel argument cannot be gob encoded; the expectation is registered
	// before the session freezes the graph
	s1.On("ChanCall", ch).Return()
	s := startRecord(t, 0, s1)
	s1.MethodCalled("Do", 1)        // event 1: normal
	s1.MethodCalled("ChanCall", ch) // event 2: carries a channel
	rec, err := s.Commit()
	require.NoError(t, err)
	_, err = rec.Export()
	se := mustSessionError(t, err)
	assert.Equal(t, SessionErrorUnsupportedValue, se.Kind)
	assert.Equal(t, 2, se.EventPosition)
}

/*
	Recovery from an interrupted record
*/

func TestCallSession_ResumeFromCheckpointContinuesChain(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Do", 1).Return(1, nil)
	s1.On("Do", 2).Return(2, nil)
	s := startRecord(t, 10, s1)
	_, _ = s1.Do(1)
	cp, err := s.Checkpoint()
	require.NoError(t, err)
	assert.False(t, cp.IsSealed())
	require.NoError(t, cp.Verify())
	require.Equal(t, 1, cp.Len())
	require.NoError(t, s.Cancel()) // simulated interruption

	// export/import the interrupted record
	data, err := cp.Export()
	require.NoError(t, err)
	restored, err := ImportCallRecord(data)
	require.NoError(t, err)

	// resume against fresh mocks with the same expectation graph
	r1, _ := newSessPair()
	r1.On("Do", 1).Return(1, nil)
	r1.On("Do", 2).Return(2, nil)
	rs, err := ResumeCallSession(restored, CallSessionConfig{Mocks: []interface{}{r1}, MaxEvents: 10})
	require.NoError(t, err)
	assert.Equal(t, cp.Version.ID, rs.Version().ID)
	assert.Equal(t, cp.Version.Revision+1, rs.Version().Revision)

	_, _ = r1.Do(2)
	rec2, err := rs.Commit()
	require.NoError(t, err)
	assert.True(t, rec2.IsSealed())
	require.NoError(t, rec2.Verify())
	require.Equal(t, 2, rec2.Len())
	assert.Equal(t, 1, rec2.Event(0).Seq)
	assert.Equal(t, 2, rec2.Event(1).Seq)
	assertDetached(t, &r1.Mock)

	// resumed record is fully replayable on a third set of mocks
	p1, _ := newSessPair()
	p1.On("Do", 1).Return(1, nil)
	p1.On("Do", 2).Return(2, nil)
	rep, err := StartReplay(rec2, ReplayConfig{Mocks: []interface{}{p1}})
	require.NoError(t, err)
	a, _ := p1.Do(1)
	b, _ := p1.Do(2)
	assert.Equal(t, 1, a)
	assert.Equal(t, 2, b)
	require.NoError(t, rep.Finish())
}

func TestCallSession_ResumeRejectsInvalidRecoveryPoints(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Do", 1).Return(1, nil)
	s := startRecord(t, 0, s1)
	_, _ = s1.Do(1)
	cp, err := s.Checkpoint()
	require.NoError(t, err)
	require.NoError(t, s.Cancel())

	// sealed records cannot be resumed
	sealed, err := s.commitSealedCopyForTest(cp)
	require.NoError(t, err)
	_, err = ResumeCallSession(sealed, CallSessionConfig{Mocks: []interface{}{s1}})
	se := mustSessionError(t, err)
	assert.Equal(t, SessionErrorRecoveryInvalid, se.Kind)

	// graph changed: a fresh expectation was added
	r1, _ := newSessPair()
	r1.On("Do", 1).Return(1, nil)
	r1.On("Trigger").Return()
	_, err = ResumeCallSession(cp, CallSessionConfig{Mocks: []interface{}{r1}})
	se = mustSessionError(t, err)
	assert.Equal(t, SessionErrorRecoveryInvalid, se.Kind)

	// tampered checkpoint fails recovery with a chain/integrity error
	data, derr := cp.Export()
	require.NoError(t, derr)
	tampered := []byte(strings.Replace(string(data), `"ret":`, `"ret":"x",`, 1))
	bad, ierr := ImportCallRecord(tampered)
	if ierr == nil {
		// import itself caught it; otherwise resume must
		d1, _ := newSessPair()
		d1.On("Do", 1).Return(1, nil)
		_, err = ResumeCallSession(bad, CallSessionConfig{Mocks: []interface{}{d1}})
		se = mustSessionError(t, err)
		assert.Equal(t, SessionErrorRecoveryInvalid, se.Kind)
	} else {
		se = mustSessionError(t, ierr)
		assert.Equal(t, SessionErrorIntegrity, se.Kind)
	}
}

// commitSealedCopyForTest re-seals an in-memory checkpoint for the
// "cannot resume a sealed record" test.
func (s *CallSession) commitSealedCopyForTest(cp *CallRecord) (*CallRecord, error) {
	cp.Sealed = true
	sealEventChain(cp.Events)
	cp.recordHash = computeRecordHash(cp)
	return cp, cp.Verify()
}

/*
	Cleanup failure leaves foreign sessions intact
*/

type fakeForeignSession struct{ version SessionVersion }

func (f *fakeForeignSession) sessionVersionInfo() SessionVersion              { return f.version }
func (f *fakeForeignSession) dispatch(*Mock, string, []interface{}) Arguments { return nil }

func TestCallSession_CleanupFailureKeepsForeignSessionIntact(t *testing.T) {
	t.Parallel()
	s1, s2 := newSessPair()
	s1.On("Trigger").Return()
	s2.On("Nested").Return()
	s := startRecord(t, 0, s1, s2)
	s1.Trigger()

	// simulate another session taking over mock#0 concurrently
	foreign := &fakeForeignSession{version: SessionVersion{Format: CallSessionFormat, ID: 999, Revision: 1}}
	s1.mutex.Lock()
	s1.session = foreign
	s1.mutex.Unlock()

	se := mustSessionError(t, commitErr(s))
	assert.Equal(t, SessionErrorCleanupFailed, se.Kind)
	assert.Equal(t, 0, se.MockIndex)

	// the foreign session is untouched, our other mock is detached
	s1.mutex.Lock()
	assert.Equal(t, sessionController(foreign), s1.session)
	s1.mutex.Unlock()
	assertDetached(t, &s2.Mock)

	// repeated commit is idempotent and still reports the failure
	se2 := mustSessionError(t, func() error { _, e := s.Commit(); return e }())
	assert.Equal(t, SessionErrorCleanupFailed, se2.Kind)
}

/*
	Commit idempotency and Checkpoint liveness
*/

func TestCallSession_CommitIdempotent(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Do", 1).Return(1, nil)
	s := startRecord(t, 0, s1)
	_, _ = s1.Do(1)
	rec1, err := s.Commit()
	require.NoError(t, err)
	rec2, err := s.Commit()
	require.NoError(t, err)
	assert.Same(t, rec1, rec2)
	assert.Equal(t, rec1.Hash(), rec2.Hash())

	// cancel after commit is an invalid-state error
	se := mustSessionError(t, s.Cancel())
	assert.Equal(t, SessionErrorInvalidState, se.Kind)
}

func TestCallSession_CheckpointDoesNotEndSession(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Do", 1).Return(1, nil)
	s1.On("Do", 2).Return(2, nil)
	s := startRecord(t, 0, s1)
	_, _ = s1.Do(1)
	cp, err := s.Checkpoint()
	require.NoError(t, err)
	assert.Equal(t, 1, cp.Len())

	// session keeps recording after the checkpoint
	_, _ = s1.Do(2)
	rec, err := s.Commit()
	require.NoError(t, err)
	assert.Equal(t, 2, rec.Len())
	require.NoError(t, rec.Verify())
}

/*
	Concurrency under -race
*/

func TestCallSession_ConcurrentCallsAreRecorded(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Trigger").Return()
	s := startRecord(t, 0, s1)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s1.Trigger() }()
	}
	wg.Wait()

	rec, err := s.Commit()
	require.NoError(t, err)
	assert.Equal(t, 20, rec.Len())
	seqs := map[int]bool{}
	for _, ev := range rec.Events {
		assert.False(t, seqs[ev.Seq], "duplicate seq")
		seqs[ev.Seq] = true
		assert.Equal(t, "Trigger", ev.Method)
		assert.Equal(t, 1, ev.Depth)
	}
}

func TestCallSession_ConcurrentOverlappingStartsRejected(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Trigger").Return()

	// a session is already attached and recording
	holder := startRecord(t, 0, s1)

	const n = 50
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	var rejected int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier
			if _, err := StartCallSession(CallSessionConfig{Mocks: []interface{}{s1}}); err != nil {
				atomic.AddInt32(&rejected, 1)
				return
			}
		}()
	}
	close(barrier)
	wg.Wait()

	assert.Equal(t, int32(n), atomic.LoadInt32(&rejected))

	// the holder session was not displaced and still records normally
	s1.Trigger()
	rec, err := holder.Commit()
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Len())
	assertDetached(t, &s1.Mock)

	// after release, fresh sessions can attach again
	next := startRecord(t, 0, s1)
	require.NoError(t, next.Cancel())

	// stress: racing start/cancel must never attach two sessions at once
	var (
		mu        sync.Mutex
		active    int
		maxActive int
	)
	var wg2 sync.WaitGroup
	for i := 0; i < n; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			s, err := StartCallSession(CallSessionConfig{Mocks: []interface{}{s1}})
			if err != nil {
				return
			}
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			_ = s.Cancel()
			mu.Lock()
			active--
			mu.Unlock()
		}()
	}
	wg2.Wait()
	assert.Equal(t, 1, maxActive, "two sessions were attached to the mock concurrently")
	assertDetached(t, &s1.Mock)
}

func TestCallSession_RecordedEventsAccessor(t *testing.T) {
	t.Parallel()
	s1, _ := newSessPair()
	s1.On("Trigger").Return()
	s := startRecord(t, 0, s1)
	s1.Trigger()
	events := s.Events()
	require.Len(t, events, 1)
	assert.Equal(t, "Trigger", events[0].Method)
	require.NoError(t, s.Cancel())
}

/*
	Disabled-mode overhead guard
*/

func BenchmarkMethodCalled_SessionDisabled(b *testing.B) {
	s1, _ := newSessPair()
	s1.On("Do", 1).Return(1, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s1.Do(1)
	}
}

func ExampleCallSession() {
	// two mocks participating in one versioned session
	var svcA, svcB sessSvc1
	svcA.On("Do", 1).Return(7, nil)
	svcB.On("Trigger").Return().Run(func(args Arguments) { fmt.Println("side effect while recording") })

	s, err := StartCallSession(CallSessionConfig{Mocks: []interface{}{&svcA, &svcB}})
	if err != nil {
		panic(err)
	}
	svcB.Trigger()
	_, _ = svcA.Do(1)
	rec, err := s.Commit()
	if err != nil {
		panic(err)
	}
	fmt.Println("events:", rec.Len(), "sealed:", rec.IsSealed())
	// Output:
	// side effect while recording
	// events: 2 sealed: true
}
