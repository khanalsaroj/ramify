package gitlab

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

type Provider struct {
	baseURL, token string
	secret         []byte
	client         *http.Client
}

var _ providerapi.GitProvider = (*Provider)(nil)

func New(token, webhookSecret, baseURL string) *Provider {
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}
	return &Provider{baseURL: strings.TrimRight(baseURL, "/"), token: token, secret: []byte(webhookSecret), client: http.DefaultClient}
}

func (p *Provider) ParseWebhook(_ context.Context, payload []byte, signature string) (providerapi.Event, error) {
	if !hmac.Equal([]byte(signature), p.secret) {
		return providerapi.Event{}, fmt.Errorf("gitlab: invalid webhook signature: %w", providerapi.ErrInvalidWebhookSignature)
	}
	var w struct {
		ObjectKind string `json:"object_kind"`
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Project    struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		Attr struct {
			Action       string `json:"action"`
			State        string `json:"state"`
			IID          int    `json:"iid"`
			SourceBranch string `json:"source_branch"`
			LastCommit   struct {
				ID string `json:"id"`
			} `json:"last_commit"`
		} `json:"object_attributes"`
	}
	if err := json.Unmarshal(payload, &w); err != nil {
		return providerapi.Event{}, fmt.Errorf("gitlab: parsing webhook payload: %w", err)
	}
	if w.ObjectKind == "merge_request" {
		if w.Attr.State == "merged" || w.Attr.State == "closed" || w.Attr.Action == "close" || w.Attr.Action == "merge" {
			return providerapi.Event{Kind: "pr_closed", Project: w.Project.PathWithNamespace, Branch: w.Attr.SourceBranch, PRNumber: w.Attr.IID}, nil
		}
		if w.Attr.Action == "open" || w.Attr.Action == "reopen" || w.Attr.Action == "update" {
			return providerapi.Event{Kind: "pr_synchronized", Project: w.Project.PathWithNamespace, Branch: w.Attr.SourceBranch, PRNumber: w.Attr.IID, Artifact: w.Attr.LastCommit.ID}, nil
		}
		return providerapi.Event{}, fmt.Errorf("gitlab: %w: merge request action %q", providerapi.ErrUnhandledWebhookEvent, w.Attr.Action)
	}
	if w.ObjectKind != "push" || !strings.HasPrefix(w.Ref, "refs/heads/") {
		return providerapi.Event{}, fmt.Errorf("gitlab: %w", providerapi.ErrUnhandledWebhookEvent)
	}
	branch := strings.TrimPrefix(w.Ref, "refs/heads/")
	if strings.Trim(w.After, "0") == "" {
		return providerapi.Event{Kind: "branch_deleted", Project: w.Project.PathWithNamespace, Branch: branch}, nil
	}
	return providerapi.Event{Kind: "branch_pushed", Project: w.Project.PathWithNamespace, Branch: strings.TrimPrefix(w.Ref, "refs/heads/"), Artifact: w.After}, nil
}

func (p *Provider) CommentOnPR(ctx context.Context, project string, prNumber int, body string) error {
	path := "/api/v4/projects/" + url.PathEscape(project) + fmt.Sprintf("/merge_requests/%d/notes", prNumber)
	form := url.Values{"body": {body}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", p.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab: comment on mr: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("gitlab: comment on mr: status %s", resp.Status)
	}
	return nil
}

func SignatureHeader() string { return "X-Gitlab-Token" }
func DeliveryHeader() string  { return "X-Gitlab-Event-UUID" }
