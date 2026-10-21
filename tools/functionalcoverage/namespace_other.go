//go:build !linux || !amd64

package main

import "context"

func launchIsolatedConsumer(context.Context, config, []string, string) (int, bool) {
	return 1, false
}
func consumeIsolated(context.Context, config, []string) int { return 1 }
