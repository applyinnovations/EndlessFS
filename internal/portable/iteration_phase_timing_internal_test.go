package portable

// Phase instrumentation is measurement only; every original fixture and
// assertion stays in place. Process counters include concurrent package work.
import (
	"encoding/json"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

type iterationPhase uint8

const (
	iterationFixtureWait iterationPhase = iota
	iterationFixtureSetup
	iterationInputSetup
	iterationAdmission
	iterationAdmissionOracle
	iterationColdIntentOracle
	iterationAdmissionReplay
	iterationTerminalization
	iterationSourceIntentOracle
	iterationCheckpoint
	iterationFrozenAuthorityOracle
	iterationRawCopy
	iterationSplitVerification
	iterationOpenCopy
	iterationWarmChildRemoval
	iterationWarmChildControl
	iterationMissingChildVerification
	iterationIndependentMissingChildVerification
	iterationRestoreClosedGate
	iterationOpenWrites
	iterationCopiedIntentOracle
	iterationCopiedReceiptOracle
	iterationCopiedReplayOracle
)

var iterationPhaseNames = [...]string{
	"fixture_wait", "fixture_setup", "input_setup", "admission",
	"admission_oracle", "cold_intent_oracle", "admission_replay",
	"terminalization", "source_intent_oracle", "checkpoint",
	"frozen_authority_oracle", "raw_copy", "split_verification", "open_copy",
	"warm_child_removal", "warm_child_control", "missing_child_verification",
	"independent_missing_child_verification", "restore_closed_gate", "open_writes",
	"copied_intent_oracle", "copied_receipt_oracle", "copied_replay_oracle",
}

var iterationRequestKinds = [...]providerbudget.RequestKind{
	providerbudget.RequestObjectHead, providerbudget.RequestObjectVerify,
	providerbudget.RequestObjectGet, providerbudget.RequestObjectOpen,
	providerbudget.RequestObjectList, providerbudget.RequestObjectPut,
	providerbudget.RequestObjectDelete, providerbudget.RequestObjectCopy,
	providerbudget.RequestUploadBegin, providerbudget.RequestUploadResume,
	providerbudget.RequestUploadProgress, providerbudget.RequestUploadAbort,
	providerbudget.RequestDownloadSign, providerbudget.RequestDataUpload,
	providerbudget.RequestDataDownload, providerbudget.RequestUnclassified,
}

// Indices are documented in README.md. No event target, operation, subsystem,
// parallel-group value, HTTP field, user, error or capability enters this type.
// Slots: source-state, source-file, copy-state, copy-file. Each kind holds
// requests, failed requests, request bytes, response bytes, as exact uint64s.
type iterationProviderVector [4][16][4]uint64

func iterationProviderSnapshot(ledgers [4]*providerbudget.Ledger) (vector iterationProviderVector, mask uint8, valid bool) {
	valid = true
	for slot, ledger := range ledgers {
		if ledger == nil {
			continue
		}
		mask |= 1 << slot
		wantRole := providerbudget.RoleState
		if slot%2 == 1 {
			wantRole = providerbudget.RoleFile
		}
		for _, event := range ledger.Events() {
			kind := -1
			for index, allowed := range iterationRequestKinds {
				if event.Kind == allowed {
					kind = index
					break
				}
			}
			if kind < 0 || event.Role != wantRole || event.RequestBytes < 0 || event.ResponseBytes < 0 {
				valid = false
				continue
			}
			counts := [4]uint64{1, 0, uint64(event.RequestBytes), uint64(event.ResponseBytes)}
			if event.Failed {
				counts[1] = 1
			}
			for counter, amount := range counts {
				if ^uint64(0)-vector[slot][kind][counter] < amount {
					valid = false
					continue
				}
				vector[slot][kind][counter] += amount
			}
		}
	}
	return vector, mask, valid
}

// startIterationPhase must begin AFTER an existing ledger Reset and end BEFORE
// the next Reset. It never resets a ledger or changes runtime settings. Call
// finish(true) after the unchanged section; cleanup reports interrupted sections.
// nil slots mean provider capture was unavailable, not zero provider work.
func startIterationPhase(t *testing.T, phase iterationPhase, ledgers [4]*providerbudget.Ledger) func(bool) {
	t.Helper()
	if int(phase) >= len(iterationPhaseNames) {
		t.Fatal("unregistered iteration timing phase")
	}
	beforeProvider, beforeMask, beforeValid := iterationProviderSnapshot(ledgers)
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	var once sync.Once
	finish := func(completed bool) {
		once.Do(func() {
			ended := time.Now()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			afterProvider, afterMask, afterValid := iterationProviderSnapshot(ledgers)
			valid := beforeValid && afterValid && beforeMask == afterMask && after.TotalAlloc >= before.TotalAlloc && after.NumGC >= before.NumGC
			var delta iterationProviderVector
			for slot := range delta {
				for kind := range delta[slot] {
					for counter := range delta[slot][kind] {
						if afterProvider[slot][kind][counter] < beforeProvider[slot][kind][counter] {
							valid = false
						} else {
							delta[slot][kind][counter] = afterProvider[slot][kind][counter] - beforeProvider[slot][kind][counter]
						}
					}
				}
			}
			row := struct {
				Schema            int                     `json:"schema"`
				Phase             string                  `json:"phase"`
				Completed         bool                    `json:"completed"`
				TestFailed        bool                    `json:"test_failed"`
				Valid             bool                    `json:"valid"`
				WallNS            int64                   `json:"wall_ns"`
				TotalAllocBefore  uint64                  `json:"process_total_alloc_before"`
				TotalAllocAfter   uint64                  `json:"process_total_alloc_after"`
				NumGCBefore       uint32                  `json:"process_num_gc_before"`
				NumGCAfter        uint32                  `json:"process_num_gc_after"`
				ForcedGCBefore    uint32                  `json:"process_forced_gc_before"`
				ForcedGCAfter     uint32                  `json:"process_forced_gc_after"`
				HeapBefore        uint64                  `json:"process_heap_alloc_before"`
				HeapAfter         uint64                  `json:"process_heap_alloc_after"`
				PauseBefore       uint64                  `json:"process_gc_pause_ns_before"`
				PauseAfter        uint64                  `json:"process_gc_pause_ns_after"`
				ProviderMask      uint8                   `json:"provider_mask"`
				ProviderDelta     iterationProviderVector `json:"provider_delta"`
				CaptureAfterEndNS int64                   `json:"capture_after_end_ns"`
			}{
				1, iterationPhaseNames[phase], completed, t.Failed(), valid, ended.Sub(started).Nanoseconds(),
				before.TotalAlloc, after.TotalAlloc, before.NumGC, after.NumGC, before.NumForcedGC, after.NumForcedGC,
				before.HeapAlloc, after.HeapAlloc, before.PauseTotalNs, after.PauseTotalNs,
				afterMask, delta, time.Since(ended).Nanoseconds(),
			}
			encoded, err := json.Marshal(row)
			if err != nil || len(encoded) > 16<<10 {
				t.Error("iteration phase evidence could not be represented within its fixed bound")
				return
			}
			t.Logf("ITERATION_PHASE_V1=%s", encoded)
			if !valid {
				t.Error("iteration phase evidence has unknown counters or crossed a ledger reset")
			}
		})
	}
	t.Cleanup(func() { finish(false) })
	return finish
}
