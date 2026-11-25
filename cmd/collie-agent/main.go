// Command collie-agent is collie's local agent: it pairs a machine with collie and runs as a user service that
// keeps the machine online and, in later versions, runs coding agents on collie's tasks.
package main

import (
	"os"

	"github.com/collies-ai/collie-agent/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
