// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/khanalsaroj/ramify/internal/core"
	"github.com/khanalsaroj/ramify/internal/core/domain"
	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// webhookProcessTimeout bounds the background reconciliation triggered by a
// webhook; it runs detached from the request context so a slow deploy doesn't hold
// the HTTP response open past GitHub's own webhook delivery timeout.
const webhookProcessTimeout = 5 * time.Minute

const maxWebhookBody = 2 << 20

const maxAPIRequestBody = 1 << 20

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// One row is enough to prove the store answers queries.
	if _, err := s.store.ListEnvironments(r.Context(), store.ListOptions{Limit: 1}); err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.ListUnprocessedEvents(r.Context()); err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "event store unavailable")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	if err := s.metrics.WritePrometheus(w); err != nil {
		s.logger.Error("writing metrics", "error", err)
	}
}

// handleWebhook verifies and parses the inbound GitHub webhook, then triggers
// reconciliation asynchronously: Apply/Destroy already persist a request event to
// the store before making any provider call, so if the process dies mid-request the
// next startup's ReplayUnprocessedEvents recovers it. Responding before that work
// completes keeps webhook delivery well under GitHub's timeout.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	s.metrics.WebhookReceived.Add(1)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "reading request body")
		return
	}

	signatureHeader, deliveryHeader := s.webhookHeaders()
	ev, err := s.git.ParseWebhook(r.Context(), body, r.Header.Get(signatureHeader))
	switch {
	case errors.Is(err, providerapi.ErrInvalidWebhookSignature):
		s.metrics.WebhookRejected.Add(1)
		s.writeError(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	case errors.Is(err, providerapi.ErrUnhandledWebhookEvent):
		w.WriteHeader(http.StatusOK)
		return
	case err != nil:
		s.metrics.WebhookRejected.Add(1)
		s.logger.ErrorContext(r.Context(), "webhook: parse failed", "error", err)
		s.writeError(w, http.StatusBadRequest, "invalid webhook payload")
		return
	}

	payload, err := core.MarshalWebhookPayload(ev)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "encoding webhook event")
		return
	}
	deliveryID := r.Header.Get(deliveryHeader)
	if deliveryID == "" {
		s.writeError(w, http.StatusBadRequest, "missing webhook delivery ID")
		return
	}
	firstRetry := time.Now().UTC()
	inbox, err := s.store.CreateEvent(r.Context(), store.Event{
		Kind:          store.EventKindWebhookReceived,
		DedupeKey:     deliveryID,
		Payload:       payload,
		NextAttemptAt: &firstRetry,
	})
	if errors.Is(err, store.ErrConflict) {
		s.metrics.WebhookDuplicates.Add(1)
		// GitHub retries deliveries. The unique delivery ID makes retries safe.
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		s.metrics.WebhookRejected.Add(1)
		s.logger.ErrorContext(r.Context(), "webhook: persisting inbox event", "error", err, "delivery_id", deliveryID)
		s.writeError(w, http.StatusServiceUnavailable, "persisting webhook event")
		return
	}

	go s.processEvent(inbox.ID, ev) //nolint:gosec // durable inbox is persisted before this worker starts
	w.WriteHeader(http.StatusAccepted)
}

// WebhookHeaderNamer is an optional capability a GitProvider implementation may
// satisfy, checked via a type assertion in webhookHeaders. Each hosting platform
// puts its signature and delivery ID in differently named headers, and only the
// provider that verifies the signature knows which ones it reads — deriving them
// anywhere else lets the two drift apart silently.
type WebhookHeaderNamer interface {
	SignatureHeader() string
	DeliveryHeader() string
}

// webhookHeaders asks the configured GitProvider which request headers carry its
// signature and delivery ID, falling back to GitHub's names for a provider that
// does not implement WebhookHeaderNamer.
//
// Note these come from the configured provider, never from the {provider} URL
// segment: that segment exists only so each platform can be pointed at a URL that
// looks native to it, and a daemon has exactly one Git provider configured.
func (s *Server) webhookHeaders() (signature, delivery string) {
	if namer, ok := s.git.(WebhookHeaderNamer); ok {
		return namer.SignatureHeader(), namer.DeliveryHeader()
	}
	return "X-Hub-Signature-256", "X-GitHub-Delivery"
}

func (s *Server) processEvent(eventID string, ev providerapi.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), webhookProcessTimeout)
	defer cancel()
	claimed, err := s.store.ClaimEvent(ctx, eventID, time.Now().UTC(), time.Now().UTC().Add(webhookProcessTimeout))
	if err != nil || !claimed {
		if err != nil {
			s.logger.ErrorContext(ctx, "webhook: claiming inbox event failed", "error", err, "event_id", eventID)
		}
		return
	}
	if err := s.reconciler.ProcessWebhookEvent(ctx, ev); err != nil {
		s.metrics.ReconciliationFailures.Add(1)
		s.logger.ErrorContext(ctx, "webhook: processing failed", "error", err, "project", ev.Project, "branch", ev.Branch)
		if retryErr := s.store.MarkEventRetry(ctx, eventID, time.Now().UTC().Add(time.Second), err.Error()); retryErr != nil {
			s.logger.ErrorContext(ctx, "webhook: scheduling retry failed", "error", retryErr, "event_id", eventID)
		}
		return
	}
	s.metrics.Reconciliations.Add(1)
	if err := s.store.MarkEventProcessed(ctx, eventID, time.Now().UTC()); err != nil {
		s.logger.ErrorContext(ctx, "webhook: marking inbox event processed", "error", err, "event_id", eventID)
	}
}

// nextOffsetHeader advertises the offset of the following page. It is absent on
// the last page, which is how a client knows to stop. The response body stays a
// plain JSON array so existing clients keep working unchanged.
const nextOffsetHeader = "X-Ramify-Next-Offset"

func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	opts := store.ListOptions{
		Project: r.URL.Query().Get("project"),
		Branch:  r.URL.Query().Get("branch"),
	}
	var err error
	if opts.Limit, err = intParam(r, "limit"); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	if opts.Offset, err = intParam(r, "offset"); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid offset")
		return
	}

	// Ask for one extra row: if it comes back there is another page, and it is
	// dropped from the response. This avoids a second COUNT query.
	page := opts
	page.Limit = min(max(opts.Limit, 1), store.MaxListLimit) + 1
	if opts.Limit == 0 {
		page.Limit = store.DefaultListLimit + 1
	}

	envs, err := s.store.ListEnvironments(r.Context(), page)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "listing environments", "error", err)
		s.writeError(w, http.StatusInternalServerError, "listing environments")
		return
	}
	if len(envs) == page.Limit {
		envs = envs[:page.Limit-1]
		w.Header().Set(nextOffsetHeader, strconv.Itoa(page.Offset+len(envs)))
	}
	if envs == nil {
		envs = []store.Environment{} // encode as [] rather than null
	}

	s.writeJSON(w, http.StatusOK, envs)
}

// intParam reads a non-negative integer query parameter. A missing or empty
// value yields 0, letting the store apply its own default.
func intParam(r *http.Request, name string) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid %s parameter", name)
	}
	return v, nil
}

func (s *Server) handleGetEnvironment(w http.ResponseWriter, r *http.Request) {
	env, ok := s.lookupEnvironment(w, r)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, env)
}

// createEnvironmentRequest is the POST /environments request body.
type createEnvironmentRequest struct {
	Project     string `json:"project"`
	Branch      string `json:"branch"`
	Subdomain   string `json:"subdomain,omitempty"`
	ArtifactRef string `json:"artifact_ref"`
	PRNumber    int    `json:"pr_number,omitempty"`
}

func (s *Server) handleCreateEnvironment(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIRequestBody)
	var body createEnvironmentRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Project == "" || body.Branch == "" || body.ArtifactRef == "" {
		s.writeError(w, http.StatusBadRequest, "project, branch, and artifact_ref are required")
		return
	}
	if len(body.Project) > 256 || len(body.Branch) > 256 || len(body.ArtifactRef) > 1024 || len(body.Subdomain) > 63 {
		s.writeError(w, http.StatusBadRequest, "request fields exceed maximum length")
		return
	}
	if body.Subdomain == "" {
		body.Subdomain = domain.Normalize(body.Branch, s.subdomainMax)
	} else {
		body.Subdomain = domain.Normalize(body.Subdomain, s.subdomainMax)
	}

	env, err := s.reconciler.Apply(r.Context(), core.ApplyRequest{
		Project: body.Project, Branch: body.Branch, PRNumber: body.PRNumber,
		Subdomain: body.Subdomain, ArtifactRef: body.ArtifactRef,
	})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "creating environment", "error", err)
		s.writeError(w, http.StatusInternalServerError, "creating environment: "+err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, env)
}

// updateEnvironmentRequest is the PUT /environments/{id} request body.
type updateEnvironmentRequest struct {
	ArtifactRef string `json:"artifact_ref"`
}

func (s *Server) handleUpdateEnvironment(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIRequestBody)
	existing, ok := s.lookupEnvironment(w, r)
	if !ok {
		return
	}

	var body updateEnvironmentRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.ArtifactRef == "" {
		s.writeError(w, http.StatusBadRequest, "artifact_ref is required")
		return
	}
	if len(body.ArtifactRef) > 1024 {
		s.writeError(w, http.StatusBadRequest, "artifact_ref exceeds maximum length")
		return
	}

	env, err := s.reconciler.Apply(r.Context(), core.ApplyRequest{
		Project: existing.Project, Branch: existing.Branch, PRNumber: existing.PRNumber,
		Subdomain: existing.Subdomain, ArtifactRef: body.ArtifactRef,
	})
	if err != nil {
		s.logger.ErrorContext(r.Context(), "updating environment", "error", err, "environment_id", existing.ID)
		s.writeError(w, http.StatusInternalServerError, "updating environment: "+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, env)
}

func (s *Server) handleDeleteEnvironment(w http.ResponseWriter, r *http.Request) {
	env, ok := s.lookupEnvironment(w, r)
	if !ok {
		return
	}
	if err := s.reconciler.Destroy(r.Context(), env); err != nil {
		s.logger.ErrorContext(r.Context(), "destroying environment", "error", err, "environment_id", env.ID)
		s.writeError(w, http.StatusInternalServerError, "destroying environment: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSleepEnvironment(w http.ResponseWriter, r *http.Request) {
	env, ok := s.lookupEnvironment(w, r)
	if !ok {
		return
	}
	if env.DeployRef == "" {
		s.writeError(w, http.StatusConflict, "environment has no deployment to sleep")
		return
	}
	if err := s.deploy.Sleep(r.Context(), env.DeployRef); err != nil {
		s.logger.ErrorContext(r.Context(), "sleeping environment", "error", err, "environment_id", env.ID)
		s.writeError(w, http.StatusInternalServerError, "sleeping environment: "+err.Error())
		return
	}
	env.Status = store.StatusSleeping
	updated, err := s.store.UpdateEnvironment(r.Context(), env)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "recording sleep status", "error", err, "environment_id", env.ID)
		s.writeError(w, http.StatusInternalServerError, "recording sleep status")
		return
	}
	s.writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleWakeEnvironment(w http.ResponseWriter, r *http.Request) {
	env, ok := s.lookupEnvironment(w, r)
	if !ok {
		return
	}
	if env.DeployRef == "" {
		s.writeError(w, http.StatusConflict, "environment has no deployment to wake")
		return
	}
	if err := s.deploy.Wake(r.Context(), env.DeployRef); err != nil {
		s.logger.ErrorContext(r.Context(), "waking environment", "error", err, "environment_id", env.ID)
		s.writeError(w, http.StatusInternalServerError, "waking environment: "+err.Error())
		return
	}
	env.Status = store.StatusReady
	updated, err := s.store.UpdateEnvironment(r.Context(), env)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "recording wake status", "error", err, "environment_id", env.ID)
		s.writeError(w, http.StatusInternalServerError, "recording wake status")
		return
	}
	s.writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	env, ok := s.lookupEnvironment(w, r)
	if !ok {
		return
	}
	if env.DeployRef == "" {
		s.writeError(w, http.StatusConflict, "environment has no deployment")
		return
	}
	fetcher, ok := s.deploy.(providerapi.LogFetcher)
	if !ok {
		s.writeError(w, http.StatusNotImplemented, "the configured deploy provider does not support log retrieval")
		return
	}
	logs, err := fetcher.Logs(r.Context(), env.DeployRef)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "fetching logs", "error", err, "environment_id", env.ID)
		s.writeError(w, http.StatusInternalServerError, "fetching logs: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(logs)) // best-effort; nothing actionable if the client disconnected mid-write
}

// lookupEnvironment resolves the {id} URL parameter, writing a 404 response and
// returning ok=false if it doesn't exist.
func (s *Server) lookupEnvironment(w http.ResponseWriter, r *http.Request) (store.Environment, bool) {
	id := chi.URLParam(r, "id")
	env, err := s.store.GetEnvironment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, http.StatusNotFound, "environment not found")
		return store.Environment{}, false
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "looking up environment", "error", err, "environment_id", id)
		s.writeError(w, http.StatusInternalServerError, "looking up environment")
		return store.Environment{}, false
	}
	return env, true
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Error("encoding json response", "error", err)
	}
}

type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	s.writeJSON(w, status, errorResponse{Error: message})
}
