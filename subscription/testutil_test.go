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
