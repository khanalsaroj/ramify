// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var project string

	cmd := &cobra.Command{
		Use:   "status <branch>",
		Short: "Show the environment(s) for a branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			envs, err := client().listEnvironments(cmd.Context(), project, args[0])
			if err != nil {
				return err
			}
			if len(envs) == 0 {
				return fmt.Errorf("no environment found for branch %q", args[0])
			}
			if len(envs) > 1 {
				printf(cmd.OutOrStdout(), "multiple environments match branch %q; use --project to disambiguate:\n\n", args[0])
				return printEnvironmentTable(cmd, envs)
			}

			e := envs[0]
			out := cmd.OutOrStdout()
			printf(out, "ID:          %s\n", e.ID)
			printf(out, "Project:     %s\n", e.Project)
			printf(out, "Branch:      %s\n", e.Branch)
			printf(out, "Status:      %s\n", e.Status)
			printf(out, "Subdomain:   %s\n", e.Subdomain)
			printf(out, "Artifact:    %s\n", e.ArtifactRef)
			printf(out, "Deploy ref:  %s\n", e.DeployRef)
			printf(out, "Pinned:      %t\n", e.Pinned)
			if e.TTLExpiresAt != nil {
				printf(out, "TTL expires: %s\n", *e.TTLExpiresAt)
			}
			printf(out, "Created:     %s\n", e.CreatedAt)
			printf(out, "Updated:     %s\n", e.UpdatedAt)
			return nil
		},
	}

	cmd.Flags().StringVar(&project, "project", "", "disambiguate when multiple projects have a matching branch")
	return cmd
}
