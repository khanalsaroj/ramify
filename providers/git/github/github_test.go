// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	ghgithub "github.com/google/go-github/v66/github"
	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
	"github.com/khanalsaroj/ramify/test/contract"
)

const testSecret = "test-webhook-secret"

func sign(t *testing.T, secret string, payload []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

var (
	pushPayload = []byte(`{
		"ref": "refs/heads/feature/login",
		"after": "abc123def456",
		"deleted": false,
		"repository": {"full_name": "acme/web"}
	}`)

	branchDeletedPayload = []byte(`{
		"ref": "refs/heads/feature/login",
		"deleted": true,
		"repository": {"full_name": "acme/web"}
	}`)

	prOpenedPayload = []byte(`{
		"action": "opened",
		"pull_request": {"number": 42, "head": {"ref": "feature/login", "sha": "abc123def456"}},
		"repository": {"full_name": "acme/web"}
	}`)

	prSynchronizePayload = []byte(`{
		"action": "synchronize",
		"pull_request": {"number": 42, "head": {"ref": "feature/login", "sha": "def456abc123"}},
		"repository": {"full_name": "acme/web"}
	}`)

	prClosedPayload = []byte(`{
		"action": "closed",
		"pull_request": {"number": 42, "head": {"ref": "feature/login", "sha": "def456abc123"}},
		"repository": {"full_name": "acme/web"}
	}`)
)

func TestGitProviderContract(t *testing.T) {
	p := NewWithToken("", testSecret)

	cases := []contract.GitProviderCase{
		{
			Name:      "branch pushed without PR",
			Payload:   pushPayload,
			Signature: sign(t, testSecret, pushPayload),
			WantEvent: providerapi.Event{Kind: "branch_pushed", Project: "acme/web", Branch: "feature/login", Artifact: "abc123def456"},
		},
		{
			Name:      "branch deleted",
			Payload:   branchDeletedPayload,
			Signature: sign(t, testSecret, branchDeletedPayload),
			WantEvent: providerapi.Event{Kind: "branch_deleted", Project: "acme/web", Branch: "feature/login"},
		},
		{
			Name:      "pull request opened",
			Payload:   prOpenedPayload,
			Signature: sign(t, testSecret, prOpenedPayload),
			WantEvent: providerapi.Event{Kind: "pr_synchronized", Project: "acme/web", Branch: "feature/login", PRNumber: 42, Artifact: "abc123def456", Labels: []string{}, LabelsKnown: true},
		},
		{
			Name:      "pull request synchronize",
			Payload:   prSynchronizePayload,
			Signature: sign(t, testSecret, prSynchronizePayload),
			WantEvent: providerapi.Event{Kind: "pr_synchronized", Project: "acme/web", Branch: "feature/login", PRNumber: 42, Artifact: "def456abc123", Labels: []string{}, LabelsKnown: true},
		},
		{
			Name:      "pull request closed",
			Payload:   prClosedPayload,
			Signature: sign(t, testSecret, prClosedPayload),
			WantEvent: providerapi.Event{Kind: "pr_closed", Project: "acme/web", Branch: "feature/login", PRNumber: 42, Artifact: "def456abc123", Labels: []string{}, LabelsKnown: true},
		},
	}

	contract.RunGitProviderContract(t, p, cases, "sha256="+hex.EncodeToString([]byte("not-a-real-signature-000000000")))
}

func TestParseWebhookBadSignatureRejectedBeforeParsing(t *testing.T) {
	p := NewWithToken("", testSecret)

	malformedButUnsigned := []byte(`{not even valid json`)
	_, err := p.ParseWebhook(context.Background(), malformedButUnsigned, "sha256=0000")
	require.ErrorIs(t, err, ErrInvalidSignature, "a bad signature must be rejected before the payload is ever parsed as JSON")
}

func TestParseWebhookUnhandledPullRequestAction(t *testing.T) {
	p := NewWithToken("", testSecret)
	payload := []byte(`{
		"action": "labeled",
		"pull_request": {"number": 42, "head": {"ref": "feature/login", "sha": "abc123"}},
		"repository": {"full_name": "acme/web"}
	}`)
	_, err := p.ParseWebhook(context.Background(), payload, sign(t, testSecret, payload))
	require.ErrorIs(t, err, ErrUnhandledEvent)
}

func TestParseWebhookUnhandledTagPush(t *testing.T) {
	p := NewWithToken("", testSecret)
	payload := []byte(`{"ref": "refs/tags/v1.0.0", "repository": {"full_name": "acme/web"}}`)
	_, err := p.ParseWebhook(context.Background(), payload, sign(t, testSecret, payload))
	require.ErrorIs(t, err, ErrUnhandledEvent)
}

func TestParseWebhookSignatureAcceptsRawHexWithoutPrefix(t *testing.T) {
	p := NewWithToken("", testSecret)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(pushPayload)
	rawSig := hex.EncodeToString(mac.Sum(nil))

	got, err := p.ParseWebhook(context.Background(), pushPayload, rawSig)
	require.NoError(t, err)
	require.Equal(t, "branch_pushed", got.Kind)
}

func TestCommentOnPR(t *testing.T) {
	var capturedPath, capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		buf, _ := io.ReadAll(r.Body)
		capturedBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer srv.Close()

	client := ghgithub.NewClient(srv.Client())
	baseURL, err := ghgithub.NewClient(nil).BaseURL.Parse(srv.URL + "/")
	require.NoError(t, err)
	client.BaseURL = baseURL

	p := New(client, testSecret)
	require.NoError(t, p.CommentOnPR(context.Background(), "acme/web", 42, "Preview deployed."))
	require.Contains(t, capturedPath, "/repos/acme/web/issues/42/comments")
	require.Contains(t, capturedBody, "Preview deployed.")
}

func TestCommentOnPRInvalidProject(t *testing.T) {
	p := NewWithToken("", testSecret)
	err := p.CommentOnPR(context.Background(), "not-a-valid-project", 1, "hi")
	require.Error(t, err)
}

// GitHub always sends the labels array, so an empty one genuinely means "no
// labels" and must be reported as known. Reporting it as unknown would make a
// required-labels policy admit every unlabeled pull request.
func TestGitHubReportsLabelsKnownEvenWhenEmpty(t *testing.T) {
	p := NewWithToken("", testSecret)

	ev, err := p.ParseWebhook(context.Background(), prOpenedPayload, sign(t, testSecret, prOpenedPayload))
	require.NoError(t, err)
	require.True(t, ev.LabelsKnown)
	require.Empty(t, ev.Labels)
}

// A push has no pull request, so labels are unknown rather than empty.
func TestGitHubPushReportsLabelsUnknown(t *testing.T) {
	p := NewWithToken("", testSecret)

	ev, err := p.ParseWebhook(context.Background(), pushPayload, sign(t, testSecret, pushPayload))
	require.NoError(t, err)
	require.False(t, ev.LabelsKnown)
}
