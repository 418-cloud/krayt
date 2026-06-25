// Command krayt is the CLI entry point (§9, §13).
package main

import (
	"fmt"
	"os"

	"github.com/418-cloud/krayt/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "krayt:", err)
		os.Exit(1)
	}
}
