package portable

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

func TestProviderBudgetUploadIndependentAdmission(t *testing.T) {
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ mode, budget string }{
		{"cold", "file-upload-admission-cold-schema-012"},
		{"active-replay", "file-upload-admission-active-replay-schema-012"},
		{"partial-failure", "file-upload-admission-partial-failure-schema-012"},
		{"selective-retry", "file-upload-admission-selective-retry-schema-012"},
		{"completed-replay", "file-upload-admission-completed-replay-schema-012"},
	}
	for _, item := range cases {
		mode := item.mode
		t.Run(mode, func(t *testing.T) {
			f := newUploadBatchScaleFixture(t, 0x17884001)
			ctx := context.Background()
			requests := scaleUploadRequestsCount("admission-budget", 100)
			var page []domain.UploadAdmissionResult
			if mode == "partial-failure" || mode == "selective-retry" {
				requests[99].Size = 1
				base, _ := f.engine.Files().transferBackend()
				control := f.engine.fileBackend
				f.engine.fileBackend = &admissionProbe{FileControlBackend: control, DirectTransferBackend: base, begin: func(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
					if request.Size != 1 {
						return base.BeginUpload(ctx, request)
					}
					trace := providerbudget.TraceFromContext(ctx)
					f.fileLedger.Record(providerbudget.Event{Role: providerbudget.RoleFile, Kind: providerbudget.RequestUploadBegin, Operation: trace.Operation, Subsystem: trace.Subsystem, ParallelGroup: trace.ParallelGroup, Failed: true})
					return objectstore.UploadHandle{}, domain.ErrUnavailable
				}}
				if mode == "selective-retry" {
					page, err = f.engine.Files().AdmitUploads(ctx, f.scope, requests)
					if err != nil || page[99].Outcome != "unknown" {
						t.Fatalf("partial setup: %+v %v", page, err)
					}
					f.engine.fileBackend = control
					requests = requests[99:]
				}
			}
			if mode == "active-replay" || mode == "completed-replay" {
				page, err = f.engine.Files().AdmitUploads(ctx, f.scope, requests)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "completed-replay" {
					caps := make([]domain.UploadCapability, len(page))
					actions := make([]domain.UploadActionItem, len(page))
					for i, item := range page {
						if item.Capability == nil {
							t.Fatalf("setup: %+v", item)
						}
						caps[i] = *item.Capability
						actions[i] = domain.UploadActionItem{UploadID: item.UploadID, Action: "complete", CRC32C: "AAAAAA"}
					}
					uploadEmptyCapabilities(t, f.server.Client(), caps)
					result, err := f.engine.Files().UploadActions(ctx, f.scope, actions)
					if err != nil {
						t.Fatal(err)
					}
					for _, item := range result {
						if item.Outcome != "applied" {
							t.Fatalf("setup completion: %+v", item)
						}
					}
				}
			}
			f.stateLedger.Reset()
			f.fileLedger.Reset()
			page, err = f.engine.Files().AdmitUploads(ctx, f.scope, requests)
			if err != nil {
				t.Fatal(err)
			}
			for i, item := range page {
				switch {
				case mode == "partial-failure" && i == 99:
					if item.Outcome != "unknown" || !item.Retryable {
						t.Fatalf("failure: %+v", item)
					}
				case mode == "completed-replay":
					if item.Outcome != "completed" || item.Capability != nil {
						t.Fatalf("completed replay: %+v", item)
					}
				default:
					if item.Capability == nil {
						t.Fatalf("admission: %+v", item)
					}
				}
			}
			name := item.budget
			events := append(f.stateLedger.Events(), f.fileLedger.Events()...)
			budget, err := providerbudget.Calibrate(name, model, []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, events)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(budget)
			totals, _ := model.Estimate(events)
			vector, _ := json.Marshal(totals)
			t.Logf("ADMISSION_CALIBRATION=%s", body)
			t.Logf("ADMISSION_VECTOR mode=%s state=%d native=%d totals=%s", mode, len(f.stateLedger.Events()), len(f.fileLedger.Events()), vector)
			checkTransferScaleBudget(t, name, f.stateLedger.Events(), f.fileLedger.Events())
		})
	}
}
