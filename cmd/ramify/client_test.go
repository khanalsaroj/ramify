// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *apiClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &apiClient{httpClient: srv.Client(), baseURL: srv.URL}
}

func TestListEnvironments(t *testing.T) {
	cl := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/environments/", r.URL.Path)
		require.Equal(t, "acme/web", r.URL.Query().Get("project"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]environment{{ID: "env-1", Project: "acme/web", Branch: "feature-x"}})
	})

	envs, err := cl.listEnvironments(context.Background(), "acme/web", "")
	require.NoError(t, err)
	require.Len(t, envs, 1)
	require.Equal(t, "env-1", envs[0].ID)
}

func TestGetEnvironmentNotFoundReturnsAPIError(t *testing.T) {
	cl := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"environment not found"}`))
	})

	_, err := cl.getEnvironment(context.Background(), "missing")
	require.Error(t, err)
	var apiErr *apiError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusNotFound, apiErr.Status)
}

func TestDeleteEnvironment(t *testing.T) {
	var gotMethod, gotPath string
	cl := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	require.NoError(t, cl.deleteEnvironment(context.Background(), "env-1"))
	require.Equal(t, http.MethodDelete, gotMethod)
	require.Equal(t, "/environments/env-1/", gotPath)
}

func TestLogsReturnsRawBody(t *testing.T) {
	cl := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/environments/env-1/logs", r.URL.Path)
		_, _ = w.Write([]byte("line one\nline two\n"))
	})

	logs, err := cl.logs(context.Background(), "env-1")
	require.NoError(t, err)
	require.Equal(t, "line one\nline two\n", logs)
}

func TestBearerTokenIsSent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cl := &apiClient{httpClient: srv.Client(), baseURL: srv.URL, token: "secret-token"}
	require.NoError(t, cl.healthz(context.Background()))
	require.Equal(t, "Bearer secret-token", gotAuth)
}
