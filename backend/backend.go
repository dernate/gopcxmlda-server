// Package backend defines the small, composable interfaces an application
// implements to plug its own process-data source into this library. It has
// no knowledge of HTTP, SOAP, or XML encoding — only of OPC XML-DA
// vocabulary (via package xmlda's Value, OPCQuality, ErrorCode, QName,
// BrowseFilter, and PropertyID types, which are stable regardless of any
// future churn in xmlda's wire-encoding structs — see
// docs/architecture/decisions/006-public-api-vs-wire-model.md and
// docs/architecture/package-structure.md).
package backend

import (
	"context"
	"errors"
	"time"

	"github.com/dernate/gopcxmlda-server/xmlda"
)

// ErrMissingStatus and ErrMissingReader are the errors Backend.Validate
// returns (via errors.Is) when a required capability is nil.
var (
	ErrMissingStatus = errors.New("backend: Status is required")
	ErrMissingReader = errors.New("backend: Reader is required")
)

// ItemRef identifies an item the way OPC XML-DA identifies it everywhere:
// ItemPath+ItemName together, or ItemName alone if ItemPath is empty.
type ItemRef struct {
	ItemPath string
	ItemName string
}

// ItemSample is a native-typed value+quality+timestamp triple as returned
// by a backend. Read-type coercion (the request's optional ReqType) is
// not the backend's concern — it is pure xmlda.Value logic performed by
// the server orchestration layer using whatever coercion the Value type
// exposes, so backends never need to know about ReqType.
//
// For a Bad-quality item with no last-known value at all (as opposed to
// a stale-but-present one), set Value to xmlda.NewNil(<the item's
// declared type>) rather than leaving it as some arbitrary zero value —
// the server layer checks Value.IsNil() to implement the specification's
// quality-driven value-presence rule (xmlda.ResolveValuePresence,
// REQ-QUALITY-003): omit the wire Value entirely unless either quality is
// Good/Uncertain, or quality is Bad and a genuine last-known value is
// present (i.e. not IsNil()).
type ItemSample struct {
	Value     xmlda.Value
	Quality   xmlda.OPCQuality
	Timestamp time.Time
}

// Result pairs a payload with an OPC result condition. Every backend
// method that reports per-item outcomes (Read, Write, GetProperties)
// returns a slice of Result aligned 1:1 with its request slice. ResultID
// is the zero xmlda.ErrorCode when there is no abnormal condition — see
// docs/architecture/decisions/005-backend-error-mapping.md for why this
// is the one and only channel for item-level conditions, structurally
// distinct from a method's whole-operation error return.
type Result[T any] struct {
	Value          T
	ResultID       xmlda.ErrorCode
	DiagnosticInfo string
}

// ServerStatus is the server status reported by GetStatus. It is also
// consulted by the server layer on every request (not just GetStatus) to
// evaluate xmlda.RequiresFault before any other backend call is made
// (REQ-SERVER-002) — State is the one field every operation's dispatch
// path depends on, not just GetStatus's own response.
type ServerStatus struct {
	// State is the server's current operating condition.
	State xmlda.ServerState
	// StartTime must be constant across the server process's lifetime
	// (REQ-STATUS-003).
	StartTime      time.Time
	ProductVersion string
	// VendorInfo and StatusInfo are locale-specific.
	VendorInfo string
	StatusInfo string
	// SupportedLocaleIDs must list at least one entry (REQ-STATUS-004).
	SupportedLocaleIDs []string
}

// StatusProvider reports the server's status. Required — every Backend
// must supply one.
type StatusProvider interface {
	GetStatus(ctx context.Context, locale string) (ServerStatus, error)
}

// ReadRequestItem is one requested item for Reader.Read.
type ReadRequestItem struct {
	Ref ItemRef
	// MaxAge is the maximum acceptable age of a cached value; zero means
	// "most accurate / force a device read" (REQ-READ-004).
	MaxAge time.Duration
}

// Reader is the one mandatory data-access capability — the subscription
// manager also uses it for Subscribe's validity-check/initial-values step
// and for polling when the backend has no ChangeNotifier.
type Reader interface {
	// Read returns one Result per item in items, in the same order
	// (len(result) == len(items) always). A non-nil error is a
	// whole-operation failure (mapped to a SOAP fault by the server);
	// per-item conditions (E_UNKNOWNITEMNAME, E_ACCESS_DENIED, E_RANGE,
	// E_TIMEDOUT, E_WRITEONLY, E_BADTYPE, ...) go in each Result's
	// ResultID.
	Read(ctx context.Context, items []ReadRequestItem) ([]Result[ItemSample], error)
}

// WriteRequestItem is one requested item for Writer.Write. If Quality
// and/or Timestamp are non-nil, the backend MUST apply Value+Quality+
// Timestamp atomically: accept all three, or reject the whole item with
// xmlda.ErrNotSupported (REQ-WRITE-003). Partial application (e.g. value
// written, quality silently dropped) is a specification violation.
type WriteRequestItem struct {
	Ref       ItemRef
	Value     xmlda.Value
	Quality   *xmlda.OPCQuality
	Timestamp *time.Time
}

// WriteOutcome is the result of writing one item. Value/Quality/Timestamp
// are optional: populate them only if the backend has an updated value
// worth reporting (e.g. after clamping); the server includes them in the
// response only if both the corresponding request option was set and the
// backend provided one.
type WriteOutcome struct {
	// Clamped reports whether the value was clamped to the item's valid
	// range; a clamped write still succeeds, reported via
	// xmlda.SuccessClamp (REQ-WRITE-005).
	Clamped   bool
	Value     *xmlda.Value
	Quality   *xmlda.OPCQuality
	Timestamp *time.Time
}

// Writer writes item values. A Backend with a nil Writer (or a
// server.Config with ReadOnly set) is read-only: every Write item
// resolves to xmlda.ErrAccessDenied without any backend call at all
// (REQ-SECURITY-002).
type Writer interface {
	Write(ctx context.Context, items []WriteRequestItem) ([]Result[WriteOutcome], error)
}

// BrowseRequest is one Browse call (§3.8.1). ContinuationPoint here is
// the backend's own private, opaque pagination cursor — the framework
// wraps it with a hash of the request's filters before exposing it on
// the wire, so backends never need to implement continuation-point
// validation themselves (REQ-BROWSE-002).
type BrowseRequest struct {
	// Ref with a blank ItemName means "browse the address space root".
	Ref                  ItemRef
	ContinuationPoint    string
	MaxElementsReturned  int
	Filter               xmlda.BrowseFilter
	ElementNameFilter    string
	VendorFilter         string
	ReturnAllProperties  bool
	ReturnPropertyValues bool
	PropertyNames        []xmlda.QName
}

// BrowseElement is one entry in a BrowseResult.
type BrowseElement struct {
	Name string
	// Ref is nil for a non-actionable "hint" node (REQ-BROWSE-005's
	// IsItem-without-identity case).
	Ref *ItemRef
	// IsItem and HasChildren are both required by the wire format; a
	// backend that cannot cheaply determine children should
	// conservatively report HasChildren=true.
	IsItem      bool
	HasChildren bool
	Properties  []Property
}

// BrowseResult is the result of one Browse call.
type BrowseResult struct {
	Elements []BrowseElement
	// ContinuationPoint is the backend's own opaque cursor for the next
	// page, "" if none.
	ContinuationPoint string
	MoreElements      bool
}

// Browser browses the address space.
type Browser interface {
	Browse(ctx context.Context, req BrowseRequest) (BrowseResult, error)
}

// PropertyRequest requests one item's properties.
type PropertyRequest struct {
	Ref ItemRef
	// All requests every property the item has; if false, only the
	// properties named in PropertyIDs.
	All           bool
	PropertyIDs   []xmlda.PropertyID
	IncludeValues bool
}

// Property is one property of an item.
type Property struct {
	ID   xmlda.PropertyID
	Name string
	// Namespace is the XML namespace for a vendor-defined property (ID
	// left at its zero value, Name set to the vendor's own property
	// name). Ignored when ID identifies a standard property, whose
	// namespace is always xmlda.Namespace. Vendor-specific codes/names
	// must live in the vendor's own namespace, never the OPC XML-DA one
	// (docs/specification/error-mapping.md) — leaving this empty for a
	// vendor property means the wire QName is left unqualified (Space
	// ""), never silently mislabeled as a standard OPC XML-DA property.
	Namespace   string
	Description string
	// Ref is non-nil iff the property is itself a directly addressable
	// item.
	Ref      *ItemRef
	Value    xmlda.Value
	ResultID xmlda.ErrorCode
}

// PropertyReader reads item properties. The outer Result's ResultID is
// the per-ITEM condition (e.g. E_UNKNOWNITEMNAME ⇒ no properties at
// all); each Property's own ResultID is a per-PROPERTY condition (e.g.
// E_INVALIDPID for one unrecognized property among several valid ones).
type PropertyReader interface {
	GetProperties(ctx context.Context, reqs []PropertyRequest) ([]Result[[]Property], error)
}

// WatchRequest is one item to watch, for ChangeNotifier.WatchItems.
type WatchRequest struct {
	Ref                   ItemRef
	RequestedSamplingRate time.Duration
	// Deadband is 0-100%, meaningful only for analog/array types.
	Deadband float64
}

// ChangeEvent is one pushed change from a ChangeNotifier. Err, if
// non-nil, means this specific item's watch broke: the subscription
// manager logs a warning and stops applying further updates for that
// item (it does not fall back to polling it individually — this
// subscription remains push-mode overall). If the backend's watch is
// broken more broadly rather than for just one item, close the whole
// channel instead (see ChangeNotifier.WatchItems) to trigger the
// subscription-wide poll-mode fallback.
type ChangeEvent struct {
	Ref    ItemRef
	Sample ItemSample
	Err    error
}

// ChangeNotifier is an OPTIONAL enhancement of Reader, detected via a
// type assertion (reader.(ChangeNotifier)) rather than a separate
// Backend field — the same idiom as http.Flusher/http.Hijacker off
// http.ResponseWriter, since it is inherently an enhanced-Reader
// capability rather than an independent operation. A backend without it
// is polled instead (the subscription manager calls Reader.Read on a
// schedule).
type ChangeNotifier interface {
	// WatchItems is called once per OPC subscription's item set. The
	// backend pushes on the returned channel and must close it when ctx
	// is done or it gives up (the subscription manager also selects on
	// ctx directly as a belt-and-suspenders measure).
	WatchItems(ctx context.Context, items []WatchRequest) (<-chan ChangeEvent, error)
}

// FaultCode is a small, backend-facing vocabulary for signaling a
// whole-operation failure precisely, via BackendError. Most backends
// never need this — see docs/architecture/decisions/005-backend-error-mapping.md.
type FaultCode string

// Standard FaultCode values.
const (
	FaultBusy         FaultCode = "busy"
	FaultAccessDenied FaultCode = "access_denied"
	FaultServerState  FaultCode = "server_state"
	FaultOutOfMemory  FaultCode = "out_of_memory"
	FaultTimedOut     FaultCode = "timed_out"
	FaultNotSupported FaultCode = "not_supported"
)

// ErrorCode maps fc to the xmlda.ErrorCode it denotes. An unrecognized
// FaultCode maps to E_FAIL.
func (fc FaultCode) ErrorCode() xmlda.ErrorCode {
	switch fc {
	case FaultBusy:
		return xmlda.ErrBusy
	case FaultAccessDenied:
		return xmlda.ErrAccessDenied
	case FaultServerState:
		return xmlda.ErrServerState
	case FaultOutOfMemory:
		return xmlda.ErrOutOfMemory
	case FaultTimedOut:
		return xmlda.ErrTimedOut
	case FaultNotSupported:
		return xmlda.ErrNotSupported
	default:
		return xmlda.ErrFail
	}
}

// ErrorCodeFor maps an arbitrary error from a backend call to the
// xmlda.ErrorCode it should be reported as (ADR-005): an opt-in
// *BackendError is honored precisely, context.DeadlineExceeded becomes
// E_TIMEDOUT, and anything else becomes E_FAIL. It is the single mapping
// used both for whole-operation SOAP faults (server layer) and for the
// per-item ResultID of an asynchronously-failing subscribed item
// (subscription layer), so the two can never drift apart.
func ErrorCodeFor(err error) xmlda.ErrorCode {
	var be *BackendError
	if errors.As(err, &be) {
		return be.Fault.ErrorCode()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return xmlda.ErrTimedOut
	}
	return xmlda.ErrFail
}

// BackendError lets a backend precisely signal a whole-operation failure
// (e.g. busy/access-denied) instead of relying on the server's
// deterministic default mapping (context.DeadlineExceeded → E_TIMEDOUT,
// else E_FAIL). The server layer checks for this via errors.As before
// falling back to the default.
type BackendError struct {
	Fault FaultCode
	Err   error
}

// Error implements the error interface. Safe to call even if Err was left
// nil (e.g. a backend author set Fault but forgot Err) — falls back to a
// message derived from Fault rather than panicking on the nil dereference.
func (e *BackendError) Error() string {
	if e.Err == nil {
		return "backend: " + string(e.Fault)
	}
	return e.Err.Error()
}

// Unwrap supports errors.Is/errors.As against the wrapped error.
func (e *BackendError) Unwrap() error { return e.Err }

// Backend composes the capabilities an application's data source
// implements. Status and Reader are required; Writer, Browser, and
// Properties are nil-able — their absence is a normal, well-defined
// feature-detection signal (e.g. a nil Writer means read-only), not an
// error condition callers must special-case beyond a nil check.
type Backend struct {
	Status     StatusProvider
	Reader     Reader
	Writer     Writer
	Browser    Browser
	Properties PropertyReader
}

// Validate reports an error if Backend is missing a required capability
// (Status or Reader). Call this once at server construction time so a
// misconfigured backend fails fast, rather than panicking or behaving
// unpredictably at request time.
func (b Backend) Validate() error {
	if b.Status == nil {
		return ErrMissingStatus
	}
	if b.Reader == nil {
		return ErrMissingReader
	}
	return nil
}
