// SPDX-License-Identifier: Apache-2.0

// Package gitlab implements providerapi.GitProvider against GitLab, verifying
// webhook deliveries with the shared secret token GitLab sends in the
// X-Gitlab-Token header and posting status notes via the GitLab REST API.
//
// Unlike GitHub, GitLab does not HMAC the payload: it echoes the configured
// secret token back verbatim. Verification is therefore a constant-time
// comparison of that token, and a Provider configured with an empty secret
// rejects every delivery rather than accepting every unsigned one.
package gitlab

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// httpClientTimeout bounds every GitLab REST API call. Without it a hung
// connection to GitLab can block a durable event's retry attempt indefinitely
// instead of failing and letting the reconciler's own retry/backoff take over.
const httpClientTimeout = 30 * time.Second

// ErrInvalidSignature is returned by ParseWebhook when the X-Gitlab-Token header
// does not match the configured webhook secret. It wraps
// providerapi.ErrInvalidWebhookSignature.
var ErrInvalidSignature = fmt.Errorf("gitlab: invalid webhook signature: %w", providerapi.ErrInvalidWebhookSignature)

// ErrUnhandledEvent is returned by ParseWebhook when the payload is a validly
// signed, well-formed webhook that Ramify has no mapping for (a tag push, a
// merge_request "approved" action). Callers should treat it as a no-op, not a
// failure. It wraps providerapi.ErrUnhandledWebhookEvent.
var ErrUnhandledEvent = fmt.Errorf("gitlab: unhandled webhook event: %w", providerapi.ErrUnhandledWebhookEvent)

// deletedSHA is the all-zero commit GitLab reports in a push payload's "after"
// field when the push deleted the branch.
const deletedSHA = "0000000000000000000000000000000000000000"

const branchRefPrefix = "refs/heads/"

// Provider implements providerapi.GitProvider against the GitLab REST API. It
// works against gitlab.com and self-managed instances alike; the baseURL passed
// to New selects which.
type Provider struct {
	baseURL string
	token   string
	secret  []byte
	client  *http.Client
}

var _ providerapi.GitProvider = (*Provider)(nil)

// New constructs a Provider authenticated with a GitLab personal, project, or
// group access token. webhookSecret is the secret token configured on the
// project's webhook. An empty baseURL defaults to https://gitlab.com.
func New(token, webhookSecret, baseURL string) *Provider {
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}
	return &Provider{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		secret:  []byte(webhookSecret),
		client:  &http.Client{Timeout: httpClientTimeout},
	}
}

// ParseWebhook verifies signature — the raw X-Gitlab-Token header — against the
// configured webhook secret before any JSON parsing occurs, then maps the payload
// to a normalized providerapi.Event.
func (p *Provider) ParseWebhook(_ context.Context, payload []byte, signature string) (providerapi.Event, error) {
	if !p.verifySignature(signature) {
		return providerapi.Event{}, ErrInvalidSignature
	}
	return parsePayload(payload)
}

// verifySignature compares the delivered token against the configured secret in
// constant time. A Provider configured with an empty secret verifies nothing, so
// it rejects every delivery rather than matching every unsigned request.
func (p *Provider) verifySignature(signature string) bool {
	if len(p.secret) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(signature), p.secret) == 1
}

// webhookPayload is a minimal superset of the GitLab push and merge_request
// webhook bodies, covering only the fields Ramify needs to build an Event.
type webhookPayload struct {
	ObjectKind string `json:"object_kind"`
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Project    struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	ObjectAttributes struct {
		Action       string `json:"action"`
		State        string `json:"state"`
		IID          int    `json:"iid"`
		SourceBranch string `json:"source_branch"`
		LastCommit   struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
	// Labels sits at the top level of the merge request hook, not inside
	// object_attributes, and carries titles rather than names.
	Labels []struct {
		Title string `json:"title"`
	} `json:"labels"`
}

func parsePayload(payload []byte) (providerapi.Event, error) {
	var wp webhookPayload
	if err := json.Unmarshal(payload, &wp); err != nil {
		return providerapi.Event{}, fmt.Errorf("gitlab: parsing webhook payload: %w", err)
	}
	switch wp.ObjectKind {
	case "merge_request":
		return parseMergeRequestPayload(wp)
	case "push":
		return parsePushPayload(wp)
	default:
		return providerapi.Event{}, fmt.Errorf("gitlab: %w: object_kind %q", ErrUnhandledEvent, wp.ObjectKind)
	}
}

// mergeRequestLabels flattens the hook's label objects to their titles. An empty
// result still means "known and none": GitLab sends the array on every merge
// request hook, so its absence is indistinguishable from a request with no labels.
func mergeRequestLabels(wp webhookPayload) []string {
	out := make([]string, 0, len(wp.Labels))
	for _, l := range wp.Labels {
		out = append(out, l.Title)
	}
	return out
}

func parseMergeRequestPayload(wp webhookPayload) (providerapi.Event, error) {
	attrs := wp.ObjectAttributes
	// State is checked before action: GitLab reports action "update" for a merge
	// request that has already been merged or closed, and reading that as a
	// synchronization would resurrect an environment Ramify has just torn down.
	switch {
	case attrs.State == "merged" || attrs.State == "closed",
		attrs.Action == "close" || attrs.Action == "merge":
		return providerapi.Event{
			Kind:     "pr_closed",
			Project:  wp.Project.PathWithNamespace,
			Branch:   attrs.SourceBranch,
			PRNumber: attrs.IID,
		}, nil
	case attrs.Action == "open" || attrs.Action == "reopen" || attrs.Action == "update":
		return providerapi.Event{
			Kind:        "pr_synchronized",
			Project:     wp.Project.PathWithNamespace,
			Branch:      attrs.SourceBranch,
			PRNumber:    attrs.IID,
			Artifact:    attrs.LastCommit.ID,
			Labels:      mergeRequestLabels(wp),
			LabelsKnown: true,
		}, nil
	default:
		return providerapi.Event{}, fmt.Errorf("gitlab: %w: merge_request action %q", ErrUnhandledEvent, attrs.Action)
	}
}

func parsePushPayload(wp webhookPayload) (providerapi.Event, error) {
	if !strings.HasPrefix(wp.Ref, branchRefPrefix) {
		return providerapi.Event{}, fmt.Errorf("gitlab: %w: ref %q is not a branch", ErrUnhandledEvent, wp.Ref)
	}
	branch := strings.TrimPrefix(wp.Ref, branchRefPrefix)

	if wp.After == deletedSHA {
		return providerapi.Event{
			Kind:    "branch_deleted",
			Project: wp.Project.PathWithNamespace,
			Branch:  branch,
		}, nil
	}
	// An absent "after" is not a deletion: treating it as one would destroy a
	// live environment on a malformed payload.
	if wp.After == "" {
		return providerapi.Event{}, fmt.Errorf("gitlab: %w: push payload has no after commit", ErrUnhandledEvent)
	}
	return providerapi.Event{
		Kind:     "branch_pushed",
		Project:  wp.Project.PathWithNamespace,
		Branch:   branch,
		Artifact: wp.After,
	}, nil
}

// CommentOnPR posts body as a new note on the given merge request. GitLab calls
// pull requests merge requests and addresses them by project-scoped IID, which is
// the number Event.PRNumber carries.
func (p *Provider) CommentOnPR(ctx context.Context, project string, prNumber int, body string) error {
	// GitLab addresses a project either by numeric ID or by its URL-encoded
	// path; url.PathEscape encodes the "/" in "group/project" as required.
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/notes", p.baseURL, url.PathEscape(project), prNumber)
	form := url.Values{"body": {body}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("gitlab: comment on mr %s!%d: %w", project, prNumber, err)
	}
	req.Header.Set("PRIVATE-TOKEN", p.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab: comment on mr %s!%d: %w", project, prNumber, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("gitlab: comment on mr %s!%d: status %s", project, prNumber, resp.Status)
	}
	return nil
}

// SignatureHeader is the request header carrying the webhook secret token that
// ParseWebhook verifies.
func (p *Provider) SignatureHeader() string { return "X-Gitlab-Token" }

// DeliveryHeader is the request header carrying GitLab's unique delivery ID, used
// to deduplicate redelivered webhooks.
func (p *Provider) DeliveryHeader() string { return "X-Gitlab-Event-UUID" }
