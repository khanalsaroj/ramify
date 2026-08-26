// SPDX-License-Identifier: Apache-2.0

// Package bitbucket implements providerapi.GitProvider against Bitbucket Cloud,
// verifying webhook signatures with HMAC-SHA256 and posting status comments via
// the Bitbucket REST API v2.
//
// Bitbucket signs a webhook only when the hook is configured with a secret. A
// Provider constructed without one rejects every delivery rather than accepting
// unauthenticated payloads.
package bitbucket

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// ErrInvalidSignature is returned by ParseWebhook when the HMAC-SHA256 signature
// does not match the payload. It wraps providerapi.ErrInvalidWebhookSignature.
var ErrInvalidSignature = fmt.Errorf("bitbucket: invalid webhook signature: %w", providerapi.ErrInvalidWebhookSignature)

// ErrUnhandledEvent is returned by ParseWebhook when the payload is a validly
// signed, well-formed webhook that Ramify has no mapping for (a tag push, a
// repo:commit_comment_created event). Callers should treat it as a no-op, not a
// failure. It wraps providerapi.ErrUnhandledWebhookEvent.
var ErrUnhandledEvent = fmt.Errorf("bitbucket: unhandled webhook event: %w", providerapi.ErrUnhandledWebhookEvent)

// Provider implements providerapi.GitProvider against the Bitbucket REST API.
type Provider struct {
	baseURL string
	token   string
	secret  []byte
	client  *http.Client
}

var _ providerapi.GitProvider = (*Provider)(nil)

// New constructs a Provider authenticated with a Bitbucket access token.
// webhookSecret is the secret configured on the repository webhook. An empty
// baseURL defaults to https://api.bitbucket.org.
func New(token, webhookSecret, baseURL string) *Provider {
	if baseURL == "" {
		baseURL = "https://api.bitbucket.org"
	}
	return &Provider{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		secret:  []byte(webhookSecret),
		client:  &http.Client{},
	}
}

// ParseWebhook verifies the HMAC-SHA256 signature of payload against the
// configured webhook secret before any JSON parsing occurs, then maps the payload
// to a normalized providerapi.Event. signature is the raw value of the
// X-Hub-Signature header, with or without the "sha256=" prefix.
func (p *Provider) ParseWebhook(_ context.Context, payload []byte, signature string) (providerapi.Event, error) {
	if !p.verifySignature(payload, signature) {
		return providerapi.Event{}, ErrInvalidSignature
	}
	return parsePayload(payload)
}

// verifySignature checks the HMAC-SHA256 of payload against signature. A Provider
// configured with an empty secret verifies nothing, so it rejects every delivery
// rather than accepting unsigned payloads.
func (p *Provider) verifySignature(payload []byte, signature string) bool {
	if len(p.secret) == 0 {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, p.secret)
	mac.Write(payload) // hash.Hash.Write never returns an error
	return hmac.Equal(want, mac.Sum(nil))
}

// webhookPayload is a minimal superset of the Bitbucket repo:push and
// pullrequest:* webhook bodies, covering only the fields Ramify needs to build an
// Event.
type webhookPayload struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Push struct {
		Changes []struct {
			// New is null when the change deleted the ref; Old then carries the
			// ref that went away.
			New *changeRef `json:"new"`
			Old *changeRef `json:"old"`
		} `json:"changes"`
	} `json:"push"`
	PullRequest *struct {
		ID     int    `json:"id"`
		State  string `json:"state"`
		Source struct {
			Branch struct {
				Name string `json:"name"`
			} `json:"branch"`
			Commit struct {
				Hash string `json:"hash"`
			} `json:"commit"`
		} `json:"source"`
	} `json:"pullrequest"`
}

type changeRef struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Target struct {
		Hash string `json:"hash"`
	} `json:"target"`
}

func parsePayload(payload []byte) (providerapi.Event, error) {
	var wp webhookPayload
	if err := json.Unmarshal(payload, &wp); err != nil {
		return providerapi.Event{}, fmt.Errorf("bitbucket: parsing webhook payload: %w", err)
	}
	if wp.PullRequest != nil && wp.PullRequest.ID != 0 {
		return parsePullRequestPayload(wp)
	}
	return parsePushPayload(wp)
}

func parsePullRequestPayload(wp webhookPayload) (providerapi.Event, error) {
	pr := wp.PullRequest
	kind := "pr_synchronized"
	switch strings.ToUpper(pr.State) {
	case "MERGED", "DECLINED", "SUPERSEDED":
		kind = "pr_closed"
	case "OPEN":
	default:
		return providerapi.Event{}, fmt.Errorf("bitbucket: %w: pull request state %q", ErrUnhandledEvent, pr.State)
	}
	return providerapi.Event{
		Kind:     kind,
		Project:  wp.Repository.FullName,
		Branch:   pr.Source.Branch.Name,
		PRNumber: pr.ID,
		Artifact: pr.Source.Commit.Hash,
	}, nil
}

// parsePushPayload maps the first branch change in a repo:push payload. A push
// may carry several changes — Bitbucket batches a multi-branch push into one
// delivery — so tags and other non-branch refs are skipped rather than treated as
// a malformed payload.
func parsePushPayload(wp webhookPayload) (providerapi.Event, error) {
	for _, change := range wp.Push.Changes {
		if change.New != nil {
			if !isBranch(change.New) {
				continue
			}
			return providerapi.Event{
				Kind:     "branch_pushed",
				Project:  wp.Repository.FullName,
				Branch:   change.New.Name,
				Artifact: change.New.Target.Hash,
			}, nil
		}
		// New is null and Old is set: the push deleted the branch. Without this
		// the preview environment is never torn down.
		if change.Old != nil && isBranch(change.Old) {
			return providerapi.Event{
				Kind:    "branch_deleted",
				Project: wp.Repository.FullName,
				Branch:  change.Old.Name,
			}, nil
		}
	}
	return providerapi.Event{}, fmt.Errorf("bitbucket: %w: push payload has no branch change", ErrUnhandledEvent)
}

// isBranch reports whether ref describes a branch. Bitbucket sets type to
// "branch" or "named_branch" for branches and "tag" for tags; an empty type is
// treated as a branch so a payload shape change does not silently drop pushes.
func isBranch(ref *changeRef) bool {
	return ref.Name != "" && ref.Type != "tag" && ref.Type != "annotated_tag" && ref.Type != "bookmark"
}

// CommentOnPR posts body as a new comment on the given pull request.
func (p *Provider) CommentOnPR(ctx context.Context, project string, prNumber int, body string) error {
	workspace, repo, err := splitProject(project)
	if err != nil {
		return fmt.Errorf("bitbucket: comment on pr: %w", err)
	}
	endpoint := fmt.Sprintf("%s/2.0/repositories/%s/%s/pullrequests/%d/comments",
		p.baseURL, url.PathEscape(workspace), url.PathEscape(repo), prNumber)

	payload, err := json.Marshal(map[string]any{"content": map[string]string{"raw": body}})
	if err != nil {
		return fmt.Errorf("bitbucket: encoding comment for %s#%d: %w", project, prNumber, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("bitbucket: comment on pr %s#%d: %w", project, prNumber, err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("bitbucket: comment on pr %s#%d: %w", project, prNumber, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("bitbucket: comment on pr %s#%d: status %s", project, prNumber, resp.Status)
	}
	return nil
}

func splitProject(project string) (workspace, repo string, err error) {
	parts := strings.SplitN(project, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("project %q must be in \"workspace/repository\" form", project)
	}
	return parts[0], parts[1], nil
}

// SignatureHeader is the request header carrying the HMAC-SHA256 signature that
// ParseWebhook verifies. Bitbucket uses the same header name as GitHub's v1
// scheme but always signs with SHA-256.
func (p *Provider) SignatureHeader() string { return "X-Hub-Signature" }

// DeliveryHeader is the request header carrying Bitbucket's unique delivery ID,
// used to deduplicate redelivered webhooks.
func (p *Provider) DeliveryHeader() string { return "X-Hook-UUID" }
