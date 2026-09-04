// Package dockerintegration exercises this repository's OPC XML-DA
// server the way a real deployment would run it: built into an actual
// Docker image (test/dockerintegration/Dockerfile) from a nested,
// multi-datapoint address space (plantbackend), started as a real
// container, and driven entirely over real HTTP by the independently
// maintained reference client (github.com/dernate/gopcxmlda) — not this
// repository's own test fixtures, and not the in-process httptest server
// test/clientintegration uses. It catches classes of bug the in-process
// test cannot: a broken Dockerfile/build, a server that doesn't actually
// listen where the image says it does, or SIGTERM/graceful-shutdown
// behavior under a real container runtime.
//
// It lives in its own Go module (see go.mod), separate even from
// test/clientintegration, so the (heavier, Docker-daemon-requiring)
// testcontainers-go dependency doesn't leak into a test suite that
// otherwise needs nothing but the real client.
//
// Requires a working Docker daemon. If none is reachable, the test is
// skipped rather than failed — see testcontainers.SkipIfProviderIsNotHealthy.
package dockerintegration

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/dernate/gopcxmlda-server/xmlda"
)

// newDockerServer builds the image from test/dockerintegration/Dockerfile
// (build context is the repository root, two levels up from this test
// file, so both the main module and this test module's go.mod are
// visible to the build), starts it, and returns a real gopcxmlda.Server
// client pointed at its mapped port.
func newDockerServer(t *testing.T) *gopcxmlda.Server {
	t.Helper()
	client, _ := newDockerServerWithContainer(t)
	return client
}

// newDockerServerWithContainer is newDockerServer plus the running
// container itself, for tests that need more than the client handle —
// soak_test.go reads the server's own logs out of it to assert nothing
// panicked while it was under load.
func newDockerServerWithContainer(t *testing.T) (*gopcxmlda.Server, testcontainers.Container) {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:       "../..",
			Dockerfile:    "test/dockerintegration/Dockerfile",
			PrintBuildLog: true,
		},
		ExposedPorts: []string{"8080/tcp"},
		WaitingFor:   wait.ForListeningPort("8080/tcp").WithStartupTimeout(120 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("starting server container: %v", err)
	}
	t.Cleanup(func() {
		// SIGTERM (not a hard kill), so the container's own graceful
		// shutdown path (main.go's signal.NotifyContext) runs for real.
		tCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := container.Terminate(tCtx); err != nil {
			t.Errorf("terminating container: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container.Host: %v", err)
	}
	mapped, err := container.MappedPort(ctx, "8080/tcp")
	if err != nil {
		t.Fatalf("container.MappedPort: %v", err)
	}

	u, err := url.Parse(fmt.Sprintf("http://%s:%s", host, mapped.Port()))
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	// Timeout must be set explicitly — see test/clientintegration's
	// identical note: gopcxmlda.Server's zero value makes every request
	// time out almost instantly (10 nanoseconds, not 10 seconds).
	return &gopcxmlda.Server{Url: u, LocaleID: "en-US", Timeout: 10 * time.Second}, container
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
// the option falls back to its false default. test/clientintegration's TestRealClient_RequestOptionsAreCaseSensitive pins that down.
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

// TestDockerServer_AllOperations builds and starts the real container
// once, then drives every OPC XML-DA operation this library implements
// against it through the real client, in the order a client would
// naturally use them.
func TestDockerServer_AllOperations(t *testing.T) {
	client := newDockerServer(t)
	ctx := context.Background()

	t.Run("GetStatus", func(t *testing.T) {
		crh, _ := newHandles(t, 0)
		got, err := client.GetStatus(ctx, &crh, "")
		if err != nil {
			t.Fatalf("GetStatus: %v", err)
		}
		if got.Response.Result.ServerState != string(xmlda.ServerStateRunning) {
			t.Fatalf("got ServerState %q, want %q", got.Response.Result.ServerState, xmlda.ServerStateRunning)
		}
	})

	t.Run("BrowseNestedFolders", func(t *testing.T) {
		// Walk the address space four levels deep: Plant -> BuildingA ->
		// Line1 -> Motor1 -> {Speed, Temperature, Running}. Confirms
		// Browse recursion works against a real nested tree, not just a
		// single flat branch.
		crh, _ := newHandles(t, 0)
		root, err := client.Browse(ctx, "", &crh, "", gopcxmlda.TBrowseOptions{})
		if err != nil {
			t.Fatalf("Browse(root): %v", err)
		}
		if len(root.Response.Elements) != 1 || root.Response.Elements[0].Name != "Plant" {
			t.Fatalf("got root Elements %+v, want a single \"Plant\" branch", root.Response.Elements)
		}

		path := "Plant"
		for _, want := range []string{"BuildingA", "Line1", "Motor1"} {
			crhN, _ := newHandles(t, 0)
			level, err := client.Browse(ctx, "", &crhN, "", gopcxmlda.TBrowseOptions{ItemName: path})
			if err != nil {
				t.Fatalf("Browse(%s): %v", path, err)
			}
			found := false
			for _, e := range level.Response.Elements {
				if e.Name == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("Browse(%s) missing expected child %q, got %+v", path, want, level.Response.Elements)
			}
			path += "/" + want
		}

		crhLeaf, _ := newHandles(t, 0)
		leaf, err := client.Browse(ctx, "", &crhLeaf, "", gopcxmlda.TBrowseOptions{ItemName: path})
		if err != nil {
			t.Fatalf("Browse(%s): %v", path, err)
		}
		names := map[string]bool{}
		for _, e := range leaf.Response.Elements {
			names[e.Name] = true
		}
		for _, want := range []string{"Speed", "Temperature", "Running"} {
			if !names[want] {
				t.Fatalf("Browse(%s) missing expected item %q, got %+v", path, want, leaf.Response.Elements)
			}
		}
	})

	t.Run("ReadInitialValue", func(t *testing.T) {
		crh, cih := newHandles(t, 1)
		items := []gopcxmlda.TItem{{ItemName: "Plant/BuildingB/Tank1/Valve"}}
		got, err := client.Read(ctx, items, &crh, &cih, "", clientOptions())
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
		if item.ItemName != "Plant/BuildingB/Tank1/Valve" {
			t.Errorf("got ItemName %q, want it echoed as requested via ReturnItemName", item.ItemName)
		}
		if item.Timestamp.IsZero() {
			t.Errorf("got no Timestamp despite ReturnItemTime")
		}
		b, ok := item.Value.Value.(bool)
		if !ok || b != false {
			t.Fatalf("got Value %#v, want bool false (initial value)", item.Value.Value)
		}
	})

	t.Run("WriteThenReadBack", func(t *testing.T) {
		// See test/clientintegration's documented finding: the real
		// client's array-typed Write payloads decode correctly (unlike
		// its scalar Writes, which hit a known client-side xsi:type
		// namespace mismatch), so this uses an array to actually observe
		// a genuine value change end to end rather than an opaque-blob
		// write.
		//
		// The target is the ArrayOfInt item, not the scalar Speed. It
		// used to be Speed, and passed only because the fixture stored
		// whatever arrived — so this test was writing an array into an
		// item whose own dataType property says xsd:double, and turning
		// it into an array item for every test that ran afterwards. A
		// backend with one canonical type per item answers that with
		// E_BADTYPE, which is what a real server would do.
		const arrayItem = "Plant/BuildingB/Tank1/Setpoints"
		crh, cih := newHandles(t, 1)
		items := []gopcxmlda.TItem{{ItemName: arrayItem, Value: gopcxmlda.TValue{Value: []int32{1500}}}}
		_, err := client.Write(ctx, items, &crh, &cih, "", clientOptions())
		if err != nil {
			t.Fatalf("Write: %v", err)
		}

		crhRead, cihRead := newHandles(t, 1)
		readItems := []gopcxmlda.TItem{{ItemName: arrayItem}}
		got, err := client.Read(ctx, readItems, &crhRead, &cihRead, "", clientOptions())
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(got.Response.ItemList.Items) != 1 || got.Response.ItemList.Items[0].Error != "" {
			t.Fatalf("got %+v, want a single successful item after the write", got.Response.ItemList.Items)
		}
		if readBack := got.Response.ItemList.Items[0]; readBack.ItemName != arrayItem || readBack.Timestamp.IsZero() {
			t.Errorf("got ItemName %q / Timestamp %s, want both echoed per clientOptions", readBack.ItemName, readBack.Timestamp)
		}
	})

	t.Run("WriteUnknownItem_ReportsResultID", func(t *testing.T) {
		crh, cih := newHandles(t, 1)
		items := []gopcxmlda.TItem{{ItemName: "Plant/NoSuchItem", Value: gopcxmlda.TValue{Value: []bool{true}}}}
		got, err := client.Write(ctx, items, &crh, &cih, "", clientOptions())
		if err == nil {
			t.Fatalf("expected the client to surface the write error, got nil")
		}
		if len(got.Response.ItemList.Items) != 1 || got.Response.ItemList.Items[0].Error != "opc:E_UNKNOWNITEMNAME" {
			t.Fatalf("got items %+v, want a single opc:E_UNKNOWNITEMNAME result", got.Response.ItemList.Items)
		}
		// An item that carries no value at all must still echo its name
		// when asked — otherwise a client batching many writes cannot
		// tell which one of them failed.
		if name := got.Response.ItemList.Items[0].ItemName; name != "Plant/NoSuchItem" {
			t.Errorf("got ItemName %q on the failed item, want it echoed per ReturnItemName", name)
		}
	})

	t.Run("WriteReadOnlyItem_ReportsResultID", func(t *testing.T) {
		crh, cih := newHandles(t, 1)
		items := []gopcxmlda.TItem{{ItemName: "Plant/BuildingB/Tank1/Level", Value: gopcxmlda.TValue{Value: []int32{10}}}}
		got, err := client.Write(ctx, items, &crh, &cih, "", clientOptions())
		if err == nil {
			t.Fatalf("expected the client to surface the write error, got nil")
		}
		if len(got.Response.ItemList.Items) != 1 || got.Response.ItemList.Items[0].Error != "opc:E_READONLY" {
			t.Fatalf("got items %+v, want a single opc:E_READONLY result", got.Response.ItemList.Items)
		}
	})

	t.Run("GetProperties", func(t *testing.T) {
		crh, _ := newHandles(t, 0)
		items := []gopcxmlda.TItem{{ItemName: "Plant/BuildingB/Tank1/Level"}}
		opts := gopcxmlda.TPropertyOptions{ReturnAllProperties: true, ReturnPropertyValues: true, ReturnErrorText: true}
		got, err := client.GetProperties(ctx, items, opts, &crh, "")
		if err != nil {
			t.Fatalf("GetProperties: %v", err)
		}
		if len(got.Response.PropertyList) != 1 {
			t.Fatalf("got %d property lists, want 1", len(got.Response.PropertyList))
		}
		// TValue wraps the value: .Value is the content, .Type the
		// xsi:type's local name.
		values := map[string]string{}
		types := map[string]string{}
		for _, p := range got.Response.PropertyList[0].Properties {
			values[p.Name] = fmt.Sprintf("%v", p.Value.Value)
			types[p.Name] = p.Value.Type
		}
		// The standard property surface a real client reaches for
		// (§3.1.10 pp.39-40, IDs 1/4/5/6/7 plus description).
		for _, want := range []string{
			"opc:dataType", "opc:quality", "opc:timestamp", "opc:accessRights",
			"opc:scanRate", "opc:euType", "opc:description",
		} {
			if _, ok := values[want]; !ok {
				t.Errorf("property %s missing; got %+v", want, got.Response.PropertyList[0].Properties)
			}
		}
		// accessRights is not a free string: §3.1.10 p.40 says "one of
		// the following valid values must be used", and Tank1/Level is
		// read-only.
		if got := values["opc:accessRights"]; got != "readable" {
			t.Errorf("accessRights = %q, want %q", got, "readable")
		}
		// An item with an engineering range is the specification's
		// analog case, and Level has none in this fixture.
		if got := values["opc:euType"]; got != "noEnum" && got != "analog" {
			t.Errorf("euType = %q, want one of noEnum/analog", got)
		}
		// Property 3's declared data type is OPCQuality (§3.1.10 p.40) —
		// the one complex type this protocol puts in a <Value> position,
		// and the reason xmlda.KindQuality exists. Asserting the type
		// rather than the content: an OPCQuality carries its state in
		// attributes, so it has no text for the client to report.
		if got := types["opc:quality"]; got != "OPCQuality" {
			t.Errorf("quality property type = %q, want OPCQuality", got)
		}
		if got := types["opc:dataType"]; got != "QName" {
			t.Errorf("dataType property type = %q, want QName", got)
		}
	})

	var subHandle string
	t.Run("SubscribeAndPolledRefresh", func(t *testing.T) {
		// Plant/BuildingB/Tank1/Level is one of plantbackend's
		// tickingFloat64Items, nudged every second — real changing data
		// to observe through a real poll, not a synthetic snapshot.
		crh, cih := newHandles(t, 1)
		items := []gopcxmlda.TItem{{ItemName: "Plant/BuildingB/Tank1/Level"}}
		sub, err := client.Subscribe(ctx, items, &crh, &cih, "", true, 0, clientOptions())
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		if sub.Response.ServerSubHandle == "" {
			t.Fatalf("expected a non-empty ServerSubHandle")
		}
		subHandle = sub.Response.ServerSubHandle

		time.Sleep(1200 * time.Millisecond)

		crh2, _ := newHandles(t, 0)
		refreshed, err := client.SubscriptionPolledRefresh(
			ctx, subHandle, 0, "", &crh2,
			clientOptions(), gopcxmlda.TServerTime{UseClientTime: true},
		)
		if err != nil {
			t.Fatalf("SubscriptionPolledRefresh: %v", err)
		}
		if len(refreshed.Response.InvalidServerSubHandles) != 0 {
			t.Fatalf("got InvalidServerSubHandles %v, want none", refreshed.Response.InvalidServerSubHandles)
		}
		if len(refreshed.Response.ItemList.Items) == 0 {
			t.Fatalf("expected at least one changed item (Tank1/Level ticks every second) after a 1.2s wait")
		}
		for _, item := range refreshed.Response.ItemList.Items {
			if item.ItemName != "Plant/BuildingB/Tank1/Level" || item.Timestamp.IsZero() {
				t.Errorf("got refreshed item %+v, want ItemName and Timestamp echoed per clientOptions", item)
			}
		}
	})

	t.Run("SubscriptionCancel", func(t *testing.T) {
		if subHandle == "" {
			t.Fatalf("SubscribeAndPolledRefresh subtest must run first")
		}
		crh, _ := newHandles(t, 0)
		ok, err := client.SubscriptionCancel(ctx, subHandle, "", &crh)
		if err != nil {
			t.Fatalf("SubscriptionCancel: %v", err)
		}
		if !ok {
			t.Fatalf("SubscriptionCancel reported failure")
		}

		crh2, _ := newHandles(t, 0)
		refreshed, err := client.SubscriptionPolledRefresh(
			ctx, subHandle, 0, "", &crh2,
			clientOptions(), gopcxmlda.TServerTime{UseClientTime: true},
		)
		if err == nil {
			t.Fatalf("expected SubscriptionPolledRefresh to report the cancelled handle as invalid")
		}
		if !strings.Contains(refreshed.Fault.FaultCode, "E_NOSUBSCRIPTION") {
			t.Fatalf("got Fault %+v, want E_NOSUBSCRIPTION — SubscriptionCancel did not actually remove the subscription", refreshed.Fault)
		}
	})
}
