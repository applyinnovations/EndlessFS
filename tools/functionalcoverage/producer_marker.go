package main

import (
	"encoding/json"
	"time"
)

type producerMarker struct {
	Schema               string    `json:"schema"`
	InputSHA256          string    `json:"input_sha256"`
	RunnerSHA256         string    `json:"runner_sha256"`
	Started              time.Time `json:"producer_started_utc"`
	Finished             time.Time `json:"producer_finished_utc"`
	Packages             uint64    `json:"packages"`
	Roots                uint64    `json:"roots"`
	Nodes                uint64    `json:"nodes"`
	AllowedSkips         uint64    `json:"allowed_skips"`
	FreshTests           bool      `json:"fresh_tests"`
	ResourceQualified    bool      `json:"resource_qualified"`
	PerformanceQualified bool      `json:"performance_qualified"`
	PhysicalQualified    bool      `json:"fresh_physical_replica_qualified"`
	ReleaseQualified     bool      `json:"release_qualified"`
}

func producerSuccessMarker(r receipt) []byte {
	if !r.Passed || !r.FunctionalAdequate || !r.FunctionalTestsExecuted || len(r.InputSHA256) != 64 || len(r.RunnerSHA256) != 64 || r.ProducerStartedUTC.IsZero() || !r.ProducerFinishedUTC.After(r.ProducerStartedUTC) || !r.Events.Adequate || r.Events.Packages == 0 || r.Events.Roots == 0 || r.Events.Nodes < r.Events.Roots || r.Events.AllowedSkips != 2 || r.ResourceProfileEligible || r.PerformanceQualified || r.FreshPhysicalReplicaQualified || r.ReleaseQualified {
		panic(errContract)
	}
	marker := producerMarker{Schema: "endlessfs-functional-producer-success-v1", InputSHA256: r.InputSHA256, RunnerSHA256: r.RunnerSHA256, Started: r.ProducerStartedUTC.UTC(), Finished: r.ProducerFinishedUTC.UTC(), Packages: r.Events.Packages, Roots: r.Events.Roots, Nodes: r.Events.Nodes, AllowedSkips: r.Events.AllowedSkips, FreshTests: true}
	body, e := json.Marshal(marker)
	must(e)
	if len(body) > 2048 {
		panic(errContract)
	}
	return body
}
