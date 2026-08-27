// Package clientintegration exercises this repository's OPC XML-DA server
// against the real, independently-maintained reference client
// (github.com/dernate/gopcxmlda), not this repository's own hand-built
// test fixtures. It lives in its own Go module (see go.mod) specifically
// so the base library's go.mod stays dependency-free — only this
// integration-test module depends on the external client.
package clientintegration

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/examples/basic-server/memorybackend"
	"github.com/dernate/gopcxmlda-server/server"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// newTestServer starts a real HTTP server (httptest) in front of a real
// memorybackend.Backend, and returns a real gopcxmlda.Server client
// pointed at it. cleanup follows this library's own documented shutdown
// ordering (cancel subscriptions before stopping the HTTP layer).
func newTestServer(t *testing.T) (*gopcxmlda.Server, *memorybackend.Backend) {
	t.Helper()

	be := memorybackend.New()
	h, err := server.New(server.Deps{
		Backend: backend.Backend{Status: be, Reader: be, Writer: be, Browser: be, Properties: be},
	}, server.Config{})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(func() {
		h.BeginShutdown() // unblock any in-flight PolledRefresh call first
		ts.Close()        // stop accepting connections, wait for in-flight handlers
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Shutdown(ctx); err != nil { // idempotent re-cancel + wait for background goroutines
			t.Errorf("Handler background goroutines did not exit: %v", err)
		}
		be.Close()
	})

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	// Timeout must be set explicitly: gopcxmlda.Server's zero value causes
	// its internal send() to substitute a literal 10 (time.Duration(10) is
	// 10 *nanoseconds*, not 10 seconds — confirmed by reading
	// github.com/dernate/gopcxmlda's client_helper.go — every request would
	// then time out almost instantly).
	client := &gopcxmlda.Server{Url: u, LocaleID: "en-US", Timeout: 10 * time.Second}
	return client, be
}

// clientOptions is the RequestOptions every client call in this module
// sends. Asking for ItemName, ItemPath and Timestamp exercises each
// response in its fully populated form instead of the sparse default one,
// so a field the server fails to echo surfaces as a test failure rather
// than going unnoticed.
//
// The attribute names are PascalCase because that is what the wire format
// defines and what the server matches. XML attribute names are
// case-sensitive, so a lowercased "returnItemName" is not a synonym for
// "ReturnItemName" — it is an unknown attribute, silently ignored, and
// the option falls back to its false default. TestRealClient_RequestOptionsAreCaseSensitive pins that down.
//
// It returns a fresh map on every call rather than exposing one shared
// value: the reference client writes ClientRequestHandle into the map it
// is handed, so a single map shared across concurrent calls is a data
// race.
func clientOptions() map[string]any {
	return map[string]any{
		"ReturnItemTime": true,
		"ReturnItemPath": true,
		"ReturnItemName": true,
	}
}

func newHandles(t *testing.T, n int) (string, []string) {
	t.Helper()
	crh, cih, err := gopcxmlda.GenerateClientHandles(n)
	if err != nil {
		t.Fatalf("GenerateClientHandles: %v", err)
	}
	return crh, cih
}

// TestRealClient_GetStatus drives GetStatus through the real client.
func TestRealClient_GetStatus(t *testing.T) {
	client, _ := newTestServer(t)
	crh, _ := newHandles(t, 0)

	got, err := client.GetStatus(context.Background(), &crh, "")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if got.Response.Result.ServerState != string(xmlda.ServerStateRunning) {
		t.Fatalf("got ServerState %q, want %q", got.Response.Result.ServerState, xmlda.ServerStateRunning)
	}
	if got.Response.Status.ProductVersion == "" {
		t.Fatalf("expected a non-empty ProductVersion")
	}
}

// TestRealClient_BrowseRootAndChild drives Browse through the real client,
// both at the address-space root and one level down. Note the client's
// Browse signature splits addressing across two places: the positional
// itemPath argument becomes the wire ItemPath attribute, while
// TBrowseOptions.ItemName becomes the wire ItemName attribute — confirmed
// by reading buildBrowsePayload, not guessed.
func TestRealClient_BrowseRootAndChild(t *testing.T) {
	client, _ := newTestServer(t)
	crh, _ := newHandles(t, 0)

	root, err := client.Browse(context.Background(), "", &crh, "", gopcxmlda.TBrowseOptions{})
	if err != nil {
		t.Fatalf("Browse(root): %v", err)
	}
	if len(root.Response.Elements) != 1 || root.Response.Elements[0].Name != "Demo" {
		t.Fatalf("got root Elements %+v, want a single \"Demo\" branch", root.Response.Elements)
	}

	crh2, _ := newHandles(t, 0)
	child, err := client.Browse(context.Background(), "", &crh2, "", gopcxmlda.TBrowseOptions{ItemName: "Demo"})
	if err != nil {
		t.Fatalf("Browse(Demo): %v", err)
	}
	names := map[string]bool{}
	for _, e := range child.Response.Elements {
		names[e.Name] = true
	}
	for _, want := range []string{"Counter", "Temperature", "Switch", "Message"} {
		if !names[want] {
			t.Fatalf("Browse(Demo) missing expected item %q, got %+v", want, child.Response.Elements)
		}
	}
}

// TestRealClient_ReadInitialValue reads Demo/Switch's initial value
// through the real client and confirms it decodes as a proper boolean —
// this is the "happy path" scalar Read, for comparison against the Write
// interoperability finding below.
func TestRealClient_ReadInitialValue(t *testing.T) {
	client, _ := newTestServer(t)
	crh, cih := newHandles(t, 1)

	items := []gopcxmlda.TItem{{ItemName: "Demo/Switch"}}
	got, err := client.Read(context.Background(), items, &crh, &cih, "", clientOptions())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Response.ItemList.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.Response.ItemList.Items))
	}
	item := got.Response.ItemList.Items[0]
	if item.Error != "" {
		t.Fatalf("unexpected ResultID %q", item.Error)
	}
	// clientOptions asked for these; a reply that omits them is a
	// server-side gating bug, not a client quirk.
	if item.ItemName != "Demo/Switch" {
		t.Errorf("got ItemName %q, want it echoed as requested via ReturnItemName", item.ItemName)
	}
	if item.Timestamp.IsZero() {
		t.Errorf("got no Timestamp despite ReturnItemTime")
	}
	b, ok := item.Value.Value.(bool)
	if !ok {
		t.Fatalf("got Value %#v (type %s), want a decoded bool", item.Value.Value, item.Value.Type)
	}
	if b != false {
		t.Fatalf("got Switch=%v, want false (initial value)", b)
	}
}

// TestRealClient_WriteScalar_TypeNamespaceMismatch documents a real
// interoperability finding, found by reading the client's source and
// confirmed here by actually running it: github.com/dernate/gopcxmlda's
// Write path builds a scalar Value's xsi:type as "<opc-ns-prefix>:<xsd
// type local name>" (e.g. xsi:type="ns0:boolean", ns0 bound to the OPC
// XML-DA namespace) instead of the specification's xsi:type="xsd:boolean"
// (XSD namespace) — see getOpcXmlDaType/buildWriteItems in the client's
// client_helper.go/types_helper.go. This library resolves xsi:type by
// namespace URI, not by local name alone (ADR-004), so it correctly
// treats this as an unrecognized type (Kind: Unknown, per ADR-003) rather
// than crashing or guessing — but the practical effect is that a scalar
// value written by this client is stored as an opaque, uninterpreted
// blob, not as a typed boolean. Array values are unaffected (see the next
// test): the client happens to use the OPC namespace prefix for
// "ArrayOfX" names too, which is where the specification actually expects
// them.
func TestRealClient_WriteScalar_TypeNamespaceMismatch(t *testing.T) {
	client, be := newTestServer(t)
	crh, cih := newHandles(t, 1)

	items := []gopcxmlda.TItem{{ItemName: "Demo/Switch", Value: gopcxmlda.TValue{Value: true}}}
	_, err := client.Write(context.Background(), items, &crh, &cih, "", clientOptions())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Verify what actually landed, via this library's own (already
	// extensively tested) Read path rather than the same client's Read —
	// isolates whether the value was stored as a proper bool or as the
	// unrecognized-type blob the source analysis above predicts.
	got, err := be.Read(context.Background(), []backend.ReadRequestItem{{Ref: backend.ItemRef{ItemName: "Demo/Switch"}}})
	if err != nil {
		t.Fatalf("backend.Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	v := got[0].Value.Value
	if v.Kind() == xmlda.KindUnknown {
		t.Logf("confirmed: real client's scalar Write decoded as Kind=Unknown, TypeName=%s, raw=%q — "+
			"the client's known xsi:type namespace mismatch, not a bug in this server", v.TypeName(), func() string {
			raw, _ := v.Raw()
			return string(raw.InnerXML)
		}())
		return
	}
	// If a future client release fixes this, the value should now be a
	// proper bool — update this test rather than leaving it stale.
	b, err := v.Bool()
	if err != nil {
		t.Fatalf("value decoded as Kind=%s, neither Unknown (documented finding) nor a proper bool: %v", v.Kind(), err)
	}
	t.Logf("client's scalar Write now decodes as a proper bool (%v) — the documented namespace mismatch appears fixed upstream", b)
}

// TestRealClient_WriteArray_DecodesCorrectly confirms the control case for
// the finding above: an array value's xsi:type ("ArrayOfInt" etc.) is
// correctly placed in the OPC XML-DA namespace by the client (matching
// the specification), so it decodes as a proper typed array.
func TestRealClient_WriteArray_DecodesCorrectly(t *testing.T) {
	client, be := newTestServer(t)
	crh, cih := newHandles(t, 1)

	// Demo/Message accepts any writable item for this test's purposes —
	// memorybackend.Write does not type-check against the item's "native"
	// type, it stores whatever Value it's given.
	items := []gopcxmlda.TItem{{ItemName: "Demo/Message", Value: gopcxmlda.TValue{Value: []int32{1, 2, 3}}}}
	_, err := client.Write(context.Background(), items, &crh, &cih, "", clientOptions())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := be.Read(context.Background(), []backend.ReadRequestItem{{Ref: backend.ItemRef{ItemName: "Demo/Message"}}})
	if err != nil {
		t.Fatalf("backend.Read: %v", err)
	}
	arr, err := got[0].Value.Value.Array()
	if err != nil {
		t.Fatalf("expected a typed array, got Kind=%s: %v", got[0].Value.Value.Kind(), err)
	}
	ints, err := arr.Int32s()
	if err != nil {
		t.Fatalf("expected an int32 array: %v", err)
	}
	if len(ints) != 3 || ints[0] != 1 || ints[1] != 2 || ints[2] != 3 {
		t.Fatalf("got %v, want [1 2 3]", ints)
	}
}

// TestRealClient_WriteUnknownItem_ReportsResultID confirms a per-item
// error condition survives the round trip through the real client.
func TestRealClient_WriteUnknownItem_ReportsResultID(t *testing.T) {
	client, _ := newTestServer(t)
	crh, cih := newHandles(t, 1)

	items := []gopcxmlda.TItem{{ItemName: "Demo/NoSuchItem", Value: gopcxmlda.TValue{Value: true}}}
	got, err := client.Write(context.Background(), items, &crh, &cih, "", clientOptions())
	// The client treats a non-empty top-level <Errors> list as an error
	// return (errReturn), so err != nil here is expected — assert on the
	// decoded ResultID instead of treating err as fatal.
	if err == nil {
		t.Fatalf("expected the client to surface the write error, got nil")
	}
	// The client's TItem.Error field is a plain xml:"ResultID,attr" string
	// — it captures the wire attribute's literal text verbatim rather than
	// resolving the QName's namespace prefix, so the value carries this
	// server's own chosen prefix ("opc", see qnameAttr/prefixForNamespace)
	// rather than a bare local name.
	if len(got.Response.ItemList.Items) != 1 || got.Response.ItemList.Items[0].Error != "opc:E_UNKNOWNITEMNAME" {
		t.Fatalf("got items %+v, want a single opc:E_UNKNOWNITEMNAME result", got.Response.ItemList.Items)
	}
}

// TestRealClient_GetProperties drives GetProperties through the real
// client. As with TestRealClient_WriteUnknownItem_ReportsResultID above,
// TProperties.Name is a plain xml:"Name,attr" string that captures the
// wire text verbatim, including this server's own "opc:" prefix — the
// client never resolves it to a bare local name.
func TestRealClient_GetProperties(t *testing.T) {
	client, _ := newTestServer(t)
	crh, _ := newHandles(t, 0)

	items := []gopcxmlda.TItem{{ItemName: "Demo/Temperature"}}
	opts := gopcxmlda.TPropertyOptions{ReturnAllProperties: true, ReturnPropertyValues: true, ReturnErrorText: true}
	got, err := client.GetProperties(context.Background(), items, opts, &crh, "")
	if err != nil {
		t.Fatalf("GetProperties: %v", err)
	}
	if len(got.Response.PropertyList) != 1 {
		t.Fatalf("got %d property lists, want 1", len(got.Response.PropertyList))
	}
	names := map[string]bool{}
	for _, p := range got.Response.PropertyList[0].Properties {
		names[p.Name] = true
	}
	if !names["opc:dataType"] || !names["opc:description"] {
		t.Fatalf("got properties %+v, want at least opc:dataType and opc:description", got.Response.PropertyList[0].Properties)
	}
}

// TestRealClient_SubscribeAndPolledRefresh drives Subscribe, then
// SubscriptionPolledRefresh, through the real client. The client
// hardcodes WaitTime=500 and ReturnAllItems=false in every
// SubscriptionPolledRefresh call (see buildSubscriptionPolledRefreshPayload
// — not configurable through its public API), so this test waits for a
// real backend tick (memorybackend increments Demo/Counter every second)
// before polling, rather than relying on an immediate ReturnAllItems
// snapshot the client cannot request.
func TestRealClient_SubscribeAndPolledRefresh(t *testing.T) {
	client, _ := newTestServer(t)
	crh, cih := newHandles(t, 1)

	items := []gopcxmlda.TItem{{ItemName: "Demo/Counter"}}
	sub, err := client.Subscribe(context.Background(), items, &crh, &cih, "", true, 0, clientOptions())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if sub.Response.ServerSubHandle == "" {
		t.Fatalf("expected a non-empty ServerSubHandle")
	}
	if len(sub.Response.ItemList.Items) != 1 {
		t.Fatalf("got %d items on Subscribe (ReturnValuesOnReply=true), want 1", len(sub.Response.ItemList.Items))
	}

	// Real wait for a real backend tick — not a fake-clock scenario: this
	// test drives an actual client over actual HTTP against an actual
	// ticking backend, so there is no clock to fake here.
	time.Sleep(1200 * time.Millisecond)

	crh2, _ := newHandles(t, 0)
	refreshed, err := client.SubscriptionPolledRefresh(
		context.Background(), sub.Response.ServerSubHandle, 0, "", &crh2,
		clientOptions(), gopcxmlda.TServerTime{UseClientTime: true},
	)
	if err != nil {
		t.Fatalf("SubscriptionPolledRefresh: %v", err)
	}
	if len(refreshed.Response.InvalidServerSubHandles) != 0 {
		t.Fatalf("got InvalidServerSubHandles %v, want none", refreshed.Response.InvalidServerSubHandles)
	}
	if len(refreshed.Response.ItemList.Items) == 0 {
		t.Fatalf("expected at least one changed item (Demo/Counter ticks every second) after a 1.2s wait")
	}
	for _, item := range refreshed.Response.ItemList.Items {
		if item.ItemName != "Demo/Counter" || item.Timestamp.IsZero() {
			t.Errorf("got refreshed item %+v, want ItemName and Timestamp echoed per clientOptions", item)
		}
	}
}

// TestRealClient_RequestOptionsAreCaseSensitive pins down why
// clientOptions spells its keys in PascalCase. The reference client
// passes an options map straight through as XML attributes without
// validating or normalizing the keys, and XML attribute names are
// case-sensitive, so a lowercased key is not a differently-spelled
// request for the same option — it is an attribute the server has never
// heard of. Nothing rejects it: the request succeeds, and the option
// quietly keeps its false default, which looks exactly like a server that
// forgot to echo the field.
//
// ItemPath cannot be asserted here the way ItemName can: the server
// echoes it as an empty attribute for these items, which the client
// decodes into the same empty string an absent attribute produces. Name
// and Timestamp are the observable discriminators.
func TestRealClient_RequestOptionsAreCaseSensitive(t *testing.T) {
	client, _ := newTestServer(t)

	read := func(t *testing.T, options map[string]any) gopcxmlda.TItem {
		t.Helper()
		crh, cih := newHandles(t, 1)
		got, err := client.Read(context.Background(),
			[]gopcxmlda.TItem{{ItemName: "Demo/Switch"}}, &crh, &cih, "", options)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(got.Response.ItemList.Items) != 1 {
			t.Fatalf("got %d items, want 1", len(got.Response.ItemList.Items))
		}
		return got.Response.ItemList.Items[0]
	}

	t.Run("PascalCaseIsHonored", func(t *testing.T) {
		item := read(t, clientOptions())
		if item.ItemName != "Demo/Switch" {
			t.Errorf("got ItemName %q, want %q", item.ItemName, "Demo/Switch")
		}
		if item.Timestamp.IsZero() {
			t.Errorf("got no Timestamp, want one")
		}
	})

	t.Run("LowercasedKeysAreIgnored", func(t *testing.T) {
		item := read(t, map[string]any{
			"ReturnItemTime": true,
			"returnItemPath": true,
			"returnItemName": true,
		})
		if item.ItemName != "" {
			t.Errorf("got ItemName %q from a lowercased returnItemName; the server must not match attributes case-insensitively", item.ItemName)
		}
		// The one correctly-spelled key in that map still works, which is
		// what makes the mistake so easy to miss: the reply is not empty,
		// just missing the two fields whose keys were misspelled.
		if item.Timestamp.IsZero() {
			t.Errorf("got no Timestamp despite a correctly spelled ReturnItemTime")
		}
	})
}

// TestRealClient_SubscriptionCancel documents a second real
// interoperability finding: github.com/dernate/gopcxmlda's
// SubscriptionCancel unconditionally returns (true, nil) — its last
// statement ignores the accumulated errReturn entirely (confirmed by
// reading client.go). Independently of that client-side quirk, this test
// verifies this server's actual SubscriptionCancel behavior by polling
// the same handle again afterward and confirming the server now reports
// it invalid.
func TestRealClient_SubscriptionCancel(t *testing.T) {
	client, _ := newTestServer(t)
	crh, cih := newHandles(t, 1)

	items := []gopcxmlda.TItem{{ItemName: "Demo/Counter"}}
	sub, err := client.Subscribe(context.Background(), items, &crh, &cih, "", false, 0, clientOptions())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	handle := sub.Response.ServerSubHandle
	if handle == "" {
		t.Fatalf("expected a non-empty ServerSubHandle")
	}

	crh2, _ := newHandles(t, 0)
	ok, err := client.SubscriptionCancel(context.Background(), handle, "", &crh2)
	if err != nil {
		t.Fatalf("SubscriptionCancel: %v", err)
	}
	if !ok {
		t.Fatalf("SubscriptionCancel reported failure — if this ever happens, the client's own success-swallowing bug (see doc comment) no longer applies and this assertion is now meaningful")
	}

	crh3, _ := newHandles(t, 0)
	refreshed, err := client.SubscriptionPolledRefresh(
		context.Background(), handle, 0, "", &crh3,
		clientOptions(), gopcxmlda.TServerTime{UseClientTime: true},
	)
	// Only one handle was ever requested, and it is now invalid — per
	// this server's own documented behavior (matching
	// examples/basic-server/e2e_test.go's SubscriptionCancel subtest),
	// "all requested handles invalid" is a whole-operation E_NOSUBSCRIPTION
	// SOAP Fault, not a per-handle InvalidServerSubHandles entry (that
	// field is only populated when some, but not all, handles are
	// invalid). The client surfaces a fault as a non-nil error alongside
	// a populated Fault field.
	if err == nil {
		t.Fatalf("expected SubscriptionPolledRefresh to report the cancelled handle as invalid")
	}
	if !strings.Contains(refreshed.Fault.FaultCode, "E_NOSUBSCRIPTION") {
		t.Fatalf("got Fault %+v, want E_NOSUBSCRIPTION — SubscriptionCancel did not actually remove the subscription", refreshed.Fault)
	}
}
