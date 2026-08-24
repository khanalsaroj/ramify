// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
)

// resolveOne finds exactly one environment for branch (optionally narrowed by
// project), returning a clear error if none or more than one match.
func resolveOne(ctx context.Context, cl *apiClient, project, branch string) (environment, error) {
	envs, err := cl.listEnvironments(ctx, project, branch)
	if err != nil {
		return environment{}, err
	}
	switch len(envs) {
	case 0:
		return environment{}, fmt.Errorf("no environment found for branch %q", branch)
	case 1:
		return envs[0], nil
	default:
		return environment{}, fmt.Errorf("multiple environments match branch %q; pass --project to disambiguate", branch)
	}
}
