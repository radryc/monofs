package diagnostics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewHandler returns a diagnostics HTTP handler exposing /metrics and pprof routes.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	return mux
}

// StartServer starts a diagnostics server when addr is non-empty.
// It returns nil when diagnostics are disabled.
func StartServer(logger *slog.Logger, component, addr string) *http.Server {
	if strings.TrimSpace(addr) == "" {
		return nil
	}
	server := &http.Server{Addr: addr, Handler: NewHandler()}
	go func() {
		logger.Info("diagnostics server listening", "component", component, "addr", addr, "pprof_path", "/debug/pprof/")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("diagnostics server error", "component", component, "error", err)
		}
	}()
	return server
}

// ShutdownServer gracefully shuts down a diagnostics server started with StartServer.
func ShutdownServer(logger *slog.Logger, component string, server *http.Server) {
	if server == nil {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("diagnostics shutdown error", "component", component, "error", fmt.Errorf("%w", err))
	}
}
