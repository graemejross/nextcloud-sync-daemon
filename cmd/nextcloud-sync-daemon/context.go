package main

import (
	"context"
	"os/signal"
	"syscall"
)

// makeContext returns a context that cancels on SIGTERM or SIGINT,
// and a stop function that deregisters the signal handler.
func makeContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
}
