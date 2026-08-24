// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"
)

func newLogsCmd() *cobra.Command {
	var project string

	cmd := &cobra.Command{
		Use:   "logs <branch>",
		Short: "Print the deploy logs for a branch's environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl := client()
			env, err := resolveOne(cmd.Context(), cl, project, args[0])
			if err != nil {
				return err
			}
			logs, err := cl.logs(cmd.Context(), env.ID)
			if err != nil {
				return err
			}
			printf(cmd.OutOrStdout(), "%s", logs)
			return nil
		},
	}

	cmd.Flags().StringVar(&project, "project", "", "disambiguate when multiple projects have a matching branch")
	return cmd
}
