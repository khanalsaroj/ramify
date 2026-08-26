// SPDX-License-Identifier: Apache-2.0

package bitbucket

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
	"github.com/khanalsaroj/ramify/test/contract"
)

const testSecret = "bitbucket-webhook-secret"

func sign(t *testing.T, payload []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, err := mac.Write(payload)
	require.NoError(t, err)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	body, err := json.Marshal(v)
	require.NoError(t, err)
	return body
}

func pushPayload(t *testing.T, changes []any) []byte {
	t.Helper()
	return marshal(t, map[string]any{
		"repository": map[string]any{"full_name": "acme/api"},
		"push":       map[string]any{"changes": changes},
	})
}

func branchChange(name, hash string) any {
	return map[string]any{
		"new": map[string]any{"type": "branch", "name": name, "target": map[string]any{"hash": hash}},
	}
}

func deleteChange(name string) any {
	return map[string]any{
		"new": nil,
		"old": map[string]any{"type": "branch", "name": name},
	}
}

func prPayload(t *testing.T, state string) []byte {
	t.Helper()
	return marshal(t, map[string]any{
		"repository": map[string]any{"full_name": "acme/api"},
		"pullrequest": map[string]any{
			"id":    7,
			"state": state,
			"source": map[string]any{
				"branch": map[string]any{"name": "feature/login"},
				"commit": map[string]any{"hash": "cafebabe"},
			},
		},
	})
}

func TestProviderContract(t *testing.T) {
	p := New("token", testSecret, "")

	pushed := pushPayload(t, []any{branchChange("feature/login", "deadbeef")})
	deleted := pushPayload(t, []any{deleteChange("feature/login")})
	opened := prPayload(t, "OPEN")
	merged := prPayload(t, "MERGED")

	contract.RunGitProviderContract(t, p, []contract.GitProviderCase{
		{
			Name: "branch pushed", Payload: pushed, Signature: sign(t, pushed),
			WantEvent: providerapi.Event{
				Kind: "branch_pushed", Project: "acme/api", Branch: "feature/login", Artifact: "deadbeef",
			},
		},
		{
			Name: "branch deleted", Payload: deleted, Signature: sign(t, deleted),
			WantEvent: providerapi.Event{
				Kind: "branch_deleted", Project: "acme/api", Branch: "feature/login",
			},
		},
		{
			Name: "pull request opened", Payload: opened, Signature: sign(t, opened),
			WantEvent: providerapi.Event{
				Kind: "pr_synchronized", Project: "acme/api", Branch: "feature/login",
				PRNumber: 7, Artifact: "cafebabe",
			},
		},
		{
			Name: "pull request merged", Payload: merged, Signature: sign(t, merged),
			WantEvent: providerapi.Event{
				Kind: "pr_closed", Project: "acme/api", Branch: "feature/login",
				PRNumber: 7, Artifact: "cafebabe",
			},
		},
	}, "sha256=00")
}

// TestBranchDeleteIsReported guards the teardown path: a push that deletes a branch
// arrives with "new": null, and reporting it as unhandled leaves the preview
// environment running forever.
func TestBranchDeleteIsReported(t *testing.T) {
	p := New("token", testSecret, "")
	payload := pushPayload(t, []any{deleteChange("feature/login")})

	ev, err := p.ParseWebhook(context.Background(), payload, sign(t, payload))
	require.NoError(t, err)
	require.Equal(t, "branch_deleted", ev.Kind)
	require.Equal(t, "feature/login", ev.Branch)
}

// TestPushSkipsTagsToReachBranch verifies a batched push is scanned rather than
// judged by its first entry alone.
func TestPushSkipsTagsToReachBranch(t *testing.T) {
	p := New("token", testSecret, "")
	tagChange := map[string]any{
		"new": map[string]any{"type": "tag", "name": "v1.0.0", "target": map[string]any{"hash": "abc"}},
	}
	payload := pushPayload(t, []any{tagChange, branchChange("feature/login", "deadbeef")})

	ev, err := p.ParseWebhook(context.Background(), payload, sign(t, payload))
	require.NoError(t, err)
	require.Equal(t, "branch_pushed", ev.Kind)
	require.Equal(t, "feature/login", ev.Branch)
}

func TestTagOnlyPushIsUnhandled(t *testing.T) {
	p := New("token", testSecret, "")
	tagChange := map[string]any{
		"new": map[string]any{"type": "tag", "name": "v1.0.0", "target": map[string]any{"hash": "abc"}},
	}
	payload := pushPayload(t, []any{tagChange})

	_, err := p.ParseWebhook(context.Background(), payload, sign(t, payload))
	require.ErrorIs(t, err, providerapi.ErrUnhandledWebhookEvent)
}

func TestEmptySecretRejectsEveryDelivery(t *testing.T) {
	p := New("token", "", "")
	payload := pushPayload(t, []any{branchChange("main", "deadbeef")})

	for _, signature := range []string{"", sign(t, payload)} {
		_, err := p.ParseWebhook(context.Background(), payload, signature)
		require.ErrorIs(t, err, providerapi.ErrInvalidWebhookSignature)
	}
}

func TestSignatureIsVerifiedBeforeParsing(t *testing.T) {
	p := New("token", testSecret, "")
	_, err := p.ParseWebhook(context.Background(), []byte("{ not json"), "sha256=00")
	require.ErrorIs(t, err, providerapi.ErrInvalidWebhookSignature)
}

func TestSignatureAcceptedWithoutPrefix(t *testing.T) {
	p := New("token", testSecret, "")
	payload := pushPayload(t, []any{branchChange("main", "deadbeef")})
	bare := sign(t, payload)[len("sha256="):]

	ev, err := p.ParseWebhook(context.Background(), payload, bare)
	require.NoError(t, err)
	require.Equal(t, "branch_pushed", ev.Kind)
}

func TestCommentOnPRPostsToPullRequest(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &gotBody))
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := New("bb-token", testSecret, srv.URL)
	require.NoError(t, p.CommentOnPR(context.Background(), "acme/api", 7, "preview is ready"))

	require.Equal(t, "/2.0/repositories/acme/api/pullrequests/7/comments", gotPath)
	require.Equal(t, "Bearer bb-token", gotAuth)
	require.Equal(t, "preview is ready", gotBody["content"].(map[string]any)["raw"])
}

func TestCommentOnPRRejectsMalformedProject(t *testing.T) {
	p := New("bb-token", testSecret, "")
	require.Error(t, p.CommentOnPR(context.Background(), "no-slash", 7, "body"))
}

func TestWebhookHeaderNames(t *testing.T) {
	p := New("token", testSecret, "")
	// Bitbucket signs with X-Hub-Signature, not X-Hook-Signature: reading the
	// wrong header means every delivery fails verification.
	require.Equal(t, "X-Hub-Signature", p.SignatureHeader())
	require.Equal(t, "X-Hook-UUID", p.DeliveryHeader())
}
