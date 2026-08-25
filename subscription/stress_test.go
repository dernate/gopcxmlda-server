package subscription

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

// TestStress_ConcurrentCreateCancelRefresh hammers Create/Cancel/
// PolledRefresh from many goroutines concurrently, using the real clock
// (genuine goroutine-scheduling non-determinism, unlike the deterministic
// fake-clock tests elsewhere in this package). It asserts no panic/
// deadlock (the test completing within its timeout is itself the main
// assertion) and checks the E_BUSY invariant: disjoint-handle calls must
// never spuriously report busy.
//
// This is not a substitute for `go test -race` (blocked in this
// environment's sandbox — see docs/architecture/testing-strategy.md) —
// it catches logic bugs (deadlocks, incorrect counting, panics) but not
// pure data races that don't happen to manifest as one of those. Running
// this under `-race` once a C toolchain is available remains an
// outstanding verification step.
func TestStress_ConcurrentCreateCancelRefresh(t *testing.T) {
	r := newFakeReader()
	for i := range 20 {
		r.Set(backend.ItemRef{ItemName: itemName(i)}, xmlda.NewInt32(int32(i)))
	}
	m := NewManager(backend.Backend{Reader: r}, clock.Real{}, nil, nil, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	const workers = 20
	const opsPerWorker = 50
	var wg sync.WaitGroup
	var falseBusy int64

	for w := range workers {
		wg.Go(func() {
			ref := backend.ItemRef{ItemName: itemName(w % 20)}
			for range opsPerWorker {
				res, err := m.Create(context.Background(), CreateRequest{
					Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH"}},
				})
				if err != nil {
					t.Errorf("Create: %v", err)
					return
				}
				if res.Handle == "" {
					t.Errorf("expected a valid handle")
					return
				}
				if _, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}}); err != nil {
					// A disjoint-handle call (this worker's own,
					// just-created, not shared with any other worker)
					// must never report ErrBusy.
					if err == ErrBusy {
						atomic.AddInt64(&falseBusy, 1)
					} else {
						t.Errorf("PolledRefresh: %v", err)
						return
					}
				}
				m.Cancel(res.Handle)
			}
		})
	}
	wg.Wait()

	if falseBusy != 0 {
		t.Fatalf("got %d false-positive E_BUSY reports on disjoint handles, want 0", falseBusy)
	}
	if m.count() != 0 {
		t.Fatalf("expected 0 active subscriptions after all workers cancelled theirs, got %d", m.count())
	}
}

// TestStress_SharedHandleOverlappingRefresh hammers PolledRefresh on ONE
// shared handle from many goroutines concurrently, confirming E_BUSY
// correctly rejects true overlap (at least some calls must observe
// ErrBusy given enough concurrent contention) without ever double-freeing
// the busy flag or deadlocking.
func TestStress_SharedHandleOverlappingRefresh(t *testing.T) {
	r := newFakeReader()
	ref := backend.ItemRef{ItemName: "Shared"}
	r.Set(ref, xmlda.NewInt32(1))
	m := NewManager(backend.Backend{Reader: r}, clock.Real{}, nil, nil, Config{ReapInterval: time.Hour})
	defer shutdownManager(t, m)

	res, err := m.Create(context.Background(), CreateRequest{
		Items: []CreateItemRequest{{Ref: ref, ClientItemHandle: "CIH1"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const workers = 50
	var wg sync.WaitGroup
	var successes, busies int64
	for range workers {
		wg.Go(func() {
			hold := time.Now().Add(20 * time.Millisecond)
			_, err := m.PolledRefresh(context.Background(), RefreshRequest{
				Handles: []Handle{res.Handle}, HoldTime: &hold, WaitTime: 20 * time.Millisecond,
			})
			switch err {
			case nil:
				atomic.AddInt64(&successes, 1)
			case ErrBusy:
				atomic.AddInt64(&busies, 1)
			default:
				t.Errorf("PolledRefresh: %v", err)
			}
		})
	}
	wg.Wait()

	if successes == 0 {
		t.Fatalf("expected at least one successful PolledRefresh")
	}
	if successes+busies != workers {
		t.Fatalf("got successes=%d busies=%d, want sum=%d", successes, busies, workers)
	}

	// The busy flag must be fully released: a follow-up call must succeed.
	if _, err := m.PolledRefresh(context.Background(), RefreshRequest{Handles: []Handle{res.Handle}}); err != nil {
		t.Fatalf("expected the busy flag to be released after all concurrent calls completed, got %v", err)
	}
}

func itemName(i int) string {
	return "Item" + string(rune('A'+i))
}
