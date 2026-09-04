// Package plantbackend is a small in-memory backend.Backend
// implementation used only by test/dockerintegration. Its address space
// is deliberately deeper than examples/basic-server/memorybackend's flat
// "Demo" branch — four levels of folders (Plant/Building/Line/Motor) plus
// a sibling branch (Plant/BuildingB/Tank1) — so the Docker-based
// integration test exercises Browse across real nesting, not just a
// single level.
//
// It intentionally does not implement backend.ChangeNotifier: the
// subscription manager falls back to polling Reader.Read on a schedule,
// which is enough to exercise Subscribe/SubscriptionPolledRefresh
// end-to-end without the extra complexity of a push-mode watcher.
package plantbackend

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

type item struct {
	mu    sync.Mutex
	value xmlda.Value
	// canonicalType is the item's data type, fixed at construction and
	// never changed by a write. A real OPC item has exactly one — it is
	// what standard property 1 (dataType) reports — and a write of some
	// other type is coerced to it or refused, never stored verbatim.
	canonicalType xmlda.QName
	quality       xmlda.OPCQuality
	ts            time.Time
	writable      bool
	description   string
	// hasRange/rangeMin/rangeMax demonstrate backend.WriteOutcome.Clamped
	// (REQ-WRITE-005), mirroring memorybackend's Demo/Counter.
	hasRange           bool
	rangeMin, rangeMax float64
}

// Backend is a small in-memory OPC XML-DA data source with a four-level
// nested address space and a mix of scalar/array, writable/read-only
// items. It implements backend.StatusProvider, backend.Reader,
// backend.Writer, backend.Browser, and backend.PropertyReader.
type Backend struct {
	mu    sync.RWMutex
	items map[backend.ItemRef]*item
	tree  map[string][]string // parent item name ("" = root) -> child names
	start time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// tickingFloat64Items are periodically nudged by simulate to give
// Subscribe/SubscriptionPolledRefresh real changing data to observe.
var tickingFloat64Items = []string{
	"Plant/BuildingA/Line1/Motor1/Temperature",
	"Plant/BuildingA/Line2/Motor1/Temperature",
	"Plant/BuildingB/Tank1/Level",
}

// New constructs a Backend and starts its background value simulator.
// Call Close to stop it.
func New() *Backend {
	ctx, cancel := context.WithCancel(context.Background())
	b := &Backend{
		items: map[backend.ItemRef]*item{},
		tree: map[string][]string{
			"":                             {"Plant"},
			"Plant":                        {"BuildingA", "BuildingB"},
			"Plant/BuildingA":              {"Line1", "Line2"},
			"Plant/BuildingA/Line1":        {"Motor1"},
			"Plant/BuildingA/Line2":        {"Motor1"},
			"Plant/BuildingA/Line1/Motor1": {"Speed", "Temperature", "Running"},
			"Plant/BuildingA/Line2/Motor1": {"Speed", "Temperature", "Running"},
			"Plant/BuildingB":              {"Tank1"},
			"Plant/BuildingB/Tank1":        {"Level", "Valve", "Label", "Capacity", "Sensors"},
		},
		start:  time.Now(),
		ctx:    ctx,
		cancel: cancel,
	}

	for _, motor := range []string{"Plant/BuildingA/Line1/Motor1", "Plant/BuildingA/Line2/Motor1"} {
		b.addItem(motor+"/Speed", xmlda.NewFloat64(0), true, "Motor speed in RPM; writes are clamped to [0, 3000].")
		b.items[backend.ItemRef{ItemName: motor + "/Speed"}].hasRange = true
		b.items[backend.ItemRef{ItemName: motor + "/Speed"}].rangeMin = 0
		b.items[backend.ItemRef{ItemName: motor + "/Speed"}].rangeMax = 3000
		b.addItem(motor+"/Temperature", xmlda.NewFloat64(40), false, "Simulated read-only motor temperature.")
		b.addItem(motor+"/Running", xmlda.NewBool(false), true, "Writable run/stop switch.")
	}

	b.addItem("Plant/BuildingB/Tank1/Level", xmlda.NewFloat64(50), false, "Simulated read-only tank fill level (%).")
	b.addItem("Plant/BuildingB/Tank1/Valve", xmlda.NewBool(false), true, "Writable inlet valve state.")
	b.addItem("Plant/BuildingB/Tank1/Label", xmlda.NewString("Tank 1"), true, "Writable free-text label.")
	b.addItem("Plant/BuildingB/Tank1/Capacity", xmlda.NewInt32(10000), false, "Read-only tank capacity in liters.")
	// An array-typed item, so the Dockerized end-to-end suite drives an
	// ArrayOf<X> value over real HTTP through the reference client — the
	// one value shape no backend in this repository used to produce, and
	// therefore the one the wire path was never exercised for.
	b.addItem("Plant/BuildingB/Tank1/Sensors",
		xmlda.NewArrayValue(xmlda.NewFloat64Array([]float64{0, 0, 0})), false,
		"Simulated read-only array of three tank sensor readings (ArrayOfDouble).")

	// A WRITABLE array item, and deliberately one the simulator does not
	// touch: writing an array and reading it back is only observable if
	// nothing overwrites it a tick later. Its element type is int
	// because that is what the reference client sends for a []int32, and
	// an item accepts its own type — the point of the round trip is the
	// array wire path, not a coercion.
	b.addItem("Plant/BuildingB/Tank1/Setpoints",
		xmlda.NewArrayValue(xmlda.NewInt32Array([]int32{0, 0, 0})), true,
		"Writable array of three sensor setpoints (ArrayOfInt).")

	b.wg.Go(func() { b.simulate(ctx) })
	return b
}

// Close stops the background simulator and waits for it to exit.
func (b *Backend) Close() {
	b.cancel()
	b.wg.Wait()
}

func (b *Backend) addItem(name string, v xmlda.Value, writable bool, description string) {
	b.items[backend.ItemRef{ItemName: name}] = &item{
		value: v, canonicalType: v.TypeName(),
		quality: xmlda.NewGoodQuality(), ts: time.Now(),
		writable: writable, description: description,
	}
}

// simulate periodically nudges tickingFloat64Items so a client polling
// via SubscriptionPolledRefresh observes real, changing data.
func (b *Backend) simulate(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, name := range tickingFloat64Items {
				ref := backend.ItemRef{ItemName: name}
				b.mu.RLock()
				itm := b.items[ref]
				b.mu.RUnlock()
				itm.mu.Lock()
				cur, _ := itm.value.Float64()
				itm.value = xmlda.NewFloat64(cur + (rand.Float64()-0.5)*2)
				itm.ts = time.Now()
				itm.mu.Unlock()
			}
			// The array item ticks too, so a soak-test subscription sees
			// an ArrayOf<X> value change over time rather than only at
			// creation.
			ref := backend.ItemRef{ItemName: "Plant/BuildingB/Tank1/Sensors"}
			b.mu.RLock()
			itm := b.items[ref]
			b.mu.RUnlock()
			itm.mu.Lock()
			readings := make([]float64, 3)
			for i := range readings {
				readings[i] = 20 + rand.Float64()*10
			}
			itm.value = xmlda.NewArrayValue(xmlda.NewFloat64Array(readings))
			itm.ts = time.Now()
			itm.mu.Unlock()
		}
	}
}

// GetStatus implements backend.StatusProvider.
func (b *Backend) GetStatus(ctx context.Context, locale string) (backend.ServerStatus, error) {
	return backend.ServerStatus{
		State:              xmlda.ServerStateRunning,
		StartTime:          b.start,
		ProductVersion:     "0.1.0-dockerintegration",
		VendorInfo:         "gopcxmlda-server docker integration test fixture",
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
		// Coerced to the item's own type before anything else. Storing
		// the written value verbatim let a write CHANGE an item's data
		// type — an int written to Speed made a double item report
		// xsi:type="xsd:int" from then on, contradicting the dataType
		// property — and it silently disabled range checking, because
		// the clamp below could only read a double. A real OPC item has
		// one canonical type; a value that will not fit it is that
		// item's E_BADTYPE (§3.4).
		newValue, ok := coerceToCanonical(it.Value, itm.canonicalType)
		if !ok {
			out[i] = backend.Result[backend.WriteOutcome]{ResultID: xmlda.ErrBadType}
			continue
		}
		clamped := false
		if itm.hasRange {
			// NumericAsFloat64, not Float64: the range applies to the
			// magnitude whatever numeric type carried it here.
			if f, numeric := newValue.NumericAsFloat64(); numeric {
				switch {
				case f < itm.rangeMin:
					newValue, clamped = xmlda.NewFloat64(itm.rangeMin), true
				case f > itm.rangeMax:
					newValue, clamped = xmlda.NewFloat64(itm.rangeMax), true
				}
				if clamped {
					// The clamp produced a double; put it back into the
					// item's own type so clamping cannot change the type
					// either.
					if back, ok := coerceToCanonical(newValue, itm.canonicalType); ok {
						newValue = back
					}
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
		itm.mu.Unlock()
		if clamped {
			cv := newValue
			out[i] = backend.Result[backend.WriteOutcome]{Value: backend.WriteOutcome{Clamped: true, Value: &cv}}
		} else {
			out[i] = backend.Result[backend.WriteOutcome]{}
		}
	}
	return out, nil
}

// Browse implements backend.Browser. The address space here is genuinely
// multi-level, so — unlike memorybackend's doc comment — recursion into
// grandchildren does happen, one Browse call at a time, exactly as a real
// OPC XML-DA client is expected to walk it.
func (b *Backend) Browse(ctx context.Context, req backend.BrowseRequest) (backend.BrowseResult, error) {
	parent := req.Ref.ItemName
	b.mu.RLock()
	children := append([]string(nil), b.tree[parent]...)
	b.mu.RUnlock()

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

		if req.Filter == xmlda.BrowseFilterItem && !isItem {
			continue
		}
		if req.Filter == xmlda.BrowseFilterBranch && !hasChildren {
			continue
		}

		el := backend.BrowseElement{Name: name, Ref: &ref, IsItem: isItem, HasChildren: hasChildren}
		if isItem && req.ReturnAllProperties {
			el.Properties = propertiesFor(itm, req.ReturnPropertyValues)
		}
		candidates = append(candidates, el)
	}

	// Pagination, so this fixture exercises the framework's
	// continuation-point handling (REQ-BROWSE-002) rather than leaving it
	// untested at the container level. The cursor is this backend's own
	// private format — the server wraps it behind a keyed MAC before it
	// reaches the wire — but it is still validated here rather than
	// trusted: a token the server issued can be replayed within its
	// lifetime, and the address space may have changed underneath it in
	// the meantime, so an offset that no longer fits is an error rather
	// than a slice index. See backend.BrowseRequest.ContinuationPoint.
	offset := 0
	if req.ContinuationPoint != "" {
		n, err := strconv.Atoi(req.ContinuationPoint)
		if err != nil || n < 0 || n > len(candidates) {
			return backend.BrowseResult{}, fmt.Errorf("plantbackend: unusable continuation point %q", req.ContinuationPoint)
		}
		offset = n
	}
	page := candidates[offset:]
	next := ""
	if req.MaxElementsReturned > 0 && len(page) > req.MaxElementsReturned {
		page = page[:req.MaxElementsReturned]
		next = strconv.Itoa(offset + len(page))
	}
	return backend.BrowseResult{
		Elements:          page,
		ContinuationPoint: next,
		MoreElements:      next != "",
	}, nil
}

// coerceToCanonical converts v to the type want names, reporting false
// if it cannot.
//
// It covers the types this fixture's items actually declare. A real
// backend would reach for whatever its data source offers; the point
// here is that SOME canonical type is enforced, because a fixture whose
// items change type on write cannot catch a server that mishandles
// types.
func coerceToCanonical(v xmlda.Value, want xmlda.QName) (xmlda.Value, bool) {
	if !v.IsValid() {
		return xmlda.Value{}, false
	}
	if v.TypeName() == want {
		return v, true
	}
	switch want {
	case xmlda.QName{Space: xmlda.XSDNamespace, Local: "double"}:
		if f, ok := v.NumericAsFloat64(); ok {
			return xmlda.NewFloat64(f), true
		}
	case xmlda.QName{Space: xmlda.XSDNamespace, Local: "int"}:
		f, ok := v.NumericAsFloat64()
		if !ok {
			return xmlda.Value{}, false
		}
		// A fractional or out-of-range value is not an int; saying so is
		// E_BADTYPE rather than silently truncating.
		if f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
			return xmlda.Value{}, false
		}
		return xmlda.NewInt32(int32(f)), true
	case xmlda.QName{Space: xmlda.XSDNamespace, Local: "boolean"}:
		if b, err := v.Bool(); err == nil {
			return xmlda.NewBool(b), true
		}
	case xmlda.QName{Space: xmlda.XSDNamespace, Local: "string"}:
		if s, err := v.String(); err == nil {
			return xmlda.NewString(s), true
		}
	}
	return xmlda.Value{}, false
}

// propertiesFor answers with the standard properties this fixture can
// legitimately know about an item. §3.9 requires a server to return "all
// properties which are available", so the set is the server's to choose
// — but a fixture that offers only dataType and description does not
// exercise the property surface a real client reaches for. An
// independent client (NothinRandom/pyopcxmlda) models a property reply
// as dataType/value/timestamp/accessRights/scanRate/quality/euType, and
// every one of those is already sitting in the item struct.
func propertiesFor(itm *item, includeValue bool) []backend.Property {
	itm.mu.Lock()
	value := itm.value
	description := itm.description
	quality := itm.quality
	ts := itm.ts
	writable := itm.writable
	hasRange := itm.hasRange
	rangeMin, rangeMax := itm.rangeMin, itm.rangeMax
	itm.mu.Unlock()

	access := xmlda.AccessRightsReadable
	if writable {
		access = xmlda.AccessRightsReadWritable
	}
	euType := xmlda.EUTypeNoEnum
	if hasRange {
		// An item with an engineering range is the specification's
		// "analog" case, and highEU/lowEU are the properties that carry
		// the range (IDs 102/103).
		euType = xmlda.EUTypeAnalog
	}

	props := []backend.Property{
		{ID: xmlda.PropDataType, Value: xmlda.NewQNameValue(value.TypeName())},
		// §3.1.10 p.40 gives this property the data type OPCQuality —
		// the one complex type the specification puts in a <Value>
		// position, which xmlda.NewQualityValue exists to express.
		{ID: xmlda.PropQuality, Value: xmlda.NewQualityValue(quality)},
		{ID: xmlda.PropTimestamp, Value: xmlda.NewDateTime(ts)},
		{ID: xmlda.PropAccessRights, Value: xmlda.NewString(access)},
		// The fastest rate the source could be sampled at, in
		// milliseconds (§3.1.10 p.40) — the fixture's simulator ticks
		// every 100ms.
		{ID: xmlda.PropScanRate, Value: xmlda.NewFloat32(100)},
		{ID: xmlda.PropEUType, Value: xmlda.NewString(euType)},
		{ID: xmlda.PropDescription, Value: xmlda.NewString(description)},
	}
	if hasRange {
		props = append(props,
			backend.Property{ID: xmlda.PropHighEU, Value: xmlda.NewFloat64(rangeMax)},
			backend.Property{ID: xmlda.PropLowEU, Value: xmlda.NewFloat64(rangeMin)},
		)
	}
	if includeValue {
		props = append(props, backend.Property{ID: xmlda.PropValue, Value: value})
	}
	return props
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
		out[i] = backend.Result[[]backend.Property]{Value: propertiesFor(itm, r.IncludeValues)}
	}
	return out, nil
}
