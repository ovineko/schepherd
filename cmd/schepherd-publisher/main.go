// Command schepherd-publisher is the maintainer tool that turns upstream JSON
// Schemas into verified, self-contained schema artifacts (prepare) and
// publishes them as a catalog snapshot to an OCI repository (publish). It is
// never shipped to users; see docs/publishing.md.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)

	stop()
	os.Exit(code)
}
