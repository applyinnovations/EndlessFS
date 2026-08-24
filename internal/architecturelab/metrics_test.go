package architecturelab

import "testing"

func TestParetoFrontierRejectsOnlyActuallyDominatedCandidates(t *testing.T) {
	measurements := []Measurement{
		{Candidate: "balanced", Scenario: "move", Valid: true, Metrics: MetricVector{Requests: 2, CostPicoUSD: 2, CriticalP95Micros: 2, SerialP95Micros: 2, RequestBytes: 2, ResponseBytes: 2, StoredObjects: 2, StoredBytes: 2}},
		{Candidate: "dominated", Scenario: "move", Valid: true, Metrics: MetricVector{Requests: 3, CostPicoUSD: 3, CriticalP95Micros: 3, SerialP95Micros: 3, RequestBytes: 3, ResponseBytes: 3, StoredObjects: 3, StoredBytes: 3}},
		{Candidate: "fewer-requests-more-bytes", Scenario: "move", Valid: true, Metrics: MetricVector{Requests: 1, CostPicoUSD: 1, CriticalP95Micros: 1, SerialP95Micros: 1, RequestBytes: 10, ResponseBytes: 10, StoredObjects: 1, StoredBytes: 10}},
		{Candidate: "invalid-fast", Scenario: "move", Metrics: MetricVector{}},
	}
	frontier := ParetoFrontier(measurements)
	if len(frontier) != 2 || frontier[0].Candidate != "balanced" || frontier[1].Candidate != "fewer-requests-more-bytes" {
		t.Fatalf("frontier = %+v", frontier)
	}
}
