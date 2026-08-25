// SPDX-License-Identifier: Apache-2.0

// Command ramify is the Ramify CLI: it talks to a running ramifyd over its local
// control API.
package main

import (
	"os"

	"github.com/spf13/cobra"
)

// version is the ramify CLI build version, overridden at build time via -ldflags.
var version = "dev"

var (
	flagSocket string
	flagAddr   string
	flagToken  string
)

const defaultSocketPath = "/var/run/ramify/ramify.sock"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "ramify",
		Short:   "Ramify CLI — manage preview environments",
		Version: version,
	}

	root.PersistentFlags().StringVar(&flagSocket, "socket", defaultSocketPath, "path to ramifyd's unix socket")
	root.PersistentFlags().StringVar(&flagAddr, "addr", "", "ramifyd TCP address (overrides --socket if set)")
	root.PersistentFlags().StringVar(&flagToken, "token", "", "bearer token for --addr")

	root.AddCommand(
		newInstallCmd(),
		newInitCmd(),
		newListCmd(),
		newStatusCmd(),
		newLogsCmd(),
		newDestroyCmd(),
		newDoctorCmd(),
		newBackupCmd(),
	)
	return root
}

func client() *apiClient {
	if flagAddr != "" {
		return newTCPClient(flagAddr, flagToken)
	}
	return newUnixClient(flagSocket)
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		printLine(os.Stderr, err)
		os.Exit(1)
	}
}
