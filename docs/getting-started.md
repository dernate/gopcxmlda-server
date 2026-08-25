# Getting Started

This walks through building the smallest useful OPC XML-DA server: one read-only item, served over HTTP.
For a fuller example (writable items, Browse, GetProperties, Subscribe, graceful shutdown), see
[`examples/basic-server/`](../examples/basic-server/) — its `memorybackend` package and `main.go` are real,
runnable code, not pseudocode.

## 1. Implement a backend

Your backend is a plain Go value that implements a handful of small interfaces from package `backend`. Only
`backend.StatusProvider` and `backend.Reader` are required:

```go
package main

import (
	"context"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/xmlda"
)

type myBackend struct {
	start time.Time
}

func (b *myBackend) GetStatus(ctx context.Context, locale string) (backend.ServerStatus, error) {
	return backend.ServerStatus{
		State:              xmlda.ServerStateRunning,
		StartTime:          b.start,
		ProductVersion:     "0.1.0",
		SupportedLocaleIDs: []string{"en-US"},
	}, nil
}

func (b *myBackend) Read(ctx context.Context, items []backend.ReadRequestItem) ([]backend.Result[backend.ItemSample], error) {
	out := make([]backend.Result[backend.ItemSample], len(items))
	for i, it := range items {
		if it.Ref.ItemName != "Temperature" {
			out[i] = backend.Result[backend.ItemSample]{ResultID: xmlda.ErrUnknownItemName}
			continue
		}
		out[i] = backend.Result[backend.ItemSample]{Value: backend.ItemSample{
			Value:     xmlda.NewFloat64(21.5),
			Quality:   xmlda.NewGoodQuality(),
			Timestamp: time.Now(),
		}}
	}
	return out, nil
}
```

A few things worth noting, all covered in depth in
[`docs/backend-implementation.md`](backend-implementation.md):

- `Read`'s returned slice must have exactly one `backend.Result` per requested item, in the same order.
- A non-nil `error` return is a **whole-operation** failure (becomes a SOAP Fault). An unknown/invalid
  *item* is not an error return — it is a per-item `ResultID` (here, `xmlda.ErrUnknownItemName`), so the
  rest of the batch is still served normally.
- `xmlda.Value` is a typed container, not `any` — use one of its `NewX` constructors (`NewFloat64`,
  `NewInt32`, `NewString`, `NewBool`, `NewDateTime`, ...) to build one, and one of its typed accessors
  (`.Float64()`, `.Int32()`, ...) to read one back; see [`docs/specification/type-mapping.md`](specification/type-mapping.md)
  for the full XSD↔OPC↔Go table.

## 2. Wire it into a server

```go
func main() {
	be := backend.Backend{
		Status: &myBackend{start: time.Now()},
		Reader: &myBackend{start: time.Now()}, // same value in practice; split here only for illustration
	}

	srv, err := server.NewServer(":8080", server.Deps{Backend: be}, server.Config{})
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(srv.Start())
}
```

`server.NewServer` validates the backend (`backend.Backend.Validate`) up front, so a missing `Status` or
`Reader` fails at startup, not on the first request. `Writer`, `Browser`, and `Properties` are left nil here
— every `Write` request will be rejected with `E_ACCESS_DENIED` (a read-only server), and `Browse`/
`GetProperties` will fault with `E_NOTSUPPORTED`, without ever calling into your backend.

`server.Deps.Clock`/`Logger`/`Metrics` default to `clock.Real{}`, a no-op logger, and no-op metrics
respectively — see [`docs/server-configuration.md`](server-configuration.md) for how to supply your own.

## 3. Send a request

Every OPC XML-DA operation is a `POST` of a SOAP-enveloped XML body to the same endpoint:

```sh
curl -s http://localhost:8080/ -H 'Content-Type: text/xml' --data-binary @- <<'EOF'
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">
  <soap:Body>
    <Read xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/">
      <Options ClientRequestHandle="example-1"/>
      <ItemList>
        <Items ItemName="Temperature"/>
      </ItemList>
    </Read>
  </soap:Body>
</soap:Envelope>
EOF
```

The response's `<Value>` element carries an `xsi:type` attribute declaring its exact XSD type — the server
never guesses a client's intended type, and a client should not assume one either (see
[`docs/interoperability.md`](interoperability.md) for how namespace prefixes are resolved).

## 4. Add write support, Browse, subscriptions

Implement `backend.Writer`/`backend.Browser`/`backend.PropertyReader` and add them to the `backend.Backend`
struct to enable `Write`/`Browse`/`GetProperties`. `Subscribe`/`SubscriptionPolledRefresh`/
`SubscriptionCancel` work automatically once `Reader` is set — no separate opt-in — and use polling via
`Reader.Read` unless your `Reader` also implements `backend.ChangeNotifier`, in which case the subscription
engine pushes changes instead. See `examples/basic-server/memorybackend/backend.go` for a complete
`ChangeNotifier` implementation, and
[`docs/architecture/subscription-model.md`](architecture/subscription-model.md) for how the engine decides
between the two.

## 5. Graceful shutdown

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

go func() { log.Fatal(srv.Start()) }()

<-ctx.Done()
shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
if err := srv.Shutdown(shutdownCtx); err != nil {
	log.Printf("shutdown: %v", err)
}
```

`Shutdown` cancels every subscription before stopping the HTTP server, so an in-flight long-poll
`SubscriptionPolledRefresh` call returns immediately instead of the shutdown blocking for the client's
requested Hold+Wait window. See `examples/basic-server/main.go` for the complete, tested version of this.
