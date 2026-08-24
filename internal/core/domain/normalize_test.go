// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalize(t *testing.T) {
	const maxLength = 63

	tests := []struct {
		name   string
		branch string
		want   string
	}{
		{
			name:   "simple lowercase branch",
			branch: "feature-login",
			want:   "feature-login",
		},
		{
			name:   "uppercase is lowercased",
			branch: "Feature/Login",
			want:   "feature-login",
		},
		{
			name:   "slashes and underscores become dashes",
			branch: "feature/user_auth",
			want:   "feature-user-auth",
		},
		{
			name:   "disallowed characters are stripped",
			branch: "feature/fix#123!",
			want:   "feature-fix123",
		},
		{
			name:   "consecutive dashes collapse",
			branch: "feature//double__dash",
			want:   "feature-double-dash",
		},
		{
			name:   "leading and trailing dashes trimmed",
			branch: "/feature/",
			want:   "feature",
		},
		{
			name:   "leading digits pass through unchanged",
			branch: "123-hotfix",
			want:   "123-hotfix",
		},
		{
			name:   "case-insensitive collision: differs only by case",
			branch: "Feature-X",
			want:   "feature-x",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Normalize(tt.branch, maxLength)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeCaseInsensitiveCollision(t *testing.T) {
	a := Normalize("Feature-X", 63)
	b := Normalize("feature-x", 63)
	require.Equal(t, a, b, "branch names identical except for case must normalize to the same label")
}

func TestNormalizeEmptyAfterStripping(t *testing.T) {
	// A branch name consisting solely of characters stripped by step 3 (here,
	// emoji/unicode) must fall back to the hash alone, never an empty label.
	got := Normalize("\U0001F680\U0001F525", 63)
	require.NotEmpty(t, got)
	require.Equal(t, shortHash("\U0001F680\U0001F525"), got)
	require.Len(t, got, 6)
}

func TestNormalizeVeryLongBranchName(t *testing.T) {
	long := "feature-" + strings.Repeat("x", 100)
	got := Normalize(long, 63)
	require.LessOrEqual(t, len(got), 63)
	require.True(t, strings.HasSuffix(got, "-"+shortHash(long)))
}

func TestNormalizeCollideOnlyAfterTruncation(t *testing.T) {
	// Two branch names that share the same 63-char prefix but differ afterward
	// must not normalize to the same label, because the hash is computed over the
	// full original branch name.
	prefix := "feature-" + strings.Repeat("x", 60)
	branchA := prefix + "-alpha"
	branchB := prefix + "-beta"
	require.NotEqual(t, branchA, branchB)

	gotA := Normalize(branchA, 63)
	gotB := Normalize(branchB, 63)
	require.NotEqual(t, gotA, gotB, "branches colliding only after truncation must remain distinct via the hash suffix")
	require.LessOrEqual(t, len(gotA), 63)
	require.LessOrEqual(t, len(gotB), 63)
}

func TestNormalizeMaxLengthTooSmallForSuffix(t *testing.T) {
	// maxLength smaller than the "-"+6-hex-char suffix falls back to the hash
	// alone rather than producing a malformed (leading-dash) label.
	got := Normalize("some-branch-name", 4)
	require.Equal(t, shortHash("some-branch-name"), got)
}

func TestNormalizeResultNeverEmpty(t *testing.T) {
	inputs := []string{"", "___", "///", "!!!", "\U0001F680"}
	for _, in := range inputs {
		got := Normalize(in, 63)
		require.NotEmpty(t, got, "input %q must not normalize to an empty label", in)
	}
}
