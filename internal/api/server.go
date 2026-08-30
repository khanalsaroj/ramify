// SPDX-License-Identifier: Apache-2.0

// Package api implements Ramify's local control plane HTTP API: the GitHub webhook
// receiver and the /environments CRUD surface used by the ramify CLI. It listens on
// a unix socket by default, with an optional token-protected TCP listener for
// remote CLI access.
package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/khanalsaroj/ramify/internal/core"
	"github.com/khanalsaroj/ramify/internal/metrics"
	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// webhookWorkerCount bounds Server's own webhook-processing pool — the
// low-latency counterpart to cmd/ramifyd's 2s-poll durable event worker. A fixed
// pool reading a bounded channel means a burst of webhooks can never spawn
// unbounded goroutines/provider work: once the pool and its queue are full,
// newly accepted webhooks simply wait for the poll-based worker to pick up their
// already-durable event instead. The value mirrors defaultMaxEventWorkers.
const webhookWorkerCount = 8

// webhookQueueSize bounds how many durably-persisted-but-not-yet-dispatched
// webhook events Server will hold for its own worker pool before falling back
// silently to the poll-based worker.
const webhookQueueSize = 256

// Server is Ramify's local control plane HTTP API.
type Server struct {
	store        store.Store
	reconciler   *core.Reconciler
	git          providerapi.GitProvider
	deploy       providerapi.DeployProvider
	baseDomain   string
	subdomainMax int
	logger       *slog.Logger
	metrics      *metrics.Metrics

	// webhookQueue feeds Server's own bounded pool of webhook-processing
	// workers (started in NewServer, run against context.Background() for the
	// life of the process — see runWebhookWorker). It is deliberately not tied
	// to Serve's ctx: a webhook accepted just before shutdown should still get
	// to finish reconciling like it always could, and every test in this
	// package constructs a Server directly via NewServer without ever calling
	// Serve.
	webhookQueue chan store.Event

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
	metricSet ...*metrics.Metrics,
) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	var m *metrics.Metrics
	if len(metricSet) > 0 {
		m = metricSet[0]
	}
	if m == nil {
		m = &metrics.Metrics{}
	}
	s := &Server{
		store:        st,
		reconciler:   reconciler,
		git:          git,
		deploy:       deploy,
		baseDomain:   baseDomain,
		subdomainMax: 63,
		logger:       logger,
		metrics:      m,
		webhookQueue: make(chan store.Event, webhookQueueSize),
	}
	s.router = s.routes()
	for range webhookWorkerCount {
		go s.runWebhookWorker()
	}
	return s
}

// runWebhookWorker is one of a fixed-size pool of goroutines dispatching
// webhook-triggered durable events. See webhookQueue's doc comment for why it
// runs against context.Background() rather than a ctx tied to Serve.
func (s *Server) runWebhookWorker() {
	for ev := range s.webhookQueue {
		ctx, cancel := context.WithTimeout(context.Background(), webhookProcessTimeout)
		s.reconciler.ProcessEvent(ctx, ev)
		cancel()
	}
}

// ServeHTTP implements http.Handler, primarily so tests can exercise Server without
// opening a real listener.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()
	r.Use(securityHeaders)
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Get("/metrics", s.handleMetrics)
	// The dashboard is static, read-only, and unauthenticated (see tokenAuth): it
	// is only the shell that then asks the operator for a bearer token. Register
	// GET and HEAD alone so no write verb can ever reach the asset handler.
	r.Get("/dashboard", s.handleDashboard)
	r.Head("/dashboard", s.handleDashboard)
	r.Get("/dashboard/config", s.handleDashboardConfig)
	r.Get("/dashboard/*", s.handleDashboard)
	r.Head("/dashboard/*", s.handleDashboard)
	// The provider segment also preserves the existing /webhooks/github URL. It is
	// cosmetic: a daemon has exactly one Git provider configured, and
	// Server.webhookHeaders asks that provider which headers to read.
	r.Post("/webhooks/{provider}", s.handleWebhook)
	r.Route("/environments", func(r chi.Router) {
		r.Get("/", s.handleListEnvironments)
		r.Post("/", s.handleCreateEnvironment)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", s.handleGetEnvironment)
			r.Put("/", s.handleUpdateEnvironment)
			r.Delete("/", s.handleDeleteEnvironment)
			r.Post("/sleep", s.handleSleepEnvironment)
			r.Post("/wake", s.handleWakeEnvironment)
			r.Post("/rollback", s.handleRollbackEnvironment)
			r.Get("/logs", s.handleLogs)
		})
	})
	return r
}

// readHeaderTimeout, readTimeout, and idleTimeout bound how long a connection may
// sit reading request headers/body or idling between keep-alive requests, closing
// off slow-loris-style connection exhaustion. There is deliberately no
// WriteTimeout: net/http measures it from the end of the request headers to the
// end of the response, so it would bound total handler execution time, not just
// the write. handleCreateEnvironment/handleUpdateEnvironment call
// core.Reconciler.Apply synchronously, which can legitimately run for minutes
// (maxApplyAttempts retries, each waiting up to the configured readiness timeout),
// and a blanket WriteTimeout would abort that mid-flight. A client that stops
// reading its response is instead bounded by request-context cancellation, which
// the reconciler already threads into every provider call.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

// Serve listens on socketPath (created fresh on every start) and, if tcpAddr is
// non-empty, also on tcpAddr with bearer-token authentication using tcpToken. If
// both tlsCertFile and tlsKeyFile are set, the TCP listener serves TLS; callers
// are expected to have already enforced (config.Validate does) that a
// non-loopback tcpAddr either has TLS configured or an explicit operator
// override, since Serve itself has no opinion on which addresses are "remote".
// It blocks until ctx is canceled, then shuts down both listeners gracefully.
func (s *Server) Serve(ctx context.Context, socketPath, tcpAddr, tcpToken string, tlsCertFile, tlsKeyFile string) error {
	// Acquired before any socket work: defense in depth for the narrow race
	// between probeStaleSocket's dial check and the actual bind/listen below,
	// where two ramifyd processes racing to start against the same socketPath
	// could otherwise both pass the probe before either is listening. See
	// acquireProcessLock's doc comment for why this is only a real lock on Unix.
	releaseLock, err := acquireProcessLock(socketPath + ".lock")
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	defer func() {
		if err := releaseLock(); err != nil {
			s.logger.Error("releasing process lock", "error", err)
		}
	}()

	unixListener, err := listenUnix(socketPath)
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}

	unixServer := &http.Server{Handler: s, ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout, IdleTimeout: idleTimeout}
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
			// The unix server is already running; shut it down rather than
			// orphaning its goroutine and listener on the way out.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = unixServer.Shutdown(shutdownCtx) // best-effort; the listen failure is the error worth reporting
			wg.Wait()
			return fmt.Errorf("api: listening on %s: %w", tcpAddr, err)
		}
		tcpServer = &http.Server{Addr: tcpAddr, Handler: tokenAuth(tcpToken)(s), ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout, IdleTimeout: idleTimeout}
		wg.Go(func() {
			var serveErr error
			if tlsCertFile != "" && tlsKeyFile != "" {
				serveErr = tcpServer.ServeTLS(tcpListener, tlsCertFile, tlsKeyFile)
			} else {
				serveErr = tcpServer.Serve(tcpListener)
			}
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				errCh <- fmt.Errorf("api: tcp server: %w", serveErr)
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

// staleSocketDialTimeout bounds the connect attempt in probeStaleSocket. A
// listening socket accepts near-instantly, so this only needs to be long enough
// to not misjudge a socket as stale under load, not long enough to matter for
// startup latency.
const staleSocketDialTimeout = 2 * time.Second

// probeStaleSocket reports whether a socket file at path is stale (safe to
// remove and rebind) by attempting to connect to it. A successful connect means
// some process is actively accepting on it — almost certainly another running
// ramifyd — so the caller must not remove it. Any dial failure (connection
// refused, no such file, etc.) means whatever is on disk is not being served,
// which is exactly what "stale" means here.
func probeStaleSocket(path string) error {
	conn, err := net.DialTimeout("unix", path, staleSocketDialTimeout)
	if err != nil {
		return nil
	}
	_ = conn.Close()
	return fmt.Errorf("refusing to remove live socket %s: another ramifyd instance appears to be running", path)
}

// listenUnix removes a stale socket file at path before listening — one left
// behind by a prior unclean shutdown, verified via probeStaleSocket rather than
// assumed, so a second daemon accidentally started against the same path
// refuses to hijack the first one's live socket instead of disconnecting it.
func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("creating socket directory: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		if err := probeStaleSocket(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("checking existing socket %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = l.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("securing socket %s: %w", path, err)
	}
	return l, nil
}

// securityHeaders sets a small set of defensive headers on every response,
// including the unauthenticated dashboard shell: CSP scoped to the dashboard's
// actual shape (one self-contained HTML file, inline style/script, fetches to
// its own origin only) blocks a script-injection scenario from exfiltrating
// the bearer token to a third-party origin, and the rest are standard
// clickjacking/MIME-sniffing/referrer hardening.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

// tokenAuth requires a matching "Authorization: Bearer <token>" header on every
// request. It exists only for the optional TCP listener; the unix socket relies on
// filesystem permissions instead.
//
// The dashboard's own assets are exempt, and deliberately so: a browser cannot
// attach an Authorization header to a top-level navigation, so gating the page
// itself would make it unreachable. What is exempted is a static page and a
// base-domain string — every route that reads or changes an environment still
// requires the token, which the page prompts the operator for and sends on the
// XHRs it makes. Nothing behind this exemption reveals environment data.
func tokenAuth(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isDashboardAsset(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			provided := r.Header.Get("Authorization")
			expected := "Bearer " + token
			if token == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isDashboardAsset reports whether path is part of the unauthenticated dashboard
// shell. The prefix is matched on a path boundary so a route such as
// "/dashboardsecrets" can never inherit the exemption.
func isDashboardAsset(path string) bool {
	return path == "/dashboard" || strings.HasPrefix(path, "/dashboard/")
}
