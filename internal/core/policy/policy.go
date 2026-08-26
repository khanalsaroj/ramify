// SPDX-License-Identifier: Apache-2.0

// Package policy decides which webhook events are allowed to create or update a
// preview environment. Without it every branch push produces an environment,
// which on a busy repository means every feature branch holds a DNS record, a
// certificate, and a running container until its TTL lapses.
//
// The decisions here are pure: they read only the normalized event. The
// concurrency ceiling is not implemented in this package because it needs to
// count existing environments; internal/core applies it separately, after these
// checks pass.
package policy

import (
	"fmt"
	"path"
	"strings"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// Policy is the set of rules an event must satisfy to produce an environment.
// The zero value allows everything, which is the behavior Ramify had before this
// package existed: adding the config block is opt-in.
type Policy struct {
	// PROnly skips branch pushes that have no associated pull request. It is the
	// blunt instrument: with it set, a branch only gets an environment once
	// someone opens a request for it.
	PROnly bool
	// AllowBranches is a list of shell-style glob patterns. When non-empty, a
	// branch must match at least one to be deployed.
	AllowBranches []string
	// DenyBranches is a list of shell-style glob patterns. A branch matching any
	// of them is never deployed, even if it also matches AllowBranches: deny wins
	// so a narrow exclusion can be carved out of a broad allow.
	DenyBranches []string
	// RequiredLabels gates on pull request labels. When non-empty, the request
	// must carry at least one of them. See Decide for what happens on a host that
	// does not expose labels.
	RequiredLabels []string
}

// Decision is the outcome of evaluating an event against a Policy. Reason is
// written for a human: it is logged, and surfaced in the pull request comment when
// an event is skipped, so nobody has to guess why their branch did not deploy.
type Decision struct {
	Allowed bool
	Reason  string
}

// Allowed is the decision for an event that passed every rule.
var allowed = Decision{Allowed: true}

// Decide reports whether ev may create or update an environment.
//
// The label rule deliberately skips rather than blocks when the host cannot
// supply labels. Bitbucket Cloud has no pull request labels, and a bare branch
// push has no request to carry them; treating either as an empty label set would
// silently disable previews entirely on Bitbucket and for every branch push. Set
// PROnly alongside RequiredLabels if you want the gate to be absolute.
func (p Policy) Decide(ev providerapi.Event) Decision {
	if p.PROnly && ev.PRNumber == 0 {
		return Decision{Reason: "skipped: pr_only is set and this event has no pull request"}
	}
	for _, pattern := range p.DenyBranches {
		if matches(pattern, ev.Branch) {
			return Decision{Reason: fmt.Sprintf("skipped: branch %q matches deny pattern %q", ev.Branch, pattern)}
		}
	}
	if len(p.AllowBranches) > 0 && !matchesAny(p.AllowBranches, ev.Branch) {
		return Decision{Reason: fmt.Sprintf("skipped: branch %q matches no allow pattern (%s)", ev.Branch, strings.Join(p.AllowBranches, ", "))}
	}
	if len(p.RequiredLabels) > 0 && ev.LabelsKnown && !hasAny(p.RequiredLabels, ev.Labels) {
		return Decision{Reason: fmt.Sprintf("skipped: pull request carries none of the required labels (%s)", strings.Join(p.RequiredLabels, ", "))}
	}
	return allowed
}

// matches reports whether branch matches a shell-style glob pattern.
//
// path.Match is used rather than a substring or regex test because branch names
// are slash-separated and operators think in terms of "feat/*". One consequence
// is worth knowing: path.Match's "*" does not cross a slash, so "feat/*" matches
// "feat/login" but not "feat/login/step-two". Use "feat/**" for that, which this
// function rewrites into a prefix test since path.Match has no "**".
func matches(pattern, branch string) bool {
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(branch, strings.TrimSuffix(pattern, "**"))
	}
	if pattern == "**" {
		return true
	}
	// A malformed pattern (an unclosed character class, say) matches nothing
	// rather than erroring: a bad deny pattern must not silently allow, and a bad
	// allow pattern must not silently allow everything either.
	ok, err := path.Match(pattern, branch)
	return err == nil && ok
}

func matchesAny(patterns []string, branch string) bool {
	for _, pattern := range patterns {
		if matches(pattern, branch) {
			return true
		}
	}
	return false
}

// hasAny reports whether have contains at least one of want, compared case
// insensitively: GitHub and GitLab both preserve the case an operator typed, and
// "Preview" and "preview" are the same label to everyone but a byte comparison.
func hasAny(want, have []string) bool {
	for _, w := range want {
		for _, h := range have {
			if strings.EqualFold(w, h) {
				return true
			}
		}
	}
	return false
}
