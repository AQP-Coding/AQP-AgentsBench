// Aqp-bench runs local coding-agent benchmarks and read-only monitoring.
// Provider-backed runs require explicit credentials; demo is an offline simulation.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/AQP-Coding/AQP-AgentsBench/internal/bench"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := bench.CLI(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
