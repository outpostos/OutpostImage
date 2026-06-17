package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"golang.org/x/sync/errgroup"
)

// Serve starts an HTTP server and manages its lifecycle within the given errgroup.
// It starts listening on the given address and shuts down gracefully when ctx is cancelled.
func Serve(ctx context.Context, g *errgroup.Group, addr string, handler http.Handler, name string) {
	srv := &http.Server{Addr: addr, Handler: handler}

	g.Go(func() error {
		slog.Info("server listening", "name", name, "addr", addr)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}
		return nil
	})

	g.Go(func() error {
		<-ctx.Done()
		slog.Info("server shutting down", "name", name)
		return srv.Shutdown(context.Background())
	})
}

// HandleHealthz returns a handler that responds with {"status":"ok"}.
func HandleHealthz() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// HandleReadyz returns a handler that responds with {"status":"ready"}.
func HandleReadyz() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	}
}
