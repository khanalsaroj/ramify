// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

func push(branch string) providerapi.Event {
	return providerapi.Event{Kind: "branch_pushed", Project: "acme/web", Branch: branch}
}

func pr(branch string, labels ...string) providerapi.Event {
	return providerapi.Event{
		Kind: "pr_synchronized", Project: "acme/web", Branch: branch, PRNumber: 7,
		Labels: labels, LabelsKnown: true,
	}
}

// The zero Policy must preserve the behavior Ramify had before filtering existed,
// or adding the config block would silently change every deployment.
func TestZeroPolicyAllowsEverything(t *testing.T) {
	var p Policy
	require.True(t, p.Decide(push("anything/at/all")).Allowed)
	require.True(t, p.Decide(pr("feat/login")).Allowed)
}

func TestPROnlySkipsBareBranchPush(t *testing.T) {
	p := Policy{PROnly: true}
	got := p.Decide(push("feat/login"))
	require.False(t, got.Allowed)
	require.Contains(t, got.Reason, "pr_only")
	require.True(t, p.Decide(pr("feat/login")).Allowed)
}

func TestAllowBranchesRestrictsToMatches(t *testing.T) {
	p := Policy{AllowBranches: []string{"feat/*", "release/*"}}
	require.True(t, p.Decide(push("feat/login")).Allowed)
	require.True(t, p.Decide(push("release/2026-01")).Allowed)

	got := p.Decide(push("dependabot/npm/lodash"))
	require.False(t, got.Allowed)
	require.Contains(t, got.Reason, "no allow pattern")
}

// Deny must beat allow so a narrow exclusion can be carved out of a broad allow.
func TestDenyBeatsAllow(t *testing.T) {
	p := Policy{AllowBranches: []string{"**"}, DenyBranches: []string{"dependabot/**"}}
	require.True(t, p.Decide(push("feat/login")).Allowed)

	got := p.Decide(push("dependabot/npm/lodash"))
	require.False(t, got.Allowed)
	require.Contains(t, got.Reason, "deny pattern")
}

// The trap this convention sets: "dependabot/*" does not stop
// "dependabot/npm/lodash", because dependabot nests two levels deep. Pinned so
// the documentation and the examples keep using the double star for deny rules.
func TestSingleStarDenyMissesNestedBranches(t *testing.T) {
	p := Policy{DenyBranches: []string{"dependabot/*"}}
	require.False(t, p.Decide(push("dependabot/lodash")).Allowed)
	require.True(t, p.Decide(push("dependabot/npm/lodash")).Allowed)
}

// path.Match's "*" stops at a slash, which surprises people writing branch
// patterns. "feat/**" is rewritten to a prefix test so the intent works.
func TestDoubleStarCrossesSlashes(t *testing.T) {
	require.False(t, matches("feat/*", "feat/login/step-two"))
	require.True(t, matches("feat/**", "feat/login/step-two"))
	require.True(t, matches("feat/**", "feat/login"))
	require.True(t, matches("**", "anything/at/all"))
}

// Patterns follow the same convention as GitHub Actions branch filters, which is
// what operators configuring this will already have in their fingers: a single
// star stops at a slash, so "*" matches "main" but not "feat/login". Getting this
// backwards would make `allow: ["*"]` look like "everything" while silently
// dropping every namespaced branch.
func TestSingleStarDoesNotCrossSlashes(t *testing.T) {
	require.True(t, matches("*", "main"))
	require.False(t, matches("*", "feat/login"))

	p := Policy{AllowBranches: []string{"*"}}
	require.True(t, p.Decide(push("main")).Allowed)
	require.False(t, p.Decide(push("feat/login")).Allowed)
}

// A malformed pattern must not silently allow: a broken deny would let blocked
// branches through, and a broken allow would open the gate to everything.
func TestMalformedPatternMatchesNothing(t *testing.T) {
	require.False(t, matches("feat/[", "feat/login"))

	p := Policy{AllowBranches: []string{"feat/["}}
	require.False(t, p.Decide(push("feat/login")).Allowed)
}

func TestRequiredLabelsGatePullRequests(t *testing.T) {
	p := Policy{RequiredLabels: []string{"preview"}}
	require.True(t, p.Decide(pr("feat/login", "preview")).Allowed)
	require.True(t, p.Decide(pr("feat/login", "bug", "preview")).Allowed)

	got := p.Decide(pr("feat/login", "bug"))
	require.False(t, got.Allowed)
	require.Contains(t, got.Reason, "required labels")
}

// An operator typing "Preview" on the pull request and "preview" in the config
// means the same thing to everyone but a byte comparison.
func TestRequiredLabelsAreCaseInsensitive(t *testing.T) {
	p := Policy{RequiredLabels: []string{"preview"}}
	require.True(t, p.Decide(pr("feat/login", "Preview")).Allowed)
}

// A pull request that GitHub reports as having zero labels is "known and empty",
// so the gate applies. This is the case that must NOT be confused with a host
// that cannot report labels at all.
func TestKnownButEmptyLabelsAreGated(t *testing.T) {
	p := Policy{RequiredLabels: []string{"preview"}}
	got := p.Decide(pr("feat/login"))
	require.False(t, got.Allowed)
}

// Bitbucket Cloud has no pull request labels and a bare branch push has no
// request at all. Gating on an absence Ramify cannot observe would disable
// previews entirely on Bitbucket, so the rule is skipped instead.
func TestLabelGateSkippedWhenHostCannotSupplyLabels(t *testing.T) {
	p := Policy{RequiredLabels: []string{"preview"}}

	bitbucket := providerapi.Event{Kind: "pr_synchronized", Branch: "feat/login", PRNumber: 7}
	require.False(t, bitbucket.LabelsKnown)
	require.True(t, p.Decide(bitbucket).Allowed)

	require.True(t, p.Decide(push("feat/login")).Allowed)
}

// PROnly is how an operator makes the label gate absolute despite the skip above.
func TestPROnlyPlusLabelsClosesTheBranchPushHole(t *testing.T) {
	p := Policy{PROnly: true, RequiredLabels: []string{"preview"}}
	require.False(t, p.Decide(push("feat/login")).Allowed)
	require.False(t, p.Decide(pr("feat/login", "bug")).Allowed)
	require.True(t, p.Decide(pr("feat/login", "preview")).Allowed)
}

// Every skip must explain itself: the reason is logged and posted to the pull
// request, so an empty one leaves the developer guessing.
func TestEverySkipCarriesAReason(t *testing.T) {
	cases := []struct {
		name string
		p    Policy
		ev   providerapi.Event
	}{
		{"pr only", Policy{PROnly: true}, push("feat/login")},
		{"deny", Policy{DenyBranches: []string{"**"}}, push("feat/login")},
		{"allow", Policy{AllowBranches: []string{"nope"}}, push("feat/login")},
		{"labels", Policy{RequiredLabels: []string{"preview"}}, pr("feat/login")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.p.Decide(tc.ev)
			require.False(t, got.Allowed)
			require.NotEmpty(t, got.Reason)
		})
	}
}
