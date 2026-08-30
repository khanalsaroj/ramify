// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Package e2e drives the full Ramify lifecycle — described in the build spec §7
// item 3 / §8 step 15 — against the services brought up by docker-compose.dev.yml:
// CoreDNS, Pebble, a fake SSH deploy target, and a mock GitHub API. It uses
// Ramify's real provider implementations and Reconciler directly, not the compiled
// ramifyd binary; see DECISIONS.md for why (cmd/ramifyd's DNS provider is fixed to
// Cloudflare, which this harness has no real account for).
//
// Run via: docker compose -f test/e2e/docker-compose.dev.yml run --build --rm test-runner
package e2e

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	ghgithub "github.com/google/go-github/v66/github"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/khanalsaroj/ramify/internal/core"
	"github.com/khanalsaroj/ramify/internal/core/domain"
	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/providers/cert/acme"
	"github.com/khanalsaroj/ramify/providers/deploy/compose"
	"github.com/khanalsaroj/ramify/providers/git/github"
	"github.com/khanalsaroj/ramify/providers/notify/prcomment"
	"github.com/khanalsaroj/ramify/test/e2e/dnsfile"
)

// certificateDir is where the Compose provider installs TLS material on the
// deploy host. Production sets this from deploy.certificate_dir; the harness has
// to set it too, or InstallCertificate refuses to write and apply fails.
const certificateDir = "/deploy/certificates"

// sshRun executes one command on the mock deploy target and returns its output,
// so assertions can inspect what the Compose provider actually wrote. The
// provider's own SSH runner is unexported, and staying out of it keeps this test
// checking observable results rather than internals.
func sshRun(t *testing.T, addr, user string, signer ssh.Signer, cmd string) string {
	t.Helper()
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // e2e harness against a throwaway container
		Timeout:         10 * time.Second,
	})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	require.NoError(t, err)
	defer func() { _ = session.Close() }()

	out, err := session.CombinedOutput(cmd)
	require.NoError(t, err, "ssh %q: %s", cmd, string(out))
	return string(out)
}

type env struct {
	zoneFile      string
	zone          string
	pebbleDirURL  string
	pebbleCACert  string
	githubBaseURL string
	sshAddr       string
	sshUser       string
	sshKeyPath    string
	sshReadyPath  string
	webhookSecret string
	artifactRef   string
}

// loadEnv reads the harness configuration injected by docker-compose.dev.yml. The
// e2e build tag alone does not imply the harness is up — `go test -tags=e2e ./...`
// on a developer machine reaches this test with nothing configured — so an empty
// environment skips rather than failing with an opaque "must be set".
func loadEnv(t *testing.T) env {
	t.Helper()
	if os.Getenv("RAMIFY_E2E_ZONE_FILE") == "" {
		t.Skip("e2e harness not configured; run: docker compose -f test/e2e/docker-compose.dev.yml run --build --rm test-runner")
	}
	get := func(name string) string {
		v := os.Getenv(name)
		// Past this point the harness is partly configured, which is a bug in the
		// compose file rather than a reason to skip.
		require.NotEmpty(t, v, "%s must be set (see docker-compose.dev.yml)", name)
		return v
	}
	return env{
		zoneFile:      get("RAMIFY_E2E_ZONE_FILE"),
		zone:          get("RAMIFY_E2E_ZONE"),
		pebbleDirURL:  get("RAMIFY_E2E_PEBBLE_DIR_URL"),
		pebbleCACert:  get("RAMIFY_E2E_PEBBLE_CA_CERT"),
		githubBaseURL: get("RAMIFY_E2E_GITHUB_BASE_URL"),
		sshAddr:       get("RAMIFY_E2E_SSH_ADDR"),
		sshUser:       get("RAMIFY_E2E_SSH_USER"),
		sshKeyPath:    get("RAMIFY_E2E_SSH_KEY_PATH"),
		sshReadyPath:  get("RAMIFY_E2E_SSH_READY_PATH"),
		webhookSecret: get("RAMIFY_E2E_WEBHOOK_SECRET"),
		artifactRef:   get("RAMIFY_E2E_ARTIFACT_REF"),
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = fn(); lastErr == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %v", what, lastErr)
}

func pebbleHTTPClient(t *testing.T, caCertPath string) *http.Client {
	t.Helper()
	pemBytes, err := os.ReadFile(caCertPath)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(pemBytes), "failed to parse pebble CA cert")
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
}

func dnsResolver(addr string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", addr)
		},
	}
}

func sign(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type pullRequestPayload struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func prPayload(action, project, branch string, prNumber int, artifactRef string) []byte {
	p := pullRequestPayload{Action: action, Repository: struct {
		FullName string `json:"full_name"`
	}{FullName: project}}
	p.PullRequest.Number = prNumber
	p.PullRequest.Head.Ref = branch
	p.PullRequest.Head.SHA = artifactRef // repurposed as the artifact tag to deploy; see comment below
	b, err := json.Marshal(p)
	if err != nil {
		panic(err) // test fixture construction; a marshal failure here is a bug in this file, not a runtime condition
	}
	return b
}

// TestFullLifecycle drives the loop required by the build spec §7 item 3: a
// webhook in, verify the deployment/DNS/certificate/comment landed, close the PR,
// verify full teardown.
func TestFullLifecycle(t *testing.T) {
	e := loadEnv(t)
	ctx := context.Background()

	const project = "acme/e2e"
	const branch = "feature-e2e"
	const prNumber = 7

	// --- wait for dependencies ---
	waitFor(t, 60*time.Second, "sshd ready file", func() error {
		_, err := os.Stat(e.sshReadyPath)
		return err
	})

	httpClient := pebbleHTTPClient(t, e.pebbleCACert)
	waitFor(t, 60*time.Second, "pebble directory reachable", func() error {
		resp, err := httpClient.Get(e.pebbleDirURL) //nolint:gosec,noctx // fixed test-harness URL, one-shot readiness probe
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }() // readiness probe only, body discarded
		if resp.StatusCode >= 300 {
			return fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
		return nil
	})

	// --- build the real providers ---
	keyBytes, err := os.ReadFile(e.sshKeyPath)
	require.NoError(t, err)
	signer, err := ssh.ParsePrivateKey(keyBytes)
	require.NoError(t, err)
	deployProvider := compose.New(e.sshAddr, e.sshUser, signer, ssh.InsecureIgnoreHostKey(), "/deploy/docker-compose.yml", "203.0.113.10") //nolint:gosec // e2e test harness only, no production host key trust to establish
	// ramifyd calls this from deploy.certificate_dir; without it InstallCertificate
	// refuses to write and the whole apply fails at the certificate step, which is
	// exactly what this test was missing before.
	deployProvider.SetCertificateDir(certificateDir)

	dnsProvider := dnsfile.New(e.zoneFile, e.zone)

	certProvider, err := acme.New(acme.Config{
		CADirURL:             e.pebbleDirURL,
		Email:                "e2e@example.com",
		Zone:                 e.zone,
		DNSProvider:          dnsProvider,
		SkipPropagationCheck: true,
		HTTPClient:           httpClient,
		StorageDir:           t.TempDir(),
	})
	require.NoError(t, err)

	ghClient := ghgithub.NewClient(nil)
	baseURL, err := ghClient.BaseURL.Parse(e.githubBaseURL)
	require.NoError(t, err)
	ghClient.BaseURL = baseURL
	gitProvider := github.New(ghClient, e.webhookSecret)

	notifyProvider, err := prcomment.New(gitProvider, nil)
	require.NoError(t, err)

	st, err := store.Open(ctx, ":memory:")
	require.NoError(t, err)
	defer func() { _ = st.Close() }()

	reconciler := core.NewReconciler(st, deployProvider, dnsProvider, certProvider, notifyProvider,
		core.NewRealClock(), e.zone, time.Hour, nil)

	// --- fake webhook in: a real signed pull_request "opened" payload, parsed by
	// the real GitProvider exactly as internal/api's webhook handler would ---
	openedPayload := prPayload("opened", project, branch, prNumber, e.artifactRef)
	openedEvent, err := gitProvider.ParseWebhook(ctx, openedPayload, sign(e.webhookSecret, openedPayload))
	require.NoError(t, err)
	require.Equal(t, "pr_synchronized", openedEvent.Kind)

	subdomain := domain.Normalize(branch, 63)
	applyReq := core.ApplyRequestFromEvent(openedEvent, subdomain)

	createdEnv, err := reconciler.Apply(ctx, applyReq)
	require.NoError(t, err, "apply must succeed: deploy over real SSH, DNS-01 cert via pebble+coredns, DNS record via coredns zone file")
	require.Equal(t, store.StatusReady, createdEnv.Status)
	require.NotEmpty(t, createdEnv.DeployRef)

	// --- verify deployment landed on the mock SSH target ---
	status, err := deployProvider.HealthCheck(ctx, createdEnv.DeployRef)
	require.NoError(t, err)
	require.True(t, status.Healthy)
	logs, err := deployProvider.Logs(ctx, createdEnv.DeployRef)
	require.NoError(t, err)
	require.Contains(t, logs, "image_tag="+e.artifactRef)

	// --- verify the issued certificate was installed on the deploy host, the
	// only way TLS material reaches a Compose target ---
	fqdnForCert := subdomain + "." + e.zone
	stem := sha256.Sum256([]byte(fqdnForCert))
	certOut := sshRun(t, e.sshAddr, e.sshUser, signer, "cat "+certificateDir+"/"+hex.EncodeToString(stem[:])+".crt")
	require.Contains(t, certOut, "BEGIN CERTIFICATE", "certificate must be installed on the deploy host")

	// --- verify the DNS record and its TXT ownership tag are actually being
	// served by CoreDNS (not just present in our own bookkeeping) ---
	fqdn := subdomain + "." + e.zone
	resolver := dnsResolver("coredns:53")
	waitFor(t, 30*time.Second, "A record resolvable via coredns", func() error {
		_, err := resolver.LookupHost(ctx, fqdn)
		return err
	})
	txts, err := resolver.LookupTXT(ctx, fqdn)
	require.NoError(t, err)
	require.Contains(t, txts, core.OwnershipTag(project, branch))

	// --- verify a comment was recorded against the mock GitHub server ---
	requireCommentContains(t, e.githubBaseURL, "https://"+fqdn)

	// --- close the PR and verify full teardown ---
	closedPayload := prPayload("closed", project, branch, prNumber, e.artifactRef)
	closedEvent, err := gitProvider.ParseWebhook(ctx, closedPayload, sign(e.webhookSecret, closedPayload))
	require.NoError(t, err)
	require.Equal(t, "pr_closed", closedEvent.Kind)

	envToDestroy, err := st.GetEnvironmentByProjectBranch(ctx, project, branch)
	require.NoError(t, err)
	require.NoError(t, reconciler.Destroy(ctx, envToDestroy))

	final, err := st.GetEnvironment(ctx, envToDestroy.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusDestroyed, final.Status)

	status, err = deployProvider.HealthCheck(ctx, createdEnv.DeployRef)
	require.NoError(t, err)
	require.False(t, status.Healthy, "deployment must be gone after destroy")

	waitFor(t, 30*time.Second, "A record removed from coredns", func() error {
		_, err := resolver.LookupHost(ctx, fqdn)
		if err == nil {
			return fmt.Errorf("A record for %s still resolves", fqdn)
		}
		return nil
	})

	requireCommentContains(t, e.githubBaseURL, "destroyed")

	// The ready notification and the destroyed one must land on a single comment.
	// UpsertPreviewComment exists to avoid pull request spam, and until the mock
	// answered list and edit that contract had no end-to-end coverage.
	require.Equal(t, 1, countComments(t, e.githubBaseURL),
		"lifecycle notifications must upsert one comment, not append several")
}

// countComments reports how many comments the mock GitHub server holds.
func countComments(t *testing.T, githubBaseURL string) int {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, githubBaseURL+"_test/comments", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var got []struct {
		Body string `json:"body"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	return len(got)
}

func requireCommentContains(t *testing.T, githubBaseURL, substring string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, githubBaseURL+"_test/comments", nil)
	require.NoError(t, err)

	var body []byte
	waitFor(t, 15*time.Second, "comment posted to mock github", func() error {
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }() // fully read into body below
		buf := make([]byte, 1<<20)
		n, _ := resp.Body.Read(buf)
		body = buf[:n]
		if !strings.Contains(string(body), substring) {
			return fmt.Errorf("no matching comment yet")
		}
		return nil
	})
	require.Contains(t, string(body), substring)
}
