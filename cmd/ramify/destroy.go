// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"strings"

	"github.com/spf13/cobra"
)

func newDestroyCmd() *cobra.Command {
	var project string
	var yes bool

	cmd := &cobra.Command{
		Use:   "destroy <branch>",
		Short: "Destroy a branch's preview environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl := client()
			env, err := resolveOne(cmd.Context(), cl, project, args[0])
			if err != nil {
				return err
			}

			if !yes {
				printf(cmd.OutOrStdout(), "destroy environment %s (%s/%s)? [y/N] ", env.ID, env.Project, env.Branch)
				reader := bufio.NewReader(cmd.InOrStdin())
				line, _ := reader.ReadString('\n')
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					printLine(cmd.OutOrStdout(), "aborted")
					return nil
				}
			}

			if err := cl.deleteEnvironment(cmd.Context(), env.ID); err != nil {
				return err
			}
			printf(cmd.OutOrStdout(), "destroyed %s\n", env.ID)
			return nil
		},
	}

	cmd.Flags().StringVar(&project, "project", "", "disambiguate when multiple projects have a matching branch")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}
