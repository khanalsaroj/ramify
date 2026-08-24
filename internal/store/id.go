// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/rand"
	"fmt"
)

// newID returns a random RFC 4122 version 4 UUID string, generated from
// crypto/rand rather than a third-party UUID library to keep the dependency list
// minimal.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: reading random bytes for id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
