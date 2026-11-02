package mock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"
)

// maxSummaryLength bounds the textual argument summary stored on a recorded
// event. Full arguments are always represented by their digest regardless of
// the summary length, so truncation never weakens verification.
const maxSummaryLength = 1024

// digestString returns the hex encoded SHA-256 digest of a string.
func digestString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// digestValues returns a stable digest of a list of values. It is used both
// for call arguments and for return arguments, so replay can compare incoming
// values against the recording without keeping any pointer identity.
func digestValues(values []interface{}) string {
	var b strings.Builder
	canonicalValue(&b, reflect.ValueOf(values), nil, 0)
	return digestString(b.String())
}

// summarizeArguments produces a bounded, human-readable one-line summary of
// the call arguments.
func summarizeArguments(values []interface{}) string {
	s := fmt.Sprintf("%#v", values)
	if len(s) > maxSummaryLength {
		s = s[:maxSummaryLength] + "...(summary truncated)"
	}
	return s
}

// canonicalValue writes a deterministic textual representation of v into b.
//
// Unlike a plain fmt.Sprintf("%#v") it never prints pointer addresses (map and
// slice ordering is normalized by the caller side helpers) and replaces
// values that cannot be compared across processes (functions, channels,
// unsafe pointers) with a type-only placeholder. Pointer cycles are guarded
// against.
func canonicalValue(b *strings.Builder, v reflect.Value, visited map[uintptr]bool, depth int) {
	const maxDepth = 64

	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.Kind() == reflect.Ptr {
			if v.IsNil() {
				b.WriteString("nil<")
				b.WriteString(v.Type().String())
				b.WriteString(">")
				return
			}
			ptr := v.Pointer()
			if depth >= maxDepth || visited[ptr] {
				b.WriteString("<cycle:")
				b.WriteString(v.Type().String())
				b.WriteString(">")
				return
			}
			if visited == nil {
				visited = make(map[uintptr]bool)
			}
			visited[ptr] = true
			b.WriteString("&")
		}
		if v.Kind() == reflect.Interface {
			if v.IsNil() {
				b.WriteString("nil<")
				b.WriteString(v.Type().String())
				b.WriteString(">")
				return
			}
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Invalid:
		b.WriteString("<nil>")
	case reflect.Func:
		b.WriteString("<func:")
		b.WriteString(v.Type().String())
		b.WriteString(">")
	case reflect.Chan:
		b.WriteString("<chan:")
		b.WriteString(v.Type().String())
		b.WriteString(">")
	case reflect.UnsafePointer:
		b.WriteString("<unsafe.Pointer>")
	case reflect.Struct:
		if depth >= maxDepth {
			b.WriteString("<maxdepth:")
			b.WriteString(v.Type().String())
			b.WriteString(">")
			return
		}
		t := v.Type()
		b.WriteString(t.String())
		b.WriteString("{")
		for i := 0; i < v.NumField(); i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(t.Field(i).Name)
			b.WriteString(":")
			if !t.Field(i).IsExported() {
				b.WriteString("<unexported>")
				continue
			}
			canonicalValue(b, v.Field(i), visited, depth+1)
		}
		b.WriteString("}")
	case reflect.Slice, reflect.Array:
		if depth >= maxDepth {
			b.WriteString("<maxdepth>")
			return
		}
		b.WriteString(v.Type().String())
		b.WriteString("[")
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				b.WriteString(",")
			}
			canonicalValue(b, v.Index(i), visited, depth+1)
		}
		b.WriteString("]")
	case reflect.Map:
		if depth >= maxDepth {
			b.WriteString("<maxdepth>")
			return
		}
		b.WriteString(v.Type().String())
		b.WriteString("{")
		keys := v.MapKeys()
		// Normalize map iteration by sorting key digests.
		keyStrings := make([]string, len(keys))
		kvals := make([]reflect.Value, len(keys))
		for i, k := range keys {
			var kb strings.Builder
			canonicalValue(&kb, k, visited, depth+1)
			keyStrings[i] = kb.String()
			kvals[i] = k
		}
		// simple insertion sort, maps in mock arguments are small
		for i := 1; i < len(keyStrings); i++ {
			for j := i; j > 0 && keyStrings[j-1] > keyStrings[j]; j-- {
				keyStrings[j-1], keyStrings[j] = keyStrings[j], keyStrings[j-1]
				kvals[j-1], kvals[j] = kvals[j], kvals[j-1]
			}
		}
		for i, k := range kvals {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(keyStrings[i])
			b.WriteString(":")
			canonicalValue(b, v.MapIndex(k), visited, depth+1)
		}
		b.WriteString("}")
	default:
		switch v.Kind() {
		case reflect.String:
			b.WriteString(strconv.Quote(v.String()))
		case reflect.Bool:
			b.WriteString(strconv.FormatBool(v.Bool()))
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			b.WriteString(strconv.FormatInt(v.Int(), 10))
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			b.WriteString(strconv.FormatUint(v.Uint(), 10))
		case reflect.Float32, reflect.Float64:
			b.WriteString(strconv.FormatFloat(v.Float(), 'g', -1, 64))
		case reflect.Complex64, reflect.Complex128:
			c := v.Complex()
			fmt.Fprintf(b, "(%g+%gi)", real(c), imag(c))
		default:
			fmt.Fprintf(b, "%v", v.Interface())
		}
		b.WriteString("<")
		b.WriteString(v.Type().String())
		b.WriteString(">")
	}
}

// goroutineID returns the ID of the current goroutine, parsed from the runtime
// stack. It is only used to attribute nested mock calls (a call performed
// while another mock call is still open on the same goroutine, typically from
// within a Run callback).
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// stack starts with "goroutine 183 ["
	s := string(buf[:n])
	s = strings.TrimPrefix(s, "goroutine ")
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}
