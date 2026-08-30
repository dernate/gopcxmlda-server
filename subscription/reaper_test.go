package subscription

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// TestReapGrace_OverflowAndNegativeClamp exercises reapGrace's two clamp
// branches directly — both exist specifically to prevent an int64
// nanosecond overflow/wraparound from producing a "grace period" shorter
// than intended (which would reap a subscription the client asked to keep
// alive for a long time), but neither branch was previously reached by any
// test driving the reaper through ordinary pingRate/multiplier values.
func TestReapGrace_OverflowAndNegativeClamp(t *testing.T) {
	tests := []struct {
		name       string
		pingRate   time.Duration
		multiplier float64
		want       time.Duration
	}{
		{"ordinary", time.Second, 2, 2 * time.Second},
		{"negative product clamps to zero", time.Second, -1, 0},
		{"huge product clamps to MaxInt64", time.Duration(math.MaxInt64), 2, time.Duration(math.MaxInt64)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reapGrace(tc.pingRate, tc.multiplier); got != tc.want {
				t.Fatalf("reapGrace(%v, %v) = %v, want %v", tc.pingRate, tc.multiplier, got, tc.want)
			}
		})
	}
}

func TestReaper_AbandonsSubscriptionPastGracePeriod(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	cfg := Config{ReapInterval: time.Second, ReapGraceMultiplier: 2, DefaultSamplingRate: time.Hour}
	m := newTestManager(r, fake, cfg)
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items:                []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
		SubscriptionPingRate: time.Second, // grace = 1s * 2 = 2s
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.count() != 1 {
		t.Fatalf("expected 1 active subscription")
	}

	fake.Advance(3 * time.Second) // exceeds the 2s grace period; reaper AfterFunc chain fires inline
	if m.count() != 0 {
		t.Fatalf("expected the subscription to be reaped after exceeding its grace period, got count=%d", m.count())
	}
	if _, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}}); !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("expected the reaped handle to now be invalid, got err=%v", err)
	}
}

func TestReaper_DoesNotAbandonActivelyPolledSubscription(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	cfg := Config{ReapInterval: time.Second, ReapGraceMultiplier: 2, DefaultSamplingRate: time.Hour}
	m := newTestManager(r, fake, cfg)
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items:                []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
		SubscriptionPingRate: time.Second,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for range 5 {
		fake.Advance(time.Second) // < 2s grace, so polling keeps it alive across sweeps
		if _, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}}); err != nil {
			t.Fatalf("PolledRefresh: %v", err)
		}
	}
	if m.count() != 1 {
		t.Fatalf("expected the actively-polled subscription to survive, got count=%d", m.count())
	}
}

// TestReaper_TerminateIfStillAbandoned_RefusesRecentlyRenewed reproduces
// the race a plain "decide, then act" reaper sweep is exposed to: reapOnce
// takes a read-locked decision pass over the whole map, then a second
// pass actually terminates whatever it found — during the window between
// those two passes, a client's PolledRefresh may have legitimately
// renewed the exact subscription that was about to be reaped.
// terminateIfStillAbandoned re-validates right before removal and must
// refuse to terminate a subscription whose lastPolledAt has since moved
// past the sweep's own decision timestamp (asOf).
func TestReaper_TerminateIfStillAbandoned_RefusesRecentlyRenewed(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	// ReapInterval is deliberately much larger than this test's own
	// Advance calls: the automatic background reap chain must not fire
	// and reap the subscription for real before this test gets a chance
	// to control the exact decision-vs-action timing itself via a direct
	// terminateIfStillAbandoned call.
	cfg := Config{ReapInterval: time.Hour, ReapGraceMultiplier: 2, DefaultSamplingRate: time.Hour}
	m := newTestManager(r, fake, cfg)
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items:                []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
		SubscriptionPingRate: time.Second, // grace = 1s * 2 = 2s
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fake.Advance(5 * time.Second) // now well past the grace period since creation
	asOf := fake.Now()            // simulates reapOnce's decision-pass timestamp

	fake.Advance(time.Second) // time passes between the decision pass and the action pass
	if _, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}}); err != nil {
		t.Fatalf("PolledRefresh (renewal racing the sweep): %v", err)
	}

	if m.terminateIfStillAbandoned(res.Handle, asOf) {
		t.Fatalf("expected terminateIfStillAbandoned to refuse a subscription renewed after asOf")
	}
	if m.count() != 1 {
		t.Fatalf("expected the subscription to survive the reap race, got count=%d", m.count())
	}
	if _, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}}); err != nil {
		t.Fatalf("expected the handle to remain valid after surviving the race, got %v", err)
	}
}

// TestReaper_TerminateIfStillAbandoned_TerminatesWhenNotRenewed is the
// sanity-check companion to the above: without a renewal, the re-check
// must still terminate — it should not refuse unconditionally.
func TestReaper_TerminateIfStillAbandoned_TerminatesWhenNotRenewed(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Item1"}
	r.Set(ref, xmlda.NewInt32(1))
	cfg := Config{ReapInterval: time.Hour, ReapGraceMultiplier: 2, DefaultSamplingRate: time.Hour}
	m := newTestManager(r, fake, cfg)
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items:                []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
		SubscriptionPingRate: time.Second,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fake.Advance(5 * time.Second)
	asOf := fake.Now()

	if !m.terminateIfStillAbandoned(res.Handle, asOf) {
		t.Fatalf("expected terminateIfStillAbandoned to terminate a genuinely still-abandoned subscription")
	}
	if m.count() != 0 {
		t.Fatalf("expected the subscription to be gone, got count=%d", m.count())
	}
}

func TestReaper_OnlyAbandonedSubscriptionsAreReaped(t *testing.T) {
	fake := clocktest.New(testEpoch)
	r := newFakeReader()
	ref1 := backend.ItemRef{ItemName: "Item1"}
	ref2 := backend.ItemRef{ItemName: "Item2"}
	r.Set(ref1, xmlda.NewInt32(1))
	r.Set(ref2, xmlda.NewInt32(2))
	cfg := Config{ReapInterval: time.Second, ReapGraceMultiplier: 2, DefaultSamplingRate: time.Hour}
	m := newTestManager(r, fake, cfg)
	defer shutdownManager(t, m)

	active, err := m.Create(context.Background(), CreateRequest{
		Items:                []CreateItemRequest{{Ref: ref1, ClientItemHandle: "CIH1"}},
		SubscriptionPingRate: time.Second,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = m.Create(context.Background(), CreateRequest{
		Items:                []CreateItemRequest{{Ref: ref2, ClientItemHandle: "CIH2"}},
		SubscriptionPingRate: time.Second,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.count() != 2 {
		t.Fatalf("expected 2 active subscriptions")
	}

	// Keep `active` alive while `abandoned` (the second one) never gets
	// polled again.
	for range 3 {
		fake.Advance(time.Second)
		if _, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{active.Handle}}); err != nil {
			t.Fatalf("PolledRefresh: %v", err)
		}
	}
	if m.count() != 1 {
		t.Fatalf("expected exactly the unpolled subscription to be reaped, got count=%d", m.count())
	}
	if _, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{active.Handle}}); err != nil {
		t.Fatalf("expected the actively-polled subscription to remain valid, got %v", err)
	}
}
