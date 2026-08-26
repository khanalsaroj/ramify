// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/khanalsaroj/ramify/internal/core/domain"
	"github.com/khanalsaroj/ramify/internal/core/policy"
	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// maxApplyAttempts is the maximum number of times Apply tries the deploy/DNS/cert
// sequence before giving up and marking the environment failed.
const maxApplyAttempts = 5

// maxEventAttempts caps durable retries of a single inbox event. Past this the
// event is dead-lettered rather than retried forever, so a permanently broken
// event cannot occupy the worker indefinitely or hide behind an ever-growing
// backoff.
const maxEventAttempts = 10

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
	// defaultTTL is applied to ttl_expires_at on every successful Apply,
	// refreshing it — so an environment stays alive as long as it keeps receiving
	// pushes and expires defaultTTL after the last one. Zero disables TTL
	// assignment (an environment is then only removed by an explicit Destroy).
	defaultTTL time.Duration

	// policy decides which webhook events may produce an environment. Its zero
	// value allows everything, so an operator who configures nothing keeps the
	// original behavior.
	policy policy.Policy
	// maxConcurrent caps the number of live environments. Zero means no ceiling.
	maxConcurrent int
	// admissionMu serializes the count-then-reserve step of admission. The
	// per-branch locks below cannot do this job: two pushes to *different*
	// branches take different locks, so without this both could observe
	// maxConcurrent-1 live environments and both create one.
	admissionMu sync.Mutex

	logger *slog.Logger
	locks  sync.Map // project/branch -> *sync.Mutex

	// jitter randomizes a computed retry delay. It is a field so tests can make
	// backoff deterministic; production uses fullJitter.
	jitter func(time.Duration) time.Duration
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
	defaultTTL time.Duration,
	logger *slog.Logger,
	opts ...Option,
) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Reconciler{
		store:      st,
		deploy:     deploy,
		dns:        dns,
		cert:       cert,
		notify:     notify,
		clock:      clock,
		baseDomain: baseDomain,
		defaultTTL: defaultTTL,
		logger:     logger,
		jitter:     fullJitter,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Option configures optional Reconciler behavior. Options exist so admission
// rules could be added without changing the signature every existing caller and
// test already passes.
type Option func(*Reconciler)

// WithAdmission sets the event-admission policy and the ceiling on live
// environments. A maxConcurrent of zero or less means no ceiling.
func WithAdmission(p policy.Policy, maxConcurrent int) Option {
	return func(r *Reconciler) {
		r.policy = p
		r.maxConcurrent = maxConcurrent
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
	unlock := r.lock(req.Project, req.Branch)
	defer unlock()
	return r.applyLocked(ctx, req)
}

func (r *Reconciler) applyLocked(ctx context.Context, req ApplyRequest) (store.Environment, error) {
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

	if applyErr == nil {
		if markErr := r.store.MarkEventProcessed(ctx, ev.ID, r.clock.Now()); markErr != nil {
			r.logger.ErrorContext(ctx, "reconciler: marking apply event processed", "error", markErr, "event_id", ev.ID)
		}
	} else {
		r.scheduleRetry(ctx, ev, applyErr)
		r.logger.WarnContext(ctx, "reconciler: apply event remains pending for retry", "event_id", ev.ID, "error", applyErr)
	}

	return result, applyErr
}

func (r *Reconciler) applyWithoutEvent(ctx context.Context, req ApplyRequest) error {
	env, err := r.upsertPendingEnvironment(ctx, req)
	if err != nil {
		return fmt.Errorf("reconciler: webhook apply: %w", err)
	}
	_, err = r.doApply(ctx, env, req)
	return err
}

// admit evaluates the policy and the concurrency ceiling for an inbound webhook
// event, returning a human-readable reason when the event must be skipped. An
// empty reason means the event may proceed.
//
// Only creation is capped. An event for an environment that already exists is
// always admitted, because updating it consumes no new slot and because dropping
// updates at the ceiling would freeze live environments on a stale commit.
func (r *Reconciler) admit(ctx context.Context, ev providerapi.Event, req ApplyRequest) (string, error) {
	if decision := r.policy.Decide(ev); !decision.Allowed {
		return decision.Reason, nil
	}
	if r.maxConcurrent <= 0 {
		return "", nil
	}

	r.admissionMu.Lock()
	defer r.admissionMu.Unlock()

	_, err := r.store.GetEnvironmentByProjectBranch(ctx, req.Project, req.Branch)
	switch {
	case err == nil:
		return "", nil
	case !errors.Is(err, store.ErrNotFound):
		return "", fmt.Errorf("reconciler: admission lookup %s/%s: %w", req.Project, req.Branch, err)
	}

	live, err := r.store.CountLiveEnvironments(ctx)
	if err != nil {
		return "", fmt.Errorf("reconciler: counting live environments: %w", err)
	}
	if live >= r.maxConcurrent {
		return fmt.Sprintf("skipped: max_concurrent_envs reached (%d of %d live); destroy an environment or let one expire to free a slot", live, r.maxConcurrent), nil
	}

	// Reserve the slot before releasing admissionMu. Counting and then letting
	// doApply create the row later would reopen the race this mutex closes.
	if _, err := r.upsertPendingEnvironment(ctx, req); err != nil {
		return "", fmt.Errorf("reconciler: reserving slot for %s/%s: %w", req.Project, req.Branch, err)
	}
	return "", nil
}

func (r *Reconciler) lock(project, branch string) func() {
	key := project + "\x00" + branch
	value, _ := r.locks.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ProcessWebhookEvent applies or destroys the desired environment represented
// by a durable webhook inbox event.
func (r *Reconciler) ProcessWebhookEvent(ctx context.Context, ev providerapi.Event) error {
	switch ev.Kind {
	case "branch_pushed", "pr_synchronized":
		subdomain := domain.Normalize(ev.Branch, 63)
		req := ApplyRequestFromEvent(ev, subdomain)
		unlock := r.lock(req.Project, req.Branch)
		defer unlock()
		reason, err := r.admit(ctx, ev, req)
		if err != nil {
			return err
		}
		if reason != "" {
			// Skips are logged, not commented on the pull request. A denied
			// branch pattern is matched on every push to that branch, and
			// commenting each time would bury the request in noise for what is
			// a standing operator decision rather than an error.
			r.logger.InfoContext(ctx, "reconciler: event skipped by admission policy",
				"project", ev.Project, "branch", ev.Branch, "pr", ev.PRNumber, "reason", reason)
			return nil
		}
		return r.applyWithoutEvent(ctx, req)
	case "pr_closed", "branch_deleted":
		env, err := r.store.GetEnvironmentByProjectBranch(ctx, ev.Project, ev.Branch)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		unlock := r.lock(env.Project, env.Branch)
		defer unlock()
		return r.destroyWithoutEvent(ctx, env)
	default:
		return fmt.Errorf("unsupported webhook event kind %q", ev.Kind)
	}
}

// markProcessed records successful completion of an event. Failed work must
// remain pending so startup replay or a later worker can retry it.
func (r *Reconciler) markProcessed(ctx context.Context, ev store.Event) {
	if markErr := r.store.MarkEventProcessed(ctx, ev.ID, r.clock.Now()); markErr != nil {
		r.logger.ErrorContext(ctx, "reconciler: marking apply event processed", "error", markErr, "event_id", ev.ID)
	}
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

	certRef, err := r.cert.EnsureCertificate(ctx, fqdn)
	if err != nil {
		return env, fmt.Errorf("ensure certificate: %w", err)
	}
	if installer, ok := r.deploy.(interface {
		InstallCertificate(context.Context, string, []byte, []byte) error
	}); ok && len(certRef.CertificatePEM) > 0 && len(certRef.PrivateKeyPEM) > 0 {
		if err := installer.InstallCertificate(ctx, fqdn, certRef.CertificatePEM, certRef.PrivateKeyPEM); err != nil {
			return env, fmt.Errorf("install certificate: %w", err)
		}
	}

	if err := r.recordDNSRow(ctx, env.ID, rec); err != nil {
		return env, err
	}

	env.DeployRef = deployment.Ref
	env.Status = store.StatusReady
	if r.defaultTTL > 0 {
		expiresAt := r.clock.Now().Add(r.defaultTTL)
		env.TTLExpiresAt = &expiresAt
	}
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
	unlock := r.lock(env.Project, env.Branch)
	defer unlock()
	return r.destroyLocked(ctx, env)
}

func (r *Reconciler) destroyLocked(ctx context.Context, env store.Environment) error {
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

	if destroyErr == nil {
		r.markProcessed(ctx, ev)
	} else {
		r.scheduleRetry(ctx, ev, destroyErr)
		r.logger.WarnContext(ctx, "reconciler: destroy event remains pending for retry", "event_id", ev.ID, "error", destroyErr)
	}

	if destroyErr != nil {
		return fmt.Errorf("reconciler: destroy %s: %w", env.ID, destroyErr)
	}
	return nil
}

// destroyWithoutEvent is used when the durable webhook inbox is the command
// event. Creating a second nested destroy event would duplicate work and make
// retry accounting ambiguous.
func (r *Reconciler) destroyWithoutEvent(ctx context.Context, env store.Environment) error {
	if env.Status != store.StatusDestroying {
		env.Status = store.StatusDestroying
		var err error
		env, err = r.store.UpdateEnvironment(ctx, env)
		if err != nil {
			return fmt.Errorf("marking environment destroying: %w", err)
		}
	}
	return r.doDestroy(ctx, env)
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
			if !errors.Is(err, providerapi.ErrRecordAlreadyAbsent) {
				return fmt.Errorf("deleting dns record %s: %w", rec.Name, err)
			}
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
	events, err := r.store.ListDueEvents(ctx, r.clock.Now(), 1000)
	if err != nil {
		return fmt.Errorf("reconciler: replay: listing unprocessed events: %w", err)
	}
	return r.ReplayEvents(ctx, events)
}

// ReplayEvents processes a supplied batch of due durable events. It is used by
// startup recovery and the long-running daemon worker.
func (r *Reconciler) ReplayEvents(ctx context.Context, events []store.Event) error {
	for _, ev := range events {
		r.replayOne(ctx, ev)
	}
	return nil
}

func (r *Reconciler) replayOne(ctx context.Context, ev store.Event) {
	claimed, err := r.store.ClaimEvent(ctx, ev.ID, r.clock.Now(), r.clock.Now().Add(10*time.Minute))
	if err != nil {
		r.logger.ErrorContext(ctx, "reconciler: claiming event failed", "error", err, "event_id", ev.ID)
		return
	}
	if !claimed {
		return
	}
	var replayErr error
	switch ev.Kind {
	case EventKindApplyRequested:
		payload, err := unmarshalApplyPayload(ev.Payload)
		if err != nil {
			// A payload that will not parse now will not parse later; retrying
			// would leave it pending forever.
			r.scheduleRetry(ctx, ev, Terminal(fmt.Errorf("replay: bad apply payload: %w", err)))
			return
		}
		env, err := r.store.GetEnvironment(ctx, ev.EnvironmentID)
		if err != nil {
			replayErr = r.classifyLookup(err, "replay: apply: loading environment")
			break
		}
		req := ApplyRequest(payload)
		unlock := r.lock(req.Project, req.Branch)
		replayErr = func() error { defer unlock(); _, err := r.doApply(ctx, env, req); return err }()
	case EventKindDestroyRequested:
		env, err := r.store.GetEnvironment(ctx, ev.EnvironmentID)
		if err != nil {
			replayErr = r.classifyLookup(err, "replay: destroy: loading environment")
			break
		}
		unlock := r.lock(env.Project, env.Branch)
		replayErr = func() error { defer unlock(); return r.doDestroy(ctx, env) }()
	case EventKindWebhookReceived:
		webhookEvent, err := UnmarshalWebhookPayload(ev.Payload)
		if err != nil {
			replayErr = Terminal(fmt.Errorf("replay: bad webhook payload: %w", err))
			break
		}
		replayErr = r.ProcessWebhookEvent(ctx, webhookEvent)
	default:
		// An unknown kind is a code/data mismatch no retry can resolve.
		r.scheduleRetry(ctx, ev, Terminal(fmt.Errorf("replay: unknown event kind %q", ev.Kind)))
		return
	}

	if replayErr != nil {
		r.scheduleRetry(ctx, ev, replayErr)
		r.logger.ErrorContext(ctx, "reconciler: replay failed; event remains pending", "error", replayErr, "event_id", ev.ID)
		return
	}
	r.markProcessed(ctx, ev)
}

// classifyLookup wraps an environment lookup failure, marking it terminal when
// the row is simply gone: an event referencing a deleted environment can never
// succeed, while any other store error may be transient.
func (r *Reconciler) classifyLookup(err error, op string) error {
	wrapped := fmt.Errorf("%s: %w", op, err)
	if errors.Is(err, store.ErrNotFound) {
		return Terminal(wrapped)
	}
	return wrapped
}

// scheduleRetry records a failed attempt. Permanent failures and events that
// have exhausted their attempt budget are dead-lettered instead of rescheduled,
// so the retry set only ever holds work that could still succeed.
func (r *Reconciler) scheduleRetry(ctx context.Context, ev store.Event, cause error) {
	attempt := ev.Attempts + 1
	if terminal := IsTerminal(cause); terminal || attempt >= maxEventAttempts {
		reason := "attempts exhausted"
		if terminal {
			reason = "permanent failure"
		}
		if err := r.store.MarkEventDeadLettered(ctx, ev.ID, r.clock.Now(), cause.Error()); err != nil {
			r.logger.ErrorContext(ctx, "reconciler: dead-lettering event", "error", err, "event_id", ev.ID)
			return
		}
		r.logger.ErrorContext(ctx, "reconciler: event dead-lettered; operator action required",
			"reason", reason, "cause", cause, "event_id", ev.ID, "kind", ev.Kind, "attempts", attempt)
		return
	}

	delay := r.jitter(retryDelay(attempt))
	if err := r.store.MarkEventRetry(ctx, ev.ID, r.clock.Now().Add(delay), cause.Error()); err != nil {
		r.logger.ErrorContext(ctx, "reconciler: recording retry", "error", err, "event_id", ev.ID)
	}
}

// retryDelay returns the un-jittered backoff for the given attempt number.
func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	const maxDelay = 5 * time.Minute
	d := time.Duration(1<<uint(min(attempt-1, 8))) * time.Second
	if d > maxDelay {
		return maxDelay
	}
	return d
}

// fullJitter picks a delay uniformly from [d/2, d]. Without it every event that
// failed against the same downed provider retries at the same instant, so the
// provider is hit by the whole backlog at once each time it comes back —
// exactly the retry storm the backoff is meant to prevent. The floor of d/2
// keeps the backoff curve meaningful instead of collapsing toward zero.
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(d-half)+1))
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
