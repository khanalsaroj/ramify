// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"context"
	"fmt"

	"golang.org/x/crypto/ssh"
)

// sshRunner is the real commandRunner, dialing a fresh SSH connection per command.
// A deploy operation happens at most a handful of times per environment change, so
// the per-call dial cost is not a meaningful concern.
type sshRunner struct {
	addr   string
	config *ssh.ClientConfig
}

// Run implements commandRunner.
func (r *sshRunner) Run(ctx context.Context, command string) (string, error) {
	client, err := dialContext(ctx, r.addr, r.config)
	if err != nil {
		return "", fmt.Errorf("dialing %s: %w", r.addr, err)
	}
	defer func() { _ = client.Close() }() // best-effort cleanup

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("opening session: %w", err)
	}
	defer func() { _ = session.Close() }() // best-effort cleanup; Wait already reports the real error

	out, err := session.CombinedOutput(command)
	if err != nil {
		return string(out), fmt.Errorf("running command: %w: %s", err, out)
	}
	return string(out), nil
}

// dialContext dials an SSH connection honoring ctx cancellation, since
// golang.org/x/crypto/ssh has no native context-aware Dial.
func dialContext(ctx context.Context, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		client, err := ssh.Dial("tcp", addr, config)
		ch <- result{client: client, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		return res.client, res.err
	}
}
