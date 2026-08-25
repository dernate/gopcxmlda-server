// Command basic-server runs a minimal OPC XML-DA server on top of this
// library, backed by memorybackend's small in-memory data source. It
// demonstrates GetStatus, Browse, Read, Write, GetProperties, Subscribe,
// SubscriptionPolledRefresh, and SubscriptionCancel against real, changing
// data, and a controlled shutdown on SIGINT/SIGTERM.
//
// Run it, then in another shell:
//
//	curl -s http://localhost:8080/ -H 'Content-Type: text/xml' --data-binary @request.xml
//
// See docs/getting-started.md for example request bodies.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/examples/basic-server/memorybackend"
	"github.com/dernate/gopcxmlda-server/server"
)

func main() {
	os.Exit(run())
}

// run holds all cleanup in deferred calls and returns the process exit
// code, rather than calling os.Exit directly on an error path: os.Exit
// skips deferred functions, which would abandon be.Close()'s documented
// contract (draining the background simulator and any live WatchItems
// goroutines) on every non-happy-path exit.
func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	addr := ":8080"
	if v := os.Getenv("BASIC_SERVER_ADDR"); v != "" {
		addr = v
	}

	be := memorybackend.New()
	defer be.Close()

	srv, err := server.NewServer(addr, server.Deps{
		Backend: backend.Backend{
			Status:     be,
			Reader:     be,
			Writer:     be,
			Browser:    be,
			Properties: be,
		},
		Logger: logger,
	}, server.Config{})
	if err != nil {
		logger.Error("failed to construct server", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		serveErr <- srv.Start()
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			logger.Error("server stopped unexpectedly", "error", err)
			return 1
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown did not complete cleanly", "error", err)
			return 1
		}
		logger.Info("shutdown complete")
	}
	return 0
}
