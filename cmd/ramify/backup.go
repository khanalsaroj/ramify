// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/khanalsaroj/ramify/internal/config"
	"github.com/khanalsaroj/ramify/internal/store"
)

func newBackupCmd() *cobra.Command {
	var configPath, output string
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Create a consistent backup of Ramify's SQLite state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return fmt.Errorf("backup: loading config: %w", err)
			}
			st, err := store.Open(context.Background(), cfg.Store.Path)
			if err != nil {
				return fmt.Errorf("backup: opening store: %w", err)
			}
			defer func() { _ = st.Close() }()
			if err := st.Backup(context.Background(), output); err != nil {
				return fmt.Errorf("backup: %w", err)
			}
			printf(cmd.OutOrStdout(), "created database backup %s\n", output)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "ramify.yaml", "path to ramify.yaml")
	cmd.Flags().StringVar(&output, "output", "ramify-backup.db", "destination backup path")
	return cmd
}
