// SPDX-License-Identifier: Apache-2.0

package githubcomment

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
	"github.com/khanalsaroj/ramify/test/fakes"
)

func TestNotifyRendersDefaultTemplateAndComments(t *testing.T) {
	git := fakes.NewGitProvider()
	p, err := New(git, nil)
	require.NoError(t, err)

	err = p.Notify(context.Background(), "acme/web", 42, providerapi.NotifyEvent{Kind: "ready", URL: "https://feature-x.preview.example.com"})
	require.NoError(t, err)

	require.Len(t, git.Comments, 1)
	require.Equal(t, "acme/web", git.Comments[0].Project)
	require.Equal(t, 42, git.Comments[0].PRNumber)
	require.Contains(t, git.Comments[0].Body, "https://feature-x.preview.example.com")
}

func TestNotifyWithoutPRNumberIsNoop(t *testing.T) {
	git := fakes.NewGitProvider()
	p, err := New(git, nil)
	require.NoError(t, err)

	require.NoError(t, p.Notify(context.Background(), "acme/web", 0, providerapi.NotifyEvent{Kind: "ready"}))
	require.Empty(t, git.Comments)
}

func TestNotifyUsesConfigOverrideTemplate(t *testing.T) {
	git := fakes.NewGitProvider()
	p, err := New(git, map[string]string{"ready": "custom: {{.URL}} for {{.Detail}}"})
	require.NoError(t, err)

	err = p.Notify(context.Background(), "acme/web", 1, providerapi.NotifyEvent{Kind: "ready", URL: "https://x.example.com", Detail: "extra info"})
	require.NoError(t, err)
	require.Equal(t, "custom: https://x.example.com for extra info", git.Comments[0].Body)
}

func TestNotifyUnknownKind(t *testing.T) {
	git := fakes.NewGitProvider()
	p, err := New(git, nil)
	require.NoError(t, err)

	err = p.Notify(context.Background(), "acme/web", 1, providerapi.NotifyEvent{Kind: "not-a-real-kind"})
	require.Error(t, err)
}

func TestNewRejectsMalformedTemplate(t *testing.T) {
	_, err := New(fakes.NewGitProvider(), map[string]string{"ready": "{{.Unclosed"})
	require.Error(t, err)
}

func TestAllNotifyEventKindsHaveDefaultTemplates(t *testing.T) {
	git := fakes.NewGitProvider()
	p, err := New(git, nil)
	require.NoError(t, err)

	for _, kind := range []string{"ready", "updated", "failed", "expiring", "destroyed"} {
		err := p.Notify(context.Background(), "acme/web", 1, providerapi.NotifyEvent{Kind: kind})
		require.NoError(t, err, "kind %q must have a default template", kind)
	}
}

func TestCommentOnPRErrorPropagates(t *testing.T) {
	git := fakes.NewGitProvider()
	git.CommentErr = context.DeadlineExceeded
	p, err := New(git, nil)
	require.NoError(t, err)

	err = p.Notify(context.Background(), "acme/web", 1, providerapi.NotifyEvent{Kind: "ready"})
	require.Error(t, err)
}
