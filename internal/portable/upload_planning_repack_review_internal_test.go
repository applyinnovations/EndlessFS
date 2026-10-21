package portable

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func reviewReadPlanningHead(t *testing.T, backend objectstore.Backend, key objectstore.Key) (storageformat.ProjectionHead, objectstore.NativeVersion) {
	t.Helper()
	object, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var head storageformat.ProjectionHead
	if err := storageformat.DecodeEnvelope(object.Body, key, duplicateProjectionHeadSchema, &envelope, &head); err != nil {
		t.Fatal(err)
	}
	if err := storageformat.ValidateProjectionHead(head); err != nil {
		t.Fatal(err)
	}
	return head, object.Version
}

func TestUploadPlanningRepackReviewLostResponsesCancellationAndNewerHead(t *testing.T) {
	t.Parallel()
	releaseSeed := acquireCorrectiveHeavyFixture(t)
	baseline := objectmemory.New()
	seedEngine := openNamespaceTestEngine(t, baseline)
	live, fullRequest := reviewSeedDeepProjectionNamespace(t, seedEngine)
	fixture := baseline.Export()
	// The exported bytes are read-only; every case imports its own backend.
	// Release the seed's slot before parallel children acquire their slots.
	releaseSeed()
	projectionID := uploadPlanningProjectionID008(live.UserID())
	headKey := storageformat.ScopedProjectionHeadKey(live.UserID().String(), storageformat.ProjectionDuplicates, projectionID)
	request := domain.UploadSizePlanRequest{Items: fullRequest.Items[:1]}
	for _, mode := range []string{"lost-pack-put", "cancel-after-pack", "lost-head-put", "newer-head-wins"} {
		t.Run(mode, func(t *testing.T) {
			parallelCorrectiveHeavyFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			base := objectmemory.New()
			if err := base.Import(fixture); err != nil {
				t.Fatal(err)
			}
			backend := &reviewRepackFaultBackend{Backend: base, head: headKey}
			engine := openNamespaceTestEngine(t, backend)
			backend.packPuts, backend.headPuts = 0, 0
			triggered := false
			var winner storageformat.ProjectionHead
			var interleaveErr error
			backend.before = func(_ context.Context, key objectstore.Key, _ []byte, _ objectstore.PutCondition) error {
				if mode != "newer-head-wins" || key != headKey || triggered {
					return nil
				}
				triggered = true
				other := openInternalTestEngine(t, base, engine.clock.(*domain.FixedClock), strings.NewReader(strings.Repeat("r", 1<<17)))
				if _, err := other.Files().CreateDirectory(context.Background(), live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/winning-directory-only-change")}); err != nil {
					interleaveErr = err
					return err
				}
				if _, err := other.Files().PlanUploadSizes(context.Background(), live.UserID(), request); err != nil {
					interleaveErr = err
					return err
				}
				winner, _ = reviewReadPlanningHead(t, base, headKey)
				return nil
			}
			backend.after = func(_ context.Context, key objectstore.Key, version objectstore.NativeVersion, err error) (objectstore.NativeVersion, error) {
				if err != nil || triggered {
					return version, err
				}
				isPack := strings.HasPrefix(key.String(), storageformat.ProjectionPrefix()) && strings.Contains(key.String(), "/packs/")
				if isPack && (mode == "lost-pack-put" || mode == "cancel-after-pack") {
					triggered = true
					if mode == "cancel-after-pack" {
						cancel()
						return version, nil
					}
					return "", domain.NewError(domain.ErrorUnavailable, "synthetic lost projection pack response")
				}
				if mode == "lost-head-put" && key == headKey {
					triggered = true
					return "", domain.NewError(domain.ErrorUnavailable, "synthetic lost projection head response")
				}
				return version, err
			}
			plan, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), request)
			if interleaveErr != nil {
				t.Fatalf("concurrent fixture failed before its publication: %v", interleaveErr)
			}
			if !triggered {
				t.Fatalf("actual capacity/repack fault boundary was not reached: %v", err)
			}
			if mode == "newer-head-wins" {
				if err != nil || len(plan.Items) != 1 || !plan.Items[0].TargetExists {
					t.Fatalf("planner did not reload the winning namespace/projection: %+v %v", plan, err)
				}
				head, _ := reviewReadPlanningHead(t, base, headKey)
				if head != winner {
					t.Fatalf("stale builder replaced the newer projection: actual=%+v winner=%+v", head, winner)
				}
			} else {
				if err == nil {
					t.Fatal("lost/canceled publication incorrectly returned a plan")
				}
				if mode == "cancel-after-pack" && (!errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable)) {
					t.Fatalf("cancellation/domain semantics changed: %v", err)
				}
				if mode != "lost-head-put" {
					if backend.headPuts != 0 {
						t.Fatalf("head publication attempted before every pack was confirmed: %d", backend.headPuts)
					}
					if _, err := base.Get(context.Background(), headKey); !errors.Is(err, domain.ErrNotFound) {
						t.Fatalf("partial repack published a head: %v", err)
					}
				}
			}
			backend.before, backend.after = nil, nil
			retried, retryErr := engine.Files().PlanUploadSizes(context.Background(), live.UserID(), request)
			if retryErr != nil || len(retried.Items) != 1 || !retried.Items[0].TargetExists {
				t.Fatalf("safe planning retry failed: %+v %v", retried, retryErr)
			}
			head, version := reviewReadPlanningHead(t, base, headKey)
			if head.SourceDomainID != live.UserID().String() || head.ProjectionID != projectionID || head.OwnerID != live.UserID().String() {
				t.Fatalf("repacked head lost owner/source binding: %+v", head)
			}
			// Read both ends of the multi-pack index from a separately constructed
			// replica; a successful head alone does not prove its closure exists.
			other := openNamespaceTestEngine(t, base)
			last := domain.UploadSizePlanRequest{Items: fullRequest.Items[len(fullRequest.Items)-1:]}
			if got, err := other.Files().PlanUploadSizes(context.Background(), live.UserID(), last); err != nil || len(got.Items) != 1 || !got.Items[0].TargetExists || !got.Items[0].FingerprintRequired {
				t.Fatalf("second replica could not open the final repacked index page: %+v %v", got, err)
			}
			after, afterVersion := reviewReadPlanningHead(t, base, headKey)
			if after != head || afterVersion != version {
				t.Fatal("warm second-replica retry republished or changed the projection")
			}
		})
	}
}
