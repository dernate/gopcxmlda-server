// This file adds what TestDockerServer_AllOperations deliberately does
// not cover. That test proves each operation is *correct*; it finishes
// in milliseconds because a localhost round trip against an in-memory
// backend is genuinely that fast. What it cannot prove is that the
// server is still correct after running for a while under continuous,
// concurrent load — that class of bug (a subscription silently dying
// mid-run, a leaked/never-rearmed timer, a background panic, stale data
// served from a frozen snapshot, an abandonment reaper that never
// actually reaps) only shows up on the clock.
//
// So this test drives the real container for a sustained window with
// several concurrent clients and asserts properties that are only
// meaningful over time. Duration defaults to soakDefaultDuration and is
// overridable with DOCKERINTEGRATION_SOAK (e.g. "2m"); `go test -short`
// skips it entirely.
package dockerintegration

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda"

	"github.com/dernate/gopcxmlda-server/xmlda"
)

const (
	// soakDefaultDuration is long enough for the abandonment reaper to
	// provably fire within the load window (see soakReapWindow) without
	// making CI crawl.
	soakDefaultDuration = 30 * time.Second

	// soakPingRate is the SubscriptionPingRate (milliseconds) requested
	// for both subscriptions. It doubles as the poller's HoldTime offset:
	// the reference client derives HoldTime = now + pingRate, so every
	// SubscriptionPolledRefresh below is a real ~2s long-poll (server
	// holds the request, then honors the client's fixed WaitTime=500ms)
	// rather than the instant snapshot the fast test takes with 0.
	soakPingRate = 2000

	// soakReapWindow is how long an unpolled subscription needs before
	// the server is guaranteed to have reaped it, derived from the
	// defaults main.go inherits by passing server.Config{}:
	// ReapGraceMultiplier 2.0 x pingRate 2s = 4s of grace, plus up to one
	// full ReapInterval (10s) before the next sweep sees it, plus slack.
	soakReapWindow = 2*soakPingRate*time.Millisecond + 10*time.Second + 6*time.Second
)

func soakDuration(t *testing.T) time.Duration {
	t.Helper()
	v := os.Getenv("DOCKERINTEGRATION_SOAK")
	if v == "" {
		return soakDefaultDuration
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("DOCKERINTEGRATION_SOAK=%q is not a valid duration: %v", v, err)
	}
	if d <= 0 {
		t.Fatalf("DOCKERINTEGRATION_SOAK=%q must be positive", v)
	}
	return d
}

// soakClient returns an independent client value for one worker. The
// reference client stores no per-request state, but it does write to
// Server.Timeout when it is zero and it mutates the options map it is
// handed, so each concurrent worker gets its own Server value and every
// call below gets a freshly allocated options map.
func soakClient(base *gopcxmlda.Server) *gopcxmlda.Server {
	c := *base
	return &c
}

// soakLog collects failures from worker goroutines, which must not call
// t.Fatalf themselves. It keeps a bounded sample but counts everything,
// so a storm of identical failures is reported as such instead of
// flooding the test output.
type soakLog struct {
	mu     sync.Mutex
	sample []string
	total  atomic.Int64
}

func (l *soakLog) errf(format string, args ...any) {
	l.total.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.sample) < 15 {
		l.sample = append(l.sample, fmt.Sprintf(format, args...))
	}
}

func (l *soakLog) report(t *testing.T, what string) {
	t.Helper()
	n := l.total.Load()
	if n == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t.Errorf("%s: %d failure(s) during the soak window; first %d:\n  %s",
		what, n, len(l.sample), strings.Join(l.sample, "\n  "))
}

// soakFloat converts a decoded item value to a float64. Values come back
// as whatever the client's decoder produced (scalar, single-element
// array, or the string form), so the write/read-back worker compares
// numerically rather than asserting one concrete Go type.
func soakFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case []float64:
		if len(n) == 1 {
			return n[0], true
		}
	case []int32:
		if len(n) == 1 {
			return float64(n[0]), true
		}
	case []interface{}:
		// How the reference client decodes an array-typed value, which
		// is the form the write worker below sends.
		if len(n) == 1 {
			return soakFloat(n[0])
		}
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

// TestDockerServer_SteadyState keeps the containerized server under
// concurrent load for a sustained window and asserts what only sustained
// operation can show.
func TestDockerServer_SteadyState(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test skipped in -short mode")
	}

	client, container := newDockerServerWithContainer(t)
	ctx := context.Background()
	dur := soakDuration(t)
	t.Logf("soak window: %s (override with DOCKERINTEGRATION_SOAK)", dur)

	// The subscription that is deliberately never polled again. Created
	// before the load window so the reaper's grace period elapses while
	// the other workers are busy, costing no extra wall-clock time.
	abandonedCRH, abandonedCIH := newHandles(t, 1)
	abandoned, err := client.Subscribe(ctx,
		[]gopcxmlda.TItem{{ItemName: "Plant/BuildingB/Tank1/Level"}},
		&abandonedCRH, &abandonedCIH, "", true, soakPingRate, clientOptions())
	if err != nil {
		t.Fatalf("Subscribe (abandoned): %v", err)
	}
	abandonedHandle := abandoned.Response.ServerSubHandle
	if abandonedHandle == "" {
		t.Fatalf("Subscribe (abandoned): empty ServerSubHandle")
	}
	abandonedAt := time.Now()

	// The subscription that is polled continuously and must survive the
	// entire window.
	liveCRH, liveCIH := newHandles(t, 3)
	liveItems := []gopcxmlda.TItem{
		{ItemName: "Plant/BuildingB/Tank1/Level"},
		{ItemName: "Plant/BuildingA/Line1/Motor1/Temperature"},
		{ItemName: "Plant/BuildingA/Line2/Motor1/Temperature"},
	}
	live, err := client.Subscribe(ctx, liveItems, &liveCRH, &liveCIH, "", true, soakPingRate, clientOptions())
	if err != nil {
		t.Fatalf("Subscribe (live): %v", err)
	}
	liveHandle := live.Response.ServerSubHandle
	if liveHandle == "" {
		t.Fatalf("Subscribe (live): empty ServerSubHandle")
	}
	t.Cleanup(func() {
		crh, _ := newHandles(t, 0)
		_, _ = client.SubscriptionCancel(context.Background(), liveHandle, "", &crh)
	})

	var (
		failures  soakLog
		polls     atomic.Int64
		items     atomic.Int64
		writes    atomic.Int64
		browses   atomic.Int64
		distinct  sync.Map // observed Tank1/Level values -> struct{}
		firstSeen time.Time
		lastSeen  time.Time
		seenMu    sync.Mutex
		wg        sync.WaitGroup
	)
	deadline := time.Now().Add(dur)

	// Worker 1 — long-poll the live subscription without pause. Asserts
	// the handle stays valid for the whole window and records the values
	// and timestamps observed, so the test can prove afterwards that the
	// data actually moved rather than repeating one frozen sample.
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := soakClient(client)
		for time.Now().Before(deadline) {
			crh, _, err := gopcxmlda.GenerateClientHandles(0)
			if err != nil {
				failures.errf("poll: GenerateClientHandles: %v", err)
				return
			}
			// clientOptions is what makes ItemName and Timestamp
			// present at all: both default to false, so a reply carries
			// neither unless asked. This worker needs them to correlate
			// values by name and judge their freshness, and asking on
			// every poll keeps the RequestOptions gating under test for
			// the whole window.
			resp, err := c.SubscriptionPolledRefresh(ctx, liveHandle, soakPingRate, "", &crh,
				clientOptions(), gopcxmlda.TServerTime{UseClientTime: true})
			if err != nil {
				failures.errf("poll #%d: %v (fault %+v)", polls.Load()+1, err, resp.Fault)
				return
			}
			if len(resp.Response.InvalidServerSubHandles) != 0 {
				failures.errf("poll #%d: server dropped the live subscription mid-run: %v",
					polls.Load()+1, resp.Response.InvalidServerSubHandles)
				return
			}
			polls.Add(1)
			for _, item := range resp.Response.ItemList.Items {
				if item.Error != "" {
					failures.errf("poll: item %q returned ResultID %q", item.ItemName, item.Error)
					continue
				}
				items.Add(1)
				if item.ItemName == "" {
					failures.errf("poll: item came back without an ItemName despite ReturnItemName=true")
					continue
				}
				if item.Timestamp.IsZero() {
					failures.errf("poll: item %q came back without a Timestamp despite ReturnItemTime=true", item.ItemName)
					continue
				}
				if item.ItemName == "Plant/BuildingB/Tank1/Level" {
					distinct.Store(fmt.Sprintf("%v", item.Value.Value), struct{}{})
				}
				seenMu.Lock()
				if firstSeen.IsZero() || item.Timestamp.Before(firstSeen) {
					firstSeen = item.Timestamp
				}
				if item.Timestamp.After(lastSeen) {
					lastSeen = item.Timestamp
				}
				seenMu.Unlock()
			}
		}
	}()

	// Worker 2 — continuous write/read-back round trips. Nothing else
	// writes Motor1/Speed (the simulator only nudges the Temperature and
	// Level items), so the value read back must match the value just
	// written, every time, for the whole window.
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := soakClient(client)
		const item = "Plant/BuildingA/Line1/Motor1/Speed"
		want := int32(0)
		for time.Now().Before(deadline) {
			want = (want + 137) % 3000
			wcrh, wcih, err := gopcxmlda.GenerateClientHandles(1)
			if err != nil {
				failures.errf("write: GenerateClientHandles: %v", err)
				return
			}
			if _, err := c.Write(ctx,
				[]gopcxmlda.TItem{{ItemName: item, Value: gopcxmlda.TValue{Value: []int32{want}}}},
				&wcrh, &wcih, "", clientOptions()); err != nil {
				failures.errf("write(%d): %v", want, err)
				return
			}
			rcrh, rcih, err := gopcxmlda.GenerateClientHandles(1)
			if err != nil {
				failures.errf("read: GenerateClientHandles: %v", err)
				return
			}
			got, err := c.Read(ctx, []gopcxmlda.TItem{{ItemName: item}}, &rcrh, &rcih, "", clientOptions())
			if err != nil {
				failures.errf("read after write(%d): %v", want, err)
				return
			}
			if len(got.Response.ItemList.Items) != 1 || got.Response.ItemList.Items[0].Error != "" {
				failures.errf("read after write(%d): got %+v, want one successful item",
					want, got.Response.ItemList.Items)
				continue
			}
			f, ok := soakFloat(got.Response.ItemList.Items[0].Value.Value)
			if !ok {
				failures.errf("read after write(%d): value %#v is not numeric",
					want, got.Response.ItemList.Items[0].Value.Value)
				continue
			}
			if f != float64(want) {
				failures.errf("read after write(%d): read back %v", want, f)
				continue
			}
			writes.Add(1)
			time.Sleep(50 * time.Millisecond)
		}
	}()

	// Worker 3 — periodic Browse/GetStatus, checking the server keeps
	// reporting a stable address space and a running state throughout.
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := soakClient(client)
		for time.Now().Before(deadline) {
			scrh, _, err := gopcxmlda.GenerateClientHandles(0)
			if err != nil {
				failures.errf("status: GenerateClientHandles: %v", err)
				return
			}
			st, err := c.GetStatus(ctx, &scrh, "")
			if err != nil {
				failures.errf("GetStatus: %v", err)
				return
			}
			if st.Response.Result.ServerState != string(xmlda.ServerStateRunning) {
				failures.errf("GetStatus: ServerState %q, want %q",
					st.Response.Result.ServerState, xmlda.ServerStateRunning)
			}
			bcrh, _, err := gopcxmlda.GenerateClientHandles(0)
			if err != nil {
				failures.errf("browse: GenerateClientHandles: %v", err)
				return
			}
			leaf, err := c.Browse(ctx, "", &bcrh, "",
				gopcxmlda.TBrowseOptions{ItemName: "Plant/BuildingA/Line1/Motor1"})
			if err != nil {
				failures.errf("Browse: %v", err)
				return
			}
			if len(leaf.Response.Elements) != 3 {
				failures.errf("Browse(Motor1): got %d elements, want 3", len(leaf.Response.Elements))
			}
			browses.Add(1)
			time.Sleep(500 * time.Millisecond)
		}
	}()

	wg.Wait()
	failures.report(t, "steady-state load")

	t.Logf("completed in %s: %d long-polls delivering %d item updates, %d write/read round trips, %d browse+status cycles",
		dur, polls.Load(), items.Load(), writes.Load(), browses.Load())

	if polls.Load() == 0 {
		t.Errorf("no SubscriptionPolledRefresh completed during the soak window")
	}
	if items.Load() == 0 {
		t.Errorf("%d long-polls completed but not one delivered an item update", polls.Load())
	}
	if writes.Load() == 0 {
		t.Errorf("no write/read round trip completed during the soak window")
	}
	if browses.Load() == 0 {
		t.Errorf("no browse/status cycle completed during the soak window")
	}

	// The backend nudges the ticking items once a second, so a window of
	// any real length must have produced several distinct values. One
	// value would mean the server is replaying a snapshot rather than
	// sampling live data.
	n := 0
	distinct.Range(func(any, any) bool { n++; return true })
	if dur >= 5*time.Second && n < 2 {
		t.Errorf("observed %d distinct Tank1/Level value(s) over %s, want at least 2 — server is not serving live data", n, dur)
	}
	seenMu.Lock()
	spread := lastSeen.Sub(firstSeen)
	seenMu.Unlock()
	if dur >= 5*time.Second && spread <= 0 {
		t.Errorf("item timestamps never advanced over %s (spread %s) — values are not being resampled", dur, spread)
	}
	t.Logf("observed %d distinct Tank1/Level values; item timestamps spanned %s", n, spread)

	// The mirror image of what the poll worker asserts: with no
	// RequestOptions at all, §5.2.2's defaults apply and the very same
	// reply carries neither ItemName nor Timestamp. Worth pinning down,
	// because "the values are there but unlabeled" is exactly the shape
	// of an omission that looks like missing data from the client side.
	defCRH, _ := newHandles(t, 0)
	def, err := client.SubscriptionPolledRefresh(ctx, liveHandle, soakPingRate, "", &defCRH,
		map[string]any{}, gopcxmlda.TServerTime{UseClientTime: true})
	if err != nil {
		t.Fatalf("SubscriptionPolledRefresh (default options): %v", err)
	}
	if len(def.Response.ItemList.Items) == 0 {
		t.Errorf("default-options poll returned no items at all")
	}
	for _, item := range def.Response.ItemList.Items {
		if item.ItemName != "" {
			t.Errorf("default-options poll returned ItemName %q; ReturnItemName defaults to false", item.ItemName)
		}
		if !item.Timestamp.IsZero() {
			t.Errorf("default-options poll returned Timestamp %s; ReturnItemTime defaults to false", item.Timestamp)
		}
	}

	// The abandonment reaper: the subscription created at the top was
	// never polled, so the server must have terminated it on its own.
	// Each check is itself a poll, which would renew the subscription if
	// it were still alive — hence a bounded number of attempts, each
	// preceded by a full reap window, rather than a tight retry loop.
	var reaped bool
	for attempt := 1; attempt <= 2 && !reaped; attempt++ {
		if wait := soakReapWindow - time.Since(abandonedAt); wait > 0 {
			t.Logf("waiting %s more for the abandonment reaper", wait.Round(time.Millisecond))
			time.Sleep(wait)
		}
		crh, _ := newHandles(t, 0)
		resp, err := client.SubscriptionPolledRefresh(ctx, abandonedHandle, 0, "", &crh,
			clientOptions(), gopcxmlda.TServerTime{UseClientTime: true})
		switch {
		case err != nil && strings.Contains(resp.Fault.FaultCode, "E_NOSUBSCRIPTION"):
			reaped = true
		case err != nil:
			t.Fatalf("polling the abandoned handle failed unexpectedly: %v (fault %+v)", err, resp.Fault)
		default:
			// Still alive — and this poll just renewed it, so the next
			// attempt has to wait out another full window.
			abandonedAt = time.Now()
			t.Logf("attempt %d: abandoned subscription still alive, retrying", attempt)
		}
	}
	if !reaped {
		t.Errorf("subscription %q was never polled but the server never reaped it", abandonedHandle)
	}

	// Finally: the server must still be healthy after all of the above,
	// and must not have recovered a panic at any point during the run.
	// slog's TextHandler emits level=ERROR only for recovered panics and
	// startup/shutdown failures — ordinary client faults (the
	// E_NOSUBSCRIPTION above included) are SOAP responses, not error logs.
	crh, _ := newHandles(t, 0)
	st, err := client.GetStatus(ctx, &crh, "")
	if err != nil {
		t.Fatalf("GetStatus after the soak window: %v", err)
	}
	if st.Response.Result.ServerState != string(xmlda.ServerStateRunning) {
		t.Errorf("after the soak window ServerState is %q, want %q",
			st.Response.Result.ServerState, xmlda.ServerStateRunning)
	}

	rc, err := container.Logs(ctx)
	if err != nil {
		t.Fatalf("reading container logs: %v", err)
	}
	defer func() { _ = rc.Close() }()
	logs, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading container logs: %v", err)
	}
	var bad []string
	for _, line := range strings.Split(string(logs), "\n") {
		if strings.Contains(line, "level=ERROR") || strings.Contains(line, "panic") {
			bad = append(bad, strings.TrimSpace(line))
		}
	}
	if len(bad) > 0 {
		t.Errorf("server logged %d error/panic line(s) during the soak:\n  %s",
			len(bad), strings.Join(bad, "\n  "))
	}
}
