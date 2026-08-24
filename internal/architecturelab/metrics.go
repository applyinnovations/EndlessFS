package architecturelab

import (
	"sort"

	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

// MetricVector is deliberately multi-dimensional. No weighted score can hide
// a regression in provider cost, critical-path latency, transfer, or retained
// state behind an improvement in another dimension.
type MetricVector struct {
	Requests          int64
	CostPicoUSD       int64
	CriticalP95Micros int64
	SerialP95Micros   int64
	RequestBytes      int64
	ResponseBytes     int64
	StoredObjects     int64
	StoredBytes       int64
}

type Measurement struct {
	Candidate   string
	Scenario    string
	Valid       bool
	Metrics     MetricVector
	Limitations []string
}

func MeasurementFromTotals(candidate, scenario string, totals providerbudget.Totals, storedObjects, storedBytes int64) Measurement {
	return Measurement{Candidate: candidate, Scenario: scenario, Valid: true, Metrics: MetricVector{
		Requests: totals.Requests, CostPicoUSD: totals.CostPicoUSD,
		CriticalP95Micros: totals.CriticalP95Micros, SerialP95Micros: totals.P95Micros,
		RequestBytes: totals.RequestBytes, ResponseBytes: totals.ResponseBytes,
		StoredObjects: storedObjects, StoredBytes: storedBytes,
	}}
}

func Dominates(left, right Measurement) bool {
	if !left.Valid || !right.Valid || left.Scenario != right.Scenario {
		return false
	}
	l, r := left.Metrics, right.Metrics
	noWorse := l.Requests <= r.Requests && l.CostPicoUSD <= r.CostPicoUSD && l.CriticalP95Micros <= r.CriticalP95Micros && l.SerialP95Micros <= r.SerialP95Micros && l.RequestBytes <= r.RequestBytes && l.ResponseBytes <= r.ResponseBytes && l.StoredObjects <= r.StoredObjects && l.StoredBytes <= r.StoredBytes
	better := l.Requests < r.Requests || l.CostPicoUSD < r.CostPicoUSD || l.CriticalP95Micros < r.CriticalP95Micros || l.SerialP95Micros < r.SerialP95Micros || l.RequestBytes < r.RequestBytes || l.ResponseBytes < r.ResponseBytes || l.StoredObjects < r.StoredObjects || l.StoredBytes < r.StoredBytes
	return noWorse && better
}

func ParetoFrontier(measurements []Measurement) []Measurement {
	frontier := make([]Measurement, 0, len(measurements))
	for index, candidate := range measurements {
		if !candidate.Valid {
			continue
		}
		dominated := false
		for otherIndex, other := range measurements {
			if index != otherIndex && Dominates(other, candidate) {
				dominated = true
				break
			}
		}
		if !dominated {
			frontier = append(frontier, candidate)
		}
	}
	sort.Slice(frontier, func(left, right int) bool { return frontier[left].Candidate < frontier[right].Candidate })
	return frontier
}
