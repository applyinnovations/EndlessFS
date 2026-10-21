//go:build !linux || !amd64

package main

import "context"

type cleanupObservation struct {
	Adequate, NaturalZero, FinalZero            bool
	Scope                                       string
	InitialOwned, FinalOwned, PidfdKillRequests uint64
	InitialKinds                                map[string]uint64
	Failure                                     string
	InitialScanComplete, FinalScanComplete      bool
	ScanStage, ScanReason                       string
	ReapedChildren                              uint64
	NamespaceIsolated                           bool
}

func backendAvailable() bool { return false }
func runCommand(context.Context, config, string, string, string, []string, []string, string, int64) commandResult {
	panic(errContract)
}
func produce(context.Context, config) { panic(errContract) }
