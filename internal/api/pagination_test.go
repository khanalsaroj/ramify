// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/internal/store"
)

func seedEnvironments(t *testing.T, h *testHarness, n int) {
	t.Helper()
	for i := range n {
		branch := fmt.Sprintf("b%02d", i)
		_, err := h.store.CreateEnvironment(context.Background(), store.Environment{
			Project: "acme/web", Branch: branch, Subdomain: branch,
			ArtifactRef: "r", Status: store.StatusPending,
		})
		require.NoError(t, err)
	}
}

func decodeEnvironments(t *testing.T, body []byte) []store.Environment {
	t.Helper()
	var envs []store.Environment
	require.NoError(t, json.Unmarshal(body, &envs))
	return envs
}

func TestListEnvironmentsPaginationHeader(t *testing.T) {
	h := newTestHarness(t)
	seedEnvironments(t, h, 10)

	rec := h.do(t, http.MethodGet, "/environments/?limit=4", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, decodeEnvironments(t, rec.Body.Bytes()), 4, "limit must be honored exactly")
	require.Equal(t, "4", rec.Header().Get(nextOffsetHeader), "a further page must be advertised")

	rec = h.do(t, http.MethodGet, "/environments/?limit=4&offset=8", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, decodeEnvironments(t, rec.Body.Bytes()), 2)
	require.Empty(t, rec.Header().Get(nextOffsetHeader), "the last page must not advertise a next offset")
}

// Walking the advertised offsets must visit every row exactly once.
func TestListEnvironmentsPagesCoverEveryRow(t *testing.T) {
	h := newTestHarness(t)
	seedEnvironments(t, h, 10)

	seen := map[string]bool{}
	offset := 0
	for range 10 {
		rec := h.do(t, http.MethodGet, fmt.Sprintf("/environments/?limit=3&offset=%d", offset), nil)
		require.Equal(t, http.StatusOK, rec.Code)
		for _, env := range decodeEnvironments(t, rec.Body.Bytes()) {
			require.False(t, seen[env.ID], "environment %s served twice", env.ID)
			seen[env.ID] = true
		}
		next := rec.Header().Get(nextOffsetHeader)
		if next == "" {
			break
		}
		var err error
		offset, err = strconv.Atoi(next)
		require.NoError(t, err)
	}
	require.Len(t, seen, 10)
}

// The response body stays a bare JSON array so existing clients keep parsing it.
func TestListEnvironmentsBodyRemainsArray(t *testing.T) {
	h := newTestHarness(t)

	rec := h.do(t, http.MethodGet, "/environments/", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "[]", strings.TrimSpace(rec.Body.String()), "an empty list must encode as [] not null")
}

func TestListEnvironmentsFilterStillApplies(t *testing.T) {
	h := newTestHarness(t)
	seedEnvironments(t, h, 5)

	rec := h.do(t, http.MethodGet, "/environments/?project=acme/web&branch=b03", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	envs := decodeEnvironments(t, rec.Body.Bytes())
	require.Len(t, envs, 1)
	require.Equal(t, "b03", envs[0].Branch)

	rec = h.do(t, http.MethodGet, "/environments/?project=nope/none", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, decodeEnvironments(t, rec.Body.Bytes()))
}

func TestListEnvironmentsRejectsBadPageParams(t *testing.T) {
	h := newTestHarness(t)

	for _, query := range []string{"?limit=abc", "?limit=-1", "?offset=-2", "?offset=x"} {
		rec := h.do(t, http.MethodGet, "/environments/"+query, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, "query %q must be rejected", query)
	}
}

// A caller must not be able to defeat pagination by asking for an enormous page.
func TestListEnvironmentsClampsOversizedLimit(t *testing.T) {
	h := newTestHarness(t)
	seedEnvironments(t, h, store.MaxListLimit+10)

	rec := h.do(t, http.MethodGet, fmt.Sprintf("/environments/?limit=%d", store.MaxListLimit*100), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, decodeEnvironments(t, rec.Body.Bytes()), store.MaxListLimit)
	require.NotEmpty(t, rec.Header().Get(nextOffsetHeader))
}
