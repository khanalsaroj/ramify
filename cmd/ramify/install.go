// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newInstallCmd() *cobra.Command {
	var configDir, dataDir string

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Create the directories Ramify needs, ready for `ramify init`",
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, dir := range []string{configDir, dataDir} {
				if err := os.MkdirAll(dir, 0o750); err != nil {
					return fmt.Errorf("creating %s: %w", dir, err)
				}
				printf(cmd.OutOrStdout(), "created %s\n", dir)
			}
			printLine(cmd.OutOrStdout(), "\nNext: run `ramify init` to generate a config file.")
			return nil
		},
	}

	cmd.Flags().StringVar(&configDir, "config-dir", "/etc/ramify", "directory ramify.yaml will live in")
	cmd.Flags().StringVar(&dataDir, "data-dir", "/var/lib/ramify", "directory the state database will live in")
	return cmd
}
