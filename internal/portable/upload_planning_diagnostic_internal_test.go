package portable

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

type planningDiagnosticWriter struct{ records chan map[string]any }

func (writer *planningDiagnosticWriter) Write(body []byte) (int, error) {
	var record map[string]any
	if err := json.Unmarshal(body, &record); err != nil {
		return 0, err
	}
	writer.records <- record
	return len(body), nil
}

func TestUploadPlanningDiagnosticFingerprintRetainsSpecificFailureStages(t *testing.T) {
	for _, test := range []struct {
		name, stage, reason string
		cause               error
	}{
		{"invalid-request", "request_validation", "validation", domain.ErrInvalid},
		{"invalid-token", "request_validation", "validation", domain.ErrInvalid},
		{"stale-snapshot", "snapshot_check", "snapshot_stale", domain.ErrConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := openNamespaceTestEngine(t, objectmemory.New())
			live := namespaceTestScope(t, domain.AreaLive)
			seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 1)
			plan, err := engine.Files().PlanUploadSizes(context.Background(), live.UserID(), uploadSizePlanFixture(1))
			if err != nil {
				t.Fatal(err)
			}
			request := uploadFingerprintPlanFixture(1, plan.Token)
			switch test.name {
			case "invalid-request":
				request.Items = nil
			case "invalid-token":
				request.Token = "sentinel-invalid-private-token"
			case "stale-snapshot":
				if _, err := engine.Files().CreateDirectory(context.Background(), live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/changed")}); err != nil {
					t.Fatal(err)
				}
			}
			writer := &planningDiagnosticWriter{records: make(chan map[string]any, 64)}
			observer := telemetry.New(nil, slog.New(slog.NewJSONHandler(writer, nil)))
			defer observer.Close()
			ctx, activity := telemetry.Start(telemetry.Context(context.Background(), observer), telemetry.UploadFingerprints, telemetry.Application)
			_, err = engine.Files().PlanUploadFingerprints(ctx, live.UserID(), request)
			activity.End(err)
			if !errors.Is(err, test.cause) {
				t.Fatalf("planning failure lost preserved domain semantics: %v", err)
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for {
				select {
				case record := <-writer.records:
					if record["operation"] != "upload.plan.fingerprints" {
						continue
					}
					if record["stage"] != test.stage || record["reason"] != test.reason {
						t.Fatalf("planner overwrote its specific failure diagnosis: stage=%v reason=%v, want %s/%s", record["stage"], record["reason"], test.stage, test.reason)
					}
					return
				case <-deadline.C:
					t.Fatal("real fingerprint planning failure diagnostic missing")
				}
			}
		})
	}
}
