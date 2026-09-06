package providerbudget_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type scaleEvidence struct {
	ID                 string `json:"id"`
	Category           string `json:"category"`
	LogicalItems       int64  `json:"logicalItems"`
	BrowserRequests    int64  `json:"browserRequests"`
	ProviderRequests   int64  `json:"providerRequests"`
	CostPicoUSD        int64  `json:"costPicoUSD"`
	AggregateP95Micros int64  `json:"aggregateP95Micros"`
	CriticalP95Micros  int64  `json:"criticalP95Micros"`
	RequestBasis       string `json:"requestBasis"`
}

type targetEvidence struct {
	ID                       string `json:"id"`
	BaselineProviderRequests int64  `json:"baselineProviderRequests,omitempty"`
	TargetBrowserRequests    int64  `json:"targetBrowserRequests"`
	TargetProviderRequests   int64  `json:"targetProviderRequests"`
	TargetCostPicoUSD        int64  `json:"targetCostPicoUSD"`
	TargetCriticalP95Micros  int64  `json:"targetCriticalP95Micros"`
	FeasibilityBasis         string `json:"feasibilityBasis"`
}

func productionScaleEvidence(t *testing.T, ratchet providerbudget.RatchetLedger) []scaleEvidence {
	t.Helper()
	seen := make(map[string]bool)
	evidence := make([]scaleEvidence, 0, len(providerbudget.ProductionScaleScenarios()))
	for _, scenario := range providerbudget.ProductionScaleScenarios() {
		if scenario.ID == "" || scenario.Category == "" || scenario.LogicalItems < 1 || scenario.BrowserRequests < 0 || scenario.RequestBasis == "" || seen[scenario.ID] {
			t.Fatalf("invalid production scale scenario: %+v", scenario)
		}
		seen[scenario.ID] = true
		item := scaleEvidence{ID: scenario.ID, Category: scenario.Category, LogicalItems: scenario.LogicalItems, BrowserRequests: scenario.BrowserRequests, RequestBasis: scenario.RequestBasis}
		for _, execution := range scenario.Executions {
			if execution.Budget == "" || execution.Executions < 1 || execution.Parallelism < 1 || execution.Parallelism > execution.Executions {
				t.Fatalf("invalid execution in %q: %+v", scenario.ID, execution)
			}
			budget, found := ratchet.Latest(execution.Budget)
			if !found {
				t.Fatalf("scale scenario %q references missing budget %q", scenario.ID, execution.Budget)
			}
			item.ProviderRequests += budget.Maximum.Requests * execution.Executions
			item.CostPicoUSD += budget.Maximum.CostPicoUSD * execution.Executions
			item.AggregateP95Micros += budget.Maximum.P95Micros * execution.Executions
			critical := budget.Maximum.CriticalP95Micros
			if critical == 0 {
				critical = budget.Maximum.P95Micros
			}
			waves := (execution.Executions + execution.Parallelism - 1) / execution.Parallelism
			item.CriticalP95Micros += critical * waves
		}
		if len(scenario.Executions) == 0 && (scenario.ID != "restored-transfer-ledger-needs-source" || scenario.BrowserRequests != 0) {
			t.Fatalf("only dormant restored transfer history may have a zero-provider scale budget: %+v", scenario)
		}
		evidence = append(evidence, item)
	}
	return evidence
}

func TestProviderBudgetProductionScaleScenarios(t *testing.T) {
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	evidence := productionScaleEvidence(t, ratchet)
	if len(evidence) < 18 {
		t.Fatalf("production scale audit has only %d scenarios", len(evidence))
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("provider-scale-v1 %s", body)
}

func TestProviderBudgetProductionScaleScenariosConformToTargets(t *testing.T) {
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	ratchet, err := gcs.RegionalStandardFlatBudgetRatchet()
	if err != nil {
		t.Fatal(err)
	}
	baselines := make(map[string]scaleEvidence)
	for _, item := range productionScaleEvidence(t, ratchet) {
		baselines[item.ID] = item
	}
	seenTargets := make(map[string]bool)
	coveredBaselines := make(map[string]bool)
	evidence := make([]targetEvidence, 0, len(providerbudget.ProductionScaleTargets()))
	for _, target := range providerbudget.ProductionScaleTargets() {
		if target.ID == "" || target.Category == "" || target.LogicalItems < 1 || target.MaximumBrowserRequests < 0 || target.FeasibilityBasis == "" || seenTargets[target.ID] {
			t.Fatalf("invalid production scale target: %+v", target)
		}
		seenTargets[target.ID] = true
		events := make([]providerbudget.Event, 0)
		for waveIndex, wave := range target.RequestWaves {
			if wave.Count < 1 || wave.Parallelism < 1 || wave.Parallelism > wave.Count || wave.RequestBytesEach < 0 || wave.ResponseBytesEach < 0 {
				t.Fatalf("invalid request wave in %q: %+v", target.ID, wave)
			}
			for requestIndex := int64(0); requestIndex < wave.Count; requestIndex++ {
				group := ""
				if wave.Parallelism > 1 {
					group = fmt.Sprintf("wave-%d-batch-%d", waveIndex, requestIndex/wave.Parallelism)
				}
				events = append(events, providerbudget.Event{
					Role: wave.Role, Kind: wave.Kind, Operation: target.ID, Subsystem: "efficiency-target", ParallelGroup: group,
					RequestBytes: wave.RequestBytesEach, ResponseBytes: wave.ResponseBytesEach,
				})
			}
		}
		totals, err := model.Estimate(events)
		if err != nil {
			t.Fatalf("estimate target %q: %v", target.ID, err)
		}
		item := targetEvidence{
			ID: target.ID, TargetBrowserRequests: target.MaximumBrowserRequests, TargetProviderRequests: totals.Requests,
			TargetCostPicoUSD: totals.CostPicoUSD, TargetCriticalP95Micros: totals.CriticalP95Micros, FeasibilityBasis: target.FeasibilityBasis,
		}
		if target.BaselineScenario != "" {
			observed, ok := baselines[target.BaselineScenario]
			if !ok || coveredBaselines[target.BaselineScenario] {
				t.Fatalf("target %q has invalid baseline %q", target.ID, target.BaselineScenario)
			}
			coveredBaselines[target.BaselineScenario] = true
			item.BaselineProviderRequests = observed.ProviderRequests
			if target.ID == "restored-transfer-ledger-needs-source" {
				if totals.Requests != 0 || observed.ProviderRequests != 0 {
					t.Fatalf("dormant transfer target = %d, observed = %d; want zero", totals.Requests, observed.ProviderRequests)
				}
			} else if observed.ProviderRequests > totals.Requests || observed.CostPicoUSD > totals.CostPicoUSD || observed.CriticalP95Micros > totals.CriticalP95Micros {
				t.Errorf("scenario %q exceeds its provider target: target=%+v observed=%+v", target.ID, totals, observed)
			}
			if observed.BrowserRequests > target.MaximumBrowserRequests {
				t.Errorf("scenario %q uses %d browser requests, target permits %d", target.ID, observed.BrowserRequests, target.MaximumBrowserRequests)
			}
		}
		evidence = append(evidence, item)
	}
	if len(coveredBaselines) != len(baselines) {
		t.Fatalf("production scale targets cover %d of %d baselines", len(coveredBaselines), len(baselines))
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("provider-target-v1 %s", body)
}

func TestProviderBudgetTargetPayloadEnvelopesAreMeasured(t *testing.T) {
	const (
		controlBodyLimit = 1 << 20
		segmentLimit     = 4 << 20
		projectionLimit  = 2 * segmentLimit
	)
	digest := strings.Repeat("d", 64)
	padding := strings.Repeat("x", controlBodyLimit-128)
	intent, err := json.Marshal(struct {
		Padding string `json:"padding"`
	}{Padding: padding})
	if err != nil || len(intent) > controlBodyLimit {
		t.Fatalf("construct maximum accepted control intent: bytes=%d err=%v", len(intent), err)
	}
	transaction, err := json.Marshal(struct {
		SchemaVersion int             `json:"schemaVersion"`
		BaseRevision  string          `json:"baseRevision"`
		RequestDigest string          `json:"requestDigest"`
		Intent        json.RawMessage `json:"intent"`
	}{SchemaVersion: 1, BaseRevision: digest, RequestDigest: digest, Intent: intent})
	if err != nil || len(transaction) > segmentLimit {
		t.Fatalf("compact transaction envelope: bytes=%d maximum=%d err=%v", len(transaction), segmentLimit, err)
	}

	entries := make([]storageformat.DirectoryEntry, 10_000)
	for index := range entries {
		entries[index] = storageformat.DirectoryEntry{
			Name: strings.Repeat("n", 249) + fmt.Sprintf("%06d", index), NameDigest: digest,
			Kind: domain.EntryFile, BlobID: fmt.Sprintf("blob-%06d", index), Size: 1,
			MediaType: "application/octet-stream", MD5: "MDEyMzQ1Njc4OWFiY2RlZg==", CRC32C: "AAAAAA==",
			ModifiedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), LogicalVersion: digest,
		}
	}
	projection, err := json.Marshal(entries)
	if err != nil || len(projection) > projectionLimit {
		t.Fatalf("10,000-entry verbose projection: bytes=%d maximum=%d err=%v", len(projection), projectionLimit, err)
	}

	type progressItem struct {
		UploadID string `json:"uploadID"`
		Lease    string `json:"sealedLease"`
	}
	progress := make([]progressItem, 1_000)
	for index := range progress {
		progress[index] = progressItem{UploadID: fmt.Sprintf("upload-%06d", index), Lease: strings.Repeat("l", 2<<10)}
	}
	progressBody, err := json.Marshal(progress)
	if err != nil || len(progressBody) > segmentLimit {
		t.Fatalf("1,000-item transfer progress segment: bytes=%d maximum=%d err=%v", len(progressBody), segmentLimit, err)
	}
	t.Logf("provider-target-payload-v1 transactionBytes=%d projectionBytes=%d transferProgressBytes=%d", len(transaction), len(projection), len(progressBody))
}
