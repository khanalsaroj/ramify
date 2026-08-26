package bitbucket

import (
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

type Provider struct {
	baseURL, token string
	secret         []byte
	client         *http.Client
}

var _ providerapi.GitProvider = (*Provider)(nil)

func New(token, webhookSecret, baseURL string) *Provider {
	if baseURL == "" {
		baseURL = "https://api.bitbucket.org"
	}
	return &Provider{strings.TrimRight(baseURL, "/"), token, []byte(webhookSecret), http.DefaultClient}
}

func (p *Provider) ParseWebhook(_ context.Context, payload []byte, signature string) (providerapi.Event, error) {
	if !verify(payload, signature, p.secret) {
		return providerapi.Event{}, fmt.Errorf("bitbucket: invalid webhook signature: %w", providerapi.ErrInvalidWebhookSignature)
	}
	var w struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Push struct {
			Changes []struct {
				New *struct {
					Name   string `json:"name"`
					Target struct {
						Hash string `json:"hash"`
					} `json:"target"`
				} `json:"new"`
				Closed bool `json:"closed"`
			} `json:"changes"`
		} `json:"push"`
		PR struct {
			ID     int    `json:"id"`
			State  string `json:"state"`
			Title  string `json:"title"`
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
	if err := json.Unmarshal(payload, &w); err != nil {
		return providerapi.Event{}, fmt.Errorf("bitbucket: parsing webhook payload: %w", err)
	}
	if w.PR.ID != 0 {
		kind := "pr_synchronized"
		if strings.EqualFold(w.PR.State, "merged") || strings.EqualFold(w.PR.State, "declined") || strings.EqualFold(w.PR.State, "superseded") {
			kind = "pr_closed"
		}
		return providerapi.Event{Kind: kind, Project: w.Repository.FullName, Branch: w.PR.Source.Branch.Name, PRNumber: w.PR.ID, Artifact: w.PR.Source.Commit.Hash}, nil
	}
	if len(w.Push.Changes) == 0 || w.Push.Changes[0].New == nil {
		return providerapi.Event{}, fmt.Errorf("bitbucket: %w", providerapi.ErrUnhandledWebhookEvent)
	}
	c := w.Push.Changes[0]
	return providerapi.Event{Kind: "branch_pushed", Project: w.Repository.FullName, Branch: c.New.Name, Artifact: c.New.Target.Hash}, nil
}

func verify(payload []byte, signature string, secret []byte) bool {
	signature = strings.TrimPrefix(signature, "sha256=")
	want, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(payload)
	return hmac.Equal(want, m.Sum(nil))
}
func (p *Provider) CommentOnPR(ctx context.Context, project string, prNumber int, body string) error {
	parts := strings.SplitN(project, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("bitbucket: project %q must be workspace/repository", project)
	}
	endpoint := p.baseURL + "/2.0/repositories/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + fmt.Sprintf("/pullrequests/%d/comments", prNumber)
	b, _ := json.Marshal(map[string]any{"content": map[string]string{"raw": body}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("bitbucket: comment on pr: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("bitbucket: comment on pr: status %s", resp.Status)
	}
	return nil
}
func SignatureHeader() string { return "X-Hook-Signature" }
func DeliveryHeader() string  { return "X-Hook-UUID" }
