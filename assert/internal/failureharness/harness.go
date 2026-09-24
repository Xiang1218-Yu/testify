// Package failureharness contains user-code helpers used by the assert
// package's own failure renderer tests. It deliberately lives in a directory
// that is not named assert/mock/require, so that CallerInfo attributes
// failures to these files just like it would for real downstream callers.
package failureharness

import (
	"errors"
	"net/http"
	"time"

	"github.com/stretchr/testify/assert"
)

// File is the base name of this file, used by the tests in location checks.
const File = "harness.go"

// Equal triggers an assert.Equal failure from external user code.
func Equal(t assert.TestingT, expected, actual interface{}, msgAndArgs ...interface{}) bool {
	return assert.Equal(t, expected, actual, msgAndArgs...)
}

// True triggers an assert.True failure from external user code.
func True(t assert.TestingT, value bool, msgAndArgs ...interface{}) bool {
	return assert.True(t, value, msgAndArgs...)
}

// False triggers an assert.False failure from external user code.
func False(t assert.TestingT, value bool, msgAndArgs ...interface{}) bool {
	return assert.False(t, value, msgAndArgs...)
}

// Nil triggers an assert.Nil failure from external user code.
func Nil(t assert.TestingT, object interface{}, msgAndArgs ...interface{}) bool {
	return assert.Nil(t, object, msgAndArgs...)
}

// Len triggers an assert.Len failure from external user code.
func Len(t assert.TestingT, object interface{}, length int, msgAndArgs ...interface{}) bool {
	return assert.Len(t, object, length, msgAndArgs...)
}

// Fail triggers an assert.Fail from external user code.
func Fail(t assert.TestingT, failureMessage string, msgAndArgs ...interface{}) bool {
	return assert.Fail(t, failureMessage, msgAndArgs...)
}

// UnexpectedError triggers a NoError failure.
func UnexpectedError(t assert.TestingT) bool {
	return assert.NoError(t, errors.New("unexpected"))
}

// HelperCallingTrue is a user-code helper function that invokes an assertion,
// exercising nested call chains.
func HelperCallingTrue(t assert.TestingT) {
	assert.True(t, false)
}

// NestedTrueEqual evaluates an Equal inside a True so that both assertions
// are reported for one outer call.
func NestedTrueEqual(t assert.TestingT) bool {
	return assert.True(t, assert.Equal(t, "outer", "inner"))
}

// HTTPStatusCodeFailure triggers an HTTPStatusCode failure with structured
// expected/actual values.
func HTTPStatusCodeFailure(t assert.TestingT, msgAndArgs ...interface{}) bool {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return assert.HTTPStatusCode(t, handler, "GET", "/missing", nil, http.StatusOK, msgAndArgs...)
}

// EventuallyWithTFailure runs EventuallyWithT with a condition that always
// fails, exercising nested CollectT assertions.
func EventuallyWithTFailure(t assert.TestingT) bool {
	return assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.True(c, false)
		assert.Equal(c, 1, 2)
	}, 20*time.Millisecond, 5*time.Millisecond)
}

// UnserializableEqual triggers an Equal failure with channels, which
// encoding/json cannot serialize.
func UnserializableEqual(t assert.TestingT) bool {
	return assert.Equal(t, make(chan int), make(chan int))
}
