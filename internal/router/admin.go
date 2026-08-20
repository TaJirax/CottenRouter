package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/TaJirax/CottenRouter/internal/telemetry"
)

func (s *Server) ServeAdmin(ctx context.Context, listener net.Listener) error {
	s.listenersMu.Lock()
	s.adminListener = listener
	s.listenersMu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s.stats.Snapshot())
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: s.cfg.QueryTimeout(), IdleTimeout: s.cfg.QueryTimeout()}
	s.listenersMu.Lock()
	s.adminServer = server
	s.listenersMu.Unlock()
	go func() { <-ctx.Done(); _ = server.Close() }()
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("admin server: %w", err)
}

func (s *Server) Stats() telemetry.Snapshot { return s.stats.Snapshot() }
