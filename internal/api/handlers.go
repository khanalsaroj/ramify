// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.ListEnvironments(r.Context()); err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleWebhook verifies and parses the inbound GitHub webhook, then triggers
// reconciliation asynchronously: Apply/Destroy already persist a request event to
// the store before making any provider call, so if the process dies mid-request the
// next startup's ReplayUnprocessedEvents recovers it. Responding before that work
// completes keeps webhook delivery well under GitHub's timeout.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "reading request body")
		return
	}

	ev, err := s.git.ParseWebhook(r.Context(), body, r.Header.Get("X-Hub-Signature-256"))
	switch {
	case errors.Is(err, providerapi.ErrInvalidWebhookSignature):
		s.writeError(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	case errors.Is(err, providerapi.ErrUnhandledWebhookEvent):
		w.WriteHeader(http.StatusOK)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "webhook: parse failed", "error", err)
		s.writeError(w, http.StatusBadRequest, "invalid webhook payload")
		return
	}

	go s.processEvent(ev) //nolint:gosec // deliberately detached from the request context; see processEvent's doc comment
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) processEvent(ev providerapi.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), webhookProcessTimeout)
	defer cancel()

	switch ev.Kind {
	case "branch_pushed", "pr_synchronized":
		subdomain := domain.Normalize(ev.Branch, s.subdomainMax)
		req := core.ApplyRequestFromEvent(ev, subdomain)
		if _, err := s.reconciler.Apply(ctx, req); err != nil {
			s.logger.ErrorContext(ctx, "webhook: apply failed", "error", err, "project", ev.Project, "branch", ev.Branch)
		}
	case "pr_closed", "branch_deleted":
		env, err := s.store.GetEnvironmentByProjectBranch(ctx, ev.Project, ev.Branch)
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		if err != nil {
			s.logger.ErrorContext(ctx, "webhook: environment lookup failed", "error", err, "project", ev.Project, "branch", ev.Branch)
			return
		}
		if err := s.reconciler.Destroy(ctx, env); err != nil {
			s.logger.ErrorContext(ctx, "webhook: destroy failed", "error", err, "environment_id", env.ID)
		}
	default:
		s.logger.WarnContext(ctx, "webhook: unrecognized event kind", "kind", ev.Kind)
	}
}

func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	envs, err := s.store.ListEnvironments(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "listing environments", "error", err)
		s.writeError(w, http.StatusInternalServerError, "listing environments")
		return
	}

	project := r.URL.Query().Get("project")
	branch := r.URL.Query().Get("branch")
	if project != "" || branch != "" {
		filtered := make([]store.Environment, 0, len(envs))
		for _, e := range envs {
			if project != "" && e.Project != project {
				continue
			}
			if branch != "" && e.Branch != branch {
				continue
			}
			filtered = append(filtered, e)
		}
		envs = filtered
	}

	s.writeJSON(w, http.StatusOK, envs)
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
	var body createEnvironmentRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Project == "" || body.Branch == "" || body.ArtifactRef == "" {
		s.writeError(w, http.StatusBadRequest, "project, branch, and artifact_ref are required")
		return
	}
	if body.Subdomain == "" {
		body.Subdomain = domain.Normalize(body.Branch, s.subdomainMax)
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
	fetcher, ok := s.deploy.(LogFetcher)
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
