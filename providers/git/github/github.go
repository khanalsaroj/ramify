// SPDX-License-Identifier: Apache-2.0

// Package github implements providerapi.GitProvider against GitHub, verifying
// webhook signatures with HMAC-SHA256 and posting status comments via the GitHub
// REST API.
package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	ghgithub "github.com/google/go-github/v66/github"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// ErrInvalidSignature is returned by ParseWebhook when the HMAC-SHA256 signature
// does not match the payload.
var ErrInvalidSignature = errors.New("github: invalid webhook signature")

// ErrUnhandledEvent is returned by ParseWebhook when the payload is a validly
// signed, well-formed webhook that Ramify has no mapping for (for example, a
// pull_request "labeled" action, or a push of a tag rather than a branch). Callers
// should treat this as a no-op, not a failure.
var ErrUnhandledEvent = errors.New("github: unhandled webhook event")

// Provider implements providerapi.GitProvider against the GitHub REST API.
type Provider struct {
	client        *ghgithub.Client
	webhookSecret []byte
}

var _ providerapi.GitProvider = (*Provider)(nil)

// New constructs a Provider from an already-configured go-github client and the
// webhook HMAC secret.
func New(client *ghgithub.Client, webhookSecret string) *Provider {
	return &Provider{client: client, webhookSecret: []byte(webhookSecret)}
}

// tokenTransport adds a bearer token Authorization header to every request. It
// exists so NewWithToken doesn't need to pull in golang.org/x/oauth2 for what is
// otherwise a one-line header.
type tokenTransport struct {
	token string
	base  http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(cloned)
	if err != nil {
		return nil, fmt.Errorf("github: request %s %s: %w", req.Method, req.URL, err)
	}
	return resp, nil
}

// NewWithToken constructs a Provider authenticated with a GitHub personal access
// token or installation token.
func NewWithToken(token, webhookSecret string) *Provider {
	hc := &http.Client{Transport: &tokenTransport{token: token}}
	return New(ghgithub.NewClient(hc), webhookSecret)
}

// ParseWebhook verifies the HMAC-SHA256 signature of payload against the configured
// webhook secret before any JSON parsing occurs, then maps the payload to a
// normalized providerapi.Event. signature is the raw value of the
// X-Hub-Signature-256 header, with or without the "sha256=" prefix.
func (p *Provider) ParseWebhook(_ context.Context, payload []byte, signature string) (providerapi.Event, error) {
	if !p.verifySignature(payload, signature) {
		return providerapi.Event{}, ErrInvalidSignature
	}
	return parsePayload(payload)
}

func (p *Provider) verifySignature(payload []byte, signature string) bool {
	signature = strings.TrimPrefix(signature, "sha256=")
	want, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, p.webhookSecret)
	mac.Write(payload) // hash.Hash.Write never returns an error
	got := mac.Sum(nil)
	return hmac.Equal(want, got)
}

// webhookPayload is a minimal superset of the GitHub push and pull_request webhook
// bodies, covering only the fields Ramify needs to build a providerapi.Event.
type webhookPayload struct {
	Ref        *string `json:"ref"`
	Deleted    *bool   `json:"deleted"`
	After      *string `json:"after"`
	Action     *string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest *struct {
		Number int `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

func parsePayload(payload []byte) (providerapi.Event, error) {
	var wp webhookPayload
	if err := json.Unmarshal(payload, &wp); err != nil {
		return providerapi.Event{}, fmt.Errorf("github: parsing webhook payload: %w", err)
	}

	if wp.PullRequest != nil {
		return parsePullRequestPayload(wp)
	}
	if wp.Ref != nil {
		return parseRefPayload(wp)
	}
	return providerapi.Event{}, fmt.Errorf("github: %w: payload has neither pull_request nor ref", ErrUnhandledEvent)
}

func parsePullRequestPayload(wp webhookPayload) (providerapi.Event, error) {
	action := ""
	if wp.Action != nil {
		action = *wp.Action
	}

	var kind string
	switch action {
	case "opened", "reopened", "synchronize":
		kind = "pr_synchronized"
	case "closed":
		kind = "pr_closed"
	default:
		return providerapi.Event{}, fmt.Errorf("github: %w: pull_request action %q", ErrUnhandledEvent, action)
	}

	return providerapi.Event{
		Kind:     kind,
		Project:  wp.Repository.FullName,
		Branch:   wp.PullRequest.Head.Ref,
		PRNumber: wp.PullRequest.Number,
		Artifact: wp.PullRequest.Head.SHA,
	}, nil
}

const branchRefPrefix = "refs/heads/"

func parseRefPayload(wp webhookPayload) (providerapi.Event, error) {
	if !strings.HasPrefix(*wp.Ref, branchRefPrefix) {
		return providerapi.Event{}, fmt.Errorf("github: %w: ref %q is not a branch", ErrUnhandledEvent, *wp.Ref)
	}
	branch := strings.TrimPrefix(*wp.Ref, branchRefPrefix)

	if wp.Deleted != nil && *wp.Deleted {
		return providerapi.Event{
			Kind:    "branch_deleted",
			Project: wp.Repository.FullName,
			Branch:  branch,
		}, nil
	}

	artifact := ""
	if wp.After != nil {
		artifact = *wp.After
	}
	return providerapi.Event{
		Kind:     "branch_pushed",
		Project:  wp.Repository.FullName,
		Branch:   branch,
		Artifact: artifact,
	}, nil
}

// CommentOnPR posts body as a new comment on the given pull request. GitHub treats
// pull requests as issues for the comments endpoint.
func (p *Provider) CommentOnPR(ctx context.Context, project string, prNumber int, body string) error {
	owner, repo, err := splitProject(project)
	if err != nil {
		return fmt.Errorf("github: comment on pr: %w", err)
	}
	if _, _, err := p.client.Issues.CreateComment(ctx, owner, repo, prNumber, &ghgithub.IssueComment{Body: &body}); err != nil {
		return fmt.Errorf("github: comment on pr %s#%d: %w", project, prNumber, err)
	}
	return nil
}

func splitProject(project string) (owner, repo string, err error) {
	parts := strings.SplitN(project, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("project %q must be in \"owner/repo\" form", project)
	}
	return parts[0], parts[1], nil
}
