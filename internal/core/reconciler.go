// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// maxApplyAttempts is the maximum number of times Apply tries the deploy/DNS/cert
// sequence before giving up and marking the environment failed.
const maxApplyAttempts = 5

// Reconciler drives preview environments toward their desired state using the
// providerapi interfaces. It never executes a build: DeployProvider.Apply only ever
// receives an already-built ArtifactRef.
type Reconciler struct {
	store  store.Store
	deploy providerapi.DeployProvider
	dns    providerapi.DNSProvider
	cert   providerapi.CertificateProvider
	notify providerapi.NotifierProvider
	clock  Clock

	// baseDomain is the DNS zone/suffix environments are published under, e.g.
	// "preview.example.com". A subdomain "feature-x" is published as
	// "feature-x.preview.example.com".
	baseDomain string

	logger *slog.Logger
}

// NewReconciler constructs a Reconciler. All dependencies are passed in explicitly;
// the Reconciler holds no package-level or global state.
func NewReconciler(
	st store.Store,
	deploy providerapi.DeployProvider,
	dns providerapi.DNSProvider,
	cert providerapi.CertificateProvider,
	notify providerapi.NotifierProvider,
	clock Clock,
	baseDomain string,
	logger *slog.Logger,
) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		store:      st,
		deploy:     deploy,
		dns:        dns,
		cert:       cert,
		notify:     notify,
		clock:      clock,
		baseDomain: baseDomain,
		logger:     logger,
	}
}

// OwnershipTag returns the stable DNS ownership marker for a project/branch pair,
// written to a TXT record alongside every A/CNAME record Ramify creates, in the
// style of the external-dns project's registry pattern.
func OwnershipTag(project, branch string) string {
	sum := sha256.Sum256([]byte(project + "/" + branch))
	return "ramify-" + hex.EncodeToString(sum[:])[:16]
}

func (r *Reconciler) fqdn(subdomain string) string {
	return subdomain + "." + r.baseDomain
}

// Apply creates the environment for req if it doesn't exist, or updates it in place
// if it does. It persists an EventKindApplyRequested event before making any
// provider call, so a crash mid-reconciliation can be recovered by replaying
// unprocessed events via ReplayUnprocessedEvents. Calling Apply twice with the same
// req is safe: it does not create a duplicate deployment or DNS record.
func (r *Reconciler) Apply(ctx context.Context, req ApplyRequest) (store.Environment, error) {
	env, err := r.upsertPendingEnvironment(ctx, req)
	if err != nil {
		return store.Environment{}, fmt.Errorf("reconciler: apply %s/%s: %w", req.Project, req.Branch, err)
	}

	payload, err := marshalApplyPayload(req)
	if err != nil {
		return store.Environment{}, fmt.Errorf("reconciler: apply %s/%s: %w", req.Project, req.Branch, err)
	}
	ev, err := r.store.CreateEvent(ctx, store.Event{EnvironmentID: env.ID, Kind: EventKindApplyRequested, Payload: payload})
	if err != nil {
		return store.Environment{}, fmt.Errorf("reconciler: apply %s/%s: %w", req.Project, req.Branch, err)
	}

	result, applyErr := r.doApply(ctx, env, req)

	if markErr := r.store.MarkEventProcessed(ctx, ev.ID, r.clock.Now()); markErr != nil {
		r.logger.ErrorContext(ctx, "reconciler: marking apply event processed", "error", markErr, "event_id", ev.ID)
	}

	return result, applyErr
}

// upsertPendingEnvironment returns the existing environment for req.Project/Branch,
// or creates a new one in StatusPending if none exists yet. It does not itself make
// any provider call.
func (r *Reconciler) upsertPendingEnvironment(ctx context.Context, req ApplyRequest) (store.Environment, error) {
	env, err := r.store.GetEnvironmentByProjectBranch(ctx, req.Project, req.Branch)
	if errors.Is(err, store.ErrNotFound) {
		return r.store.CreateEnvironment(ctx, store.Environment{
			Project:     req.Project,
			Branch:      req.Branch,
			PRNumber:    req.PRNumber,
			Subdomain:   req.Subdomain,
			ArtifactRef: req.ArtifactRef,
			Status:      store.StatusPending,
		})
	}
	if err != nil {
		return store.Environment{}, fmt.Errorf("looking up environment: %w", err)
	}
	return env, nil
}

// doApply drives the deploy -> DNS -> certificate sequence for env/req, retrying
// with capped exponential backoff up to maxApplyAttempts before marking the
// environment failed and notifying.
func (r *Reconciler) doApply(ctx context.Context, env store.Environment, req ApplyRequest) (store.Environment, error) {
	isUpdate := env.Status == store.StatusReady || env.DeployRef != ""

	env.PRNumber = req.PRNumber
	env.Subdomain = req.Subdomain
	env.ArtifactRef = req.ArtifactRef
	env.Status = store.StatusDeploying
	env, err := r.store.UpdateEnvironment(ctx, env)
	if err != nil {
		return store.Environment{}, fmt.Errorf("marking environment deploying: %w", err)
	}

	fqdn := r.fqdn(req.Subdomain)
	tag := OwnershipTag(req.Project, req.Branch)

	var lastErr error
	for attempt := 1; attempt <= maxApplyAttempts; attempt++ {
		if attempt > 1 {
			r.clock.Sleep(applyBackoff(attempt))
		}

		env, lastErr = r.attemptApply(ctx, env, req, fqdn, tag)
		if lastErr == nil {
			notifyKind := "ready"
			if isUpdate {
				notifyKind = "updated"
			}
			if err := r.notify.Notify(ctx, req.Project, req.PRNumber, providerapi.NotifyEvent{Kind: notifyKind, URL: "https://" + fqdn}); err != nil {
				r.logger.ErrorContext(ctx, "reconciler: notify failed", "error", err)
			}
			return env, nil
		}
	}

	env.Status = store.StatusFailed
	if _, err := r.store.UpdateEnvironment(ctx, env); err != nil {
		r.logger.ErrorContext(ctx, "reconciler: marking environment failed", "error", err)
	}
	if err := r.notify.Notify(ctx, req.Project, req.PRNumber, providerapi.NotifyEvent{Kind: "failed", Detail: lastErr.Error()}); err != nil {
		r.logger.ErrorContext(ctx, "reconciler: notify failed", "error", err)
	}
	return store.Environment{}, fmt.Errorf("apply failed after %d attempts: %w", maxApplyAttempts, lastErr)
}

// attemptApply runs a single deploy -> DNS -> certificate -> persist attempt.
func (r *Reconciler) attemptApply(ctx context.Context, env store.Environment, req ApplyRequest, fqdn, tag string) (store.Environment, error) {
	deployment, err := r.deploy.Apply(ctx, providerapi.EnvSpec{
		Project:     req.Project,
		Branch:      req.Branch,
		Subdomain:   req.Subdomain,
		ArtifactRef: req.ArtifactRef,
		PreviousRef: env.DeployRef,
	})
	if err != nil {
		return env, fmt.Errorf("deploy apply: %w", err)
	}

	rec := providerapi.DNSRecord{Zone: r.baseDomain, Name: fqdn, Type: "A", Value: deployment.InternalAddr, OwnershipTag: tag}
	if err := r.dns.EnsureRecord(ctx, rec); err != nil {
		return env, fmt.Errorf("dns ensure record: %w", err)
	}

	if _, err := r.cert.EnsureCertificate(ctx, fqdn); err != nil {
		return env, fmt.Errorf("ensure certificate: %w", err)
	}

	if err := r.recordDNSRow(ctx, env.ID, rec); err != nil {
		return env, err
	}

	env.DeployRef = deployment.Ref
	env.Status = store.StatusReady
	env, err = r.store.UpdateEnvironment(ctx, env)
	if err != nil {
		return env, fmt.Errorf("marking environment ready: %w", err)
	}
	return env, nil
}

// recordDNSRow persists a dns_records row for rec if one doesn't already exist for
// this environment and name, keeping the store idempotent alongside the provider's
// own idempotent EnsureRecord.
func (r *Reconciler) recordDNSRow(ctx context.Context, environmentID string, rec providerapi.DNSRecord) error {
	existing, err := r.store.ListDNSRecords(ctx, environmentID)
	if err != nil {
		return fmt.Errorf("listing dns records: %w", err)
	}
	for _, e := range existing {
		if e.Name == rec.Name {
			return nil
		}
	}
	if _, err := r.store.CreateDNSRecord(ctx, store.DNSRecord{
		EnvironmentID: environmentID,
		RecordType:    rec.Type,
		Name:          rec.Name,
		Value:         rec.Value,
		OwnershipTag:  rec.OwnershipTag,
	}); err != nil {
		return fmt.Errorf("recording dns record: %w", err)
	}
	return nil
}

// Destroy tears env down in the fixed order: revoke/delete DNS records, then
// certificate, then deploy target. It persists an EventKindDestroyRequested event
// before any provider call. If any step fails, env stays in StatusDestroying and the
// caller (typically the reaper) is expected to retry Destroy later; it is never
// marked destroyed on a partial failure. Calling Destroy twice on an already-torn-
// down environment does not error.
func (r *Reconciler) Destroy(ctx context.Context, env store.Environment) error {
	payload, err := marshalDestroyPayload(env.Project, env.Branch)
	if err != nil {
		return fmt.Errorf("reconciler: destroy %s: %w", env.ID, err)
	}
	ev, err := r.store.CreateEvent(ctx, store.Event{EnvironmentID: env.ID, Kind: EventKindDestroyRequested, Payload: payload})
	if err != nil {
		return fmt.Errorf("reconciler: destroy %s: %w", env.ID, err)
	}

	if env.Status != store.StatusDestroying {
		env.Status = store.StatusDestroying
		env, err = r.store.UpdateEnvironment(ctx, env)
		if err != nil {
			return fmt.Errorf("reconciler: destroy %s: marking destroying: %w", env.ID, err)
		}
	}

	destroyErr := r.doDestroy(ctx, env)

	if markErr := r.store.MarkEventProcessed(ctx, ev.ID, r.clock.Now()); markErr != nil {
		r.logger.ErrorContext(ctx, "reconciler: marking destroy event processed", "error", markErr, "event_id", ev.ID)
	}

	if destroyErr != nil {
		return fmt.Errorf("reconciler: destroy %s: %w", env.ID, destroyErr)
	}
	return nil
}

func (r *Reconciler) doDestroy(ctx context.Context, env store.Environment) error {
	records, err := r.store.ListDNSRecords(ctx, env.ID)
	if err != nil {
		return fmt.Errorf("listing dns records: %w", err)
	}
	for _, rec := range records {
		if err := r.dns.DeleteRecord(ctx, providerapi.DNSRecord{
			Zone: r.baseDomain, Name: rec.Name, Type: rec.RecordType, Value: rec.Value, OwnershipTag: rec.OwnershipTag,
		}); err != nil {
			return fmt.Errorf("deleting dns record %s: %w", rec.Name, err)
		}
		if err := r.store.DeleteDNSRecord(ctx, rec.ID); err != nil {
			return fmt.Errorf("removing dns record row %s: %w", rec.ID, err)
		}
	}

	if err := r.cert.RevokeCertificate(ctx, r.fqdn(env.Subdomain)); err != nil {
		return fmt.Errorf("revoking certificate: %w", err)
	}

	if env.DeployRef != "" {
		if err := r.deploy.Destroy(ctx, env.DeployRef); err != nil {
			return fmt.Errorf("destroying deployment: %w", err)
		}
	}

	env.Status = store.StatusDestroyed
	if _, err := r.store.UpdateEnvironment(ctx, env); err != nil {
		return fmt.Errorf("marking environment destroyed: %w", err)
	}

	if err := r.notify.Notify(ctx, env.Project, env.PRNumber, providerapi.NotifyEvent{Kind: "destroyed"}); err != nil {
		r.logger.ErrorContext(ctx, "reconciler: notify failed", "error", err)
	}
	return nil
}

// ReplayUnprocessedEvents re-drives every event with a NULL processed_at, oldest
// first. Call it once at startup, before serving new events, so a crash
// mid-reconciliation is recovered.
func (r *Reconciler) ReplayUnprocessedEvents(ctx context.Context) error {
	events, err := r.store.ListUnprocessedEvents(ctx)
	if err != nil {
		return fmt.Errorf("reconciler: replay: listing unprocessed events: %w", err)
	}
	for _, ev := range events {
		r.replayOne(ctx, ev)
	}
	return nil
}

func (r *Reconciler) replayOne(ctx context.Context, ev store.Event) {
	switch ev.Kind {
	case EventKindApplyRequested:
		payload, err := unmarshalApplyPayload(ev.Payload)
		if err != nil {
			r.logger.ErrorContext(ctx, "reconciler: replay: bad apply payload", "error", err, "event_id", ev.ID)
			return
		}
		env, err := r.store.GetEnvironment(ctx, ev.EnvironmentID)
		if err != nil {
			r.logger.ErrorContext(ctx, "reconciler: replay: environment missing", "error", err, "event_id", ev.ID)
			return
		}
		req := ApplyRequest(payload)
		if _, err := r.doApply(ctx, env, req); err != nil {
			r.logger.ErrorContext(ctx, "reconciler: replay: apply failed", "error", err, "event_id", ev.ID)
		}
	case EventKindDestroyRequested:
		env, err := r.store.GetEnvironment(ctx, ev.EnvironmentID)
		if err != nil {
			r.logger.ErrorContext(ctx, "reconciler: replay: environment missing", "error", err, "event_id", ev.ID)
			return
		}
		if err := r.doDestroy(ctx, env); err != nil {
			r.logger.ErrorContext(ctx, "reconciler: replay: destroy failed", "error", err, "event_id", ev.ID)
		}
	default:
		r.logger.WarnContext(ctx, "reconciler: replay: unknown event kind", "kind", ev.Kind, "event_id", ev.ID)
		return
	}

	if err := r.store.MarkEventProcessed(ctx, ev.ID, r.clock.Now()); err != nil {
		r.logger.ErrorContext(ctx, "reconciler: replay: marking event processed", "error", err, "event_id", ev.ID)
	}
}

// applyBackoff returns the delay before retry attempt n (n >= 2): 1s, 2s, 4s, 8s,
// capped at 30s.
func applyBackoff(attempt int) time.Duration {
	const maxBackoff = 30 * time.Second
	d := time.Duration(1<<uint(attempt-2)) * time.Second
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}
