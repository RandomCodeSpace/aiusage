// Command aiusage is a local, read-only AI-agent token-usage daemon and TUI.
// It delegates all behaviour to the cobra command tree in internal/cmd.
package main

import (
	"fmt"
	"os"

	"aiusage/internal/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
