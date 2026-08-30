// SPDX-License-Identifier: Apache-2.0

//go:build unix

package api

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquireProcessLock takes an advisory exclusive flock on a sibling ".lock"
// file next to the control socket, held for the daemon's lifetime. It exists as
// defense in depth for the narrow TOCTOU window between probeStaleSocket's dial
// check and listenUnix's actual bind/listen: two ramifyd processes racing to
// start against the same socket path could otherwise both pass the probe before
// either is listening. flock is held by the OS per open file description and is
// automatically released if the process dies, so it never needs manual cleanup
// on an unclean shutdown the way the socket file itself does.
func acquireProcessLock(path string) (func() error, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640) //nolint:gosec // path is derived from the operator-configured socket path
	if err != nil {
		return nil, fmt.Errorf("opening process lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquiring process lock %s: another ramifyd instance is already running: %w", path, err)
	}
	return func() error {
		unlockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
		closeErr := f.Close()
		return errors.Join(unlockErr, closeErr)
	}, nil
}
