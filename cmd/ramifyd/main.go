// SPDX-License-Identifier: Apache-2.0

// Command ramifyd is the Ramify control plane daemon: it loads ramify.yaml,
// constructs the real providers, and runs the control API, reconciler event
// replay, and TTL reaper until told to stop.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/khanalsaroj/ramify/internal/api"
	"github.com/khanalsaroj/ramify/internal/config"
	"github.com/khanalsaroj/ramify/internal/core"
	"github.com/khanalsaroj/ramify/internal/metrics"
	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/providers/cert/acme"
	"github.com/khanalsaroj/ramify/providers/deploy/compose"
	"github.com/khanalsaroj/ramify/providers/deploy/kubernetes"
	"github.com/khanalsaroj/ramify/providers/dns/cloudflare"
	"github.com/khanalsaroj/ramify/providers/dns/digitalocean"
	"github.com/khanalsaroj/ramify/providers/dns/googlecloud"
	"github.com/khanalsaroj/ramify/providers/dns/route53"
	"github.com/khanalsaroj/ramify/providers/git/bitbucket"
	"github.com/khanalsaroj/ramify/providers/git/github"
	"github.com/khanalsaroj/ramify/providers/git/gitlab"
	"github.com/khanalsaroj/ramify/providers/notify/githubcomment"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// version is the ramifyd build version, overridden at build time via -ldflags.
var version = "dev"

const reaperSweepTimeout = 5 * time.Minute

func main() {
	configPath := flag.String("config", "/etc/ramify/ramify.yaml", "path to ramify.yaml")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("ramifyd " + version)
		return
	}

	logger := newLogger()
	if err := run(*configPath, logger); err != nil {
		logger.Error("ramifyd exited with error", "error", err)
		os.Exit(1)
	}
}

func newLogger() *slog.Logger {
	format := os.Getenv("RAMIFY_LOG_FORMAT")
	isTTY := false
	if stat, err := os.Stdout.Stat(); err == nil {
		isTTY = stat.Mode()&os.ModeCharDevice != 0
	}

	if format == "text" || (format == "" && isTTY) {
		return slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	logger.Info("config loaded", "config", cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	st, err := store.Open(ctx, cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Error("closing store", "error", err)
		}
	}()

	gitProvider, err := newGitProvider(cfg)
	if err != nil {
		return err
	}

	dnsProvider, err := newDNSProvider(ctx, cfg)
	if err != nil {
		return fmt.Errorf("constructing dns provider: %w", err)
	}

	deployProvider, err := newDeployProvider(cfg, logger)
	if err != nil {
		return fmt.Errorf("constructing deploy provider: %w", err)
	}
	if setter, ok := deployProvider.(interface{ SetCertificateDir(string) }); ok {
		setter.SetCertificateDir(cfg.Deploy.CertificateDir)
	}

	certProvider, err := acme.New(acme.Config{
		CADirURL:    cfg.ACME.CADirURL,
		Email:       cfg.ACME.Email,
		Zone:        cfg.DNS.Zone,
		DNSProvider: dnsProvider,
		StorageDir:  acmeStorageDir(cfg),
	})
	if err != nil {
		return fmt.Errorf("constructing acme provider: %w", err)
	}

	notifyProvider, err := githubcomment.New(gitProvider, cfg.Notify.CommentTemplates)
	if err != nil {
		return fmt.Errorf("constructing notifier: %w", err)
	}

	reconciler := core.NewReconciler(st, deployProvider, dnsProvider, certProvider, notifyProvider,
		core.NewRealClock(), cfg.BaseDomain, cfg.Reaper.DefaultTTL, logger)
	metricSet := &metrics.Metrics{}
	reaper := core.NewReaper(st, reconciler, core.NewRealClock(), logger, metricSet)
	server := api.NewServer(st, reconciler, gitProvider, deployProvider, cfg.BaseDomain, logger, metricSet)

	logger.Info("replaying unprocessed events")
	if err := reconciler.ReplayUnprocessedEvents(ctx); err != nil {
		return fmt.Errorf("replaying unprocessed events: %w", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	wg.Go(func() {
		logger.Info("control api starting", "socket", cfg.Server.SocketPath, "tcp_addr", cfg.Server.TCPAddr)
		if err := server.Serve(ctx, cfg.Server.SocketPath, cfg.Server.TCPAddr, cfg.Server.TCPToken); err != nil {
			errCh <- fmt.Errorf("control api: %w", err)
		}
	})

	wg.Go(func() {
		runReaperLoop(ctx, reaper, cfg.Reaper.Interval, logger)
	})

	wg.Go(func() {
		runEventLoop(ctx, st, reconciler, logger, metricSet, cfg.Reaper.EventRetention)
	})

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func newDNSProvider(ctx context.Context, cfg *config.Config) (providerapi.DNSProvider, error) {
	switch cfg.DNS.Provider {
	case "cloudflare":
		return cloudflare.New(cfg.DNS.APIToken)
	case "route53":
		return route53.New(ctx, cfg.DNS.ZoneID)
	case "googlecloud":
		return googlecloud.New(ctx, cfg.DNS.Project, cfg.DNS.ZoneID)
	case "digitalocean":
		return digitalocean.New(cfg.DNS.APIToken, cfg.DNS.Zone), nil
	default:
		return nil, fmt.Errorf("unsupported dns provider %q", cfg.DNS.Provider)
	}
}

func newGitProvider(cfg *config.Config) (providerapi.GitProvider, error) {
	switch cfg.Git.Provider {
	case "github":
		return github.NewWithToken(cfg.Git.Token, cfg.Git.WebhookSecret), nil
	case "gitlab":
		return gitlab.New(cfg.Git.Token, cfg.Git.WebhookSecret, cfg.Git.BaseURL), nil
	case "bitbucket":
		return bitbucket.New(cfg.Git.Token, cfg.Git.WebhookSecret, cfg.Git.BaseURL), nil
	default:
		return nil, fmt.Errorf("unsupported git provider %q", cfg.Git.Provider)
	}
}

func runEventLoop(ctx context.Context, st store.Store, reconciler *core.Reconciler, logger *slog.Logger, metricSet *metrics.Metrics, eventRetention time.Duration) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			events, err := st.ListDueEvents(ctx, now, 100)
			if err != nil {
				logger.Error("event worker: listing due events failed", "error", err)
				continue
			}
			metricSet.InboxPending.Store(int64(len(events)))
			if dead, err := st.CountDeadLetteredEvents(ctx); err != nil {
				logger.Error("event worker: counting dead-lettered events failed", "error", err)
			} else {
				metricSet.DeadLettered.Store(int64(dead))
			}
			if err := reconciler.ReplayEvents(ctx, events); err != nil {
				logger.Error("event worker: replay failed", "error", err)
			}
			if eventRetention > 0 && (lastPrune.IsZero() || now.Sub(lastPrune) >= time.Hour) {
				if _, err := st.PruneProcessedEvents(ctx, now.Add(-eventRetention), 5000); err != nil {
					logger.Error("event worker: pruning processed events failed", "error", err)
				}
				lastPrune = now
			}
		}
	}
}

func acmeStorageDir(cfg *config.Config) string {
	if cfg.ACME.StorageDir != "" {
		return cfg.ACME.StorageDir
	}
	return filepath.Join(filepath.Dir(cfg.Store.Path), "certificates")
}

func runReaperLoop(ctx context.Context, reaper *core.Reaper, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepCtx, cancel := context.WithTimeout(context.Background(), reaperSweepTimeout)
			if err := reaper.Sweep(sweepCtx); err != nil {
				logger.Error("reaper sweep failed", "error", err)
			}
			cancel()
		}
	}
}

func newDeployProvider(cfg *config.Config, logger *slog.Logger) (providerapi.DeployProvider, error) {
	provider := cfg.Deploy.Provider
	if provider == "" {
		provider = "compose"
	}
	if provider == "kubernetes" {
		return kubernetes.New(cfg.Deploy.KubernetesNamespace, cfg.BaseDomain, cfg.Deploy.DNSTarget,
			cfg.Deploy.KubernetesIngressClass, cfg.Deploy.KubernetesKubeconfig, cfg.Deploy.KubernetesContext,
			cfg.Deploy.KubernetesContainerPort, cfg.Deploy.KubernetesServicePort), nil
	}
	if provider != "compose" {
		return nil, fmt.Errorf("unsupported deploy provider %q", provider)
	}
	keyBytes, err := os.ReadFile(cfg.Deploy.SSHPrivateKeyPath) //nolint:gosec // operator-supplied, config-driven path
	if err != nil {
		return nil, fmt.Errorf("reading ssh private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing ssh private key: %w", err)
	}

	hostKeyCallback, err := deployHostKeyCallback(cfg.Deploy.SSHKnownHostsPath, logger)
	if err != nil {
		return nil, err
	}

	return compose.New(cfg.Deploy.SSHAddr, cfg.Deploy.SSHUser, signer, hostKeyCallback, cfg.Deploy.ComposeFile, cfg.Deploy.DNSTarget), nil
}

func deployHostKeyCallback(knownHostsPath string, logger *slog.Logger) (ssh.HostKeyCallback, error) {
	if knownHostsPath == "" {
		logger.Warn("deploy.ssh_known_hosts_path is not set; the deploy host's SSH key will not be verified")
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // explicit operator opt-out, warned above
	}
	cb, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("loading known_hosts %s: %w", knownHostsPath, err)
	}
	return cb, nil
}
