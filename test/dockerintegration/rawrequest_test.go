package dockerintegration

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/soap"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// Container-level regression coverage for the tolerant-input, continuation-
// point and resource-limit behavior.
//
// The value this file adds over test/clientintegration's own versions of
// these tests is the transport and the process: a real container running
// the real server binary with the real Config defaults, reached over a
// real TCP socket through a mapped port. Notably it is the only place the
// SHIPPED defaults are exercised — the in-process tests set Config fields
// to keep themselves fast, so a default that is wrong only shows up here.
//
// Every test here posts hand-written request bytes, because that is the
// point: these fixes are about accepting input a conforming client library
// would never generate.

const rawEnvelopeOpen = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/"><SOAP-ENV:Body>`
const rawEnvelopeClose = `</SOAP-ENV:Body></SOAP-ENV:Envelope>`

// containerURL starts the container (once per test) and returns the base
// URL of its mapped port, for tests that post raw request bodies rather
// than driving the reference client.
func containerURL(t *testing.T) string {
	t.Helper()
	client := newDockerServer(t)
	return client.Url.String()
}

func rawPost(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	status, data, err := doRawPost(url, body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return status, data
}

// doRawPost has no *testing.T dependency, so it is safe to call from a
// goroutine other than the test's own.
func doRawPost(url, body string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

func rawDecode[T any](t *testing.T, data []byte) *T {
	t.Helper()
	var env soap.Envelope[T]
	if err := xmlda.Decode(data, &env); err != nil {
		t.Fatalf("decoding response: %v\nbody: %s", err, data)
	}
	if env.Body.Fault != nil {
		t.Fatalf("unexpected fault %s: %s", env.Body.Fault.Code, env.Body.Fault.Text)
	}
	if env.Body.Content == nil {
		t.Fatal("response carried no content")
	}
	return env.Body.Content
}

func rawFault(t *testing.T, data []byte) *soap.Fault {
	t.Helper()
	var env soap.Envelope[struct{}]
	if err := xml.Unmarshal(data, &env); err != nil {
		t.Fatalf("decoding fault envelope: %v\nbody: %s", err, data)
	}
	if env.Body.Fault == nil {
		t.Fatalf("expected a SOAP fault, got: %s", data)
	}
	return env.Body.Fault
}

const dockerItem = "Plant/BuildingB/Tank1/Level"

// TestDockerServer_TolerantInput drives the malformed-item, offsetless-
// dateTime and failing-item-quality cases against the containerized server
// in one pass, so the (slow) image build
// and container start are paid for once.
//
// They belong together anyway: all three are about a real client sending
// something slightly off the happy path and the server answering usefully
// instead of discarding the whole exchange.
func TestDockerServer_TolerantInput(t *testing.T) {
	url := containerURL(t)

	t.Run("malformed item does not fail the batch", func(t *testing.T) {
		const n = 12
		const badIdx = 5
		var items strings.Builder
		for i := range n {
			_, _ = fmt.Fprintf(&items, `<Items ItemName="%s" ClientItemHandle="H%d"`, dockerItem, i)
			if i == badIdx {
				items.WriteString(` MaxAge="not-a-number"`)
			}
			items.WriteString(`/>`)
		}
		body := rawEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
			`<Options ReturnItemName="true" ReturnDiagnosticInfo="true"/><ItemList>` +
			items.String() + `</ItemList></Read>` + rawEnvelopeClose

		status, data := rawPost(t, url, body)
		if status != http.StatusOK {
			t.Fatalf("got status %d, want 200: %s", status, data)
		}
		out := rawDecode[xmlda.ReadResponse](t, data)
		if len(out.RItemList.Items) != n {
			t.Fatalf("got %d reply items, want %d", len(out.RItemList.Items), n)
		}
		for i, iv := range out.RItemList.Items {
			if i == badIdx {
				if iv.ResultID != xmlda.ErrFail {
					t.Errorf("item %d: ResultID = %v, want E_FAIL", i, iv.ResultID)
				}
				continue
			}
			if !iv.ResultID.IsZero() || iv.Value == nil {
				t.Errorf("item %d lost its value over one unrelated bad item: %+v", i, iv)
			}
		}
	})

	t.Run("offsetless dateTime is accepted", func(t *testing.T) {
		subBody := rawEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" ReturnValuesOnReply="true" SubscriptionPingRate="30000">` +
			`<Options ReturnItemName="true"/><ItemList>` +
			`<Items ItemName="` + dockerItem + `" ClientItemHandle="C1"/>` +
			`</ItemList></Subscribe>` + rawEnvelopeClose
		status, data := rawPost(t, url, subBody)
		if status != http.StatusOK {
			t.Fatalf("Subscribe got status %d: %s", status, data)
		}
		sub := rawDecode[xmlda.SubscribeResponse](t, data)
		if sub.ServerSubHandle == "" {
			t.Fatal("no subscription handle")
		}
		t.Cleanup(func() {
			rawPost(t, url, rawEnvelopeOpen+
				`<SubscriptionCancel xmlns="`+xmlda.Namespace+`" ServerSubHandle="`+sub.ServerSubHandle+`"/>`+
				rawEnvelopeClose)
		})

		// No timezone offset: legal xsd:dateTime, illegal RFC 3339. The
		// container's clock is UTC, and HoldTime is interpreted as server
		// time, so a UTC-formatted value is the right thing to send.
		hold := time.Now().UTC().Add(300 * time.Millisecond).Format("2006-01-02T15:04:05.000")
		pollBody := rawEnvelopeOpen + `<SubscriptionPolledRefresh xmlns="` + xmlda.Namespace + `"` +
			` HoldTime="` + hold + `" WaitTime="0" ReturnAllItems="true">` +
			`<Options ReturnItemName="true" ReturnItemTime="true"/>` +
			`<ServerSubHandles>` + sub.ServerSubHandle + `</ServerSubHandles>` +
			`</SubscriptionPolledRefresh>` + rawEnvelopeClose

		status, data = rawPost(t, url, pollBody)
		if status != http.StatusOK {
			t.Fatalf("an offsetless HoldTime faulted the poll (status %d): %s", status, data)
		}
		poll := rawDecode[xmlda.SubscriptionPolledRefreshResponse](t, data)
		if len(poll.RItemList) != 1 {
			t.Fatalf("got %d subscription result lists, want 1", len(poll.RItemList))
		}

		// The same widening on the Write path.
		wBody := rawEnvelopeOpen + `<Write xmlns="` + xmlda.Namespace + `"` +
			` xmlns:xsi="` + xmlda.XSINamespace + `" xmlns:xsd="` + xmlda.XSDNamespace + `">` +
			`<Options ReturnItemName="true"/><ItemList>` +
			`<Items ItemName="Plant/BuildingB/Tank1/Label" Timestamp="2026-08-30T12:00:00">` +
			`<Value xsi:type="xsd:string">from-docker</Value></Items>` +
			`</ItemList></Write>` + rawEnvelopeClose
		status, data = rawPost(t, url, wBody)
		if status != http.StatusOK {
			t.Fatalf("an offsetless item Timestamp faulted the Write (status %d): %s", status, data)
		}
		rawDecode[xmlda.WriteResponse](t, data)
	})

	t.Run("failed item reports bad quality", func(t *testing.T) {
		body := rawEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `">` +
			`<Options ReturnItemName="true"/><ItemList>` +
			`<Items ItemName="` + dockerItem + `" ClientItemHandle="OK"/>` +
			`<Items ItemName="No/Such/Tag/Anywhere" ClientItemHandle="BAD"/>` +
			`</ItemList></Read>` + rawEnvelopeClose
		status, data := rawPost(t, url, body)
		if status != http.StatusOK {
			t.Fatalf("got status %d: %s", status, data)
		}
		// One <Quality> per item, and the failing one must SAY bad. An
		// attribute-less <Quality/> reads as QualityField="good" under the
		// schema's own defaults — and so does a missing element, which is
		// why omitting it (the first attempt at this) had the same failure
		// mode one step removed. §2.6 p.22 states the quality outright:
		// <Items ResultID="E_UNKNOWNITEMNAME"><Quality QualityField="bad"/></Items>.
		if n := strings.Count(string(data), "<Quality"); n != 2 {
			t.Errorf("the response carries %d Quality elements, want 2 (one per item):\n%s", n, data)
		}
		if !strings.Contains(string(data), `QualityField="bad"`) {
			t.Errorf("the failing item's Quality does not spell out bad:\n%s", data)
		}
		out := rawDecode[xmlda.ReadResponse](t, data)
		for _, iv := range out.RItemList.Items {
			if iv.ClientItemHandle != "BAD" {
				continue
			}
			if iv.ResultID.IsZero() {
				t.Error("the unknown item reported no condition")
			}
			if iv.Quality == nil {
				t.Error("the failing item carries no Quality; the schema default then reads as good")
			} else if got := iv.Quality.QualityField(); got != xmlda.QualityBad {
				t.Errorf("the failing item reports quality %q, want %q", got, xmlda.QualityBad)
			}
		}
	})
}

// TestDockerServer_ContinuationPointIsAuthenticated pins token authenticity
// against the container. plantbackend paginates for real, so this exercises the
// server's HMAC wrapping over a genuine backend cursor rather than a
// test double.
func TestDockerServer_ContinuationPointIsAuthenticated(t *testing.T) {
	url := containerURL(t)

	browse := func(attrs string) (int, []byte) {
		return rawPost(t, url, rawEnvelopeOpen+
			`<Browse xmlns="`+xmlda.Namespace+`" `+attrs+`/>`+rawEnvelopeClose)
	}

	// One element per page against a branch with several children.
	status, data := browse(`ItemName="Plant/BuildingA" MaxElementsReturned="1"`)
	if status != http.StatusOK {
		t.Fatalf("Browse got status %d: %s", status, data)
	}
	first := rawDecode[xmlda.BrowseResponse](t, data)
	if first.ContinuationPoint == "" {
		t.Fatalf("no continuation point for a paged Browse; MoreElements=%v, %d elements",
			first.MoreElements, len(first.Elements))
	}

	// Replay works; a page-size change works; a tampered cursor does not.
	if status, data := browse(`ItemName="Plant/BuildingA" MaxElementsReturned="1" ContinuationPoint="` +
		first.ContinuationPoint + `"`); status != http.StatusOK {
		t.Fatalf("a legitimate continuation point was rejected (status %d): %s", status, data)
	}
	if status, data := browse(`ItemName="Plant/BuildingA" MaxElementsReturned="4" ContinuationPoint="` +
		first.ContinuationPoint + `"`); status != http.StatusOK {
		t.Fatalf("changing the page size mid-pagination was rejected (status %d): %s", status, data)
	}

	idx := strings.LastIndex(first.ContinuationPoint, ":")
	forged := first.ContinuationPoint[:idx+1] + "forged-cursor"
	status, data = browse(`ItemName="Plant/BuildingA" MaxElementsReturned="1" ContinuationPoint="` + forged + `"`)
	if status == http.StatusOK {
		t.Fatalf("a forged cursor was accepted: %s", data)
	}
	if f := rawFault(t, data); f.Code.Local != "E_INVALIDCONTINUATIONPOINT" {
		t.Errorf("got fault %s, want E_INVALIDCONTINUATIONPOINT", f.Code)
	}

	// An invalid BrowseFilter is refused rather than forwarded to the backend.
	status, data = browse(`BrowseFilter="everything"`)
	if status == http.StatusOK {
		t.Fatalf("an invalid BrowseFilter was accepted: %s", data)
	}
	if f := rawFault(t, data); f.Code.Local != "E_INVALIDFILTER" {
		t.Errorf("got fault %s, want E_INVALIDFILTER", f.Code)
	}
}

// TestDockerServer_DefaultLimitsAreUsable pins the SHIPPED defaults, which
// no in-process test covers: the container runs server.Config{}, so a
// default that is too tight to be usable — or too loose to bound
// anything — shows up only here.
//
// It covers both the HoldTime ceiling (an over-long HoldTime is clamped, not
// rejected) and the concurrency limit (bounded, but generously).
func TestDockerServer_DefaultLimitsAreUsable(t *testing.T) {
	url := containerURL(t)

	t.Run("two-minute HoldTime is honored under the defaults", func(t *testing.T) {
		if testing.Short() {
			// This subtest genuinely blocks for the full default ceiling —
			// that IS the assertion — so it is skipped where a fast run is
			// asked for, the same way the soak test is.
			t.Skip("-short: this subtest holds for the full 120s default ceiling")
		}
		// The specification's own guidance is "generally no more than a
		// minute or two" (§3.1.6). Two minutes must therefore work against
		// the shipped default — which is exactly what the old 90s ceiling
		// turned into a fault on every poll.
		subBody := rawEnvelopeOpen + `<Subscribe xmlns="` + xmlda.Namespace + `" SubscriptionPingRate="30000">` +
			`<Options/><ItemList><Items ItemName="` + dockerItem + `" ClientItemHandle="C1"/></ItemList>` +
			`</Subscribe>` + rawEnvelopeClose
		_, data := rawPost(t, url, subBody)
		sub := rawDecode[xmlda.SubscribeResponse](t, data)
		t.Cleanup(func() {
			rawPost(t, url, rawEnvelopeOpen+
				`<SubscriptionCancel xmlns="`+xmlda.Namespace+`" ServerSubHandle="`+sub.ServerSubHandle+`"/>`+
				rawEnvelopeClose)
		})

		// WaitTime 0 and ReturnAllItems, with a HoldTime just past now: the
		// point is that a two-minute-scale HoldTime is ACCEPTED, not that
		// the test waits two minutes for it.
		hold := time.Now().UTC().Add(2 * time.Minute).Format("2006-01-02T15:04:05.000Z")
		pollBody := rawEnvelopeOpen + `<SubscriptionPolledRefresh xmlns="` + xmlda.Namespace + `"` +
			` HoldTime="` + hold + `" WaitTime="0" ReturnAllItems="true">` +
			`<Options/><ServerSubHandles>` + sub.ServerSubHandle + `</ServerSubHandles>` +
			`</SubscriptionPolledRefresh>` + rawEnvelopeClose

		start := time.Now()
		status, data := rawPost(t, url, pollBody)
		elapsed := time.Since(start)
		if status != http.StatusOK {
			t.Fatalf("a two-minute HoldTime faulted under the shipped defaults (status %d): %s", status, data)
		}
		rawDecode[xmlda.SubscriptionPolledRefreshResponse](t, data)
		// It must actually have held for about the full two minutes, since
		// the default ceiling is now 120s — proof the request was honored
		// rather than silently answered immediately.
		if elapsed < 100*time.Second {
			t.Errorf("the two-minute hold returned after %v; it was neither honored nor clamped to the 120s default", elapsed)
		}
	})

	t.Run("ordinary concurrency is not refused", func(t *testing.T) {
		// The default MaxConcurrentRequests is 1024, so a burst well inside
		// it must all succeed: a limit that refuses ordinary traffic is
		// worse than no limit.
		body := rawEnvelopeOpen + `<Read xmlns="` + xmlda.Namespace + `"><Options/><ItemList>` +
			`<Items ItemName="` + dockerItem + `"/></ItemList></Read>` + rawEnvelopeClose

		const n = 48
		var wg sync.WaitGroup
		var mu sync.Mutex
		statuses := map[int]int{}
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				status, _, err := doRawPost(url, body)
				if err != nil {
					mu.Lock()
					statuses[-1]++
					mu.Unlock()
					return
				}
				mu.Lock()
				statuses[status]++
				mu.Unlock()
			}()
		}
		wg.Wait()

		mu.Lock()
		defer mu.Unlock()
		if statuses[http.StatusOK] != n {
			t.Errorf("%d of %d concurrent reads succeeded under the default limit: %v",
				statuses[http.StatusOK], n, statuses)
		}
	})
}
