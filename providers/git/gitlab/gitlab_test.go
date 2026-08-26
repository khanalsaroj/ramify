// SPDX-License-Identifier: Apache-2.0

package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
	"github.com/khanalsaroj/ramify/test/contract"
)

const testSecret = "gitlab-webhook-secret"

func pushPayload(t *testing.T, ref, after string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"object_kind": "push",
		"ref":         ref,
		"after":       after,
		"project":     map[string]any{"path_with_namespace": "acme/api"},
	})
	require.NoError(t, err)
	return body
}

func mergeRequestPayload(t *testing.T, action, state string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"object_kind": "merge_request",
		"project":     map[string]any{"path_with_namespace": "acme/api"},
		"object_attributes": map[string]any{
			"action":        action,
			"state":         state,
			"iid":           42,
			"source_branch": "feature/login",
			"last_commit":   map[string]any{"id": "cafebabe"},
		},
	})
	require.NoError(t, err)
	return body
}

func TestProviderContract(t *testing.T) {
	p := New("token", testSecret, "")

	contract.RunGitProviderContract(t, p, []contract.GitProviderCase{
		{
			Name:      "branch pushed",
			Payload:   pushPayload(t, "refs/heads/feature/login", "deadbeef"),
			Signature: testSecret,
			WantEvent: providerapi.Event{
				Kind: "branch_pushed", Project: "acme/api", Branch: "feature/login", Artifact: "deadbeef",
			},
		},
		{
			Name:      "branch deleted",
			Payload:   pushPayload(t, "refs/heads/feature/login", deletedSHA),
			Signature: testSecret,
			WantEvent: providerapi.Event{
				Kind: "branch_deleted", Project: "acme/api", Branch: "feature/login",
			},
		},
		{
			Name:      "merge request opened",
			Payload:   mergeRequestPayload(t, "open", "opened"),
			Signature: testSecret,
			WantEvent: providerapi.Event{
				Kind: "pr_synchronized", Project: "acme/api", Branch: "feature/login",
				PRNumber: 42, Artifact: "cafebabe",
			},
		},
		{
			Name:      "merge request merged",
			Payload:   mergeRequestPayload(t, "merge", "merged"),
			Signature: testSecret,
			WantEvent: providerapi.Event{
				Kind: "pr_closed", Project: "acme/api", Branch: "feature/login", PRNumber: 42,
			},
		},
	}, "not-the-secret")
}

// TestUpdateOnMergedRequestClosesEnvironment pins the reason State is checked before
// Action: GitLab sends action "update" for edits to an already-merged merge
// request, and reading that as a synchronization would recreate a torn-down
// environment.
func TestUpdateOnMergedRequestClosesEnvironment(t *testing.T) {
	p := New("token", testSecret, "")
	ev, err := p.ParseWebhook(context.Background(), mergeRequestPayload(t, "update", "merged"), testSecret)
	require.NoError(t, err)
	require.Equal(t, "pr_closed", ev.Kind)
}

func TestEmptySecretRejectsEveryDelivery(t *testing.T) {
	p := New("token", "", "")
	for _, signature := range []string{"", "anything"} {
		_, err := p.ParseWebhook(context.Background(), pushPayload(t, "refs/heads/main", "deadbeef"), signature)
		require.ErrorIs(t, err, providerapi.ErrInvalidWebhookSignature)
	}
}

func TestUnhandledEvents(t *testing.T) {
	p := New("token", testSecret, "")
	ctx := context.Background()

	for name, payload := range map[string][]byte{
		"tag push":            pushPayload(t, "refs/tags/v1.0.0", "deadbeef"),
		"push without after":  pushPayload(t, "refs/heads/main", ""),
		"unapproved":          mergeRequestPayload(t, "approved", "opened"),
		"unknown object kind": []byte(`{"object_kind":"pipeline"}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.ParseWebhook(ctx, payload, testSecret)
			require.ErrorIs(t, err, providerapi.ErrUnhandledWebhookEvent)
		})
	}
}

func TestSignatureIsVerifiedBeforeParsing(t *testing.T) {
	p := New("token", testSecret, "")
	_, err := p.ParseWebhook(context.Background(), []byte("{ not json"), "wrong")
	require.ErrorIs(t, err, providerapi.ErrInvalidWebhookSignature)
}

func TestCommentOnPRPostsNoteToMergeRequest(t *testing.T) {
	var gotPath, gotToken, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		require.NoError(t, r.ParseForm())
		gotBody = r.Form.Get("body")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := New("gl-token", testSecret, srv.URL)
	require.NoError(t, p.CommentOnPR(context.Background(), "acme/api", 42, "preview is ready"))

	// The group/project path must arrive percent-encoded as a single segment.
	require.Equal(t, "/api/v4/projects/acme%2Fapi/merge_requests/42/notes", gotPath)
	require.Equal(t, "gl-token", gotToken)
	require.Equal(t, "preview is ready", gotBody)
}

func TestCommentOnPRSurfacesAPIFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	p := New("gl-token", testSecret, srv.URL)
	require.Error(t, p.CommentOnPR(context.Background(), "acme/api", 42, "preview is ready"))
}

func TestWebhookHeaderNames(t *testing.T) {
	p := New("token", testSecret, "")
	require.Equal(t, "X-Gitlab-Token", p.SignatureHeader())
	require.Equal(t, "X-Gitlab-Event-UUID", p.DeliveryHeader())
}
