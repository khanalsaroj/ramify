// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
)

// printf and println write to the CLI's own stdout/stderr; a failed write there
// isn't actionable (the user losing their terminal output isn't something the
// program can recover from), so the error is deliberately discarded.
func printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func printLine(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}
