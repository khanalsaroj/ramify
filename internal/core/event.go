// SPDX-License-Identifier: Apache-2.0

package core

import (
	"encoding/json"
	"fmt"

	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// Event kinds recorded in the store.events log. These are persisted before any
// provider call is made, so a crash mid-reconciliation can be recovered by
// replaying unprocessed events on restart.
const (
	EventKindApplyRequested   = "apply_requested"
	EventKindDestroyRequested = "destroy_requested"
	EventKindSleepRequested   = "sleep_requested"
	EventKindWakeRequested    = "wake_requested"
	EventKindWebhookReceived  = store.EventKindWebhookReceived
)

// ApplyRequest is the reconciler-level request to create or update an environment.
// It is built from a normalized providerapi.Event plus a computed Subdomain (see
// internal/core/domain.Normalize) by the webhook ingestion path.
type ApplyRequest struct {
	Project     string
	Branch      string
	PRNumber    int
	Subdomain   string
	ArtifactRef string
}

// ApplyRequestFromEvent builds an ApplyRequest from a normalized GitProvider event
// and a precomputed subdomain label.
func ApplyRequestFromEvent(ev providerapi.Event, subdomain string) ApplyRequest {
	return ApplyRequest{
		Project:     ev.Project,
		Branch:      ev.Branch,
		PRNumber:    ev.PRNumber,
		Subdomain:   subdomain,
		ArtifactRef: ev.Artifact,
	}
}

// applyRequestedPayload is the JSON shape stored in events.payload for an
// EventKindApplyRequested event.
type applyRequestedPayload struct {
	Project     string `json:"project"`
	Branch      string `json:"branch"`
	PRNumber    int    `json:"pr_number"`
	Subdomain   string `json:"subdomain"`
	ArtifactRef string `json:"artifact_ref"`
}

// projectBranchPayload is the JSON shape stored in events.payload for events
// that only need to name their target, not describe a change: destroy, sleep,
// and wake all resolve their target environment from the event row's
// EnvironmentID at replay time, so this payload exists purely for operator
// visibility (e.g. reading events.payload while debugging), not because replay
// consumes it.
type projectBranchPayload struct {
	Project string `json:"project"`
	Branch  string `json:"branch"`
}

// webhookReceivedPayload is the durable inbox representation of a normalized
// provider event. It is persisted before the HTTP webhook is acknowledged.
type webhookReceivedPayload struct {
	Event providerapi.Event `json:"event"`
}

func marshalApplyPayload(req ApplyRequest) (string, error) {
	b, err := json.Marshal(applyRequestedPayload(req))
	if err != nil {
		return "", fmt.Errorf("marshaling apply event payload: %w", err)
	}
	return string(b), nil
}

func marshalDestroyPayload(project, branch string) (string, error) {
	b, err := json.Marshal(projectBranchPayload{Project: project, Branch: branch})
	if err != nil {
		return "", fmt.Errorf("marshaling destroy event payload: %w", err)
	}
	return string(b), nil
}

func marshalSleepPayload(project, branch string) (string, error) {
	b, err := json.Marshal(projectBranchPayload{Project: project, Branch: branch})
	if err != nil {
		return "", fmt.Errorf("marshaling sleep event payload: %w", err)
	}
	return string(b), nil
}

func marshalWakePayload(project, branch string) (string, error) {
	b, err := json.Marshal(projectBranchPayload{Project: project, Branch: branch})
	if err != nil {
		return "", fmt.Errorf("marshaling wake event payload: %w", err)
	}
	return string(b), nil
}

func unmarshalApplyPayload(payload string) (applyRequestedPayload, error) {
	var p applyRequestedPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return applyRequestedPayload{}, fmt.Errorf("unmarshaling apply event payload: %w", err)
	}
	return p, nil
}

// MarshalWebhookPayload serializes a normalized provider event for the durable
// webhook inbox.
func MarshalWebhookPayload(ev providerapi.Event) (string, error) {
	b, err := json.Marshal(webhookReceivedPayload{Event: ev})
	if err != nil {
		return "", fmt.Errorf("marshaling webhook event payload: %w", err)
	}
	return string(b), nil
}

// UnmarshalWebhookPayload decodes a durable webhook inbox payload.
func UnmarshalWebhookPayload(payload string) (providerapi.Event, error) {
	var p webhookReceivedPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return providerapi.Event{}, fmt.Errorf("unmarshaling webhook event payload: %w", err)
	}
	return p.Event, nil
}
