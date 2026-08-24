// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/internal/core"
	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/providers/providerapi"
	"github.com/khanalsaroj/ramify/test/fakes"
)

const testHarnessSecret = "webhook-secret"

// stubGitProvider wraps fakes.GitProvider to also simulate providerapi's sentinel
// errors, since fakes.GitProvider's own ErrBadSignature isn't wired to them.
type stubGitProvider struct {
	*fakes.GitProvider
	nextErr error
}

func (g *stubGitProvider) ParseWebhook(ctx context.Context, payload []byte, signature string) (providerapi.Event, error) {
	if g.nextErr != nil {
		err := g.nextErr
		g.nextErr = nil
		return providerapi.Event{}, err
	}
	return g.GitProvider.ParseWebhook(ctx, payload, signature)
}

type testHarness struct {
	server *Server
	store  store.Store
	deploy *fakes.DeployProvider
	git    *stubGitProvider
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	deploy := fakes.NewDeployProvider()
	dns := fakes.NewDNSProvider()
	cert := fakes.NewCertificateProvider()
	notify := fakes.NewNotifierProvider()
	git := &stubGitProvider{GitProvider: fakes.NewGitProvider()}

	reconciler := core.NewReconciler(st, deploy, dns, cert, notify, core.NewRealClock(), "preview.example.com", 0, nil)
	server := NewServer(st, reconciler, git, deploy, "preview.example.com", nil)

	return &testHarness{server: server, store: st, deploy: deploy, git: git}
}

func (h *testHarness) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	h := newTestHarness(t)
	rec := h.do(t, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestCreateAndGetEnvironment(t *testing.T) {
	h := newTestHarness(t)

	rec := h.do(t, http.MethodPost, "/environments/", createEnvironmentRequest{
		Project: "acme/web", Branch: "feature-x", ArtifactRef: "ref1",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var created store.Environment
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, store.StatusReady, created.Status)

	rec = h.do(t, http.MethodGet, "/environments/"+created.ID+"/", nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestCreateEnvironmentMissingFields(t *testing.T) {
	h := newTestHarness(t)
	rec := h.do(t, http.MethodPost, "/environments/", createEnvironmentRequest{Project: "acme/web"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetEnvironmentNotFound(t *testing.T) {
	h := newTestHarness(t)
	rec := h.do(t, http.MethodGet, "/environments/does-not-exist/", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestListEnvironmentsWithFilter(t *testing.T) {
	h := newTestHarness(t)
	h.do(t, http.MethodPost, "/environments/", createEnvironmentRequest{Project: "acme/web", Branch: "a", ArtifactRef: "r"})
	h.do(t, http.MethodPost, "/environments/", createEnvironmentRequest{Project: "acme/web", Branch: "b", ArtifactRef: "r"})

	rec := h.do(t, http.MethodGet, "/environments/?branch=a", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var envs []store.Environment
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envs))
	require.Len(t, envs, 1)
	require.Equal(t, "a", envs[0].Branch)
}

func TestUpdateEnvironment(t *testing.T) {
	h := newTestHarness(t)
	rec := h.do(t, http.MethodPost, "/environments/", createEnvironmentRequest{Project: "acme/web", Branch: "feature-x", ArtifactRef: "ref1"})
	var created store.Environment
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	rec = h.do(t, http.MethodPut, "/environments/"+created.ID+"/", updateEnvironmentRequest{ArtifactRef: "ref2"})
	require.Equal(t, http.StatusOK, rec.Code)
	var updated store.Environment
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, "ref2", updated.ArtifactRef)
}

func TestDeleteEnvironment(t *testing.T) {
	h := newTestHarness(t)
	rec := h.do(t, http.MethodPost, "/environments/", createEnvironmentRequest{Project: "acme/web", Branch: "feature-x", ArtifactRef: "ref1"})
	var created store.Environment
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	rec = h.do(t, http.MethodDelete, "/environments/"+created.ID+"/", nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	got, err := h.store.GetEnvironment(context.Background(), created.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusDestroyed, got.Status)
}

func TestSleepAndWakeEnvironment(t *testing.T) {
	h := newTestHarness(t)
	rec := h.do(t, http.MethodPost, "/environments/", createEnvironmentRequest{Project: "acme/web", Branch: "feature-x", ArtifactRef: "ref1"})
	var created store.Environment
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	rec = h.do(t, http.MethodPost, "/environments/"+created.ID+"/sleep", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var slept store.Environment
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &slept))
	require.Equal(t, store.StatusSleeping, slept.Status)

	rec = h.do(t, http.MethodPost, "/environments/"+created.ID+"/wake", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var woken store.Environment
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &woken))
	require.Equal(t, store.StatusReady, woken.Status)
}

func TestLogsNotSupportedByDeployProvider(t *testing.T) {
	h := newTestHarness(t)
	rec := h.do(t, http.MethodPost, "/environments/", createEnvironmentRequest{Project: "acme/web", Branch: "feature-x", ArtifactRef: "ref1"})
	var created store.Environment
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	rec = h.do(t, http.MethodGet, "/environments/"+created.ID+"/logs", nil)
	require.Equal(t, http.StatusNotImplemented, rec.Code)
}

func sign(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookBadSignatureReturns401(t *testing.T) {
	h := newTestHarness(t)
	h.git.nextErr = providerapi.ErrInvalidWebhookSignature

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestWebhookUnhandledEventReturns200(t *testing.T) {
	h := newTestHarness(t)
	h.git.nextErr = providerapi.ErrUnhandledWebhookEvent

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestWebhookBranchPushedTriggersApply(t *testing.T) {
	h := newTestHarness(t)
	payload := []byte(`{"ref":"refs/heads/feature-x"}`)
	h.git.QueueEvent(sign(testHarnessSecret, payload), providerapi.Event{
		Kind: "branch_pushed", Project: "acme/web", Branch: "feature-x", Artifact: "sha123",
	})

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-Hub-Signature-256", sign(testHarnessSecret, payload))
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	require.Eventually(t, func() bool {
		env, err := h.store.GetEnvironmentByProjectBranch(context.Background(), "acme/web", "feature-x")
		return err == nil && env.Status == store.StatusReady
	}, 2*time.Second, 10*time.Millisecond, "webhook-triggered apply must complete asynchronously")
}

func TestWebhookPRClosedTriggersDestroyWhenEnvironmentExists(t *testing.T) {
	h := newTestHarness(t)
	rec := h.do(t, http.MethodPost, "/environments/", createEnvironmentRequest{Project: "acme/web", Branch: "feature-x", ArtifactRef: "ref1"})
	var created store.Environment
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	payload := []byte(`{"action":"closed"}`)
	h.git.QueueEvent(sign(testHarnessSecret, payload), providerapi.Event{
		Kind: "pr_closed", Project: "acme/web", Branch: "feature-x", PRNumber: 1,
	})

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-Hub-Signature-256", sign(testHarnessSecret, payload))
	respRec := httptest.NewRecorder()
	h.server.ServeHTTP(respRec, req)
	require.Equal(t, http.StatusAccepted, respRec.Code)

	require.Eventually(t, func() bool {
		env, err := h.store.GetEnvironment(context.Background(), created.ID)
		return err == nil && env.Status == store.StatusDestroyed
	}, 2*time.Second, 10*time.Millisecond)
}

func TestWebhookPRClosedNoExistingEnvironmentIsNoop(t *testing.T) {
	h := newTestHarness(t)
	payload := []byte(`{"action":"closed"}`)
	h.git.QueueEvent(sign(testHarnessSecret, payload), providerapi.Event{
		Kind: "pr_closed", Project: "acme/web", Branch: "never-existed", PRNumber: 1,
	})

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-Hub-Signature-256", sign(testHarnessSecret, payload))
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
}
