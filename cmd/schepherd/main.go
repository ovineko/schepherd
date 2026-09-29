// Command schepherd delivers pinned JSON Schemas from OCI registries to
// verified local files and optionally runs an external consumer on them.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ovineko/schepherd/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)

	stop()
	os.Exit(code)
}
