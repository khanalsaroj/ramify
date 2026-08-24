// SPDX-License-Identifier: Apache-2.0

// Command ramify is the Ramify CLI.
package main

import "fmt"

// version is the ramify CLI build version, overridden at build time via -ldflags.
var version = "dev"

func main() {
	fmt.Println("ramify " + version)
}
