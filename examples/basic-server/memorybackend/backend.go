// Package memorybackend is a small in-memory backend.Backend
// implementation used by examples/basic-server to demonstrate this
// library end to end. It is example code, not part of the library's
// public API, and intentionally has no production-specific concerns
// (persistence, authentication, real device I/O) — see
// docs/backend-implementation.md for what a real backend needs to
// consider instead.
package memorybackend

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

type item struct {
	mu          sync.Mutex
	value       xmlda.Value
	quality     xmlda.OPCQuality
	ts          time.Time
	writable    bool
	description string
	// hasRange/rangeMin/rangeMax demonstrate backend.WriteOutcome.Clamped
	// (REQ-WRITE-005): a Write of an int32 value outside [rangeMin,
	// rangeMax] is clamped rather than rejected. Only meaningful when
	// hasRange is true and the item's value is a TypeInt32 scalar.
	hasRange           bool
	rangeMin, rangeMax int32
}

type watcher struct {
	ch   chan backend.ChangeEvent
	refs map[backend.ItemRef]bool
}

// Backend is a small in-memory OPC XML-DA data source with a few static
// and periodically-changing demo items, and a two-level address space
// (a "Demo" branch containing the items) for Browse. It implements
// backend.StatusProvider, backend.Reader (+ backend.ChangeNotifier),
// backend.Writer, backend.Browser, and backend.PropertyReader.
type Backend struct {
	mu       sync.RWMutex
	items    map[backend.ItemRef]*item
	tree     map[string][]string // parent item name ("" = root) -> child names
	watchers []*watcher
	start    time.Time

	// ctx/cancel are this Backend's own lifecycle, independent of any
	// caller-supplied context — see WatchItems's cleanup goroutine and
	// Close's doc comment for why this matters.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New constructs a Backend and starts its background value simulator.
// Call Close to stop it — see Close's doc comment for the shutdown
// contract this mirrors from subscription.Manager.
func New() *Backend {
	ctx, cancel := context.WithCancel(context.Background())
	b := &Backend{
		items: map[backend.ItemRef]*item{},
		tree: map[string][]string{
			"":     {"Demo"},
			"Demo": {"Counter", "Temperature", "Switch", "Message", "Readings"},
		},
		start:  time.Now(),
		ctx:    ctx,
		cancel: cancel,
	}
	b.addItem("Demo/Counter", xmlda.NewInt32(0), true,
		"An incrementing counter; also directly writable. Writes are clamped to [0, 1000] (demonstrates WriteOutcome.Clamped / S_CLAMP).")
	b.addItem("Demo/Temperature", xmlda.NewFloat64(21.5), false, "A simulated read-only temperature reading.")
	b.addItem("Demo/Switch", xmlda.NewBool(false), true, "A writable boolean switch.")
	b.addItem("Demo/Message", xmlda.NewString("hello"), true, "A writable string value.")
	// An array-typed item. Arrays are ordinary process data in OPC (a
	// VT_ARRAY tag is the normal way a device exposes a block of
	// registers), and every example here was scalar-only for a while —
	// which is exactly why the missing xmlda.NewArrayValue went unnoticed
	// for so long. Keeping one in the example keeps that path exercised.
	b.addItem("Demo/Readings", xmlda.NewArrayValue(xmlda.NewFloat64Array([]float64{0, 0, 0, 0})), false,
		"A simulated read-only block of four sensor readings (ArrayOfDouble).")
	b.items[backend.ItemRef{ItemName: "Demo/Counter"}].hasRange = true
	b.items[backend.ItemRef{ItemName: "Demo/Counter"}].rangeMin = 0
	b.items[backend.ItemRef{ItemName: "Demo/Counter"}].rangeMax = 1000

	b.wg.Go(func() { b.simulate(ctx) })
	return b
}

// Close stops the background simulator and every WatchItems cleanup
// goroutine, and waits for all of them to exit — bounded by this
// Backend's own internal context, never by a caller-supplied one. A
// WatchItems cleanup goroutine also exits when its own caller cancels the
// context it passed in (the normal case, once the owning
// subscription.Manager tears the subscription down), but Close must not
// depend on that happening: an application calling Close directly (e.g.
// outside the ordering main.go happens to use, or before every
// subscription's context was cancelled) would otherwise hang here
// forever.
func (b *Backend) Close() {
	b.cancel()
	b.wg.Wait()
}

func (b *Backend) addItem(name string, v xmlda.Value, writable bool, description string) {
	b.items[backend.ItemRef{ItemName: name}] = &item{
		value: v, quality: xmlda.NewGoodQuality(), ts: time.Now(),
		writable: writable, description: description,
	}
}

// simulate periodically updates Counter and Temperature to demonstrate
// Subscribe/SubscriptionPolledRefresh (and, via notify, push-mode
// delivery) with genuinely changing data. Its only abort path is ctx
// cancellation (from Close), checked on every tick.
func (b *Backend) simulate(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	counterRef := backend.ItemRef{ItemName: "Demo/Counter"}
	tempRef := backend.ItemRef{ItemName: "Demo/Temperature"}
	readingsRef := backend.ItemRef{ItemName: "Demo/Readings"}
	var counter int32
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			counter++
			b.setValue(counterRef, xmlda.NewInt32(counter))

			temp := 21.5 + (rand.Float64()-0.5)*2
			b.setValue(tempRef, xmlda.NewFloat64(temp))

			readings := make([]float64, 4)
			for i := range readings {
				readings[i] = float64(counter) + float64(i)/10
			}
			b.setValue(readingsRef, xmlda.NewArrayValue(xmlda.NewFloat64Array(readings)))
		}
	}
}

func (b *Backend) setValue(ref backend.ItemRef, v xmlda.Value) {
	b.mu.RLock()
	itm, ok := b.items[ref]
	b.mu.RUnlock()
	if !ok {
		return
	}
	itm.mu.Lock()
	itm.value = v
	itm.ts = time.Now()
	sample := backend.ItemSample{Value: itm.value, Quality: itm.quality, Timestamp: itm.ts}
	itm.mu.Unlock()
	b.notify(ref, sample)
}

// notify pushes sample to every watcher currently interested in ref
// (push-mode subscription delivery). A watcher whose buffer is full has
// the update dropped rather than blocking the simulator — an accepted
// simplification for this example; a production backend would size its
// buffers and/or block with a bounded timeout instead.
func (b *Backend) notify(ref backend.ItemRef, sample backend.ItemSample) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, w := range b.watchers {
		if !w.refs[ref] {
			continue
		}
		select {
		case w.ch <- backend.ChangeEvent{Ref: ref, Sample: sample}:
		default:
		}
	}
}

// GetStatus implements backend.StatusProvider.
func (b *Backend) GetStatus(ctx context.Context, locale string) (backend.ServerStatus, error) {
	return backend.ServerStatus{
		State:              xmlda.ServerStateRunning,
		StartTime:          b.start,
		ProductVersion:     "0.1.0-example",
		VendorInfo:         "gopcxmlda-server basic-server example",
		SupportedLocaleIDs: []string{"en-US"},
	}, nil
}

// Read implements backend.Reader.
func (b *Backend) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i, it := range items {
		b.mu.RLock()
		itm, ok := b.items[it.Ref]
		b.mu.RUnlock()
		if !ok {
			out[i] = backend.Result[backend.ItemSample]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		itm.mu.Lock()
		sample := backend.ItemSample{Value: itm.value, Quality: itm.quality, Timestamp: itm.ts}
		itm.mu.Unlock()
		out[i] = backend.Result[backend.ItemSample]{Value: sample}
	}
	return out, nil
}

// WatchItems implements backend.ChangeNotifier, making Backend's Reader
// support push-mode subscriptions (docs/architecture/subscription-model.md).
func (b *Backend) WatchItems(ctx context.Context, items []backend.WatchRequest) (<-chan backend.ChangeEvent, error) {
	refs := make(map[backend.ItemRef]bool, len(items))
	for _, it := range items {
		refs[it.Ref] = true
	}
	ch := make(chan backend.ChangeEvent, 16)
	w := &watcher{ch: ch, refs: refs}

	b.mu.Lock()
	b.watchers = append(b.watchers, w)
	b.mu.Unlock()

	b.wg.Go(func() {
		select {
		case <-ctx.Done():
		case <-b.ctx.Done():
			// Close was called directly, independent of whether the
			// caller-supplied ctx (owned by whoever called WatchItems,
			// typically subscription.Manager) was ever cancelled — see
			// Close's doc comment.
		}
		b.mu.Lock()
		for i, ww := range b.watchers {
			if ww == w {
				b.watchers = append(b.watchers[:i], b.watchers[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
		close(ch)
	})
	return ch, nil
}

// Write implements backend.Writer.
func (b *Backend) Write(ctx context.Context, items []backend.WriteRequestItem) ([]backend.Result[backend.WriteOutcome], error) {
	out := make([]backend.Result[backend.WriteOutcome], len(items))
	for i, it := range items {
		b.mu.RLock()
		itm, ok := b.items[it.Ref]
		b.mu.RUnlock()
		if !ok {
			out[i] = backend.Result[backend.WriteOutcome]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		if !itm.writable {
			out[i] = backend.Result[backend.WriteOutcome]{ResultID: xmlda.ErrReadOnly}
			continue
		}
		newValue := it.Value
		clamped := false
		if itm.hasRange {
			if i32, err := newValue.Int32(); err == nil {
				switch {
				case i32 < itm.rangeMin:
					newValue, clamped = xmlda.NewInt32(itm.rangeMin), true
				case i32 > itm.rangeMax:
					newValue, clamped = xmlda.NewInt32(itm.rangeMax), true
				}
			}
		}

		itm.mu.Lock()
		itm.value = newValue
		if it.Quality != nil {
			itm.quality = *it.Quality
		} else {
			itm.quality = xmlda.NewGoodQuality()
		}
		if it.Timestamp != nil {
			itm.ts = *it.Timestamp
		} else {
			itm.ts = time.Now()
		}
		sample := backend.ItemSample{Value: itm.value, Quality: itm.quality, Timestamp: itm.ts}
		itm.mu.Unlock()
		b.notify(it.Ref, sample)
		if clamped {
			cv := newValue
			out[i] = backend.Result[backend.WriteOutcome]{Value: backend.WriteOutcome{Clamped: true, Value: &cv}}
		} else {
			out[i] = backend.Result[backend.WriteOutcome]{}
		}
	}
	return out, nil
}

// browseFilterMatches implements REQ-BROWSE-004's IsItem/HasChildren
// truth table for xmlda.BrowseFilter: "item" keeps only actionable items,
// "branch" keeps only elements with children, "all" (or unset) keeps
// everything.
func browseFilterMatches(filter xmlda.BrowseFilter, isItem, hasChildren bool) bool {
	switch filter {
	case xmlda.BrowseFilterItem:
		return isItem
	case xmlda.BrowseFilterBranch:
		return hasChildren
	default: // "" or BrowseFilterAll
		return true
	}
}

// elementNameFilterMatches is this demo backend's own, documented
// interpretation of ElementNameFilter — the specification leaves its
// exact matching syntax to the backend (docs/specification/
// specification-analysis.md notes its interaction with VendorFilter is
// "undefined... per spec"), so there is no one mandated behavior to
// implement. Case-insensitive match against name, where "*" matches any
// run of characters (e.g. "Temp*" matches "Temperature"). An empty
// filter matches everything; a malformed pattern is treated as
// "matches everything" rather than silently hiding all results.
func elementNameFilterMatches(filter, name string) bool {
	if filter == "" {
		return true
	}
	pattern := "^(?i:" + strings.ReplaceAll(regexp.QuoteMeta(filter), `\*`, ".*") + ")$"
	re, err := regexp.Compile(pattern)
	if err != nil {
		return true
	}
	return re.MatchString(name)
}

// propertyIDForName reverse-maps a standard property QName (as carried
// by BrowseRequest.PropertyNames on the wire) back to the PropertyID
// propertiesFor filters by. Only the three properties this demo backend
// knows about are matchable; an unrecognized name simply contributes
// nothing rather than erroring.
func propertyIDForName(name xmlda.QName) (xmlda.PropertyID, bool) {
	for _, id := range []xmlda.PropertyID{xmlda.PropDataType, xmlda.PropDescription, xmlda.PropValue} {
		if xmlda.StandardPropertyName(id) == name {
			return id, true
		}
	}
	return 0, false
}

// knownPropertyIDs are the only property IDs this demo backend has data
// for. Referenced by propertiesFor to report E_INVALIDPID for anything
// else, per docs/backend-implementation.md's documented PropertyReader
// contract ("E_INVALIDPID for one unrecognized property among several
// valid ones").
var knownPropertyIDs = map[xmlda.PropertyID]bool{
	xmlda.PropDataType:    true,
	xmlda.PropDescription: true,
	xmlda.PropValue:       true,
}

// propertiesFor returns itm's properties, filtered the way both
// GetProperties and Browse need: every property this backend knows about
// if all is true, only the ones named in ids otherwise
// (backend.PropertyRequest's own doc comment: "All requests every
// property the item has; if false, only the properties named in
// PropertyIDs"). PropValue is additionally gated by includeValue,
// mirroring backend.PropertyRequest.IncludeValues/
// BrowseRequest.ReturnPropertyValues. One snapshot under one critical
// section — reading value/description via two separate lock/unlock pairs
// could otherwise mix two different generations of this item's state
// across a concurrent Write. When all is false, an id in ids this backend
// doesn't recognize is reported back as its own Property with
// ResultID: xmlda.ErrInvalidPID rather than silently dropped.
func propertiesFor(itm *item, all bool, ids []xmlda.PropertyID, includeValue bool) []backend.Property {
	itm.mu.Lock()
	value := itm.value
	description := itm.description
	itm.mu.Unlock()

	wanted := func(id xmlda.PropertyID) bool {
		if all {
			return true
		}
		return slices.Contains(ids, id)
	}

	var props []backend.Property
	if wanted(xmlda.PropDataType) {
		props = append(props, backend.Property{ID: xmlda.PropDataType, Value: xmlda.NewQNameValue(value.TypeName())})
	}
	if wanted(xmlda.PropDescription) {
		props = append(props, backend.Property{ID: xmlda.PropDescription, Value: xmlda.NewString(description)})
	}
	if includeValue && wanted(xmlda.PropValue) {
		props = append(props, backend.Property{ID: xmlda.PropValue, Value: value})
	}
	if !all {
		for _, id := range ids {
			if !knownPropertyIDs[id] {
				props = append(props, backend.Property{ID: id, ResultID: xmlda.ErrInvalidPID})
			}
		}
	}
	return props
}

// Browse implements backend.Browser — single-level (this demo address
// space is exactly two levels deep, so recursion is never needed).
// Honors Filter, a "*"-wildcard ElementNameFilter (see
// elementNameFilterMatches), ReturnAllProperties/PropertyNames/
// ReturnPropertyValues (via the same propertiesFor helper GetProperties
// uses), and MaxElementsReturned/ContinuationPoint pagination — the
// continuation point is this backend's own private cursor (a stringified
// offset into the filtered, sorted result set), opaque to the framework
// per docs/backend-implementation.md. VendorFilter is accepted but has no
// effect: this demo backend defines no vendor-specific properties or
// filtering criteria for it to act on.
func (b *Backend) Browse(ctx context.Context, req backend.BrowseRequest) (backend.BrowseResult, error) {
	parent := req.Ref.ItemName
	b.mu.RLock()
	children := append([]string(nil), b.tree[parent]...)
	b.mu.RUnlock()
	sort.Strings(children) // deterministic order, required for stable pagination across calls

	var wantIDs []xmlda.PropertyID
	for _, n := range req.PropertyNames {
		if id, ok := propertyIDForName(n); ok {
			wantIDs = append(wantIDs, id)
		}
	}

	var candidates []backend.BrowseElement
	for _, name := range children {
		full := name
		if parent != "" {
			full = parent + "/" + name
		}
		ref := backend.ItemRef{ItemName: full}
		b.mu.RLock()
		itm, isItem := b.items[ref]
		_, hasChildren := b.tree[full]
		b.mu.RUnlock()

		if !browseFilterMatches(req.Filter, isItem, hasChildren) {
			continue
		}
		if !elementNameFilterMatches(req.ElementNameFilter, name) {
			continue
		}

		el := backend.BrowseElement{Name: name, Ref: &ref, IsItem: isItem, HasChildren: hasChildren}
		if isItem && (req.ReturnAllProperties || len(wantIDs) > 0) {
			el.Properties = propertiesFor(itm, req.ReturnAllProperties, wantIDs, req.ReturnPropertyValues)
		}
		candidates = append(candidates, el)
	}

	start := 0
	if req.ContinuationPoint != "" {
		n, err := strconv.Atoi(req.ContinuationPoint)
		if err != nil || n < 0 || n > len(candidates) {
			return backend.BrowseResult{}, fmt.Errorf("memorybackend: invalid continuation point %q", req.ContinuationPoint)
		}
		start = n
	}

	end := len(candidates)
	moreElements := false
	if req.MaxElementsReturned > 0 && start+req.MaxElementsReturned < end {
		end = start + req.MaxElementsReturned
		moreElements = true
	}

	result := backend.BrowseResult{Elements: candidates[start:end], MoreElements: moreElements}
	if moreElements {
		result.ContinuationPoint = strconv.Itoa(end)
	}
	return result, nil
}

// GetProperties implements backend.PropertyReader.
func (b *Backend) GetProperties(ctx context.Context, reqs []backend.PropertyRequest) ([]backend.Result[[]backend.Property], error) {
	out := make([]backend.Result[[]backend.Property], len(reqs))
	for i, r := range reqs {
		b.mu.RLock()
		itm, ok := b.items[r.Ref]
		b.mu.RUnlock()
		if !ok {
			out[i] = backend.Result[[]backend.Property]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		out[i] = backend.Result[[]backend.Property]{Value: propertiesFor(itm, r.All, r.PropertyIDs, r.IncludeValues)}
	}
	return out, nil
}
