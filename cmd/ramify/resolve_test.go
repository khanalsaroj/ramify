// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveOneNoMatches(t *testing.T) {
	cl := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]environment{})
	})
	_, err := resolveOne(context.Background(), cl, "", "feature-x")
	require.ErrorContains(t, err, "no environment found")
}

func TestResolveOneAmbiguous(t *testing.T) {
	cl := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]environment{{ID: "1"}, {ID: "2"}})
	})
	_, err := resolveOne(context.Background(), cl, "", "feature-x")
	require.ErrorContains(t, err, "multiple environments")
}

func TestResolveOneExactMatch(t *testing.T) {
	cl := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]environment{{ID: "env-1", Branch: "feature-x"}})
	})
	env, err := resolveOne(context.Background(), cl, "", "feature-x")
	require.NoError(t, err)
	require.Equal(t, "env-1", env.ID)
}
