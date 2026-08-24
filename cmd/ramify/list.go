// SPDX-License-Identifier: Apache-2.0

package main

import (
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	var project string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List preview environments",
		RunE: func(cmd *cobra.Command, _ []string) error {
			envs, err := client().listEnvironments(cmd.Context(), project, "")
			if err != nil {
				return err
			}
			return printEnvironmentTable(cmd, envs)
		},
	}

	cmd.Flags().StringVar(&project, "project", "", "filter by project (owner/repo)")
	return cmd
}

func printEnvironmentTable(cmd *cobra.Command, envs []environment) error {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
	printLine(tw, "ID\tPROJECT\tBRANCH\tSTATUS\tSUBDOMAIN\tARTIFACT")
	for _, e := range envs {
		printf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.Project, e.Branch, e.Status, e.Subdomain, e.ArtifactRef)
	}
	return tw.Flush()
}
