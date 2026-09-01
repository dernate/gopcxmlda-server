package subscription

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock/clocktest"
	"github.com/dernate/gopcxmlda-server/telemetry"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

var testEpoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeReader is a controllable backend.Reader for tests. Set/SetNotFound
// mutate what Read returns; safe for concurrent use since poll-mode
// scheduling reads it from a timer callback while a test goroutine may
// mutate it concurrently.
type fakeReader struct {
	mu           sync.Mutex
	values       map[backend.ItemRef]xmlda.Value
	quality      map[backend.ItemRef]xmlda.OPCQuality
	notFound     map[backend.ItemRef]bool
	readCount    int
	refReadCount map[backend.ItemRef]int // how many Read calls actually included each ref
}

func newFakeReader() *fakeReader {
	return &fakeReader{
		values:       map[backend.ItemRef]xmlda.Value{},
		quality:      map[backend.ItemRef]xmlda.OPCQuality{},
		notFound:     map[backend.ItemRef]bool{},
		refReadCount: map[backend.ItemRef]int{},
	}
}

// RefReadCount returns how many Read calls have actually included ref —
// distinct from ReadCount, which only counts calls, not which items each
// call carried.
func (f *fakeReader) RefReadCount(ref backend.ItemRef) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refReadCount[ref]
}

func (f *fakeReader) Set(ref backend.ItemRef, v xmlda.Value) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[ref] = v
	if _, ok := f.quality[ref]; !ok {
		f.quality[ref] = xmlda.NewGoodQuality()
	}
}

func (f *fakeReader) SetQuality(ref backend.ItemRef, q xmlda.OPCQuality) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quality[ref] = q
}

func (f *fakeReader) SetNotFound(ref backend.ItemRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notFound[ref] = true
}

func (f *fakeReader) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCount++
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i, it := range items {
		f.refReadCount[it.Ref]++
		if f.notFound[it.Ref] {
			out[i] = backend.Result[backend.ItemSample]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		v, ok := f.values[it.Ref]
		if !ok {
			out[i] = backend.Result[backend.ItemSample]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		out[i] = backend.Result[backend.ItemSample]{Value: backend.ItemSample{Value: v, Quality: f.quality[it.Ref]}}
	}
	return out, nil
}

func (f *fakeReader) ReadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readCount
}

// pushReader additionally implements backend.ChangeNotifier, for
// exercising push-mode subscription refresh.
type pushReader struct {
	*fakeReader
	mu    sync.Mutex
	chans []chan backend.ChangeEvent
}

func newPushReader() *pushReader {
	return &pushReader{fakeReader: newFakeReader()}
}

func (p *pushReader) WatchItems(ctx context.Context, items []backend.WatchRequest) (<-chan backend.ChangeEvent, error) {
	ch := make(chan backend.ChangeEvent, 16)
	p.mu.Lock()
	p.chans = append(p.chans, ch)
	p.mu.Unlock()
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// Push delivers ev to every active watcher.
func (p *pushReader) Push(ev backend.ChangeEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ch := range p.chans {
		ch <- ev
	}
}

func newTestManager(r backend.Reader, fake *clocktest.Fake, cfg Config) *Manager {
	return NewManager(backend.Backend{Reader: r}, fake, telemetry.NoopLogger(), telemetry.NoopMetrics(), cfg)
}

// shutdownManager shuts m down and fails the test if Shutdown reports an
// error (e.g. a background goroutine failed to exit in time), matching the
// check test/clientintegration/client_test.go already performs on
// Handler.Shutdown.
func shutdownManager(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Shutdown(context.Background()); err != nil {
		t.Errorf("Manager.Shutdown: %v", err)
	}
}

// --- the backend can revise a sampling rate ---

// rateReviser is a fakeReader that also implements
// backend.SamplingRateReviser, pinning every rate to fixedRate.
type rateReviser struct {
	*fakeReader
	fixedRate time.Duration
	err       error
	shortBy   int // return this many fewer rates than asked for
	mu        sync.Mutex
	calls     int
}

func (r *rateReviser) ReviseSamplingRates(_ context.Context, reqs []backend.RateRequest) ([]time.Duration, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	out := make([]time.Duration, max(len(reqs)-r.shortBy, 0))
	for i := range out {
		out[i] = r.fixedRate
	}
	return out, nil
}

func (r *rateReviser) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// clampReader reports every read with S_CLAMP plus a usable value.
type clampReader struct{ *fakeReader }

func (c *clampReader) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	out, err := c.fakeReader.Read(ctx, items)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].ResultID.IsZero() {
			out[i].ResultID = xmlda.SuccessClamp
		}
	}
	return out, nil
}

// --- shared minimal backend ---

type fixStatus struct{}

func (fixStatus) GetStatus(context.Context, string) (backend.ServerStatus, error) {
	return backend.ServerStatus{
		State:              xmlda.ServerStateRunning,
		SupportedLocaleIDs: []string{"en-US"},
	}, nil
}

type fixReader struct{}

func (fixReader) Read(_ context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i := range items {
		out[i] = backend.Result[backend.ItemSample]{Value: backend.ItemSample{
			Value:   xmlda.NewInt32(1),
			Quality: xmlda.NewGoodQuality(),
		}}
	}
	return out, nil
}

// slowReader consumes a fixed amount of fake-clock time per Read, so a
// test can make the backend "slow" without any real waiting.
type slowReader struct {
	clk  *clocktest.Fake
	cost time.Duration
	mu   sync.Mutex
	n    int
}

func (r *slowReader) Read(_ context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	// Advancing from inside the callback is safe: Fake fires callbacks
	// with its own lock released (see clocktest.Fake's doc comment).
	r.clk.Advance(r.cost)
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i := range items {
		out[i] = backend.Result[backend.ItemSample]{Value: backend.ItemSample{
			Value: xmlda.NewInt32(1), Quality: xmlda.NewGoodQuality()}}
	}
	return out, nil
}

func (r *slowReader) count() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

func (r *slowReader) reset() { r.mu.Lock(); defer r.mu.Unlock(); r.n = 0 }
