package main

import (
	"context"
	"os/signal"
	"syscall"
)

// makeContext returns a context that cancels on SIGTERM or SIGINT.
func makeContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	return ctx
}
