// SPDX-License-Identifier: Apache-2.0

// Package domain normalizes git branch names into valid DNS subdomain labels.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	invalidChars = regexp.MustCompile(`[^a-z0-9-]`)
	multiDash    = regexp.MustCompile(`-+`)
)

// Normalize converts branch into a valid DNS subdomain label of at most maxLength
// characters:
//  1. Lowercase.
//  2. Replace '/' and '_' with '-'.
//  3. Strip every character outside [a-z0-9-].
//  4. Collapse consecutive '-' into one; trim leading/trailing '-'.
//  5. If the result exceeds maxLength, truncate and append '-' plus the first 6 hex
//     characters of sha256(branch).
//
// If the result is empty after step 4 (for example, branch consists only of
// characters stripped in step 3), Normalize falls back to the hash alone so the
// label is never empty.
func Normalize(branch string, maxLength int) string {
	hash := shortHash(branch)

	s := strings.ToLower(branch)
	s = strings.NewReplacer("/", "-", "_", "-").Replace(s)
	s = invalidChars.ReplaceAllString(s, "")
	s = multiDash.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")

	if s == "" {
		return hash
	}
	if len(s) <= maxLength {
		return s
	}

	suffix := "-" + hash
	truncateTo := maxLength - len(suffix)
	if truncateTo <= 0 {
		return hash
	}
	truncated := strings.TrimRight(s[:truncateTo], "-")
	return truncated + suffix
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:6]
}
