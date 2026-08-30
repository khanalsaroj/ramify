// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package api

// acquireProcessLock is an intentional no-op outside Unix. Ramify's documented
// production target is Linux/macOS (see docs/operations.md, docs/quickstart.md)
// deployed over a Unix domain socket; on other platforms callers still get
// probeStaleSocket's cross-platform dial-based hijack protection in
// listenUnix, just not this additional flock-based defense-in-depth layer.
func acquireProcessLock(_ string) (func() error, error) {
	return func() error { return nil }, nil
}
