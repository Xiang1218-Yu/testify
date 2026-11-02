package mock

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// ExpectationRecord is the versioned snapshot of one expectation inside a
// frozen expectation graph.
type ExpectationRecord struct {
	MockIndex     int       `json:"mockIndex"`
	Index         int       `json:"index"`
	Method        string    `json:"method"`
	Repeatability int       `json:"repeatability"`
	Optional      bool      `json:"optional"`
	HasPanic      bool      `json:"hasPanic"`
	HasRun        bool      `json:"hasRun"`
	HasWait       bool      `json:"hasWait"`
	HasAfter      bool      `json:"hasAfter"`
	Requires      [][2]int  `json:"requires"`
	Fingerprint   string    `json:"fingerprint"`
}

// CallRecord is a versioned, integrity-protected recording of one test
// interaction across one or more mocks. Records produced by Commit are sealed
// and replayable; records produced by Checkpoint are unsealed and can be
// resumed.
type CallRecord struct {
	// Version identifies the format and the session lineage.
	Version SessionVersion
	// Sealed reports whether the record was committed (true) or only
	// checkpointed as an interrupted recording (false).
	Sealed bool
	// CreatedAt is the UTC time the record was built.
	CreatedAt time.Time
	// Mocks describes the frozen mocks, in freeze order.
	Mocks []MockInfo
	// Expectations is the frozen expectation graph.
	Expectations []ExpectationRecord
	// GraphHash is the version hash of the frozen expectation graph.
	GraphHash string
	// Events are the recorded call events in global sequence (Seq) order.
	Events []*CallEvent

	// recordHash covers version, graph and the complete event chain.
	recordHash string
}

// Len returns the number of recorded events.
func (r *CallRecord) Len() int { return len(r.Events) }

// Event returns the event at the 0-based index.
func (r *CallRecord) Event(i int) *CallEvent { return r.Events[i] }

// Hash returns the record integrity hash.
func (r *CallRecord) Hash() string { return r.recordHash }

// IsSealed reports whether the record is committed.
func (r *CallRecord) IsSealed() bool { return r.Sealed }

/*
	Integrity chain
*/

// canonicalEventForChain renders all verification-relevant fields of an event
// into a deterministic string used by the hash chain.
func canonicalEventForChain(ev *CallEvent) string {
	return strings.Join([]string{
		fmt.Sprintf("seq=%d", ev.Seq),
		fmt.Sprintf("mock=%d", ev.MockIndex),
		"name=" + ev.MockName,
		"method=" + ev.Method,
		fmt.Sprintf("expect=%d", ev.ExpectationIndex),
		"args=" + ev.argsDigest,
		fmt.Sprintf("parent=%d", ev.ParentSeq),
		fmt.Sprintf("depth=%d", ev.Depth),
		fmt.Sprintf("viarun=%t", ev.ViaRun),
		fmt.Sprintf("hadrun=%t", ev.HadRun),
		fmt.Sprintf("ranrun=%t", ev.RanRun),
		"panic=" + ev.Panic,
		"panicsource=" + ev.PanicSource,
		"ret=" + ev.retDigest,
		fmt.Sprintf("erroridx=%d", ev.ErrorIndex),
	}, "\n")
}

// sealEventChain (re)computes every event's chain hash over the given, Seq
// ordered slice.
func sealEventChain(events []*CallEvent) {
	prev := ""
	for _, ev := range events {
		ev.chainHash = digestString(prev + "\x00" + canonicalEventForChain(ev))
		prev = ev.chainHash
	}
}

// computeRecordHash hashes versioning, graph and the complete event chain.
func computeRecordHash(r *CallRecord) string {
	var b strings.Builder
	fmt.Fprintln(&b, r.Version.String())
	fmt.Fprintln(&b, r.Sealed)
	fmt.Fprintln(&b, r.GraphHash)
	fmt.Fprintf(&b, "events=%d\n", len(r.Events))
	for _, ev := range r.Events {
		fmt.Fprintln(&b, ev.chainHash)
	}
	return digestString(b.String())
}

// Verify revalidates the record on its own: format version, sequence
// continuity, nesting topology, per-event hash chain, value digests and the
// final record hash. It returns a *SessionError of kind
// SessionErrorIntegrity carrying the position of the first invalid event.
func (r *CallRecord) Verify() error {
	if r == nil {
		return newSessionError(SessionErrorIntegrity, "verify", SessionVersion{Format: CallSessionFormat}, 0, -1, "", "nil record")
	}
	v := r.Version
	if v.Format != CallSessionFormat {
		return newSessionError(SessionErrorIntegrity, "verify", v, 0, -1, "",
			"unsupported record format: "+v.Format)
	}
	if r.GraphHash == "" || r.recordHash == "" {
		return newSessionError(SessionErrorIntegrity, "verify", v, 0, -1, "", "record is missing signatures")
	}

	prev := ""
	for i, ev := range r.Events {
		pos := i + 1
		if ev == nil {
			return integrityEventError(v, pos, "nil event")
		}
		if ev.Seq != pos {
			return integrityEventError(v, pos, fmt.Sprintf("event sequence is not contiguous: expected seq %d, got %d", pos, ev.Seq))
		}
		if ev.MockIndex < 0 || ev.MockIndex >= len(r.Mocks) {
			return integrityEventError(v, pos, "event references an unknown mock index")
		}
		if r.Mocks[ev.MockIndex].Name != ev.MockName {
			return integrityEventError(v, pos, "event mock name does not match the frozen mocks")
		}
		if ev.ParentSeq != 0 {
			if ev.ParentSeq >= ev.Seq {
				return integrityEventError(v, pos, "event parent seq must be smaller than the event seq")
			}
			if !ev.ViaRun {
				return integrityEventError(v, pos, "nested event must be marked as generated by a Run callback")
			}
		} else if ev.ViaRun {
			return integrityEventError(v, pos, "top-level event must not be marked as generated by a Run callback")
		}
		if ev.argsDigest == "" {
			return integrityEventError(v, pos, "event is missing its arguments digest")
		}
		if ev.args != nil && digestValues(ev.args) != ev.argsDigest {
			return integrityEventError(v, pos, "event arguments do not match their digest")
		}
		if ev.hasReturns && digestValues(ev.returns) != ev.retDigest {
			return integrityEventError(v, pos, "event return arguments do not match their digest")
		}
		want := digestString(prev + "\x00" + canonicalEventForChain(ev))
		if ev.chainHash != want {
			return integrityEventError(v, pos, "event hash chain is broken; the record was truncated or tampered with")
		}
		prev = ev.chainHash
	}

	if computeRecordHash(r) != r.recordHash {
		return newSessionError(SessionErrorIntegrity, "verify", v, len(r.Events), -1, "",
			"record hash mismatch; the record was truncated or tampered with")
	}
	return nil
}

func integrityEventError(v SessionVersion, pos int, detail string) *SessionError {
	return newSessionError(SessionErrorIntegrity, "verify", v, pos, -1, "", detail)
}

func countRecordExpectations(r *CallRecord, mockIndex int) int {
	n := 0
	for _, e := range r.Expectations {
		if e.MockIndex == mockIndex {
			n++
		}
	}
	return n
}

func findRecordExpectation(r *CallRecord, mockIndex, index int) *ExpectationRecord {
	for i := range r.Expectations {
		if r.Expectations[i].MockIndex == mockIndex && r.Expectations[i].Index == index {
			return &r.Expectations[i]
		}
	}
	return nil
}

/*
	Export / import
*/

type eventDTO struct {
	Seq              int    `json:"seq"`
	MockIndex        int    `json:"mockIndex"`
	MockName         string `json:"mockName"`
	Method           string `json:"method"`
	ExpectationIndex int    `json:"expectationIndex"`
	ArgsSummary      string `json:"argsSummary"`
	ArgsDigest       string `json:"argsDigest"`
	RetDigest        string `json:"retDigest"`
	ParentSeq        int    `json:"parentSeq"`
	ViaRun           bool   `json:"viaRun"`
	Depth            int    `json:"depth"`
	HadRun           bool   `json:"hadRun"`
	RanRun           bool   `json:"ranRun"`
	Panic            string `json:"panic"`
	PanicSource      string `json:"panicSource"`
	ErrorIndex       int    `json:"errorIndex"`
	HasReturns       bool   `json:"hasReturns"`
	ChainHash        string `json:"chainHash"`
}

type recordDTO struct {
	Format       string              `json:"format"`
	Version      SessionVersion      `json:"version"`
	Sealed       bool                `json:"sealed"`
	CreatedAt    time.Time           `json:"createdAt"`
	Mocks        []MockInfo          `json:"mocks"`
	Expectations []ExpectationRecord `json:"expectations"`
	GraphHash    string              `json:"graphHash"`
	Events       []eventDTO          `json:"events"`
	RecordHash   string              `json:"recordHash"`
	Values       string              `json:"values"`
}

// gobEventValues carries the live arguments and return values of one event.
type gobEventValues struct {
	Seq     int
	Args    []interface{}
	Returns []interface{}
}

type gobValueBundle struct {
	Values []gobEventValues
}

// Export serializes the record into a self-describing, versioned JSON document
// with an embedded gob payload holding live argument/return values for
// replay. Values gob cannot encode (functions, channels, ...) produce a
// SessionError of kind SessionErrorUnsupportedValue carrying the event
// position. Custom named types used as interface values must be registered
// with encoding/gob by the caller, as with any gob stream.
func (r *CallRecord) Export() ([]byte, error) {
	if err := r.Verify(); err != nil {
		return nil, err
	}

	dto := recordDTO{
		Format:       CallSessionFormat,
		Version:      r.Version,
		Sealed:       r.Sealed,
		CreatedAt:    r.CreatedAt,
		Mocks:        r.Mocks,
		Expectations: r.Expectations,
		GraphHash:    r.GraphHash,
		RecordHash:   r.recordHash,
	}

	bundle := gobValueBundle{}
	named := make(map[reflect.Type]bool)
	for _, ev := range r.Events {
		dto.Events = append(dto.Events, eventDTO{
			Seq:              ev.Seq,
			MockIndex:        ev.MockIndex,
			MockName:         ev.MockName,
			Method:           ev.Method,
			ExpectationIndex: ev.ExpectationIndex,
			ArgsSummary:      ev.ArgsSummary,
			ArgsDigest:       ev.argsDigest,
			RetDigest:        ev.retDigest,
			ParentSeq:        ev.ParentSeq,
			ViaRun:           ev.ViaRun,
			Depth:            ev.Depth,
			HadRun:           ev.HadRun,
			RanRun:           ev.RanRun,
			Panic:            ev.Panic,
			PanicSource:      ev.PanicSource,
			ErrorIndex:       ev.ErrorIndex,
			HasReturns:       ev.hasReturns,
			ChainHash:        ev.chainHash,
		})
		if err := planEventValues(r.Version, ev, named); err != nil {
			return nil, err
		}
		bundle.Values = append(bundle.Values, gobEventValues{
			Seq:     ev.Seq,
			Args:    append([]interface{}{}, ev.args...),
			Returns: append([]interface{}{}, ev.returns...),
		})
	}
	if err := ensureNamedTypes(named, r.Version); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := safeGobEncode(enc, &bundle); err != nil {
		return nil, newSessionError(SessionErrorUnsupportedValue, "export", r.Version, 0, -1, "",
			"could not encode live call values: "+err.Error())
	}
	dto.Values = base64.StdEncoding.EncodeToString(buf.Bytes())

	out, err := json.MarshalIndent(&dto, "", "  ")
	if err != nil {
		return nil, newSessionError(SessionErrorUnsupportedValue, "export", r.Version, 0, -1, "", err.Error())
	}
	return out, nil
}

func safeGobEncode(enc *gob.Encoder, v interface{}) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("gob register/encode panic: %v", rec)
		}
	}()
	return enc.Encode(v)
}

// planEventValues rejects values gob can never represent and otherwise
// collects named concrete types for registration.
func planEventValues(version SessionVersion, ev *CallEvent, named map[reflect.Type]bool) error {
	for _, v := range append(append([]interface{}{}, ev.args...), ev.returns...) {
		if v == nil {
			continue
		}
		t := reflect.TypeOf(v)
		switch t.Kind() {
		case reflect.Func, reflect.Chan, reflect.UnsafePointer:
			return newSessionError(SessionErrorUnsupportedValue, "export", version, ev.Seq, ev.MockIndex, ev.MockName,
				fmt.Sprintf("value of kind %s cannot be exported; replace it with a serializable value or keep the record in-process", t.Kind()))
		}
		collectNamedTypes(t, named)
	}
	return nil
}

func collectNamedTypes(t reflect.Type, named map[reflect.Type]bool) {
	switch t.Kind() {
	case reflect.Ptr:
		named[t] = true
		collectNamedTypes(t.Elem(), named)
	case reflect.Struct:
		named[t] = true
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).IsExported() {
				collectNamedTypes(t.Field(i).Type, named)
			}
		}
	case reflect.Slice, reflect.Array:
		collectNamedTypes(t.Elem(), named)
	case reflect.Map:
		collectNamedTypes(t.Key(), named)
		collectNamedTypes(t.Elem(), named)
	case reflect.Interface:
		// no static type available
	default:
		if t.Name() != "" && t.PkgPath() != "" {
			named[t] = true
		}
	}
}

// gobRegistrarMu/gobRegisteredTypes memoize types known to round-trip
// through gob, including types callers registered themselves. gob.Register
// panics on duplicate registration (even identical), so every registration
// is preceded by a trial round-trip.
var (
	gobRegistrarMu    sync.Mutex
	gobRegisteredType = make(map[reflect.Type]bool)
)

// ensureNamedTypes makes sure every observed concrete type can be carried as
// an interface value through gob. Types already registered (by the caller or
// by a previous export) and built-in types are left untouched. Types that
// cannot be registered or encoded produce a structured unsupported-value
// error.
func ensureNamedTypes(named map[reflect.Type]bool, version SessionVersion) error {
	types := make([]reflect.Type, 0, len(named))
	for t := range named {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return types[i].String() < types[j].String() })
	for _, t := range types {
		gobRegistrarMu.Lock()
		known := gobRegisteredType[t]
		gobRegistrarMu.Unlock()
		if known {
			continue
		}
		if err := ensureGobType(t); err != nil {
			return newSessionError(SessionErrorUnsupportedValue, "export", version, 0, -1, "",
				"gob type registration failed: "+err.Error())
		}
		gobRegistrarMu.Lock()
		gobRegisteredType[t] = true
		gobRegistrarMu.Unlock()
	}
	return nil
}

func ensureGobType(t reflect.Type) error {
	if err := gobTrialRoundTrip(gobProbeValue(t)); err == nil {
		return nil
	}
	probe := gobProbeValue(t)
	var registerPanic interface{}
	func() {
		defer func() { registerPanic = recover() }()
		gob.Register(probe)
	}()
	if err := gobTrialRoundTrip(probe); err == nil {
		return nil
	}
	if registerPanic != nil {
		return fmt.Errorf("type %s: %v (pre-register the type with encoding/gob to resolve conflicts)", t, registerPanic)
	}
	return fmt.Errorf("type %s cannot be gob-encoded; only serializable values can be exported", t)
}

// gobProbeValue builds a usable value of t for a trial round-trip: a non-nil
// pointer for pointer types, an empty collection for slices/maps, and a zero
// value otherwise.
func gobProbeValue(t reflect.Type) interface{} {
	switch t.Kind() {
	case reflect.Ptr:
		return reflect.New(t.Elem()).Interface()
	case reflect.Slice:
		return reflect.MakeSlice(t, 0, 1).Interface()
	case reflect.Map:
		return reflect.MakeMap(t).Interface()
	default:
		return reflect.New(t).Elem().Interface()
	}
}

// gobTrialRoundTrip encodes one value inside an interface slice and decodes
// it back, exactly as the export payload will carry it.
func gobTrialRoundTrip(v interface{}) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("gob panic: %v", rec)
		}
	}()
	var buf bytes.Buffer
	if err = gob.NewEncoder(&buf).Encode([]interface{}{v}); err != nil {
		return err
	}
	var out []interface{}
	return gob.NewDecoder(&buf).Decode(&out)
}

// ImportCallRecord parses, validates and returns an exported record. The
// record format, version, expectation graph, event chain and final signature
// are checked before return. Custom value types carried in the gob payload
// must be registered with encoding/gob by the caller beforehand.
func ImportCallRecord(data []byte) (*CallRecord, error) {
	version := SessionVersion{Format: CallSessionFormat}
	var dto recordDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return nil, newSessionError(SessionErrorIntegrity, "import", version, 0, -1, "",
			"malformed session record: "+err.Error())
	}
	version = dto.Version
	if dto.Format != CallSessionFormat {
		return nil, newSessionError(SessionErrorIntegrity, "import", version, 0, -1, "",
			"unsupported record format: "+dto.Format)
	}

	rec := &CallRecord{
		Version:      dto.Version,
		Sealed:       dto.Sealed,
		CreatedAt:    dto.CreatedAt,
		Mocks:        dto.Mocks,
		Expectations: dto.Expectations,
		GraphHash:    dto.GraphHash,
		recordHash:   dto.RecordHash,
	}
	for _, e := range dto.Events {
		rec.Events = append(rec.Events, &CallEvent{
			Seq:              e.Seq,
			MockIndex:        e.MockIndex,
			MockName:         e.MockName,
			Method:           e.Method,
			ExpectationIndex: e.ExpectationIndex,
			ArgsSummary:      e.ArgsSummary,
			argsDigest:       e.ArgsDigest,
			retDigest:        e.RetDigest,
			ParentSeq:        e.ParentSeq,
			ViaRun:           e.ViaRun,
			Depth:            e.Depth,
			HadRun:           e.HadRun,
			RanRun:           e.RanRun,
			Panic:            e.Panic,
			PanicSource:      e.PanicSource,
			ErrorIndex:       e.ErrorIndex,
			hasReturns:       e.HasReturns,
			chainHash:        e.ChainHash,
		})
	}

	if dto.Values != "" {
		raw, err := base64.StdEncoding.DecodeString(dto.Values)
		if err != nil {
			return nil, newSessionError(SessionErrorIntegrity, "import", version, 0, -1, "",
				"corrupt value payload: "+err.Error())
		}
		var bundle gobValueBundle
		if err := safeGobDecode(gob.NewDecoder(bytes.NewReader(raw)), &bundle); err != nil {
			return nil, newSessionError(SessionErrorIntegrity, "import", version, 0, -1, "",
				"cannot decode live call values (register custom gob types before import): "+err.Error())
		}
		bySeq := make(map[int]*gobEventValues, len(bundle.Values))
		for i := range bundle.Values {
			bySeq[bundle.Values[i].Seq] = &bundle.Values[i]
		}
		for i, ev := range rec.Events {
			vals := bySeq[ev.Seq]
			if vals == nil {
				// report the event slot, not the (possibly tampered) seq
				return nil, newSessionError(SessionErrorIntegrity, "import", version, i+1, ev.MockIndex, ev.MockName,
					"record is missing live values for event")
			}
			ev.args = vals.Args
			ev.returns = vals.Returns
			ev.payloadPresent = true
		}
	}

	if err := rec.Verify(); err != nil {
		return nil, err
	}
	return rec, nil
}

func safeGobDecode(dec *gob.Decoder, v interface{}) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("gob decode panic: %v", rec)
		}
	}()
	return dec.Decode(v)
}
