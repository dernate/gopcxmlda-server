package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/examples/basic-server/memorybackend"
	"github.com/dernate/gopcxmlda-server/server"
	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

const soapEnvelopeOpen = `<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/"><SOAP-ENV:Body>`
const soapEnvelopeClose = `</SOAP-ENV:Body></SOAP-ENV:Envelope>`

func getStatusRequestBody() string {
	return soapEnvelopeOpen + `<GetStatus xmlns="` + xmlda.Namespace + `" ClientRequestHandle="CRH1"/>` + soapEnvelopeClose
}

func browseRequestBody(itemName string) string {
	attr := ""
	if itemName != "" {
		attr = ` ItemName="` + itemName + `"`
	}
	return soapEnvelopeOpen + `<Browse xmlns="` + xmlda.Namespace + `"` + attr + `/>` + soapEnvelopeClose
}

func readRequestBody(itemName string) string {
	return soapEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `"><Options ClientRequestHandle="CRH1"/><ItemList>` +
		`<Items ItemName="` + itemName + `"/></ItemList></Read>` + soapEnvelopeClose
}

func writeRequestBody(itemName, xsiType, value string) string {
	return soapEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="false">` +
		`<Options ClientRequestHandle="CRH1"/><ItemList><Items ItemName="` + itemName + `">` +
		`<Value xmlns:xsd="` + xmlda.XSDNamespace + `" xmlns:xsi="` + xmlda.XSINamespace + `" xsi:type="xsd:` + xsiType + `">` + value + `</Value>` +
		`</Items></ItemList></Write>` + soapEnvelopeClose
}

func getPropertiesRequestBody(itemName string) string {
	return soapEnvelopeOpen + `<GetProperties xmlns="` + xmlda.Namespace + `" ReturnAllProperties="true">` +
		`<ItemIDs ItemName="` + itemName + `"/></GetProperties>` + soapEnvelopeClose
}

func subscribeRequestBody(itemName, clientItemHandle string) string {
	return soapEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="true" SubscriptionPingRate="0">` +
		`<Options ClientRequestHandle="CRH1"/><ItemList><Items ItemName="` + itemName + `" ClientItemHandle="` + clientItemHandle + `"/></ItemList></Subscribe>` + soapEnvelopeClose
}

func polledRefreshRequestBody(handle string, waitTimeMs int, returnAllItems bool) string {
	return soapEnvelopeOpen + `<SubscriptionPolledRefresh xmlns="` + xmlda.Namespace + `" WaitTime="` + strconv.Itoa(waitTimeMs) +
		`" ReturnAllItems="` + strconv.FormatBool(returnAllItems) + `">` +
		`<ServerSubHandles>` + handle + `</ServerSubHandles></SubscriptionPolledRefresh>` + soapEnvelopeClose
}

func subscriptionCancelRequestBody(handle string) string {
	return soapEnvelopeOpen + `<SubscriptionCancel xmlns="` + xmlda.Namespace + `" ServerSubHandle="` + handle + `"/>` + soapEnvelopeClose
}

func postSOAP(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "text/xml; charset=utf-8", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

func decodeFault(t *testing.T, resp *http.Response) *soap.Fault {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	var env soap.Envelope[struct{}]
	if err := xmlda.Decode(data, &env); err != nil {
		t.Fatalf("decoding response envelope: %v\nbody: %s", err, data)
	}
	if env.Body.Fault == nil {
		t.Fatalf("expected a SOAP fault, got none\nbody: %s", data)
	}
	return env.Body.Fault
}

func decodeResponse[T any](t *testing.T, resp *http.Response) *T {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	var env soap.Envelope[T]
	if err := xmlda.Decode(data, &env); err != nil {
		t.Fatalf("decoding response: %v\nbody: %s", err, data)
	}
	if env.Body.Fault != nil {
		t.Fatalf("unexpected fault: %+v\nbody: %s", env.Body.Fault, data)
	}
	if env.Body.Content == nil {
		t.Fatalf("expected non-nil response content\nbody: %s", data)
	}
	return env.Body.Content
}

// TestBasicServerEndToEnd drives the example server's real memorybackend
// through a real HTTP round trip, exercising every operation the example
// is meant to demonstrate: GetStatus, Browse (root and one level down),
// Read, Write, GetProperties, Subscribe, SubscriptionPolledRefresh, and
// SubscriptionCancel, followed by a controlled Shutdown.
func TestBasicServerEndToEnd(t *testing.T) {
	be := memorybackend.New()
	defer be.Close()

	h, err := server.New(server.Deps{
		Backend: backend.Backend{Status: be, Reader: be, Writer: be, Browser: be, Properties: be},
	}, server.Config{})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Shutdown(ctx); err != nil {
			t.Errorf("Handler.Shutdown: %v", err)
		}
	})

	ts := httptest.NewServer(h)
	defer ts.Close()

	t.Run("GetStatus", func(t *testing.T) {
		got := decodeResponse[xmlda.GetStatusResponse](t, postSOAP(t, ts.URL, getStatusRequestBody()))
		if got.Result.ServerState != xmlda.ServerStateRunning {
			t.Fatalf("got ServerState %q, want running", got.Result.ServerState)
		}
		if len(got.Status.SupportedLocaleIDs) == 0 {
			t.Fatalf("expected at least one supported locale")
		}
	})

	t.Run("BrowseRoot", func(t *testing.T) {
		got := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, ts.URL, browseRequestBody("")))
		if len(got.Elements) != 1 || got.Elements[0].Name != "Demo" {
			t.Fatalf("got Elements %+v, want a single \"Demo\" branch", got.Elements)
		}
		if got.Elements[0].IsItem {
			t.Fatalf("Demo should be a branch, not an item")
		}
		if !got.Elements[0].HasChildren {
			t.Fatalf("Demo should report HasChildren=true")
		}
	})

	t.Run("BrowseDemo", func(t *testing.T) {
		got := decodeResponse[xmlda.BrowseResponse](t, postSOAP(t, ts.URL, browseRequestBody("Demo")))
		names := map[string]bool{}
		for _, e := range got.Elements {
			names[e.Name] = true
			if !e.IsItem {
				t.Fatalf("element %q should be an item", e.Name)
			}
		}
		for _, want := range []string{"Counter", "Temperature", "Switch", "Message"} {
			if !names[want] {
				t.Fatalf("Browse(Demo) missing expected item %q, got %+v", want, got.Elements)
			}
		}
	})

	t.Run("ReadInitialSwitch", func(t *testing.T) {
		got := decodeResponse[xmlda.ReadResponse](t, postSOAP(t, ts.URL, readRequestBody("Demo/Switch")))
		if len(got.RItemList.Items) != 1 {
			t.Fatalf("got %d items, want 1", len(got.RItemList.Items))
		}
		item := got.RItemList.Items[0]
		if !item.ResultID.IsZero() {
			t.Fatalf("unexpected ResultID %+v", item.ResultID)
		}
		if item.Value == nil {
			t.Fatalf("expected a Value")
		}
		b, err := item.Value.Bool()
		if err != nil {
			t.Fatalf("Value.Bool: %v", err)
		}
		if b != false {
			t.Fatalf("got Switch=%v, want false (initial value)", b)
		}
	})

	t.Run("WriteSwitch", func(t *testing.T) {
		got := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, ts.URL, writeRequestBody("Demo/Switch", "boolean", "true")))
		if len(got.RItemList.Items) != 1 || !got.RItemList.Items[0].ResultID.IsZero() {
			t.Fatalf("got %+v, want a single successful item", got.RItemList.Items)
		}

		read := decodeResponse[xmlda.ReadResponse](t, postSOAP(t, ts.URL, readRequestBody("Demo/Switch")))
		b, err := read.RItemList.Items[0].Value.Bool()
		if err != nil {
			t.Fatalf("Value.Bool: %v", err)
		}
		if b != true {
			t.Fatalf("got Switch=%v after write, want true", b)
		}
	})

	t.Run("WriteUnknownItemFails", func(t *testing.T) {
		got := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, ts.URL, writeRequestBody("Demo/NoSuchItem", "boolean", "true")))
		if len(got.RItemList.Items) != 1 || got.RItemList.Items[0].ResultID != xmlda.ErrUnknownItemName {
			t.Fatalf("got %+v, want E_UNKNOWNITEMNAME", got.RItemList.Items)
		}
	})

	t.Run("WriteReadOnlyItemFails", func(t *testing.T) {
		got := decodeResponse[xmlda.WriteResponse](t, postSOAP(t, ts.URL, writeRequestBody("Demo/Temperature", "double", "10")))
		if len(got.RItemList.Items) != 1 || got.RItemList.Items[0].ResultID != xmlda.ErrReadOnly {
			t.Fatalf("got %+v, want E_READONLY", got.RItemList.Items)
		}
	})

	t.Run("GetProperties", func(t *testing.T) {
		got := decodeResponse[xmlda.GetPropertiesResponse](t, postSOAP(t, ts.URL, getPropertiesRequestBody("Demo/Temperature")))
		if len(got.PropertyLists) != 1 {
			t.Fatalf("got %d property lists, want 1", len(got.PropertyLists))
		}
		pl := got.PropertyLists[0]
		if !pl.ResultID.IsZero() {
			t.Fatalf("unexpected ResultID %+v", pl.ResultID)
		}
		found := map[string]bool{}
		for _, p := range pl.Properties {
			found[p.Name.Local] = true
		}
		if !found["dataType"] || !found["description"] {
			t.Fatalf("got properties %+v, want at least dataType and description", pl.Properties)
		}
	})

	var subHandle string
	t.Run("Subscribe", func(t *testing.T) {
		got := decodeResponse[xmlda.SubscribeResponse](t, postSOAP(t, ts.URL, subscribeRequestBody("Demo/Counter", "CIH1")))
		if got.ServerSubHandle == "" {
			t.Fatalf("expected a non-empty ServerSubHandle")
		}
		if len(got.RItemList.Items) != 1 {
			t.Fatalf("got %d items, want 1 (ReturnValuesOnReply=true)", len(got.RItemList.Items))
		}
		subHandle = got.ServerSubHandle
	})

	t.Run("PolledRefreshImmediateSnapshot", func(t *testing.T) {
		if subHandle == "" {
			t.Fatalf("Subscribe subtest must run first")
		}
		got := decodeResponse[xmlda.SubscriptionPolledRefreshResponse](t, postSOAP(t, ts.URL, polledRefreshRequestBody(subHandle, 0, true)))
		if len(got.InvalidServerSubHandles) != 0 {
			t.Fatalf("got InvalidServerSubHandles %v, want none", got.InvalidServerSubHandles)
		}
		if len(got.RItemList) != 1 || len(got.RItemList[0].Items) != 1 {
			t.Fatalf("got RItemList %+v, want one subscription with one item", got.RItemList)
		}
	})

	t.Run("SubscriptionCancel", func(t *testing.T) {
		if subHandle == "" {
			t.Fatalf("Subscribe subtest must run first")
		}
		got := decodeResponse[xmlda.SubscriptionCancelResponse](t, postSOAP(t, ts.URL, subscriptionCancelRequestBody(subHandle)))
		_ = got

		// Refreshing the now-cancelled (and only) handle leaves none valid,
		// which is a whole-operation E_NOSUBSCRIPTION fault, not a per-item
		// result — see subscription.Manager.PolledRefresh.
		f := decodeFault(t, postSOAP(t, ts.URL, polledRefreshRequestBody(subHandle, 0, true)))
		if f.Code.Local != "E_NOSUBSCRIPTION" {
			t.Fatalf("got fault %+v, want E_NOSUBSCRIPTION", f)
		}
	})
}
