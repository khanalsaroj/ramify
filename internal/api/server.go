// SPDX-License-Identifier: Apache-2.0

// Package api implements Ramify's local control plane HTTP API: the GitHub webhook
// receiver and the /environments CRUD surface used by the ramify CLI. It listens on
// a unix socket by default, with an optional token-protected TCP listener for
// remote CLI access.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/khanalsaroj/ramify/internal/core"
	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// LogFetcher is an optional capability a DeployProvider implementation may satisfy,
// checked via a type assertion in handleLogs. It is deliberately not part of
// providerapi (§5 of the build spec forbids speculative interface methods): log
// retrieval has no meaningful contract shared across every possible deploy target.
type LogFetcher interface {
	Logs(ctx context.Context, ref string) (string, error)
}

// Server is Ramify's local control plane HTTP API.
type Server struct {
	store        store.Store
	reconciler   *core.Reconciler
	git          providerapi.GitProvider
	deploy       providerapi.DeployProvider
	baseDomain   string
	subdomainMax int
	logger       *slog.Logger

	router chi.Router
}

// NewServer constructs a Server and wires its routes.
func NewServer(
	st store.Store,
	reconciler *core.Reconciler,
	git providerapi.GitProvider,
	deploy providerapi.DeployProvider,
	baseDomain string,
	logger *slog.Logger,
) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		store:        st,
		reconciler:   reconciler,
		git:          git,
		deploy:       deploy,
		baseDomain:   baseDomain,
		subdomainMax: 63,
		logger:       logger,
	}
	s.router = s.routes()
	return s
}

// ServeHTTP implements http.Handler, primarily so tests can exercise Server without
// opening a real listener.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/healthz", s.handleHealthz)
	r.Post("/webhooks/github", s.handleWebhook)
	r.Route("/environments", func(r chi.Router) {
		r.Get("/", s.handleListEnvironments)
		r.Post("/", s.handleCreateEnvironment)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", s.handleGetEnvironment)
			r.Put("/", s.handleUpdateEnvironment)
			r.Delete("/", s.handleDeleteEnvironment)
			r.Post("/sleep", s.handleSleepEnvironment)
			r.Post("/wake", s.handleWakeEnvironment)
			r.Get("/logs", s.handleLogs)
		})
	})
	return r
}

// Serve listens on socketPath (created fresh on every start) and, if tcpAddr is
// non-empty, also on tcpAddr with bearer-token authentication using tcpToken. It
// blocks until ctx is canceled, then shuts down both listeners gracefully.
func (s *Server) Serve(ctx context.Context, socketPath, tcpAddr, tcpToken string) error {
	unixListener, err := listenUnix(socketPath)
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}

	unixServer := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	var tcpServer *http.Server

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Go(func() {
		if err := unixServer.Serve(unixListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("api: unix socket server: %w", err)
		}
	})

	if tcpAddr != "" {
		tcpListener, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			return fmt.Errorf("api: listening on %s: %w", tcpAddr, err)
		}
		tcpServer = &http.Server{Addr: tcpAddr, Handler: tokenAuth(tcpToken)(s), ReadHeaderTimeout: 10 * time.Second}
		wg.Go(func() {
			if err := tcpServer.Serve(tcpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("api: tcp server: %w", err)
			}
		})
	}

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = unixServer.Shutdown(shutdownCtx) // best-effort; errCh already reports Serve failures
	if tcpServer != nil {
		_ = tcpServer.Shutdown(shutdownCtx) // best-effort; errCh already reports Serve failures
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// listenUnix removes any stale socket file at path before listening, so a prior
// unclean shutdown doesn't block startup.
func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("creating socket directory: %w", err)
	}
	if err := os.RemoveAll(path); err != nil {
		return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	return l, nil
}

// tokenAuth requires a matching "Authorization: Bearer <token>" header on every
// request. It exists only for the optional TCP listener; the unix socket relies on
// filesystem permissions instead.
func tokenAuth(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
