// Command server runs an OPC XML-DA server backed by plantbackend's
// nested demo data set, built into a Docker image by
// test/dockerintegration/Dockerfile purely for use by the Docker-based
// integration test in this module — not an example for library users
// (see examples/basic-server for that).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/server"
	"github.com/dernate/gopcxmlda-server/test/dockerintegration/plantbackend"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	addr := ":8080"
	if v := os.Getenv("SERVER_ADDR"); v != "" {
		addr = v
	}

	be := plantbackend.New()
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
