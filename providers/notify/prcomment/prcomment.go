// SPDX-License-Identifier: Apache-2.0

// Package prcomment implements providerapi.NotifierProvider by posting a
// templated comment on the pull/merge request associated with an environment. It
// depends only on providerapi.GitProvider.CommentOnPR (or the optional
// UpsertPreviewComment upgrade), so it works unchanged against GitHub, GitLab, or
// Bitbucket — whichever GitProvider ramifyd is configured with.
package prcomment

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"text/template"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// defaultTemplates are used for any providerapi.NotifyEvent.Kind not present in the
// config file's notify.comment_templates.
var defaultTemplates = map[string]string{
	"ready":     "Preview environment ready: {{.URL}}",
	"updated":   "Preview environment updated: {{.URL}}",
	"failed":    "Preview environment failed to deploy.\n\n{{.Detail}}",
	"degraded":  "Preview environment update failed and was rolled back to the last known-good revision; it is still serving, but not the requested change.\n\n{{.Detail}}",
	"expiring":  "This preview environment will expire soon.",
	"destroyed": "Preview environment destroyed.",
}

// Provider implements providerapi.NotifierProvider by posting a comment via a
// providerapi.GitProvider, using a Go text/template per NotifyEvent.Kind.
type Provider struct {
	git       providerapi.GitProvider
	templates map[string]*template.Template
}

type previewCommentUpdater interface {
	UpsertPreviewComment(ctx context.Context, project string, prNumber int, body string) error
}

var _ providerapi.NotifierProvider = (*Provider)(nil)

// New constructs a Provider. overrides maps a NotifyEvent.Kind to a custom
// text/template string that replaces the built-in default for that kind; kinds not
// present in overrides keep their default. Every template is parsed eagerly so a
// malformed template in the config file is caught at startup, not at notify time.
func New(git providerapi.GitProvider, overrides map[string]string) (*Provider, error) {
	merged := make(map[string]string, len(defaultTemplates))
	maps.Copy(merged, defaultTemplates)
	maps.Copy(merged, overrides)

	parsed := make(map[string]*template.Template, len(merged))
	for kind, tmplStr := range merged {
		t, err := template.New(kind).Parse(tmplStr)
		if err != nil {
			return nil, fmt.Errorf("prcomment: parsing template for %q: %w", kind, err)
		}
		parsed[kind] = t
	}

	return &Provider{git: git, templates: parsed}, nil
}

// Notify implements providerapi.NotifierProvider. It is a no-op when prNumber is 0
// (the environment isn't associated with an open pull request, e.g. a plain branch
// push), since there is nothing to comment on.
func (p *Provider) Notify(ctx context.Context, project string, prNumber int, ev providerapi.NotifyEvent) error {
	if prNumber == 0 {
		return nil
	}

	tmpl, ok := p.templates[ev.Kind]
	if !ok {
		return fmt.Errorf("prcomment: no template configured for notify kind %q", ev.Kind)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ev); err != nil {
		return fmt.Errorf("prcomment: rendering template for %q: %w", ev.Kind, err)
	}

	if updater, ok := p.git.(previewCommentUpdater); ok {
		if err := updater.UpsertPreviewComment(ctx, project, prNumber, buf.String()); err != nil {
			return fmt.Errorf("prcomment: notify %s#%d: %w", project, prNumber, err)
		}
		return nil
	}
	if err := p.git.CommentOnPR(ctx, project, prNumber, buf.String()); err != nil {
		return fmt.Errorf("prcomment: notify %s#%d: %w", project, prNumber, err)
	}
	return nil
}
