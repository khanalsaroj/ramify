// SPDX-License-Identifier: Apache-2.0

// Command ramifyd is the Ramify control plane daemon.
package main

import (
	"fmt"
	"os"
)

// version is the ramifyd build version, overridden at build time via -ldflags.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("ramifyd " + version)
		return
	}
	fmt.Println("ramifyd " + version)
}
